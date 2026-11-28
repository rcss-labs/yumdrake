//go:build integration

package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/github"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

const (
	invRepo  = "acme/inventory"
	invPath  = "inv"
	jobsRepo = "acme/jobs"
	jobPath  = "jobs/deploy-backend.yaml"
	jobPath2 = "jobs/other.yaml"
)

// fakeSource serves files from an in-memory map keyed "repo@commit/path".
type fakeSource struct {
	mu        gosync.Mutex
	files     map[string][]byte
	heads     map[string]string // "repo@branch" -> commit
	err       error
	headCalls int
	// listErr, when set, is returned by ListFiles only.
	listErr error
	// onRead, when set, runs at the start of every ReadFile without the lock
	// held, so a test can park a read until it chooses to release it.
	onRead func(repo, path string)
}

func newFakeSource() *fakeSource {
	return &fakeSource{files: map[string][]byte{}, heads: map[string]string{}}
}

func (f *fakeSource) put(repo, commit, path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[repo+"@"+commit+"/"+path] = data
}

func (f *fakeSource) remove(repo, commit, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, repo+"@"+commit+"/"+path)
}

func (f *fakeSource) setHead(repo, branch, commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads[repo+"@"+branch] = commit
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSource) headCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headCalls
}

func (f *fakeSource) HeadCommit(_ context.Context, repo, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headCalls++
	if f.err != nil {
		return "", f.err
	}
	return f.heads[repo+"@"+branch], nil
}

