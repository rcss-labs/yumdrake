//go:build integration

package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

var baseTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

type testEnv struct {
	pool  *pgxpool.Pool
	runID int64
	legs  []store.Leg
	steps []store.RunStep
}

// newEnv creates a database with one run that has two steps and n step legs.
// Leg i belongs to step i%2 and service svc-a (i/2 even) or svc-b (i/2 odd).
func newEnv(t *testing.T, n int) testEnv {
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

	user, err := store.CreateLocalUser(ctx, pool, "logs@example.com", "Logs", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	folder, err := store.CreateFolder(ctx, pool, "logs-folder")
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	source, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: folder.ID, Repo: "acme/jobs", Branch: "main", Path: "deploy.yaml"})
	if err != nil {
		t.Fatalf("CreateJobSource: %v", err)
	}
	steps := []store.RunStep{
		{Name: "copy", Use: "copy-config", Position: 0, Needs: []byte(`[]`)},
		{Name: "deploy", Use: "ecs-deploy", Position: 1, Needs: []byte(`[]`)},
	}
	legs := make([]store.Leg, n)
	for i := range legs {
		idx := int64(i % 2)
		legs[i] = store.Leg{Kind: "step", StepID: &idx, Service: "svc-" + string(rune('a'+(i/2)%2)), State: "running"}
	}
	snapshot, err := store.ActivateSnapshot(ctx, pool, "logs-inventory", []byte(`{}`), nil, time.Now())
	if err != nil {
		t.Fatalf("ActivateSnapshot: %v", err)
	}
	run, err := store.InsertRun(ctx, pool, store.Run{
		Kind: "deploy", InventorySnapshotID: snapshot.ID, JobSourceID: source.ID, JobID: "deploy-api", JobSnapshot: []byte(`{}`),
		Environment: "prod", FolderID: folder.ID, Selection: []byte(`{}`), State: "running",
		RequestedBy: user.ID, Via: "web",
	}, steps, legs)
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	gotLegs, err := store.LegsOfRun(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("LegsOfRun: %v", err)
	}
	gotSteps, err := store.StepsOfRun(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("StepsOfRun: %v", err)
	}
	return testEnv{pool: pool, runID: run.ID, legs: gotLegs, steps: gotSteps}
}

func (e testEnv) seed(t *testing.T, lines ...store.LogLine) {
	t.Helper()
	if err := store.InsertLogLines(context.Background(), e.pool, lines); err != nil {
		t.Fatalf("InsertLogLines: %v", err)
	}
}

func (e testEnv) line(legIdx int, seq int64, at time.Time, level, text string) store.LogLine {
	return store.LogLine{RunID: e.runID, LegID: e.legs[legIdx].ID, Seq: seq, At: at, Level: level, Text: text}
}

