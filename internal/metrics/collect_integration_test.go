//go:build integration

package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	url := testdb.New(t)
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixture creates what runs and legs hang on.
type fixture struct {
	pool                                   *pgxpool.Pool
	userID, folderID, sourceID, snapshotID int64
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	user, err := store.CreateLocalUser(ctx, pool, "metrics@example.com", "Metrics", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	folder, err := store.CreateFolder(ctx, pool, "metrics-folder")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	source, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: folder.ID, Repo: "acme/jobs", Branch: "main", Path: "deploy.yaml"})
	if err != nil {
		t.Fatalf("CreateJobSource: %v", err)
	}
	snapshot, err := store.ActivateSnapshot(ctx, pool, "metrics-inventory", []byte(`{}`), nil, time.Now())
	if err != nil {
		t.Fatalf("ActivateSnapshot: %v", err)
	}
	return fixture{pool: pool, userID: user.ID, folderID: folder.ID, sourceID: source.ID, snapshotID: snapshot.ID}
}

// run inserts a run in the given state with the given start and finish.
func (f fixture) run(t *testing.T, state string, startedAt, finishedAt *time.Time) store.Run {
	t.Helper()
	r, err := store.InsertRun(context.Background(), f.pool, store.Run{
		Kind: "deploy", InventorySnapshotID: f.snapshotID, JobSourceID: f.sourceID, JobID: "deploy-api", JobSnapshot: []byte(`{"id":"deploy-api"}`),
		Environment: "prod-eu", AWSAccount: "111122223333", FolderID: f.folderID, Selection: []byte(`{}`),
		State: state, RequestedBy: f.userID, Via: "web",
	}, nil, nil)
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	// InsertRun does not store the times; set them as the engine does.
	if _, err := f.pool.Exec(context.Background(), `UPDATE runs SET started_at = $2, finished_at = $3 WHERE id = $1`, r.ID, startedAt, finishedAt); err != nil {
		t.Fatalf("set run times: %v", err)
	}
	return r
}

func (f fixture) leg(t *testing.T, runID int64, state string, queuedAt *time.Time) {
	t.Helper()
	if _, err := store.InsertLeg(context.Background(), f.pool, store.Leg{RunID: runID, Kind: "resolve", State: state, QueuedAt: queuedAt}); err != nil {
		t.Fatalf("InsertLeg: %v", err)
	}
}

func valuesOf(samples []Sample, name string) []float64 {
	var out []float64
	for _, s := range samples {
		if s.Name == name {
			out = append(out, s.Value)
		}
	}
	return out
}

func one(t *testing.T, samples []Sample, name string) float64 {
	t.Helper()
	got := valuesOf(samples, name)
	if len(got) != 1 {
		t.Fatalf("%s has %d samples %v, want exactly one", name, len(got), got)
	}
	return got[0]
}

func ptr[T any](v T) *T { return &v }

