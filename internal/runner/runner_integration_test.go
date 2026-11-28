//go:build integration

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

const testLease = 30 * time.Second

// fakeClock is a settable clock for the queue, so a test can expire leases
// without waiting for them.
type fakeClock struct{ nanos atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// sinkEvent is one call to the recording sink.
type sinkEvent struct {
	kind        string // "write" | "flush"
	legID, gen  int64
	level, text string
}

// recordingSink satisfies LineSink and remembers every call in order.
type recordingSink struct {
	mu      sync.Mutex
	events  []sinkEvent
	onFlush func(legID int64)
	// failAfter, when above zero, makes Write refuse lines once that many events are recorded.
	failAfter int
}

func (s *recordingSink) Write(_, legID, generation int64, level, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAfter > 0 && len(s.events) >= s.failAfter {
		return errors.New("log buffer is full")
	}
	s.events = append(s.events, sinkEvent{kind: "write", legID: legID, gen: generation, level: level, text: text})
	return nil
}

func (s *recordingSink) Flush(_ context.Context, legID int64) error {
	s.mu.Lock()
	s.events = append(s.events, sinkEvent{kind: "flush", legID: legID})
	hook := s.onFlush
	s.mu.Unlock()
	if hook != nil {
		hook(legID)
	}
	return nil
}

func (s *recordingSink) snapshot() []sinkEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkEvent(nil), s.events...)
}

// execFunc is a scripted executor.
type execFunc func(ctx context.Context, env Env, task queue.Claimed) queue.Outcome

func (f execFunc) Execute(ctx context.Context, env Env, task queue.Claimed) queue.Outcome {
	return f(ctx, env, task)
}

type harness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	clock  *fakeClock
	q      *queue.Queue
	runID  int64
	stepID int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	url := testdb.New(t)
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)

	user, err := store.CreateLocalUser(ctx, pool, "runner@example.com", "Runner", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	folder, err := store.CreateFolder(ctx, pool, "runner-folder")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	source, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: folder.ID, Repo: "acme/jobs", Branch: "main", Path: "deploy.yaml"})
	if err != nil {
		t.Fatalf("CreateJobSource: %v", err)
	}
	snapshot, err := store.ActivateSnapshot(ctx, pool, "runner-inventory", []byte(`{}`), nil, time.Now())
	if err != nil {
		t.Fatalf("ActivateSnapshot: %v", err)
	}
	run, err := store.InsertRun(ctx, pool, store.Run{
		Kind: "deploy", InventorySnapshotID: snapshot.ID, JobSourceID: source.ID, JobID: "deploy-api", JobSnapshot: []byte(`{"id":"deploy-api"}`),
		Environment: "prod-eu", AWSAccount: "111122223333", FolderID: folder.ID, Selection: []byte(`{}`),
		State: "running", RequestedBy: user.ID, Via: "web",
	}, []store.RunStep{{Name: "deploy", Use: "ecs-deploy", Position: 0, Needs: []byte(`[]`)}}, nil)
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	steps, err := store.StepsOfRun(ctx, pool, run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("StepsOfRun: %v", err)
	}
	clock := newFakeClock()
	return &harness{t: t, pool: pool, clock: clock, q: queue.New(pool, testLease, clock.Now), runID: run.ID, stepID: steps[0].ID}
}

// addLeg inserts a queued step leg.
func (h *harness) addLeg(service string) store.Leg {
	h.t.Helper()
	queuedAt := h.clock.Now()
	l, err := store.InsertLeg(context.Background(), h.pool, store.Leg{
		RunID: h.runID, StepID: &h.stepID, Kind: "step", Service: service, Version: "1.0",
		State: "queued", QueuedAt: &queuedAt,
	})
	if err != nil {
		h.t.Fatalf("InsertLeg: %v", err)
	}
	return l
}

func (h *harness) leg(id int64) store.Leg {
	h.t.Helper()
	l, err := store.GetLeg(context.Background(), h.pool, id)
	if err != nil {
		h.t.Fatalf("GetLeg: %v", err)
	}
	return l
}

// expireAndSweep moves the clock past the lease until Sweep requeues exactly
// one leg. A heartbeat that lands between the clock move and the Sweep renews
// the lease, so the move is repeated.
func (h *harness) expireAndSweep() {
	h.t.Helper()
	for range 200 {
		h.clock.Advance(testLease + time.Second)
		requeued, _, err := h.q.Sweep(context.Background())
		if err != nil {
			h.t.Fatalf("Sweep: %v", err)
		}
		if requeued == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("Sweep never requeued the leg")
}

// abandonedAttempt claims the leg as runner "gone", lets the test act as that
// runner through f, then expires its lease and sweeps, as if it had died.
func (h *harness) abandonedAttempt(f func(c *queue.Claimed)) {
	h.t.Helper()
	c, err := h.q.Claim(context.Background(), "gone")
	if err != nil || c == nil {
		h.t.Fatalf("Claim = %v, %v", c, err)
	}
	if err := h.q.MarkRunning(context.Background(), c.Leg.ID, c.Leg.Generation); err != nil {
		h.t.Fatalf("MarkRunning: %v", err)
	}
	f(c)
	h.expireAndSweep()
}

type runnerOptions struct {
	id              string
	concurrency     int
	heartbeat, poll time.Duration
	shutdown        time.Duration
	executors       map[string]Executor
	sink            LineSink
	onSettled       func(runID int64)
	logTo           io.Writer // the runner's own (service) log; default discarded
}

// lockedBuilder is a strings.Builder that logging goroutines may write to.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type runningRunner struct {
	cancel context.CancelFunc
	done   chan error
}

