package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/store"
)

const (
	// maxAttempts is how many failed posts a message gets before it is given up on.
	maxAttempts = 12
	// firstBackoff doubles after every failed attempt up to maxBackoff.
	firstBackoff = time.Minute
	maxBackoff   = 15 * time.Minute
	// noConnectionBackoff is how long messages wait when no Slack token is stored.
	noConnectionBackoff = 15 * time.Minute
	// claimBatch is how many messages one claim takes; deliveries repeat until none are due.
	claimBatch = 50
	// maxRoundsPerDelivery bounds one DeliverDue call so a flood cannot hold it forever.
	maxRoundsPerDelivery = 200
	// shutdownWriteTimeout bounds the write that releases a claim, or records a
	// sent post, after shutdown began.
	shutdownWriteTimeout = 5 * time.Second
	// claimPostingBudget is the 2-minute claim lease minus the 10s Slack timeout
	// and a margin, so a row is never posted after its lease may have run out.
	claimPostingBudget = 90 * time.Second
	// defaultRateLimitWait is how long a 429 without a usable Retry-After pauses
	// sending; maxRateLimitWait caps what Slack asks for.
	defaultRateLimitWait = 30 * time.Second
	maxRateLimitWait     = 15 * time.Minute
	// maxPendingAge is how long a message may wait before it is given up on.
	maxPendingAge = 24 * time.Hour
	// expiredReason is the error recorded on a message that waited too long.
	expiredReason = "expired while Slack was unavailable"
)

// Sender delivers the notification outbox to Slack.
type Sender struct {
	pool   *pgxpool.Pool
	slack  *Slack
	tokens TokenSource
	now    func() time.Time
	log    *slog.Logger

	mu          sync.Mutex
	pausedUntil time.Time // set by a 429: no round starts before it
}

// NewSender builds a sender over pool. A nil now means time.Now; a nil log discards.
func NewSender(pool *pgxpool.Pool, slack *Slack, tokens TokenSource, now func() time.Time, log *slog.Logger) *Sender {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sender{pool: pool, slack: slack, tokens: tokens, now: now, log: log}
}

