package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

const (
	validDatabaseURL = "postgres://u@db:5432/yumdrake"
	validValkeyAddr  = "valkey:6379"
	// base64 of 32 bytes of 0x2a; a test value only.
	validSecretKey = "KioqKioqKioqKioqKioqKioqKioqKioqKioqKioqKio=" //nolint:gosec // test value
)

func requiredEnv() map[string]string {
	return map[string]string{
		"YUMDRAKE_DATABASE_URL": validDatabaseURL,
		"YUMDRAKE_VALKEY_ADDR":  validValkeyAddr,

		"YUMDRAKE_LOCAL_SECRET_KEY": validSecretKey,
		"YUMDRAKE_PUBLIC_URL":       "https://yumdrake.example.com",

		"YUMDRAKE_ARTIFACT_BUCKET": "yumdrake-artifacts-test",
	}
}

func TestLoadRunnerArtifactStore(t *testing.T) {
	env := requiredEnv()
	cfg, err := LoadRunner(envFrom(env))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.ArtifactBucket != "yumdrake-artifacts-test" || cfg.ArtifactKMSKeyID != "" {
		t.Errorf("bucket, key = %q, %q", cfg.ArtifactBucket, cfg.ArtifactKMSKeyID)
	}
	env["YUMDRAKE_ARTIFACT_KMS_KEY_ID"] = "alias/yumdrake-artifacts"
	cfg, err = LoadRunner(envFrom(env))
	if err != nil || cfg.ArtifactKMSKeyID != "alias/yumdrake-artifacts" {
		t.Errorf("kms key = %q, err = %v", cfg.ArtifactKMSKeyID, err)
	}

	delete(env, "YUMDRAKE_ARTIFACT_BUCKET")
	if _, err := LoadRunner(envFrom(env)); err == nil || !strings.Contains(err.Error(), "YUMDRAKE_ARTIFACT_BUCKET") {
		t.Errorf("missing bucket error = %v", err)
	}
	for _, bad := range []string{"ab", "Upper-Case", "has_underscore", "-leading", "trailing-", "a/b", "has space", "double..dot", strings.Repeat("a", 64)} {
		env["YUMDRAKE_ARTIFACT_BUCKET"] = bad
		if _, err := LoadRunner(envFrom(env)); err == nil {
			t.Errorf("bucket %q accepted", bad)
		}
	}
	env["YUMDRAKE_ARTIFACT_BUCKET"] = "yumdrake-artifacts-test"
	env["YUMDRAKE_ARTIFACT_KMS_KEY_ID"] = "has space"
	if _, err := LoadRunner(envFrom(env)); err == nil {
		t.Error("kms key id with a space accepted")
	}
}

func TestLoadRunnerGitHubAPIURL(t *testing.T) {
	env := requiredEnv()
	cfg, err := LoadRunner(envFrom(env))
	if err != nil || cfg.GitHubAPIURL != "https://api.github.com" {
		t.Fatalf("default GitHub API URL = %q, err = %v", cfg.GitHubAPIURL, err)
	}
	env["YUMDRAKE_GITHUB_API_URL"] = "https://github.example.com/api/v3"
	cfg, err = LoadRunner(envFrom(env))
	if err != nil || cfg.GitHubAPIURL != "https://github.example.com/api/v3" {
		t.Errorf("GitHub API URL = %q, err = %v", cfg.GitHubAPIURL, err)
	}
	env["YUMDRAKE_GITHUB_API_URL"] = "ftp://nope"
	if _, err := LoadRunner(envFrom(env)); err == nil || !strings.Contains(err.Error(), "YUMDRAKE_GITHUB_API_URL") {
		t.Errorf("bad GitHub API URL error = %v", err)
	}
}

