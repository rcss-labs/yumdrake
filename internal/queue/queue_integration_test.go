//go:build integration

package queue

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

const testLease = 30 * time.Second

// fakeClock is a settable clock shared by the queue under test and the test.
type fakeClock struct{ nanos atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	clock  *fakeClock
	q      *Queue
	runID  int64
	userID int64
}

func newEnv(t *testing.T) *env {
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

	user, err := store.CreateLocalUser(ctx, pool, "queue@example.com", "Queue", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	folder, err := store.CreateFolder(ctx, pool, "queue-folder")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	source, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: folder.ID, Repo: "acme/jobs", Branch: "main", Path: "deploy.yaml"})
	if err != nil {
		t.Fatalf("CreateJobSource: %v", err)
	}
	snapshot, err := store.ActivateSnapshot(ctx, pool, "queue-inventory", []byte(`{}`), nil, time.Now())
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
	clock := newFakeClock()
	return &env{t: t, pool: pool, clock: clock, q: New(pool, testLease, clock.Now), runID: run.ID, userID: user.ID}
}

// addLeg inserts a queued step leg for the env's run, queued at the given time.
func (e *env) addLeg(service string, queuedAt time.Time) store.Leg {
	e.t.Helper()
	steps, err := store.StepsOfRun(context.Background(), e.pool, e.runID)
	if err != nil || len(steps) == 0 {
		e.t.Fatalf("StepsOfRun: %v", err)
	}
	l, err := store.InsertLeg(context.Background(), e.pool, store.Leg{
		RunID: e.runID, StepID: &steps[0].ID, Kind: "step", Service: service, Version: "1.0",
		State: "queued", QueuedAt: &queuedAt,
	})
	if err != nil {
		e.t.Fatalf("InsertLeg: %v", err)
	}
	return l
}

func (e *env) get(legID int64) store.Leg {
	e.t.Helper()
	l, err := store.GetLeg(context.Background(), e.pool, legID)
	if err != nil {
		e.t.Fatalf("GetLeg: %v", err)
	}
	return l
}

func (e *env) claim(runner string) *Claimed {
	e.t.Helper()
	c, err := e.q.Claim(context.Background(), runner)
	if err != nil || c == nil {
		e.t.Fatalf("Claim(%s) = %v, %v", runner, c, err)
	}
	return c
}

// reclaimed leaves a leg that runner "old" claimed (generation 1), whose lease
// expired, was swept back to the queue and claimed again by "new" (generation 2).
func (e *env) reclaimed() (staleGen, currentGen, legID int64) {
	e.t.Helper()
	e.addLeg("svc", e.clock.Now())
	first := e.claim("old")
	e.clock.Advance(testLease + time.Second)
	if requeued, unknown, err := e.q.Sweep(context.Background()); err != nil || requeued != 1 || unknown != 0 {
		e.t.Fatalf("Sweep = %d, %d, %v", requeued, unknown, err)
	}
	second := e.claim("new")
	if second.Leg.ID != first.Leg.ID || second.Leg.Generation != first.Leg.Generation+1 {
		e.t.Fatalf("reclaim got leg %d gen %d, want leg %d gen %d", second.Leg.ID, second.Leg.Generation, first.Leg.ID, first.Leg.Generation+1)
	}
	return first.Leg.Generation, second.Leg.Generation, first.Leg.ID
}

func TestClaimOrderAndFields(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	base := e.clock.Now()
	// Inserted newest first so id order differs from queued_at order; the two
	// legs queued at the same instant must come out by id.
	late := e.addLeg("late", base.Add(2*time.Second))
	tieB := e.addLeg("tie-b", base.Add(time.Second))
	tieC := e.addLeg("tie-c", base.Add(time.Second))
	early := e.addLeg("early", base)

	var order []int64
	for i, runner := range []string{"r1", "r2", "r3", "r4"} {
		c := e.claim(runner)
		order = append(order, c.Leg.ID)
		if c.Leg.State != "claimed" || c.Leg.Generation != 1 || c.Leg.Attempt != 1 || c.Leg.RunnerID != runner {
			t.Fatalf("claim %d: leg = %+v", i, c.Leg)
		}
		wantLease := e.clock.Now().Add(testLease)
		if c.Leg.LeaseUntil == nil || !c.Leg.LeaseUntil.Equal(wantLease) {
			t.Fatalf("claim %d: lease = %v, want %v", i, c.Leg.LeaseUntil, wantLease)
		}
		if c.Run.ID != e.runID || c.Run.JobID != "deploy-api" {
			t.Fatalf("claim %d: run = %+v", i, c.Run)
		}
		if c.Step == nil || c.Step.Use != "ecs-deploy" {
			t.Fatalf("claim %d: step = %+v", i, c.Step)
		}
	}
	want := []int64{early.ID, tieB.ID, tieC.ID, late.ID}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("claim order = %v, want %v", order, want)
		}
	}
}

