//go:build integration

package cache

import (
	"context"
	"os"
	"testing"
	"time"
)

func valkeyTestAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("YUMDRAKE_TEST_VALKEY_ADDR")
	if addr == "" {
		t.Fatal("YUMDRAKE_TEST_VALKEY_ADDR must be set when running integration tests")
	}
	return addr
}

func TestPingSucceedsAgainstRunningValkey(t *testing.T) {
	client, err := Open(Options{Addr: valkeyTestAddr(t)})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Ping(ctx, client); err != nil {
		t.Fatalf("Ping against running Valkey: %v", err)
	}
}

func TestOpenDoesNotFailWhenValkeyIsDown(t *testing.T) {
	client, err := Open(Options{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Open with unreachable Valkey returned error: %v", err)
	}
	if client == nil {
		t.Fatal("Open with unreachable Valkey returned nil client")
	}
	client.Close()
}

func TestPingFailsWithinDeadlineWhenValkeyIsDown(t *testing.T) {
	client, err := Open(Options{Addr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	err = Ping(ctx, client)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Ping against unreachable Valkey returned nil, want error")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("Ping took %v against unreachable Valkey, want under 2s", elapsed)
	}
}
