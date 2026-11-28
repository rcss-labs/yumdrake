package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrRunFinal is returned by UpdateRunState when the run is already in a final
// state; nothing was changed.
var ErrRunFinal = errors.New("run is in a final state")

// Run is one deploy or restore of a job. Its state machine lives in the engine
// package; the store only enforces the list of valid values.
type Run struct {
	ID                         int64
	Kind                       string // "deploy" | "restore"
	JobSourceID                int64
	JobID                      string
	JobSnapshot                []byte // JSON spec.Job
	InventoryCommit, JobCommit string
	// InventorySnapshotID is the inventory snapshot the run was planned against.
	InventorySnapshotID int64
	Environment, Label  string
	AWSAccount          string
	FolderID            int64
	Selection           []byte // JSON engine.Selection
	// LockResources are the resources the run locks when it starts.
	LockResources     []string
	Manifest          []byte // JSON, nil until resolved
	ManifestID        string
	ApprovalRequired  bool
	AutoStart         bool
	State, WaitReason string
	// EndReason says in plain words why the run ended; empty until it is final.
	EndReason         string
	RequestedBy       int64
	Via               string // "web" | "cli"
	ParentRunID       *int64
	CancelRequestedBy *int64
	CreatedAt         time.Time
	// StateChangedAt is when the run last went through UpdateRunState (or was
	// inserted). Planned and approval expiry measure from it.
	StateChangedAt                           time.Time
	StartedAt, FinishedAt, CancelRequestedAt *time.Time
}

// RunStep is one step of a run's plan, copied from the job when the run is
// created.
type RunStep struct {
	ID, RunID   int64
	Name, Use   string
	GroupID     string
	Position    int
	MaxParallel int
	Timeout     time.Duration
	Needs       []byte // JSON []spec.Need
}

// RunFilter selects runs for ListRuns. Zero fields mean "any".
type RunFilter struct {
	JobSourceID int64
	Environment string
	States      []string
	// Query matches, case-insensitively as a substring, the job id, label,
	// environment, manifest id or the run id.
	Query    string
	BeforeID int64 // only runs with a smaller id, for paging
	Limit    int   // default 50, at most 500
}

const (
	defaultRunListLimit = 50
	maxRunListLimit     = 500
)

// finalRunStates are the states a run never leaves.
const finalRunStatesSQL = `('succeeded', 'partially_succeeded', 'failed', 'cancelled', 'rejected', 'expired', 'invalidated')`

const runColumns = `id, kind, job_source_id, job_id, job_snapshot, inventory_commit, job_commit,
	inventory_snapshot_id, environment, label, aws_account, folder_id, selection, lock_resources, manifest, manifest_id,
	approval_required, auto_start, state, wait_reason, end_reason, requested_by, via, parent_run_id,
	cancel_requested_by, cancel_requested_at, created_at, state_changed_at, started_at, finished_at`

func scanRun(r rowScanner) (Run, error) {
	var x Run
	err := r.Scan(&x.ID, &x.Kind, &x.JobSourceID, &x.JobID, &x.JobSnapshot, &x.InventoryCommit, &x.JobCommit,
		&x.InventorySnapshotID, &x.Environment, &x.Label, &x.AWSAccount, &x.FolderID, &x.Selection, &x.LockResources, &x.Manifest, &x.ManifestID,
		&x.ApprovalRequired, &x.AutoStart, &x.State, &x.WaitReason, &x.EndReason, &x.RequestedBy, &x.Via, &x.ParentRunID,
		&x.CancelRequestedBy, &x.CancelRequestedAt, &x.CreatedAt, &x.StateChangedAt, &x.StartedAt, &x.FinishedAt)
	return x, err
}