func TestClaimWithoutStepReturnsNilStep(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	now := e.clock.Now()
	if _, err := store.InsertLeg(context.Background(), e.pool, store.Leg{RunID: e.runID, Kind: "revalidate", State: "queued", QueuedAt: &now}); err != nil {
		t.Fatalf("InsertLeg: %v", err)
	}
	if c := e.claim("r"); c.Step != nil || c.Leg.Kind != "revalidate" {
		t.Fatalf("claimed = %+v, want revalidate leg without a step", c)
	}
}

func TestClaimReturnsNilWhenEmpty(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	got, err := e.q.Claim(context.Background(), "r")
	if err != nil || got != nil {
		t.Fatalf("Claim on empty queue = %v, %v; want nil, nil", got, err)
	}
	// A claimed leg is not claimable again.
	e.addLeg("a", e.clock.Now())
	e.claim("r")
	if got, err := e.q.Claim(context.Background(), "r2"); err != nil || got != nil {
		t.Fatalf("second Claim = %v, %v; want nil, nil", got, err)
	}
}

func TestConcurrentClaimsNeverShareALeg(t *testing.T) {
	t.Parallel()
	const legs, claimers = 200, 50
	e := newEnv(t)
	for i := 0; i < legs; i++ {
		e.addLeg(fmt.Sprintf("svc-%d", i), e.clock.Now())
	}

	var total atomic.Int64
	var mu sync.Mutex
	claimedBy := map[int64][]string{}
	var wg sync.WaitGroup
	errs := make(chan error, claimers)
	for i := 0; i < claimers; i++ {
		runner := fmt.Sprintf("runner-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for total.Add(1) <= 2*legs { // bounded: a broken queue must fail, not loop
				c, err := e.q.Claim(context.Background(), runner)
				if err != nil {
					errs <- err
					return
				}
				if c == nil {
					return
				}
				if c.Leg.Generation != 1 || c.Leg.Attempt != 1 {
					errs <- fmt.Errorf("leg %d generation %d attempt %d, want 1, 1", c.Leg.ID, c.Leg.Generation, c.Leg.Attempt)
				}
				mu.Lock()
				claimedBy[c.Leg.ID] = append(claimedBy[c.Leg.ID], runner)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("claimer: %v", err)
	}
	if len(claimedBy) != legs {
		t.Fatalf("%d distinct legs claimed, want %d", len(claimedBy), legs)
	}
	for id, runners := range claimedBy {
		if len(runners) != 1 {
			t.Errorf("leg %d claimed %d times: %v", id, len(runners), runners)
		}
	}
}

// Claimers must not wait for each other: a leg another transaction has locked
// is skipped, not waited for (FOR UPDATE SKIP LOCKED).
func TestClaimSkipsLegLockedByAnotherTransaction(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	base := e.clock.Now()
	first := e.addLeg("first", base)
	second := e.addLeg("second", base.Add(time.Second))

	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(context.Background(), "SELECT id FROM legs WHERE id = $1 FOR UPDATE", first.ID); err != nil {
		t.Fatalf("lock leg: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := e.q.Claim(ctx, "r")
	if err != nil || c == nil || c.Leg.ID != second.ID {
		t.Fatalf("Claim with the oldest leg locked = %+v, %v; want the next leg (%d) without waiting", c, err, second.ID)
	}
}

func TestHeartbeatExtendsLease(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.addLeg("a", e.clock.Now())
	c := e.claim("r")
	e.clock.Advance(20 * time.Second)
	if err := e.q.Heartbeat(context.Background(), c.Leg.ID, c.Leg.Generation); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	got := e.get(c.Leg.ID)
	if want := e.clock.Now().Add(testLease); got.LeaseUntil == nil || !got.LeaseUntil.Equal(want) {
		t.Fatalf("lease = %v, want %v", got.LeaseUntil, want)
	}
	// 40s after the claim the original lease is over but the extended one is not.
	e.clock.Advance(20 * time.Second)
	if requeued, unknown, err := e.q.Sweep(context.Background()); err != nil || requeued != 0 || unknown != 0 {
		t.Fatalf("Sweep = %d, %d, %v; want nothing swept", requeued, unknown, err)
	}
}

func TestMarkRunningAndCheckpoint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("a", e.clock.Now())
	c := e.claim("r")
	if err := e.q.MarkRunning(ctx, c.Leg.ID, c.Leg.Generation); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := e.q.Checkpoint(ctx, c.Leg.ID, c.Leg.Generation, []byte(`{"phase":2}`)); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	got := e.get(c.Leg.ID)
	if got.State != "running" || got.StartedAt == nil || string(got.Resume) != `{"phase": 2}` {
		t.Fatalf("leg = state %q started %v resume %s", got.State, got.StartedAt, got.Resume)
	}
	// A running leg still accepts writes.
	if err := e.q.Heartbeat(ctx, c.Leg.ID, c.Leg.Generation); err != nil {
		t.Fatalf("Heartbeat while running: %v", err)
	}
}

// legWrites is every method that takes a generation, called with a generation.
func legWrites(q *Queue, legID, opID int64) map[string]func(gen int64) error {
	ctx := context.Background()
	return map[string]func(int64) error{
		"Heartbeat":   func(g int64) error { return q.Heartbeat(ctx, legID, g) },
		"MarkRunning": func(g int64) error { return q.MarkRunning(ctx, legID, g) },
		"Checkpoint":  func(g int64) error { return q.Checkpoint(ctx, legID, g, []byte(`{"stale":true}`)) },
		"Intend": func(g int64) error {
			_, err := q.Intend(ctx, legID, g, "stale-op", "stale-key", []byte(`{}`))
			return err
		},
		"Finish": func(g int64) error { return q.Finish(ctx, legID, g, opID, true, []byte(`{"stale":true}`)) },
		"Complete": func(g int64) error {
			return q.Complete(ctx, legID, g, Outcome{State: "succeeded", Result: []byte(`{"stale":true}`)})
		},
		"Release": func(g int64) error { return q.Release(ctx, legID, g) },
	}
}

func TestStaleGenerationIsRejectedEverywhere(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Heartbeat", "MarkRunning", "Checkpoint", "Intend", "Finish", "Complete", "Release"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.addLeg("svc", e.clock.Now())
			first := e.claim("old")
			// The old holder intended an operation before it lost the lease; Finish
			// must not be able to settle it after the leg moved on.
			op, err := e.q.Intend(context.Background(), first.Leg.ID, first.Leg.Generation, "deploy", "k1", []byte(`{"v":1}`))
			if err != nil {
				t.Fatalf("Intend: %v", err)
			}
			e.clock.Advance(testLease + time.Second)
			if _, _, err := e.q.Sweep(context.Background()); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			second := e.claim("new")
			before := e.get(second.Leg.ID)
			opsBefore, err := e.q.Ops(context.Background(), second.Leg.ID)
			if err != nil {
				t.Fatalf("Ops: %v", err)
			}

			err = legWrites(e.q, first.Leg.ID, op.ID)[name](first.Leg.Generation)
			if !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("%s with stale generation = %v, want ErrLeaseLost", name, err)
			}

			after := e.get(second.Leg.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("stale %s changed the leg:\nbefore %+v\nafter  %+v", name, before, after)
			}
			opsAfter, err := e.q.Ops(context.Background(), second.Leg.ID)
			if err != nil {
				t.Fatalf("Ops: %v", err)
			}
			if !reflect.DeepEqual(opsBefore, opsAfter) {
				t.Fatalf("stale %s changed the operations:\nbefore %+v\nafter  %+v", name, opsBefore, opsAfter)
			}
		})
	}
}