func (f *fakeSource) ListFiles(_ context.Context, repo, commit, dir string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.err != nil {
		return nil, f.err
	}
	prefix := repo + "@" + commit + "/"
	var out []string
	for key := range f.files {
		if p, ok := strings.CutPrefix(key, prefix); ok && strings.HasPrefix(p, dir+"/") {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeSource) ReadFile(_ context.Context, repo, commit, path string) ([]byte, error) {
	if f.onRead != nil {
		f.onRead(repo, path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.files[repo+"@"+commit+"/"+path]
	if !ok {
		return nil, fmt.Errorf("fake: %s@%s/%s: not found", repo, commit, path)
	}
	return data, nil
}

// harness wires an engine to a fresh database and a fake source.
type harness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	src    *fakeSource
	engine *Engine
	clock  time.Time
	jobIDs []int64
}

func fixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "spec", "testdata", "valid", rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return data
}

func newHarness(t *testing.T) *harness { return newHarnessMaxConns(t, 0) }

// newHarnessMaxConns limits the pool size when maxConns > 0.
func newHarnessMaxConns(t *testing.T, maxConns int32) *harness {
	t.Helper()
	ctx := context.Background()
	url := testdb.New(t)
	if err := db.Migrate(ctx, url); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var pool *pgxpool.Pool
	var err error
	if maxConns > 0 {
		cfg, perr := pgxpool.ParseConfig(url)
		if perr != nil {
			t.Fatalf("ParseConfig: %v", perr)
		}
		cfg.MaxConns = maxConns
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
	} else {
		pool, err = db.Open(ctx, url)
	}
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// The clock carries nanoseconds on purpose: PostgreSQL keeps microseconds.
	h := &harness{t: t, pool: pool, src: newFakeSource(), clock: time.Date(2026, 11, 27, 10, 0, 0, 123456789, time.UTC)}
	h.engine = NewEngine(pool,
		func(context.Context, string) (Source, error) { return h.src, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() time.Time { return h.clock })
	return h
}

func (h *harness) tick() time.Time {
	h.clock = h.clock.Add(time.Minute)
	return h.clock
}

func (h *harness) configureInventory() {
	h.t.Helper()
	if err := store.SetInventorySource(context.Background(), h.pool, store.InventorySource{Repo: invRepo, Branch: "main", Path: invPath}); err != nil {
		h.t.Fatalf("SetInventorySource: %v", err)
	}
}

// pushInventory publishes the valid fixture inventory at commit and moves head.
func (h *harness) pushInventory(commit string) {
	h.src.put(invRepo, commit, invPath+"/environments.yaml", fixture(h.t, "environments.yaml"))
	h.src.put(invRepo, commit, invPath+"/services/orders.yaml", fixture(h.t, "services/orders.yaml"))
	h.src.put(invRepo, commit, invPath+"/services/backend-api.yaml", fixture(h.t, "services/backend-api.yaml"))
	h.src.setHead(invRepo, "main", commit)
}

func (h *harness) addJob(path string) int64 {
	h.t.Helper()
	ctx := context.Background()
	folder, err := store.CreateFolder(ctx, h.pool, fmt.Sprintf("folder-%d", len(h.jobIDs)))
	if err != nil {
		h.t.Fatalf("CreateFolder: %v", err)
	}
	js, err := store.CreateJobSource(ctx, h.pool, store.JobSource{FolderID: folder.ID, Repo: jobsRepo, Branch: "main", Path: path})
	if err != nil {
		h.t.Fatalf("CreateJobSource: %v", err)
	}
	h.jobIDs = append(h.jobIDs, js.ID)
	return js.ID
}

func (h *harness) pushJob(path, commit string, data []byte) {
	h.src.put(jobsRepo, commit, path, data)
	h.src.setHead(jobsRepo, "main", commit)
}

func (h *harness) syncAll() error {
	h.t.Helper()
	return h.engine.SyncAll(context.Background())
}

func (h *harness) job(id int64) store.JobSource {
	h.t.Helper()
	js, err := store.GetJobSource(context.Background(), h.pool, id)
	if err != nil {
		h.t.Fatalf("GetJobSource: %v", err)
	}
	return js
}

func (h *harness) active() store.Snapshot {
	h.t.Helper()
	s, ok, err := store.ActiveSnapshot(context.Background(), h.pool)
	if err != nil || !ok {
		h.t.Fatalf("ActiveSnapshot: ok=%v err=%v", ok, err)
	}
	return s
}

func (h *harness) status() store.InventoryStatus {
	h.t.Helper()
	st, err := store.GetInventoryStatus(context.Background(), h.pool)
	if err != nil {
		h.t.Fatalf("GetInventoryStatus: %v", err)
	}
	return st
}

func decodeProblems(t *testing.T, raw []byte) spec.Problems {
	t.Helper()
	var p spec.Problems
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode problems %q: %v", raw, err)
	}
	return p
}

func problemsText(t *testing.T, raw []byte) string {
	t.Helper()
	return decodeProblems(t, raw).Error()
}

func (h *harness) inventoryOf(s store.Snapshot) spec.Inventory {
	var inv spec.Inventory
	if err := json.Unmarshal(s.Inventory, &inv); err != nil {
		h.t.Fatalf("decode inventory: %v", err)
	}
	return inv
}

// firstSync configures one inventory (c1), one job (j1) and syncs them.
func (h *harness) firstSync() int64 {
	h.t.Helper()
	h.configureInventory()
	h.pushInventory("c1")
	id := h.addJob(jobPath)
	h.pushJob(jobPath, "j1", fixture(h.t, "jobs/deploy-backend.yaml"))
	if err := h.syncAll(); err != nil {
		h.t.Fatalf("first SyncAll: %v", err)
	}
	return id
}

func TestFirstSyncActivatesInventoryAndJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.firstSync()

	snap := h.active()
	if snap.Commit != "c1" {
		t.Fatalf("active commit = %q, want c1", snap.Commit)
	}
	if got := h.inventoryOf(snap).ServiceNames(); strings.Join(got, ",") != "backend-api,orders" {
		t.Fatalf("services = %v", got)
	}
	st := h.status()
	if st.Problems != nil || st.ProblemsCommit != "" || st.CheckedAt == nil || !st.CheckedAt.Equal(micro(h.clock)) {
		t.Fatalf("unexpected status: %+v", st)
	}
	js := h.job(id)
	if !js.Runnable || js.Problems != nil || js.JobID != "deploy-backend" || js.Title != "Backend and microservices" ||
		js.LastGoodCommit != "j1" || js.LastGood == nil || js.SyncedAt == nil || !js.SyncedAt.Equal(micro(h.clock)) {
		t.Fatalf("unexpected job: %+v", js)
	}
}

func TestUnchangedCommitOnlyRecordsCheckTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.firstSync()
	snapBefore := h.active()
	jobBefore := h.job(id)

	later := h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if snapAfter := h.active(); snapAfter.ID != snapBefore.ID || !snapAfter.ActivatedAt.Equal(snapBefore.ActivatedAt) {
		t.Fatalf("snapshot changed: %+v -> %+v", snapBefore, snapAfter)
	}
	if st := h.status(); st.CheckedAt == nil || !st.CheckedAt.Equal(micro(later)) {
		t.Fatalf("status CheckedAt = %v, want %v", st.CheckedAt, later)
	}
	jobAfter := h.job(id)
	if jobAfter.CheckedAt == nil || !jobAfter.CheckedAt.Equal(micro(later)) {
		t.Fatalf("job CheckedAt = %v, want %v", jobAfter.CheckedAt, later)
	}
	if !jobAfter.SyncedAt.Equal(*jobBefore.SyncedAt) || !bytes.Equal(jobAfter.LastGood, jobBefore.LastGood) || !jobAfter.Runnable {
		t.Fatalf("job content changed: %+v", jobAfter)
	}
}

