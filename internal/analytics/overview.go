// Package analytics answers the read-only questions the overview page asks of
// the run history. Every figure is a SQL aggregate; no run is loaded to be
// counted in memory.
package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
)

const maxTopFailing = 5

// Overview is the answer to "how are deployments going" for one period and,
// for comparison, the period of the same length before it.
type Overview struct {
	From, To           time.Time
	Runs, PrevRuns     int
	SuccessRate        *float64 // nil when no run finished in the period; 0..1
	PrevSuccessRate    *float64
	MedianDuration     *time.Duration
	PrevMedianDuration *time.Duration
	WaitingApproval    int
	OldestWaiting      *time.Time
	Days               []Day // one per day of the period, oldest first, zeros included
	TopFailing         []ServiceFailures
	Attention          []AttentionItem
	Environments       []EnvLast
}

// Day counts the runs of one UTC date. Failed includes partially_succeeded;
// Other is cancelled and invalidated runs.
type Day struct {
	Date                     string
	Succeeded, Failed, Other int
}

type ServiceFailures struct {
	Service      string
	Failures     int
	CommonReason string
}

type AttentionItem struct {
	RunID                                      int64
	State, Job, Environment, Label, Reason, By string
	At                                         time.Time
}

// EnvLast is the latest run of an environment; RunID and At are nil and the
// strings empty when it has none.
type EnvLast struct {
	Environment, Label string
	RunID              *int64
	State, Job         string
	At                 *time.Time
}

// countedRuns selects the runs the overview counts, with $1 the label filter
// (” for none). A run counts once it has left planning: kind deploy or
// restore and state not resolving or planned. Its period timestamp ts is
// started_at, or created_at for a run that never started (cancelled or
// invalidated while queued or waiting). Accepted cost: a run planned on one
// day and started on another is bucketed by the day it started, and one that
// never started by the day it was created.
const countedRuns = `SELECT id, state, label, environment, job_id, requested_by, started_at, finished_at,
		state_changed_at, end_reason, created_at, coalesce(started_at, created_at) AS ts
	FROM runs
	WHERE kind IN ('deploy', 'restore') AND state NOT IN ('resolving', 'planned')
		AND ($1::text = '' OR label = $1)`

const finishedStates = `('succeeded', 'partially_succeeded', 'failed')`

// Load computes the overview for the last days whole UTC days ending today (to
// now) and the same number of days before them. label limits it to one
// environment label; "" means all. The principal is not used to scope the
// answer in version 1, when every signed-in person may view every run; it is
// a parameter so scoping can be added without changing callers.
func Load(ctx context.Context, db store.DBTX, _ auth.Principal, label string, days int, now time.Time) (Overview, error) {
	if days < 1 {
		return Overview{}, errors.New("overview: days must be at least 1")
	}
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -(days - 1))
	prevFrom := from.AddDate(0, 0, -days)
	out := Overview{From: from, To: now, Days: []Day{}, TopFailing: []ServiceFailures{}, Attention: []AttentionItem{}, Environments: []EnvLast{}}

	if err := loadTotals(ctx, db, &out, label, prevFrom, from, now); err != nil {
		return Overview{}, err
	}
	if err := loadWaiting(ctx, db, &out, label); err != nil {
		return Overview{}, err
	}
	if err := loadDays(ctx, db, &out, label, from, today, now); err != nil {
		return Overview{}, err
	}
	if err := loadTopFailing(ctx, db, &out, label, from, now); err != nil {
		return Overview{}, err
	}
	if err := loadAttention(ctx, db, &out, label, from, now); err != nil {
		return Overview{}, err
	}
	if err := loadEnvironments(ctx, db, &out, label); err != nil {
		return Overview{}, err
	}
	return out, nil
}

func rate(succeeded, finished int) *float64 {
	if finished == 0 {
		return nil
	}
	r := float64(succeeded) / float64(finished)
	return &r
}

func seconds(s *float64) *time.Duration {
	if s == nil {
		return nil
	}
	d := time.Duration(*s * float64(time.Second))
	return &d
}

