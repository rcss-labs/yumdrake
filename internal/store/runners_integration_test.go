//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestUpsertRunnerAndListSeenSince(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	started := now.Add(-time.Hour)
	if err := UpsertRunner(ctx, pool, RunnerInfo{ID: "r1", Version: "1.0", Concurrency: 4, Busy: 1, StartedAt: started, LastSeenAt: now}); err != nil {
		t.Fatalf("UpsertRunner: %v", err)
	}
	// The same id again updates the row instead of adding one.
	if err := UpsertRunner(ctx, pool, RunnerInfo{ID: "r1", Version: "1.1", Concurrency: 4, Busy: 3, StartedAt: started, LastSeenAt: now.Add(10 * time.Second)}); err != nil {
		t.Fatalf("UpsertRunner again: %v", err)
	}
	if err := UpsertRunner(ctx, pool, RunnerInfo{ID: "r2", Version: "1.0", Concurrency: 2, Busy: 0, StartedAt: now, LastSeenAt: now.Add(-5 * time.Minute)}); err != nil {
		t.Fatalf("UpsertRunner r2: %v", err)
	}

	got, err := ListRunners(ctx, pool, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListRunners = %+v, want only r1 (r2 was last seen 5 minutes ago)", got)
	}
	r := got[0]
	if r.ID != "r1" || r.Version != "1.1" || r.Concurrency != 4 || r.Busy != 3 ||
		!r.StartedAt.Equal(started) || !r.LastSeenAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("runner = %+v, want the second upsert's values", r)
	}

	all, err := ListRunners(ctx, pool, now.Add(-time.Hour))
	if err != nil || len(all) != 2 || all[0].ID != "r1" || all[1].ID != "r2" {
		t.Fatalf("ListRunners(all) = %+v, %v; want r1, r2 ordered by id", all, err)
	}
	none, err := ListRunners(ctx, pool, now.Add(time.Hour))
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("ListRunners(future) = %#v, %v; want an empty non-nil slice", none, err)
	}
}

func TestGoneRunnerDropsOutAfterTwoMinutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for id, seen := range map[string]time.Duration{"fresh": -30 * time.Second, "edge": -2*time.Minute + time.Second, "gone": -2*time.Minute - time.Second} {
		if err := UpsertRunner(ctx, pool, RunnerInfo{ID: id, Version: "v", Concurrency: 1, StartedAt: now.Add(-time.Hour), LastSeenAt: now.Add(seen)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListRunners(ctx, pool, now.Add(-RunnerGoneAfter))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.ID] = true
	}
	if len(got) != 2 || !ids["fresh"] || !ids["edge"] {
		t.Fatalf("runners seen within two minutes = %+v, want fresh and edge only", got)
	}
}

func TestRecordAndCountEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, at := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute), now.Add(-30 * time.Second), now} {
		if err := RecordEvent(ctx, pool, "lease_expired", at); err != nil {
			t.Fatalf("RecordEvent: %v", err)
		}
	}
	if err := RecordEvent(ctx, pool, "sync_failed", now); err != nil {
		t.Fatal(err)
	}
	// since is exclusive: the event at exactly now-1m is outside.
	if n, err := CountEvents(ctx, pool, "lease_expired", now.Add(-time.Minute)); err != nil || n != 2 {
		t.Fatalf("CountEvents = %d, %v; want 2 (now-30s and now)", n, err)
	}
	if n, err := CountEvents(ctx, pool, "log_persist_failed", now.Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("CountEvents(other kind) = %d, %v; want 0", n, err)
	}
	if err := RecordEvent(ctx, pool, "nonsense", now); err == nil {
		t.Fatal("RecordEvent accepted an unknown kind")
	}
}

func TestQueueStatsOldestQueued(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	run := fx.insert(t, pool, "running")
	now := time.Now().UTC().Truncate(time.Microsecond)

	if n, oldest, err := QueueStats(ctx, pool, now); err != nil || n != 0 || oldest != nil {
		t.Fatalf("QueueStats on an empty queue = %d, %v, %v; want 0, nil", n, oldest, err)
	}
	for _, l := range []Leg{
		{State: "queued", QueuedAt: ptr(now.Add(-10 * time.Minute))},
		{State: "queued", QueuedAt: ptr(now.Add(-2 * time.Minute))},
		// Not queued: must not count, even though it is older.
		{State: "running", QueuedAt: ptr(now.Add(-time.Hour))},
		{State: "pending"},
	} {
		l.RunID, l.Kind = run.ID, "resolve"
		if _, err := InsertLeg(ctx, pool, l); err != nil {
			t.Fatalf("InsertLeg: %v", err)
		}
	}
	n, oldest, err := QueueStats(ctx, pool, now)
	if err != nil || n != 2 || oldest == nil || !oldest.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("QueueStats = %d, %v, %v; want 2 queued, oldest %v", n, oldest, err, now.Add(-10*time.Minute))
	}
}

func TestOldRunnerRowsArePruned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-7 * 24 * time.Hour)

	for id, seen := range map[string]time.Time{"old": cutoff.Add(-time.Hour), "young": cutoff.Add(time.Hour)} {
		if err := UpsertRunner(ctx, pool, RunnerInfo{ID: id, Version: "v", Concurrency: 1, StartedAt: seen, LastSeenAt: seen}); err != nil {
			t.Fatal(err)
		}
	}
	for _, at := range []time.Time{cutoff.Add(-time.Hour), cutoff.Add(time.Hour)} {
		if err := RecordEvent(ctx, pool, "sync_failed", at); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := PruneRunners(ctx, pool, cutoff); err != nil || n != 1 {
		t.Fatalf("PruneRunners = %d, %v; want 1", n, err)
	}
	if n, err := PruneEvents(ctx, pool, cutoff); err != nil || n != 1 {
		t.Fatalf("PruneEvents = %d, %v; want 1", n, err)
	}
	left, err := ListRunners(ctx, pool, time.Time{})
	if err != nil || len(left) != 1 || left[0].ID != "young" {
		t.Fatalf("runners left = %+v, %v; want only the young one", left, err)
	}
	if n, err := CountEvents(ctx, pool, "sync_failed", time.Time{}); err != nil || n != 1 {
		t.Fatalf("events left = %d, %v; want 1", n, err)
	}
}

func ptr[T any](v T) *T { return &v }
