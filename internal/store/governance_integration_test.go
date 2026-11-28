//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newApproval(fx runFixture, runID int64, now time.Time) Approval {
	return Approval{RunID: runID, ManifestID: "m-1", RequestedBy: fx.userID, RequestedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
}

func TestOneOpenApprovalPerRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	run := fx.insert(t, pool, "waiting_approval")
	now := time.Now().UTC().Truncate(time.Microsecond)

	first, err := RequestApproval(ctx, pool, newApproval(fx, run.ID, now))
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if first.ID == 0 || first.Decision != "" || first.DecidedBy != nil {
		t.Fatalf("unexpected approval: %+v", first)
	}
	if _, err := RequestApproval(ctx, pool, newApproval(fx, run.ID, now)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second open approval: got %v, want ErrDuplicate", err)
	}
	got, ok, err := OpenApproval(ctx, pool, run.ID)
	if err != nil || !ok || got.ID != first.ID || !got.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("OpenApproval = %+v, %v, %v", got, ok, err)
	}

	// Once decided, a new request for the same run is allowed again.
	if err := DecideApproval(ctx, pool, first.ID, "rejected", &fx.userID, "no", now); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if _, ok, err := OpenApproval(ctx, pool, run.ID); err != nil || ok {
		t.Fatalf("OpenApproval after decision = %v, %v; want none", ok, err)
	}
	if _, err := RequestApproval(ctx, pool, newApproval(fx, run.ID, now)); err != nil {
		t.Fatalf("RequestApproval after decision: %v", err)
	}
}

func TestDecideApprovalOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	run := fx.insert(t, pool, "waiting_approval")
	now := time.Now().UTC().Truncate(time.Microsecond)
	a, err := RequestApproval(ctx, pool, newApproval(fx, run.ID, now))
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if err := DecideApproval(ctx, pool, a.ID, "approved", &fx.userID, "ok", now); err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}
	if err := DecideApproval(ctx, pool, a.ID, "rejected", &fx.userID, "late", now); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("second decision: got %v, want ErrAlreadyDecided", err)
	}
	if err := DecideApproval(ctx, pool, a.ID+1000, "approved", nil, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: got %v, want ErrNotFound", err)
	}
	if err := DecideApproval(ctx, pool, a.ID, "maybe", nil, "", now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown decision: got %v, want ErrInvalid", err)
	}
	var decision, comment string
	var by *int64
	if err := pool.QueryRow(ctx, "SELECT decision, comment, decided_by FROM approvals WHERE id = $1", a.ID).Scan(&decision, &comment, &by); err != nil {
		t.Fatal(err)
	}
	if decision != "approved" || comment != "ok" || by == nil || *by != fx.userID {
		t.Fatalf("stored decision = %q %q %v; the first decision must stand", decision, comment, by)
	}
}

func TestDecideApprovalConcurrentlyOneWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	run := fx.insert(t, pool, "waiting_approval")
	now := time.Now().UTC().Truncate(time.Microsecond)
	a, err := RequestApproval(ctx, pool, newApproval(fx, run.ID, now))
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()

	if err := DecideApproval(ctx, tx1, a.ID, "approved", &fx.userID, "first", now); err != nil {
		t.Fatalf("tx1 decide: %v", err)
	}
	// tx2 runs while tx1 is still open: it must wait for tx1's row lock, not
	// overwrite it, and fail once tx1 commits.
	result := make(chan error, 1)
	go func() { result <- DecideApproval(ctx, tx2, a.ID, "rejected", &fx.userID, "second", now) }()
	select {
	case err := <-result:
		t.Fatalf("tx2 returned %v before tx1 committed; it should have waited", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatalf("tx1 commit: %v", err)
	}
	if err := <-result; !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("tx2 decide: got %v, want ErrAlreadyDecided", err)
	}
	var decision, comment string
	if err := pool.QueryRow(ctx, "SELECT decision, comment FROM approvals WHERE id = $1", a.ID).Scan(&decision, &comment); err != nil {
		t.Fatal(err)
	}
	if decision != "approved" || comment != "first" {
		t.Fatalf("stored = %q %q, want approved/first", decision, comment)
	}
}

func TestExpiredApprovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if open, err := OpenApprovals(ctx, pool); err != nil || open == nil || len(open) != 0 {
		t.Fatalf("empty OpenApprovals = %#v, %v; want empty non-nil slice", open, err)
	}
	if exp, err := ExpiredApprovals(ctx, pool, now); err != nil || exp == nil || len(exp) != 0 {
		t.Fatalf("empty ExpiredApprovals = %#v, %v; want empty non-nil slice", exp, err)
	}

	mk := func(expires time.Time) Approval {
		run := fx.insert(t, pool, "waiting_approval")
		a := newApproval(fx, run.ID, now.Add(-48*time.Hour))
		a.ExpiresAt = expires
		got, err := RequestApproval(ctx, pool, a)
		if err != nil {
			t.Fatalf("RequestApproval: %v", err)
		}
		return got
	}
	past := mk(now.Add(-time.Hour))
	future := mk(now.Add(time.Hour))
	decidedPast := mk(now.Add(-time.Hour))
	if err := DecideApproval(ctx, pool, decidedPast.ID, "approved", &fx.userID, "", now); err != nil {
		t.Fatal(err)
	}

	exp, err := ExpiredApprovals(ctx, pool, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp) != 1 || exp[0].ID != past.ID {
		t.Fatalf("ExpiredApprovals = %+v, want only %d", exp, past.ID)
	}
	open, err := OpenApprovals(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].ID != past.ID || open[1].ID != future.ID {
		t.Fatalf("OpenApprovals = %+v, want %d and %d", open, past.ID, future.ID)
	}
}

func TestFreezeNeedsExactlyOneScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	base := Freeze{Reason: "release week", CreatedBy: fx.userID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}

	both := base
	both.Label, both.Environment = "prod", "prod-eu"
	if _, err := CreateFreeze(ctx, pool, both); !errors.Is(err, ErrInvalid) {
		t.Fatalf("both scopes: got %v, want ErrInvalid", err)
	}
	if _, err := CreateFreeze(ctx, pool, base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no scope: got %v, want ErrInvalid", err)
	}
	// The schema enforces it too, for any writer that skips the Go check.
	if _, err := pool.Exec(ctx,
		`INSERT INTO freezes (label, environment, reason, created_by, created_at, expires_at) VALUES ('a', 'b', 'x', $1, now(), now() + interval '1 hour')`,
		fx.userID); !errors.Is(MapError(err), ErrInvalid) {
		t.Fatalf("direct insert with both scopes: got %v, want a check violation", err)
	}

	byLabel := base
	byLabel.Label = "prod"
	got, err := CreateFreeze(ctx, pool, byLabel)
	if err != nil || got.ID == 0 || got.Label != "prod" || got.Environment != "" {
		t.Fatalf("label freeze = %+v, %v", got, err)
	}
	byEnv := base
	byEnv.Environment = "prod-eu"
	if got, err := CreateFreeze(ctx, pool, byEnv); err != nil || got.Environment != "prod-eu" {
		t.Fatalf("environment freeze = %+v, %v", got, err)
	}
}

func TestActiveFreezesExcludeLiftedAndExpired(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if active, err := ActiveFreezes(ctx, pool, now); err != nil || active == nil || len(active) != 0 {
		t.Fatalf("empty ActiveFreezes = %#v, %v; want empty non-nil slice", active, err)
	}
	if all, err := ListFreezes(ctx, pool, 10); err != nil || all == nil || len(all) != 0 {
		t.Fatalf("empty ListFreezes = %#v, %v; want empty non-nil slice", all, err)
	}

	mk := func(label string, expires time.Time) Freeze {
		f, err := CreateFreeze(ctx, pool, Freeze{Label: label, Reason: "r", CreatedBy: fx.userID, CreatedAt: now.Add(-time.Hour), ExpiresAt: expires})
		if err != nil {
			t.Fatalf("CreateFreeze: %v", err)
		}
		return f
	}
	live := mk("live", now.Add(time.Hour))
	lifted := mk("lifted", now.Add(time.Hour))
	expired := mk("expired", now.Add(-time.Minute))

	if err := LiftFreeze(ctx, pool, lifted.ID, fx.userID, now); err != nil {
		t.Fatalf("LiftFreeze: %v", err)
	}
	if err := LiftFreeze(ctx, pool, lifted.ID, fx.userID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lifting a lifted freeze: got %v, want ErrNotFound", err)
	}
	if err := LiftFreeze(ctx, pool, expired.ID, fx.userID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lifting an expired freeze: got %v, want ErrNotFound", err)
	}
	if err := LiftFreeze(ctx, pool, live.ID+1000, fx.userID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lifting an unknown freeze: got %v, want ErrNotFound", err)
	}

	active, err := ActiveFreezes(ctx, pool, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != live.ID {
		t.Fatalf("ActiveFreezes = %+v, want only %d", active, live.ID)
	}
	all, err := ListFreezes(ctx, pool, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].ID != expired.ID || all[1].ID != lifted.ID {
		t.Fatalf("ListFreezes(2) = %+v, want newest first: %d, %d", all, expired.ID, lifted.ID)
	}
	if all[1].LiftedBy == nil || *all[1].LiftedBy != fx.userID || all[1].LiftedAt == nil {
		t.Fatalf("lifted freeze lost who/when: %+v", all[1])
	}
}

func notif(runID int64, kind string, now time.Time) Notification {
	return Notification{RunID: runID, Kind: kind, Channel: "#deploys", Payload: []byte(`{"text":"hi"}`), NextAttemptAt: now}
}

func TestEnqueueIsOncePerRunAndKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	run := fx.insert(t, pool, "planned")
	now := time.Now().UTC().Truncate(time.Microsecond)

	for i := 0; i < 2; i++ {
		if err := EnqueueNotification(ctx, pool, notif(run.ID, "waiting_approval", now)); err != nil {
			t.Fatalf("enqueue #%d: %v", i, err)
		}
	}
	if err := EnqueueNotification(ctx, pool, notif(run.ID, "succeeded", now)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM notifications WHERE run_id = $1", run.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rows = %d, want 2 (one per kind)", n)
	}
	got, err := ClaimDueNotifications(ctx, pool, now, 10)
	if err != nil || len(got) != 1 || got[0].Kind != "waiting_approval" || string(got[0].Payload) != `{"text":"hi"}` || got[0].Channel != "#deploys" {
		t.Fatalf("claimed = %+v, %v", got, err)
	}
}

