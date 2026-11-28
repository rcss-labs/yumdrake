//go:build integration

package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/version"
)

// startRegisteredRunner is startRunner with the runner registry switched on.
func (h *harness) startRegisteredRunner(db store.DBTX, every time.Duration, o runnerOptions) *runningRunner {
	h.t.Helper()
	if o.concurrency == 0 {
		o.concurrency = 1
	}
	r := New(h.q, &recordingSink{}, "runner-hb", o.concurrency, 10*time.Millisecond, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithShutdownTimeout(5*time.Second).WithRegistry(db, every)
	for kind, e := range o.executors {
		r.Register(kind, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rr := &runningRunner{cancel: cancel, done: make(chan error, 1)}
	go func() { rr.done <- r.Run(ctx, nil) }()
	h.t.Cleanup(func() { rr.cancel() })
	return rr
}

func (h *harness) registered(id string) (store.RunnerInfo, bool) {
	h.t.Helper()
	list, err := store.ListRunners(context.Background(), h.pool, time.Time{})
	if err != nil {
		h.t.Fatalf("ListRunners: %v", err)
	}
	for _, r := range list {
		if r.ID == id {
			return r, true
		}
	}
	return store.RunnerInfo{}, false
}

func TestRunnerReportsBusyCount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.addLeg("a")
	h.addLeg("b")
	release := make(chan struct{})
	started := time.Now().UTC().Add(-time.Second)
	rr := h.startRegisteredRunner(h.pool, 10*time.Millisecond, runnerOptions{concurrency: 3, executors: map[string]Executor{
		"ecs-deploy": execFunc(func(ctx context.Context, _ Env, _ queue.Claimed) queue.Outcome {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return succeed(`{}`)
		}),
	}})

	waitFor(t, "two busy legs to be reported", func() bool {
		r, ok := h.registered("runner-hb")
		return ok && r.Busy == 2
	})
	r, _ := h.registered("runner-hb")
	if r.Version != version.Version || r.Concurrency != 3 || r.StartedAt.Before(started) || r.LastSeenAt.Before(r.StartedAt) {
		t.Fatalf("registry row = %+v, want version %q, concurrency 3, started and seen just now", r, version.Version)
	}

	close(release)
	waitFor(t, "idle runner to report busy 0", func() bool {
		r, ok := h.registered("runner-hb")
		return ok && r.Busy == 0
	})
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunnerReportsOnceMoreOnCleanShutdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// An hour between beats: only the start-up beat and the shutdown beat can write.
	rr := h.startRegisteredRunner(h.pool, time.Hour, runnerOptions{})
	waitFor(t, "the first beat", func() bool { _, ok := h.registered("runner-hb"); return ok })
	first, _ := h.registered("runner-hb")
	time.Sleep(20 * time.Millisecond)
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
	last, _ := h.registered("runner-hb")
	if !last.LastSeenAt.After(first.LastSeenAt) || last.Busy != 0 {
		t.Fatalf("after shutdown row = %+v, first = %+v; want a later last_seen_at and busy 0", last, first)
	}
}

// failingDB refuses every write, as a database that is down would.
type failingDB struct{ store.DBTX }

func (failingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("database is down")
}
func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("database is down")
}
func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row { return nil }
func (failingDB) Begin(context.Context) (pgx.Tx, error)            { return nil, errors.New("database is down") }

func TestFailedHeartbeatDoesNotStopTheRunner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	leg := h.addLeg("a")
	rr := h.startRegisteredRunner(failingDB{}, 5*time.Millisecond, runnerOptions{executors: map[string]Executor{
		"ecs-deploy": execFunc(func(context.Context, Env, queue.Claimed) queue.Outcome { return succeed(`{}`) }),
	}})
	waitFor(t, "the leg to succeed although every heartbeat fails", func() bool { return h.leg(leg.ID).State == "succeeded" })
	if err := rr.stop(t); err != nil {
		t.Fatalf("Run = %v", err)
	}
}