func TestCurrentGenerationIsAcceptedEverywhere(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Heartbeat", "MarkRunning", "Checkpoint", "Intend", "Finish", "Complete", "Release"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.addLeg("svc", e.clock.Now())
			c := e.claim("r")
			op, err := e.q.Intend(context.Background(), c.Leg.ID, c.Leg.Generation, "deploy", "k1", nil)
			if err != nil {
				t.Fatalf("Intend: %v", err)
			}
			if err := legWrites(e.q, c.Leg.ID, op.ID)[name](c.Leg.Generation); err != nil {
				t.Fatalf("%s with the current generation = %v", name, err)
			}
		})
	}
}

func TestSettledLegRejectsFurtherWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())
	c := e.claim("r")
	op, err := e.q.Intend(ctx, c.Leg.ID, c.Leg.Generation, "deploy", "k", nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if err := e.q.Complete(ctx, c.Leg.ID, c.Leg.Generation, Outcome{State: "failed", FailureShort: "boom"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for name, call := range legWrites(e.q, c.Leg.ID, op.ID) {
		if err := call(c.Leg.Generation); !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s after Complete = %v, want ErrLeaseLost", name, err)
		}
	}
	if got := e.get(c.Leg.ID); got.State != "failed" {
		t.Fatalf("state = %q, want failed", got.State)
	}
}

func TestStaleCompleteDoesNotOverwriteNewerResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	oldGen, newGen, legID := e.reclaimed()

	if err := e.q.Complete(ctx, legID, newGen, Outcome{State: "failed", FailureShort: "new failed", FailureDetail: "detail", Result: []byte(`{"by":"new"}`)}); err != nil {
		t.Fatalf("new holder Complete: %v", err)
	}
	err := e.q.Complete(ctx, legID, oldGen, Outcome{State: "succeeded", Result: []byte(`{"by":"old"}`)})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("old holder Complete = %v, want ErrLeaseLost", err)
	}
	got := e.get(legID)
	if got.State != "failed" || got.FailureShort != "new failed" || string(got.Result) != `{"by": "new"}` {
		t.Fatalf("leg = state %q short %q result %s; the newer result must stand", got.State, got.FailureShort, got.Result)
	}
}