func brokenOrders(t *testing.T) []byte {
	t.Helper()
	return []byte(strings.Replace(string(fixture(t, "services/orders.yaml")), "name: orders", "name: orders\nbogus: true", 1))
}

func TestInvalidFileKeepsPreviousInventoryWhole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	before := h.active()

	h.pushInventory("c2")
	h.src.put(invRepo, "c2", invPath+"/services/orders.yaml", brokenOrders(t))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	after := h.active()
	if after.ID != before.ID || after.Commit != "c1" {
		t.Fatalf("active snapshot changed: %+v", after)
	}
	if got := h.inventoryOf(after).ServiceNames(); len(got) != 2 {
		t.Fatalf("active services = %v, want both", got)
	}
	st := h.status()
	if st.ProblemsCommit != "c2" {
		t.Fatalf("ProblemsCommit = %q, want c2", st.ProblemsCommit)
	}
	text := problemsText(t, st.Problems)
	if !strings.Contains(text, "services/orders.yaml") || !strings.Contains(text, "bogus") {
		t.Fatalf("problems do not name the broken file and field:\n%s", text)
	}
	if strings.Contains(text, "backend-api.yaml") {
		t.Fatalf("problems blame a valid file:\n%s", text)
	}
}

func TestFixedCommitActivatesAndClearsProblems(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	h.pushInventory("c2")
	h.src.put(invRepo, "c2", invPath+"/services/orders.yaml", brokenOrders(t))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if h.status().Problems == nil {
		t.Fatal("expected problems after the broken commit")
	}

	h.pushInventory("c3")
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := h.active().Commit; got != "c3" {
		t.Fatalf("active commit = %q, want c3", got)
	}
	if st := h.status(); st.Problems != nil || st.ProblemsCommit != "" {
		t.Fatalf("problems not cleared: %+v", st)
	}
}

func jobWithGroupService(t *testing.T, service string) []byte {
	t.Helper()
	return []byte(strings.Replace(string(fixture(t, "jobs/deploy-backend.yaml")), "services: [orders]", "services: ["+service+"]", 1))
}

func TestJobInvalidAgainstInventoryKeepsLastGood(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.firstSync()
	before := h.job(id)

	h.pushJob(jobPath, "j2", jobWithGroupService(t, "no-such-service"))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	after := h.job(id)
	if !bytes.Equal(after.LastGood, before.LastGood) || after.LastGoodCommit != "j1" {
		t.Fatalf("last good changed: %q @ %s", after.LastGood, after.LastGoodCommit)
	}
	if after.ProblemsCommit != "j2" || !strings.Contains(problemsText(t, after.Problems), "no-such-service") {
		t.Fatalf("problems not saved for j2: %+v", after)
	}
	if !after.Runnable {
		t.Fatal("last good is still valid, job should stay runnable")
	}
}

func TestInventoryChangeMakesJobNotRunnable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.firstSync()
	before := h.job(id)

	h.pushInventory("c2")
	h.src.remove(invRepo, "c2", invPath+"/services/orders.yaml")
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := h.active().Commit; got != "c2" {
		t.Fatalf("active commit = %q, want c2", got)
	}
	after := h.job(id)
	if after.Runnable || after.Problems == nil || !strings.Contains(problemsText(t, after.Problems), "orders") {
		t.Fatalf("job should be not runnable with a reason naming orders: %+v", after)
	}
	if !bytes.Equal(after.LastGood, before.LastGood) || after.LastGoodCommit != "j1" {
		t.Fatalf("LastGood not intact: %+v", after)
	}

	// The next round must not undo that, and restoring the service heals it.
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if h.job(id).Runnable {
		t.Fatal("job became runnable without the service")
	}
	h.pushInventory("c3")
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	healed := h.job(id)
	if !healed.Runnable || healed.Problems != nil {
		t.Fatalf("job should be runnable again: %+v", healed)
	}
}