func onlyRun(ns []Notification, runID int64) []Notification {
	out := []Notification{}
	for _, n := range ns {
		if n.RunID == runID {
			out = append(out, n)
		}
	}
	return out
}

func TestClaimKeepsPerRunOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	runA := fx.insert(t, pool, "planned")
	runB := fx.insert(t, pool, "planned")
	now := time.Now().UTC().Truncate(time.Microsecond)

	for _, n := range []Notification{
		notif(runA.ID, "waiting_approval", now), notif(runA.ID, "succeeded", now), notif(runB.ID, "waiting_approval", now),
		notif(runB.ID, "failed", now.Add(time.Hour)), // not due yet
	} {
		if err := EnqueueNotification(ctx, pool, n); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ClaimDueNotifications(ctx, pool, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RunID != runA.ID || got[0].Kind != "waiting_approval" || got[1].RunID != runB.ID || got[1].Kind != "waiting_approval" {
		t.Fatalf("first claim = %+v, want only each run's first message, oldest first", got)
	}

	// While the first is pending (claimed, in flight), a second claim gets nothing.
	again, err := ClaimDueNotifications(ctx, pool, now, 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("second claim = %+v, %v; want nothing (in flight, later ones blocked)", again, err)
	}

	// A retry keeps the run's second message waiting.
	if err := MarkNotificationRetry(ctx, pool, got[0].ID, "boom", now.Add(time.Minute), false); err != nil {
		t.Fatal(err)
	}
	if c, _ := ClaimDueNotifications(ctx, pool, now, 10); len(c) != 0 {
		t.Fatalf("claim while first of run A retries = %+v, want nothing", c)
	}
	later, err := ClaimDueNotifications(ctx, pool, now.Add(90*time.Second), 10)
	if err != nil || len(later) != 1 || later[0].ID != got[0].ID || later[0].Attempts != 1 || later[0].LastError != "boom" {
		t.Fatalf("claim after retry time = %+v, %v; want the retried first message", later, err)
	}

	// Sent unblocks the next one.
	if err := MarkNotificationSent(ctx, pool, got[0].ID, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	next, err := ClaimDueNotifications(ctx, pool, now.Add(3*time.Minute), 10)
	next = onlyRun(next, runA.ID) // run B's unfinished first message may be reclaimed once its lease ends
	if err != nil || len(next) != 1 || next[0].RunID != runA.ID || next[0].Kind != "succeeded" {
		t.Fatalf("claim after sent = %+v, %v; want run A's second message", next, err)
	}

	// A failed (given up) message does not block the run's next one.
	if err := MarkNotificationRetry(ctx, pool, got[1].ID, "gave up", now, true); err != nil {
		t.Fatal(err)
	}
	final, err := ClaimDueNotifications(ctx, pool, now.Add(2*time.Hour), 10)
	final = onlyRun(final, runB.ID)
	if err != nil || len(final) != 1 || final[0].RunID != runB.ID || final[0].Kind != "failed" {
		t.Fatalf("claim after give-up = %+v, %v; want run B's second message", final, err)
	}
	if err := MarkNotificationSent(ctx, pool, 999999, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("MarkNotificationSent unknown id: got %v, want ErrNotFound", err)
	}
}

func TestClaimedRowIsNotClaimedAgainUntilLeaseEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	run := fx.insert(t, pool, "planned")
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := EnqueueNotification(ctx, pool, notif(run.ID, "waiting_approval", now)); err != nil {
		t.Fatal(err)
	}
	if got, err := ClaimDueNotifications(ctx, pool, now, 10); err != nil || len(got) != 1 {
		t.Fatalf("first claim = %+v, %v", got, err)
	}
	// A second sender on a pool (no transaction held open) must not get the same row.
	if got, err := ClaimDueNotifications(ctx, pool, now.Add(time.Second), 10); err != nil || len(got) != 0 {
		t.Fatalf("claim during lease = %+v, %v; want nothing", got, err)
	}
	// A sender that died: after the lease the row is claimable again.
	if got, err := ClaimDueNotifications(ctx, pool, now.Add(time.Hour), 10); err != nil || len(got) != 1 {
		t.Fatalf("claim after lease = %+v, %v; want the row again", got, err)
	}
}

func TestExpireStaleNotificationsSkipsOneBeingSent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	createdBefore := now.Add(-time.Hour)
	old := now.Add(-2 * time.Hour)
	enqueueOld := func(runID int64) {
		t.Helper()
		n := notif(runID, "waiting_approval", now)
		n.CreatedAt = old
		if err := EnqueueNotification(ctx, pool, n); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// Claimed first, so its lease is still running at now and it is the only row a sender can claim.
	beingSent := fx.insert(t, pool, "planned")
	enqueueOld(beingSent.ID)
	if got, err := ClaimDueNotifications(ctx, pool, now, 10); err != nil || len(got) != 1 {
		t.Fatalf("claim = %+v, %v; want the one message", got, err)
	}
	unclaimed := fx.insert(t, pool, "planned")
	enqueueOld(unclaimed.ID)
	staleClaim := fx.insert(t, pool, "planned")
	enqueueOld(staleClaim.ID)
	if _, err := pool.Exec(ctx, "UPDATE notifications SET claimed_until = $2 WHERE run_id = $1", staleClaim.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	fresh := fx.insert(t, pool, "planned")
	n := notif(fresh.ID, "waiting_approval", now)
	n.CreatedAt = now
	if err := EnqueueNotification(ctx, pool, n); err != nil {
		t.Fatalf("enqueue fresh: %v", err)
	}

	expired, err := ExpireStaleNotifications(ctx, pool, createdBefore, now, "gave up")
	if err != nil || expired != 2 {
		t.Fatalf("ExpireStaleNotifications = %d, %v; want 2 expired", expired, err)
	}

	type outcome struct {
		state, lastError string
		leased           bool
	}
	outcomeOf := func(runID int64) outcome {
		t.Helper()
		var o outcome
		if err := pool.QueryRow(ctx,
			"SELECT state, last_error, claimed_until IS NOT NULL AND claimed_until > $2 FROM notifications WHERE run_id = $1",
			runID, now).Scan(&o.state, &o.lastError, &o.leased); err != nil {
			t.Fatalf("read run %d: %v", runID, err)
		}
		return o
	}
	if got := outcomeOf(unclaimed.ID); got.state != "failed" || got.lastError != "gave up" {
		t.Errorf("unclaimed old message = %+v, want failed with the reason", got)
	}
	if got := outcomeOf(staleClaim.ID); got.state != "failed" || got.lastError != "gave up" {
		t.Errorf("old message with a stale claim = %+v, want failed with the reason", got)
	}
	if got := outcomeOf(beingSent.ID); got.state != "pending" || !got.leased {
		t.Errorf("message being sent = %+v, want still pending and leased", got)
	}
	if got := outcomeOf(fresh.ID); got.state != "pending" {
		t.Errorf("message newer than the cutoff = %+v, want still pending", got)
	}
}

func TestNotificationStats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	fx := newRunFixture(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	pending, failed, oldest, err := NotificationStats(ctx, pool)
	if err != nil || pending != 0 || failed != 0 || oldest != nil {
		t.Fatalf("empty stats = %d, %d, %v, %v", pending, failed, oldest, err)
	}

	runs := []Run{fx.insert(t, pool, "planned"), fx.insert(t, pool, "planned"), fx.insert(t, pool, "planned")}
	older := now.Add(-2 * time.Hour)
	for i, r := range runs {
		n := notif(r.ID, "waiting_approval", now)
		n.CreatedAt = now.Add(-time.Duration(i) * time.Hour)
		if i == 0 {
			n.CreatedAt = now.Add(-time.Minute)
		}
		if i == 2 {
			n.CreatedAt = older
		}
		if err := EnqueueNotification(ctx, pool, n); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := ClaimDueNotifications(ctx, pool, now, 10)
	if err != nil || len(claimed) != 3 {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	for _, c := range claimed {
		switch c.RunID {
		case runs[0].ID:
			if err := MarkNotificationSent(ctx, pool, c.ID, now); err != nil {
				t.Fatal(err)
			}
		case runs[1].ID:
			if err := MarkNotificationRetry(ctx, pool, c.ID, "x", now, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	pending, failed, oldest, err = NotificationStats(ctx, pool)
	if err != nil || pending != 1 || failed != 1 || oldest == nil || !oldest.Equal(older) {
		t.Fatalf("stats = %d pending, %d failed, oldest %v (want %v), %v", pending, failed, oldest, older, err)
	}
}