func TestCompleteStoresOutcomeAndRejectsBadStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	for _, state := range []string{"succeeded", "failed", "unknown", "cancelled"} {
		e.addLeg(state, e.clock.Now())
		c := e.claim("r")
		if err := e.q.Complete(ctx, c.Leg.ID, c.Leg.Generation, Outcome{State: state, FailureShort: "s", FailureDetail: "d", Result: []byte(`{"x":1}`)}); err != nil {
			t.Fatalf("Complete(%s): %v", state, err)
		}
		got := e.get(c.Leg.ID)
		if got.State != state || got.FinishedAt == nil || !got.FinishedAt.Equal(e.clock.Now()) ||
			got.FailureShort != "s" || got.FailureDetail != "d" || string(got.Result) != `{"x": 1}` {
			t.Fatalf("after Complete(%s): %+v", state, got)
		}
	}
	for _, bad := range []string{"", "queued", "running", "claimed", "pending", "skipped", "bogus"} {
		e.addLeg("bad-"+bad, e.clock.Now())
		c := e.claim("r")
		err := e.q.Complete(ctx, c.Leg.ID, c.Leg.Generation, Outcome{State: bad})
		if !errors.Is(err, store.ErrInvalid) {
			t.Errorf("Complete(%q) = %v, want store.ErrInvalid", bad, err)
		}
		if got := e.get(c.Leg.ID); got.State != "claimed" || got.FinishedAt != nil {
			t.Errorf("Complete(%q) changed the leg: %+v", bad, got)
		}
	}
}

func TestIntendIsIdempotentPerKeyAndSurvivesReclaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())
	first := e.claim("old")
	op, err := e.q.Intend(ctx, first.Leg.ID, first.Leg.Generation, "register-task-def", "key-1", []byte(`{"image":"v1"}`))
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if op.State != "intended" || op.Name != "register-task-def" || op.Key != "key-1" || op.LegID != first.Leg.ID || string(op.Intent) != `{"image": "v1"}` {
		t.Fatalf("op = %+v", op)
	}
	if other, err := e.q.Intend(ctx, first.Leg.ID, first.Leg.Generation, "update-service", "key-2", nil); err != nil || other.ID == op.ID || other.Seq != op.Seq+1 {
		t.Fatalf("second key = %+v, %v; want a new operation with the next seq", other, err)
	}

	// The runner dies before it finishes the operation; another one takes over.
	e.clock.Advance(testLease + time.Second)
	if _, _, err := e.q.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	second := e.claim("new")
	ops, err := e.q.Ops(ctx, second.Leg.ID)
	if err != nil || len(ops) != 2 || ops[0].ID != op.ID || ops[0].State != "intended" || ops[1].Key != "key-2" {
		t.Fatalf("Ops after reclaim = %+v, %v; want both intended operations kept in order", ops, err)
	}
	again, err := e.q.Intend(ctx, second.Leg.ID, second.Leg.Generation, "register-task-def", "key-1", []byte(`{"image":"CHANGED"}`))
	if err != nil {
		t.Fatalf("Intend after reclaim: %v", err)
	}
	if again.ID != op.ID || again.State != "intended" || string(again.Intent) != `{"image": "v1"}` {
		t.Fatalf("second Intend = %+v, want the original operation %+v", again, op)
	}
	if ops, _ := e.q.Ops(ctx, second.Leg.ID); len(ops) != 2 {
		t.Fatalf("%d operations, want 2", len(ops))
	}

	// Finish settles it; a later Intend with the same key reports it done.
	if err := e.q.Finish(ctx, second.Leg.ID, second.Leg.Generation, op.ID, true, []byte(`{"arn":"x"}`)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	done, err := e.q.Intend(ctx, second.Leg.ID, second.Leg.Generation, "register-task-def", "key-1", nil)
	if err != nil || done.ID != op.ID || done.State != "done" || string(done.Result) != `{"arn": "x"}` {
		t.Fatalf("Intend after Finish = %+v, %v; want the done operation", done, err)
	}
}

