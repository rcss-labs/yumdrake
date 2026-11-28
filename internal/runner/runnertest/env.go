// Package runnertest has a fake runner.Env for executor tests: it keeps the
// leg's log lines and operations in memory and follows the contract of
// runner.Env.Do without a database.
package runnertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// Line is one logged line.
type Line struct{ Level, Text string }

// Env is a runner.Env held in memory. Set the exported fields before the
// executor runs; read the results through Lines, OpsSnapshot and DoKeys.
type Env struct {
	// Cancel is what CancelRequested reports.
	Cancel bool
	// PresetOps are the operations already recorded by earlier attempts. Do
	// appends to them.
	PresetOps []store.Op
	// Attempt is the attempt number of the leg being run; tests copy it into
	// the store.Leg they pass to Execute.
	Attempt int
	// AfterDo, when set, runs after each call to Do has finished (whatever it
	// returned). A test uses it to request a cancel in the middle of a leg.
	AfterDo func(e *Env)

	mu     sync.Mutex
	lines  []Line
	doKeys []string
	nextID int64
}

var _ runner.Env = (*Env)(nil)

// Log records a line.
func (e *Env) Log(level, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lines = append(e.lines, Line{level, text})
}

// Lines returns a copy of the logged lines.
func (e *Env) Lines() []Line {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Line(nil), e.lines...)
}

// OpsSnapshot returns a copy of the operations.
func (e *Env) OpsSnapshot() []store.Op {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]store.Op(nil), e.PresetOps...)
}

// DoKeys returns the keys Do was called with, in order, including calls that
// did not run their function.
func (e *Env) DoKeys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.doKeys...)
}

// Resume finds no checkpoint.
func (e *Env) Resume(any) (bool, error) { return false, nil }

// Checkpoint accepts and forgets a checkpoint.
func (e *Env) Checkpoint(context.Context, any) error { return nil }

// Ops lists the operations.
func (e *Env) Ops(context.Context) ([]store.Op, error) { return e.OpsSnapshot(), nil }

// CancelRequested reports Cancel.
func (e *Env) CancelRequested(context.Context) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Cancel
}

// SetCancel sets Cancel.
func (e *Env) SetCancel(v bool) {
	e.mu.Lock()
	e.Cancel = v
	e.mu.Unlock()
}

// Do follows the contract in runner.Env: done returns the op, intended gives
// ErrAmbiguous, failed gives ErrPreviouslyFailed, otherwise the intent is
// recorded and fn runs. An fn error wrapping runner.ErrMaybeApplied (or a
// context error) leaves the op intended; any other fn error marks it failed.
func (e *Env) Do(ctx context.Context, name, key string, intent any, fn func() (any, error)) (store.Op, error) {
	op, err := e.do(ctx, name, key, intent, fn)
	if e.AfterDo != nil {
		e.AfterDo(e)
	}
	return op, err
}

func (e *Env) do(ctx context.Context, name, key string, intent any, fn func() (any, error)) (store.Op, error) {
	e.mu.Lock()
	e.doKeys = append(e.doKeys, key)
	for _, op := range e.PresetOps {
		if op.Key != key {
			continue
		}
		e.mu.Unlock()
		switch op.State {
		case "done":
			return op, nil
		case "failed":
			return op, fmt.Errorf("do %q: %w", key, runner.ErrPreviouslyFailed)
		default:
			return op, fmt.Errorf("do %q: %w", key, runner.ErrAmbiguous)
		}
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		e.mu.Unlock()
		return store.Op{}, fmt.Errorf("do %q: encode intent: %w", key, err)
	}
	e.nextID++
	op := store.Op{ID: e.nextID, Seq: len(e.PresetOps) + 1, Name: name, Key: key, Intent: intentJSON, State: "intended"}
	e.PresetOps = append(e.PresetOps, op)
	index := len(e.PresetOps) - 1
	e.mu.Unlock()

	result, fnErr := fn()
	if fnErr != nil && (errors.Is(fnErr, runner.ErrMaybeApplied) || ctx.Err() != nil ||
		errors.Is(fnErr, context.Canceled) || errors.Is(fnErr, context.DeadlineExceeded)) {
		return op, fmt.Errorf("do %q: %w", key, fnErr)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if fnErr != nil {
		failure, _ := json.Marshal(map[string]string{"error": fnErr.Error()})
		e.PresetOps[index].State, e.PresetOps[index].Result = "failed", failure
		op = e.PresetOps[index]
		return op, fnErr
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return op, fmt.Errorf("do %q: encode result: %w: %w", key, runner.ErrMaybeApplied, err)
	}
	e.PresetOps[index].State, e.PresetOps[index].Result = "done", resultJSON
	return e.PresetOps[index], nil
}