func TestLoadOrchestratorDefaults(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	if cfg.DatabaseURL != validDatabaseURL || cfg.ValkeyAddr != validValkeyAddr {
		t.Errorf("required values not carried over: %+v", cfg.Common)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 20*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 20s", cfg.ShutdownTimeout)
	}
	if cfg.StartupTimeout != 60*time.Second {
		t.Errorf("StartupTimeout = %v, want 60s", cfg.StartupTimeout)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if !cfg.MigrateOnStart {
		t.Error("MigrateOnStart = false, want true")
	}
	if cfg.MigrateTimeout != 10*time.Minute {
		t.Errorf("MigrateTimeout = %v, want 10m", cfg.MigrateTimeout)
	}
	if cfg.TokenSSOMaxAge != 168*time.Hour {
		t.Errorf("TokenSSOMaxAge = %v, want 168h", cfg.TokenSSOMaxAge)
	}
}

func TestTokenSSOMaxAge(t *testing.T) {
	for value, want := range map[string]time.Duration{"1h": time.Hour, "72h": 72 * time.Hour, "720h": 720 * time.Hour} {
		env := requiredEnv()
		env["YUMDRAKE_TOKEN_SSO_MAX_AGE"] = value
		cfg, err := LoadOrchestrator(envFrom(env))
		if err != nil || cfg.TokenSSOMaxAge != want {
			t.Errorf("YUMDRAKE_TOKEN_SSO_MAX_AGE=%s: %v, %v; want %v", value, cfg.TokenSSOMaxAge, err, want)
		}
	}
	for _, value := range []string{"59m", "30s", "0", "-5h", "soon"} {
		env := requiredEnv()
		env["YUMDRAKE_TOKEN_SSO_MAX_AGE"] = value
		_, err := LoadOrchestrator(envFrom(env))
		if err == nil || !strings.Contains(err.Error(), "YUMDRAKE_TOKEN_SSO_MAX_AGE") {
			t.Errorf("YUMDRAKE_TOKEN_SSO_MAX_AGE=%s: error %v, want one naming the variable", value, err)
		}
	}
}

func TestLoadOrchestratorMigrateTimeoutOverride(t *testing.T) {
	env := requiredEnv()
	env["YUMDRAKE_MIGRATE_TIMEOUT"] = "45m"
	cfg, err := LoadOrchestrator(envFrom(env))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	if cfg.MigrateTimeout != 45*time.Minute {
		t.Errorf("MigrateTimeout = %v, want 45m", cfg.MigrateTimeout)
	}
}

func TestLoadOrchestratorOverrides(t *testing.T) {
	env := map[string]string{
		"YUMDRAKE_DATABASE_URL":     "postgresql://user@db.example:5433/other?sslmode=require",
		"YUMDRAKE_VALKEY_ADDR":      "cache.example:6380",
		"YUMDRAKE_LOCAL_SECRET_KEY": validSecretKey,
		"YUMDRAKE_LOG_LEVEL":        "debug",
		"YUMDRAKE_SHUTDOWN_TIMEOUT": "5s",
		"YUMDRAKE_STARTUP_TIMEOUT":  "2m",
		"YUMDRAKE_HTTP_ADDR":        "0.0.0.0:9090",
		"YUMDRAKE_MIGRATE_ON_START": "false",
		"YUMDRAKE_PUBLIC_URL":       "http://localhost:8080",
	}
	cfg, err := LoadOrchestrator(envFrom(env))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	if cfg.DatabaseURL != env["YUMDRAKE_DATABASE_URL"] {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.ValkeyAddr != "cache.example:6380" {
		t.Errorf("ValkeyAddr = %q", cfg.ValkeyAddr)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
	if cfg.StartupTimeout != 2*time.Minute {
		t.Errorf("StartupTimeout = %v", cfg.StartupTimeout)
	}
	if cfg.HTTPAddr != "0.0.0.0:9090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.MigrateOnStart {
		t.Error("MigrateOnStart = true, want false")
	}
}

func TestLoadRunnerOverrides(t *testing.T) {
	env := requiredEnv()
	env["YUMDRAKE_HEALTH_ADDR"] = "0.0.0.0:9091"
	env["YUMDRAKE_RUNNER_ID"] = "runner-7"
	cfg, err := LoadRunner(envFrom(env))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.HealthAddr != "0.0.0.0:9091" || cfg.RunnerID != "runner-7" {
		t.Errorf("got HealthAddr=%q RunnerID=%q", cfg.HealthAddr, cfg.RunnerID)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	env := map[string]string{
		"YUMDRAKE_VALKEY_ADDR":      validValkeyAddr,
		"YUMDRAKE_LOCAL_SECRET_KEY": validSecretKey,
		"YUMDRAKE_LOG_LEVEL":        "loud",
		"YUMDRAKE_SHUTDOWN_TIMEOUT": "-1s",
	}
	_, err := LoadOrchestrator(envFrom(env))
	if err == nil {
		t.Fatal("LoadOrchestrator() error = nil, want error")
	}
	for _, name := range []string{"YUMDRAKE_DATABASE_URL", "YUMDRAKE_LOG_LEVEL", "YUMDRAKE_SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), "config: "+name+": ") {
			t.Errorf("error does not report %s: %v", name, err)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"YUMDRAKE_VALKEY_ADDR":      "no-port",
		"YUMDRAKE_STARTUP_TIMEOUT":  "0s",
		"YUMDRAKE_MIGRATE_TIMEOUT":  "soon",
		"YUMDRAKE_HTTP_ADDR":        "host:99999",
		"YUMDRAKE_MIGRATE_ON_START": "yes",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			env := requiredEnv()
			env[name] = value
			_, err := LoadOrchestrator(envFrom(env))
			if err == nil || !strings.Contains(err.Error(), "config: "+name+": ") {
				t.Errorf("error = %v, want a problem with %s", err, name)
			}
		})
	}
}

func TestLoadErrorNeverContainsDatabasePassword(t *testing.T) {
	env := requiredEnv()
	env["YUMDRAKE_DATABASE_URL"] = "postgres://u:s3cret@host/db?sslmode=bogus%zz"
	_, err := LoadOrchestrator(envFrom(env))
	if err == nil {
		t.Fatal("LoadOrchestrator() error = nil, want error")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "config: YUMDRAKE_DATABASE_URL: ") {
		t.Errorf("error does not name the variable: %v", err)
	}
}

func TestLoadRunnerDefaultsRunnerIDToHostname(t *testing.T) {
	want, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}
	cfg, err := LoadRunner(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.RunnerID != want {
		t.Errorf("RunnerID = %q, want %q", cfg.RunnerID, want)
	}
}

func TestRedactURL(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"password replaced", "postgres://u:s3cret@h:5432/db?sslmode=require", "postgres://u:REDACTED@h:5432/db?sslmode=require"},
		{"no password unchanged", "postgres://h:5432/db", "postgres://h:5432/db"},
		{"unparseable", "::not a url", "invalid URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedactURL(tc.in); got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactURLQueryPassword(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"query password", "postgres://u@h:5432/db?password=s3cret&sslmode=require", "postgres://u@h:5432/db?password=REDACTED&sslmode=require"},
		{"percent-encoded query password", "postgres://u@h/db?password=p%40ss%2Fword", "postgres://u@h/db?password=REDACTED"},
		{"percent-encoded user-info password", "postgres://u:p%40ss%2Fword@h/db", "postgres://u:REDACTED@h/db"},
		{"both forms", "postgres://u:one@h/db?password=two", "postgres://u:REDACTED@h/db?password=REDACTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestScrubRemovesEncodedAndPlainPassword(t *testing.T) {
	cases := []struct{ name, url, password string }{
		{"user-info", "postgres://u:p%40ss%2Fw%20rd@h:5432/db", "p@ss/w rd"},
		{"query", "postgres://u@h:5432/db?password=p%40ss%2Fw%20rd", "p@ss/w rd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := "dial failed for " + tc.url + "; saw " + tc.password +
				" and p%40ss%2Fw%20rd and p%40ss%2Fw+rd and p@ss%2Fw%20rd"
			got := Scrub(text, tc.url)
			for _, leaked := range []string{tc.password, "p%40ss%2Fw%20rd", "p%40ss%2Fw+rd", "p@ss%2Fw%20rd", "ss/w"} {
				if strings.Contains(got, leaked) {
					t.Errorf("Scrub() = %q, still contains %q", got, leaked)
				}
			}
			if !strings.Contains(got, "REDACTED") || !strings.Contains(got, "dial failed for") {
				t.Errorf("Scrub() = %q, want surrounding text kept and REDACTED present", got)
			}
		})
	}
	if got := Scrub("nothing secret", "postgres://u@h/db"); got != "nothing secret" {
		t.Errorf("Scrub() without a password = %q, want text unchanged", got)
	}
}

func TestLoadRunnerDefaultsHealthAddrToLoopback(t *testing.T) {
	cfg, err := LoadRunner(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.HealthAddr != "127.0.0.1:8081" {
		t.Errorf("HealthAddr = %q, want 127.0.0.1:8081 so the runner is not reachable from outside its task by default", cfg.HealthAddr)
	}
}

func TestValidateHostPort(t *testing.T) {
	for _, ok := range []string{":8080", "127.0.0.1:8081", "[::1]:9"} {
		if err := ValidateHostPort(ok); err != nil {
			t.Errorf("ValidateHostPort(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "nope", "host:0", "host:70000", "host:abc"} {
		if err := ValidateHostPort(bad); err == nil {
			t.Errorf("ValidateHostPort(%q) = nil, want an error", bad)
		}
	}
}

func TestSecretAndSyncDefaults(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	if cfg.SecretsMode != "local" {
		t.Errorf("SecretsMode = %q, want local", cfg.SecretsMode)
	}
	if got := cfg.LocalSecretKey.Reveal(); got != strings.Repeat("*", 32) {
		t.Errorf("LocalSecretKey did not decode to the 32 key bytes")
	}
	if cfg.SyncInterval != 60*time.Second {
		t.Errorf("SyncInterval = %v, want 60s", cfg.SyncInterval)
	}
	if cfg.GitHubAPIURL != "https://api.github.com" {
		t.Errorf("GitHubAPIURL = %q", cfg.GitHubAPIURL)
	}
	runner, err := LoadRunner(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if runner.SecretsMode != "local" || runner.LocalSecretKey.Reveal() != strings.Repeat("*", 32) {
		t.Error("runner does not load the secrets settings")
	}
}

func TestSecretAndSyncRules(t *testing.T) {
	tests := []struct {
		name    string
		set     map[string]string
		wantVar string // empty means valid
	}{
		{"key missing", map[string]string{"YUMDRAKE_LOCAL_SECRET_KEY": ""}, "YUMDRAKE_LOCAL_SECRET_KEY"},
		{"key not base64", map[string]string{"YUMDRAKE_LOCAL_SECRET_KEY": "not base64 !!"}, "YUMDRAKE_LOCAL_SECRET_KEY"},
		{"key wrong length", map[string]string{"YUMDRAKE_LOCAL_SECRET_KEY": "c2hvcnQ="}, "YUMDRAKE_LOCAL_SECRET_KEY"}, //nolint:gosec // test value
		{"mode unknown", map[string]string{"YUMDRAKE_SECRETS_MODE": "vault"}, "YUMDRAKE_SECRETS_MODE"},
		{"kms without key id", map[string]string{"YUMDRAKE_SECRETS_MODE": "kms", "YUMDRAKE_LOCAL_SECRET_KEY": ""}, "YUMDRAKE_KMS_KEY_ID"},
		{"kms key id with spaces", map[string]string{"YUMDRAKE_SECRETS_MODE": "kms", "YUMDRAKE_KMS_KEY_ID": "a b"}, "YUMDRAKE_KMS_KEY_ID"},
		{"kms needs no local key", map[string]string{"YUMDRAKE_SECRETS_MODE": "kms", "YUMDRAKE_KMS_KEY_ID": "alias/yumdrake", "YUMDRAKE_LOCAL_SECRET_KEY": ""}, ""},
		{"interval too short", map[string]string{"YUMDRAKE_SYNC_INTERVAL": "9s"}, "YUMDRAKE_SYNC_INTERVAL"},
		{"interval minimum", map[string]string{"YUMDRAKE_SYNC_INTERVAL": "10s"}, ""},
		{"interval not duration", map[string]string{"YUMDRAKE_SYNC_INTERVAL": "soon"}, "YUMDRAKE_SYNC_INTERVAL"},
		{"github url ftp", map[string]string{"YUMDRAKE_GITHUB_API_URL": "ftp://example.com"}, "YUMDRAKE_GITHUB_API_URL"},
		{"github url no host", map[string]string{"YUMDRAKE_GITHUB_API_URL": "http://"}, "YUMDRAKE_GITHUB_API_URL"},
		{"github url http", map[string]string{"YUMDRAKE_GITHUB_API_URL": "http://127.0.0.1:9999"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := requiredEnv()
			for k, v := range tc.set {
				env[k] = v
			}
			_, err := LoadOrchestrator(envFrom(env))
			if tc.wantVar == "" {
				if err != nil {
					t.Fatalf("error = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantVar) {
				t.Fatalf("error = %v, want it to name %s", err, tc.wantVar)
			}
		})
	}
}

func TestRunnerRejectsBadSecretKey(t *testing.T) {
	env := requiredEnv()
	env["YUMDRAKE_LOCAL_SECRET_KEY"] = "c2hvcnQ="
	if _, err := LoadRunner(envFrom(env)); err == nil {
		t.Fatal("LoadRunner() accepted a 5-byte key")
	}
}

func TestSecretsNeverAppearInErrorsOrFormatting(t *testing.T) {
	badKey := "c2VjcmV0LWtleS12YWx1ZS10b28tc2hvcnQ="
	env := requiredEnv()
	env["YUMDRAKE_LOCAL_SECRET_KEY"] = badKey
	env["YUMDRAKE_SYNC_INTERVAL"] = "bad"
	_, err := LoadOrchestrator(envFrom(env))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, secret := range []string{badKey, "secret-key-value"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error text contains a secret value: %q", err.Error())
		}
	}

	env = requiredEnv()
	cfg, err := LoadOrchestrator(envFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", "config", cfg, "key", cfg.LocalSecretKey)
	rendered := []string{
		buf.String(),
		fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg),
		fmt.Sprint(cfg.LocalSecretKey),
	}
	for _, text := range rendered {
		for _, secret := range []string{validSecretKey, strings.Repeat("*", 32)} {
			if strings.Contains(text, secret) {
				t.Errorf("formatted output leaks a secret: %q", text)
			}
		}
	}
}

func TestPublicURLRules(t *testing.T) {
	tests := []struct {
		name, value string
		wantErr     bool
		want        string
	}{
		{"missing", "", true, ""},
		{"https host", "https://yumdrake.example.com", false, "https://yumdrake.example.com"},
		{"https with port", "https://yumdrake.example.com:8443", false, "https://yumdrake.example.com:8443"},
		{"trailing slash dropped", "https://yumdrake.example.com/", false, "https://yumdrake.example.com"},
		{"http on a public host", "http://yumdrake.example.com", true, ""},
		{"http on localhost", "http://localhost:8080", false, "http://localhost:8080"},
		{"http on 127.0.0.1", "http://127.0.0.1:8080", false, "http://127.0.0.1:8080"},
		{"http on a lookalike host", "http://localhost.example.com", true, ""},
		{"other scheme", "ftp://localhost", true, ""},
		{"no host", "https://", true, ""},
		{"path", "https://yumdrake.example.com/app", true, ""},
		{"query", "https://yumdrake.example.com/?a=b", true, ""},
		{"user info", "https://user:pw@yumdrake.example.com", true, ""},
		{"not a url", "::::", true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := requiredEnv()
			env["YUMDRAKE_PUBLIC_URL"] = tc.value
			cfg, err := LoadOrchestrator(envFrom(env))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "YUMDRAKE_PUBLIC_URL") {
					t.Fatalf("error = %v, want it to name YUMDRAKE_PUBLIC_URL", err)
				}
				if tc.value != "" && strings.Contains(err.Error(), "pw@") {
					t.Errorf("error text echoes the user info: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want none", err)
			}
			if cfg.PublicURL != tc.want {
				t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, tc.want)
			}
		})
	}
}

func TestValkeyKeyPrefix(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil || cfg.ValkeyKeyPrefix != "" {
		t.Fatalf("default prefix = %q, %v; want empty", cfg.ValkeyKeyPrefix, err)
	}
	env := requiredEnv()
	env["YUMDRAKE_VALKEY_KEY_PREFIX"] = "blue:"
	if cfg, err = LoadOrchestrator(envFrom(env)); err != nil || cfg.ValkeyKeyPrefix != "blue:" {
		t.Fatalf("prefix = %q, %v; want blue:", cfg.ValkeyKeyPrefix, err)
	}
	for _, bad := range []string{"has space", "wild*", strings.Repeat("a", 65)} {
		env["YUMDRAKE_VALKEY_KEY_PREFIX"] = bad
		if _, err := LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), "YUMDRAKE_VALKEY_KEY_PREFIX") {
			t.Errorf("prefix %q: error = %v, want it to name the variable", bad, err)
		}
	}
}

func TestAWSEndpointURLRules(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantVar bool // true means the value is rejected
	}{
		{"unset", "", false},
		{"http", "http://localhost:4577", false},
		{"https", "https://aws.example.com", false},
		{"ftp", "ftp://example.com", true},
		{"no host", "http://", true},
		{"not a url", "::::", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := requiredEnv()
			env["YUMDRAKE_AWS_ENDPOINT_URL"] = tc.value
			orch, orchErr := LoadOrchestrator(envFrom(env))
			run, runErr := LoadRunner(envFrom(env))
			if tc.wantVar {
				for _, err := range []error{orchErr, runErr} {
					if err == nil || !strings.Contains(err.Error(), "YUMDRAKE_AWS_ENDPOINT_URL") {
						t.Fatalf("error = %v, want it to name YUMDRAKE_AWS_ENDPOINT_URL", err)
					}
				}
				return
			}
			if orchErr != nil || runErr != nil {
				t.Fatalf("errors = %v, %v", orchErr, runErr)
			}
			if orch.AWS.EndpointURL != tc.value || run.AWS.EndpointURL != tc.value {
				t.Errorf("AWS.EndpointURL = %q and %q, want %q", orch.AWS.EndpointURL, run.AWS.EndpointURL, tc.value)
			}
		})
	}
}

