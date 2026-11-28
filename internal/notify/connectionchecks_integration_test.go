//go:build integration

package notify

import (
	"context"
	"testing"

	"github.com/rcss-labs/yamdrake/internal/store"
)

func TestSlackDeliveryRecordsConnectionCheck(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, f *senderFixture) store.ConnectionCheck {
		t.Helper()
		checks, err := store.ListConnectionChecks(context.Background(), f.pool)
		if err != nil {
			t.Fatalf("ListConnectionChecks: %v", err)
		}
		for _, c := range checks {
			if c.Kind == "slack" && c.Ref == "slack" {
				return c
			}
		}
		t.Fatalf("no slack connection check in %+v", checks)
		return store.ConnectionCheck{}
	}

	t.Run("a delivered message records OK", func(t *testing.T) {
		t.Parallel()
		f := newSenderFixture(t)
		enqueue(t, f.pool, f.runs.newRun(t), "started", f.clock.now())
		f.deliver(t)
		if c := check(t, f); c.LastOKAt == nil || c.LastErrorAt != nil {
			t.Fatalf("check = %+v, want OK recorded and no error", c)
		}
	})
	t.Run("a refused token records its code", func(t *testing.T) {
		t.Parallel()
		f := newSenderFixture(t)
		enqueue(t, f.pool, f.runs.newRun(t), "started", f.clock.now())
		f.slack.set(func(post) (int, string, string) { return 200, "", `{"ok":false,"error":"invalid_auth"}` })
		f.deliver(t)
		if c := check(t, f); c.LastErrorCode != "invalid_auth" || c.LastErrorAt == nil {
			t.Fatalf("check = %+v, want invalid_auth recorded", c)
		}
	})
}