// InsertRun stores a run with its steps and legs in one transaction (a
// savepoint when db is already a transaction), so a failure leaves nothing.
//
// A leg's StepID, when not nil, is an index into steps (the ids do not exist
// yet); the stored leg points at the stored step. A zero r.CreatedAt means
// now; CreatedAt and StateChangedAt both start at that time. ErrInvalid for a
// value the CHECK constraints refuse or a step index out of range,
// ErrInvalidReference for an unknown job source, folder or user.
func InsertRun(ctx context.Context, db DBTX, r Run, steps []RunStep, legs []Leg) (Run, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("insert run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var created any // NULL makes the column default (now()) apply to both times
	if !r.CreatedAt.IsZero() {
		created = dbTime(r.CreatedAt)
	}
	lockResources := r.LockResources
	if lockResources == nil {
		lockResources = []string{} // a nil slice would be written as NULL
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO runs (kind, job_source_id, job_id, job_snapshot, inventory_commit, job_commit,
			inventory_snapshot_id, environment, label, aws_account, folder_id, selection, lock_resources,
			manifest, manifest_id, approval_required, auto_start, state, wait_reason, requested_by, via,
			parent_run_id, created_at, state_changed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
			$21, $22, coalesce($23::timestamptz, now()), coalesce($23::timestamptz, now()))
		 RETURNING `+runColumns,
		r.Kind, r.JobSourceID, r.JobID, r.JobSnapshot, r.InventoryCommit, r.JobCommit,
		r.InventorySnapshotID, r.Environment, r.Label, r.AWSAccount, r.FolderID, r.Selection, lockResources,
		r.Manifest, r.ManifestID, r.ApprovalRequired, r.AutoStart, r.State, r.WaitReason, r.RequestedBy, r.Via,
		r.ParentRunID, created)
	run, err := scanRun(row)
	if err != nil {
		return Run{}, fmt.Errorf("insert run: %w", mapError(err))
	}

	stepIDs := make([]int64, len(steps))
	for i, s := range steps {
		err := tx.QueryRow(ctx,
			`INSERT INTO run_steps (run_id, name, use, group_id, position, max_parallel, timeout_ns, needs)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce($8::jsonb, '[]'::jsonb)) RETURNING id`,
			run.ID, s.Name, s.Use, s.GroupID, s.Position, s.MaxParallel, int64(s.Timeout), s.Needs).Scan(&stepIDs[i])
		if err != nil {
			return Run{}, fmt.Errorf("insert run step %q: %w", s.Name, mapError(err))
		}
	}
	for i, l := range legs {
		l.RunID = run.ID
		if l.StepID != nil {
			idx := *l.StepID
			if idx < 0 || idx >= int64(len(stepIDs)) {
				return Run{}, fmt.Errorf("insert run leg %d: %w: step index %d out of range", i, ErrInvalid, idx)
			}
			id := stepIDs[idx]
			l.StepID = &id
		}
		if _, err := InsertLeg(ctx, tx, l); err != nil {
			return Run{}, fmt.Errorf("insert run leg %d: %w", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Run{}, fmt.Errorf("insert run: %w", mapError(err))
	}
	return run, nil
}

// GetRun returns ErrNotFound for an unknown id.
func GetRun(ctx context.Context, db DBTX, id int64) (Run, error) {
	r, err := scanRun(db.QueryRow(ctx, "SELECT "+runColumns+" FROM runs WHERE id = $1", id))
	if err != nil {
		return Run{}, fmt.Errorf("get run: %w", mapError(err))
	}
	return r, nil
}

// LockRun reads a run and locks its row until the transaction ends (SELECT ...
// FOR NO KEY UPDATE, which unlike FOR UPDATE does not block inserts that only
// reference the run, such as log lines). Every state change of a run starts with this, so two
// orchestrators never advance one run at the same time. ErrNotFound for an
// unknown id.
func LockRun(ctx context.Context, tx DBTX, id int64) (Run, error) {
	r, err := scanRun(tx.QueryRow(ctx, "SELECT "+runColumns+" FROM runs WHERE id = $1 FOR NO KEY UPDATE", id))
	if err != nil {
		return Run{}, fmt.Errorf("lock run: %w", mapError(err))
	}
	return r, nil
}

// ListRuns returns runs newest first (by id).
func ListRuns(ctx context.Context, db DBTX, f RunFilter) ([]Run, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.JobSourceID != 0 {
		add("job_source_id = $%d", f.JobSourceID)
	}
	if f.Environment != "" {
		add("environment = $%d", f.Environment)
	}
	if len(f.States) > 0 {
		add("state = ANY($%d)", f.States)
	}
	if f.Query != "" {
		args = append(args, "%"+escapeLike(f.Query)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(
			`(job_id ILIKE $%[1]d OR label ILIKE $%[1]d OR environment ILIKE $%[1]d OR manifest_id ILIKE $%[1]d OR id::text ILIKE $%[1]d)`, n))
	}
	if f.BeforeID != 0 {
		add("id < $%d", f.BeforeID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultRunListLimit
	}
	if limit > maxRunListLimit {
		limit = maxRunListLimit
	}
	args = append(args, limit)
	query := "SELECT " + runColumns + " FROM runs"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", len(args))

	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", mapError(err))
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", mapError(err))
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", mapError(err))
	}
	return runs, nil
}

// escapeLike makes s match literally inside a LIKE pattern.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ActiveRunIDs returns, in ascending order, every run not in a final state.
func ActiveRunIDs(ctx context.Context, db DBTX) ([]int64, error) {
	rows, err := db.Query(ctx, "SELECT id FROM runs WHERE state NOT IN "+finalRunStatesSQL+" ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("active run ids: %w", mapError(err))
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("active run ids: %w", mapError(err))
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("active run ids: %w", mapError(err))
	}
	return ids, nil
}

// UpdateRunState sets the state and wait reason and stamps state_changed_at
// with at only when the state actually changes (a same-state write keeps the
// stamp, so expiry clocks are not restarted). A run in a final state is never
// changed: ErrRunFinal. started_at is set
// the first time the run is running, finished_at when it enters a final state.
// The caller holds the run lock (LockRun) and decides whether the move is
// legal; the store only checks the state is a known value (ErrInvalid).
func UpdateRunState(ctx context.Context, tx DBTX, id int64, state, waitReason string, at time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE runs SET state = $2, wait_reason = $3, state_changed_at = CASE WHEN state IS DISTINCT FROM $2 THEN $4 ELSE state_changed_at END,
			started_at = CASE WHEN $2 = 'running' THEN coalesce(started_at, $4) ELSE started_at END,
			finished_at = CASE WHEN $2 IN `+finalRunStatesSQL+` THEN $4 ELSE finished_at END
		 WHERE id = $1 AND state NOT IN `+finalRunStatesSQL, id, state, waitReason, dbTime(at))
	if err != nil {
		return fmt.Errorf("update run state: %w", mapError(err))
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1)", id).Scan(&exists); err != nil {
		return fmt.Errorf("update run state: %w", mapError(err))
	}
	if !exists {
		return fmt.Errorf("update run state: %w", ErrNotFound)
	}
	return fmt.Errorf("update run state: %w", ErrRunFinal)
}