func TestFakeStepsRefusedOutsideDev(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantError string // substring; empty means the load must succeed
	}{
		{"prod with fakes", map[string]string{"YUMDRAKE_ENV": "prod", "YUMDRAKE_FAKE_STEPS": "true"}, "YUMDRAKE_FAKE_STEPS"},
		{"default env with fakes", map[string]string{"YUMDRAKE_FAKE_STEPS": "true"}, "YUMDRAKE_FAKE_STEPS"},
		{"dev with fakes", map[string]string{"YUMDRAKE_ENV": "dev", "YUMDRAKE_FAKE_STEPS": "true"}, ""},
		{"prod without fakes", map[string]string{"YUMDRAKE_ENV": "prod"}, ""},
		{"unknown env", map[string]string{"YUMDRAKE_ENV": "staging"}, "YUMDRAKE_ENV"},
		{"fake flag not a bool", map[string]string{"YUMDRAKE_ENV": "dev", "YUMDRAKE_FAKE_STEPS": "yes"}, "YUMDRAKE_FAKE_STEPS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := requiredEnv()
			for k, v := range tc.env {
				env[k] = v
			}
			cfg, err := LoadRunner(envFrom(env))
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("LoadRunner() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("LoadRunner() = %+v, %v; want an error naming %s", cfg, err, tc.wantError)
			}
		})
	}
}

