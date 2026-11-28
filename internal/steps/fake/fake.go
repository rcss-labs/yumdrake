// Package fake holds step executors that change nothing real. They exist for
// development and tests: a person can try every outcome from the UI by the
// version they pick, and tests drive them with Options. The runner registers
// them only when YUMDRAKE_ENV=dev and YUMDRAKE_FAKE_STEPS=true.
//
// The version string selects the outcome of the ecs-deploy fake: containing
// "fail" it fails with "health checks failed", containing "unknown" it ends
// unknown, containing "slow" it takes Options.SlowFor, otherwise it succeeds
// after Options.StepTime.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
)

// Options tune the fakes.
type Options struct {
	// StepTime is how long a normal fake step takes. SlowFor is how long a
	// version containing "slow" takes.
	StepTime, SlowFor time.Duration
	// World is the pretend external system. Nil means each Register creates a
	// private one.
	World *World
}

// World is a concurrency-safe stand-in for the external system. It records
// each applied change by operation key with a count, so a test can prove a
// change happened exactly once and a fake that sees runner.ErrAmbiguous can
// check whether the change is already in place.
type World struct {
	mu      sync.Mutex
	applied map[string]int
}

// NewWorld returns an empty world.
func NewWorld() *World { return &World{applied: map[string]int{}} }

// Apply records one application of the change named key.
func (w *World) Apply(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.applied[key]++
}

// Count is how many times the change named key was applied.
func (w *World) Count(key string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.applied[key]
}

