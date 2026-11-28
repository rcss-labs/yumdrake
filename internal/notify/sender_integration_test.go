//go:build integration

package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/secrets"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

const senderToken = "xoxb-sender-fake-token" //nolint:gosec // a fake token for tests

func newTestPool(t *testing.T) *pgxpool.Pool {
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

// runMaker inserts runs, which notifications belong to.
type runMaker struct {
	pool                       *pgxpool.Pool
	userID, folderID, sourceID int64
	snapshotID                 int64
	count                      int
}

func newRunMaker(t *testing.T, pool *pgxpool.Pool) *runMaker {
	t.Helper()
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	user, err := store.CreateLocalUser(ctx, pool, fmt.Sprintf("sender-%d@example.com", stamp), "Requester", "local", "")
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	folder, err := store.CreateFolder(ctx, pool, fmt.Sprintf("folder-%d", stamp))
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	source, err := store.CreateJobSource(ctx, pool, store.JobSource{FolderID: folder.ID, Repo: "acme/jobs", Branch: "main", Path: "deploy.yaml"})
	if err != nil {
		t.Fatalf("CreateJobSource: %v", err)
	}
	snapshot, err := store.ActivateSnapshot(ctx, pool, fmt.Sprintf("inv-%d", stamp), []byte(`{}`), nil, time.Now())
	if err != nil {
		t.Fatalf("ActivateSnapshot: %v", err)
	}
	return &runMaker{pool: pool, userID: user.ID, folderID: folder.ID, sourceID: source.ID, snapshotID: snapshot.ID}
}

func (m *runMaker) newRun(t *testing.T) int64 {
	t.Helper()
	run, err := store.InsertRun(context.Background(), m.pool, store.Run{
		Kind: "deploy", JobSourceID: m.sourceID, JobID: "deploy-api",
		JobSnapshot: []byte(`{"id":"deploy-api"}`), InventoryCommit: "inv1", JobCommit: "job1", InventorySnapshotID: m.snapshotID,
		LockResources: []string{"arn:svc/a"}, Environment: "prod-eu", Label: "prod", AWSAccount: "111122223333", FolderID: m.folderID,
		Selection: []byte(`{"services":["a"]}`), State: "running", RequestedBy: m.userID, Via: "web",
	}, nil, nil)
	if err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	return run.ID
}

// enqueue adds a message for runID with the given kind; its text is "<kind>@<run>".
func enqueue(t *testing.T, pool *pgxpool.Pool, runID int64, kind string, at time.Time) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"text": fmt.Sprintf("%s@%d", kind, runID)})
	if err := store.EnqueueNotification(context.Background(), pool, store.Notification{
		RunID: runID, Kind: kind, Channel: "#deploys", Payload: payload, NextAttemptAt: at,
	}); err != nil {
		t.Fatalf("EnqueueNotification: %v", err)
	}
}

type notificationRow struct {
	ID            int64
	State         string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	Claimed       bool
}