func TestInventoryChangeKeepsJobsOwnProblems(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.firstSync()
	h.pushJob(jobPath, "j2", jobWithGroupService(t, "no-such-service"))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	own := h.job(id)

	// A valid new inventory without orders: j1 no longer validates, but the
	// problems of the newer j2 must not be overwritten.
	h.pushInventory("c2")
	h.src.remove(invRepo, "c2", invPath+"/services/orders.yaml")
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	after := h.job(id)
	if after.Runnable {
		t.Fatal("last good no longer validates, job must not be runnable")
	}
	if after.ProblemsCommit != "j2" || string(after.Problems) != string(own.Problems) {
		t.Fatalf("job's own problems were overwritten: %+v", after)
	}
}

func TestInventoryFixMakesInvalidJobValid(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.firstSync()
	// j2 names a service the inventory only gains at c2.
	h.pushJob(jobPath, "j2", jobWithGroupService(t, "payments"))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if h.job(id).ProblemsCommit != "j2" {
		t.Fatal("expected j2 to be rejected")
	}

	h.pushInventory("c2")
	payments := strings.ReplaceAll(string(fixture(t, "services/orders.yaml")), "orders", "payments")
	h.src.put(invRepo, "c2", invPath+"/services/payments.yaml", []byte(payments))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	got := h.job(id)
	if got.LastGoodCommit != "j2" || got.Problems != nil || !got.Runnable {
		t.Fatalf("j2 should be accepted after the inventory gained the service: %+v", got)
	}
}

func TestJobWithoutActiveInventoryReportsProblemAndRecovers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.configureInventory()
	id := h.addJob(jobPath)
	h.pushJob(jobPath, "j1", fixture(t, "jobs/deploy-backend.yaml"))
	if err := h.engine.SyncJob(context.Background(), id); err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	js := h.job(id)
	if js.Runnable || js.LastGood != nil || !strings.Contains(problemsText(t, js.Problems), "no active inventory yet") {
		t.Fatalf("unexpected job: %+v", js)
	}

	h.pushInventory("c1")
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if js := h.job(id); !js.Runnable || js.LastGoodCommit != "j1" {
		t.Fatalf("job should recover once an inventory exists: %+v", js)
	}
}