// stop cancels the runner and waits for Run to return.
func (rr *runningRunner) stop(t *testing.T) error {
	t.Helper()
	rr.cancel()
	select {
	case err := <-rr.done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
		return nil
	}
}

func (h *harness) startRunner(q *queue.Queue, o runnerOptions) *runningRunner {
	h.t.Helper()
	if o.id == "" {
		o.id = "runner-1"
	}
	if o.concurrency == 0 {
		o.concurrency = 1
	}
	if o.heartbeat == 0 {
		o.heartbeat = 10 * time.Millisecond
	}
	if o.poll == 0 {
		o.poll = time.Hour // tests wake the runner through the leg finishing, not the poll
	}
	if o.shutdown == 0 {
		o.shutdown = 5 * time.Second
	}
	if o.sink == nil {
		o.sink = &recordingSink{}
	}
	logTo := o.logTo
	if logTo == nil {
		logTo = io.Discard
	}
	r := New(q, o.sink, o.id, o.concurrency, o.heartbeat, o.poll, slog.New(slog.NewTextHandler(logTo, nil))).
		WithShutdownTimeout(o.shutdown).WithSettledNotifier(o.onSettled)
	for kind, e := range o.executors {
		r.Register(kind, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rr := &runningRunner{cancel: cancel, done: make(chan error, 1)}
	go func() { rr.done <- r.Run(ctx, nil) }()
	h.t.Cleanup(func() { rr.cancel() })
	return rr
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func succeed(result string) queue.Outcome {
	return queue.Outcome{State: "succeeded", Result: []byte(result)}
}

func TestRunnerExecutesClaimedLegAndCompletes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")

	var sawTask atomic.Value
	sink := &recordingSink{}
	var stateAtFlush atomic.Value
	sink.onFlush = func(legID int64) {
		l, _ := store.GetLeg(context.Background(), h.pool, legID)
		stateAtFlush.Store(l.State)
	}
	rr := h.startRunner(h.q, runnerOptions{sink: sink, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(_ context.Context, env Env, task queue.Claimed) queue.Outcome {
			sawTask.Store(task)
			env.Log("info", "deploying")
			env.Log("ok", "done")
			return succeed(`{"deployed":true}`)
		}),
	}})
	waitFor(t, "leg to succeed", func() bool { return h.leg(leg.ID).State == "succeeded" })
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}

	got := h.leg(leg.ID)
	task, _ := sawTask.Load().(queue.Claimed)
	if task.Leg.ID != leg.ID || task.Step == nil || task.Step.Use != "ecs-deploy" {
		t.Errorf("executor was given %+v", task)
	}
	if got.Attempt != 1 || got.FinishedAt == nil || got.StartedAt == nil {
		t.Errorf("leg = %+v, want attempt 1, started and finished", got)
	}
	var result map[string]bool
	if err := json.Unmarshal(got.Result, &result); err != nil || !result["deployed"] {
		t.Errorf("Result = %s (%v)", got.Result, err)
	}

	events := sink.snapshot()
	if len(events) != 3 || events[0].text != "deploying" || events[1].text != "done" || events[2].kind != "flush" {
		t.Fatalf("sink events = %+v, want two writes then one flush", events)
	}
	for _, e := range events[:2] {
		if e.legID != leg.ID || e.gen != got.Generation {
			t.Errorf("line %+v is not stamped with leg %d generation %d", e, leg.ID, got.Generation)
		}
	}
	if s, _ := stateAtFlush.Load().(string); s != "running" {
		t.Errorf("leg was %q when the lines were flushed, want running (flush before Complete)", s)
	}
}

func TestRunnerHonoursConcurrencyLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var ids []int64
	for _, service := range []string{"a", "b", "c", "d", "e"} {
		ids = append(ids, h.addLeg(service).ID)
	}
	var active, peak atomic.Int32
	release := make(chan struct{})
	rr := h.startRunner(h.q, runnerOptions{concurrency: 2, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, _ Env, _ queue.Claimed) queue.Outcome {
			n := active.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			defer active.Add(-1)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return succeed(`{}`)
		}),
	}})

	count := func(states ...string) int {
		n := 0
		for _, id := range ids {
			for _, s := range states {
				if h.leg(id).State == s {
					n++
				}
			}
		}
		return n
	}
	waitFor(t, "two legs to be running", func() bool { return active.Load() == 2 })
	time.Sleep(150 * time.Millisecond) // a third claim, if the limit were missing, would land by now
	if got := count("claimed", "running"); got != 2 {
		t.Fatalf("%d legs are held while 2 are executing, want 2", got)
	}
	if got := count("queued"); got != 3 {
		t.Fatalf("%d legs are still queued, want 3", got)
	}
	close(release)
	waitFor(t, "all legs to succeed", func() bool { return count("succeeded") == 5 })
	if p := peak.Load(); p > 2 {
		t.Errorf("peak concurrency = %d, want at most 2", p)
	}
	_ = rr.stop(t)
}

func TestUnregisteredKindFailsLegWithReason(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	rr := h.startRunner(h.q, runnerOptions{executors: map[string]Executor{
		"copy-config": execFunc(func(context.Context, Env, queue.Claimed) queue.Outcome { return succeed(`{}`) }),
	}})
	waitFor(t, "leg to settle", func() bool { return h.leg(leg.ID).State == "failed" })
	got := h.leg(leg.ID)
	if !strings.Contains(got.FailureShort, "no executor") || !strings.Contains(got.FailureDetail, `"ecs-deploy"`) {
		t.Errorf("failure = %q / %q, want it to say no executor is registered for ecs-deploy", got.FailureShort, got.FailureDetail)
	}
	_ = rr.stop(t)
}