func rowsOfRun(t *testing.T, pool *pgxpool.Pool, runID int64) []notificationRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id, state, attempts, next_attempt_at, last_error, claimed_until IS NOT NULL FROM notifications WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		t.Fatalf("query notifications: %v", err)
	}
	defer rows.Close()
	var out []notificationRow
	for rows.Next() {
		var r notificationRow
		if err := rows.Scan(&r.ID, &r.State, &r.Attempts, &r.NextAttemptAt, &r.LastError, &r.Claimed); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

type post struct{ Token, Channel, Text string }

// slackStub answers chat.postMessage as respond says and records every post.
type slackStub struct {
	srv *httptest.Server

	mu      sync.Mutex
	posts   []post
	respond func(p post) (status int, retryAfter, body string)
}

func newSlackStub(t *testing.T) *slackStub {
	t.Helper()
	f := &slackStub{respond: func(post) (int, string, string) { return 200, "", `{"ok":true}` }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct{ Channel, Text string }
		_ = json.Unmarshal(raw, &body)
		p := post{Token: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), Channel: body.Channel, Text: body.Text}
		f.mu.Lock()
		f.posts = append(f.posts, p)
		respond := f.respond
		f.mu.Unlock()
		status, retryAfter, reply := respond(p)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *slackStub) set(respond func(post) (int, string, string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *slackStub) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, p := range f.posts {
		out = append(out, p.Text)
	}
	return out
}

func (f *slackStub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Now().UTC().Truncate(time.Microsecond)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *testClock) advance(d time.Duration) { c.set(c.now().Add(d)) }

type senderFixture struct {
	pool  *pgxpool.Pool
	runs  *runMaker
	slack *slackStub
	clock *testClock
	token string // what the token source returns; "" means no connection
	tmu   sync.Mutex
	send  *Sender
}

func newSenderFixture(t *testing.T) *senderFixture {
	t.Helper()
	pool := newTestPool(t)
	f := &senderFixture{pool: pool, runs: newRunMaker(t, pool), slack: newSlackStub(t), clock: newTestClock(), token: senderToken}
	f.send = NewSender(pool, NewSlack(f.slack.srv.URL, nil), func(context.Context) (string, error) {
		f.tmu.Lock()
		defer f.tmu.Unlock()
		return f.token, nil
	}, f.clock.now, nil)
	return f
}

func (f *senderFixture) setToken(token string) {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	f.token = token
}

func (f *senderFixture) deliver(t *testing.T) (sent, retried, failed int) {
	t.Helper()
	sent, retried, failed, err := f.send.DeliverDue(context.Background())
	if err != nil {
		t.Fatalf("DeliverDue: %v", err)
	}
	return sent, retried, failed
}

func TestDeliverSendsInOrderPerRun(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	runA, runB := f.runs.newRun(t), f.runs.newRun(t)
	now := f.clock.now()
	for _, kind := range []string{"approval_requested", "approved", "started", "finished"} {
		enqueue(t, f.pool, runA, kind, now)
		enqueue(t, f.pool, runB, kind, now)
	}

	if sent, retried, failed := f.deliver(t); sent != 8 || retried != 0 || failed != 0 {
		t.Fatalf("DeliverDue = %d sent, %d retried, %d failed; want 8, 0, 0", sent, retried, failed)
	}

	perRun := map[string][]string{}
	for _, text := range f.slack.texts() {
		_, run, _ := strings.Cut(text, "@")
		perRun[run] = append(perRun[run], text)
	}
	for _, run := range []int64{runA, runB} {
		want := []string{}
		for _, kind := range []string{"approval_requested", "approved", "started", "finished"} {
			want = append(want, fmt.Sprintf("%s@%d", kind, run))
		}
		if got := strings.Join(perRun[fmt.Sprint(run)], ","); got != strings.Join(want, ",") {
			t.Errorf("run %d got %s, want %s", run, got, strings.Join(want, ","))
		}
		for _, row := range rowsOfRun(t, f.pool, run) {
			if row.State != "sent" || row.Claimed {
				t.Errorf("run %d row %+v, want sent and unclaimed", run, row)
			}
		}
	}
	for _, p := range f.slack.posts {
		if p.Token != senderToken || p.Channel != "#deploys" {
			t.Errorf("post %+v, want the token and #deploys", p)
		}
	}
}

func TestTransientErrorRetriesWithBackoffThenFails(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	enqueue(t, f.pool, run, "finished", f.clock.now())
	// The server's reply echoes the token: it must not reach last_error.
	f.slack.set(func(post) (int, string, string) { return 500, "", "upstream broke " + senderToken })

	wantDelay := []time.Duration{1, 2, 4, 8, 15, 15, 15, 15, 15, 15, 15}
	for attempt := 1; attempt <= 12; attempt++ {
		before := f.clock.now()
		sent, retried, failed := f.deliver(t)
		row := rowsOfRun(t, f.pool, run)[0]
		if attempt < 12 {
			if sent != 0 || retried != 1 || failed != 0 || row.State != "pending" || row.Attempts != attempt {
				t.Fatalf("attempt %d: %d/%d/%d row %+v, want one retry and pending", attempt, sent, retried, failed, row)
			}
			if want := before.Add(wantDelay[attempt-1] * time.Minute); !row.NextAttemptAt.Equal(want) {
				t.Fatalf("attempt %d: next attempt %v, want %v", attempt, row.NextAttemptAt, want)
			}
			// Not due before then: nothing is posted.
			posts := f.slack.count()
			f.clock.advance(wantDelay[attempt-1]*time.Minute - time.Second)
			f.deliver(t)
			if f.slack.count() != posts {
				t.Fatalf("attempt %d: a post was made before the backoff ended", attempt)
			}
			f.clock.advance(time.Second)
			continue
		}
		if failed != 1 || row.State != "failed" || row.Attempts != 12 {
			t.Fatalf("attempt 12: %d/%d/%d row %+v, want failed after 12 attempts", sent, retried, failed, row)
		}
	}
	rows := rowsOfRun(t, f.pool, run)
	if rows[0].LastError != "http_5xx" || strings.Contains(rows[0].LastError, senderToken) {
		t.Errorf("last_error = %q, want the class http_5xx and no token", rows[0].LastError)
	}
	// A failed message no longer holds back the run's next one: it was tried in
	// the same round (and failed once while Slack was still down) ...
	if second := rowsOfRun(t, f.pool, run)[1]; second.State != "pending" || second.Attempts != 1 {
		t.Errorf("second message %+v, want it tried once after the first was given up on", second)
	}
	// ... and goes out once Slack is back.
	f.slack.set(func(post) (int, string, string) { return 200, "", `{"ok":true}` })
	f.clock.advance(time.Minute)
	if sent, _, _ := f.deliver(t); sent != 1 {
		t.Errorf("after Slack came back, sent = %d, want the second message to go out", sent)
	}
}

func TestPermanentErrorFailsAtOnceWithCode(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	enqueue(t, f.pool, run, "finished", f.clock.now())
	f.slack.set(func(p post) (int, string, string) {
		if p.Text == fmt.Sprintf("started@%d", run) {
			return 200, "", `{"ok":false,"error":"channel_not_found"}`
		}
		return 200, "", `{"ok":true}`
	})

	sent, retried, failed := f.deliver(t)
	rows := rowsOfRun(t, f.pool, run)
	if failed != 1 || retried != 0 || rows[0].State != "failed" || rows[0].Attempts != 1 || rows[0].LastError != "channel_not_found" {
		t.Fatalf("first message: %d/%d/%d row %+v, want failed at once with channel_not_found", sent, retried, failed, rows[0])
	}
	// The failure frees the run's next message.
	if rows[1].State != "sent" || sent != 1 {
		t.Errorf("second message: %+v (sent %d), want it sent in the same round", rows[1], sent)
	}
}

func TestSlackDownThenUpDeliversEverythingInOrder(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	runs := []int64{f.runs.newRun(t), f.runs.newRun(t)}
	kinds := []string{"approval_requested", "approved", "started", "finished"}
	for _, kind := range kinds {
		for _, run := range runs {
			enqueue(t, f.pool, run, kind, f.clock.now())
		}
	}
	down := true
	f.slack.set(func(post) (int, string, string) {
		if down {
			return 503, "", "unavailable"
		}
		return 200, "", `{"ok":true}`
	})

	// Slack is down for ten minutes of rounds: nothing is delivered, nothing is lost.
	for range 10 {
		f.deliver(t)
		f.clock.advance(time.Minute)
	}
	for _, run := range runs {
		for _, row := range rowsOfRun(t, f.pool, run) {
			if row.State == "sent" || row.State == "failed" {
				t.Fatalf("run %d row %+v while Slack was down, want pending", run, row)
			}
		}
	}
	for _, text := range f.slack.texts() {
		_, run, _ := strings.Cut(text, "@")
		if !strings.HasPrefix(text, "approval_requested@") {
			t.Fatalf("posted %q while the first message of run %s was pending: order broken", text, run)
		}
	}

	down = false
	f.slack.mu.Lock()
	f.slack.posts = nil
	f.slack.mu.Unlock()
	for range 20 {
		f.deliver(t)
		f.clock.advance(time.Minute)
	}
	perRun := map[string][]string{}
	for _, text := range f.slack.texts() {
		_, run, _ := strings.Cut(text, "@")
		perRun[run] = append(perRun[run], strings.TrimSuffix(text, "@"+run))
	}
	for _, run := range runs {
		if got := strings.Join(perRun[fmt.Sprint(run)], ","); got != strings.Join(kinds, ",") {
			t.Errorf("run %d delivered %s, want each message once in order %s", run, got, strings.Join(kinds, ","))
		}
		for _, row := range rowsOfRun(t, f.pool, run) {
			if row.State != "sent" {
				t.Errorf("run %d row %+v, want sent", run, row)
			}
		}
	}
}

func TestNoConnectionKeepsPending(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	f.setToken("")

	before := f.clock.now()
	if sent, retried, failed := f.deliver(t); sent+retried+failed != 0 {
		t.Fatalf("DeliverDue = %d/%d/%d with no connection, want all zero", sent, retried, failed)
	}
	row := rowsOfRun(t, f.pool, run)[0]
	if row.State != "pending" || row.Attempts != 0 || row.LastError != "" || !row.NextAttemptAt.Equal(before.Add(15*time.Minute)) || row.Claimed {
		t.Fatalf("row %+v, want pending, no attempt counted, next in 15 minutes, claim released", row)
	}
	if f.slack.count() != 0 {
		t.Fatalf("%d posts without a token", f.slack.count())
	}

	// Many rounds with no connection never use up the attempts.
	for range 20 {
		f.clock.advance(15 * time.Minute)
		f.deliver(t)
	}
	if row := rowsOfRun(t, f.pool, run)[0]; row.State != "pending" || row.Attempts != 0 {
		t.Fatalf("after 20 rounds: %+v, want still pending with 0 attempts", row)
	}

	// Connecting delivers it at the next round.
	f.setToken(senderToken)
	f.clock.advance(15 * time.Minute)
	if sent, _, _ := f.deliver(t); sent != 1 {
		t.Fatalf("after connecting, sent = %d, want 1", sent)
	}
}

func TestShutdownDuringAPostNeitherCountsNorFails(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	inFlight := make(chan struct{})
	release := make(chan struct{})
	f.slack.set(func(post) (int, string, string) {
		close(inFlight)
		<-release
		return 200, "", `{"ok":true}`
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := f.send.DeliverDue(ctx)
		done <- err
	}()
	<-inFlight
	cancel()
	if err := <-done; err == nil {
		t.Error("DeliverDue after shutdown returned no error")
	}

	row := rowsOfRun(t, f.pool, run)[0]
	if row.State != "pending" || row.Attempts != 0 || row.LastError != "" || row.Claimed {
		t.Fatalf("row %+v, want pending, 0 attempts, no error and the claim released", row)
	}
	// And it is due at once for the next sender.
	f.slack.set(func(post) (int, string, string) { return 200, "", `{"ok":true}` })
	if sent, _, _ := f.deliver(t); sent != 1 {
		t.Errorf("after restart sent = %d, want 1", sent)
	}
}

func TestRowWhoseLeaseRanOutIsNotPosted(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run1, run2 := f.runs.newRun(t), f.runs.newRun(t)
	enqueue(t, f.pool, run1, "started", f.clock.now())
	enqueue(t, f.pool, run2, "started", f.clock.now())
	row2 := rowsOfRun(t, f.pool, run2)[0].ID

	// On its first post this sender stalls past its two-minute lease. A second
	// sender then claims the rows, including run 2's, and delivers run 2's row.
	stalled := false
	f.slack.set(func(post) (int, string, string) {
		if !stalled {
			stalled = true
			f.clock.advance(2*time.Minute + time.Second)
			claimed, err := store.ClaimDueNotifications(context.Background(), f.pool, f.clock.now(), 10)
			if err != nil {
				t.Errorf("second sender's claim: %v", err)
			}
			found := false
			for _, n := range claimed {
				found = found || n.ID == row2
			}
			if !found {
				t.Errorf("second sender's claim = %+v, want run 2's row %d", claimed, row2)
			}
			if err := store.MarkNotificationSent(context.Background(), f.pool, row2, f.clock.now()); err != nil {
				t.Errorf("second sender's MarkNotificationSent: %v", err)
			}
		}
		return 200, "", `{"ok":true}`
	})

	f.deliver(t)
	if got := f.slack.texts(); len(got) != 1 || got[0] != fmt.Sprintf("started@%d", run1) {
		t.Fatalf("posts = %v, want exactly run 1's started message", got)
	}
}

func TestShutdownAfterASuccessfulPostKeepsItSent(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())

	ctx, cancel := context.WithCancel(context.Background())
	var posts int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		posts++
		// Shutdown begins while the post is in flight; Slack still answers ok.
		cancel()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		}, nil
	})}
	send := NewSender(f.pool, NewSlack("http://slack.invalid", client), func(context.Context) (string, error) {
		return senderToken, nil
	}, f.clock.now, nil)
	_, _, _, _ = send.DeliverDue(ctx)

	if row := rowsOfRun(t, f.pool, run)[0]; row.State != "sent" {
		t.Fatalf("row %+v after a successful post and shutdown, want sent", row)
	}
	if _, _, _, err := send.DeliverDue(context.Background()); err != nil {
		t.Fatalf("second DeliverDue: %v", err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1: the sent message must not be posted again", posts)
	}
}

