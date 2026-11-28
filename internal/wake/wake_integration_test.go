//go:build integration

package wake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/cache"
)

func valkeyAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("YUMDRAKE_TEST_VALKEY_ADDR")
	if addr == "" {
		t.Fatal("YUMDRAKE_TEST_VALKEY_ADDR must be set when running integration tests")
	}
	return addr
}

func openClient(t *testing.T, addr string) valkey.Client {
	t.Helper()
	c, err := cache.Open(cache.Options{Addr: addr})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// randomPrefix keeps tests that share one Valkey apart.
func randomPrefix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "wake-test-" + hex.EncodeToString(b[:]) + ":"
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func receive(t *testing.T, ch <-chan int64, within time.Duration) (int64, bool) {
	t.Helper()
	select {
	case id, ok := <-ch:
		return id, ok
	case <-time.After(within):
		return 0, false
	}
}

// publishUntilReceived publishes repeatedly, because a subscriber that is still
// (re)subscribing misses wake-ups by design.
func publishUntilReceived(t *testing.T, bus *Bus, topic string, id int64, ch <-chan int64, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		bus.Publish(context.Background(), topic, id)
		if got, ok := receive(t, ch, 100*time.Millisecond); ok && got == id {
			return true
		}
	}
	return false
}

func TestPublishReachesSubscriber(t *testing.T) {
	prefix := randomPrefix(t)
	client := openClient(t, valkeyAddr(t))
	bus := New(client, quietLog(), WithKeyPrefix(prefix))
	other := New(client, quietLog(), WithKeyPrefix(randomPrefix(t)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := bus.Subscribe(ctx, TopicRuns)
	legs := bus.Subscribe(ctx, TopicLegs)
	otherRuns := other.Subscribe(ctx, TopicRuns)

	if !publishUntilReceived(t, bus, TopicRuns, 42, runs, 5*time.Second) {
		t.Fatal("subscriber did not receive the published run id")
	}
	if id, ok := receive(t, legs, 200*time.Millisecond); ok {
		t.Errorf("legs subscriber received %d for a runs wake-up", id)
	}
	// Drain what the retries above sent to the other bus's subscriber: it must have none.
	if id, ok := receive(t, otherRuns, 200*time.Millisecond); ok {
		t.Errorf("subscriber with another key prefix received %d", id)
	}

	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-runs:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("subscription channel was not closed after the context ended")
		}
	}
}

// cuttableProxy forwards to target until cut, closes every connection and
// refuses new ones, and accepts again on the same address after restore.
type cuttableProxy struct {
	addr, target string
	mu           sync.Mutex
	ln           net.Listener
	conns        []net.Conn
}

func startCuttableProxy(t *testing.T, target string) *cuttableProxy {
	t.Helper()
	p := &cuttableProxy{target: target}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.serve(ln)
	t.Cleanup(p.cut)
	return p
}

func (p *cuttableProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", p.target)
			if err != nil {
				in.Close()
				continue
			}
			p.mu.Lock()
			if p.ln != ln {
				p.mu.Unlock()
				in.Close()
				out.Close()
				return
			}
			p.conns = append(p.conns, in, out)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); out.Close() }()
			go func() { _, _ = io.Copy(in, out); in.Close() }()
		}
	}()
}

func (p *cuttableProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func (p *cuttableProxy) restore(t *testing.T) {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", p.addr); err == nil {
			p.serve(ln)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("could not listen on %s again: %v", p.addr, err)
}

func TestSubscriberSurvivesValkeyRestart(t *testing.T) {
	prefix := randomPrefix(t)
	proxy := startCuttableProxy(t, valkeyAddr(t))
	subscriberBus := New(openClient(t, proxy.addr), quietLog(), WithKeyPrefix(prefix))
	// The publisher goes straight to Valkey, so only the subscriber's connection is cut.
	publisherBus := New(openClient(t, valkeyAddr(t)), quietLog(), WithKeyPrefix(prefix))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := subscriberBus.Subscribe(ctx, TopicRuns)

	if !publishUntilReceived(t, publisherBus, TopicRuns, 1, ch, 5*time.Second) {
		t.Fatal("no wake-up before the restart")
	}
	proxy.cut()
	// While the connection is cut a wake-up must not arrive: this proves the
	// cut really broke the subscriber's connection, so the receive below
	// shows a recovery and not a connection that never dropped.
	time.Sleep(300 * time.Millisecond)
	publisherBus.Publish(context.Background(), TopicRuns, 99)
	if id, ok := receive(t, ch, 300*time.Millisecond); ok {
		t.Fatalf("received %d while the connection was cut", id)
	}
	proxy.restore(t)

	if !publishUntilReceived(t, publisherBus, TopicRuns, 2, ch, 15*time.Second) {
		t.Fatal("subscriber did not receive wake-ups after Valkey came back")
	}
}

func TestPublishWithValkeyDownIsANoOp(t *testing.T) {
	client := openClient(t, "127.0.0.1:1") // nothing listens there
	bus := New(client, quietLog())
	done := make(chan struct{})
	go func() {
		bus.Publish(context.Background(), TopicRuns, 7)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked with Valkey down")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := bus.Subscribe(ctx, TopicRuns)
	if id, ok := receive(t, ch, 300*time.Millisecond); ok {
		t.Errorf("received %d with Valkey down", id)
	}
	cancel()
}

// failingReceiveClient makes the first Receive calls fail, as a lost
// connection does when the client library does not recover by itself.
type failingReceiveClient struct {
	valkey.Client
	failures int32
	calls    atomic.Int32
}

func (c *failingReceiveClient) Receive(ctx context.Context, subscribe valkey.Completed, fn func(valkey.PubSubMessage)) error {
	if c.calls.Add(1) <= c.failures {
		return errors.New("injected: connection lost")
	}
	return c.Client.Receive(ctx, subscribe, fn)
}

func TestSubscribeResubscribesAfterReceiveFails(t *testing.T) {
	prefix := randomPrefix(t)
	direct := openClient(t, valkeyAddr(t))
	flaky := &failingReceiveClient{Client: direct, failures: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := New(flaky, quietLog(), WithKeyPrefix(prefix)).Subscribe(ctx, TopicRuns)

	if !publishUntilReceived(t, New(direct, quietLog(), WithKeyPrefix(prefix)), TopicRuns, 5, ch, 10*time.Second) {
		t.Fatal("no wake-up after Receive failed twice")
	}
	if got := flaky.calls.Load(); got < 3 {
		t.Errorf("Receive called %d times, want at least 3 (two failures, then a working subscription)", got)
	}
}
