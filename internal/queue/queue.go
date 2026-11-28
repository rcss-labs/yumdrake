// Package queue is the leased, generation-checked work queue between the
// orchestrator and the runners.
//
// A runner claims a queued leg and gets a generation number. Every later write
// the runner makes to that leg is conditional on the generation (and on the leg
// still being claimed or running), so a runner that lost its lease, for
// instance because it stalled past the lease and the leg was given to another
// runner, cannot overwrite the newer attempt: its write changes nothing and
// returns ErrLeaseLost.
//
// The queue writes its own SQL rather than using store.SetLegState, which is
// the orchestrator-side, state-conditional move.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/store"
)

// ErrLeaseLost means the caller no longer holds the leg: another attempt was
// started (the generation moved on), or the leg is settled or back in the queue.
var ErrLeaseLost = errors.New("lease lost")

// MaxLeaseExpiries is how many times a leg's lease may run out before the leg
// becomes unknown. A graceful Release does not count; Attempt (claims) is
// informational.
const MaxLeaseExpiries = 5

const unknownDetail = "no runner could confirm the outcome after 5 attempts"

// heldBy is the condition every runner write carries: the generation the
// caller was given and a leg that is still in a runner's hands. It is the
// mechanism that stops a stale runner; do not weaken it.
const heldBy = `generation = $2 AND state IN ('claimed', 'running')`

// Claimed is what a runner gets when it takes a leg.
type Claimed struct {
	Leg  store.Leg
	Run  store.Run
	Step *store.RunStep // nil for resolve and revalidate legs
	// PreviousRunnerID is the runner that held the leg before this claim: ""
	// on a first attempt. Sweep and Release keep the last holder's name for
	// this, so a runner can say whose attempt did not finish.
	PreviousRunnerID string
}

// Outcome is how a leg ended. State is succeeded, failed, unknown or cancelled.
type Outcome struct {
	State, FailureShort, FailureDetail string
	Result                             []byte
}

// Queue hands legs to runners under a lease.
type Queue struct {
	pool  *pgxpool.Pool
	lease time.Duration
	now   func() time.Time
}

// New returns a queue whose leases last lease from the time of the claim or
// heartbeat. A nil now means the database clock, which production uses so that
// every orchestrator and runner agrees on when a lease ends whatever their own
// clocks say; tests inject a clock instead.
func New(pool *pgxpool.Pool, lease time.Duration, now func() time.Time) *Queue {
	return &Queue{pool: pool, lease: lease, now: now}
}

// at is the time to write: nil (SQL NULL) when the database clock applies.
// Every statement that takes it wraps the parameter as coalesce($n::timestamptz, now()).
func (q *Queue) at() any {
	if q.now == nil {
		return nil
	}
	return q.now().UTC().Truncate(time.Microsecond)
}

// leaseSeconds is the lease as a parameter for make_interval(secs => ...).
func (q *Queue) leaseSeconds() float64 { return q.lease.Seconds() }

