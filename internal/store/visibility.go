package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// QueueRun is a run the Queue page lists: one that is waiting or being worked on.
type QueueRun struct {
	ID                      int64
	JobID, JobName          string
	Environment, Label      string
	State, WaitReason       string
	WaitingSince            time.Time
	LegsRunning, LegsQueued int
}

// ListQueueRuns returns the runs in queued, running, cancelling,
// needs_reconciliation and waiting_approval, oldest first, with how many of
// their legs run and wait. The job name is the job source's title.
func ListQueueRuns(ctx context.Context, db DBTX) ([]QueueRun, error) {
	rows, err := db.Query(ctx, `
		SELECT r.id, r.job_id, js.title, r.environment, r.label, r.state, r.wait_reason, r.state_changed_at,
			count(l.id) FILTER (WHERE l.state IN ('claimed', 'running')),
			count(l.id) FILTER (WHERE l.state = 'queued')
		FROM runs r
		JOIN job_sources js ON js.id = r.job_source_id
		LEFT JOIN legs l ON l.run_id = r.id
		WHERE r.state IN ('queued', 'running', 'cancelling', 'needs_reconciliation', 'waiting_approval')
		GROUP BY r.id, js.title
		ORDER BY r.created_at, r.id`)
	if err != nil {
		return nil, fmt.Errorf("list queue runs: %w", err)
	}
	defer rows.Close()
	out := []QueueRun{}
	for rows.Next() {
		var q QueueRun
		if err := rows.Scan(&q.ID, &q.JobID, &q.JobName, &q.Environment, &q.Label, &q.State, &q.WaitReason, &q.WaitingSince,
			&q.LegsRunning, &q.LegsQueued); err != nil {
			return nil, fmt.Errorf("list queue runs: %w", err)
		}
		q.WaitingSince = q.WaitingSince.UTC()
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list queue runs: %w", err)
	}
	return out, nil
}

// HeldLock is a resource a run holds.
type HeldLock struct {
	Resource   string
	RunID      int64
	AcquiredAt time.Time
}

// ListLocks returns every held lock ordered by resource.
func ListLocks(ctx context.Context, db DBTX) ([]HeldLock, error) {
	rows, err := db.Query(ctx, `SELECT resource, run_id, acquired_at FROM run_locks ORDER BY resource`)
	if err != nil {
		return nil, fmt.Errorf("list locks: %w", err)
	}
	defer rows.Close()
	out := []HeldLock{}
	for rows.Next() {
		var l HeldLock
		if err := rows.Scan(&l.Resource, &l.RunID, &l.AcquiredAt); err != nil {
			return nil, fmt.Errorf("list locks: %w", err)
		}
		l.AcquiredAt = l.AcquiredAt.UTC()
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list locks: %w", err)
	}
	return out, nil
}

// ServiceDeployment is one ecs-deploy leg of a service.
type ServiceDeployment struct {
	RunID, LegID         int64
	Environment, Version string
	State                string
	// At is when the leg finished, else started, else was queued, else when
	// its run was created.
	At time.Time
	By string
}

// ServiceDeployments returns the newest limit ecs-deploy step legs of a
// service, newest first. By is the requester's name, else their email.
func ServiceDeployments(ctx context.Context, db DBTX, service string, limit int) ([]ServiceDeployment, error) {
	rows, err := db.Query(ctx, `
		SELECT l.run_id, l.id, r.environment, l.version, l.state,
			coalesce(l.finished_at, l.started_at, l.queued_at, r.created_at),
			coalesce(nullif(u.name, ''), u.email)
		FROM legs l
		JOIN run_steps st ON st.id = l.step_id AND st.use = 'ecs-deploy'
		JOIN runs r ON r.id = l.run_id
		JOIN users u ON u.id = r.requested_by
		WHERE l.kind = 'step' AND l.service = $1
		ORDER BY l.id DESC LIMIT $2`, service, limit)
	if err != nil {
		return nil, fmt.Errorf("service deployments: %w", err)
	}
	defer rows.Close()
	out := []ServiceDeployment{}
	for rows.Next() {
		var d ServiceDeployment
		if err := rows.Scan(&d.RunID, &d.LegID, &d.Environment, &d.Version, &d.State, &d.At, &d.By); err != nil {
			return nil, fmt.Errorf("service deployments: %w", err)
		}
		d.At = d.At.UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("service deployments: %w", err)
	}
	return out, nil
}