func (e testEnv) stored(t *testing.T, legID int64) []store.LogLine {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT run_id, leg_id, seq, at, level, text FROM log_lines WHERE run_id = $1 AND leg_id = $2 ORDER BY seq`, e.runID, legID)
	if err != nil {
		t.Fatalf("query stored: %v", err)
	}
	defer rows.Close()
	var out []store.LogLine
	for rows.Next() {
		var l store.LogLine
		if err := rows.Scan(&l.RunID, &l.LegID, &l.Seq, &l.At, &l.Level, &l.Text); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, l)
	}
	return out
}

func seqsOf(lines []store.LogLine) []int64 {
	out := make([]int64, len(lines))
	for i, l := range lines {
		out[i] = l.Seq
	}
	return out
}

// tickingClock returns a clock that advances one second per call.
func tickingClock() func() time.Time {
	var n atomic.Int64
	return func() time.Time { return baseTime.Add(time.Duration(n.Add(1)) * time.Second) }
}

func newTestWriter(e testEnv, maxBatch int, notify func(int64)) *Writer {
	return NewWriter(e.pool, time.Hour, maxBatch, tickingClock(), notify, nil)
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

func TestWriterBatchesAndNumbersPerLeg(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 2)
	var notified atomic.Int64
	var wrongRun atomic.Bool
	w := newTestWriter(e, 3, func(runID int64) {
		if runID != e.runID {
			wrongRun.Store(true)
		}
		notified.Add(1)
	})
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	a, b := e.legs[0].ID, e.legs[1].ID

	for i := 0; i < 7; i++ {
		w.Write(e.runID, a, 0, "info", fmt.Sprintf("a%d", i+1))
		if i < 2 {
			w.Write(e.runID, b, 0, "warn", fmt.Sprintf("b%d", i+1))
		}
	}
	// The flush interval is an hour, so these first lines can only have been
	// stored because a leg reached maxBatch.
	waitFor(t, "full batches to be stored", func() bool { return len(e.stored(t, a)) >= 3 })

	if err := w.Flush(context.Background(), a); err != nil {
		t.Fatalf("Flush a: %v", err)
	}
	if err := w.Flush(context.Background(), b); err != nil {
		t.Fatalf("Flush b: %v", err)
	}
	gotA, gotB := e.stored(t, a), e.stored(t, b)
	if !reflect.DeepEqual(seqsOf(gotA), []int64{1, 2, 3, 4, 5, 6, 7}) || !reflect.DeepEqual(seqsOf(gotB), []int64{1, 2}) {
		t.Fatalf("sequences: a=%v b=%v", seqsOf(gotA), seqsOf(gotB))
	}
	for i, l := range gotA {
		if l.Text != fmt.Sprintf("a%d", i+1) || l.Level != "info" {
			t.Fatalf("leg a line %d = %+v", i, l)
		}
	}
	if gotB[0].Level != "warn" || gotB[1].Text != "b2" {
		t.Fatalf("leg b lines = %+v", gotB)
	}
	// 7 lines with at most 3 per batch need at least 3 batches, plus leg b's.
	if n := notified.Load(); n < 4 || wrongRun.Load() {
		t.Fatalf("notify called %d times (wrong run: %v), want at least 4 for the run", n, wrongRun.Load())
	}
}

func TestWriterContinuesNumberingAfterRestart(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID

	first := newTestWriter(e, 10, nil)
	for i := 1; i <= 3; i++ {
		first.Write(e.runID, leg, 0, "info", fmt.Sprintf("before %d", i))
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := newTestWriter(e, 10, nil)
	second.Write(e.runID, leg, 0, "info", "after 1")
	second.Write(e.runID, leg, 0, "info", "after 2")
	if err := second.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := e.stored(t, leg)
	if !reflect.DeepEqual(seqsOf(got), []int64{1, 2, 3, 4, 5}) || got[3].Text != "after 1" {
		t.Fatalf("stored = %+v", got)
	}
}

func TestDuplicateBatchIsDropped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID
	w := newTestWriter(e, 10, nil)
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	// The first attempt commits but the acknowledgement is lost, so the writer
	// sends the very same batch again.
	var attempts atomic.Int32
	w.afterCommit = func() error {
		if attempts.Add(1) == 1 {
			return errors.New("connection lost after commit")
		}
		return nil
	}
	for i := 1; i <= 3; i++ {
		w.Write(e.runID, leg, 0, "info", fmt.Sprintf("line %d", i))
	}
	if err := w.Flush(context.Background(), leg); err == nil {
		t.Fatal("first Flush: want the injected error")
	}
	if err := w.Flush(context.Background(), leg); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	got := e.stored(t, leg)
	if !reflect.DeepEqual(seqsOf(got), []int64{1, 2, 3}) {
		t.Fatalf("stored sequences = %v, want [1 2 3] once each", seqsOf(got))
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want the batch sent twice", attempts.Load())
	}
}

func TestFailedBatchIsRetriedBeforeLaterOnes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID
	w := newTestWriter(e, 2, nil)
	t.Cleanup(func() { _ = w.Close(context.Background()) })

	var calls atomic.Int32
	w.beforeInsert = func() error {
		if calls.Add(1) == 1 {
			return errors.New("injected failure")
		}
		return nil
	}
	// After every commit the stored sequences must be exactly 1..k: a later
	// batch committed before the failed one would leave a hole.
	var commitMu sync.Mutex
	var commits []int64
	w.afterCommit = func() error {
		var n, highest int64
		if err := e.pool.QueryRow(context.Background(),
			`SELECT count(*), coalesce(max(seq), 0) FROM log_lines WHERE leg_id = $1`, leg).Scan(&n, &highest); err != nil {
			t.Errorf("count after commit: %v", err)
			return nil
		}
		if n != highest {
			t.Errorf("after a commit %d rows are stored with highest seq %d: a batch was committed out of order", n, highest)
		}
		commitMu.Lock()
		commits = append(commits, n)
		commitMu.Unlock()
		return nil
	}
	for i := 1; i <= 5; i++ {
		w.Write(e.runID, leg, 0, "info", fmt.Sprintf("line %d", i))
	}
	// The failure may surface in the background flush; Flush must end with
	// everything stored in order, retrying as needed.
	var err error
	for try := 0; try < 5; try++ {
		if err = w.Flush(context.Background(), leg); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	got := e.stored(t, leg)
	if !reflect.DeepEqual(seqsOf(got), []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("stored sequences = %v, want contiguous 1..5", seqsOf(got))
	}
	commitMu.Lock()
	if len(commits) < 3 {
		t.Errorf("only %d commits observed, want at least 3 batches of at most 2", len(commits))
	}
	commitMu.Unlock()
	for i, l := range got {
		if l.Text != fmt.Sprintf("line %d", i+1) {
			t.Fatalf("line %d has text %q: order was not kept", i+1, l.Text)
		}
	}
}

func TestLongLineIsCut(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID
	w := newTestWriter(e, 10, nil)
	long := strings.Repeat("é", 20000) // 40,000 bytes; must be cut on a character boundary
	w.Write(e.runID, leg, 0, "info", long)
	w.Write(e.runID, leg, 0, "bogus-level", "bad level and a nul \x00 byte")
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := e.stored(t, leg)
	if len(got) != 2 {
		t.Fatalf("stored %d lines, want 2", len(got))
	}
	if len(got[0].Text) > maxLineBytes+len(truncationMarker) || !strings.HasSuffix(got[0].Text, truncationMarker) {
		t.Fatalf("long line: %d bytes, suffix ok = %v", len(got[0].Text), strings.HasSuffix(got[0].Text, truncationMarker))
	}
	if got[1].Level != "info" || strings.ContainsRune(got[1].Text, 0) {
		t.Fatalf("line with bad level/NUL was stored as %+v", got[1])
	}
}

// A runner that lost its lease keeps sending lines. They must not land next to
// the new attempt's lines, and the new attempt continues the numbering.
func TestStaleGenerationLinesAreDropped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	leg := e.legs[0].ID
	w := newTestWriter(e, 10, nil)
	t.Cleanup(func() { _ = w.Close(ctx) })

	w.Write(e.runID, leg, 0, "info", "gen0 first")
	w.Write(e.runID, leg, 0, "info", "gen0 second")
	if err := w.Flush(ctx, leg); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	// The leg is given to another runner: generation 1.
	if _, err := e.pool.Exec(ctx, `UPDATE legs SET generation = 1 WHERE id = $1`, leg); err != nil {
		t.Fatalf("bump generation: %v", err)
	}
	// Both are queued before the flush, so the writer numbers the stale line
	// first and must restart numbering for the new generation.
	w.Write(e.runID, leg, 0, "info", "stale line from the old runner")
	w.Write(e.runID, leg, 1, "info", "gen1 first")
	if err := w.Flush(ctx, leg); err != nil {
		t.Fatalf("Flush: %v (a refused batch is dropped, not an error)", err)
	}
	got := e.stored(t, leg)
	texts := make([]string, len(got))
	for i, l := range got {
		texts[i] = l.Text
	}
	want := []string{"gen0 first", "gen0 second", "gen1 first"}
	if !reflect.DeepEqual(texts, want) || !reflect.DeepEqual(seqsOf(got), []int64{1, 2, 3}) {
		t.Fatalf("stored %v with sequences %v, want %v numbered 1..3", texts, seqsOf(got), want)
	}
}

func TestReadOrdersAcrossLegsAndFilters(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	// legs: 0=step0/svc-a 1=step1/svc-a 2=step0/svc-b 3=step1/svc-b
	e.seed(t,
		e.line(0, 1, baseTime.Add(1*time.Second), "info", "pulling image"),
		e.line(1, 1, baseTime.Add(2*time.Second), "warn", "Slow Start detected"),
		e.line(2, 1, baseTime.Add(2*time.Second), "info", "pulling image again"),
		e.line(0, 2, baseTime.Add(3*time.Second), "error", "boom"),
		e.line(3, 1, baseTime.Add(4*time.Second), "ok", "done"),
	)
	if err := store.AppendRunLine(ctx, e.pool, e.runID, "info", "run started", baseTime); err != nil {
		t.Fatalf("AppendRunLine: %v", err)
	}
	step0, svcB := e.steps[0].ID, "svc-b"
	cases := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"no filter, time then leg then seq (run line first)", Filter{}, []string{"run started", "pulling image", "Slow Start detected", "pulling image again", "boom", "done"}},
		{"by step", Filter{StepID: &step0}, []string{"pulling image", "pulling image again", "boom"}},
		{"by service", Filter{Service: svcB}, []string{"pulling image again", "done"}},
		{"by level", Filter{Level: "warn"}, []string{"Slow Start detected"}},
		{"by text, case-insensitive", Filter{Query: "SLOW start"}, []string{"Slow Start detected"}},
		{"text matches inside words", Filter{Query: "ull"}, []string{"pulling image", "pulling image again"}},
		{"step and service together", Filter{StepID: &step0, Service: svcB}, []string{"pulling image again"}},
		{"nothing matches", Filter{Query: "no such text"}, nil},
	}
	r := NewReader(e.pool, nil)
	for _, tc := range cases {
		page, err := r.Read(ctx, e.runID, tc.filter, "", 100)
		if err != nil {
			t.Fatalf("%s: Read: %v", tc.name, err)
		}
		var got []string
		for _, l := range page.Lines {
			got = append(got, l.Text)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if len(page.Gaps) != 0 {
			t.Errorf("%s: unexpected gaps %v", tc.name, page.Gaps)
		}
	}
}

// Leg B's later lines are read first; leg A's earlier-stamped batch commits
// afterwards. A cursor built on time would never return A's lines.
func TestCursorNeverSkipsLateCommittedLines(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 2)
	ctx := context.Background()
	r := NewReader(e.pool, nil)
	e.seed(t,
		e.line(1, 1, baseTime.Add(10*time.Second), "info", "b1"),
		e.line(1, 2, baseTime.Add(11*time.Second), "info", "b2"),
	)
	first, err := r.Read(ctx, e.runID, Filter{}, "", 100)
	if err != nil || len(first.Lines) != 2 {
		t.Fatalf("first read = %+v, %v", first, err)
	}
	e.seed(t,
		e.line(0, 1, baseTime.Add(1*time.Second), "info", "a1"),
		e.line(0, 2, baseTime.Add(2*time.Second), "info", "a2"),
		e.line(1, 3, baseTime.Add(12*time.Second), "info", "b3"),
	)
	second, err := r.Read(ctx, e.runID, Filter{}, first.Next, 100)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	var got []string
	for _, l := range second.Lines {
		got = append(got, l.Text)
	}
	if !reflect.DeepEqual(got, []string{"a1", "a2", "b3"}) {
		t.Fatalf("second read = %v, want the late lines a1 a2 plus b3, each once", got)
	}
	third, err := r.Read(ctx, e.runID, Filter{}, second.Next, 100)
	if err != nil || len(third.Lines) != 0 || third.Next != second.Next {
		t.Fatalf("third read = %+v, %v; want nothing new and the same cursor", third, err)
	}
}

func TestCursorPagingReturnsEveryLineExactlyOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 5)
	rng := rand.New(rand.NewSource(1))
	var lines []store.LogLine
	next := make([]int64, 5)
	clock := make([]time.Time, 5)
	for len(lines) < 5000 {
		i := rng.Intn(5)
		next[i]++
		clock[i] = clock[i].Add(time.Duration(rng.Intn(3)) * time.Millisecond) // many equal times across legs
		if clock[i].IsZero() {
			clock[i] = baseTime
		}
		lines = append(lines, e.line(i, next[i], clock[i], "info", fmt.Sprintf("leg %d line %d", i, next[i])))
	}
	for start := 0; start < len(lines); start += 1000 {
		e.seed(t, lines[start:start+1000]...)
	}
	r := NewReader(e.pool, nil)
	seen := map[[2]int64]bool{}
	lastSeq := map[int64]int64{}
	var cursor Cursor
	pages := 0
	for {
		page, err := r.Read(context.Background(), e.runID, Filter{}, cursor, 200)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(page.Lines) == 0 {
			break
		}
		pages++
		for _, l := range page.Lines {
			key := [2]int64{l.LegID, l.Seq}
			if seen[key] {
				t.Fatalf("line %v returned twice", key)
			}
			seen[key] = true
			if l.Seq != lastSeq[l.LegID]+1 {
				t.Fatalf("leg %d: got seq %d after %d (skipped or out of order)", l.LegID, l.Seq, lastSeq[l.LegID])
			}
			lastSeq[l.LegID] = l.Seq
		}
		cursor = page.Next
	}
	if len(seen) != 5000 || pages != 25 {
		t.Fatalf("saw %d lines in %d pages, want 5000 in 25", len(seen), pages)
	}
}

// Writers commit batches for several legs in an order unrelated to their
// timestamps while a reader pages; nothing may be skipped or repeated.
func TestCursorUnderConcurrentOutOfOrderCommits(t *testing.T) {
	t.Parallel()
	const legsN, batches, perBatch = 4, 10, 5
	e := newEnv(t, legsN)
	r := NewReader(e.pool, nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	var done atomic.Bool
	for i := 0; i < legsN; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(i)))
			seq := int64(0)
			for b := 0; b < batches; b++ {
				batch := make([]store.LogLine, perBatch)
				for k := range batch {
					seq++
					// Later legs get earlier timestamps, so commit order and time order disagree.
					at := baseTime.Add(time.Duration(seq)*time.Second + time.Duration(legsN-i)*time.Millisecond)
					batch[k] = e.line(i, seq, at, "info", "x")
				}
				if err := store.InsertLogLines(ctx, e.pool, batch); err != nil {
					t.Errorf("InsertLogLines: %v", err)
					return
				}
				time.Sleep(time.Duration(rng.Intn(3)) * time.Millisecond)
			}
		}()
	}
	go func() { wg.Wait(); done.Store(true) }()

	seen := map[[2]int64]int{}
	var cursor Cursor
	for {
		finished := done.Load() // read the flag before reading: a read after it sees everything
		page, err := r.Read(ctx, e.runID, Filter{}, cursor, 7)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, l := range page.Lines {
			seen[[2]int64{l.LegID, l.Seq}]++
		}
		cursor = page.Next
		if finished && len(page.Lines) == 0 {
			break
		}
	}
	if len(seen) != legsN*batches*perBatch {
		t.Fatalf("saw %d distinct lines, want %d", len(seen), legsN*batches*perBatch)
	}
	for key, n := range seen {
		if n != 1 {
			t.Fatalf("line %v returned %d times", key, n)
		}
	}
}

func TestGapIsReported(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	for i := int64(1); i <= 6; i++ {
		e.seed(t, e.line(0, i, baseTime.Add(time.Duration(i)*time.Second), "info", fmt.Sprintf("l%d", i)))
	}
	leg := e.legs[0].ID
	if _, err := e.pool.Exec(ctx, `DELETE FROM log_lines WHERE run_id = $1 AND leg_id = $2 AND seq IN (3, 4)`, e.runID, leg); err != nil {
		t.Fatalf("delete: %v", err)
	}
	r := NewReader(e.pool, nil)
	page, err := r.Read(ctx, e.runID, Filter{}, "", 100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Lines) != 4 || !reflect.DeepEqual(page.Gaps, []Gap{{LegID: leg, FromSeq: 3, ToSeq: 4}}) {
		t.Fatalf("lines=%d gaps=%+v, want 4 lines and the gap 3..4", len(page.Lines), page.Gaps)
	}
	// A filter hides lines but never creates or hides a gap.
	filtered, err := r.Read(ctx, e.runID, Filter{Query: "l6"}, "", 100)
	if err != nil || len(filtered.Lines) != 1 || !reflect.DeepEqual(filtered.Gaps, page.Gaps) {
		t.Fatalf("filtered = %+v, %v", filtered, err)
	}
	// Once reported, the gap is not reported again on the next page.
	again, err := r.Read(ctx, e.runID, Filter{}, page.Next, 100)
	if err != nil || len(again.Gaps) != 0 {
		t.Fatalf("again = %+v, %v", again, err)
	}
}

func TestMalformedCursorIsRejectedByRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	r := NewReader(e.pool, nil)
	if _, err := r.Read(context.Background(), e.runID, Filter{}, "garbage!", 10); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("Read with a malformed cursor: %v, want ErrBadCursor", err)
	}
}

// settle marks every leg as finished long ago and returns an archiver on dir.
func settle(t *testing.T, e testEnv, state string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE legs SET state = $1, finished_at = $2 WHERE run_id = $3`, state, baseTime, e.runID); err != nil {
		t.Fatalf("settle: %v", err)
	}
}