// Claim gives the oldest queued leg (by queued_at, then id) to runnerID: state
// claimed, a new generation, one more attempt, a lease. It returns nil, nil when
// nothing is queued. Concurrent claimers never get the same leg: the row is
// chosen with FOR UPDATE SKIP LOCKED.
func (q *Queue) Claim(ctx context.Context, runnerID string) (*Claimed, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The CTE picks and locks the row and reads the runner_id it had before
	// the UPDATE overwrites it.
	var legID int64
	var previousRunner string
	err = tx.QueryRow(ctx,
		`WITH picked AS (SELECT id, runner_id AS previous FROM legs WHERE state = 'queued'
			ORDER BY queued_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
		 UPDATE legs SET state = 'claimed', generation = generation + 1, attempt = attempt + 1,
			runner_id = $1, lease_until = coalesce($2::timestamptz, now()) + make_interval(secs => $3::float8)
		 FROM picked WHERE legs.id = picked.id AND legs.state = 'queued'
		 RETURNING legs.id, picked.previous`, runnerID, q.at(), q.leaseSeconds()).Scan(&legID, &previousRunner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}

	leg, err := store.GetLeg(ctx, tx, legID)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	run, err := store.GetRun(ctx, tx, leg.RunID)
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	c := &Claimed{Leg: leg, Run: run, PreviousRunnerID: previousRunner}
	if leg.StepID != nil {
		steps, err := store.StepsOfRun(ctx, tx, leg.RunID)
		if err != nil {
			return nil, fmt.Errorf("claim: %w", err)
		}
		for i := range steps {
			if steps[i].ID == *leg.StepID {
				c.Step = &steps[i]
			}
		}
		if c.Step == nil {
			return nil, fmt.Errorf("claim: leg %d refers to missing step %d", leg.ID, *leg.StepID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return c, nil
}

// guarded runs one conditional UPDATE on the leg and turns "no row matched"
// into ErrLeaseLost. query must use $1 for the leg id and $2 for the
// generation, and name heldBy in its WHERE clause.
func (q *Queue) guarded(ctx context.Context, what, query string, legID, generation int64, args ...any) error {
	tag, err := q.pool.Exec(ctx, query, append([]any{legID, generation}, args...)...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, store.MapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s leg %d generation %d: %w", what, legID, generation, ErrLeaseLost)
	}
	return nil
}

// Heartbeat extends the lease. ErrLeaseLost when the caller no longer holds the leg.
func (q *Queue) Heartbeat(ctx context.Context, legID, generation int64) error {
	return q.guarded(ctx, "heartbeat",
		`UPDATE legs SET lease_until = coalesce($3::timestamptz, now()) + make_interval(secs => $4::float8)
		 WHERE id = $1 AND `+heldBy,
		legID, generation, q.at(), q.leaseSeconds())
}

// MarkRunning moves a claimed leg to running (started_at is set the first time).
func (q *Queue) MarkRunning(ctx context.Context, legID, generation int64) error {
	return q.guarded(ctx, "mark running",
		`UPDATE legs SET state = 'running', started_at = coalesce(started_at, coalesce($3::timestamptz, now())) WHERE id = $1 AND `+heldBy,
		legID, generation, q.at())
}

// Checkpoint stores resume data that the next attempt, if any, is given.
func (q *Queue) Checkpoint(ctx context.Context, legID, generation int64, resume []byte) error {
	return q.guarded(ctx, "checkpoint",
		`UPDATE legs SET resume = $3 WHERE id = $1 AND `+heldBy,
		legID, generation, jsonOrNull(resume))
}

// Complete settles the leg: succeeded, failed, unknown or cancelled (anything
// else is store.ErrInvalid). It sets finished_at and ends the lease. ErrLeaseLost
// when the caller no longer holds the leg, so a stale holder cannot overwrite a
// newer attempt's outcome.
func (q *Queue) Complete(ctx context.Context, legID, generation int64, o Outcome) error {
	switch o.State {
	case "succeeded", "failed", "unknown", "cancelled":
	default:
		return fmt.Errorf("complete: %w: %q is not an outcome", store.ErrInvalid, o.State)
	}
	return q.guarded(ctx, "complete",
		`UPDATE legs SET state = $3, result = $4, failure_short = $5, failure_detail = $6,
			finished_at = coalesce($7::timestamptz, now()), lease_until = NULL
		 WHERE id = $1 AND `+heldBy,
		legID, generation, o.State, jsonOrNull(o.Result), o.FailureShort, o.FailureDetail, q.at())
}

// Release gives the leg back to the queue: state queued, no lease. runner_id
// keeps the last holder (Claimed.PreviousRunnerID). Attempt, resume data and operations are kept, and so is queued_at: a leg a
// runner handed back gracefully keeps its place at the front. (Sweep, by
// contrast, sets queued_at to now: a leg whose runner vanished goes to the
// back, so a leg that keeps killing runners does not starve the others.)
func (q *Queue) Release(ctx context.Context, legID, generation int64) error {
	return q.guarded(ctx, "release",
		`UPDATE legs SET state = 'queued', lease_until = NULL WHERE id = $1 AND `+heldBy,
		legID, generation)
}

// holdLeg locks the leg row and checks the caller still holds it. The lock
// lasts until tx ends, so the leg cannot be swept and reclaimed while the
// caller writes an operation row.
func holdLeg(ctx context.Context, tx pgx.Tx, what string, legID, generation int64) error {
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM legs WHERE id = $1 AND `+heldBy+` FOR UPDATE`, legID, generation).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%s leg %d generation %d: %w", what, legID, generation, ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// Intend records, before an external change is made, that the leg means to make
// it. (leg, key) identifies the change: calling Intend again with the same key,
// from this attempt or a later one, returns the existing operation unchanged
// (check its State: it may already be done) instead of creating a second one.
func (q *Queue) Intend(ctx context.Context, legID, generation int64, name, key string, intent []byte) (store.Op, error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return store.Op{}, fmt.Errorf("intend: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := holdLeg(ctx, tx, "intend", legID, generation); err != nil {
		return store.Op{}, err
	}
	// The leg row lock serialises Intends of one leg, so max(seq)+1 is safe;
	// ON CONFLICT on (leg_id, key) is the idempotency guarantee itself.
	_, err = tx.Exec(ctx,
		`INSERT INTO operations (leg_id, seq, name, key, intent, state, at)
		 VALUES ($1, (SELECT coalesce(max(seq), 0) + 1 FROM operations WHERE leg_id = $1), $2, $3, $4, 'intended', coalesce($5::timestamptz, now()))
		 ON CONFLICT (leg_id, key) DO NOTHING`,
		legID, name, key, jsonOrNull(intent), q.at())
	if err != nil {
		return store.Op{}, fmt.Errorf("intend: %w", store.MapError(err))
	}
	op, err := store.ScanOp(tx.QueryRow(ctx,
		`SELECT `+store.OpColumns+` FROM operations WHERE leg_id = $1 AND key = $2`, legID, key))
	if err != nil {
		return store.Op{}, fmt.Errorf("intend: %w", store.MapError(err))
	}
	if op.Name != name {
		return store.Op{}, fmt.Errorf("intend: key %q already names operation %q, not %q: %w", key, op.Name, name, store.ErrInvalid)
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Op{}, fmt.Errorf("intend: %w", err)
	}
	return op, nil
}

// Finish settles an operation the caller intended: done when ok, else failed,
// with its result. Generation-checked like every runner write. store.ErrNotFound
// when opID is not an operation of legID. An operation that is already settled
// is left as it is.
func (q *Queue) Finish(ctx context.Context, legID, generation, opID int64, ok bool, result []byte) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := holdLeg(ctx, tx, "finish", legID, generation); err != nil {
		return err
	}
	state := "failed"
	if ok {
		state = "done"
	}
	tag, err := tx.Exec(ctx,
		`UPDATE operations SET state = $3, result = $4, at = coalesce($5::timestamptz, now())
		 WHERE id = $1 AND leg_id = $2 AND state = 'intended'`,
		opID, legID, state, jsonOrNull(result), q.at())
	if err != nil {
		return fmt.Errorf("finish: %w", store.MapError(err))
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM operations WHERE id = $1 AND leg_id = $2)`, opID, legID).Scan(&exists); err != nil {
			return fmt.Errorf("finish: %w", err)
		}
		if !exists {
			return fmt.Errorf("finish: operation %d of leg %d: %w", opID, legID, store.ErrNotFound)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("finish: %w", err)
	}
	return nil
}

// Ops returns the leg's operations in the order they were intended.
func (q *Queue) Ops(ctx context.Context, legID int64) ([]store.Op, error) {
	return store.OpsOfLeg(ctx, q.pool, legID)
}

// CancelRequested reports whether the orchestrator asked for the leg to stop.
// store.ErrNotFound for an unknown leg.
func (q *Queue) CancelRequested(ctx context.Context, legID int64) (bool, error) {
	var requested bool
	err := q.pool.QueryRow(ctx, `SELECT cancel_requested FROM legs WHERE id = $1`, legID).Scan(&requested)
	if err != nil {
		return false, fmt.Errorf("cancel requested: %w", store.MapError(err))
	}
	return requested, nil
}

// Sweep takes every leg whose lease has passed away from its runner. With fewer
// than MaxLeaseExpiries expiries the leg goes back to the queue (resume data and
// operations kept); at MaxLeaseExpiries it becomes unknown, because a runner may
// have changed something outside and nobody can say what.
//
// Each UPDATE re-checks its predicate against the latest committed row, so a
// runner that completes or heartbeats at the same moment either wins (the leg is
// not touched) or loses (its next write gets ErrLeaseLost).
func (q *Queue) Sweep(ctx context.Context) (requeued, unknown int, err error) {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	now := q.at()
	expired := `state IN ('claimed', 'running') AND lease_until < coalesce($1::timestamptz, now())`
	tag, err := tx.Exec(ctx,
		`UPDATE legs SET state = 'queued', lease_until = NULL,
			queued_at = coalesce($1::timestamptz, now()), lease_expiries = lease_expiries + 1
		 WHERE `+expired+` AND lease_expiries + 1 < $2`, now, MaxLeaseExpiries)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep: requeue: %w", err)
	}
	requeued = int(tag.RowsAffected())
	tag, err = tx.Exec(ctx,
		`UPDATE legs SET state = 'unknown', lease_until = NULL, finished_at = coalesce($1::timestamptz, now()),
			lease_expiries = lease_expiries + 1,
			failure_short = 'outcome unknown', failure_detail = $3
		 WHERE `+expired+` AND lease_expiries + 1 >= $2`, now, MaxLeaseExpiries, unknownDetail)
	if err != nil {
		return 0, 0, fmt.Errorf("sweep: mark unknown: %w", err)
	}
	unknown = int(tag.RowsAffected())
	// One event per leg taken away from its runner, in the same transaction, so
	// the count is exactly the number of expiries that were applied.
	// The insert runs in a savepoint: if it fails, only it is rolled back and
	// the requeue and unknown changes still commit.
	if requeued+unknown > 0 {
		if err := recordExpiredLeases(ctx, tx, now, requeued+unknown); err != nil {
			slog.Warn("could not record lease_expired events", "error", err.Error(), "count", requeued+unknown)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("sweep: %w", err)
	}
	return requeued, unknown, nil
}

// recordExpiredLeases inserts n lease_expired events inside a savepoint and
// rolls back only the savepoint when the insert fails.
func recordExpiredLeases(ctx context.Context, tx pgx.Tx, at any, n int) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := sp.Exec(ctx,
		`INSERT INTO system_events (kind, at) SELECT 'lease_expired', coalesce($1::timestamptz, now())
		 FROM generate_series(1, $2::int)`, at, n); err != nil {
		_ = sp.Rollback(ctx)
		return err
	}
	return sp.Commit(ctx)
}

// jsonOrNull makes empty data SQL NULL, since an empty string is not valid JSON.
func jsonOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