// manifestServiceSQL selects the manifest entry of the service named by $1
// from the run r, as jsonb. A run with no manifest, or a manifest with no
// such service, yields no row.
const manifestServiceSQL = `
	SELECT s FROM jsonb_array_elements(
		CASE WHEN jsonb_typeof(r.manifest -> 'services') = 'array' THEN r.manifest -> 'services' ELSE '[]'::jsonb END) AS s
	WHERE s ->> 'name' = $1`

// ServiceConfigRun is a run whose manifest carries config items for a
// service; Service is that manifest entry as JSON.
type ServiceConfigRun struct {
	RunID       int64
	Environment string
	At          time.Time
	Service     json.RawMessage
}

// ServiceConfigRuns returns the newest limit runs whose manifest has config
// items for the service, newest first.
func ServiceConfigRuns(ctx context.Context, db DBTX, service string, limit int) ([]ServiceConfigRun, error) {
	rows, err := db.Query(ctx, `
		SELECT r.id, r.environment, r.created_at, m.s
		FROM runs r
		CROSS JOIN LATERAL (`+manifestServiceSQL+` AND CASE WHEN jsonb_typeof(s -> 'config') = 'array' THEN jsonb_array_length(s -> 'config') ELSE 0 END > 0 LIMIT 1) AS m(s)
		ORDER BY r.id DESC LIMIT $2`, service, limit)
	if err != nil {
		return nil, fmt.Errorf("service config runs: %w", err)
	}
	defer rows.Close()
	out := []ServiceConfigRun{}
	for rows.Next() {
		var c ServiceConfigRun
		if err := rows.Scan(&c.RunID, &c.Environment, &c.At, &c.Service); err != nil {
			return nil, fmt.Errorf("service config runs: %w", err)
		}
		c.At = c.At.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("service config runs: %w", err)
	}
	return out, nil
}

// RestoreSource is the run a restore of a service in an environment could
// start from.
type RestoreSource struct {
	RunID, FolderID int64
}

// RestoreSources returns, per environment, the newest deploy or restore run
// that ended succeeded or partially_succeeded, has a succeeded ecs-deploy leg
// for the service and whose manifest records a previous task definition for it.
func RestoreSources(ctx context.Context, db DBTX, service string) (map[string]RestoreSource, error) {
	rows, err := db.Query(ctx, `
		SELECT DISTINCT ON (r.environment) r.environment, r.id, r.folder_id
		FROM runs r
		WHERE r.kind IN ('deploy', 'restore') AND r.state IN ('succeeded', 'partially_succeeded')
		  AND EXISTS (
			SELECT 1 FROM legs l JOIN run_steps st ON st.id = l.step_id AND st.use = 'ecs-deploy'
			WHERE l.run_id = r.id AND l.kind = 'step' AND l.service = $1 AND l.state = 'succeeded')
		  AND EXISTS (SELECT 1 FROM (`+manifestServiceSQL+`) AS m(s) WHERE coalesce(m.s ->> 'previous_task_definition', '') <> '')
		ORDER BY r.environment, r.id DESC`, service)
	if err != nil {
		return nil, fmt.Errorf("restore sources: %w", err)
	}
	defer rows.Close()
	out := map[string]RestoreSource{}
	for rows.Next() {
		var environment string
		var source RestoreSource
		if err := rows.Scan(&environment, &source.RunID, &source.FolderID); err != nil {
			return nil, fmt.Errorf("restore sources: %w", err)
		}
		out[environment] = source
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("restore sources: %w", err)
	}
	return out, nil
}

// FailedNotificationsSince counts the notifications that were given up on
// after since.
func FailedNotificationsSince(ctx context.Context, db DBTX, since time.Time) (int, error) {
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE state = 'failed' AND failed_at > $1`, dbTime(since)).Scan(&n); err != nil {
		return 0, fmt.Errorf("failed notifications: %w", err)
	}
	return n, nil
}