func TestKMSModeCarriesTheKeyIDAndNeverEchoesBadValues(t *testing.T) {
	env := requiredEnv()
	delete(env, "YUMDRAKE_LOCAL_SECRET_KEY")
	env["YUMDRAKE_SECRETS_MODE"] = "kms"
	env["YUMDRAKE_KMS_KEY_ID"] = "arn:aws:kms:eu-west-1:123456789012:key/abcd"
	cfg, err := LoadOrchestrator(envFrom(env))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	if cfg.SecretsMode != "kms" || cfg.KMSKeyID != env["YUMDRAKE_KMS_KEY_ID"] {
		t.Errorf("SecretsMode = %q, KMSKeyID = %q", cfg.SecretsMode, cfg.KMSKeyID)
	}
	if runner, err := LoadRunner(envFrom(env)); err != nil || runner.KMSKeyID != cfg.KMSKeyID {
		t.Errorf("runner KMS settings = %q, %v", runner.KMSKeyID, err)
	}

	env["YUMDRAKE_SECRETS_MODE"] = "hunter2-mode"
	if _, err := LoadOrchestrator(envFrom(env)); err == nil || strings.Contains(err.Error(), "hunter2-mode") {
		t.Errorf("an unknown mode error = %v; it must exist and not echo the value", err)
	}
}

