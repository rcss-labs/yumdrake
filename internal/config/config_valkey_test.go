package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const testValkeyPassword = "valkey-pw-do-not-print" //nolint:gosec // test value

func loadBoth(t *testing.T, env map[string]string) (Orchestrator, Runner) {
	t.Helper()
	orch, err := LoadOrchestrator(envFrom(env))
	if err != nil {
		t.Fatalf("LoadOrchestrator: %v", err)
	}
	run, err := LoadRunner(envFrom(env))
	if err != nil {
		t.Fatalf("LoadRunner: %v", err)
	}
	return orch, run
}

func TestValkeyAuthDefaults(t *testing.T) {
	t.Parallel()
	orch, run := loadBoth(t, requiredEnv())
	for name, c := range map[string]Common{"orchestrator": orch.Common, "runner": run.Common} {
		if c.ValkeyPassword.Reveal() != "" {
			t.Errorf("%s: ValkeyPassword is set by default", name)
		}
		if c.ValkeyTLS {
			t.Errorf("%s: ValkeyTLS is true by default", name)
		}
	}
}

func TestValkeyAuthValuesCarriedOver(t *testing.T) {
	t.Parallel()
	env := requiredEnv()
	env["YUMDRAKE_VALKEY_PASSWORD"] = testValkeyPassword
	env["YUMDRAKE_VALKEY_TLS"] = "true"
	orch, run := loadBoth(t, env)
	for name, c := range map[string]Common{"orchestrator": orch.Common, "runner": run.Common} {
		if c.ValkeyPassword.Reveal() != testValkeyPassword {
			t.Errorf("%s: password not carried over", name)
		}
		if !c.ValkeyTLS {
			t.Errorf("%s: ValkeyTLS = false, want true", name)
		}
	}
}

func TestValkeyTLSRejectsOtherValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"yes", "1", "TRUE"} {
		env := requiredEnv()
		env["YUMDRAKE_VALKEY_TLS"] = value
		const want = `YUMDRAKE_VALKEY_TLS: must be "true" or "false"`
		if _, err := LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadOrchestrator with %q: error = %v, want %q", value, err, want)
		}
		if _, err := LoadRunner(envFrom(env)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("LoadRunner with %q: error = %v, want %q", value, err, want)
		}
	}
}

func TestValkeyPasswordNeverPrinted(t *testing.T) {
	t.Parallel()
	env := requiredEnv()
	env["YUMDRAKE_VALKEY_PASSWORD"] = testValkeyPassword
	orch, run := loadBoth(t, env)

	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("config", "orchestrator", orch, "runner", run, "common", orch.Common)

	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v %v", orch, run),
		"%+v":  fmt.Sprintf("%+v %+v", orch, run),
		"%#v":  fmt.Sprintf("%#v %#v", orch, run),
		"slog": logged.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, testValkeyPassword) {
			t.Errorf("%s output contains the Valkey password", name)
		}
	}
}

func TestPlaceholderPasswordsAreRefused(t *testing.T) {
	t.Parallel()
	cases := []struct{ variable, value string }{
		{"YUMDRAKE_VALKEY_PASSWORD", "<generated-password>"},
		{"YUMDRAKE_VALKEY_PASSWORD", "<anything>"},
		{"PGPASSWORD", "<generated-password>"},
	}
	for _, tc := range cases {
		env := requiredEnv()
		env[tc.variable] = tc.value
		if _, err := LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), tc.variable) {
			t.Errorf("LoadOrchestrator with %s=%s: error = %v, want one naming the variable", tc.variable, tc.value, err)
		}
		if _, err := LoadRunner(envFrom(env)); err == nil || !strings.Contains(err.Error(), tc.variable) {
			t.Errorf("LoadRunner with %s=%s: error = %v, want one naming the variable", tc.variable, tc.value, err)
		}
	}
	// A real password that merely contains angle brackets inside is fine.
	env := requiredEnv()
	env["YUMDRAKE_VALKEY_PASSWORD"] = "ab<c>d"
	if _, err := LoadOrchestrator(envFrom(env)); err != nil {
		t.Errorf("a password with brackets inside was refused: %v", err)
	}
}