func loadTotals(ctx context.Context, db store.DBTX, out *Overview, label string, prevFrom, from, now time.Time) error {
	const cur, prev = `ts >= $3 AND ts <= $4`, `ts >= $2 AND ts < $3`
	const fin = ` AND state IN ` + finishedStates
	const withDuration = fin + ` AND started_at IS NOT NULL AND finished_at IS NOT NULL`
	const median = `percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM finished_at - started_at)::float8)`
	var succeeded, finished, prevSucceeded, prevFinished int
	var medianSeconds, prevMedianSeconds *float64
	err := db.QueryRow(ctx, `WITH c AS (`+countedRuns+`)
		SELECT count(*) FILTER (WHERE `+cur+`),
			count(*) FILTER (WHERE `+prev+`),
			count(*) FILTER (WHERE `+cur+` AND state = 'succeeded'),
			count(*) FILTER (WHERE `+cur+fin+`),
			count(*) FILTER (WHERE `+prev+` AND state = 'succeeded'),
			count(*) FILTER (WHERE `+prev+fin+`),
			`+median+` FILTER (WHERE `+cur+withDuration+`),
			`+median+` FILTER (WHERE `+prev+withDuration+`)
		FROM c WHERE ts >= $2 AND ts <= $4`, label, prevFrom, from, now).
		Scan(&out.Runs, &out.PrevRuns, &succeeded, &finished, &prevSucceeded, &prevFinished, &medianSeconds, &prevMedianSeconds)
	if err != nil {
		return fmt.Errorf("overview totals: %w", err)
	}
	out.SuccessRate, out.PrevSuccessRate = rate(succeeded, finished), rate(prevSucceeded, prevFinished)
	out.MedianDuration, out.PrevMedianDuration = seconds(medianSeconds), seconds(prevMedianSeconds)
	return nil
}