func TestStaleLeaseCannotOverwriteARow(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	id := rowsOfRun(t, f.pool, run)[0].ID
	// While this sender's post is in flight, its lease runs out and another
	// sender delivers the same message. This sender's failure must then change nothing.
	f.slack.set(func(post) (int, string, string) {
		if err := store.MarkNotificationSent(context.Background(), f.pool, id, f.clock.now()); err != nil {
			t.Errorf("other sender's MarkNotificationSent: %v", err)
		}
		return 500, "", "boom"
	})

	f.deliver(t)
	row := rowsOfRun(t, f.pool, run)[0]
	if row.State != "sent" || row.Attempts != 0 || row.LastError != "" {
		t.Fatalf("row %+v, want the other sender's result (sent, 0 attempts) kept", row)
	}

	// The same holds when the stale sender succeeds after the row was given up on.
	run2 := f.runs.newRun(t)
	enqueue(t, f.pool, run2, "started", f.clock.now())
	id2 := rowsOfRun(t, f.pool, run2)[0].ID
	f.slack.set(func(post) (int, string, string) {
		if err := store.MarkNotificationRetry(context.Background(), f.pool, id2, "gave_up", f.clock.now(), true); err != nil {
			t.Errorf("other sender's MarkNotificationRetry: %v", err)
		}
		return 200, "", `{"ok":true}`
	})
	f.deliver(t)
	if row := rowsOfRun(t, f.pool, run2)[0]; row.State != "failed" || row.LastError != "gave_up" {
		t.Fatalf("row %+v, want failed as the other sender left it", row)
	}
}