func TestHeartbeatKeepsLeaseDuringLongLeg(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	// A real clock and a short lease: the leg below outlives three leases. The
	// lease is long enough, and the heartbeat short enough (lease/7), that a
	// single stalled database call or scheduler pause of a second under load
	// does not let the lease lapse; a stall of 730 ms was measured under load.
	const lease = 1400 * time.Millisecond
	const heartbeat = 200 * time.Millisecond
	const legDuration = 3*lease + 200*time.Millisecond
	q := queue.New(h.pool, lease, time.Now)

	var calls atomic.Int32
	rr := h.startRunner(q, runnerOptions{heartbeat: heartbeat, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(context.Context, Env, queue.Claimed) queue.Outcome {
			calls.Add(1)
			time.Sleep(legDuration)
			return succeed(`{}`)
		}),
	}})
	sweeperStop := make(chan struct{})
	sweeperDone := make(chan struct{})
	go func() {
		defer close(sweeperDone)
		for {
			select {
			case <-sweeperStop:
				return
			case <-time.After(20 * time.Millisecond):
				_, _, _ = q.Sweep(context.Background())
			}
		}
	}()
	waitFor(t, "leg to settle or be swept back", func() bool {
		l := h.leg(leg.ID)
		return l.Attempt > 0 && (l.State == "succeeded" || l.State == "queued" || l.State == "unknown")
	})
	close(sweeperStop)
	<-sweeperDone
	got := h.leg(leg.ID)
	if got.State != "succeeded" || got.Attempt != 1 || calls.Load() != 1 {
		t.Errorf("leg = state %s attempt %d, executor calls %d; want succeeded in one attempt", got.State, got.Attempt, calls.Load())
	}
	_ = rr.stop(t)
}

// Review Focus 3: a runner whose lease was taken away is told to stop, and
// whatever it returns afterwards changes nothing.
func TestLeaseLostCancelsExecutorAndDropsItsResult(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")

	started := make(chan struct{})
	sawCancel := make(chan struct{})
	letGo := make(chan struct{})
	var executions atomic.Int32
	sink := &recordingSink{}
	rr := h.startRunner(h.q, runnerOptions{id: "stale", sink: sink, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			executions.Add(1)
			env.Log("info", "stale runner still talking")
			close(started)
			select {
			case <-ctx.Done(): // only the heartbeat noticing the lost lease can end the context
			case <-time.After(20 * time.Second):
			}
			close(sawCancel)
			<-letGo
			return succeed(`{"from":"stale"}`)
		}),
	}})
	<-started
	h.expireAndSweep()
	// An in-flight heartbeat of the stale runner can hold the row lock for a
	// moment, and Claim skips locked rows, so it may come back empty once.
	var fresh *queue.Claimed
	var err error
	for range 200 {
		if fresh, err = h.q.Claim(context.Background(), "fresh"); err != nil || fresh != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || fresh == nil || fresh.Leg.Generation != 2 {
		t.Fatalf("Claim by fresh = %+v, %v", fresh, err)
	}
	select {
	case <-sawCancel:
	case <-time.After(10 * time.Second):
		t.Fatal("the stale executor's context was never cancelled")
	}
	if ctxErr := h.leg(leg.ID); ctxErr.State != "claimed" {
		t.Fatalf("leg state %s", ctxErr.State)
	}

	close(letGo)
	// Run returns only after the stale leg goroutine has flushed and tried to
	// complete, so what follows sees the outcome of that refused Complete.
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	got := h.leg(leg.ID)
	if got.State != "claimed" || got.RunnerID != "fresh" || got.Generation != 2 || got.Result != nil {
		t.Fatalf("leg = state %s runner %q generation %d result %s; the stale result must not touch it", got.State, got.RunnerID, got.Generation, got.Result)
	}
	for _, e := range sink.snapshot() {
		if e.kind == "write" && e.gen != 1 {
			t.Errorf("stale line carries generation %d, want 1 so the sink can refuse it", e.gen)
		}
	}
	if err := h.q.Complete(context.Background(), leg.ID, 2, queue.Outcome{State: "succeeded", Result: []byte(`{"from":"fresh"}`)}); err != nil {
		t.Fatalf("fresh Complete: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(h.leg(leg.ID).Result, &result); err != nil || result["from"] != "fresh" {
		t.Errorf("Result = %s, want the fresh attempt's", h.leg(leg.ID).Result)
	}
	if n := executions.Load(); n != 1 {
		t.Errorf("stale runner executed %d times, want once", n)
	}
}

// afterAbandonedOp leaves behind an operation written by an attempt that has
// since died (done when finish, else only intended), then starts a runner that
// runs exec as the next attempt.
func (h *harness) afterAbandonedOp(finish bool, exec Executor) (legID int64, rr *runningRunner) {
	h.t.Helper()
	leg := h.addLeg("billing")
	h.abandonedAttempt(func(c *queue.Claimed) {
		op, err := h.q.Intend(context.Background(), c.Leg.ID, c.Leg.Generation, "deploy", "deploy-1", []byte(`{"image":"app:1"}`))
		if err != nil {
			h.t.Fatalf("Intend: %v", err)
		}
		if finish {
			if err := h.q.Finish(context.Background(), c.Leg.ID, c.Leg.Generation, op.ID, true, []byte(`{"arn":"task-1"}`)); err != nil {
				h.t.Fatalf("Finish: %v", err)
			}
		}
	})
	return leg.ID, h.startRunner(h.q, runnerOptions{id: "second", executors: map[string]Executor{"ecs-deploy": exec}})
}

type doResult struct {
	op  store.Op
	err error
}

func TestDoSkipsFnWhenOperationAlreadyDone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var fnCalls atomic.Int32
	var got atomic.Value
	legID, rr := h.afterAbandonedOp(true, execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
		op, err := env.Do(ctx, "deploy", "deploy-1", map[string]string{"image": "app:1"}, func() (any, error) {
			fnCalls.Add(1)
			return map[string]string{"arn": "task-2"}, nil
		})
		got.Store(doResult{op, err})
		return succeed(`{}`)
	}))
	waitFor(t, "leg to succeed", func() bool { return h.leg(legID).State == "succeeded" })
	_ = rr.stop(t)
	r, _ := got.Load().(doResult)
	if r.err != nil {
		t.Fatalf("Do = %v, want nil for a done operation", r.err)
	}
	if fnCalls.Load() != 0 {
		t.Errorf("fn ran %d times, want 0: the change was already made", fnCalls.Load())
	}
	if r.op.State != "done" || !strings.Contains(string(r.op.Result), "task-1") {
		t.Errorf("Do returned %+v, want the earlier attempt's done operation", r.op)
	}
}