func TestLoadRunnerEngineDefaultsAndOverrides(t *testing.T) {
	cfg, err := LoadRunner(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.Env != "prod" || cfg.FakeSteps || cfg.Concurrency != 8 ||
		cfg.Lease != 30*time.Second || cfg.Heartbeat != 10*time.Second || cfg.Poll != 2*time.Second {
		t.Errorf("defaults = %+v", cfg)
	}
	env := requiredEnv()
	env["YUMDRAKE_RUNNER_CONCURRENCY"] = "3"
	env["YUMDRAKE_LEASE"] = "1m"
	env["YUMDRAKE_HEARTBEAT"] = "20s"
	env["YUMDRAKE_RUNNER_POLL"] = "500ms"
	cfg, err = LoadRunner(envFrom(env))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.Concurrency != 3 || cfg.Lease != time.Minute || cfg.Heartbeat != 20*time.Second || cfg.Poll != 500*time.Millisecond {
		t.Errorf("overrides = %+v", cfg)
	}
}

func TestLoadRunnerRolloutDefaultsAndOverrides(t *testing.T) {
	cfg, err := LoadRunner(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.RolloutPoll != 5*time.Second || cfg.RolloutTimeout != 15*time.Minute {
		t.Errorf("defaults: RolloutPoll=%v RolloutTimeout=%v, want 5s and 15m", cfg.RolloutPoll, cfg.RolloutTimeout)
	}
	env := requiredEnv()
	env["YUMDRAKE_ROLLOUT_POLL"] = "250ms"
	env["YUMDRAKE_ROLLOUT_TIMEOUT"] = "30m"
	cfg, err = LoadRunner(envFrom(env))
	if err != nil {
		t.Fatalf("LoadRunner() error = %v", err)
	}
	if cfg.RolloutPoll != 250*time.Millisecond || cfg.RolloutTimeout != 30*time.Minute {
		t.Errorf("overrides: RolloutPoll=%v RolloutTimeout=%v, want 250ms and 30m", cfg.RolloutPoll, cfg.RolloutTimeout)
	}
}

func TestLoadRunnerRefusesHeartbeatNotShorterThanLease(t *testing.T) {
	for _, heartbeat := range []string{"30s", "45s"} {
		env := requiredEnv()
		env["YUMDRAKE_HEARTBEAT"] = heartbeat // the default lease is 30s
		if _, err := LoadRunner(envFrom(env)); err == nil || !strings.Contains(err.Error(), "YUMDRAKE_HEARTBEAT") {
			t.Errorf("heartbeat %s: error = %v, want one naming YUMDRAKE_HEARTBEAT", heartbeat, err)
		}
	}
	env := requiredEnv()
	env["YUMDRAKE_RUNNER_CONCURRENCY"] = "0"
	if _, err := LoadRunner(envFrom(env)); err == nil || !strings.Contains(err.Error(), "YUMDRAKE_RUNNER_CONCURRENCY") {
		t.Errorf("concurrency 0: error = %v", err)
	}
}

func TestLoadOrchestratorLogArchiveDir(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil || cfg.LogArchiveDir != "" {
		t.Fatalf("default: dir = %q, err = %v; want archiving off", cfg.LogArchiveDir, err)
	}
	env := requiredEnv()
	env["YUMDRAKE_LOG_ARCHIVE_DIR"] = "/var/lib/yumdrake/logs"
	if cfg, err = LoadOrchestrator(envFrom(env)); err != nil || cfg.LogArchiveDir != "/var/lib/yumdrake/logs" {
		t.Fatalf("absolute: dir = %q, err = %v", cfg.LogArchiveDir, err)
	}
	env["YUMDRAKE_LOG_ARCHIVE_DIR"] = "relative/dir"
	if _, err = LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), "YUMDRAKE_LOG_ARCHIVE_DIR") {
		t.Fatalf("relative path: err = %v, want a complaint naming the variable", err)
	}
}

