//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestConnectionChecksKeepLastOKAndLastError(t *testing.T) {
	t.Parallel()
	pool := newPool(t)
	ctx := context.Background()
	empty, err := ListConnectionChecks(ctx, pool)
	if err != nil {
		t.Fatalf("ListConnectionChecks on an empty table: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("ListConnectionChecks on an empty table = %#v, want a non-nil empty slice", empty)
	}
	ok1 := micro(nanosecondNow())
	failed := ok1.Add(time.Minute)
	ok2 := failed.Add(time.Minute)

	if err := RecordConnectionOK(ctx, pool, "github", "acme", ok1); err != nil {
		t.Fatalf("RecordConnectionOK: %v", err)
	}
	if err := RecordConnectionError(ctx, pool, "github", "acme", "RateLimited", failed); err != nil {
		t.Fatalf("RecordConnectionError: %v", err)
	}
	if err := RecordConnectionOK(ctx, pool, "github", "acme", ok2); err != nil {
		t.Fatalf("RecordConnectionOK again: %v", err)
	}
	if err := RecordConnectionError(ctx, pool, "github", "acme", "RequestFailed", failed.Add(time.Second)); err != nil {
		t.Fatalf("RecordConnectionError again: %v", err)
	}
	if err := RecordConnectionOK(ctx, pool, "aws", "111122223333", ok1); err != nil {
		t.Fatalf("RecordConnectionOK aws: %v", err)
	}
	if err := RecordConnectionOK(ctx, pool, "github", "beta", ok1); err != nil {
		t.Fatalf("RecordConnectionOK beta: %v", err)
	}

	checks, err := ListConnectionChecks(ctx, pool)
	if err != nil {
		t.Fatalf("ListConnectionChecks: %v", err)
	}
	// Ordered by kind, then ref.
	want := []ConnectionCheck{
		{Kind: "aws", Ref: "111122223333", LastOKAt: &ok1},
		{Kind: "github", Ref: "acme", LastOKAt: &ok2, LastErrorAt: ptrTime(failed.Add(time.Second)), LastErrorCode: "RequestFailed"},
		{Kind: "github", Ref: "beta", LastOKAt: &ok1},
	}
	if len(checks) != len(want) {
		t.Fatalf("ListConnectionChecks = %+v, want %d rows", checks, len(want))
	}
	for i := range want {
		got := checks[i]
		if got.Kind != want[i].Kind || got.Ref != want[i].Ref || got.LastErrorCode != want[i].LastErrorCode ||
			!sameOptionalTime(got.LastOKAt, want[i].LastOKAt) || !sameOptionalTime(got.LastErrorAt, want[i].LastErrorAt) {
			t.Fatalf("row %d = %+v, want %+v", i, got, want[i])
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
