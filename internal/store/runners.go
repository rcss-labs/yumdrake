package store

import (
	"context"
	"fmt"
	"time"
)

// RunnerGoneAfter is how long a runner may stay silent before it counts as gone.
const RunnerGoneAfter = 2 * time.Minute

// RunnerInfo is what a runner reports about itself.
type RunnerInfo struct {
	ID, Version           string
	Concurrency, Busy     int
	StartedAt, LastSeenAt time.Time
}

// UpsertRunner stores or refreshes a runner's row.
func UpsertRunner(ctx context.Context, db DBTX, r RunnerInfo) error {
	_, err := db.Exec(ctx,
		`INSERT INTO runners (id, version, concurrency, busy, started_at, last_seen_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version, concurrency = EXCLUDED.concurrency,
			busy = EXCLUDED.busy, started_at = EXCLUDED.started_at, last_seen_at = EXCLUDED.last_seen_at`,
		r.ID, r.Version, r.Concurrency, r.Busy, dbTime(r.StartedAt), dbTime(r.LastSeenAt))
	if err != nil {
		return fmt.Errorf("upsert runner: %w", mapError(err))
	}
	return nil
}

// ListRunners returns the runners last seen after seenSince, ordered by id.
func ListRunners(ctx context.Context, db DBTX, seenSince time.Time) ([]RunnerInfo, error) {
	rows, err := db.Query(ctx,
		`SELECT id, version, concurrency, busy, started_at, last_seen_at FROM runners
		 WHERE last_seen_at > $1 ORDER BY id`, dbTime(seenSince))
	if err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	defer rows.Close()
	out := []RunnerInfo{}
	for rows.Next() {
		var r RunnerInfo
		if err := rows.Scan(&r.ID, &r.Version, &r.Concurrency, &r.Busy, &r.StartedAt, &r.LastSeenAt); err != nil {
			return nil, fmt.Errorf("list runners: %w", err)
		}
		r.StartedAt, r.LastSeenAt = r.StartedAt.UTC(), r.LastSeenAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runners: %w", err)
	}
	return out, nil
}

// RecordEvent adds one system event. kind is lease_expired, log_persist_failed
// or sync_failed; anything else is ErrInvalid.
func RecordEvent(ctx context.Context, db DBTX, kind string, at time.Time) error {
	if _, err := db.Exec(ctx, `INSERT INTO system_events (kind, at) VALUES ($1, $2)`, kind, dbTime(at)); err != nil {
		return fmt.Errorf("record event: %w", mapError(err))
	}
	return nil
}

// CountEvents counts the events of a kind after since (exclusive).
func CountEvents(ctx context.Context, db DBTX, kind string, since time.Time) (int, error) {
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM system_events WHERE kind = $1 AND at > $2`, kind, dbTime(since)).Scan(&n); err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// QueueStats returns how many legs are queued and the queued_at of the oldest
// one (nil when none is queued). now is accepted so callers pass the clock they
// use for the rest of a sample; the counts do not depend on it.
func QueueStats(ctx context.Context, db DBTX, _ time.Time) (queuedLegs int, oldestQueued *time.Time, err error) {
	err = db.QueryRow(ctx, `SELECT count(*), min(queued_at) FROM legs WHERE state = 'queued'`).Scan(&queuedLegs, &oldestQueued)
	if err != nil {
		return 0, nil, fmt.Errorf("queue stats: %w", err)
	}
	if oldestQueued != nil {
		u := oldestQueued.UTC()
		oldestQueued = &u
	}
	return queuedLegs, oldestQueued, nil
}

// PruneRunners deletes the runners last seen before olderThan.
func PruneRunners(ctx context.Context, db DBTX, olderThan time.Time) (int, error) {
	tag, err := db.Exec(ctx, `DELETE FROM runners WHERE last_seen_at < $1`, dbTime(olderThan))
	if err != nil {
		return 0, fmt.Errorf("prune runners: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PruneEvents deletes the system events recorded before olderThan.
func PruneEvents(ctx context.Context, db DBTX, olderThan time.Time) (int, error) {
	tag, err := db.Exec(ctx, `DELETE FROM system_events WHERE at < $1`, dbTime(olderThan))
	if err != nil {
		return 0, fmt.Errorf("prune events: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