func TestCollect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	f := newFixture(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ago := func(d time.Duration) *time.Time { return ptr(now.Add(-d)) }

	// Runs: the window is (now-60s, now]. One at exactly now-60s is outside, one at now is inside.
	f.run(t, "succeeded", ago(70*time.Second), ago(10*time.Second))
	f.run(t, "failed", ago(30*time.Second), ago(0))
	f.run(t, "partially_succeeded", ago(100*time.Second), ago(60*time.Second))
	f.run(t, "cancelled", nil, ago(59*time.Second))
	f.run(t, "succeeded", ago(5*time.Second), ptr(now.Add(time.Second)))
	running := f.run(t, "running", ago(5*time.Second), nil)

	// Legs: two queued (the older at 300s), others do not count.
	f.leg(t, running.ID, "queued", ago(300*time.Second))
	f.leg(t, running.ID, "queued", ago(50*time.Second))
	f.leg(t, running.ID, "running", ago(900*time.Second))
	f.leg(t, running.ID, "pending", nil)

	// Runners: seen within two minutes counts.
	for id, seen := range map[string]time.Duration{"a": 10 * time.Second, "b": 119 * time.Second, "c": 121 * time.Second} {
		if err := store.UpsertRunner(ctx, pool, store.RunnerInfo{ID: id, Version: "v", Concurrency: 1, StartedAt: now.Add(-time.Hour), LastSeenAt: now.Add(-seen)}); err != nil {
			t.Fatal(err)
		}
	}

	// Events.
	for _, ev := range []struct {
		kind string
		ago  time.Duration
	}{
		{"lease_expired", 60 * time.Second}, {"lease_expired", 59 * time.Second}, {"lease_expired", 0},
		{"log_persist_failed", 30 * time.Second}, {"log_persist_failed", 5 * time.Minute},
		{"sync_failed", 1 * time.Second},
	} {
		if err := store.RecordEvent(ctx, pool, ev.kind, now.Add(-ev.ago)); err != nil {
			t.Fatal(err)
		}
	}

	// Notifications: two pending, three failed of which one failed exactly at now-60s.
	notify := func(kind, state string, failedAt *time.Time) {
		t.Helper()
		run := f.run(t, "succeeded", nil, ptr(now.Add(-time.Hour)))
		if err := store.EnqueueNotification(ctx, pool, store.Notification{RunID: run.ID, Kind: kind, Channel: "#deploys", Payload: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE notifications SET state = $1, failed_at = $2 WHERE run_id = $3`, state, failedAt, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	notify("started", "pending", nil)
	notify("started", "pending", nil)
	notify("started", "failed", ago(60*time.Second))
	notify("started", "failed", ago(1*time.Second))
	notify("started", "failed", ago(0))
	notify("started", "sent", nil)

	// Syncs: the inventory 1000s ago, one job 4000s ago, another never.
	if _, err := pool.Exec(ctx, `UPDATE inventory_status SET checked_at = $1 WHERE id = 1`, now.Add(-1000*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE job_sources SET checked_at = $1 WHERE id = $2`, now.Add(-4000*time.Second), f.sourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: f.folderID, Repo: "acme/jobs", Branch: "main", Path: "never-synced.yaml"}); err != nil {
		t.Fatal(err)
	}

	samples, err := Collect(ctx, pool, now)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for name, want := range map[string]float64{
		"QueueDepth":             2,
		"OldestQueuedLegAge":     300,
		"ActiveRunners":          2,
		"ExpiredLeases":          2,
		"RunsFinished":           3, // now-10s, now, now-59s; not now-60s, not now+1s
		"RunsFailed":             1, // the failed one at now; the partially_succeeded one is at now-60s
		"SyncAge":                4000,
		"LogPersistenceFailures": 1,
		"NotificationFailures":   2,
		"NotificationsPending":   2,
	} {
		if got := one(t, samples, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if got := valuesOf(samples, "RunDuration"); len(got) != 2 || !(got[0] == 60 && got[1] == 30 || got[0] == 30 && got[1] == 60) {
		t.Errorf("RunDuration = %v, want one sample each of 60 and 30 (the run without started_at has none)", got)
	}
	units := map[string]string{}
	for _, s := range samples {
		units[s.Name] = s.Unit
	}
	for name, want := range map[string]string{"QueueDepth": "Count", "OldestQueuedLegAge": "Seconds", "RunDuration": "Seconds", "SyncAge": "Seconds", "RunsFailed": "Count"} {
		if units[name] != want {
			t.Errorf("unit of %s = %q, want %q", name, units[name], want)
		}
	}
}

func TestCollectOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()
	pool := newPool(t)
	samples, err := Collect(context.Background(), pool, time.Now().UTC())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, name := range []string{"QueueDepth", "OldestQueuedLegAge", "ActiveRunners", "ExpiredLeases", "RunsFinished", "RunsFailed",
		"LogPersistenceFailures", "NotificationFailures", "NotificationsPending"} {
		if got := one(t, samples, name); got != 0 {
			t.Errorf("%s = %v, want 0 on an empty database", name, got)
		}
	}
	// Config refuses dimensions named like a metric, so its list must cover every name Collect emits.
	f := newFixture(t, pool)
	f.run(t, "succeeded", ptr(time.Now().Add(-5*time.Second)), ptr(time.Now()))
	if _, err := pool.Exec(context.Background(), `UPDATE inventory_status SET checked_at = now() WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	all, err := Collect(context.Background(), pool, time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, s := range all {
		if !slices.Contains(config.MetricNames, s.Name) {
			t.Errorf("Collect emits %s, which is missing from config.MetricNames", s.Name)
		}
	}
	// Nothing has finished and nothing has ever synced, so these have no value.
	for _, name := range []string{"RunDuration", "SyncAge"} {
		if got := valuesOf(samples, name); len(got) != 0 {
			t.Errorf("%s = %v, want no samples", name, got)
		}
	}
}

// lockedBuffer is a bytes.Buffer that an emitter goroutine and the test share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) lines() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), "\n")
}

func (l *lockedBuffer) first() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	line, _, _ := strings.Cut(l.b.String(), "\n")
	return line
}

type runningEmitter struct {
	out    *lockedBuffer
	cancel context.CancelFunc
	done   chan struct{}
}

func startEmitter(t *testing.T, pool *pgxpool.Pool, dims map[string]string) *runningEmitter {
	t.Helper()
	out := &lockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	re := &runningEmitter{out: out, cancel: cancel, done: make(chan struct{})}
	e := NewEmitter(pool, out, "Yumdrake", dims, 30*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { defer close(re.done); e.Run(ctx) }()
	t.Cleanup(re.stop)
	return re
}

func (re *runningEmitter) stop() {
	re.cancel()
	<-re.done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEmitterWritesEMFLines(t *testing.T) {
	t.Parallel()
	pool := newPool(t)
	re := startEmitter(t, pool, map[string]string{"Environment": "prod"})
	waitFor(t, "two lines", func() bool { return re.out.lines() >= 2 })
	re.stop()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(re.out.first()), &doc); err != nil {
		t.Fatalf("first line is not JSON: %v\n%s", err, re.out.first())
	}
	if string(doc["Environment"]) != `"prod"` || string(doc["QueueDepth"]) != `0` || doc["_aws"] == nil {
		t.Fatalf("first line = %s, want the EMF directive, the Environment dimension and QueueDepth 0", re.out.first())
	}
}

func TestOnlyOneReplicaEmits(t *testing.T) {
	t.Parallel()
	pool := newPool(t)
	first := startEmitter(t, pool, nil)
	waitFor(t, "the first emitter to write", func() bool { return first.out.lines() >= 1 })
	second := startEmitter(t, pool, nil)

	// Several ticks of both: only the lock holder writes.
	time.Sleep(300 * time.Millisecond)
	if n := second.out.lines(); n != 0 {
		t.Fatalf("the replica without the lock wrote %d lines, want none", n)
	}
	if n := first.out.lines(); n < 3 {
		t.Fatalf("the lock holder wrote %d lines in 300ms with a 30ms tick, want several", n)
	}

	// When the holder stops, the other takes over.
	first.stop()
	waitFor(t, "the other replica to take over", func() bool { return second.out.lines() >= 1 })
}