func TestFinishRecordsFailureAndIgnoresOtherLegsOperations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("a", e.clock.Now())
	e.addLeg("b", e.clock.Now())
	a, b := e.claim("r"), e.claim("r")
	opA, err := e.q.Intend(ctx, a.Leg.ID, a.Leg.Generation, "n", "k", nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if err := e.q.Finish(ctx, a.Leg.ID, a.Leg.Generation, opA.ID, false, []byte(`{"err":"x"}`)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ops, _ := e.q.Ops(ctx, a.Leg.ID)
	if len(ops) != 1 || ops[0].State != "failed" || string(ops[0].Result) != `{"err": "x"}` {
		t.Fatalf("ops = %+v, want one failed operation", ops)
	}
	// b's valid generation must not let it settle a's operation.
	opA2, _ := e.q.Intend(ctx, a.Leg.ID, a.Leg.Generation, "n2", "k2", nil)
	if err := e.q.Finish(ctx, b.Leg.ID, b.Leg.Generation, opA2.ID, true, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Finish of another leg's operation = %v, want store.ErrNotFound", err)
	}
	if ops, _ := e.q.Ops(ctx, a.Leg.ID); ops[1].State != "intended" {
		t.Fatalf("a's operation was settled through b: %+v", ops[1])
	}
}

func TestConcurrentIntendWithOneKeyCreatesOneOperation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())
	c := e.claim("r")

	const callers = 20
	ids := make([]int64, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := e.q.Intend(ctx, c.Leg.ID, c.Leg.Generation, "deploy", "same-key", []byte(`{}`))
			if err != nil {
				t.Errorf("Intend: %v", err)
				return
			}
			ids[i] = op.ID
		}()
	}
	wg.Wait()
	ops, err := e.q.Ops(ctx, c.Leg.ID)
	if err != nil || len(ops) != 1 {
		t.Fatalf("Ops = %+v, %v; want exactly one operation", ops, err)
	}
	for i, id := range ids {
		if id != ops[0].ID {
			t.Errorf("caller %d got operation %d, want %d", i, id, ops[0].ID)
		}
	}
}

func TestConcurrentIntendWithDistinctKeysGivesDistinctSeq(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())
	c := e.claim("r")
	const callers = 20
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.q.Intend(ctx, c.Leg.ID, c.Leg.Generation, "n", fmt.Sprintf("key-%d", i), nil); err != nil {
				t.Errorf("Intend key-%d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	ops, err := e.q.Ops(ctx, c.Leg.ID)
	if err != nil || len(ops) != callers {
		t.Fatalf("Ops = %d, %v; want %d", len(ops), err, callers)
	}
	seqs := make([]int, len(ops))
	for i, op := range ops {
		seqs[i] = op.Seq
	}
	if !sort.IntsAreSorted(seqs) || seqs[0] != 1 || seqs[len(seqs)-1] != callers {
		t.Fatalf("seqs = %v, want 1..%d in order", seqs, callers)
	}
}

func TestSweepRequeuesThenMarksUnknownAtFiveAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	leg := e.addLeg("svc", e.clock.Now())
	var opID int64
	for attempt := 1; attempt <= 5; attempt++ {
		c := e.claim(fmt.Sprintf("runner-%d", attempt))
		if c.Leg.Attempt != attempt || c.Leg.Generation != int64(attempt) {
			t.Fatalf("attempt %d: leg attempt %d generation %d", attempt, c.Leg.Attempt, c.Leg.Generation)
		}
		if attempt == 1 {
			if err := e.q.Checkpoint(ctx, leg.ID, c.Leg.Generation, []byte(`{"cp":1}`)); err != nil {
				t.Fatalf("Checkpoint: %v", err)
			}
			op, err := e.q.Intend(ctx, leg.ID, c.Leg.Generation, "n", "k", nil)
			if err != nil {
				t.Fatalf("Intend: %v", err)
			}
			opID = op.ID
		}
		// Not expired yet: a sweep leaves it alone.
		if requeued, unknown, err := e.q.Sweep(ctx); err != nil || requeued+unknown != 0 {
			t.Fatalf("early Sweep = %d, %d, %v", requeued, unknown, err)
		}
		e.clock.Advance(testLease + time.Second)
		requeued, unknown, err := e.q.Sweep(ctx)
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		got := e.get(leg.ID)
		if attempt < 5 {
			if requeued != 1 || unknown != 0 || got.State != "queued" || got.RunnerID != fmt.Sprintf("runner-%d", attempt) || got.LeaseUntil != nil ||
				got.Attempt != attempt || got.LeaseExpiries != attempt || string(got.Resume) != `{"cp": 1}` {
				t.Fatalf("attempt %d: sweep = %d, %d; leg %+v", attempt, requeued, unknown, got)
			}
			if ops, _ := e.q.Ops(ctx, leg.ID); len(ops) != 1 || ops[0].ID != opID || ops[0].State != "intended" {
				t.Fatalf("attempt %d: operations %+v not kept", attempt, ops)
			}
			continue
		}
		if requeued != 0 || unknown != 1 || got.State != "unknown" ||
			got.FailureDetail != "no runner could confirm the outcome after 5 attempts" || got.FinishedAt == nil {
			t.Fatalf("attempt 5: sweep = %d, %d; leg %+v", requeued, unknown, got)
		}
	}
	// An unknown leg is not offered again.
	if c, err := e.q.Claim(ctx, "r"); err != nil || c != nil {
		t.Fatalf("Claim after unknown = %v, %v", c, err)
	}
}