func seedForArchive(t *testing.T, e testEnv) {
	t.Helper()
	for i := 0; i < 2; i++ {
		for s := int64(1); s <= 4; s++ {
			e.seed(t, e.line(i, s, baseTime.Add(time.Duration(s*int64(i+1))*time.Second), "info", fmt.Sprintf("leg %d line %d", i, s)))
		}
	}
	if err := store.AppendRunLine(context.Background(), e.pool, e.runID, "info", "run started", baseTime); err != nil {
		t.Fatalf("AppendRunLine: %v", err)
	}
}

func readEverything(t *testing.T, r *Reader, runID int64) Page {
	t.Helper()
	var all Page
	var cursor Cursor
	for {
		page, err := r.Read(context.Background(), runID, Filter{}, cursor, 3)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(page.Lines) == 0 {
			return all
		}
		all.Lines = append(all.Lines, page.Lines...)
		all.Gaps = append(all.Gaps, page.Gaps...)
		cursor = page.Next
		all.Next = cursor
	}
}

func normalize(lines []Line) []Line {
	out := make([]Line, len(lines))
	for i, l := range lines {
		l.At = l.At.UTC()
		out[i] = l
	}
	return out
}

func TestArchiveThenReadIsIdenticalToBefore(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 3)
	ctx := context.Background()
	seedForArchive(t, e)
	settle(t, e, "succeeded")
	// Leg 2 stays running and unarchived: a read must merge both sources.
	if _, err := e.pool.Exec(ctx, `UPDATE legs SET state = 'running', finished_at = NULL WHERE id = $1`, e.legs[2].ID); err != nil {
		t.Fatalf("unsettle: %v", err)
	}
	e.seed(t, e.line(2, 1, baseTime.Add(500*time.Millisecond), "info", "live line"))

	archive, err := NewDirArchive(t.TempDir(), archiveTestCipher(t, "0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewDirArchive: %v", err)
	}
	r := NewReader(e.pool, archive)
	before := readEverything(t, r, e.runID)
	if len(before.Lines) != 10 {
		t.Fatalf("before: %d lines, want 10", len(before.Lines))
	}
	firstPage, err := r.Read(ctx, e.runID, Filter{}, "", 4) // read before archiving
	if err != nil || len(firstPage.Lines) != 4 {
		t.Fatalf("first page = %+v, %v", firstPage, err)
	}

	now := baseTime.Add(48 * time.Hour)
	ar := NewArchiver(e.pool, archive, time.Hour, func() time.Time { return now }, nil)
	n, err := ar.ArchiveSettledLegs(ctx)
	if err != nil || n != 2 {
		t.Fatalf("ArchiveSettledLegs = %d, %v; want 2 legs", n, err)
	}
	for i := 0; i < 2; i++ {
		if left := e.stored(t, e.legs[i].ID); len(left) != 0 {
			t.Fatalf("leg %d still has %d rows in PostgreSQL", i, len(left))
		}
	}
	if left := e.stored(t, e.legs[2].ID); len(left) != 1 {
		t.Fatalf("running leg lost its row: %v", left)
	}
	var key string
	if err := e.pool.QueryRow(ctx, `SELECT key FROM log_archives WHERE run_id = $1 AND leg_id = $2`, e.runID, e.legs[0].ID).Scan(&key); err != nil {
		t.Fatalf("log_archives row: %v", err)
	}
	if want := fmt.Sprintf("runs/%d/legs/%d.ndjson.gz", e.runID, e.legs[0].ID); key != want {
		t.Fatalf("archive key = %q, want %q", key, want)
	}

	after := readEverything(t, r, e.runID)
	if !reflect.DeepEqual(normalize(before.Lines), normalize(after.Lines)) || len(after.Gaps) != 0 {
		t.Fatalf("read after archiving differs:\nbefore %+v\nafter  %+v", before.Lines, after.Lines)
	}
	// A cursor taken from a page read before archiving keeps working afterwards.
	rest, err := r.Read(ctx, e.runID, Filter{}, firstPage.Next, 100)
	if err != nil {
		t.Fatalf("Read after archiving: %v", err)
	}
	joined := append(append([]Line(nil), firstPage.Lines...), rest.Lines...)
	if !reflect.DeepEqual(normalize(joined), normalize(before.Lines)) {
		t.Fatalf("page before + page after archiving differ from the full read:\n%+v\nvs\n%+v", joined, before.Lines)
	}
	// Filters apply to archived lines too.
	step1 := e.steps[1].ID
	f, err := r.Read(ctx, e.runID, Filter{StepID: &step1}, "", 100)
	if err != nil || len(f.Lines) != 4 || f.Lines[0].LegID != e.legs[1].ID {
		t.Fatalf("filtered read of archived leg = %+v, %v", f.Lines, err)
	}
}