func TestBadPayloadFailsAtOnce(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	if err := store.EnqueueNotification(context.Background(), f.pool, store.Notification{
		RunID: run, Kind: "started", Channel: "#deploys", Payload: []byte(`not json`), NextAttemptAt: f.clock.now(),
	}); err != nil {
		t.Fatal(err)
	}
	f.deliver(t)
	if row := rowsOfRun(t, f.pool, run)[0]; row.State != "failed" || row.LastError != "bad_payload" || f.slack.count() != 0 {
		t.Fatalf("row %+v with %d posts, want failed as bad_payload and nothing posted", row, f.slack.count())
	}
}

func TestRunLoopDeliversUntilStopped(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		f.send.Run(ctx, 10*time.Millisecond)
		close(stopped)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for rowsOfRun(t, f.pool, run)[0].State != "sent" {
		if time.Now().After(deadline) {
			t.Fatal("the run loop did not deliver the message")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestSlackTokenSourceDecryptsWhatWasSaved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newTestPool(t)
	cipher, err := secrets.NewLocal([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	source := SlackTokenSource(pool, cipher)
	if token, err := source(ctx); err != nil || token != "" {
		t.Fatalf("with nothing saved: %q, %v; want \"\" and no error", token, err)
	}
	status, err := LoadSlackStatus(ctx, pool)
	if err != nil || status.Configured {
		t.Fatalf("status with nothing saved: %+v, %v", status, err)
	}

	runs := newRunMaker(t, pool)
	at := time.Now().UTC().Truncate(time.Microsecond)
	if err := SaveSlackToken(ctx, pool, cipher, senderToken, runs.userID, at); err != nil {
		t.Fatalf("SaveSlackToken: %v", err)
	}
	if token, err := source(ctx); err != nil || token != senderToken {
		t.Fatalf("after saving: token matches = %v, err %v; want the saved token", token == senderToken, err)
	}
	status, err = LoadSlackStatus(ctx, pool)
	if err != nil || !status.Configured || status.SetBy == nil || *status.SetBy != runs.userID || status.SetAt == nil || !status.SetAt.Equal(at) {
		t.Fatalf("status = %+v, %v; want configured by the user at %v", status, err, at)
	}
	var stored string
	if err := pool.QueryRow(ctx, "SELECT value::text FROM instance_settings WHERE key = 'slack'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, senderToken) {
		t.Error("the stored document holds the token in the clear")
	}
}

// saveStoredToken stores token the way PUT /admin/slack does, so the sender
// can record that it was refused.
func (f *senderFixture) saveStoredToken(t *testing.T, token string) {
	t.Helper()
	cipher, err := secrets.NewLocal([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSlackToken(context.Background(), f.pool, cipher, token, f.runs.userID, f.clock.now()); err != nil {
		t.Fatalf("SaveSlackToken: %v", err)
	}
	f.setToken(token)
}

func TestRateLimitStopsTheRoundAndWaitsRetryAfter(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	var runs []int64
	for range 20 {
		run := f.runs.newRun(t)
		runs = append(runs, run)
		enqueue(t, f.pool, run, "started", f.clock.now())
	}
	f.slack.set(func(post) (int, string, string) { return 429, "30", `{"ok":false,"error":"ratelimited"}` })

	f.deliver(t)
	if got := f.slack.count(); got != 1 {
		t.Fatalf("%d posts in the round that was rate limited, want exactly 1", got)
	}
	for _, run := range runs {
		if row := rowsOfRun(t, f.pool, run)[0]; row.State != "pending" || row.Attempts != 0 || row.Claimed {
			t.Fatalf("run %d row %+v, want pending, no attempt used, not claimed", run, row)
		}
	}

	f.slack.set(func(post) (int, string, string) { return 200, "", `{"ok":true}` })
	f.clock.advance(29 * time.Second)
	f.deliver(t)
	if got := f.slack.count(); got != 1 {
		t.Fatalf("%d posts 29s after the 429, want none before Retry-After ends", got)
	}
	f.clock.advance(time.Second)
	if sent, _, _ := f.deliver(t); sent != 20 {
		t.Fatalf("sent %d after Retry-After, want all 20", sent)
	}
}

func TestRateLimitWithoutRetryAfterWaitsThirtySecondsAndIsCapped(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	retryAfter := ""
	f.slack.set(func(post) (int, string, string) { return 429, retryAfter, `{"ok":false,"error":"ratelimited"}` })

	f.deliver(t)
	f.clock.advance(29 * time.Second)
	f.deliver(t)
	if got := f.slack.count(); got != 1 {
		t.Fatalf("%d posts, want the default 30s wait to hold", got)
	}
	f.clock.advance(time.Second)
	retryAfter = "86400"
	f.deliver(t)
	f.clock.advance(15*time.Minute - time.Second)
	f.deliver(t)
	if got := f.slack.count(); got != 2 {
		t.Fatalf("%d posts, want the wait held for just under 15 minutes", got)
	}
	f.clock.advance(time.Second)
	f.deliver(t)
	if got := f.slack.count(); got != 3 {
		t.Fatalf("%d posts, want the wait capped at 15 minutes", got)
	}
}

func TestRevokedTokenPausesSendingAndKeepsMessagesPending(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"invalid_auth", "token_revoked", "account_inactive", "not_authed"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			f := newSenderFixture(t)
			f.saveStoredToken(t, senderToken)
			var runs []int64
			for range 5 {
				run := f.runs.newRun(t)
				runs = append(runs, run)
				enqueue(t, f.pool, run, "started", f.clock.now())
				enqueue(t, f.pool, run, "finished", f.clock.now())
			}
			f.slack.set(func(post) (int, string, string) { return 200, "", `{"ok":false,"error":"` + code + `"}` })

			f.deliver(t)
			if got := f.slack.count(); got != 1 {
				t.Fatalf("%d posts with a refused token, want 1 and then a stop", got)
			}
			for _, run := range runs {
				for _, row := range rowsOfRun(t, f.pool, run) {
					if row.State != "pending" || row.Attempts != 0 || row.Claimed {
						t.Fatalf("run %d row %+v, want pending, no attempt used, not claimed", run, row)
					}
				}
			}
			status, err := LoadSlackStatus(context.Background(), f.pool)
			if err != nil || !status.Configured || status.InvalidSince == nil || !status.InvalidSince.Equal(f.clock.now()) {
				t.Fatalf("status %+v, %v; want the token recorded as invalid now", status, err)
			}

			f.clock.advance(2 * time.Hour)
			f.deliver(t)
			if got := f.slack.count(); got != 1 {
				t.Fatalf("%d posts later, want sending to stay paused until a new token is set", got)
			}
		})
	}
}

func TestSettingANewTokenResumesSending(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	f.saveStoredToken(t, senderToken)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	enqueue(t, f.pool, run, "finished", f.clock.now())
	f.slack.set(func(p post) (int, string, string) {
		if p.Token == senderToken {
			return 200, "", `{"ok":false,"error":"token_revoked"}`
		}
		return 200, "", `{"ok":true}`
	})
	f.deliver(t)
	f.clock.advance(time.Minute)

	const newToken = "xoxb-replacement-fake-token" //nolint:gosec // a fake token for tests
	f.saveStoredToken(t, newToken)
	status, err := LoadSlackStatus(context.Background(), f.pool)
	if err != nil || status.InvalidSince != nil {
		t.Fatalf("status %+v, %v after a new token, want it not invalid", status, err)
	}
	if sent, _, _ := f.deliver(t); sent != 2 {
		t.Fatalf("sent %d with the new token, want both messages", sent)
	}
	for _, row := range rowsOfRun(t, f.pool, run) {
		if row.State != "sent" || row.Attempts != 0 {
			t.Errorf("row %+v, want sent without any attempt used", row)
		}
	}
}

func TestANewTokenSetWhileAPostWasInFlightIsNotMarkedInvalid(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	f.saveStoredToken(t, senderToken)
	run := f.runs.newRun(t)
	enqueue(t, f.pool, run, "started", f.clock.now())
	f.slack.set(func(post) (int, string, string) {
		f.saveStoredToken(t, "xoxb-replacement-fake-token") // an admin sets one while this post is out
		return 200, "", `{"ok":false,"error":"invalid_auth"}`
	})
	f.deliver(t)
	if status, err := LoadSlackStatus(context.Background(), f.pool); err != nil || status.InvalidSince != nil {
		t.Fatalf("status %+v, %v; the new token must not be marked invalid by the old one's refusal", status, err)
	}
}

func TestMessagesPendingForADayExpire(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	f.setToken("") // no connection
	old, fresh := f.runs.newRun(t), f.runs.newRun(t)
	payload, _ := json.Marshal(map[string]string{"text": "old"})
	if err := store.EnqueueNotification(context.Background(), f.pool, store.Notification{
		RunID: old, Kind: "started", Channel: "#deploys", Payload: payload,
		CreatedAt: f.clock.now().Add(-25 * time.Hour), NextAttemptAt: f.clock.now(),
	}); err != nil {
		t.Fatal(err)
	}
	enqueue(t, f.pool, fresh, "started", f.clock.now().Add(-23*time.Hour))

	f.deliver(t)
	if row := rowsOfRun(t, f.pool, old)[0]; row.State != "failed" || row.LastError != "expired while Slack was unavailable" {
		t.Errorf("old row %+v, want failed as expired while Slack was unavailable", row)
	}
	if row := rowsOfRun(t, f.pool, fresh)[0]; row.State != "pending" {
		t.Errorf("fresh row %+v, want it still pending", row)
	}
}

func TestFailedNotificationGetsFailedAt(t *testing.T) {
	t.Parallel()
	f := newSenderFixture(t)
	refused, expired, delivered := f.runs.newRun(t), f.runs.newRun(t), f.runs.newRun(t)
	enqueue(t, f.pool, refused, "started", f.clock.now())
	enqueue(t, f.pool, delivered, "started", f.clock.now())
	payload, _ := json.Marshal(map[string]string{"text": "old"})
	if err := store.EnqueueNotification(context.Background(), f.pool, store.Notification{
		RunID: expired, Kind: "started", Channel: "#deploys", Payload: payload,
		CreatedAt: f.clock.now().Add(-25 * time.Hour), NextAttemptAt: f.clock.now(),
	}); err != nil {
		t.Fatal(err)
	}
	f.slack.set(func(p post) (int, string, string) {
		if p.Text == fmt.Sprintf("started@%d", refused) {
			return 200, "", `{"ok":false,"error":"channel_not_found"}`
		}
		return 200, "", `{"ok":true}`
	})
	f.deliver(t)

	failedAt := func(run int64) (state string, at *time.Time) {
		t.Helper()
		err := f.pool.QueryRow(context.Background(), `SELECT state, failed_at FROM notifications WHERE run_id = $1`, run).Scan(&state, &at)
		if err != nil {
			t.Fatalf("read failed_at: %v", err)
		}
		return state, at
	}
	for name, run := range map[string]int64{"refused by Slack": refused, "expired": expired} {
		state, at := failedAt(run)
		if state != "failed" || at == nil || at.Sub(f.clock.now()).Abs() > time.Minute {
			t.Errorf("%s: state %q, failed_at %v; want failed with failed_at at about %v", name, state, at, f.clock.now())
		}
	}
	if state, at := failedAt(delivered); state != "sent" || at != nil {
		t.Errorf("delivered: state %q, failed_at %v; want sent with no failed_at", state, at)
	}
}