// Keys lists the applied changes, sorted.
func (w *World) Keys() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	keys := make([]string, 0, len(w.applied))
	for k := range w.applied {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Registrar is what Register needs; *runner.Runner satisfies it.
type Registrar interface {
	Register(kind string, e runner.Executor)
}

// Register adds the fakes for resolve, revalidate, copy-config and ecs-deploy.
func Register(r Registrar, opts Options) {
	f := newFakes(opts)
	r.Register("resolve", runner.Executor(executorFunc(f.resolve)))
	r.Register("revalidate", runner.Executor(executorFunc(f.revalidate)))
	deploySteps(r, f)
}

// RegisterDeploySteps adds only the fakes for copy-config and ecs-deploy, so
// the real resolve and revalidate stay in place.
func RegisterDeploySteps(r Registrar, opts Options) {
	deploySteps(r, newFakes(opts))
}

func newFakes(opts Options) *fakes {
	if opts.World == nil {
		opts.World = NewWorld()
	}
	return &fakes{opts: opts}
}

func deploySteps(r Registrar, f *fakes) {
	r.Register("copy-config", runner.Executor(executorFunc(f.copyConfig)))
	r.Register("ecs-deploy", runner.Executor(executorFunc(f.ecsDeploy)))
}

type executorFunc func(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome

func (f executorFunc) Execute(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	return f(ctx, env, task)
}

type fakes struct{ opts Options }

// pause waits d or until ctx ends; false means ctx ended first.
func pause(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func stopped(env runner.Env, ctx context.Context) queue.Outcome {
	if env.CancelRequested(context.WithoutCancel(ctx)) {
		return queue.Outcome{State: "cancelled", FailureShort: "cancelled on request"}
	}
	return queue.Outcome{State: "unknown", FailureShort: "runner stopped", FailureDetail: "the fake step was interrupted"}
}

// selectionEntry mirrors engine.GroupSelection's JSON (no field tags).
type selectionEntry struct {
	Version  string
	Services map[string]string
}

func (f *fakes) resolve(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	env.Log("info", "resolving the selection against the inventory snapshot")
	var selection map[string]selectionEntry
	if len(task.Run.Selection) > 0 {
		if err := json.Unmarshal(task.Run.Selection, &selection); err != nil {
			return queue.Outcome{State: "failed", FailureShort: "selection is not valid", FailureDetail: err.Error()}
		}
	}
	groups := make([]string, 0, len(selection))
	for g := range selection {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	services := []manifest.Service{}
	for _, g := range groups {
		names := make([]string, 0, len(selection[g].Services))
		for s := range selection[g].Services {
			names = append(names, s)
		}
		sort.Strings(names)
		for _, s := range names {
			version := selection[g].Services[s]
			if version == "" {
				version = selection[g].Version
			}
			services = append(services, manifest.Service{Name: s, Group: g, Version: version, Config: []manifest.ConfigItem{}})
			env.Log("info", fmt.Sprintf("selected %s %s in group %s", s, version, g))
		}
	}
	if !pause(ctx, f.opts.StepTime) {
		return stopped(env, ctx)
	}
	// A real manifest of this run: the engine refuses a plan that is not.
	plan := manifest.Manifest{
		RunID: task.Run.ID, Environment: task.Run.Environment, Label: task.Run.Label, AWSAccount: task.Run.AWSAccount,
		InventoryCommit: task.Run.InventoryCommit, JobCommit: task.Run.JobCommit, Services: services, Checks: []manifest.Check{},
	}
	plan.ID = manifest.Identity(plan)
	encoded, err := json.Marshal(plan)
	if err != nil {
		return queue.Outcome{State: "failed", FailureShort: "could not build the manifest", FailureDetail: err.Error()}
	}
	env.Log("ok", fmt.Sprintf("resolved %d services", len(services)))
	return queue.Outcome{State: "succeeded", Result: encoded}
}

func (f *fakes) revalidate(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	env.Log("info", "checking that the plan still matches "+task.Run.Environment)
	if !pause(ctx, f.opts.StepTime) {
		return stopped(env, ctx)
	}
	if strings.Contains(task.Run.Environment, "stale") {
		env.Log("error", "the environment changed since the plan was made")
		return queue.Outcome{State: "failed", FailureShort: "plan is stale", FailureDetail: "environment " + task.Run.Environment + " changed since the plan was made"}
	}
	env.Log("ok", "plan is still valid")
	return queue.Outcome{State: "succeeded", Result: []byte(`{}`)}
}

func (f *fakes) copyConfig(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	return f.change(ctx, env, task, "copy-config", "copying configuration", "configuration copied")
}

func (f *fakes) ecsDeploy(ctx context.Context, env runner.Env, task queue.Claimed) queue.Outcome {
	version := task.Leg.Version
	if strings.Contains(version, "fail") {
		env.Log("info", fmt.Sprintf("deploying %s %s", task.Leg.Service, version))
		if !pause(ctx, f.opts.StepTime) {
			return stopped(env, ctx)
		}
		env.Log("error", "tasks started but the target group reports unhealthy")
		return queue.Outcome{State: "failed", FailureShort: "health checks failed", FailureDetail: "the new tasks did not become healthy"}
	}
	if strings.Contains(version, "unknown") {
		env.Log("info", fmt.Sprintf("deploying %s %s", task.Leg.Service, version))
		env.Log("warn", "lost contact with the cluster; the outcome is not known")
		return queue.Outcome{State: "unknown", FailureShort: "outcome unknown", FailureDetail: "lost contact with the cluster"}
	}
	return f.change(ctx, env, task, "ecs-deploy", "registering task definition and updating the service", "service is stable")
}

// change is the shared shape of the real steps: log, make one pretend change
// through Env.Do, reconcile when an earlier attempt left it ambiguous.
func (f *fakes) change(ctx context.Context, env runner.Env, task queue.Claimed, use, doing, done string) queue.Outcome {
	key := fmt.Sprintf("%s/%s/%s", use, task.Leg.Service, task.Leg.Version)
	duration := f.opts.StepTime
	if strings.Contains(task.Leg.Version, "slow") {
		duration = f.opts.SlowFor
	}
	env.Log("info", fmt.Sprintf("%s: %s %s", use, task.Leg.Service, task.Leg.Version))
	env.Log("info", doing)
	_, err := env.Do(ctx, use, key, map[string]string{"service": task.Leg.Service, "version": task.Leg.Version}, func() (any, error) {
		if !pause(ctx, duration) {
			return nil, ctx.Err()
		}
		f.opts.World.Apply(key)
		return map[string]string{"applied": key}, nil
	})
	switch {
	case err == nil:
	case errors.Is(err, runner.ErrAmbiguous):
		env.Log("warn", "an earlier attempt may have made this change; checking the system")
		if f.opts.World.Count(key) == 0 {
			return queue.Outcome{State: "unknown", FailureShort: "change not in place", FailureDetail: "the earlier attempt's change is not visible in the system"}
		}
		env.Log("info", "the change is already in place")
	case errors.Is(err, runner.ErrMaybeApplied):
		// The change may be in place although nothing recorded it: unknown, not
		// failed, so the run keeps its locks until a person reconciles.
		env.Log("warn", "the change may have been applied but could not be recorded")
		return queue.Outcome{State: "unknown", FailureShort: "outcome unknown", FailureDetail: err.Error()}
	case ctx.Err() != nil:
		return stopped(env, ctx)
	default:
		env.Log("error", err.Error())
		return queue.Outcome{State: "failed", FailureShort: "change failed", FailureDetail: err.Error()}
	}
	env.Log("ok", done)
	return queue.Outcome{State: "succeeded", Result: []byte(`{}`)}
}