func TestLoadOrchestratorLogBucket(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil || cfg.LogBucket != "" {
		t.Fatalf("default: bucket = %q, err = %v; want none", cfg.LogBucket, err)
	}
	env := requiredEnv()
	env["YUMDRAKE_LOG_BUCKET"] = "yumdrake-logs"
	if cfg, err = LoadOrchestrator(envFrom(env)); err != nil || cfg.LogBucket != "yumdrake-logs" {
		t.Fatalf("bucket = %q, err = %v", cfg.LogBucket, err)
	}
	env["YUMDRAKE_LOG_BUCKET"] = "Not_A_Bucket"
	if _, err = LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), "YUMDRAKE_LOG_BUCKET") {
		t.Fatalf("invalid name: err = %v, want a complaint naming the variable", err)
	}
	env["YUMDRAKE_LOG_BUCKET"] = "yumdrake-logs"
	env["YUMDRAKE_LOG_ARCHIVE_DIR"] = "/var/lib/yumdrake/logs"
	if _, err = LoadOrchestrator(envFrom(env)); err == nil ||
		!strings.Contains(err.Error(), "YUMDRAKE_LOG_BUCKET") || !strings.Contains(err.Error(), "YUMDRAKE_LOG_ARCHIVE_DIR") {
		t.Fatalf("bucket and directory together: err = %v, want a complaint naming both variables", err)
	}
}