// dumpTables renders every row of the sync tables as text for exact comparison.
func dumpTables(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var out strings.Builder
	for _, q := range []string{
		"SELECT (to_jsonb(t) - 'sync_error' - 'sync_error_at')::text FROM job_sources t ORDER BY id",
		"SELECT t::text FROM inventory_snapshots t ORDER BY id",
		"SELECT (to_jsonb(t) - 'sync_error' - 'sync_error_at')::text FROM inventory_status t ORDER BY id",
		"SELECT t::text FROM folders t ORDER BY id",
	} {
		rows, err := pool.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("dump %q: %v", q, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out.WriteString(line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
	}
	return out.String()
}

func TestSourceErrorChangesNothingAndIsReturned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	h.addJob(jobPath2)
	h.src.put(jobsRepo, "j1", jobPath2, fixture(t, "jobs/deploy-backend.yaml"))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	before := dumpTables(t, h.pool)

	boom := errors.New("github: 502 bad gateway")
	h.src.setErr(boom)
	callsBefore := h.src.headCallCount()
	h.tick()
	err := h.syncAll()
	if !errors.Is(err, boom) {
		t.Fatalf("SyncAll error = %v, want it to wrap the source error", err)
	}
	if got := h.src.headCallCount() - callsBefore; got != 3 {
		t.Fatalf("HeadCommit attempts = %d, want 3 (inventory and both jobs)", got)
	}
	if after := dumpTables(t, h.pool); after != before {
		t.Fatalf("stored rows changed on a source error:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The failure is visible on the inventory status and on every job, with the
	// time it happened, and it is cleared by the next successful contact.
	ctx := context.Background()
	failedAt := h.clock
	if st := h.status(); !strings.Contains(st.SyncError, "502 bad gateway") || st.SyncErrorAt == nil || !st.SyncErrorAt.Equal(micro(failedAt)) {
		t.Fatalf("inventory sync error not recorded: %+v", st)
	}
	for _, id := range h.jobIDs {
		if js := h.job(id); !strings.Contains(js.SyncError, "502 bad gateway") || js.SyncErrorAt == nil || !js.SyncErrorAt.Equal(micro(failedAt)) {
			t.Fatalf("job %d sync error not recorded: %+v", id, js)
		}
	}
	h.src.setErr(nil)
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll after recovery: %v", err)
	}
	if st := h.status(); st.SyncError != "" || st.SyncErrorAt != nil {
		t.Fatalf("inventory sync error not cleared: %+v", st)
	}
	jobs, err := store.ListJobSources(ctx, h.pool)
	if err != nil {
		t.Fatalf("ListJobSources: %v", err)
	}
	for _, js := range jobs {
		if js.SyncError != "" || js.SyncErrorAt != nil {
			t.Fatalf("job %d sync error not cleared: %+v", js.ID, js)
		}
	}
}

func TestMissingConnectionIsASourceError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.configureInventory()
	h.pushInventory("c1")
	missing := errors.New("no connection for owner")
	h.engine.sources = func(context.Context, string) (Source, error) { return nil, missing }
	before := dumpTables(t, h.pool)
	if err := h.engine.SyncInventory(context.Background()); !errors.Is(err, missing) {
		t.Fatalf("SyncInventory error = %v", err)
	}
	if after := dumpTables(t, h.pool); after != before {
		t.Fatalf("rows changed:\n%s\n%s", before, after)
	}
}

func TestMissingEnvironmentsFileIsAProblemNotASourceError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.configureInventory()
	h.pushInventory("c1")
	h.src.remove(invRepo, "c1", invPath+"/environments.yaml")
	if err := h.engine.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}
	if _, ok, _ := store.ActiveSnapshot(context.Background(), h.pool); ok {
		t.Fatal("nothing should be active")
	}
	st := h.status()
	if st.ProblemsCommit != "c1" || !strings.Contains(problemsText(t, st.Problems), "environments.yaml") {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestRunSkipsRoundWhenAnotherHolderHasTheLock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.configureInventory()
	h.pushInventory("c1")
	ctx := context.Background()

	holder, err := h.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer holder.Release()
	var got bool
	if err := holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&got); err != nil || !got {
		t.Fatalf("holder lock: %v %v", got, err)
	}

	ran, err := h.engine.runRound(ctx)
	if err != nil || ran {
		t.Fatalf("runRound while locked: ran=%v err=%v", ran, err)
	}
	if calls := h.src.headCallCount(); calls != 0 {
		t.Fatalf("HeadCommit calls while another holder has the lock = %d", calls)
	}

	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", advisoryLockKey); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	ran, err = h.engine.runRound(ctx)
	if err != nil || !ran {
		t.Fatalf("runRound after release: ran=%v err=%v", ran, err)
	}
	if calls := h.src.headCallCount(); calls == 0 {
		t.Fatal("expected HeadCommit calls once the lock was free")
	}
	// The round released its lock: the holder can take it again.
	if err := holder.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", advisoryLockKey).Scan(&got); err != nil || !got {
		t.Fatalf("lock not released by the round: %v %v", got, err)
	}
}

func TestRunStopsWhenContextEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.engine.Run(ctx, time.Hour); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for h.src.headCallCount() == 0 && time.Now().Before(deadline) {
		h.configureInventory()
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop after the context ended")
	}
}

func TestNoInventorySourceIsNotAnError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.engine.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}
	if calls := h.src.headCallCount(); calls != 0 {
		t.Fatalf("HeadCommit calls = %d, want 0", calls)
	}
}

