package store

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// notificationLease is how long a claimed notification belongs to its sender.
// A sender that dies after claiming leaves the row to be claimed again once
// the lease has run out.
const notificationLease = 2 * time.Minute

// Notification is one outgoing message in the outbox. A run has at most one
// per Kind.
type Notification struct {
	ID, RunID     int64
	Kind, Channel string
	Payload       []byte
	State         string // "pending" | "sent" | "failed"
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	CreatedAt     time.Time
	SentAt        *time.Time
}

const notificationColumns = `id, run_id, kind, channel, payload, state, attempts, next_attempt_at, last_error, created_at, sent_at`

func scanNotification(r rowScanner) (Notification, error) {
	var n Notification
	err := r.Scan(&n.ID, &n.RunID, &n.Kind, &n.Channel, &n.Payload, &n.State, &n.Attempts, &n.NextAttemptAt, &n.LastError, &n.CreatedAt, &n.SentAt)
	return n, err
}

// EnqueueNotification adds a pending message in the caller's transaction, so
// it exists exactly when the state change that caused it does. It silently
// does nothing when the run already has a message of that Kind. A zero
// NextAttemptAt or CreatedAt means now.
func EnqueueNotification(ctx context.Context, tx DBTX, n Notification) error {
	var next, created any
	if !n.NextAttemptAt.IsZero() {
		next = dbTime(n.NextAttemptAt)
	}
	if !n.CreatedAt.IsZero() {
		created = dbTime(n.CreatedAt)
	}
	payload := n.Payload
	if payload == nil {
		payload = []byte{} // a nil slice would be written as NULL
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO notifications (run_id, kind, channel, payload, next_attempt_at, created_at)
		 VALUES ($1, $2, $3, $4, coalesce($5::timestamptz, now()), coalesce($6::timestamptz, now()))
		 ON CONFLICT (run_id, kind) DO NOTHING`,
		n.RunID, n.Kind, n.Channel, payload, next, created)
	if err != nil {
		return fmt.Errorf("enqueue notification: %w", mapError(err))
	}
	return nil
}

// ClaimDueNotifications returns up to limit pending notifications whose
// NextAttemptAt is not after now, oldest (lowest id) first, and claims them
// for notificationLease. A notification is never returned while an earlier
// (lower id) one of the same run is still pending, whether that one is due,
// waiting for a retry or in flight, so a run's messages go out in order. A
// failed (given up) or sent message does not hold back later ones.
//
// What is guaranteed. The rows are chosen with FOR UPDATE SKIP LOCKED, but
// that lock lasts only until db's transaction ends:
//   - db is a transaction: concurrent claimers skip the rows until it commits
//     or rolls back, so a sender that claims, posts and marks inside one
//     transaction needs nothing more.
//   - db is a pool: each call is its own statement and the row lock is gone
//     when it returns, so the lock alone would let a second sender claim the
//     same rows. The claim therefore also pushes claimed_until forward in the
//     same statement and rows with an unexpired claim are not returned. Two
//     concurrent senders never get the same row until the lease (a couple of
//     minutes) has run out; that only happens if the first sender died or
//     stalled, in which case the message is tried again. MarkNotificationSent
//     and MarkNotificationRetry release the claim.
//
// Delivery is therefore at least once, never concurrent while a lease holds.
func ClaimDueNotifications(ctx context.Context, db DBTX, now time.Time, limit int) ([]Notification, error) {
	if limit < 1 {
		return []Notification{}, nil
	}
	now = dbTime(now)
	rows, err := db.Query(ctx,
		`UPDATE notifications SET claimed_until = $3
		 WHERE id IN (
		     SELECT n.id FROM notifications n
		     WHERE n.state = 'pending' AND n.next_attempt_at <= $1
		       AND (n.claimed_until IS NULL OR n.claimed_until <= $1)
		       AND NOT EXISTS (SELECT 1 FROM notifications earlier
		                       WHERE earlier.run_id = n.run_id AND earlier.id < n.id AND earlier.state = 'pending')
		     ORDER BY n.id LIMIT $2
		     FOR UPDATE SKIP LOCKED)
		   AND state = 'pending' AND (claimed_until IS NULL OR claimed_until <= $1)
		 RETURNING `+notificationColumns, now, limit, now.Add(notificationLease))
	if err != nil {
		return nil, fmt.Errorf("claim notifications: %w", mapError(err))
	}
	defer rows.Close()
	claimed := []Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, fmt.Errorf("claim notifications: %w", mapError(err))
		}
		claimed = append(claimed, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim notifications: %w", mapError(err))
	}
	sort.Slice(claimed, func(i, j int) bool { return claimed[i].ID < claimed[j].ID }) // RETURNING has no order
	return claimed, nil
}

// The Mark functions only touch a pending message. A sender that stalled past
// its lease and finds the message already settled by another sender must not
// overwrite that result, so for a message that is not pending they change
// nothing and return ErrNotFound, as for an unknown id.

// MarkNotificationSent records a delivered message and releases its claim.
// ErrNotFound for an unknown id or a message that is no longer pending.
func MarkNotificationSent(ctx context.Context, db DBTX, id int64, at time.Time) error {
	tag, err := db.Exec(ctx,
		`UPDATE notifications SET state = 'sent', sent_at = $2, claimed_until = NULL, last_error = ''
		 WHERE id = $1 AND state = 'pending'`,
		id, dbTime(at))
	if err != nil {
		return fmt.Errorf("mark notification sent: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark notification sent: %w", ErrNotFound)
	}
	return nil
}

// MarkNotificationRetry records a failed attempt (Attempts goes up by one) and
// releases the claim. With failed false the message stays pending and is tried
// again at next; with failed true it is given up on (state "failed") and no
// longer holds back later messages of its run. ErrNotFound for an unknown id
// or a message that is no longer pending. A given-up message gets failed_at =
// next, so pass the current time as next when failed is true.
func MarkNotificationRetry(ctx context.Context, db DBTX, id int64, lastError string, next time.Time, failed bool) error {
	state := "pending"
	var failedAt *time.Time
	if failed {
		state = "failed"
		failedAt = dbTimePtr(&next)
	}
	tag, err := db.Exec(ctx,
		`UPDATE notifications SET state = $2, attempts = attempts + 1, last_error = $3, next_attempt_at = $4, claimed_until = NULL,
			failed_at = $5
		 WHERE id = $1 AND state = 'pending'`, id, state, lastError, dbTime(next), failedAt)
	if err != nil {
		return fmt.Errorf("mark notification retry: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mark notification retry: %w", ErrNotFound)
	}
	return nil
}

// DeferNotification releases a claim and moves the next attempt to next
// without counting an attempt: for a message that could not be tried (no Slack
// connection) or whose try was cut short (shutdown). ErrNotFound for an
// unknown id or a message that is no longer pending.
func DeferNotification(ctx context.Context, db DBTX, id int64, next time.Time) error {
	tag, err := db.Exec(ctx,
		`UPDATE notifications SET next_attempt_at = $2, claimed_until = NULL WHERE id = $1 AND state = 'pending'`,
		id, dbTime(next))
	if err != nil {
		return fmt.Errorf("defer notification: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("defer notification: %w", ErrNotFound)
	}
	return nil
}

// ExpireStaleNotifications gives up on pending messages created before
// createdBefore (and not being posted right now at now), recording reason as
// their error, and returns how many it ended. Without it a long Slack outage
// would leave a backlog that grows without bound.
func ExpireStaleNotifications(ctx context.Context, db DBTX, createdBefore, now time.Time, reason string) (int, error) {
	tag, err := db.Exec(ctx,
		`UPDATE notifications SET state = 'failed', last_error = $3, claimed_until = NULL, failed_at = $2
		 WHERE state = 'pending' AND created_at < $1 AND (claimed_until IS NULL OR claimed_until <= $2)`,
		dbTime(createdBefore), dbTime(now), reason)
	if err != nil {
		return 0, fmt.Errorf("expire notifications: %w", mapError(err))
	}
	return int(tag.RowsAffected()), nil
}

// NotificationStats is for system health: how many messages are pending and
// failed, and when the oldest pending one was created (nil when none).
func NotificationStats(ctx context.Context, db DBTX) (pending, failed int, oldestPending *time.Time, err error) {
	err = db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE state = 'pending'), count(*) FILTER (WHERE state = 'failed'),
		        min(created_at) FILTER (WHERE state = 'pending')
		 FROM notifications`).Scan(&pending, &failed, &oldestPending)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("notification stats: %w", mapError(err))
	}
	return pending, failed, oldestPending, nil
}