// Run delivers due notifications every interval until ctx ends. A failed
// round is logged and the next one tries again.
func (s *Sender) Run(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		sent, retried, failed, err := s.DeliverDue(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			s.log.Error("notification delivery failed", "error", err.Error())
		case sent+retried+failed > 0:
			s.log.Info("notifications delivered", "sent", sent, "retried", retried, "failed", failed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// backoff is the wait after the n-th failed attempt: one minute, doubling, at most fifteen.
func backoff(n int) time.Duration {
	d := firstBackoff
	for i := 1; i < n && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// DeliverDue claims the notifications that are due and posts them, until none
// are left that are due. Rows of one run go out in order (the claim never
// hands out a message while an earlier one of its run is pending).
//
// A 429 ends the round at once, uses no attempt and holds this sender back for
// the Retry-After seconds (30 when missing, at most 15 minutes). A token Slack
// refuses (invalid_auth, token_revoked, account_inactive, not_authed) stops
// sending too: the token is recorded as invalid, every message stays pending
// with no attempt used, and nothing is posted until a new token is stored.
// Messages pending for more than 24 hours are failed on every call.
//
// With no Slack token stored, due messages stay pending and wait fifteen
// minutes without using up an attempt. When ctx ends mid-post, the message is
// released without an attempt counted and ctx's error is returned.
func (s *Sender) DeliverDue(ctx context.Context) (sent, retried, failed int, err error) {
	if expired, err := store.ExpireStaleNotifications(ctx, s.pool, s.now().Add(-maxPendingAge), s.now(), expiredReason); err != nil {
		return 0, 0, 0, err
	} else if expired > 0 {
		s.log.Warn("notifications expired while Slack was unavailable", "count", expired)
		failed += expired
	}
	s.mu.Lock()
	paused := s.now().Before(s.pausedUntil)
	s.mu.Unlock()
	if paused {
		return sent, retried, failed, nil
	}
	status, err := LoadSlackStatus(ctx, s.pool)
	if err != nil {
		return sent, retried, failed, err
	}
	if status.InvalidSince != nil {
		return sent, retried, failed, nil // waiting for a new token
	}
	token, err := s.tokens(ctx)
	if err != nil {
		return sent, retried, failed, err
	}
	for range maxRoundsPerDelivery {
		claimedAt := s.now()
		due, err := store.ClaimDueNotifications(ctx, s.pool, claimedAt, claimBatch)
		if err != nil {
			return sent, retried, failed, err
		}
		if len(due) == 0 {
			return sent, retried, failed, nil
		}
		for i, n := range due {
			if token == "" {
				if err := s.settle(ctx, n, store.DeferNotification(ctx, s.pool, n.ID, s.now().Add(noConnectionBackoff))); err != nil {
					return sent, retried, failed, err
				}
				continue
			}
			if s.now().Sub(claimedAt) >= claimPostingBudget {
				// The rest of the batch may now be held by another sender; their leases run out on their own.
				return sent, retried, failed, nil
			}
			deliveryResult, err := s.deliver(ctx, token, n)
			if err != nil {
				s.release(due[i+1:])
				return sent, retried, failed, err
			}
			switch deliveryResult {
			case deliveryResultSent:
				sent++
			case deliveryResultRetry:
				retried++
			case deliveryResultFailed:
				failed++
			case deliveryResultRateLimited:
				s.release(due[i+1:]) // the message itself was deferred
				return sent, retried, failed, nil
			case deliveryResultTokenRefused:
				s.release(due[i+1:])
				if _, err := MarkSlackTokenInvalid(ctx, s.pool, status, s.now()); err != nil {
					return sent, retried, failed, err
				}
				return sent, retried, failed, nil
			}
		}
		if token == "" {
			return sent, retried, failed, nil
		}
	}
	return sent, retried, failed, nil
}

type deliveryResult int

const (
	deliveryResultSent deliveryResult = iota
	deliveryResultRetry
	deliveryResultFailed
	deliveryResultStale        // another sender settled the message first
	deliveryResultRateLimited  // Slack asked for a pause; the round ends
	deliveryResultTokenRefused // Slack refused the token; the round ends
)

// deliver posts one claimed message and records the result. It returns a
// non-nil error only when ctx ended (the message is released first) or the
// database failed.
func (s *Sender) deliver(ctx context.Context, token string, n store.Notification) (deliveryResult, error) {
	text, ok := messageText(n.Payload)
	if !ok {
		return s.record(ctx, n, store.MarkNotificationRetry(ctx, s.pool, n.ID, "bad_payload", s.now(), true), deliveryResultFailed)
	}

	postErr := s.slack.Post(ctx, token, n.Channel, text)
	if postErr == nil {
		// The post happened, so the sent mark is written even if shutdown began during it.
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownWriteTimeout)
		defer cancel()
		s.recordSlackOK(markCtx)
		return s.record(markCtx, n, store.MarkNotificationSent(markCtx, s.pool, n.ID, s.now()), deliveryResultSent)
	}
	if ctx.Err() != nil {
		// Shutdown: the post's result is unknown; give the message back as it was.
		s.release([]store.Notification{n})
		return deliveryResultStale, ctx.Err()
	}

	code, permanent, retryAfter := classify(postErr)
	var slackErr *SlackError
	if errors.As(postErr, &slackErr) {
		switch {
		case slackErr.Status == http.StatusTooManyRequests || code == "ratelimited":
			wait := retryAfter
			if wait <= 0 {
				wait = defaultRateLimitWait
			}
			wait = min(wait, maxRateLimitWait)
			s.mu.Lock()
			s.pausedUntil = s.now().Add(wait)
			s.mu.Unlock()
			s.log.Warn("slack rate limited the sender", "wait", wait.String())
			return s.record(ctx, n, store.DeferNotification(ctx, s.pool, n.ID, s.now().Add(wait)), deliveryResultRateLimited)
		case tokenFaultCodes[code]:
			s.log.Error("slack refused the token; sending is paused until a new token is stored", "error", code)
			s.recordSlackError(ctx, code)
			return s.record(ctx, n, store.DeferNotification(ctx, s.pool, n.ID, s.now()), deliveryResultTokenRefused)
		}
	}
	attempt := n.Attempts + 1
	if permanent || attempt >= maxAttempts {
		s.log.Warn("notification given up", "id", n.ID, "run_id", n.RunID, "kind", n.Kind, "attempts", attempt, "error", code)
		s.recordSlackError(ctx, code)
		return s.record(ctx, n, store.MarkNotificationRetry(ctx, s.pool, n.ID, code, s.now(), true), deliveryResultFailed)
	}
	s.recordSlackError(ctx, code)
	wait := max(backoff(attempt), retryAfter)
	return s.record(ctx, n, store.MarkNotificationRetry(ctx, s.pool, n.ID, code, s.now().Add(wait), false), deliveryResultRetry)
}

// record turns the result of a Mark call into an deliveryResult. A message another
// sender already settled (ErrNotFound) is left as that sender made it.
func (s *Sender) record(ctx context.Context, n store.Notification, markErr error, done deliveryResult) (deliveryResult, error) {
	switch {
	case markErr == nil:
		return done, nil
	case errors.Is(markErr, store.ErrNotFound):
		s.log.Warn("notification was settled by another sender; leaving it", "id", n.ID, "run_id", n.RunID, "kind", n.Kind)
		return deliveryResultStale, nil
	case ctx.Err() != nil:
		s.release([]store.Notification{n})
		return deliveryResultStale, ctx.Err()
	}
	return deliveryResultStale, markErr
}

// settle is record for the no-connection path, where a stale row is not an error.
func (s *Sender) settle(ctx context.Context, n store.Notification, markErr error) error {
	_, err := s.record(ctx, n, markErr, deliveryResultRetry)
	return err
}

// release gives claimed messages back, due at once, without counting an
// attempt. It uses its own short context because it runs after shutdown began.
func (s *Sender) release(ns []store.Notification) {
	if len(ns) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWriteTimeout)
	defer cancel()
	for _, n := range ns {
		if err := store.DeferNotification(ctx, s.pool, n.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("could not release a claimed notification; its lease will run out", "id", n.ID)
		}
	}
}

// recordSlackOK stores that Slack accepted a post. Failing to record is logged only.
func (s *Sender) recordSlackOK(ctx context.Context) {
	if err := store.RecordConnectionOK(ctx, s.pool, "slack", "slack", s.now()); err != nil {
		s.log.Warn("could not record connection check", "kind", "slack", "error", err.Error())
	}
}

// recordSlackError stores that a post failed, with its safe code. Failing to record is logged only.
func (s *Sender) recordSlackError(ctx context.Context, code string) {
	if err := store.RecordConnectionError(ctx, s.pool, "slack", "slack", code, s.now()); err != nil {
		s.log.Warn("could not record connection check", "kind", "slack", "error", err.Error())
	}
}

// messageText reads the text out of a payload of the form {"text": "..."}.
func messageText(payload []byte) (string, bool) {
	var doc struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil || strings.TrimSpace(doc.Text) == "" {
		return "", false
	}
	return doc.Text, true
}

// ErrorCode is the short, safe description of a failed post that is stored or
// shown to people: Slack's error code, "http_5xx" or "transport".
func ErrorCode(err error) string {
	code, _, _ := classify(err)
	return code
}

// classify reduces a failed post to what is safe to store: Slack's error code
// or a short class, never a body, URL or token.
func classify(err error) (code string, permanent bool, retryAfter time.Duration) {
	var se *SlackError
	if !errors.As(err, &se) {
		return "transport", false, 0
	}
	code = se.Code
	if se.Status >= http.StatusInternalServerError && strings.HasPrefix(code, "http_") {
		code = "http_5xx"
	}
	return code, se.Permanent, se.RetryAfter
}
