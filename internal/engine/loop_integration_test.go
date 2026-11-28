//go:build integration

package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/rcss-labs/yamdrake/internal/cache"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/wake"
)

// startLoop runs h.eng.Loop and returns a function that stops it and checks
// that it returns promptly.
func (h *harness) startLoop(wakeRuns <-chan int64, every time.Duration, q *queue.Queue) (stop func()) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.eng.Loop(ctx, wakeRuns, every, q)
		close(done)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				h.t.Error("Loop did not return after its context ended")
			}
		})
	}
	h.t.Cleanup(stop)
	return stop
}

func (h *harness) waitRunState(runID int64, want string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.run(runID).State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("run %d is %q, want %q after 10s", runID, h.run(runID).State, want)
}

// settleQueuedLegs stands in for a runner: every queued leg succeeds.
func (h *harness) settleQueuedLegs() {
	// A resolve leg answers with the plan of its own run.
	rows, err := h.pool.Query(context.Background(), `SELECT run_id FROM legs WHERE state = 'queued' AND kind = 'resolve'`)
	if err != nil {
		h.t.Errorf("list queued resolve legs: %v", err)
		return
	}
	var runIDs []int64
	for rows.Next() {
		var runID int64
		if err := rows.Scan(&runID); err != nil {
			h.t.Errorf("scan: %v", err)
		}
		runIDs = append(runIDs, runID)
	}
	rows.Close()
	for _, runID := range runIDs {
		h.settleResolve(runID, "succeeded", sampleManifest(runID))
	}
	_, err = h.pool.Exec(context.Background(),
		`UPDATE legs SET state = 'succeeded', result = '{"ok":true}'::jsonb, finished_at = now() WHERE state = 'queued'`)
	if err != nil {
		h.t.Errorf("settle queued legs: %v", err)
	}
}

func TestLoopAdvancesOnWake(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	run := h.create(h.starter, false, "s1")
	h.resolved(run.ID) // the runner's work is done; only the orchestrator's step is missing

	wakes := make(chan int64, 1)
	h.startLoop(wakes, time.Hour, nil) // no timer tick can help within the test
	time.Sleep(100 * time.Millisecond)
	h.expectState(run.ID, "resolving")

	wakes <- run.ID
	h.waitRunState(run.ID, "planned")
}

func TestLoopMakesProgressWithoutValkey(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	client, err := cache.Open(cache.Options{Addr: "127.0.0.1:1"}) // nothing listens on this port
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(client.Close)
	bus := wake.New(client, nil)
	h.eng = New(Deps{DB: h.pool, Now: h.clock.Now, Publisher: bus})

	run := h.create(h.starter, true, "s1", "s2") // auto-start: no person is needed
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startLoop(bus.Subscribe(ctx, wake.TopicRuns), 20*time.Millisecond, nil)

	// A runner stand-in settles whatever the loop queues.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && h.run(run.ID).State != "succeeded" {
		h.settleQueuedLegs()
		time.Sleep(10 * time.Millisecond)
	}
	h.expectState(run.ID, "succeeded")
}

func TestLoopSweepsExpiredLeasesAndExpiresApprovals(t *testing.T) {
	t.Run("a leg whose lease ran out goes back to the queue", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		run := h.create(h.starter, false, "s1")
		q := queue.New(h.pool, 30*time.Second, h.clock.Now)
		claimed, err := q.Claim(context.Background(), "runner-1")
		if err != nil || claimed == nil {
			t.Fatalf("Claim = %v, %v", claimed, err)
		}
		h.clock.Advance(time.Minute) // the runner went silent

		h.startLoop(nil, 20*time.Millisecond, q)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && h.legByKind(run.ID, "resolve").State != "queued" {
			time.Sleep(10 * time.Millisecond)
		}
		leg := h.legByKind(run.ID, "resolve")
		if leg.State != "queued" || leg.LeaseExpiries != 1 {
			t.Fatalf("leg = state %q after %d expiries, want queued after 1", leg.State, leg.LeaseExpiries)
		}
	})

	t.Run("a run nobody approved expires after the job's approval time", func(t *testing.T) {
		h := newHarness(t, harnessOptions{approvalFor: []string{testLabel}, approvalExpiry: time.Hour})
		run := h.planned(h.starter, "s1")
		if err := h.eng.Start(context.Background(), h.starter, run.ID); err != nil {
			t.Fatalf("Start: %v", err)
		}
		h.expectState(run.ID, "waiting_approval")

		h.startLoop(nil, 20*time.Millisecond, nil)
		h.clock.Advance(30 * time.Minute)
		time.Sleep(200 * time.Millisecond) // several ticks
		h.expectState(run.ID, "waiting_approval")

		h.clock.Advance(2 * time.Hour)
		h.waitRunState(run.ID, "expired")
	})

	t.Run("a planned run nobody started expires", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		run := h.planned(h.starter, "s1")
		h.clock.Advance(2 * time.Hour)
		h.startLoop(nil, 20*time.Millisecond, nil)
		h.waitRunState(run.ID, "expired")
	})
}

// failingDB makes locking one run fail, as a corrupt row or a lost
// connection would, and lets everything else through.
type failingDB struct {
	store.DBTX
	failID  int64
	asPanic bool // panic instead of returning an error
}

func (f failingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.DBTX.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingTx{Tx: tx, failID: f.failID, asPanic: f.asPanic}, nil
}