// SetEndReason records why a run ended. The caller holds the run lock and sets
// it together with the move to a final state.
func SetEndReason(ctx context.Context, tx DBTX, id int64, reason string) error {
	tag, err := tx.Exec(ctx, "UPDATE runs SET end_reason = $2 WHERE id = $1", id, reason)
	if err != nil {
		return fmt.Errorf("set end reason: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set end reason: %w", ErrNotFound)
	}
	return nil
}

// SetManifest stores the resolved plan and its id.
func SetManifest(ctx context.Context, tx DBTX, id int64, manifest []byte, manifestID string) error {
	tag, err := tx.Exec(ctx, "UPDATE runs SET manifest = $2, manifest_id = $3 WHERE id = $1", id, manifest, manifestID)
	if err != nil {
		return fmt.Errorf("set manifest: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set manifest: %w", ErrNotFound)
	}
	return nil
}

// RequestCancel records who asked for the run to be cancelled and when. The
// first request wins; later ones change nothing and are not an error.
func RequestCancel(ctx context.Context, tx DBTX, id, by int64, at time.Time) error {
	tag, err := tx.Exec(ctx,
		`UPDATE runs SET cancel_requested_by = coalesce(cancel_requested_by, $2),
			cancel_requested_at = coalesce(cancel_requested_at, $3) WHERE id = $1`, id, by, dbTime(at))
	if err != nil {
		return fmt.Errorf("request cancel: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("request cancel: %w", ErrNotFound)
	}
	return nil
}

// StepsOfRun returns the run's steps in position order.
func StepsOfRun(ctx context.Context, db DBTX, runID int64) ([]RunStep, error) {
	rows, err := db.Query(ctx,
		`SELECT id, run_id, name, use, group_id, position, max_parallel, timeout_ns, needs
		 FROM run_steps WHERE run_id = $1 ORDER BY position`, runID)
	if err != nil {
		return nil, fmt.Errorf("steps of run: %w", mapError(err))
	}
	defer rows.Close()
	steps := []RunStep{}
	for rows.Next() {
		var s RunStep
		var timeout int64
		if err := rows.Scan(&s.ID, &s.RunID, &s.Name, &s.Use, &s.GroupID, &s.Position, &s.MaxParallel, &timeout, &s.Needs); err != nil {
			return nil, fmt.Errorf("steps of run: %w", mapError(err))
		}
		s.Timeout = time.Duration(timeout)
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("steps of run: %w", mapError(err))
	}
	return steps, nil
}

// RunNames are the display names a run's screens show: the job source's title and the requester's name
// (their email when they have no name).
type RunNames struct {
	JobName, RequestedByName string
}

// NamesOfRuns reads the names of the given runs in one query. A run id that does not exist is absent from the map.
func NamesOfRuns(ctx context.Context, db DBTX, runIDs []int64) (map[int64]RunNames, error) {
	rows, err := db.Query(ctx,
		`SELECT r.id, js.title, COALESCE(NULLIF(u.name, ''), u.email)
		 FROM runs r
		 JOIN job_sources js ON js.id = r.job_source_id
		 JOIN users u ON u.id = r.requested_by
		 WHERE r.id = ANY($1)`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("names of runs: %w", mapError(err))
	}
	defer rows.Close()
	names := make(map[int64]RunNames, len(runIDs))
	for rows.Next() {
		var id int64
		var n RunNames
		if err := rows.Scan(&id, &n.JobName, &n.RequestedByName); err != nil {
			return nil, fmt.Errorf("names of runs: %w", mapError(err))
		}
		names[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("names of runs: %w", mapError(err))
	}
	return names, nil
}

// StepNamesOfRuns maps every step id of the given runs to its name, in one query.
func StepNamesOfRuns(ctx context.Context, db DBTX, runIDs []int64) (map[int64]string, error) {
	rows, err := db.Query(ctx, `SELECT id, name FROM run_steps WHERE run_id = ANY($1)`, runIDs)
	if err != nil {
		return nil, fmt.Errorf("step names of runs: %w", mapError(err))
	}
	defer rows.Close()
	names := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("step names of runs: %w", mapError(err))
		}
		names[id] = name
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("step names of runs: %w", mapError(err))
	}
	return names, nil
}