func TestSweepIgnoresSettledAndQueuedLegs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("done", e.clock.Now())
	e.addLeg("queued", e.clock.Now())
	c := e.claim("r")
	if err := e.q.Complete(ctx, c.Leg.ID, c.Leg.Generation, Outcome{State: "succeeded"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	e.clock.Advance(time.Hour)
	if requeued, unknown, err := e.q.Sweep(ctx); err != nil || requeued != 0 || unknown != 0 {
		t.Fatalf("Sweep = %d, %d, %v", requeued, unknown, err)
	}
	if got := e.get(c.Leg.ID); got.State != "succeeded" {
		t.Fatalf("settled leg state = %q", got.State)
	}
}

func TestSweepRacingCompleteLeavesOneConsistentOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	for i := 0; i < 30; i++ {
		e.addLeg(fmt.Sprintf("svc-%d", i), e.clock.Now())
	}
	var claimed []*Claimed
	for i := 0; i < 30; i++ {
		claimed = append(claimed, e.claim("r"))
	}
	e.clock.Advance(testLease + time.Second)
	var wg sync.WaitGroup
	completed := make([]bool, len(claimed))
	for i, c := range claimed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.q.Complete(ctx, c.Leg.ID, c.Leg.Generation, Outcome{State: "succeeded"})
			if err != nil && !errors.Is(err, ErrLeaseLost) {
				t.Errorf("Complete leg %d: %v; the loser must get ErrLeaseLost", c.Leg.ID, err)
			}
			completed[i] = err == nil
		}()
	}
	if _, _, err := e.q.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	wg.Wait()
	for i, c := range claimed {
		got := e.get(c.Leg.ID)
		switch {
		case completed[i] && got.State != "succeeded":
			t.Errorf("leg %d: Complete reported success but state is %q", c.Leg.ID, got.State)
		case !completed[i] && got.State != "queued":
			t.Errorf("leg %d: Complete lost but state is %q, want queued", c.Leg.ID, got.State)
		}
	}
}

func TestReleaseReturnsLegToQueueKeepingResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())
	c := e.claim("r1")
	if err := e.q.Checkpoint(ctx, c.Leg.ID, c.Leg.Generation, []byte(`{"cp":7}`)); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	op, err := e.q.Intend(ctx, c.Leg.ID, c.Leg.Generation, "n", "k", nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if err := e.q.MarkRunning(ctx, c.Leg.ID, c.Leg.Generation); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := e.q.Release(ctx, c.Leg.ID, c.Leg.Generation); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got := e.get(c.Leg.ID)
	if got.State != "queued" || got.RunnerID != "r1" || got.LeaseUntil != nil || got.Attempt != 1 || got.LeaseExpiries != 0 || got.Generation != 1 || string(got.Resume) != `{"cp": 7}` {
		t.Fatalf("leg after Release = %+v", got)
	}
	if ops, _ := e.q.Ops(ctx, c.Leg.ID); len(ops) != 1 || ops[0].ID != op.ID {
		t.Fatalf("operations after Release = %+v", ops)
	}
	// The next claim is attempt 2, generation 2, and the released holder is stale.
	next := e.claim("r2")
	if next.Leg.Attempt != 2 || next.Leg.Generation != 2 || string(next.Leg.Resume) != `{"cp": 7}` {
		t.Fatalf("reclaimed leg = %+v", next.Leg)
	}
	if err := e.q.Heartbeat(ctx, c.Leg.ID, c.Leg.Generation); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Heartbeat by released holder = %v, want ErrLeaseLost", err)
	}
}

func TestCancelRequested(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	leg := e.addLeg("svc", e.clock.Now())
	if got, err := e.q.CancelRequested(ctx, leg.ID); err != nil || got {
		t.Fatalf("CancelRequested = %v, %v; want false", got, err)
	}
	if err := store.FlagLegCancel(ctx, e.pool, leg.ID); err != nil {
		t.Fatalf("FlagLegCancel: %v", err)
	}
	if got, err := e.q.CancelRequested(ctx, leg.ID); err != nil || !got {
		t.Fatalf("CancelRequested = %v, %v; want true", got, err)
	}
	if _, err := e.q.CancelRequested(ctx, 999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CancelRequested(unknown) = %v, want store.ErrNotFound", err)
	}
}