func TestDoReportsAmbiguousWhenOnlyIntended(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var fnCalls atomic.Int32
	var got atomic.Value
	legID, rr := h.afterAbandonedOp(false, execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
		op, err := env.Do(ctx, "deploy", "deploy-1", map[string]string{"image": "app:1"}, func() (any, error) {
			fnCalls.Add(1)
			return nil, nil
		})
		got.Store(doResult{op, err})
		return succeed(`{}`)
	}))
	waitFor(t, "leg to succeed", func() bool { return h.leg(legID).State == "succeeded" })
	_ = rr.stop(t)
	r, _ := got.Load().(doResult)
	if !errors.Is(r.err, ErrAmbiguous) {
		t.Fatalf("Do error = %v, want ErrAmbiguous", r.err)
	}
	if fnCalls.Load() != 0 {
		t.Errorf("fn ran %d times, want 0: it may already have changed something", fnCalls.Load())
	}
	if r.op.State != "intended" || r.op.Key != "deploy-1" {
		t.Errorf("Do returned %+v, want the intended operation so the executor can reconcile", r.op)
	}
}

func TestDoRecordsFailedOperationAndRefusesToRepeatIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	var first, second atomic.Value
	var fnCalls atomic.Int32
	rr := h.startRunner(h.q, runnerOptions{executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			fn := func() (any, error) { fnCalls.Add(1); return nil, errors.New("service not found") }
			_, err := env.Do(ctx, "deploy", "deploy-1", nil, fn)
			first.Store(doResult{err: err})
			_, err = env.Do(ctx, "deploy", "deploy-1", nil, fn)
			second.Store(doResult{err: err})
			return queue.Outcome{State: "failed", FailureShort: "deploy failed"}
		}),
	}})
	waitFor(t, "leg to fail", func() bool { return h.leg(leg.ID).State == "failed" })
	_ = rr.stop(t)
	f, _ := first.Load().(doResult)
	s, _ := second.Load().(doResult)
	if f.err == nil || !strings.Contains(f.err.Error(), "service not found") {
		t.Errorf("first Do error = %v, want fn's error", f.err)
	}
	if !errors.Is(s.err, ErrPreviouslyFailed) {
		t.Errorf("second Do error = %v, want ErrPreviouslyFailed", s.err)
	}
	if fnCalls.Load() != 1 {
		t.Errorf("fn ran %d times, want 1", fnCalls.Load())
	}
	ops, err := store.OpsOfLeg(context.Background(), h.pool, leg.ID)
	if err != nil || len(ops) != 1 || ops[0].State != "failed" {
		t.Errorf("ops = %+v, %v; want one failed operation", ops, err)
	}
}

// externalSystem stands in for the thing a step changes (AWS, say). It counts
// how often each change was applied.
type externalSystem struct {
	mu      sync.Mutex
	applied map[string]int
}

func (e *externalSystem) apply(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.applied == nil {
		e.applied = map[string]int{}
	}
	e.applied[key]++
}

func (e *externalSystem) count(key string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applied[key]
}