// loadWaiting counts the runs waiting for approval now, whatever their age.
func loadWaiting(ctx context.Context, db store.DBTX, out *Overview, label string) error {
	var oldest *time.Time
	err := db.QueryRow(ctx, `WITH c AS (`+countedRuns+`)
		SELECT count(*), min(state_changed_at) FROM c WHERE state = 'waiting_approval'`, label).
		Scan(&out.WaitingApproval, &oldest)
	if err != nil {
		return fmt.Errorf("overview waiting: %w", err)
	}
	out.OldestWaiting = utc(oldest)
	return nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func loadDays(ctx context.Context, db store.DBTX, out *Overview, label string, from, today, now time.Time) error {
	rows, err := db.Query(ctx, `WITH c AS (`+countedRuns+`),
		bucketed AS (SELECT (ts AT TIME ZONE 'UTC')::date AS day, state FROM c WHERE ts >= $2 AND ts <= $3)
		SELECT to_char(g, 'YYYY-MM-DD'),
			count(b.state) FILTER (WHERE b.state = 'succeeded'),
			count(b.state) FILTER (WHERE b.state IN ('failed', 'partially_succeeded')),
			count(b.state) FILTER (WHERE b.state IN ('cancelled', 'invalidated'))
		FROM generate_series($4::date, $5::date, interval '1 day') AS g
		LEFT JOIN bucketed b ON b.day = g::date
		GROUP BY g ORDER BY g`,
		label, from, now, from.Format(time.DateOnly), today.Format(time.DateOnly))
	if err != nil {
		return fmt.Errorf("overview days: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.Date, &d.Succeeded, &d.Failed, &d.Other); err != nil {
			return fmt.Errorf("overview days: %w", err)
		}
		out.Days = append(out.Days, d)
	}
	return rows.Err()
}

// loadTopFailing groups the failed ecs-deploy and copy-config legs of the
// period by service. The common reason is the most frequent non-empty short
// failure of the service, the alphabetically first on a tie.
func loadTopFailing(ctx context.Context, db store.DBTX, out *Overview, label string, from, now time.Time) error {
	rows, err := db.Query(ctx, `WITH c AS (`+countedRuns+`),
		f AS (
			SELECT l.service, l.failure_short
			FROM c JOIN legs l ON l.run_id = c.id JOIN run_steps s ON s.id = l.step_id
			WHERE c.ts >= $2 AND c.ts <= $3 AND l.state = 'failed' AND s.use IN ('ecs-deploy', 'copy-config')),
		totals AS (SELECT service, count(*) AS failures FROM f GROUP BY service),
		reasons AS (
			SELECT DISTINCT ON (service) service, failure_short
			FROM (SELECT service, failure_short, count(*) AS n FROM f WHERE failure_short <> '' GROUP BY service, failure_short) r
			ORDER BY service, n DESC, failure_short)
		SELECT t.service, t.failures, coalesce(r.failure_short, '')
		FROM totals t LEFT JOIN reasons r ON r.service = t.service
		ORDER BY t.failures DESC, t.service
		LIMIT `+fmt.Sprint(maxTopFailing), label, from, now)
	if err != nil {
		return fmt.Errorf("overview top failing: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s ServiceFailures
		if err := rows.Scan(&s.Service, &s.Failures, &s.CommonReason); err != nil {
			return fmt.Errorf("overview top failing: %w", err)
		}
		out.TopFailing = append(out.TopFailing, s)
	}
	return rows.Err()
}

// loadAttention lists the runs that need a person: those waiting for approval
// or reconciliation now (oldest first), then each job and environment whose
// latest finished run failed (newest first). A failure a later run fixed is
// not listed, because that later run is the latest.
func loadAttention(ctx context.Context, db store.DBTX, out *Overview, label string, from, now time.Time) error {
	const columns = `r.id, r.state, r.job_id, r.environment, r.label, coalesce(nullif(u.name, ''), u.email, '')`
	queries := []struct {
		sql  string
		args []any
	}{
		{`WITH c AS (` + countedRuns + `)
			SELECT ` + columns + `,
				CASE r.state WHEN 'waiting_approval' THEN 'Waiting for approval' ELSE 'Needs reconciliation' END,
				r.state_changed_at
			FROM c r LEFT JOIN users u ON u.id = r.requested_by
			WHERE r.state IN ('waiting_approval', 'needs_reconciliation')
			ORDER BY r.state_changed_at, r.id`, []any{label}},
		{`WITH c AS (` + countedRuns + `)
			SELECT ` + columns + `,
				coalesce(nullif(r.end_reason, ''),
					(SELECT l.failure_short FROM legs l WHERE l.run_id = r.id AND l.state = 'failed' ORDER BY l.id LIMIT 1), ''),
				r.finished_at
			FROM (SELECT DISTINCT ON (job_id, environment) *
				FROM c WHERE state IN ` + finishedStates + ` AND finished_at IS NOT NULL
				ORDER BY job_id, environment, finished_at DESC, id DESC) r
			LEFT JOIN users u ON u.id = r.requested_by
			WHERE r.state IN ('failed', 'partially_succeeded') AND r.finished_at >= $2 AND r.finished_at <= $3
			ORDER BY r.finished_at DESC, r.id DESC`, []any{label, from, now}},
	}
	for _, q := range queries {
		rows, err := db.Query(ctx, q.sql, q.args...)
		if err != nil {
			return fmt.Errorf("overview attention: %w", err)
		}
		for rows.Next() {
			var item AttentionItem
			if err := rows.Scan(&item.RunID, &item.State, &item.Job, &item.Environment, &item.Label, &item.By, &item.Reason, &item.At); err != nil {
				rows.Close()
				return fmt.Errorf("overview attention: %w", err)
			}
			item.At = item.At.UTC()
			out.Attention = append(out.Attention, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("overview attention: %w", err)
		}
	}
	return nil
}

// loadEnvironments lists the environments of the active inventory (those with
// the label, if one is given) with the latest counted run of each, whatever
// its age.
func loadEnvironments(ctx context.Context, db store.DBTX, out *Overview, label string) error {
	snapshot, active, err := store.ActiveSnapshot(ctx, db)
	if err != nil {
		return fmt.Errorf("overview environments: %w", err)
	}
	if !active {
		return nil
	}
	var inventory spec.Inventory
	if err := json.Unmarshal(snapshot.Inventory, &inventory); err != nil {
		return fmt.Errorf("overview environments: decode inventory: %w", err)
	}
	var names []string
	for _, e := range inventory.Environments {
		if label == "" || e.Label == label {
			names = append(names, e.Name)
			out.Environments = append(out.Environments, EnvLast{Environment: e.Name, Label: e.Label})
		}
	}
	if len(names) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `SELECT DISTINCT ON (environment) environment, id, state, job_id,
			coalesce(finished_at, started_at, created_at)
		FROM (`+countedRuns+`) c WHERE environment = ANY($2)
		ORDER BY environment, ts DESC, id DESC`, label, names)
	if err != nil {
		return fmt.Errorf("overview environments: %w", err)
	}
	defer rows.Close()
	last := map[string]EnvLast{}
	for rows.Next() {
		var name string
		var e EnvLast
		var id int64
		var at time.Time
		if err := rows.Scan(&name, &id, &e.State, &e.Job, &at); err != nil {
			return fmt.Errorf("overview environments: %w", err)
		}
		at = at.UTC()
		e.RunID, e.At = &id, &at
		last[name] = e
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("overview environments: %w", err)
	}
	for i, env := range out.Environments {
		if e, ok := last[env.Environment]; ok {
			out.Environments[i].RunID, out.Environments[i].State, out.Environments[i].Job, out.Environments[i].At = e.RunID, e.State, e.Job, e.At
		}
	}
	return nil
}