// Isolates the generation predicate: the leg is still claimed (by the new
// holder), so only the generation tells the old holder apart.
func TestStaleCompleteWhileNewHolderStillClaimedChangesNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	oldGen, _, legID := e.reclaimed()
	before := e.get(legID)
	err := e.q.Complete(context.Background(), legID, oldGen, Outcome{State: "succeeded", Result: []byte(`{"by":"old"}`)})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale Complete = %v, want ErrLeaseLost", err)
	}
	if after := e.get(legID); !reflect.DeepEqual(before, after) {
		t.Fatalf("stale Complete changed a leg held by the new runner:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestStaleHolderOfSweptQueuedLegIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())
	first := e.claim("old")
	op, err := e.q.Intend(ctx, first.Leg.ID, first.Leg.Generation, "n", "k", nil)
	if err != nil {
		t.Fatalf("Intend: %v", err)
	}
	e.clock.Advance(testLease + time.Second)
	if _, _, err := e.q.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	before := e.get(first.Leg.ID)
	if before.State != "queued" {
		t.Fatalf("state = %q, want queued", before.State)
	}
	for name, call := range legWrites(e.q, first.Leg.ID, op.ID) {
		if err := call(first.Leg.Generation); !errors.Is(err, ErrLeaseLost) {
			t.Errorf("%s on a swept, queued leg = %v, want ErrLeaseLost", name, err)
		}
	}
	if after := e.get(first.Leg.ID); !reflect.DeepEqual(before, after) {
		t.Fatalf("stale writes changed the queued leg:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A graceful Release is not a lease expiry: only expiries count toward unknown.
func TestReleasesDoNotConsumeTheExpiryBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	leg := e.addLeg("svc", e.clock.Now())
	for i := 1; i <= 4; i++ {
		c := e.claim("r")
		if err := e.q.Release(ctx, leg.ID, c.Leg.Generation); err != nil {
			t.Fatalf("Release %d: %v", i, err)
		}
	}
	for expiry := 1; expiry <= 5; expiry++ {
		c := e.claim("r")
		if c.Leg.Attempt != 4+expiry {
			t.Fatalf("claim %d: attempt = %d, want %d", expiry, c.Leg.Attempt, 4+expiry)
		}
		e.clock.Advance(testLease + time.Second)
		if _, _, err := e.q.Sweep(ctx); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		got := e.get(leg.ID)
		wantState := "queued"
		if expiry == 5 {
			wantState = "unknown"
		}
		if got.State != wantState || got.LeaseExpiries != expiry {
			t.Fatalf("after expiry %d: state %q expiries %d, want %q, %d", expiry, got.State, got.LeaseExpiries, wantState, expiry)
		}
	}
}

func TestIntendWithKnownKeyAndDifferentNameIsInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())
	c := e.claim("r")
	if _, err := e.q.Intend(ctx, c.Leg.ID, c.Leg.Generation, "register", "k", nil); err != nil {
		t.Fatalf("Intend: %v", err)
	}
	if _, err := e.q.Intend(ctx, c.Leg.ID, c.Leg.Generation, "deregister", "k", nil); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Intend with another name = %v, want store.ErrInvalid", err)
	}
	if ops, _ := e.q.Ops(ctx, c.Leg.ID); len(ops) != 1 || ops[0].Name != "register" {
		t.Fatalf("ops = %+v", ops)
	}
}

// With a nil clock the queue uses the database clock (R22).
func TestNilClockUsesDatabaseTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	q := New(e.pool, testLease, nil)
	// The legs' queued_at is far in the past of the fake clock's date, so the
	// database clock cannot be mistaken for the test clock.
	e.addLeg("svc", e.clock.Now())

	var before, after time.Time
	if err := e.pool.QueryRow(ctx, "SELECT now()").Scan(&before); err != nil {
		t.Fatal(err)
	}
	c, err := q.Claim(ctx, "r")
	if err != nil || c == nil {
		t.Fatalf("Claim = %v, %v", c, err)
	}
	if err := q.Heartbeat(ctx, c.Leg.ID, c.Leg.Generation); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := e.pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	got := e.get(c.Leg.ID)
	if got.LeaseUntil == nil || got.LeaseUntil.Before(before.Add(testLease-time.Second)) || got.LeaseUntil.After(after.Add(testLease+time.Second)) {
		t.Fatalf("lease_until = %v, want database now (%v .. %v) + %v", got.LeaseUntil, before, after, testLease)
	}
	// Sweep compares against the database clock too: nothing is expired yet,
	// and a lease already in the database's past is.
	if requeued, unknown, err := q.Sweep(ctx); err != nil || requeued+unknown != 0 {
		t.Fatalf("Sweep = %d, %d, %v; want nothing", requeued, unknown, err)
	}
	if _, err := e.pool.Exec(ctx, "UPDATE legs SET lease_until = now() - interval '1 second' WHERE id = $1", c.Leg.ID); err != nil {
		t.Fatal(err)
	}
	if requeued, _, err := q.Sweep(ctx); err != nil || requeued != 1 {
		t.Fatalf("Sweep = %d, %v; want 1 requeued", requeued, err)
	}
}

