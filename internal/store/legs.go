package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrLegMoved is returned by SetLegState when the leg is not in a state from
// which the requested move is allowed, typically because a runner claimed it
// first. Nothing was changed.
var ErrLegMoved = errors.New("leg is not in a state that allows the move")

// Leg is one unit of work for a runner: the resolve, the revalidation, or one
// step for one service.
type Leg struct {
	ID, RunID        int64
	StepID           *int64 // nil for resolve and revalidate
	Kind             string // "resolve" | "revalidate" | "step"
	Service, Version string
	State            string
	Attempt          int
	Generation       int64
	// LeaseExpiries counts how often the leg's lease ran out (queue.Sweep); a
	// graceful Release does not count. Attempt counts claims.
	LeaseExpiries   int
	RunnerID        string
	LeaseUntil      *time.Time
	CancelRequested bool
	Resume, Result  []byte // JSON
	FailureShort    string
	FailureDetail   string
	QueuedAt        *time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// Op is one external change a leg makes, recorded before and after it is made.
// (LegID, Key) is unique: the key makes a repeated attempt find its own record.
type Op struct {
	ID, LegID      int64
	Seq            int
	Name, Key      string
	Intent, Result []byte
	State          string // "intended" | "done" | "failed"
	At             time.Time
}

const legColumns = `id, run_id, step_id, kind, service, version, state, attempt, generation, lease_expiries, runner_id,
	lease_until, cancel_requested, resume, result, failure_short, failure_detail,
	queued_at, started_at, finished_at`

func scanLeg(r rowScanner) (Leg, error) {
	var l Leg
	err := r.Scan(&l.ID, &l.RunID, &l.StepID, &l.Kind, &l.Service, &l.Version, &l.State, &l.Attempt,
		&l.Generation, &l.LeaseExpiries, &l.RunnerID, &l.LeaseUntil, &l.CancelRequested, &l.Resume, &l.Result,
		&l.FailureShort, &l.FailureDetail, &l.QueuedAt, &l.StartedAt, &l.FinishedAt)
	return l, err
}

// InsertLeg stores a leg; an empty State means "pending". ErrInvalid for an
// unknown kind or state, ErrInvalidReference for an unknown run or step.
func InsertLeg(ctx context.Context, tx DBTX, l Leg) (Leg, error) {
	if l.State == "" {
		l.State = "pending"
	}
	got, err := scanLeg(tx.QueryRow(ctx,
		`INSERT INTO legs (run_id, step_id, kind, service, version, state, attempt, generation, runner_id,
			lease_until, cancel_requested, resume, result, failure_short, failure_detail,
			queued_at, started_at, finished_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		 RETURNING `+legColumns,
		l.RunID, l.StepID, l.Kind, l.Service, l.Version, l.State, l.Attempt, l.Generation, l.RunnerID,
		dbTimePtr(l.LeaseUntil), l.CancelRequested, l.Resume, l.Result, l.FailureShort, l.FailureDetail,
		dbTimePtr(l.QueuedAt), dbTimePtr(l.StartedAt), dbTimePtr(l.FinishedAt)))
	if err != nil {
		return Leg{}, fmt.Errorf("insert leg: %w", mapError(err))
	}
	return got, nil
}

// LegsOfRun returns the run's legs in id order.
func LegsOfRun(ctx context.Context, db DBTX, runID int64) ([]Leg, error) {
	return queryLegs(ctx, db, "legs of run", "SELECT "+legColumns+" FROM legs WHERE run_id = $1 ORDER BY id", runID)
}

// LegsOfRuns returns the legs of all the given runs in id order, in one query.
func LegsOfRuns(ctx context.Context, db DBTX, runIDs []int64) ([]Leg, error) {
	return queryLegs(ctx, db, "legs of runs", "SELECT "+legColumns+" FROM legs WHERE run_id = ANY($1) ORDER BY id", runIDs)
}

// ExpiredLegs returns claimed or running legs whose lease ended before now.
func ExpiredLegs(ctx context.Context, db DBTX, now time.Time) ([]Leg, error) {
	return queryLegs(ctx, db, "expired legs",
		"SELECT "+legColumns+" FROM legs WHERE state IN ('claimed', 'running') AND lease_until < $1 ORDER BY id", dbTime(now))
}

func queryLegs(ctx context.Context, db DBTX, what, query string, args ...any) ([]Leg, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, mapError(err))
	}
	defer rows.Close()
	legs := []Leg{}
	for rows.Next() {
		l, err := scanLeg(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, mapError(err))
		}
		legs = append(legs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, mapError(err))
	}
	return legs, nil
}

// legMoves lists, per target state, the states the orchestrator may move a leg
// from. Runners own every other transition and write them conditionally on the
// generation they claimed.
var legMoves = map[string][]string{
	"queued":    {"pending", "unknown"},
	"skipped":   {"pending"},
	"cancelled": {"pending", "queued"},
	"succeeded": {"unknown"},
	"failed":    {"unknown"},
}

