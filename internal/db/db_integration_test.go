//go:build integration

package db

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

func TestMigrateAppliesAllAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbURL := testdb.New(t)

	if err := Migrate(ctx, dbURL); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	pool, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	version, err := SchemaVersion(ctx, pool)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != LatestVersion() {
		t.Fatalf("SchemaVersion = %d, want %d", version, LatestVersion())
	}
	var exists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.instance_settings') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatalf("check instance_settings: %v", err)
	}
	if !exists {
		t.Fatal("table instance_settings does not exist after Migrate")
	}

	if err := Migrate(ctx, dbURL); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	again, err := SchemaVersion(ctx, pool)
	if err != nil {
		t.Fatalf("SchemaVersion after second Migrate: %v", err)
	}
	if again != version {
		t.Fatalf("version changed from %d to %d on idempotent Migrate", version, again)
	}
}

func TestMigrateConcurrentCallersApplyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbURL := testdb.New(t)

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = Migrate(ctx, dbURL)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}

	pool, err := Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()
	// goose records its own version 0 bookkeeping row; count real migrations only.
	var rows, distinct int64
	err = pool.QueryRow(ctx,
		"SELECT count(*), count(DISTINCT version_id) FROM goose_db_version WHERE version_id > 0").Scan(&rows, &distinct)
	if err != nil {
		t.Fatalf("count goose rows: %v", err)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	want := int64(0)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".sql") {
			want++
		}
	}
	if rows != want || distinct != rows {
		t.Fatalf("goose_db_version has %d rows (%d distinct) with version_id > 0, want %d unique", rows, distinct, want)
	}
}

func TestSchemaVersionIsZeroOnFreshDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, err := Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	version, err := SchemaVersion(ctx, pool)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 0 {
		t.Fatalf("SchemaVersion = %d, want 0", version)
	}
	var created bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('goose_db_version') IS NOT NULL").Scan(&created); err != nil {
		t.Fatalf("check goose table: %v", err)
	}
	if created {
		t.Fatal("SchemaVersion created the goose version table")
	}
}

func TestWaitSchemaTimesOutNamingVersions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, err := Open(ctx, testdb.New(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	err = WaitSchema(ctx, pool, 2*time.Second)
	if err == nil {
		t.Fatal("WaitSchema returned nil on an unmigrated database")
	}
	for _, want := range []string{"0", strconv.FormatInt(LatestVersion(), 10)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention version %s", err, want)
		}
	}
}

func TestWaitOpenGivesUpWithRedactedURL(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	started := time.Now()
	pool, err := WaitOpen(context.Background(), "postgres://yumdrake:s3cret@127.0.0.1:1/none", 2*time.Second, logger)
	elapsed := time.Since(started)
	if pool != nil {
		pool.Close()
		t.Fatal("WaitOpen returned a pool for an unreachable database")
	}
	if err == nil {
		t.Fatal("WaitOpen returned nil error for an unreachable database")
	}
	if elapsed > 4*time.Second {
		t.Fatalf("WaitOpen took %s, want under 4s", elapsed)
	}
	if !strings.Contains(err.Error(), "2s") {
		t.Errorf("error %q does not mention the 2s wait", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the password: %q", err)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Errorf("log output leaks the password: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "REDACTED") {
		t.Errorf("log output does not show the redacted URL: %q", logs.String())
	}
}

func TestWaitOpenSucceedsWhenDatabaseAppearsLate(t *testing.T) {
	t.Parallel()
	realURL := testdb.New(t)
	parsed, err := url.Parse(realURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	upstream := parsed.Host

	// Reserve a free port, then release it so nothing listens there at first.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	proxyAddr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	parsed.Host = proxyAddr

	proxyReady := make(chan net.Listener, 1)
	go func() {
		time.Sleep(time.Second)
		ln, err := net.Listen("tcp", proxyAddr)
		if err != nil {
			proxyReady <- nil
			return
		}
		proxyReady <- ln
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go proxyConnection(client, upstream)
		}
	}()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	pool, err := WaitOpen(context.Background(), parsed.String(), 10*time.Second, logger)
	if ln := <-proxyReady; ln != nil {
		defer ln.Close()
	} else {
		t.Fatal("could not start the late proxy")
	}
	if err != nil {
		t.Fatalf("WaitOpen: %v", err)
	}
	defer pool.Close()
	if !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("no warn-level failed-attempt line before the proxy came up: %q", logs.String())
	}
	// The pool must stay usable after WaitOpen's per-attempt context is cancelled.
	if err := pool.Ping(context.Background()); err != nil {
		t.Errorf("pool unusable after WaitOpen returned: %v", err)
	}
}

func proxyConnection(client net.Conn, upstreamAddr string) {
	defer client.Close()
	upstream, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

func TestOpenAndMigrateErrorsNeverContainPassword(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const badURL = "postgres://yumdrake:s3cret@127.0.0.1:1/none"

	if _, err := Open(ctx, badURL); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("Open error = %v, want a non-nil error without the password", err)
	}
	if err := Migrate(ctx, badURL); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("Migrate error = %v, want a non-nil error without the password", err)
	}
}

func TestWaitOpenBoundsASingleHangingAttempt(t *testing.T) {
	t.Parallel()
	// A listener that accepts TCP connections and never answers the handshake.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()

	type result struct {
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		started := time.Now()
		pool, err := WaitOpen(context.Background(), "postgres://yumdrake:s3cret@"+ln.Addr().String()+"/none?sslmode=disable",
			2*time.Second, slog.New(slog.NewJSONHandler(io.Discard, nil)))
		if pool != nil {
			pool.Close()
		}
		done <- result{err, time.Since(started)}
	}()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatal("WaitOpen succeeded against a listener that never answers")
		}
		if res.elapsed > 4*time.Second {
			t.Errorf("WaitOpen took %s, want under 4s", res.elapsed)
		}
		if !strings.Contains(res.err.Error(), "2s") {
			t.Errorf("error %q does not mention the 2s wait", res.err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("WaitOpen still hanging after 8s with maxWait 2s: a single attempt is not bounded")
	}
}

// Characterization test: the Compose setup hands the database password to the
// services through PGPASSWORD instead of the URL, which relies on pgx reading
// it. Not parallel because it uses t.Setenv.
func TestOpenHonoursPGPASSWORD(t *testing.T) {
	withPassword := testdb.New(t)
	parsed, err := url.Parse(withPassword)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	password, ok := parsed.User.Password()
	if !ok || password == "" {
		t.Fatal("the test database URL carries no password, so PGPASSWORD cannot be proved to supply it")
	}
	parsed.User = url.User(parsed.User.Username())
	withoutPassword := parsed.String()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Setenv("PGPASSWORD", password)
	pool, err := Open(ctx, withoutPassword)
	if err != nil {
		t.Fatalf("Open with PGPASSWORD set: %v", err)
	}
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Errorf("SELECT 1 = %d, %v", one, err)
	}
	pool.Close()

	t.Setenv("PGPASSWORD", password+"-wrong")
	pool, err = Open(ctx, withoutPassword)
	if err == nil {
		pool.Close()
		t.Fatal("Open with a wrong PGPASSWORD succeeded; the server does not require the password")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "28P01" {
		t.Errorf("Open with a wrong PGPASSWORD: error = %v, want an authentication failure (28P01)", err)
	}
}
