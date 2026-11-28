//go:build integration

package session

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/cache"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Now().UTC().Truncate(time.Second)}
}

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

// randomUserID avoids collisions with other tests sharing the Valkey instance.
func randomUserID(t *testing.T) int64 {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 2)
}

func newManager(t *testing.T) (*Manager, *fakeClock) {
	t.Helper()
	clock := newClock()
	return NewManager(openClient(t, valkeyAddr(t)), clock.Now), clock
}

func TestCreateGetDelete(t *testing.T) {
	m, clock := newManager(t)
	ctx := context.Background()
	user := randomUserID(t)

	created, err := m.Create(ctx, user)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(created.ID) != 43 || created.UserID != user || !created.CreatedAt.Equal(clock.Now()) {
		t.Fatalf("unexpected created session: userID=%d idLen=%d", created.UserID, len(created.ID))
	}
	other, err := m.Create(ctx, user)
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if other.ID == created.ID {
		t.Fatal("two sessions share an id")
	}

	clock.Advance(time.Hour)
	got, err := m.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != user || !got.CreatedAt.Equal(created.CreatedAt) {
		t.Fatal("Get returned a different session")
	}
	if !got.LastSeenAt.Equal(clock.Now()) {
		t.Fatalf("LastSeenAt = %v, want refreshed to %v", got.LastSeenAt, clock.Now())
	}

	if err := m.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.Get(ctx, created.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Get after Delete = %v, want ErrNoSession", err)
	}
	if err := m.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete of absent session: %v", err)
	}
	if _, err := m.Get(ctx, other.ID); err != nil {
		t.Fatalf("other session must survive: %v", err)
	}
}

func TestGetMalformedIDIsNoSessionWithoutValkey(t *testing.T) {
	// A client for a closed port: any Valkey call would give ErrUnavailable
	// (or hang until the deadline), so ErrNoSession proves none was made.
	m := NewManager(openClient(t, "127.0.0.1:1"), time.Now)
	ids := map[string]string{
		"empty":                   "",
		"too short":               "short",
		"43 chars, not base64url": validLookingID()[:42] + "/",
	}
	for name, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := m.Get(ctx, id)
		cancel()
		if !errors.Is(err, ErrNoSession) {
			t.Fatalf("%s: Get = %v, want ErrNoSession", name, err)
		}
	}
}

func TestIdleTimeoutExpires(t *testing.T) {
	m, clock := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, randomUserID(t))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(7*time.Hour + 59*time.Minute)
	if _, err := m.Get(ctx, s.ID); err != nil {
		t.Fatalf("Get just inside idle timeout: %v", err)
	}
	// Activity at 7h59m resets the idle window, so 8h1m after that is expired.
	clock.Advance(8*time.Hour + time.Minute)
	if _, err := m.Get(ctx, s.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Get after idle timeout = %v, want ErrNoSession", err)
	}
}

func TestIdleTimeoutSetsKeyExpiry(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, randomUserID(t))
	if err != nil {
		t.Fatal(err)
	}
	ttl, err := m.client.Do(ctx, m.client.B().Pttl().Key(m.sessionKey(s.ID)).Build()).AsInt64()
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Duration(ttl) * time.Millisecond; d <= 7*time.Hour+55*time.Minute || d > IdleTimeout {
		t.Fatalf("key TTL = %v, want about the 8h idle timeout", d)
	}
}

func TestMaximumAgeExpiresDespiteActivity(t *testing.T) {
	m, clock := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, randomUserID(t))
	if err != nil {
		t.Fatal(err)
	}
	for h := 1; h <= 23; h++ {
		clock.Advance(time.Hour)
		if _, err := m.Get(ctx, s.ID); err != nil {
			t.Fatalf("Get at hour %d: %v", h, err)
		}
	}
	// The key's TTL must never reach past the maximum age.
	ttl, err := m.client.Do(ctx, m.client.B().Pttl().Key(m.sessionKey(s.ID)).Build()).AsInt64()
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Duration(ttl) * time.Millisecond; d > time.Hour {
		t.Fatalf("key TTL at hour 23 = %v, want at most the remaining hour", d)
	}
	clock.Advance(time.Hour + time.Second)
	if _, err := m.Get(ctx, s.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Get after 24h = %v, want ErrNoSession", err)
	}
}