type failingTx struct {
	pgx.Tx
	failID  int64
	asPanic bool
}

type failedRow struct{}

func (failedRow) Scan(...any) error { return errors.New("injected failure") }

func (f failingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FOR NO KEY UPDATE") && len(args) == 1 && args[0] == f.failID {
		if f.asPanic {
			panic("injected panic")
		}
		return failedRow{}
	}
	return f.Tx.QueryRow(ctx, sql, args...)
}

// lockedBuffer is a log destination several goroutines may write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// setup creates a failing run (the lower id) and a good one, both resolved.
func setup(t *testing.T, asPanic bool) (h *harness, bad, good store.Run, logs *lockedBuffer) {
	h = newHarness(t, harnessOptions{})
	bad = h.create(h.starter, false, "s1") // the lower id: a loop that gave up on the first error would never reach good
	good = h.create(h.starter, false, "s2")
	h.resolved(bad.ID)
	h.resolved(good.ID)
	logs = &lockedBuffer{}
	h.eng = New(Deps{DB: failingDB{DBTX: h.pool, failID: bad.ID, asPanic: asPanic}, Now: h.clock.Now,
		Log: slog.New(slog.NewTextHandler(logs, nil))})
	return h, bad, good, logs
}

func TestOneFailingRunDoesNotBlockOthers(t *testing.T) {
	assertFailureLogged := func(t *testing.T, logs *lockedBuffer) {
		t.Helper()
		if out := logs.String(); !strings.Contains(out, "injected failure") {
			t.Errorf("the failure was not logged; log:\n%s", out)
		}
	}

	t.Run("on the timer", func(t *testing.T) {
		h, bad, good, logs := setup(t, false)
		h.startLoop(nil, 20*time.Millisecond, nil)
		h.waitRunState(good.ID, "planned")
		h.expectState(bad.ID, "resolving")
		assertFailureLogged(t, logs)
	})

	t.Run("on wake-ups", func(t *testing.T) {
		h, bad, good, logs := setup(t, false)
		wakes := make(chan int64, 2)
		h.startLoop(wakes, time.Hour, nil)
		wakes <- bad.ID
		wakes <- good.ID
		h.waitRunState(good.ID, "planned")
		h.expectState(bad.ID, "resolving")
		assertFailureLogged(t, logs)
	})
}

// A panic while advancing one run (a bug, a corrupt row) must not take the
// orchestrator down: it is logged with the run id and the loop carries on.
func TestPanicInOneRunDoesNotStopTheLoop(t *testing.T) {
	for name, wakeUp := range map[string]bool{"on the timer": false, "on wake-ups": true} {
		t.Run(name, func(t *testing.T) {
			h, bad, good, logs := setup(t, true)
			every := 20 * time.Millisecond
			var wakes chan int64
			if wakeUp {
				wakes = make(chan int64, 2)
				every = time.Hour
				wakes <- bad.ID
				wakes <- good.ID
			}
			h.startLoop(wakes, every, nil)
			h.waitRunState(good.ID, "planned")
			h.expectState(bad.ID, "resolving")
			// The loop is still alive: a run resolved after the panic is advanced too.
			later := h.create(h.starter, false, "s3")
			h.resolved(later.ID)
			if wakeUp {
				wakes <- later.ID
			}
			h.waitRunState(later.ID, "planned")
			if out := logs.String(); !strings.Contains(out, "injected panic") || !strings.Contains(out, fmt.Sprintf("run_id=%d", bad.ID)) {
				t.Errorf("the panic was not logged with the run id; log:\n%s", out)
			}
		})
	}
}

func TestLoopPrunesOldRunnerRowsAndEventsAtMostOncePerHour(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	ctx := context.Background()
	now := h.clock.Now()
	old, young := now.Add(-7*24*time.Hour-time.Minute), now.Add(-6*24*time.Hour)
	seed := func() {
		t.Helper()
		for id, seen := range map[string]time.Time{"old": old, "young": young} {
			if err := store.UpsertRunner(ctx, h.pool, store.RunnerInfo{ID: id, Version: "v", Concurrency: 1, StartedAt: seen, LastSeenAt: seen}); err != nil {
				t.Fatal(err)
			}
		}
		for _, at := range []time.Time{old, young} {
			if err := store.RecordEvent(ctx, h.pool, "sync_failed", at); err != nil {
				t.Fatal(err)
			}
		}
	}
	left := func() (runners, events int) {
		t.Helper()
		rs, err := store.ListRunners(ctx, h.pool, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		n, err := store.CountEvents(ctx, h.pool, "sync_failed", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return len(rs), n
	}

	seed()
	h.eng.sweepTick(ctx, nil)
	if r, e := left(); r != 1 || e != 1 {
		t.Fatalf("after the first tick %d runners and %d events are left, want 1 and 1 (only the week-old ones go)", r, e)
	}
	// Within the hour the next tick does not look again.
	seed()
	h.clock.Advance(59 * time.Minute)
	h.eng.sweepTick(ctx, nil)
	if r, e := left(); r != 2 || e != 3 {
		t.Fatalf("a tick within the hour left %d runners and %d events, want 2 and 3 (no pruning yet)", r, e)
	}
	h.clock.Advance(2 * time.Minute)
	h.eng.sweepTick(ctx, nil)
	if r, e := left(); r != 1 || e != 2 {
		t.Fatalf("a tick after the hour left %d runners and %d events, want 1 and 2", r, e)
	}
}
