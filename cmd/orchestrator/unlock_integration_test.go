//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rcss-labs/yamdrake/internal/api"
	"github.com/rcss-labs/yamdrake/internal/cache"
	"github.com/rcss-labs/yamdrake/internal/store"
)

func runUnlock(t *testing.T, env map[string]string, args ...string) adminRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"yumdrake-orchestrator", "unlock"}, args...), envFrom(env), nil, &stdout, &stderr)
	return adminRun{code, stdout.String(), stderr.String()}
}

func TestUnlockClearsOnlyThatEmailsFailureCounterAndAuditsIt(t *testing.T) {
	env, dbURL := adminEnv(t)
	email := uniqueEmail(t)
	if got := runCreateAdmin(t, env, strings.NewReader(adminPassword+"\n"), "--email", email, "--name", "Ada"); got.code != 0 {
		t.Fatalf("create-admin = %d (stderr %q)", got.code, got.stderr)
	}
	other := uniqueEmail(t)
	valkeyClient, err := cache.Open(cache.Options{Addr: valkeyAddr(t)})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(valkeyClient.Close)
	ctx := context.Background()
	prefix := env["YUMDRAKE_VALKEY_KEY_PREFIX"]
	setCounter := func(address string) string {
		key := api.LoginFailureKey(prefix, address)
		if err := valkeyClient.Do(ctx, valkeyClient.B().Set().Key(key).Value("6").Ex(time.Minute).Build()).Error(); err != nil {
			t.Fatalf("set counter: %v", err)
		}
		return key
	}
	exists := func(key string) bool {
		n, err := valkeyClient.Do(ctx, valkeyClient.B().Exists().Key(key).Build()).AsInt64()
		if err != nil {
			t.Fatalf("exists: %v", err)
		}
		return n == 1
	}
	lockedKey, otherKey := setCounter(email), setCounter(other)

	// The bypass to try: the address written differently from how it was stored.
	got := runUnlock(t, env, "--email", "  "+strings.ToUpper(email)+" ")
	if got.code != 0 || got.stdout != "unlocked\n" || got.stderr != "" {
		t.Fatalf("unlock = %d, stdout %q, stderr %q; want 0 and \"unlocked\\n\"", got.code, got.stdout, got.stderr)
	}
	if exists(lockedKey) {
		t.Error("the failure counter of the email is still there")
	}
	if !exists(otherKey) {
		t.Error("the failure counter of another email was cleared")
	}

	entries, err := store.ListAudit(ctx, openPool(t, dbURL), 0, 20)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	var audited bool
	for _, e := range entries {
		if e.Action == "auth.unlock" {
			audited = strings.Contains(string(e.Detail), email) && e.TargetType == "user" && e.Actor == "system"
		}
	}
	if !audited {
		t.Errorf("no auth.unlock audit entry naming the email: %+v", entries)
	}
}

func TestUnlockRefusesWhatItCannotDo(t *testing.T) {
	env, dbURL := adminEnv(t)
	// An email that is nobody's: exit 1, nothing audited.
	got := runUnlock(t, env, "--email", "nobody@example.test")
	if got.code != 1 || got.stdout != "" || !strings.Contains(got.stderr, "no user") {
		t.Errorf("unlock of an unknown email = %d, stdout %q, stderr %q; want 1 and \"no user\"", got.code, got.stdout, got.stderr)
	}
	entries, _ := store.ListAudit(context.Background(), openPool(t, dbURL), 0, 20)
	for _, e := range entries {
		if e.Action == "auth.unlock" {
			t.Errorf("an audit entry was written for an unknown email")
		}
	}
	for name, args := range map[string][]string{
		"no email": {}, "empty email": {"--email", " "}, "stray argument": {"--email", "a@example.test", "extra"}, "unknown flag": {"--password", "x"},
	} {
		if got := runUnlock(t, env, args...); got.code != 2 || got.stdout != "" || !strings.Contains(got.stderr, "usage") {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 2 and usage text", name, got.code, got.stdout, got.stderr)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"yumdrake-orchestrator", "unlock", "--email", "a@example.test"}, envFrom(nil), nil, &stdout, &stderr); code != 2 {
		t.Errorf("unlock without configuration = %d, want 2", code)
	}
}