// Review Focus 1: the process dies after the external change but before the
// operation is recorded as done. The next attempt must find out, not repeat it.
func TestCrashAfterChangeBeforeRecordIsReconciledByNextAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	system := &externalSystem{}
	const key = "deploy-billing"

	applied := make(chan struct{})
	die := make(chan struct{}) // the first runner "dies" inside fn, never recording the result
	first := h.startRunner(h.q, runnerOptions{id: "first", heartbeat: time.Hour, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			_, err := env.Do(ctx, "deploy", key, map[string]string{"image": "app:1"}, func() (any, error) {
				system.apply(key)
				close(applied)
				<-die
				return nil, errors.New("process gone")
			})
			return queue.Outcome{State: "unknown", FailureDetail: "first runner: " + errString(err)}
		}),
	}})
	defer func() { close(die); _ = first.stop(t) }()
	<-applied
	h.expireAndSweep()

	var secondFnCalls atomic.Int32
	var doErr atomic.Value
	second := h.startRunner(h.q, runnerOptions{id: "second", executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			op, err := env.Do(ctx, "deploy", key, map[string]string{"image": "app:1"}, func() (any, error) {
				secondFnCalls.Add(1)
				system.apply(key)
				return nil, nil
			})
			doErr.Store(doResult{err: err})
			if !errors.Is(err, ErrAmbiguous) {
				return queue.Outcome{State: "unknown", FailureDetail: "expected ErrAmbiguous, got " + errString(err)}
			}
			if system.count(key) != 1 { // reconcile against the real system
				return queue.Outcome{State: "failed", FailureShort: "change is not in place"}
			}
			return succeed(`{"reconciled":"` + op.Key + `"}`)
		}),
	}})
	waitFor(t, "second attempt to settle the leg", func() bool { return h.leg(leg.ID).State == "succeeded" })
	_ = second.stop(t)

	got := h.leg(leg.ID)
	if got.Attempt != 2 || got.Generation != 2 {
		t.Errorf("leg attempt %d generation %d, want 2 and 2", got.Attempt, got.Generation)
	}
	if secondFnCalls.Load() != 0 || system.count(key) != 1 {
		t.Errorf("fn ran %d times in the second attempt; the system saw the change %d times; want 0 and 1", secondFnCalls.Load(), system.count(key))
	}
	if r, _ := doErr.Load().(doResult); !errors.Is(r.err, ErrAmbiguous) {
		t.Errorf("second attempt's Do error = %v, want ErrAmbiguous", r.err)
	}
}

func errString(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func TestPanicInExecutorCompletesUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	sink := &recordingSink{}
	serviceLog := &lockedBuilder{}
	rr := h.startRunner(h.q, runnerOptions{poll: 20 * time.Millisecond, sink: sink, logTo: serviceLog, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(context.Context, Env, queue.Claimed) queue.Outcome {
			panic("kaboom in the executor")
		}),
	}})
	waitFor(t, "leg to settle", func() bool { return h.leg(leg.ID).State == "unknown" })
	got := h.leg(leg.ID)
	// Everyone who can see the run reads the leg's log and failure fields, so the
	// panic text (which may carry secrets or internals) stays out of them.
	visible := got.FailureShort + "\n" + got.FailureDetail
	for _, e := range sink.snapshot() {
		visible += "\n" + e.text
	}
	if strings.Contains(visible, "kaboom") {
		t.Errorf("leg-visible text %q carries the panic message", visible)
	}
	if !strings.Contains(got.FailureDetail, "the step crashed; see the service log") {
		t.Errorf("failure detail = %q, want the generic crash text", got.FailureDetail)
	}
	if !strings.Contains(serviceLog.String(), "kaboom in the executor") {
		t.Errorf("service log %q lacks the panic text", serviceLog.String())
	}
	// the runner survives and keeps serving
	next := h.addLeg("search")
	_ = next
	waitFor(t, "the runner to pick up the next leg", func() bool {
		return h.leg(next.ID).State == "unknown"
	})
	_ = rr.stop(t)
}

func TestShutdownReleasesUnfinishedLegs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	started := make(chan struct{})
	rr := h.startRunner(h.q, runnerOptions{shutdown: 100 * time.Millisecond, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, _ Env, _ queue.Claimed) queue.Outcome {
			close(started)
			<-ctx.Done()
			return queue.Outcome{State: "cancelled"}
		}),
	}})
	<-started
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v, want nil on shutdown", err)
	}
	got := h.leg(leg.ID)
	if got.State != "queued" || got.RunnerID != "runner-1" || got.LeaseUntil != nil || got.Attempt != 1 {
		t.Errorf("leg = state %s runner %q lease %v attempt %d; want it queued, unleased, last holder and attempt kept",
			got.State, got.RunnerID, got.LeaseUntil, got.Attempt)
	}
}

func TestShutdownLetsLegFinishWithinTheTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	started := make(chan struct{})
	rr := h.startRunner(h.q, runnerOptions{shutdown: 5 * time.Second, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, _ Env, _ queue.Claimed) queue.Outcome {
			close(started)
			time.Sleep(200 * time.Millisecond) // ignores the shutdown, finishes by itself
			return succeed(`{}`)
		}),
	}})
	<-started
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := h.leg(leg.ID); got.State != "succeeded" {
		t.Errorf("leg state = %s, want succeeded: a leg that finishes inside the timeout is not released", got.State)
	}
}

func TestSecondAttemptWritesContinuationLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	h.abandonedAttempt(func(c *queue.Claimed) {
		if err := h.q.Checkpoint(context.Background(), c.Leg.ID, c.Leg.Generation, []byte(`{"step":2}`)); err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
	})

	type resumeResult struct {
		found bool
		step  int
		err   error
	}
	sink := &recordingSink{}
	var resumed atomic.Value
	rr := h.startRunner(h.q, runnerOptions{id: "second", sink: sink, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(_ context.Context, env Env, _ queue.Claimed) queue.Outcome {
			var cp struct{ Step int }
			found, err := env.Resume(&cp)
			resumed.Store(resumeResult{found, cp.Step, err})
			return succeed(`{}`)
		}),
	}})
	waitFor(t, "leg to succeed", func() bool { return h.leg(leg.ID).State == "succeeded" })
	_ = rr.stop(t)

	var warnings []string
	for _, e := range sink.snapshot() {
		if e.kind == "write" && e.level == "warn" {
			warnings = append(warnings, e.text)
		}
	}
	const want = "attempt 2: a previous attempt by runner gone did not finish; continuing from its last checkpoint. Log lines of the previous attempt may be missing."
	if len(warnings) != 1 || warnings[0] != want {
		t.Errorf("warn lines = %q, want exactly %q", warnings, want)
	}
	r, _ := resumed.Load().(resumeResult)
	if !r.found || r.step != 2 || r.err != nil {
		t.Errorf("Resume = %+v, want the previous attempt's checkpoint", r)
	}
}

func TestFirstAttemptWritesNoContinuationLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	sink := &recordingSink{}
	rr := h.startRunner(h.q, runnerOptions{sink: sink, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(_ context.Context, env Env, _ queue.Claimed) queue.Outcome {
			found, err := env.Resume(&struct{}{})
			if found || err != nil {
				return queue.Outcome{State: "failed", FailureShort: "unexpected checkpoint"}
			}
			return succeed(`{}`)
		}),
	}})
	waitFor(t, "leg to succeed", func() bool { return h.leg(leg.ID).State == "succeeded" })
	_ = rr.stop(t)
	for _, e := range sink.snapshot() {
		if e.level == "warn" {
			t.Errorf("unexpected warn line %q on a first attempt", e.text)
		}
	}
}

// A change cut short by shutdown may have been applied, so the next attempt
// must be told it is ambiguous, not that it failed.
func TestShutdownDuringChangeLeavesOperationAmbiguousForNextAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	system := &externalSystem{}
	const key = "deploy-billing"

	inFn := make(chan struct{})
	first := h.startRunner(h.q, runnerOptions{id: "first", shutdown: 100 * time.Millisecond, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			_, err := env.Do(ctx, "deploy", key, nil, func() (any, error) {
				system.apply(key) // the call went out, then the context ended before the answer
				close(inFn)
				<-ctx.Done()
				return nil, ctx.Err()
			})
			return queue.Outcome{State: "unknown", FailureDetail: errString(err)}
		}),
	}})
	<-inFn
	if err := first.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := h.leg(leg.ID); got.State != "queued" {
		t.Fatalf("leg state = %s, want released to queued", got.State)
	}
	ops, err := store.OpsOfLeg(context.Background(), h.pool, leg.ID)
	if err != nil || len(ops) != 1 || ops[0].State != "intended" {
		t.Fatalf("ops = %+v, %v; want one operation still intended", ops, err)
	}

	var secondFn atomic.Int32
	var got atomic.Value
	second := h.startRunner(h.q, runnerOptions{id: "second", executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			_, err := env.Do(ctx, "deploy", key, nil, func() (any, error) { secondFn.Add(1); return nil, nil })
			got.Store(doResult{err: err})
			if errors.Is(err, ErrAmbiguous) && system.count(key) == 1 {
				return succeed(`{}`)
			}
			return queue.Outcome{State: "failed", FailureShort: "Do said " + errString(err)}
		}),
	}})
	waitFor(t, "second attempt to settle", func() bool { return h.leg(leg.ID).State == "succeeded" })
	_ = second.stop(t)
	if r, _ := got.Load().(doResult); !errors.Is(r.err, ErrAmbiguous) || errors.Is(r.err, ErrPreviouslyFailed) || secondFn.Load() != 0 {
		t.Errorf("second attempt: Do error = %v, fn calls = %d; want ErrAmbiguous and no call", r.err, secondFn.Load())
	}
}

func TestDoLeavesOperationIntendedWhenFnReportsMaybeApplied(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	var doErr atomic.Value
	rr := h.startRunner(h.q, runnerOptions{executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			_, err := env.Do(ctx, "deploy", "k", nil, func() (any, error) {
				return nil, fmt.Errorf("connection reset after sending: %w", ErrMaybeApplied)
			})
			doErr.Store(doResult{err: err})
			return queue.Outcome{State: "unknown", FailureShort: "maybe applied"}
		}),
	}})
	waitFor(t, "leg to settle", func() bool { return h.leg(leg.ID).State == "unknown" })
	_ = rr.stop(t)
	if r, _ := doErr.Load().(doResult); !errors.Is(r.err, ErrMaybeApplied) {
		t.Errorf("Do error = %v, want it to wrap ErrMaybeApplied", r.err)
	}
	ops, err := store.OpsOfLeg(context.Background(), h.pool, leg.ID)
	if err != nil || len(ops) != 1 || ops[0].State != "intended" {
		t.Errorf("ops = %+v, %v; want one operation still intended", ops, err)
	}
}

func TestUnregisteredResolveAndRevalidateFailWithReason(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var ids []int64
	for _, kind := range []string{"resolve", "revalidate"} {
		queuedAt := h.clock.Now()
		l, err := store.InsertLeg(context.Background(), h.pool, store.Leg{RunID: h.runID, Kind: kind, State: "queued", QueuedAt: &queuedAt})
		if err != nil {
			t.Fatalf("InsertLeg %s: %v", kind, err)
		}
		ids = append(ids, l.ID)
	}
	rr := h.startRunner(h.q, runnerOptions{poll: 20 * time.Millisecond})
	for i, kind := range []string{"resolve", "revalidate"} {
		id := ids[i]
		waitFor(t, kind+" leg to fail", func() bool { return h.leg(id).State == "failed" })
		if d := h.leg(id).FailureDetail; !strings.Contains(d, `"`+kind+`"`) {
			t.Errorf("%s leg failure detail = %q, want it to name the kind", kind, d)
		}
	}
	_ = rr.stop(t)
}