// An activation that removes a service must not lose its re-validation to a
// SyncJob that read the old inventory before the activation and writes after.
func TestActivationAndConcurrentJobSyncKeepJobNotRunnable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.firstSync()

	updated := strings.Replace(string(fixture(t, "jobs/deploy-backend.yaml")), "Backend and microservices", "Backend and microservices v2", 1)
	h.pushJob(jobPath, "j2", []byte(updated))
	h.pushInventory("c2")
	h.src.remove(invRepo, "c2", invPath+"/services/orders.yaml")

	entered := make(chan struct{})
	release := make(chan struct{})
	h.src.onRead = func(repo, _ string) {
		if repo == jobsRepo {
			close(entered)
			<-release
		}
	}
	h.tick()

	jobDone := make(chan error, 1)
	go func() { jobDone <- h.engine.SyncJob(context.Background(), id) }()
	<-entered // the job read is parked; it has seen the old inventory

	inventoryDone := make(chan error, 1)
	go func() { inventoryDone <- h.engine.SyncInventory(context.Background()) }()
	// Without serialisation the activation finishes now; with it, it waits for
	// the job sync. Either way the final state must be the same.
	select {
	case <-inventoryDone:
		inventoryDone <- nil
	case <-time.After(500 * time.Millisecond):
	}
	close(release)

	if err := <-jobDone; err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if err := <-inventoryDone; err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}
	if got := h.active().Commit; got != "c2" {
		t.Fatalf("active commit = %q, want c2", got)
	}
	got := h.job(id)
	if got.Runnable || !strings.Contains(problemsText(t, got.Problems), "orders") {
		t.Fatalf("job must be not runnable with a reason naming orders: %+v", got)
	}
}

// Callers waiting for the lock must not hold pool connections the lock holder
// needs: with a tiny pool, waiters used to starve the holder for good.
func TestWaitingSyncCallsDoNotStarveTheLockHolder(t *testing.T) {
	t.Parallel()
	h := newHarnessMaxConns(t, 2)
	id := h.firstSync()
	h.pushJob(jobPath, "j2", fixture(t, "jobs/deploy-backend.yaml"))

	entered := make(chan struct{})
	release := make(chan struct{})
	var once gosync.Once
	h.src.onRead = func(repo, _ string) {
		if repo != jobsRepo {
			return
		}
		once.Do(func() {
			close(entered)
			<-release
		})
	}

	// Registered after the pool's own cleanup, so it runs first and unblocks
	// everything if the test fails with callers still stuck.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	done := make(chan error, 4)
	go func() { done <- h.engine.SyncJob(ctx, id) }()
	<-entered
	for range 3 {
		go func() { done <- h.engine.SyncAll(ctx) }()
	}
	time.Sleep(300 * time.Millisecond) // let the waiters queue up
	close(release)

	timeout := time.After(5 * time.Second)
	for range 4 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("sync call failed: %v", err)
			}
		case <-timeout:
			t.Fatal("sync calls did not finish: waiters starved the lock holder of pool connections")
		}
	}
}

// micro is the precision PostgreSQL (and therefore the store) keeps.
func micro(t time.Time) time.Time { return t.Truncate(time.Microsecond) }

// setSource changes the inventory source the way the API does.
func (h *harness) setSource(path string) {
	h.t.Helper()
	if err := store.SetInventorySource(context.Background(), h.pool, store.InventorySource{Repo: invRepo, Branch: "main", Path: path}); err != nil {
		h.t.Fatalf("SetInventorySource: %v", err)
	}
}

func TestCorrectedInventoryPathIsReadAtTheSameHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.setSource("typo")
	h.pushInventory("c1")
	if err := h.engine.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}
	if st := h.status(); st.ProblemsCommit != "c1" || !strings.Contains(problemsText(t, st.Problems), "not found") {
		t.Fatalf("expected the missing-file problem, got %+v", st)
	}

	h.tick()
	h.setSource(invPath)
	if err := h.engine.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory after the fix: %v", err)
	}
	if got := h.active().Commit; got != "c1" {
		t.Fatalf("active commit = %q, want c1", got)
	}
	if st := h.status(); st.Problems != nil || st.ProblemsCommit != "" {
		t.Fatalf("stale problems remain: %+v", st)
	}
}

func TestMovedInventoryPathReplacesTheActiveInventoryAtTheSameHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	before := h.active()

	// A second, smaller inventory at the same commit under another path.
	h.src.put(invRepo, "c1", "other/environments.yaml", fixture(t, "environments.yaml"))
	h.src.put(invRepo, "c1", "other/services/orders.yaml", fixture(t, "services/orders.yaml"))
	h.tick()
	h.setSource("other")
	if err := h.engine.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}
	after := h.active()
	if after.ID == before.ID {
		t.Fatal("the active snapshot still serves the old path's inventory")
	}
	if got := strings.Join(h.inventoryOf(after).ServiceNames(), ","); got != "orders" {
		t.Fatalf("services = %q, want the new path's orders only", got)
	}
	var active int
	if err := h.pool.QueryRow(context.Background(), "SELECT count(*) FROM inventory_snapshots WHERE active").Scan(&active); err != nil || active != 1 {
		t.Fatalf("active snapshots = %d, %v; want exactly one", active, err)
	}
}

