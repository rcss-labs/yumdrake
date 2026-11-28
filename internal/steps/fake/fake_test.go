package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/manifest"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// recordingEnv is an in-memory runner.Env: Do runs fn once per key like the
// real one, and the lines and checkpoints are kept for assertions.
type recordingEnv struct {
	mu         sync.Mutex
	lines      []string
	ops        map[string]store.Op
	checkpoint []byte
}

func newRecordingEnv() *recordingEnv { return &recordingEnv{ops: map[string]store.Op{}} }

func (e *recordingEnv) Log(level, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lines = append(e.lines, level+": "+text)
}
func (e *recordingEnv) Resume(v any) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.checkpoint == nil {
		return false, nil
	}
	return true, json.Unmarshal(e.checkpoint, v)
}
func (e *recordingEnv) Checkpoint(_ context.Context, v any) error {
	b, err := json.Marshal(v)
	e.mu.Lock()
	e.checkpoint = b
	e.mu.Unlock()
	return err
}
func (e *recordingEnv) Ops(context.Context) ([]store.Op, error) { return nil, nil }
func (e *recordingEnv) CancelRequested(context.Context) bool    { return false }
func (e *recordingEnv) Do(_ context.Context, name, key string, _ any, fn func() (any, error)) (store.Op, error) {
	e.mu.Lock()
	if op, ok := e.ops[key]; ok && op.State == "done" {
		e.mu.Unlock()
		return op, nil
	}
	e.mu.Unlock()
	result, err := fn()
	if err != nil {
		return store.Op{}, err
	}
	b, _ := json.Marshal(result)
	op := store.Op{Name: name, Key: key, State: "done", Result: b}
	e.mu.Lock()
	e.ops[key] = op
	e.mu.Unlock()
	return op, nil
}

func (e *recordingEnv) text() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.lines, "\n")
}

func registered(t *testing.T, opts Options) map[string]runner.Executor {
	t.Helper()
	executors := map[string]runner.Executor{}
	Register(registrar(func(kind string, e runner.Executor) { executors[kind] = e }), opts)
	return executors
}

type registrar func(kind string, e runner.Executor)

func (r registrar) Register(kind string, e runner.Executor) { r(kind, e) }

func stepClaim(use, version string) queue.Claimed {
	return queue.Claimed{
		Leg:  store.Leg{ID: 7, RunID: 3, Kind: "step", Service: "billing", Version: version},
		Run:  store.Run{ID: 3, Environment: "prod-eu"},
		Step: &store.RunStep{Use: use, Name: "deploy"},
	}
}

func TestRegisterCoversEveryKind(t *testing.T) {
	executors := registered(t, Options{})
	for _, kind := range []string{"resolve", "revalidate", "copy-config", "ecs-deploy"} {
		if executors[kind] == nil {
			t.Errorf("no executor registered for %q", kind)
		}
	}
}

func TestEcsDeploySucceedsAfterStepTime(t *testing.T) {
	world := NewWorld()
	exec := registered(t, Options{StepTime: 30 * time.Millisecond, SlowFor: time.Hour, World: world})["ecs-deploy"]
	env := newRecordingEnv()
	start := time.Now()
	out := exec.Execute(context.Background(), env, stepClaim("ecs-deploy", "1.2.3"))
	if out.State != "succeeded" {
		t.Fatalf("outcome = %+v, want succeeded", out)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("took %v, want about StepTime", elapsed)
	}
	if got := world.Count("ecs-deploy/billing/1.2.3"); got != 1 {
		t.Errorf("world count = %d, want 1", got)
	}
	if strings.Count(env.text(), "\n") < 2 {
		t.Errorf("want several log lines, got %q", env.text())
	}
}

func TestEcsDeployVersionFailFailsWithHealthChecks(t *testing.T) {
	exec := registered(t, Options{World: NewWorld()})["ecs-deploy"]
	out := exec.Execute(context.Background(), newRecordingEnv(), stepClaim("ecs-deploy", "2.0.0-fail"))
	if out.State != "failed" || out.FailureShort != "health checks failed" {
		t.Fatalf("outcome = %+v, want failed with %q", out, "health checks failed")
	}
}

func TestEcsDeployVersionUnknownReturnsUnknown(t *testing.T) {
	exec := registered(t, Options{World: NewWorld()})["ecs-deploy"]
	out := exec.Execute(context.Background(), newRecordingEnv(), stepClaim("ecs-deploy", "unknown-1"))
	if out.State != "unknown" {
		t.Fatalf("outcome = %+v, want unknown", out)
	}
}

func TestEcsDeployVersionSlowTakesSlowFor(t *testing.T) {
	exec := registered(t, Options{StepTime: time.Millisecond, SlowFor: 80 * time.Millisecond, World: NewWorld()})["ecs-deploy"]
	start := time.Now()
	out := exec.Execute(context.Background(), newRecordingEnv(), stepClaim("ecs-deploy", "slow-1"))
	if out.State != "succeeded" {
		t.Fatalf("outcome = %+v", out)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Errorf("took %v, want at least SlowFor", elapsed)
	}
}

func TestEcsDeployStopsWhenContextEnds(t *testing.T) {
	exec := registered(t, Options{StepTime: time.Hour, World: NewWorld()})["ecs-deploy"]
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	out := exec.Execute(ctx, newRecordingEnv(), stepClaim("ecs-deploy", "1.0"))
	if out.State == "succeeded" {
		t.Fatalf("outcome = %+v, want a cancelled run to not succeed", out)
	}
}