func TestDeleteAllForUser(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	user, otherUser := randomUserID(t), randomUserID(t)

	var mine []Session
	for i := 0; i < 3; i++ {
		s, err := m.Create(ctx, user)
		if err != nil {
			t.Fatal(err)
		}
		mine = append(mine, s)
	}
	theirs, err := m.Create(ctx, otherUser)
	if err != nil {
		t.Fatal(err)
	}
	// One already-expired (deleted) id remains in the set; DeleteAllForUser must cope.
	if err := m.Delete(ctx, mine[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.client.Do(ctx, m.client.B().Sadd().Key(m.userSessionsKey(user)).Member(mine[0].ID, "not-a-real-id").Build()).ToInt64(); err != nil {
		t.Fatal(err)
	}

	if err := m.DeleteAllForUser(ctx, user); err != nil {
		t.Fatalf("DeleteAllForUser: %v", err)
	}
	for _, s := range mine {
		if _, err := m.Get(ctx, s.ID); !errors.Is(err, ErrNoSession) {
			t.Fatalf("session still valid after DeleteAllForUser: %v", err)
		}
	}
	if _, err := m.Get(ctx, theirs.ID); err != nil {
		t.Fatalf("another user's session must survive: %v", err)
	}
	exists, err := m.client.Do(ctx, m.client.B().Exists().Key(m.userSessionsKey(user)).Build()).AsInt64()
	if err != nil || exists != 0 {
		t.Fatalf("user set should be gone: exists=%d err=%v", exists, err)
	}
}

func TestDeleteAllForUserKeepsSessionCreatedDuringDeletion(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	user := randomUserID(t)
	if _, err := m.Create(ctx, user); err != nil {
		t.Fatal(err)
	}

	var late Session
	m.afterListingUserSessions = func() {
		var err error
		if late, err = m.Create(ctx, user); err != nil {
			t.Errorf("concurrent Create: %v", err)
		}
	}
	if err := m.DeleteAllForUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	m.afterListingUserSessions = nil

	// The session created mid-deletion may survive that call, but it must
	// still be findable by the next one.
	if err := m.DeleteAllForUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, late.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session created during DeleteAllForUser is orphaned: Get = %v", err)
	}
}

func TestUserSetHasExpiry(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	user := randomUserID(t)
	if _, err := m.Create(ctx, user); err != nil {
		t.Fatal(err)
	}
	ttl, err := m.client.Do(ctx, m.client.B().Pttl().Key(m.userSessionsKey(user)).Build()).AsInt64()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Fatalf("user-sessions set has no expiry (PTTL=%d)", ttl)
	}
}

func TestValkeyDownIsErrUnavailable(t *testing.T) {
	t.Run("never reachable", func(t *testing.T) {
		m := NewManager(openClient(t, "127.0.0.1:1"), time.Now)
		assertAllUnavailable(t, m, validLookingID())
	})

	t.Run("lost mid-run", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		proxy := startProxy(t, valkeyAddr(t))
		m := NewManager(openClient(t, proxy.addr), time.Now)
		s, err := m.Create(ctx, randomUserID(t))
		if err != nil {
			t.Fatalf("Create through proxy: %v", err)
		}
		proxy.stop()
		assertAllUnavailable(t, m, s.ID)
	})
}

func validLookingID() string {
	b := make([]byte, 43)
	for i := range b {
		b[i] = 'A'
	}
	return string(b)
}

// assertAllUnavailable gives each call its own short deadline: against a dead
// server valkey-go keeps redialling until the context ends.
func assertAllUnavailable(t *testing.T, m *Manager, id string) {
	t.Helper()
	calls := map[string]func(ctx context.Context) error{
		"Get":              func(ctx context.Context) error { _, err := m.Get(ctx, id); return err },
		"Create":           func(ctx context.Context) error { _, err := m.Create(ctx, 1); return err },
		"Delete":           func(ctx context.Context) error { return m.Delete(ctx, id) },
		"DeleteAllForUser": func(ctx context.Context) error { return m.DeleteAllForUser(ctx, 1) },
	}
	for name, call := range calls {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := call(ctx)
		cancel()
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s = %v, want ErrUnavailable", name, err)
		}
	}
}

// tcpProxy forwards to Valkey until stopped, then closes every connection and
// refuses new ones.
type tcpProxy struct {
	addr    string
	ln      net.Listener
	mu      sync.Mutex
	stopped bool
	conns   []net.Conn
}

func startProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			if !p.register(in, out) {
				in.Close()
				out.Close()
				return
			}
			go func() { _, _ = io.Copy(out, in); out.Close() }()
			go func() { _, _ = io.Copy(in, out); in.Close() }()
		}
	}()
	t.Cleanup(p.stop)
	return p
}

// register records the connections unless the proxy has already stopped.
func (p *tcpProxy) register(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return false
	}
	p.conns = append(p.conns, conns...)
	return true
}

func (p *tcpProxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	p.ln.Close()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

// Two services (or two test runs) that share one Valkey and number their users
// from 1 must not see each other's sessions: signing a user out everywhere in
// one must leave the same-numbered user of the other alone. This is what made
// concurrent end-to-end runs sign each other out.
func TestKeyPrefixKeepsSharedValkeyUsersApart(t *testing.T) {
	ctx := context.Background()
	client := openClient(t, valkeyAddr(t))
	suffix := randomUserID(t)
	mine := NewManager(client, time.Now, WithKeyPrefix(fmt.Sprintf("test-%d-a:", suffix)))
	other := NewManager(client, time.Now, WithKeyPrefix(fmt.Sprintf("test-%d-b:", suffix)))

	const sameUserID = 1
	keep, err := mine.Create(ctx, sameUserID)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := other.Create(ctx, sameUserID); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := other.DeleteAllForUser(ctx, sameUserID); err != nil {
		t.Fatalf("DeleteAllForUser: %v", err)
	}
	if _, err := mine.Get(ctx, keep.ID); err != nil {
		t.Fatalf("a session of another prefix was deleted: Get = %v", err)
	}
	if _, err := other.Get(ctx, keep.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a session is visible under another prefix: Get = %v", err)
	}
	if err := mine.DeleteAllForUser(ctx, sameUserID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