func TestSettledNotifierCalledAfterTheLegIsCompleted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	type call struct {
		runID int64
		state string
	}
	calls := make(chan call, 4)
	rr := h.startRunner(h.q, runnerOptions{
		onSettled: func(runID int64) {
			// The notifier must see the leg already settled, or the orchestrator would advance too early.
			calls <- call{runID, h.leg(leg.ID).State}
		},
		executors: map[string]Executor{
			"ecs-deploy": execFunc(func(context.Context, Env, queue.Claimed) queue.Outcome { return succeed(`{}`) }),
		},
	})
	select {
	case c := <-calls:
		if c.runID != h.runID || c.state != "succeeded" {
			t.Errorf("notified %+v, want run %d with the leg succeeded", c, h.runID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the notifier was not called")
	}
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

// A leg whose cancel was requested before a runner picked it up is settled as
// cancelled without ever calling its executor (spec 7.9).
func TestLegCancelledBeforeStartIsNotExecuted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	if err := store.FlagLegCancel(context.Background(), h.pool, leg.ID); err != nil {
		t.Fatalf("FlagLegCancel: %v", err)
	}
	var executed atomic.Int32
	rr := h.startRunner(h.q, runnerOptions{executors: map[string]Executor{
		"ecs-deploy": execFunc(func(context.Context, Env, queue.Claimed) queue.Outcome {
			executed.Add(1)
			return succeed(`{}`)
		}),
	}})
	waitFor(t, "the leg to settle", func() bool { return h.leg(leg.ID).State == "cancelled" })
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if n := executed.Load(); n != 0 {
		t.Errorf("the executor ran %d times for a leg cancelled before it started, want 0", n)
	}
}

// "Check again" after a cancel request must run the executor: the leg was
// attempted, its operation is still intended, and settling it as "cancelled
// before it started" would hide an uncertain change and release the run's locks.
// The executor runs with CancelRequested true, reconciles through ErrAmbiguous
// and settles the leg with what it found.
func TestCheckAgainAfterCancelRunsTheExecutorToReconcile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	leg := h.addLeg("billing")
	h.abandonedAttempt(func(c *queue.Claimed) {
		if _, err := h.q.Intend(ctx, c.Leg.ID, c.Leg.Generation, "deploy", "deploy-1", []byte(`{"image":"app:1"}`)); err != nil {
			t.Fatalf("Intend: %v", err)
		}
	})
	// The leg ended unknown, a person cancelled the run, then asked to check again.
	if _, err := h.pool.Exec(ctx, `UPDATE legs SET state = 'unknown', finished_at = now() WHERE id = $1`, leg.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FlagLegCancel(ctx, h.pool, leg.ID); err != nil {
		t.Fatalf("FlagLegCancel: %v", err)
	}
	if err := store.SetLegState(ctx, h.pool, leg.ID, "queued", h.clock.Now()); err != nil {
		t.Fatalf("check again: %v", err)
	}

	var executed atomic.Int32
	var sawCancel, sawAmbiguous atomic.Bool
	rr := h.startRunner(h.q, runnerOptions{id: "checker", executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			executed.Add(1)
			sawCancel.Store(env.CancelRequested(ctx))
			_, err := env.Do(ctx, "deploy", "deploy-1", nil, func() (any, error) { return nil, nil })
			sawAmbiguous.Store(errors.Is(err, ErrAmbiguous))
			return succeed(`{}`) // the executor looked at the real system and found the change in place
		}),
	}})
	waitFor(t, "the leg to settle", func() bool {
		return h.leg(leg.ID).State != "queued" && h.leg(leg.ID).State != "claimed" && h.leg(leg.ID).State != "running"
	})
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if got := h.leg(leg.ID); got.State != "succeeded" {
		t.Fatalf("leg = %s (%s), want succeeded after reconciling, not cancelled before it started", got.State, got.FailureShort)
	}
	if executed.Load() != 1 || !sawCancel.Load() || !sawAmbiguous.Load() {
		t.Errorf("executed %d times, saw cancel %v, saw ambiguous %v; want once, true, true", executed.Load(), sawCancel.Load(), sawAmbiguous.Load())
	}
}

// failRecordingOperationResults makes the database refuse to record an
// operation's result: the first `times` updates of an operation fail, then they
// work again. times < 0 means every update fails.
func failRecordingOperationResults(t *testing.T, h *harness, times int) {
	t.Helper()
	limit := times
	if times < 0 {
		limit = 1 << 30
	}
	for _, stmt := range []string{
		`CREATE SEQUENCE finish_refusals`,
		`CREATE FUNCTION refuse_finish() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF nextval('finish_refusals') <= ` + fmt.Sprint(limit) + ` THEN RAISE EXCEPTION 'recording refused'; END IF;
				RETURN NEW;
			END $$`,
		`CREATE TRIGGER refuse_finish BEFORE UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION refuse_finish()`,
	} {
		if _, err := h.pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// If fn applied the change but recording its result keeps failing, Do must not
// report a plain error (the executor would call the leg failed and the run would
// release its locks over a change that is in place): it wraps ErrMaybeApplied
// and leaves the operation intended.
func TestDoReportsMaybeAppliedWhenRecordingTheResultKeepsFailing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	failRecordingOperationResults(t, h, -1)
	leg := h.addLeg("billing")
	var doErr atomic.Value
	var fnCalls atomic.Int32
	rr := h.startRunner(h.q, runnerOptions{executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			_, err := env.Do(ctx, "deploy", "k", nil, func() (any, error) { fnCalls.Add(1); return map[string]string{"ok": "yes"}, nil })
			doErr.Store(doResult{err: err})
			return queue.Outcome{State: "unknown", FailureShort: "maybe applied"}
		}),
	}})
	waitFor(t, "leg to settle", func() bool { return h.leg(leg.ID).State == "unknown" })
	_ = rr.stop(t)
	if r, _ := doErr.Load().(doResult); !errors.Is(r.err, ErrMaybeApplied) {
		t.Errorf("Do error = %v, want it to wrap ErrMaybeApplied", r.err)
	}
	if fnCalls.Load() != 1 {
		t.Errorf("fn ran %d times, want once", fnCalls.Load())
	}
	ops, err := store.OpsOfLeg(context.Background(), h.pool, leg.ID)
	if err != nil || len(ops) != 1 || ops[0].State != "intended" {
		t.Errorf("ops = %+v, %v; want one operation still intended", ops, err)
	}
}

