package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/rcss-labs/yamdrake/internal/store"
)

// window is how far back the counters and the run figures of one sample look.
// It equals the emit interval, so every event lands in exactly one sample.
const window = 60 * time.Second

// Collect reads the values of one sample from the database. Counters cover the
// window (now-60s, now]: a row at exactly now-60s belongs to the previous
// sample, one at now to this one.
//
//   - QueueDepth, OldestQueuedLegAge (0 when nothing is queued)
//   - ActiveRunners: runners seen within store.RunnerGoneAfter
//   - ExpiredLeases, LogPersistenceFailures: system events in the window
//   - RunsFinished, RunsFailed (failed or partially_succeeded), and one
//     RunDuration sample per finished run that has a start time
//   - NotificationFailures: notifications given up in the window;
//     NotificationsPending: all that are pending now
//   - SyncAge: the age of the oldest "last check" among the inventory and the
//     job sources that have been checked at least once; absent when none has
//     (a source never checked is not counted, it has no last sync to age)
func Collect(ctx context.Context, db store.DBTX, now time.Time) ([]Sample, error) {
	since := now.Add(-window)
	var samples []Sample
	add := func(name string, value float64, unit string) {
		samples = append(samples, Sample{Name: name, Value: value, Unit: unit})
	}

	queued, oldest, err := store.QueueStats(ctx, db, now)
	if err != nil {
		return nil, fmt.Errorf("collect metrics: %w", err)
	}
	age := 0.0
	if oldest != nil {
		age = now.Sub(*oldest).Seconds()
	}
	add("QueueDepth", float64(queued), "Count")
	add("OldestQueuedLegAge", age, "Seconds")

	runners, err := store.ListRunners(ctx, db, now.Add(-store.RunnerGoneAfter))
	if err != nil {
		return nil, fmt.Errorf("collect metrics: %w", err)
	}
	add("ActiveRunners", float64(len(runners)), "Count")

	expired, err := store.CountEvents(ctx, db, "lease_expired", since)
	if err != nil {
		return nil, fmt.Errorf("collect metrics: %w", err)
	}
	add("ExpiredLeases", float64(expired), "Count")

	var finished, failed int
	err = db.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE state IN ('failed', 'partially_succeeded'))
		 FROM runs WHERE finished_at > $1 AND finished_at <= $2`, since, now).Scan(&finished, &failed)
	if err != nil {
		return nil, fmt.Errorf("collect metrics: runs finished: %w", err)
	}
	add("RunsFinished", float64(finished), "Count")
	add("RunsFailed", float64(failed), "Count")

	durations, err := runDurations(ctx, db, since, now)
	if err != nil {
		return nil, err
	}
	for _, d := range durations {
		add("RunDuration", d, "Seconds")
	}

	var oldestSync *time.Time
	err = db.QueryRow(ctx,
		`SELECT min(checked_at) FROM (
			SELECT checked_at FROM inventory_status UNION ALL SELECT checked_at FROM job_sources
		 ) AS sources WHERE checked_at IS NOT NULL`).Scan(&oldestSync)
	if err != nil {
		return nil, fmt.Errorf("collect metrics: sync age: %w", err)
	}
	if oldestSync != nil {
		add("SyncAge", max(now.Sub(*oldestSync).Seconds(), 0), "Seconds")
	}

	persist, err := store.CountEvents(ctx, db, "log_persist_failed", since)
	if err != nil {
		return nil, fmt.Errorf("collect metrics: %w", err)
	}
	add("LogPersistenceFailures", float64(persist), "Count")

	var notificationFailures, pending int
	err = db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE failed_at > $1 AND failed_at <= $2), count(*) FILTER (WHERE state = 'pending')
		 FROM notifications`, since, now).Scan(&notificationFailures, &pending)
	if err != nil {
		return nil, fmt.Errorf("collect metrics: notifications: %w", err)
	}
	add("NotificationFailures", float64(notificationFailures), "Count")
	add("NotificationsPending", float64(pending), "Count")
	return samples, nil
}

// runDurations returns finished_at - started_at in seconds of the runs that
// finished in (since, until] and have a start time, oldest finish first.
func runDurations(ctx context.Context, db store.DBTX, since, until time.Time) ([]float64, error) {
	rows, err := db.Query(ctx,
		`SELECT extract(epoch FROM finished_at - started_at)::float8 FROM runs
		 WHERE finished_at > $1 AND finished_at <= $2 AND started_at IS NOT NULL
		 ORDER BY finished_at, id`, since, until)
	if err != nil {
		return nil, fmt.Errorf("collect metrics: run durations: %w", err)
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("collect metrics: run durations: %w", err)
		}
		out = append(out, max(d, 0))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("collect metrics: run durations: %w", err)
	}
	return out, nil
}