func TestEcsDeployAppliesItsChangeOnceAcrossAttempts(t *testing.T) {
	world := NewWorld()
	exec := registered(t, Options{World: world})["ecs-deploy"]
	env := newRecordingEnv()
	for range 2 {
		if out := exec.Execute(context.Background(), env, stepClaim("ecs-deploy", "1.0")); out.State != "succeeded" {
			t.Fatalf("outcome = %+v", out)
		}
	}
	if got := world.Count("ecs-deploy/billing/1.0"); got != 1 {
		t.Errorf("world count after two attempts = %d, want 1", got)
	}
}

func TestEcsDeployReconcilesAmbiguousFromWorld(t *testing.T) {
	world := NewWorld()
	world.Apply("ecs-deploy/billing/1.0") // the earlier attempt changed the system, then died
	exec := registered(t, Options{World: world})["ecs-deploy"]
	env := &ambiguousEnv{recordingEnv: newRecordingEnv()}
	out := exec.Execute(context.Background(), env, stepClaim("ecs-deploy", "1.0"))
	if out.State != "succeeded" {
		t.Fatalf("outcome = %+v, want succeeded after reconciling", out)
	}
	if got := world.Count("ecs-deploy/billing/1.0"); got != 1 {
		t.Errorf("world count = %d, want still 1", got)
	}
}

// ambiguousEnv answers every Do as an intended-only operation.
type ambiguousEnv struct{ *recordingEnv }

func (e *ambiguousEnv) Do(_ context.Context, name, key string, _ any, _ func() (any, error)) (store.Op, error) {
	return store.Op{Name: name, Key: key, State: "intended"}, runner.ErrAmbiguous
}

func TestCopyConfigAppliesChangeAndSucceeds(t *testing.T) {
	world := NewWorld()
	exec := registered(t, Options{World: world})["copy-config"]
	out := exec.Execute(context.Background(), newRecordingEnv(), stepClaim("copy-config", "1.0"))
	if out.State != "succeeded" || world.Count("copy-config/billing/1.0") != 1 {
		t.Fatalf("outcome = %+v, count = %d", out, world.Count("copy-config/billing/1.0"))
	}
}

func TestResolveReturnsManifestListingTheSelection(t *testing.T) {
	exec := registered(t, Options{})["resolve"]
	claim := queue.Claimed{
		Leg: store.Leg{ID: 1, RunID: 3, Kind: "resolve"},
		Run: store.Run{ID: 3, Selection: []byte(`{"web":{"Version":"1.2.3","Services":{"billing":"","search":"1.2.4"}}}`)},
	}
	out := exec.Execute(context.Background(), newRecordingEnv(), claim)
	if out.State != "succeeded" {
		t.Fatalf("outcome = %+v", out)
	}
	var plan manifest.Manifest
	if err := json.Unmarshal(out.Result, &plan); err != nil {
		t.Fatalf("Result %s is not JSON: %v", out.Result, err)
	}
	// The engine takes only a plan that is this run's own, with its identity.
	if plan.RunID != 3 || plan.ID == "" || plan.ID != manifest.Identity(plan) {
		t.Errorf("manifest run %d, id %q: want run 3 and its own identity", plan.RunID, plan.ID)
	}
	got := map[string]string{}
	for _, s := range plan.Services {
		got[s.Name] = s.Version
		if s.PreviousTaskDefinition == "" {
			t.Errorf("service %s has no previous task definition, so the service page could offer no restore", s.Name)
		}
	}
	if len(got) != 2 || got["billing"] != "1.2.3" || got["search"] != "1.2.4" {
		t.Errorf("manifest services = %v, want billing 1.2.3 and search 1.2.4", got)
	}
}

func TestRevalidateFailsForStaleEnvironment(t *testing.T) {
	exec := registered(t, Options{})["revalidate"]
	ok := exec.Execute(context.Background(), newRecordingEnv(), queue.Claimed{Leg: store.Leg{Kind: "revalidate"}, Run: store.Run{Environment: "prod-eu"}})
	if ok.State != "succeeded" {
		t.Errorf("fresh environment: outcome = %+v, want succeeded", ok)
	}
	stale := exec.Execute(context.Background(), newRecordingEnv(), queue.Claimed{Leg: store.Leg{Kind: "revalidate"}, Run: store.Run{Environment: "stale-eu"}})
	if stale.State != "failed" || stale.FailureShort == "" {
		t.Errorf("stale environment: outcome = %+v, want failed with a reason", stale)
	}
}

func TestWorldIsSafeForConcurrentUse(t *testing.T) {
	world := NewWorld()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() { defer wg.Done(); world.Apply("k") }()
	}
	wg.Wait()
	if world.Count("k") != 50 || world.Count("other") != 0 {
		t.Errorf("counts = %d, %d", world.Count("k"), world.Count("other"))
	}
}

// maybeAppliedEnv answers every Do as a change that was made but could not be
// recorded, which Env.Do reports by wrapping runner.ErrMaybeApplied.
type maybeAppliedEnv struct{ *recordingEnv }

func (e *maybeAppliedEnv) Do(_ context.Context, name, key string, _ any, fn func() (any, error)) (store.Op, error) {
	_, _ = fn() // the change is applied; only recording it failed
	return store.Op{Name: name, Key: key, State: "intended"}, fmt.Errorf("do %q: %w", key, runner.ErrMaybeApplied)
}

func TestEcsDeployReportsUnknownNotFailedWhenTheChangeMayHaveApplied(t *testing.T) {
	exec := registered(t, Options{World: NewWorld()})["ecs-deploy"]
	out := exec.Execute(context.Background(), &maybeAppliedEnv{recordingEnv: newRecordingEnv()}, stepClaim("ecs-deploy", "1.0"))
	if out.State != "unknown" {
		t.Fatalf("outcome = %+v, want unknown: the change was applied but not recorded", out)
	}
}