// SetLegState makes an orchestrator-side move: pending to queued, skipped or
// cancelled; queued to cancelled; unknown to succeeded, failed or queued. It is
// conditional on the leg's current state, so it cannot overwrite a runner that
// claimed the leg meanwhile: then nothing changes and it returns ErrLegMoved.
// A queued leg with an intended operation is never moved (its outcome is
// uncertain and a runner must reconcile it), also ErrLegMoved.
// ErrNotFound for an unknown leg, ErrInvalid for a target not in the list.
//
// Moving to queued sets queued_at and clears runner, lease and finish time (an
// unknown leg being given another try); settled targets set finished_at.
func SetLegState(ctx context.Context, tx DBTX, legID int64, state string, at time.Time) error {
	from, ok := legMoves[state]
	if !ok {
		return fmt.Errorf("set leg state: %w: %q is not an orchestrator move", ErrInvalid, state)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE legs SET state = $2,
			queued_at   = CASE WHEN $2 = 'queued' THEN $3 ELSE queued_at END,
			runner_id   = CASE WHEN $2 = 'queued' THEN '' ELSE runner_id END,
			lease_until = CASE WHEN $2 = 'queued' THEN NULL ELSE lease_until END,
			finished_at = CASE WHEN $2 = 'queued' THEN NULL ELSE coalesce(finished_at, $3) END
		 WHERE id = $1 AND state = ANY($4)
		   AND NOT (state = 'queued' AND EXISTS (
		       SELECT 1 FROM operations o WHERE o.leg_id = legs.id AND o.state = 'intended'))`, legID, state, dbTime(at), from)
	if err != nil {
		return fmt.Errorf("set leg state: %w", mapError(err))
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM legs WHERE id = $1)", legID).Scan(&exists); err != nil {
		return fmt.Errorf("set leg state: %w", mapError(err))
	}
	if !exists {
		return fmt.Errorf("set leg state: %w", ErrNotFound)
	}
	return fmt.Errorf("set leg state: %w", ErrLegMoved)
}

// FlagLegCancel asks the runner holding the leg to stop it.
func FlagLegCancel(ctx context.Context, tx DBTX, legID int64) error {
	tag, err := tx.Exec(ctx, "UPDATE legs SET cancel_requested = true WHERE id = $1", legID)
	if err != nil {
		return fmt.Errorf("flag leg cancel: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("flag leg cancel: %w", ErrNotFound)
	}
	return nil
}

// ActiveLegsOfOtherRuns counts the queued, claimed and running legs of runs
// other than runID: in the same environment and in the same AWS account (an
// empty account never matches). The scheduler uses it to stay under the
// concurrency limits.
func ActiveLegsOfOtherRuns(ctx context.Context, db DBTX, runID int64, environment, awsAccount string) (inEnvironment, inAccount int, err error) {
	err = db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE r.environment = $2),
		        count(*) FILTER (WHERE $3 <> '' AND r.aws_account = $3)
		 FROM legs l JOIN runs r ON r.id = l.run_id
		 WHERE l.run_id <> $1 AND l.state IN ('queued', 'claimed', 'running')
		   AND (r.environment = $2 OR ($3 <> '' AND r.aws_account = $3))`,
		runID, environment, awsAccount).Scan(&inEnvironment, &inAccount)
	if err != nil {
		return 0, 0, fmt.Errorf("active legs of other runs: %w", mapError(err))
	}
	return inEnvironment, inAccount, nil
}

// GetLeg returns ErrNotFound for an unknown id.
func GetLeg(ctx context.Context, db DBTX, id int64) (Leg, error) {
	l, err := scanLeg(db.QueryRow(ctx, "SELECT "+legColumns+" FROM legs WHERE id = $1", id))
	if err != nil {
		return Leg{}, fmt.Errorf("get leg: %w", mapError(err))
	}
	return l, nil
}

// OpsOfLeg returns the leg's operations in seq order.
func OpsOfLeg(ctx context.Context, db DBTX, legID int64) ([]Op, error) {
	rows, err := db.Query(ctx,
		`SELECT `+OpColumns+`
		 FROM operations WHERE leg_id = $1 ORDER BY seq`, legID)
	if err != nil {
		return nil, fmt.Errorf("ops of leg: %w", mapError(err))
	}
	defer rows.Close()
	ops := []Op{}
	for rows.Next() {
		op, err := scanOp(rows)
		if err != nil {
			return nil, fmt.Errorf("ops of leg: %w", mapError(err))
		}
		ops = append(ops, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ops of leg: %w", mapError(err))
	}
	return ops, nil
}

// OpColumns is the column list ScanOp reads, for queries written outside the
// store package (the queue writes its own generation-checked SQL).
const OpColumns = `id, leg_id, seq, name, key, intent, result, state, at`

// ScanOp reads one row selected with OpColumns.
func ScanOp(r interface{ Scan(dest ...any) error }) (Op, error) { return scanOp(r) }

func scanOp(r rowScanner) (Op, error) {
	var o Op
	err := r.Scan(&o.ID, &o.LegID, &o.Seq, &o.Name, &o.Key, &o.Intent, &o.Result, &o.State, &o.At)
	return o, err
}

// LegsWithIntendedOperation returns the ids of the run's legs that have an
// operation still in the intended state: the external change may have been made
// and nobody has recorded the outcome.
func LegsWithIntendedOperation(ctx context.Context, db DBTX, runID int64) (map[int64]bool, error) {
	rows, err := db.Query(ctx,
		`SELECT DISTINCT o.leg_id FROM operations o JOIN legs l ON l.id = o.leg_id
		 WHERE l.run_id = $1 AND o.state = 'intended'`, runID)
	if err != nil {
		return nil, fmt.Errorf("legs with intended operations: %w", mapError(err))
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("legs with intended operations: %w", mapError(err))
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("legs with intended operations: %w", mapError(err))
	}
	return out, nil
}
