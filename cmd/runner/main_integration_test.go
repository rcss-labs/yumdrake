//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/testutil/testdb"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// Development only: base64 of a fixed, public 32-byte key. Never use outside tests.
const devSecretKey = "ZGV2LW9ubHktaW5zZWN1cmUta2V5LTMyLWJ5dGVzISE="

func runnerEnv(t *testing.T, databaseURL, healthAddr, startupTimeout string) map[string]string {
	t.Helper()
	valkeyAddr := os.Getenv("YUMDRAKE_TEST_VALKEY_ADDR")
	if valkeyAddr == "" {
		t.Fatal("YUMDRAKE_TEST_VALKEY_ADDR must be set for integration tests")
	}
	return map[string]string{
		"YUMDRAKE_DATABASE_URL":     databaseURL,
		"YUMDRAKE_VALKEY_ADDR":      valkeyAddr,
		"YUMDRAKE_LOCAL_SECRET_KEY": devSecretKey,
		"YUMDRAKE_HEALTH_ADDR":      healthAddr,
		"YUMDRAKE_RUNNER_ID":        "runner-test",
		"YUMDRAKE_ARTIFACT_BUCKET":  "yumdrake-artifacts-test",
		"YUMDRAKE_SHUTDOWN_TIMEOUT": "5s",
		"YUMDRAKE_STARTUP_TIMEOUT":  startupTimeout,
	}
}

type running struct {
	done   chan int
	cancel context.CancelFunc
	stdout *syncBuffer
	once   sync.Once
	code   int
}

// start runs the runner in a goroutine; cleanup cancels it and waits for run to return.
func start(t *testing.T, env map[string]string) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{done: make(chan int, 1), cancel: cancel, stdout: &syncBuffer{}}
	go func() {
		r.done <- run(ctx, []string{"yumdrake-runner"}, envFrom(env), r.stdout, &syncBuffer{})
	}()
	t.Cleanup(func() {
		cancel()
		r.wait(t, 10*time.Second)
	})
	return r
}

// wait returns run's exit code (cached), failing the test if run does not return in time.
func (r *running) wait(t *testing.T, within time.Duration) int {
	t.Helper()
	r.once.Do(func() {
		select {
		case r.code = <-r.done:
		case <-time.After(within):
			r.code = -1
		}
	})
	if r.code == -1 {
		t.Fatalf("run did not return within %v", within)
	}
	return r.code
}

func status(addr, path string) int {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestRunnerWaitsForSchemaThenBecomesReady(t *testing.T) {
	dbURL := testdb.New(t)
	addr := freeAddr(t)
	env := runnerEnv(t, dbURL, addr, "30s")

	r := start(t, env)
	stdout := r.stdout

	time.Sleep(time.Second)
	if err := db.Migrate(context.Background(), dbURL); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for status(addr, "/readyz") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatalf("/readyz did not reach 200; stdout: %s", stdout.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), `"runner_id":"runner-test"`) {
		t.Errorf("starting line should carry runner_id: %s", stdout.String())
	}

	r.cancel()
	if code := r.wait(t, 10*time.Second); code != 0 {
		t.Errorf("run() = %d after cancel, want 0", code)
	}
}

func TestRunnerExits0WhenSignalledDuringSchemaWait(t *testing.T) {
	env := runnerEnv(t, testdb.New(t), freeAddr(t), "30s")
	r := start(t, env)

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.stdout.String(), "waiting for schema") {
		if time.Now().After(deadline) {
			t.Fatalf("runner never started waiting for the schema: %s", r.stdout.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.cancel()
	if code := r.wait(t, 10*time.Second); code != 0 {
		t.Errorf("run() = %d when signalled during start-up, want 0; stdout: %s", code, r.stdout.String())
	}
	if !strings.Contains(r.stdout.String(), "stopped during start-up") {
		t.Errorf("expected a 'stopped during start-up' line: %s", r.stdout.String())
	}
}

// registeredKinds builds the runner the way serve does and registers its executors.
func registeredKinds(t *testing.T, env map[string]string) map[string]bool {
	t.Helper()
	cfg, err := config.LoadRunner(envFrom(env))
	if err != nil {
		t.Fatalf("LoadRunner: %v", err)
	}
	pool, err := db.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newRunner(cfg, pool, nil, nil, log)
	if err := registerExecutors(context.Background(), cfg, pool, r, log); err != nil {
		t.Fatalf("registerExecutors: %v", err)
	}
	kinds := map[string]bool{}
	for kind, e := range r.Executors() {
		kinds[kind] = true
		if isFakeExecutor(e) {
			t.Errorf("kind %q is registered with a fake executor", kind)
		}
	}
	return kinds
}

// isFakeExecutor reports whether e is one of the executors of internal/steps/fake.
func isFakeExecutor(e runner.Executor) bool {
	typ := reflect.TypeOf(e)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	return strings.HasSuffix(typ.PkgPath(), "/internal/steps/fake")
}

func TestRunnerRegistersRealStepsByDefault(t *testing.T) {
	env := runnerEnv(t, testdb.New(t), freeAddr(t), "30s")
	kinds := registeredKinds(t, env)
	want := []string{"resolve", "revalidate", "copy-config", "ecs-deploy"}
	if len(kinds) != len(want) {
		t.Errorf("registered kinds = %v, want exactly %v", kinds, want)
	}
	for _, kind := range want {
		if !kinds[kind] {
			t.Errorf("kind %q is not registered", kind)
		}
	}
}

func TestFakeModeKeepsFakeDeploySteps(t *testing.T) {
	env := runnerEnv(t, testdb.New(t), freeAddr(t), "30s")
	env["YUMDRAKE_ENV"] = "dev"
	env["YUMDRAKE_FAKE_STEPS"] = "true"
	cfg, err := config.LoadRunner(envFrom(env))
	if err != nil {
		t.Fatalf("LoadRunner: %v", err)
	}
	pool, err := db.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := newRunner(cfg, pool, nil, nil, log)
	if err := registerExecutors(context.Background(), cfg, pool, r, log); err != nil {
		t.Fatalf("registerExecutors: %v", err)
	}
	executors := r.Executors()
	for _, kind := range []string{"copy-config", "ecs-deploy"} {
		if !isFakeExecutor(executors[kind]) {
			t.Errorf("kind %q should be the fake in dev fake mode", kind)
		}
	}
	for _, kind := range []string{"resolve", "revalidate"} {
		if isFakeExecutor(executors[kind]) {
			t.Errorf("kind %q should stay real in dev fake mode", kind)
		}
	}
}

func TestRunnerExits1WhenSchemaNeverArrivesAndDoesNotMigrate(t *testing.T) {
	dbURL := testdb.New(t)
	env := runnerEnv(t, dbURL, freeAddr(t), "2s")

	var stdout, stderr syncBuffer
	code := run(context.Background(), []string{"yumdrake-runner"}, envFrom(env), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want 1", code)
	}
	want := fmt.Sprintf("want at least %d", db.LatestVersion())
	if !strings.Contains(stdout.String(), "at version 0") || !strings.Contains(stdout.String(), want) {
		t.Errorf("log should name both versions (%q): %s", want, stdout.String())
	}

	pool, err := db.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer pool.Close()
	got, err := db.SchemaVersion(context.Background(), pool)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if got != 0 {
		t.Errorf("schema version = %d, runner must never migrate", got)
	}
}