// R23: a leg keeps its last holder's name while queued, and the next claim says
// who that was, so the runner can tell a person which attempt did not finish.
func TestClaimReportsPreviousRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	e.addLeg("svc", e.clock.Now())

	first := e.claim("old")
	if first.PreviousRunnerID != "" {
		t.Fatalf("first claim PreviousRunnerID = %q, want empty", first.PreviousRunnerID)
	}
	e.clock.Advance(testLease + time.Second)
	if requeued, _, err := e.q.Sweep(ctx); err != nil || requeued != 1 {
		t.Fatalf("Sweep = %d, %v", requeued, err)
	}
	if got := e.get(first.Leg.ID); got.State != "queued" || got.RunnerID != "old" || got.LeaseUntil != nil {
		t.Fatalf("swept leg = %+v, want queued, last holder kept, no lease", got)
	}
	second := e.claim("middle")
	if second.PreviousRunnerID != "old" || second.Leg.RunnerID != "middle" {
		t.Fatalf("after a sweep: PreviousRunnerID = %q, RunnerID = %q; want old and middle", second.PreviousRunnerID, second.Leg.RunnerID)
	}
	if err := e.q.Release(ctx, second.Leg.ID, second.Leg.Generation); err != nil {
		t.Fatalf("Release: %v", err)
	}
	third := e.claim("last")
	if third.PreviousRunnerID != "middle" {
		t.Fatalf("after a release: PreviousRunnerID = %q, want middle", third.PreviousRunnerID)
	}
}

func TestSweepRecordsLeaseExpired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	start := e.clock.Now().Add(-time.Minute)
	expired := func() int {
		t.Helper()
		n, err := store.CountEvents(ctx, e.pool, "lease_expired", start)
		if err != nil {
			t.Fatalf("CountEvents: %v", err)
		}
		return n
	}

	// Two legs held by dead runners and one that is still alive.
	e.addLeg("a", e.clock.Now())
	e.addLeg("b", e.clock.Now())
	e.claim("dead-1")
	e.claim("dead-2")
	e.clock.Advance(testLease - 5*time.Second)
	e.addLeg("c", e.clock.Now())
	e.claim("alive")
	e.clock.Advance(10 * time.Second)

	if requeued, unknown, err := e.q.Sweep(ctx); err != nil || requeued != 2 || unknown != 0 {
		t.Fatalf("Sweep = %d, %d, %v; want 2 requeued", requeued, unknown, err)
	}
	if n := expired(); n != 2 {
		t.Fatalf("lease_expired events after the first sweep = %d, want 2 (one per requeued leg, none for the live one)", n)
	}
	if _, _, err := e.q.Sweep(ctx); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if n := expired(); n != 2 {
		t.Fatalf("lease_expired events after a sweep that found nothing = %d, want still 2", n)
	}

	// A leg that reaches the fifth expiry becomes unknown and counts too.
	e2 := newEnv(t)
	e2.addLeg("svc", e2.clock.Now())
	for attempt := 1; attempt <= MaxLeaseExpiries; attempt++ {
		e2.claim(fmt.Sprintf("runner-%d", attempt))
		e2.clock.Advance(testLease + time.Second)
		if _, _, err := e2.q.Sweep(ctx); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}
	n, err := store.CountEvents(ctx, e2.pool, "lease_expired", start)
	if err != nil || n != MaxLeaseExpiries {
		t.Fatalf("lease_expired events = %d, %v; want %d (four requeues and the unknown)", n, err, MaxLeaseExpiries)
	}
}

func TestSweepSurvivesEventRecordingFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	leg := e.addLeg("svc", e.clock.Now())
	e.claim("dead")
	// Every insert into system_events now fails.
	if _, err := e.pool.Exec(ctx, `ALTER TABLE system_events ADD CONSTRAINT never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("break system_events: %v", err)
	}
	e.clock.Advance(testLease + time.Second)
	requeued, unknown, err := e.q.Sweep(ctx)
	if err != nil || requeued != 1 || unknown != 0 {
		t.Fatalf("Sweep = %d, %d, %v; want the leg requeued and no error", requeued, unknown, err)
	}
	if got := e.get(leg.ID); got.State != "queued" {
		t.Fatalf("leg state = %q, want queued", got.State)
	}
}
