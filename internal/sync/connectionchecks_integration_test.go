//go:build integration

package sync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/github"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// connectionCheck returns the stored check for kind and ref.
func (h *harness) connectionCheck(kind, ref string) store.ConnectionCheck {
	h.t.Helper()
	checks, err := store.ListConnectionChecks(context.Background(), h.pool)
	if err != nil {
		h.t.Fatalf("ListConnectionChecks: %v", err)
	}
	for _, c := range checks {
		if c.Kind == kind && c.Ref == ref {
			return c
		}
	}
	h.t.Fatalf("no connection check for %s %s in %+v", kind, ref, checks)
	return store.ConnectionCheck{}
}

func TestSyncRecordsGitHubConnectionCheck(t *testing.T) {
	t.Parallel()
	t.Run("a working source records OK for the owner", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.firstSync()
		if c := h.connectionCheck("github", "acme"); c.LastOKAt == nil || c.LastErrorAt != nil {
			t.Fatalf("check = %+v, want OK recorded and no error", c)
		}
	})
	t.Run("a request failure records RequestFailed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.firstSync()
		h.src.setErr(errors.New("github: 502 bad gateway"))
		h.tick()
		if err := h.syncAll(); err == nil {
			t.Fatal("SyncAll succeeded with a failing source")
		}
		if c := h.connectionCheck("github", "acme"); c.LastErrorCode != "RequestFailed" || c.LastErrorAt == nil {
			t.Fatalf("check = %+v, want RequestFailed recorded", c)
		}
	})
	t.Run("a rate limit records RateLimited", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.firstSync()
		h.src.setErr(&github.RateLimitError{RetryAfter: time.Minute})
		h.tick()
		if err := h.syncAll(); err == nil {
			t.Fatal("SyncAll succeeded when rate limited")
		}
		if c := h.connectionCheck("github", "acme"); c.LastErrorCode != "RateLimited" || c.LastErrorAt == nil {
			t.Fatalf("check = %+v, want RateLimited recorded", c)
		}
	})
	t.Run("a missing file records OK and no error", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.configureInventory()
		h.pushInventory("c1")
		h.src.setErr(fmt.Errorf("github: 404: %w", github.ErrNotFound))
		if err := h.engine.SyncInventory(context.Background()); err == nil {
			t.Fatal("SyncInventory succeeded with a missing file")
		}
		if c := h.connectionCheck("github", "acme"); c.LastOKAt == nil || c.LastErrorAt != nil {
			t.Fatalf("check = %+v, want OK recorded and no error", c)
		}
	})
}