type failingArchive struct{ Archive }

func (failingArchive) Put(context.Context, string, []byte) error { return errors.New("storage down") }

func TestArchiveFailureKeepsRows(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 2)
	ctx := context.Background()
	seedForArchive(t, e)
	settle(t, e, "failed")
	real, err := NewDirArchive(t.TempDir(), archiveTestCipher(t, "0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewDirArchive: %v", err)
	}
	now := baseTime.Add(48 * time.Hour)
	ar := NewArchiver(e.pool, failingArchive{real}, time.Hour, func() time.Time { return now }, nil)
	n, err := ar.ArchiveSettledLegs(ctx)
	if err == nil || n != 0 {
		t.Fatalf("ArchiveSettledLegs = %d, %v; want 0 and an error", n, err)
	}
	for i := 0; i < 2; i++ {
		if got := e.stored(t, e.legs[i].ID); len(got) != 4 {
			t.Fatalf("leg %d has %d rows after a failed Put, want 4", i, len(got))
		}
	}
	var archives int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM log_archives`).Scan(&archives); err != nil || archives != 0 {
		t.Fatalf("log_archives rows = %d, %v; want none", archives, err)
	}
}

type countingArchive struct {
	Archive
	puts atomic.Int32
}

func (c *countingArchive) Put(ctx context.Context, key string, data []byte) error {
	c.puts.Add(1)
	return c.Archive.Put(ctx, key, data)
}

func TestArchiverIsIdempotent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	e.seed(t, e.line(0, 1, baseTime, "info", "x"))
	settle(t, e, "succeeded")
	counting := &countingArchive{Archive: newDirArchiveT(t)}
	now := baseTime.Add(48 * time.Hour)
	ar := NewArchiver(e.pool, counting, time.Hour, func() time.Time { return now }, nil)
	for round, want := range []int{1, 0, 0} {
		n, err := ar.ArchiveSettledLegs(ctx)
		if err != nil || n != want {
			t.Fatalf("round %d: ArchiveSettledLegs = %d, %v; want %d", round, n, err, want)
		}
	}
	if counting.puts.Load() != 1 {
		t.Fatalf("Put called %d times, want 1", counting.puts.Load())
	}
}

func newDirArchiveT(t *testing.T) Archive {
	t.Helper()
	a, err := NewDirArchive(t.TempDir(), archiveTestCipher(t, "0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewDirArchive: %v", err)
	}
	return a
}

func TestArchiverSkipsUnsettledRecentAndUnknownLegs(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		e.seed(t, e.line(i, 1, baseTime, "info", "x"))
	}
	set := func(i int, state string, finished time.Time) {
		if _, err := e.pool.Exec(ctx, `UPDATE legs SET state = $1, finished_at = $2 WHERE id = $3`, state, finished, e.legs[i].ID); err != nil {
			t.Fatalf("set leg: %v", err)
		}
	}
	set(0, "succeeded", baseTime)                   // old and settled: archived
	set(1, "succeeded", baseTime.Add(47*time.Hour)) // settled too recently
	set(2, "running", baseTime)                     // not settled
	set(3, "unknown", baseTime)                     // may still be reconciled and re-queued: kept
	now := baseTime.Add(48 * time.Hour)
	ar := NewArchiver(e.pool, newDirArchiveT(t), 2*time.Hour, func() time.Time { return now }, nil)
	if n, err := ar.ArchiveSettledLegs(ctx); err != nil || n != 1 {
		t.Fatalf("ArchiveSettledLegs = %d, %v; want 1", n, err)
	}
	for i := 1; i < 4; i++ {
		if len(e.stored(t, e.legs[i].ID)) != 1 {
			t.Fatalf("leg %d lost its row", i)
		}
	}
}

// The writer checks "is the leg archived" after it has the leg row locked. An
// archive transaction that holds the lock and commits while the writer waits
// must therefore make the writer's batch refused, not stored next to the
// archive.
func TestBatchRacingAnArchiveCommitIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	leg := e.legs[0].ID
	e.seed(t, e.line(0, 1, baseTime, "info", "archived line"))
	settle(t, e, "succeeded")

	// The "archiver": holds the leg row, records the archive and deletes the rows.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM legs WHERE id = $1 FOR UPDATE`, leg); err != nil {
		t.Fatalf("lock leg: %v", err)
	}

	w := newTestWriter(e, 10, nil)
	t.Cleanup(func() { _ = w.Close(ctx) })
	w.Write(e.runID, leg, 0, "info", "late line")
	flushed := make(chan error, 1)
	go func() { flushed <- w.Flush(ctx, leg) }()
	select {
	case err := <-flushed:
		t.Fatalf("Flush finished (%v) while the leg row was locked", err)
	case <-time.After(300 * time.Millisecond): // the writer is waiting for the lock
	}

	if _, err := tx.Exec(ctx, `INSERT INTO log_archives (run_id, leg_id, key, line_count, first_seq, last_seq, archived_at)
		VALUES ($1, $2, 'k', 1, 1, 1, $3)`, e.runID, leg, baseTime); err != nil {
		t.Fatalf("record archive: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM log_lines WHERE run_id = $1 AND leg_id = $2`, e.runID, leg); err != nil {
		t.Fatalf("delete rows: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-flushed; err != nil {
		t.Fatalf("Flush: %v (a refused batch is dropped, not an error)", err)
	}
	if got := e.stored(t, leg); len(got) != 0 {
		t.Fatalf("a batch raced the archive and was stored: %+v", got)
	}
}

// A runner whose leg is being re-claimed (generation bumped in a transaction
// that has not committed yet) must wait for it and then be refused.
func TestStaleBatchRacingAClaimIsRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	leg := e.legs[0].ID
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE legs SET generation = generation + 1 WHERE id = $1`, leg); err != nil {
		t.Fatalf("claim: %v", err)
	}
	w := newTestWriter(e, 10, nil)
	t.Cleanup(func() { _ = w.Close(ctx) })
	w.Write(e.runID, leg, 0, "info", "line of the old runner")
	flushed := make(chan error, 1)
	go func() { flushed <- w.Flush(ctx, leg) }()
	select {
	case err := <-flushed:
		t.Fatalf("Flush finished (%v) while the claim was uncommitted: the insert did not wait for the leg row", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-flushed; err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := e.stored(t, leg); len(got) != 0 {
		t.Fatalf("a stale batch was stored: %+v", got)
	}
}

// Rows that reappear for a leg that is already archived must never make the
// archiver replace the archive (which would lose the archived lines).
func TestArchiverNeverOverwritesAnArchive(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	leg := e.legs[0].ID
	e.seed(t, e.line(0, 1, baseTime, "info", "one"), e.line(0, 2, baseTime.Add(time.Second), "info", "two"))
	settle(t, e, "succeeded")
	archive := &countingArchive{Archive: newDirArchiveT(t)}
	now := baseTime.Add(48 * time.Hour)
	ar := NewArchiver(e.pool, archive, time.Hour, func() time.Time { return now }, nil)
	if n, err := ar.ArchiveSettledLegs(ctx); err != nil || n != 1 {
		t.Fatalf("first archive = %d, %v", n, err)
	}
	key := archiveKey(e.runID, leg)
	original, err := archive.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	e.seed(t, e.line(0, 1, baseTime, "info", "leftover")) // a row that should not exist
	if n, err := ar.ArchiveSettledLegs(ctx); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v; want nothing archived", n, err)
	}
	if ok, err := ar.archiveLeg(ctx, e.runID, leg); err == nil || ok {
		t.Fatalf("archiveLeg on an archived leg = %v, %v; want a refusal", ok, err)
	}
	after, err := archive.Get(ctx, key)
	if err != nil || string(after) != string(original) || archive.puts.Load() != 1 {
		t.Fatalf("the archive was rewritten (puts=%d, err=%v)", archive.puts.Load(), err)
	}
	// Reads keep returning the archived lines, not the leftover.
	page, err := NewReader(e.pool, archive).Read(ctx, e.runID, Filter{}, "", 100)
	if err != nil || len(page.Lines) != 2 || page.Lines[0].Text != "one" {
		t.Fatalf("read = %+v, %v; want the two archived lines", page.Lines, err)
	}
}

// Two orchestrators archiving at once: the second blocks on the leg row, then
// finds the archive in place and no rows left. That is not an error.
func TestArchivingAnAlreadyArchivedLegWithNoRowsIsAQuietNoOp(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	e.seed(t, e.line(0, 1, baseTime, "info", "one"))
	settle(t, e, "succeeded")
	var logged lockedBuffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	now := baseTime.Add(48 * time.Hour)
	ar := NewArchiver(e.pool, newDirArchiveT(t), time.Hour, func() time.Time { return now }, log)
	if ok, err := ar.archiveLeg(ctx, e.runID, e.legs[0].ID); err != nil || !ok {
		t.Fatalf("first archiveLeg = %v, %v", ok, err)
	}
	// The other orchestrator picked the same leg before the first one committed.
	if ok, err := ar.archiveLeg(ctx, e.runID, e.legs[0].ID); err != nil || ok {
		t.Fatalf("second archiveLeg = %v, %v; want a quiet no-op (false, nil)", ok, err)
	}
	if text := logged.String(); strings.Contains(text, "level=ERROR") || strings.Contains(text, "level=WARN") {
		t.Fatalf("the no-op was logged above debug level: %s", text)
	}
}

// lockedBuffer is a bytes buffer that logging goroutines may write to.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// getCountingArchive counts Get calls per key.
type getCountingArchive struct {
	Archive
	mu   sync.Mutex
	gets map[string]int
}

func (c *getCountingArchive) Get(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	if c.gets == nil {
		c.gets = map[string]int{}
	}
	c.gets[key]++
	c.mu.Unlock()
	return c.Archive.Get(ctx, key)
}

func (c *getCountingArchive) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.gets {
		n += v
	}
	return n
}

// A paged read of archived legs must decompress each archived leg once for the
// whole read sequence, not once per page.
func TestPagedReadFetchesEachArchivedLegOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 3)
	ctx := context.Background()
	const perLeg = 40
	for leg := 0; leg < 3; leg++ {
		for s := int64(1); s <= perLeg; s++ {
			e.seed(t, e.line(leg, s, baseTime.Add(time.Duration(s)*time.Second+time.Duration(leg)*time.Millisecond), "info", fmt.Sprintf("leg %d line %d", leg, s)))
		}
	}
	settle(t, e, "succeeded")
	archive := &getCountingArchive{Archive: newDirArchiveT(t)}
	now := baseTime.Add(48 * time.Hour)
	if n, err := NewArchiver(e.pool, archive, time.Hour, func() time.Time { return now }, nil).ArchiveSettledLegs(ctx); err != nil || n != 3 {
		t.Fatalf("ArchiveSettledLegs = %d, %v; want 3", n, err)
	}

	reader := NewReader(e.pool, archive)
	session := reader.NewSession(e.runID)
	var got []Line
	var cursor Cursor
	pages := 0
	for {
		page, err := session.Read(ctx, Filter{}, cursor, 7)
		if err != nil {
			t.Fatalf("session Read: %v", err)
		}
		if len(page.Lines) == 0 {
			break
		}
		pages++
		got = append(got, page.Lines...)
		cursor = page.Next
	}
	if len(got) != 3*perLeg {
		t.Fatalf("read %d lines, want %d", len(got), 3*perLeg)
	}
	if pages < 10 {
		t.Fatalf("only %d pages: the test needs many pages to show the effect", pages)
	}
	if n := archive.total(); n != 3 {
		t.Fatalf("archive Get called %d times over %d pages, want 3 (once per archived leg)", n, pages)
	}
	// The same sequence through the stateless Read is the reference.
	var want []Line
	cursor = ""
	for {
		page, err := reader.Read(ctx, e.runID, Filter{}, cursor, 7)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(page.Lines) == 0 {
			break
		}
		want = append(want, page.Lines...)
		cursor = page.Next
	}
	if !reflect.DeepEqual(normalize(got), normalize(want)) {
		t.Fatalf("session read differs from stateless read")
	}
}

// With PostgreSQL unreachable the writer must not grow without bound: once a
// leg has maxQueued lines waiting, Write blocks briefly and then refuses.
func TestWriteRefusesWhenTheLegsQueueIsFullAndTheDatabaseStaysDown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID
	w := newTestWriter(e, 2, nil).WithQueueLimit(5, 150*time.Millisecond)
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	w.beforeInsert = func() error { return errors.New("database unreachable") }

	var refused int
	var slowest time.Duration
	for i := 1; i <= 20; i++ {
		started := time.Now()
		err := w.Write(e.runID, leg, 0, "info", fmt.Sprintf("line %d", i))
		if d := time.Since(started); d > slowest {
			slowest = d
		}
		if err != nil {
			if !errors.Is(err, ErrBufferFull) {
				t.Fatalf("Write error = %v, want ErrBufferFull", err)
			}
			refused++
		}
	}
	if refused == 0 {
		t.Fatalf("no Write was refused although 20 lines went to a queue limited to 5")
	}
	if slowest < 100*time.Millisecond {
		t.Errorf("slowest Write took %v: a full queue should block the executor briefly first", slowest)
	}
	w.mu.Lock()
	queued := len(w.legs[leg].queue)
	w.mu.Unlock()
	if queued > 5 {
		t.Fatalf("%d lines queued, want at most the limit of 5", queued)
	}
}

// A queue that is full only for a moment (the database comes back) does not
// lose the line: Write waits for room.
func TestWriteWaitsForRoomWhenTheDatabaseComesBack(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID
	w := newTestWriter(e, 2, nil).WithQueueLimit(4, 5*time.Second)
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	var down atomic.Bool
	down.Store(true)
	w.beforeInsert = func() error {
		if down.Load() {
			return errors.New("database unreachable")
		}
		return nil
	}
	time.AfterFunc(200*time.Millisecond, func() { down.Store(false) })
	for i := 1; i <= 12; i++ {
		if err := w.Write(e.runID, leg, 0, "info", fmt.Sprintf("line %d", i)); err != nil {
			t.Fatalf("Write %d: %v, want it to wait for the database", i, err)
		}
	}
	if err := w.Flush(context.Background(), leg); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(e.stored(t, leg)); got != 12 {
		t.Fatalf("%d lines stored, want 12", got)
	}
}

// After a leg's first refusal, later Writes for it fail at once instead of
// each making the executor wait the full time again, until the queue drains.
func TestWriteRefusesAtOnceAfterTheFirstRefusalUntilTheQueueDrains(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID
	w := newTestWriter(e, 2, nil).WithQueueLimit(3, 400*time.Millisecond)
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	var down atomic.Bool
	down.Store(true)
	w.beforeInsert = func() error {
		if down.Load() {
			return errors.New("database unreachable")
		}
		return nil
	}
	var first error
	for i := 1; i <= 10 && first == nil; i++ {
		first = w.Write(e.runID, leg, 0, "info", fmt.Sprintf("line %d", i))
	}
	if !errors.Is(first, ErrBufferFull) {
		t.Fatalf("no Write was refused, got %v", first)
	}
	for i := 0; i < 5; i++ {
		started := time.Now()
		err := w.Write(e.runID, leg, 0, "info", "after the refusal")
		if !errors.Is(err, ErrBufferFull) {
			t.Fatalf("Write after the refusal = %v, want ErrBufferFull", err)
		}
		if d := time.Since(started); d > 100*time.Millisecond {
			t.Fatalf("Write after the refusal took %v, want it to fail at once", d)
		}
	}
	down.Store(false)
	if err := w.Flush(context.Background(), leg); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := w.Write(e.runID, leg, 0, "info", "after the queue drained"); err != nil {
		t.Fatalf("Write after the queue drained = %v, want it accepted", err)
	}
}

// Legs that ran one after the other must not all sit decoded in the session:
// a leg is decoded when the page needs it and dropped once the cursor is past
// it, so memory is about one leg plus one page. Each leg is still fetched once.
func TestSessionHoldsAtMostTheLegBeingRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 4)
	ctx := context.Background()
	const perLeg = 40
	for leg := 0; leg < 4; leg++ {
		for s := int64(1); s <= perLeg; s++ {
			at := baseTime.Add(time.Duration(leg)*time.Hour + time.Duration(s)*time.Second)
			e.seed(t, e.line(leg, s, at, "info", fmt.Sprintf("leg %d line %d", leg, s)))
		}
	}
	settle(t, e, "succeeded")
	archive := &getCountingArchive{Archive: newDirArchiveT(t)}
	now := baseTime.Add(48 * time.Hour)
	if n, err := NewArchiver(e.pool, archive, time.Hour, func() time.Time { return now }, nil).ArchiveSettledLegs(ctx); err != nil || n != 4 {
		t.Fatalf("ArchiveSettledLegs = %d, %v; want 4", n, err)
	}
	session := NewReader(e.pool, archive).NewSession(e.runID)
	var cursor Cursor
	total, held := 0, 0
	for {
		page, err := session.Read(ctx, Filter{}, cursor, 7)
		if err != nil {
			t.Fatalf("session Read: %v", err)
		}
		if len(page.Lines) == 0 {
			break
		}
		total += len(page.Lines)
		cursor = page.Next
		if n := len(session.cache); n > held {
			held = n
		}
	}
	if total != 4*perLeg {
		t.Fatalf("read %d lines, want %d", total, 4*perLeg)
	}
	if held > 1 {
		t.Fatalf("the session held %d decoded legs at once, want at most 1 for legs that ran one after another", held)
	}
	if n := archive.total(); n != 4 {
		t.Fatalf("archive Get called %d times, want 4 (once per archived leg)", n)
	}
}

// explainingQuerier runs every statement under EXPLAIN ANALYZE first and adds
// the rows its log_lines scans produced to scanned, then runs it for real.
type explainingQuerier struct {
	pool    *pgxpool.Pool
	scanned *int64
}

func (q explainingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	var raw []byte
	if err := q.pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		return nil, err
	}
	var plans []struct{ Plan map[string]any }
	if err := json.Unmarshal(raw, &plans); err != nil {
		return nil, err
	}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		if rel, _ := node["Relation Name"].(string); rel == "log_lines" {
			rows, _ := node["Actual Rows"].(float64)
			loops, _ := node["Actual Loops"].(float64)
			*q.scanned += int64(rows * loops)
		}
		kids, _ := node["Plans"].([]any)
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				walk(m)
			}
		}
	}
	for _, p := range plans {
		walk(p.Plan)
	}
	return q.pool.Query(ctx, sql, args...)
}

// The unfiltered read of a page must look at about one page of rows per leg,
// not every row after the cursor: that is what keeps a paged read of N lines
// linear. It fails if the per-leg LIMIT is replaced by a rescan of the rest.
func TestUnfilteredPageReadsOnlyAPageOfRowsPerLeg(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	ctx := context.Background()
	const total, limit = 6000, 10
	if _, err := e.pool.Exec(ctx, `INSERT INTO log_lines (run_id, leg_id, seq, at, level, text)
		SELECT $1, $2, g, $3::timestamptz + g * interval '1 second', 'info', 'line ' || g FROM generate_series(1, $4) g`,
		e.runID, e.legs[0].ID, baseTime, total); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `ANALYZE log_lines`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	reader := NewReader(e.pool, nil)
	for _, cursor := range []map[int64]int64{nil, {e.legs[0].ID: 100}} {
		var scanned int64
		cands, err := reader.queryRows(ctx, explainingQuerier{pool: e.pool, scanned: &scanned}, e.runID, Filter{}, cursor, limit, nil)
		if err != nil {
			t.Fatalf("queryRows: %v", err)
		}
		if len(cands) != limit {
			t.Fatalf("got %d rows, want %d", len(cands), limit)
		}
		if scanned > 10*limit {
			t.Fatalf("the query scanned %d log_lines rows for a page of %d (cursor %v); it must stop after the page", scanned, limit, cursor)
		}
	}
}

func TestLogWriterRecordsPersistFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t, 1)
	leg := e.legs[0].ID
	w := newTestWriter(e, 10, nil)
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	failing := atomic.Bool{}
	failing.Store(true)
	w.beforeInsert = func() error {
		if failing.Load() {
			return errors.New("injected failure")
		}
		return nil
	}
	events := func() int {
		t.Helper()
		n, err := store.CountEvents(context.Background(), e.pool, "log_persist_failed", time.Time{})
		if err != nil {
			t.Fatalf("CountEvents: %v", err)
		}
		return n
	}

	w.Write(e.runID, leg, 0, "info", "line 1")
	// The writer retries a batch; one or two failures are a blip, not a lost log.
	for attempt := 1; attempt < persistFailureThreshold; attempt++ {
		if err := w.Flush(context.Background(), leg); err == nil {
			t.Fatalf("Flush %d: want the injected error", attempt)
		}
		if n := events(); n != 0 {
			t.Fatalf("after %d failed attempts there are %d events, want none yet", attempt, n)
		}
	}
	// The batch keeps failing: that is one event, however long it goes on.
	for range 3 {
		if err := w.Flush(context.Background(), leg); err == nil {
			t.Fatal("Flush: want the injected error")
		}
	}
	if n := events(); n != 1 {
		t.Fatalf("after the batch kept failing there are %d events, want exactly 1", n)
	}

	// Recovery stores the batch and records nothing more.
	failing.Store(false)
	if err := w.Flush(context.Background(), leg); err != nil {
		t.Fatalf("Flush after recovery: %v", err)
	}
	if n := events(); n != 1 {
		t.Fatalf("after recovery there are %d events, want still 1", n)
	}
}