func TestSourceChangeIsReadByThePollEvenAfterAFailedAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	h.src.put(invRepo, "c1", "other/environments.yaml", fixture(t, "environments.yaml"))
	h.src.put(invRepo, "c1", "other/services/orders.yaml", fixture(t, "services/orders.yaml"))
	h.setSource("other")

	h.src.setErr(errors.New("github is down"))
	if err := h.engine.SyncInventory(context.Background()); err == nil {
		t.Fatal("expected the source error")
	}
	h.src.setErr(nil)
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := strings.Join(h.inventoryOf(h.active()).ServiceNames(), ","); got != "orders" {
		t.Fatalf("services = %q, want the new path's orders only", got)
	}
}

func TestUnchangedSourceKeepsTheStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	before := h.status()
	h.setSource(invPath) // the same value again
	if after := h.status(); after.CheckedAt == nil || !after.CheckedAt.Equal(*before.CheckedAt) {
		t.Fatalf("status reset by an unchanged source: %+v -> %+v", before, after)
	}
}

func TestYmlServiceFileIsReportedNotIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.configureInventory()
	h.pushInventory("c1")
	h.src.put(invRepo, "c1", invPath+"/services/payments.yml", fixture(t, "services/orders.yaml"))
	if err := h.engine.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v", err)
	}
	text := problemsText(t, h.status().Problems)
	if !strings.Contains(text, "services/payments.yml") || !strings.Contains(text, ".yaml extension") {
		t.Fatalf("problems do not name the .yml file:\n%s", text)
	}
}

func TestRateLimitEndsTheRoundWithoutMoreGitHubCalls(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	h.addJob(jobPath2)
	h.addJob("jobs/third.yaml")
	before := h.src.headCallCount()
	limited := &github.RateLimitError{RetryAfter: time.Minute}
	h.src.setErr(limited)

	err := h.syncAll()
	var got *github.RateLimitError
	if !errors.As(err, &got) {
		t.Fatalf("SyncAll error = %v, want the rate limit error", err)
	}
	if calls := h.src.headCallCount() - before; calls != 1 {
		t.Fatalf("GitHub head lookups = %d, want 1: the round must stop at the rate limit", calls)
	}
}

func TestMissingInventoryDirectoryIsAProblemNotASourceError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.configureInventory()
	h.pushInventory("c1")
	h.src.listErr = fmt.Errorf("github: GET /repos/acme/inventory/git/trees/c1:inv: %w", github.ErrNotFound)
	if err := h.engine.SyncInventory(context.Background()); err != nil {
		t.Fatalf("SyncInventory: %v, want a problem rather than a source error", err)
	}
	if st := h.status(); st.ProblemsCommit != "c1" || !strings.Contains(problemsText(t, st.Problems), "environments.yaml") {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestSyncFailureIsRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.firstSync()
	h.addJob(jobPath2)
	h.src.put(jobsRepo, "j1", jobPath2, fixture(t, "jobs/deploy-backend.yaml"))
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	failures := func() int {
		t.Helper()
		n, err := store.CountEvents(context.Background(), h.pool, "sync_failed", time.Time{})
		if err != nil {
			t.Fatalf("CountEvents: %v", err)
		}
		return n
	}
	if n := failures(); n != 0 {
		t.Fatalf("sync_failed events after successful syncs = %d, want 0", n)
	}

	h.src.setErr(errors.New("github: 502 bad gateway"))
	h.tick()
	if err := h.syncAll(); err == nil {
		t.Fatal("SyncAll with a failing source returned no error")
	}
	// One for the inventory and one for each job source (the first sync's job and this one).
	if want := 1 + len(h.jobIDs); failures() != want {
		t.Fatalf("sync_failed events = %d, want %d (the inventory and every job)", failures(), want)
	}

	h.src.setErr(nil)
	h.tick()
	if err := h.syncAll(); err != nil {
		t.Fatalf("SyncAll after recovery: %v", err)
	}
	if want := 1 + len(h.jobIDs); failures() != want {
		t.Fatalf("sync_failed events after recovery = %d, want still %d", failures(), want)
	}
}