// A brief failure to record is retried while the lease is held: Do succeeds and
// the operation ends done, and fn is not run again.
func TestDoRetriesRecordingTheResultAfterATransientFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	failRecordingOperationResults(t, h, 2)
	leg := h.addLeg("billing")
	var doErr atomic.Value
	var fnCalls atomic.Int32
	rr := h.startRunner(h.q, runnerOptions{executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			_, err := env.Do(ctx, "deploy", "k", nil, func() (any, error) { fnCalls.Add(1); return map[string]string{"ok": "yes"}, nil })
			doErr.Store(doResult{err: err})
			return succeed(`{}`)
		}),
	}})
	waitFor(t, "leg to succeed", func() bool { return h.leg(leg.ID).State == "succeeded" })
	_ = rr.stop(t)
	if r, _ := doErr.Load().(doResult); r.err != nil {
		t.Errorf("Do error = %v, want nil after the retry recorded the result", r.err)
	}
	if fnCalls.Load() != 1 {
		t.Errorf("fn ran %d times, want once", fnCalls.Load())
	}
	ops, err := store.OpsOfLeg(context.Background(), h.pool, leg.ID)
	if err != nil || len(ops) != 1 || ops[0].State != "done" {
		t.Errorf("ops = %+v, %v; want one done operation", ops, err)
	}
}

// When the log writer refuses lines (its queue is full because PostgreSQL is
// unreachable) the executor is stopped and the leg ends failed with a clear
// reason, instead of the runner buffering without limit.
func TestLegFailsWithAClearReasonWhenItsLogCannotBeStored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	sink := &recordingSink{failAfter: 3}
	var stopped atomic.Bool
	rr := h.startRunner(h.q, runnerOptions{sink: sink, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			for i := 0; ; i++ {
				select {
				case <-ctx.Done():
					stopped.Store(true)
					return queue.Outcome{State: "cancelled"}
				case <-time.After(time.Millisecond):
					env.Log("info", fmt.Sprintf("line %d", i))
				}
			}
		}),
	}})
	waitFor(t, "the leg to settle", func() bool { return h.leg(leg.ID).State == "failed" })
	_ = rr.stop(t)
	got := h.leg(leg.ID)
	if !stopped.Load() {
		t.Errorf("the executor's context was not cancelled")
	}
	if got.FailureShort != "log storage unavailable" || !strings.Contains(got.FailureDetail, "log") {
		t.Errorf("failure = %q / %q, want a clear log-storage reason", got.FailureShort, got.FailureDetail)
	}
}

// A leg that an earlier runner claimed and released, or died on before its first
// operation, has changed nothing: with a cancel requested it is settled as
// cancelled without calling the executor, whatever its attempt count.
func TestReleasedLegWithNoIntendedOperationIsNotExecutedAfterCancel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	leg := h.addLeg("billing")
	c, err := h.q.Claim(ctx, "gone")
	if err != nil || c == nil {
		t.Fatalf("Claim = %v, %v", c, err)
	}
	if err := h.q.Release(ctx, c.Leg.ID, c.Leg.Generation); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := store.FlagLegCancel(ctx, h.pool, leg.ID); err != nil {
		t.Fatalf("FlagLegCancel: %v", err)
	}
	var executed atomic.Int32
	rr := h.startRunner(h.q, runnerOptions{executors: map[string]Executor{
		"ecs-deploy": execFunc(func(context.Context, Env, queue.Claimed) queue.Outcome {
			executed.Add(1)
			return succeed(`{}`)
		}),
	}})
	waitFor(t, "the leg to settle", func() bool { return h.leg(leg.ID).State == "cancelled" })
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if n := executed.Load(); n != 0 {
		t.Errorf("the executor ran %d times for a leg with no intended operation, want 0", n)
	}
}

// When the log sink refuses lines the leg is stopped, but an executor that
// answers unknown (it may have changed something) must not be downgraded to
// failed: failed would release the run's locks over an uncertain change.
func TestLogStorageFailureNeverDowngradesAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("billing")
	sink := &recordingSink{failAfter: 3}
	rr := h.startRunner(h.q, runnerOptions{sink: sink, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, env Env, _ queue.Claimed) queue.Outcome {
			for i := 0; ; i++ {
				select {
				case <-ctx.Done():
					return queue.Outcome{State: "unknown", FailureShort: "change may have been applied"}
				case <-time.After(time.Millisecond):
					env.Log("info", fmt.Sprintf("line %d", i))
				}
			}
		}),
	}})
	waitFor(t, "the leg to settle", func() bool {
		s := h.leg(leg.ID).State
		return s != "queued" && s != "claimed" && s != "running"
	})
	_ = rr.stop(t)
	if got := h.leg(leg.ID); got.State != "unknown" {
		t.Fatalf("leg = %s (%s), want unknown: the executor said unknown and the log failure must not downgrade it", got.State, got.FailureShort)
	}
}
