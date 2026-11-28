package engine

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const defaultSweepEvery = 2 * time.Second

// Loop keeps runs moving until ctx ends. A run id arriving on wakeRuns (may be
// nil) advances that run at once. Every sweepEvery it also sweeps expired
// leases (q may be nil), expires planned runs and runs whose approval request
// has expired (the approval row's expiry decides), and advances every active
// run, so progress never depends on a wake-up having arrived: the timer alone
// is enough when Valkey is down. A failure for one run is logged and the
// others carry on.
func (e *Engine) Loop(ctx context.Context, wakeRuns <-chan int64, sweepEvery time.Duration, q *queue.Queue) {
	if sweepEvery <= 0 {
		sweepEvery = defaultSweepEvery
	}
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-wakeRuns:
			if !ok {
				wakeRuns = nil // closed: only the timer is left
				continue
			}
			e.advanceLogged(ctx, id)
		case <-ticker.C:
			e.sweepTick(ctx, q)
		}
	}
}

// guard runs fn and turns a panic into a log line (with the run id and the
// stack), so one broken run or a bug cannot take the whole orchestrator down.
// The stack goes to the local log only.
func (e *Engine) guard(what string, runID int64, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("panic recovered in the engine loop", "while", what, "run_id", runID,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
		}
	}()
	fn()
}

func (e *Engine) advanceLogged(ctx context.Context, runID int64) {
	e.guard("advancing", runID, func() { e.advanceOrLog(ctx, runID) })
}

func (e *Engine) advanceOrLog(ctx context.Context, runID int64) {
	if err := e.Advance(ctx, runID); err != nil && ctx.Err() == nil {
		e.log.Error("could not advance run", "run_id", runID, "error", err.Error())
	}
}

// sweepTick is one pass of the fallback timer.
func (e *Engine) sweepTick(ctx context.Context, q *queue.Queue) {
	e.guard("sweeping leases", 0, func() { e.sweepLeases(ctx, q) })
	e.guard("expiring approvals", 0, func() { e.expireDueApprovals(ctx) })
	e.guard("pruning runners and events", 0, func() { e.pruneSystemRecords(ctx) })
	var ids []int64
	e.guard("listing active runs", 0, func() { ids = e.activeRunIDs(ctx) })
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		e.guard("expiring", id, func() { e.expirePlannedIfDue(ctx, id) })
		e.advanceLogged(ctx, id)
	}
}

func (e *Engine) sweepLeases(ctx context.Context, q *queue.Queue) {
	if q != nil {
		// Sweep touches legs only; advancing every active run below is what
		// makes the run notice a requeued or unknown leg.
		requeued, unknown, err := q.Sweep(ctx)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				e.log.Error("lease sweep failed", "error", err.Error())
			}
		case requeued > 0 || unknown > 0:
			e.log.Warn("swept expired leases", "requeued", requeued, "unknown", unknown)
		}
	}
}

const (
	// systemRecordsKeep is how long runner rows and system events are kept.
	systemRecordsKeep = 7 * 24 * time.Hour
	// pruneEvery is the least time between two prunes.
	pruneEvery = time.Hour
)

// pruneSystemRecords deletes runner rows not seen, and system events recorded,
// for systemRecordsKeep, at most once an hour. A failure is logged and the next
// attempt waits for the hour too: pruning is housekeeping, not urgent.
func (e *Engine) pruneSystemRecords(ctx context.Context) {
	now := e.now()
	if !e.lastPruned.IsZero() && now.Sub(e.lastPruned) < pruneEvery {
		return
	}
	e.lastPruned = now
	cutoff := now.Add(-systemRecordsKeep)
	runners, err := store.PruneRunners(ctx, e.db, cutoff)
	if err != nil && ctx.Err() == nil {
		e.log.Error("could not prune old runner rows", "error", err.Error())
	}
	events, err := store.PruneEvents(ctx, e.db, cutoff)
	if err != nil && ctx.Err() == nil {
		e.log.Error("could not prune old system events", "error", err.Error())
	}
	if runners > 0 || events > 0 {
		e.log.Info("pruned old runner rows and system events", "runners", runners, "events", events)
	}
}

func (e *Engine) activeRunIDs(ctx context.Context) []int64 {
	ids, err := store.ActiveRunIDs(ctx, e.db)
	if err != nil && ctx.Err() == nil {
		e.log.Error("could not list active runs", "error", err.Error())
	}
	return ids
}

// expireDueApprovals expires the waiting runs whose approval request has run out.
func (e *Engine) expireDueApprovals(ctx context.Context) {
	due, err := store.ExpiredApprovals(ctx, e.db, e.now())
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("could not list expired approvals", "error", err.Error())
		}
		return
	}
	for _, a := range due {
		if ctx.Err() != nil {
			return
		}
		e.guard("expiring approval", a.RunID, func() {
			if err := e.Expire(ctx, a.RunID); err != nil && ctx.Err() == nil {
				e.log.Error("could not expire run", "run_id", a.RunID, "error", err.Error())
			}
		})
	}
}

// expirePlannedIfDue expires a planned run that nobody started in time.
func (e *Engine) expirePlannedIfDue(ctx context.Context, runID int64) {
	run, err := store.GetRun(ctx, e.db, runID)
	if err != nil {
		if ctx.Err() == nil {
			e.log.Error("could not read run", "run_id", runID, "error", err.Error())
		}
		return
	}
	if run.State != "planned" {
		return
	}
	if err := e.Expire(ctx, runID); err != nil && ctx.Err() == nil {
		e.log.Error("could not expire run", "run_id", runID, "error", err.Error())
	}
}