func TestOrchestratorEngineSettings(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil || cfg.SweepInterval != 2*time.Second || cfg.LimitPerEnvironment != 10 || cfg.LimitPerAccount != 20 {
		t.Fatalf("defaults = %v, %d, %d, %v; want 2s, 10, 20", cfg.SweepInterval, cfg.LimitPerEnvironment, cfg.LimitPerAccount, err)
	}
	env := requiredEnv()
	env["YUMDRAKE_SWEEP_INTERVAL"] = "500ms"
	env["YUMDRAKE_LIMIT_PER_ENVIRONMENT"] = "3"
	env["YUMDRAKE_LIMIT_PER_ACCOUNT"] = "6"
	if cfg, err = LoadOrchestrator(envFrom(env)); err != nil || cfg.SweepInterval != 500*time.Millisecond || cfg.LimitPerEnvironment != 3 || cfg.LimitPerAccount != 6 {
		t.Fatalf("overrides = %v, %d, %d, %v", cfg.SweepInterval, cfg.LimitPerEnvironment, cfg.LimitPerAccount, err)
	}
	for variable, bad := range map[string]string{"YUMDRAKE_SWEEP_INTERVAL": "0s", "YUMDRAKE_LIMIT_PER_ENVIRONMENT": "0", "YUMDRAKE_LIMIT_PER_ACCOUNT": "many"} {
		env := requiredEnv()
		env[variable] = bad
		if _, err := LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), variable) {
			t.Errorf("%s=%s: error = %v, want one naming the variable", variable, bad, err)
		}
	}
}

func TestRunnerReadsTheKeyPrefix(t *testing.T) {
	env := requiredEnv()
	env["YUMDRAKE_VALKEY_KEY_PREFIX"] = "blue:"
	if cfg, err := LoadRunner(envFrom(env)); err != nil || cfg.ValkeyKeyPrefix != "blue:" {
		t.Fatalf("prefix = %q, %v; want blue:", cfg.ValkeyKeyPrefix, err)
	}
}

func TestOrchestratorLogArchiveSchedule(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil || cfg.LogArchiveInterval != time.Minute || cfg.LogArchiveAfter != time.Hour {
		t.Fatalf("defaults = %v, %v, %v; want 1m and 1h", cfg.LogArchiveInterval, cfg.LogArchiveAfter, err)
	}
	env := requiredEnv()
	env["YUMDRAKE_LOG_ARCHIVE_INTERVAL"] = "5s"
	env["YUMDRAKE_LOG_ARCHIVE_AFTER"] = "30m"
	if cfg, err = LoadOrchestrator(envFrom(env)); err != nil || cfg.LogArchiveInterval != 5*time.Second || cfg.LogArchiveAfter != 30*time.Minute {
		t.Fatalf("overrides = %v, %v, %v", cfg.LogArchiveInterval, cfg.LogArchiveAfter, err)
	}
	for variable, bad := range map[string]string{
		"YUMDRAKE_LOG_ARCHIVE_INTERVAL": "0s", "YUMDRAKE_LOG_ARCHIVE_AFTER": "-1h",
	} {
		env := requiredEnv()
		env[variable] = bad
		if _, err := LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), variable) {
			t.Errorf("%s=%s: error = %v, want one naming the variable", variable, bad, err)
		}
	}
}

func TestSlackAndNotifySettings(t *testing.T) {
	cfg, err := LoadOrchestrator(envFrom(requiredEnv()))
	if err != nil {
		t.Fatalf("LoadOrchestrator() error = %v", err)
	}
	if cfg.SlackAPIURL != "https://slack.com/api" || cfg.NotifyInterval != 5*time.Second {
		t.Errorf("defaults: SlackAPIURL = %q, NotifyInterval = %v", cfg.SlackAPIURL, cfg.NotifyInterval)
	}

	env := requiredEnv()
	env["YUMDRAKE_SLACK_API_URL"] = "http://127.0.0.1:9999/api"
	env["YUMDRAKE_NOTIFY_INTERVAL"] = "30s"
	cfg, err = LoadOrchestrator(envFrom(env))
	if err != nil {
		t.Fatalf("LoadOrchestrator() with overrides: %v", err)
	}
	if cfg.SlackAPIURL != "http://127.0.0.1:9999/api" || cfg.NotifyInterval != 30*time.Second {
		t.Errorf("overrides: SlackAPIURL = %q, NotifyInterval = %v", cfg.SlackAPIURL, cfg.NotifyInterval)
	}

	for name, set := range map[string]map[string]string{
		"YUMDRAKE_SLACK_API_URL":   {"YUMDRAKE_SLACK_API_URL": "ftp://example.com"},
		"YUMDRAKE_NOTIFY_INTERVAL": {"YUMDRAKE_NOTIFY_INTERVAL": "0s"},
	} {
		env := requiredEnv()
		for k, v := range set {
			env[k] = v
		}
		if _, err := LoadOrchestrator(envFrom(env)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error = %v, want it to name the variable", name, err)
		}
	}
}
