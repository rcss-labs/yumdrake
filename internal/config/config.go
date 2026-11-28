// Package config loads and validates the environment configuration of the
// Yumdrake orchestrator and runner.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rcss-labs/yamdrake/internal/awsx"
)

// Common holds the settings shared by both service binaries.
type Common struct {
	DatabaseURL     string
	ValkeyAddr      string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	StartupTimeout  time.Duration
	// AWS holds the deployment-wide AWS settings (YUMDRAKE_AWS_ENDPOINT_URL).
	AWS awsx.Options

	SecretSettings
}

// Orchestrator holds the settings of the orchestrator service.
type Orchestrator struct {
	Common
	HTTPAddr       string
	MigrateOnStart bool
	// MigrateTimeout bounds the schema migration, separately from StartupTimeout.
	MigrateTimeout time.Duration
	// PublicURL is the address users reach the service at (scheme, host and
	// optional port, no trailing slash). Cookie requests must come from it.
	PublicURL string
	// ValkeyKeyPrefix goes in front of the Valkey keys of sessions and sign-in
	// counters. Empty by default; set it to tell apart services (or test runs)
	// that share one Valkey.
	ValkeyKeyPrefix string
	// LogArchiveDir is the directory settled legs' logs are archived to. Empty
	// means no directory archive (see LogBucket).
	LogArchiveDir string
	// LogBucket (YUMDRAKE_LOG_BUCKET) is the S3 bucket settled legs' logs are
	// archived to instead of LogArchiveDir; setting both is a config error.
	LogBucket string
	// LogArchiveInterval is how often settled legs are archived, and
	// LogArchiveAfter how long a leg must have been settled first. Both apply
	// only when LogArchiveDir is set.
	LogArchiveInterval, LogArchiveAfter time.Duration
	// SweepInterval is how often the engine loop sweeps expired leases and
	// advances every active run, whether or not a wake-up arrived.
	SweepInterval time.Duration
	// SlackAPIURL is the Slack Web API base (YUMDRAKE_SLACK_API_URL, default
	// https://slack.com/api); tests point it at a fake. NotifyInterval is how
	// often the sender looks for due notifications (YUMDRAKE_NOTIFY_INTERVAL, 5s).
	SlackAPIURL    string
	NotifyInterval time.Duration
	// LimitPerEnvironment and LimitPerAccount cap the legs running at once.
	LimitPerEnvironment, LimitPerAccount int
	// TokenSSOMaxAge is how old the last SSO sign-in of a SAML user may be for
	// their CLI tokens to keep working (YUMDRAKE_TOKEN_SSO_MAX_AGE, default 168h,
	// at least 1h).
	TokenSSOMaxAge time.Duration

	SyncSettings
}

// Runner holds the settings of the runner service.
type Runner struct {
	Common
	HealthAddr string
	RunnerID   string

	// ArtifactBucket (YUMDRAKE_ARTIFACT_BUCKET, required) holds config copies;
	// ArtifactKMSKeyID (YUMDRAKE_ARTIFACT_KMS_KEY_ID, optional) selects KMS
	// encryption instead of S3-managed keys.
	ArtifactBucket   string
	ArtifactKMSKeyID string
	// GitHubAPIURL (YUMDRAKE_GITHUB_API_URL, default https://api.github.com)
	// is where resolving reads env files from.
	GitHubAPIURL string
	// ValkeyKeyPrefix goes in front of the Valkey channels; it must equal the
	// orchestrator's so that wake-ups reach each other.
	ValkeyKeyPrefix string
	// Env is "dev" or "prod". Fake steps are allowed only in dev.
	Env string
	// FakeSteps registers the fake step executors instead of real ones.
	FakeSteps bool
	// Concurrency is how many legs one runner executes at once.
	Concurrency int
	// Lease is how long a claimed leg stays with its runner without a
	// heartbeat. Heartbeat is how often the runner renews it (shorter than
	// Lease) and Poll how often it looks for queued legs on its own.
	Lease, Heartbeat, Poll time.Duration
	// RolloutPoll (YUMDRAKE_ROLLOUT_POLL, default 5s) is how often ecs-deploy
	// checks a service's rollout. RolloutTimeout (YUMDRAKE_ROLLOUT_TIMEOUT,
	// default 15m) is how long it waits before it reports the rollout unconfirmed.
	RolloutPoll, RolloutTimeout time.Duration
}

const redactedPassword = "REDACTED"

const minTokenSSOMaxAge = time.Hour

var (
	bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	kmsKeyIDPattern   = regexp.MustCompile(`^[A-Za-z0-9:/_+=,.@-]{1,1000}$`)
)

var keyPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9_:-]{0,64}$`)

// loader collects problems so that every one is reported at once.
type loader struct {
	getenv   func(string) string
	problems []error
}

func (l *loader) fail(variable, reason string) {
	l.problems = append(l.problems, fmt.Errorf("config: %s: %s", variable, reason))
}

func (l *loader) err() error {
	return errors.Join(l.problems...)
}

func (l *loader) loadCommon() Common {
	var c Common

	c.DatabaseURL = l.getenv("YUMDRAKE_DATABASE_URL")
	if reason := validateDatabaseURL(c.DatabaseURL); reason != "" {
		l.fail("YUMDRAKE_DATABASE_URL", reason)
	}

	c.ValkeyAddr = l.getenv("YUMDRAKE_VALKEY_ADDR")
	if c.ValkeyAddr == "" {
		l.fail("YUMDRAKE_VALKEY_ADDR", "is required")
	} else if reason := validateHostPort(c.ValkeyAddr); reason != "" {
		l.fail("YUMDRAKE_VALKEY_ADDR", reason)
	}

	c.LogLevel = l.logLevel("YUMDRAKE_LOG_LEVEL", "info")
	c.ShutdownTimeout = l.duration("YUMDRAKE_SHUTDOWN_TIMEOUT", "20s")
	c.StartupTimeout = l.duration("YUMDRAKE_STARTUP_TIMEOUT", "60s")
	c.SecretSettings = l.loadSecretSettings()

	c.AWS.EndpointURL = l.getenv("YUMDRAKE_AWS_ENDPOINT_URL")
	if c.AWS.EndpointURL != "" {
		if reason := validateHTTPURL(c.AWS.EndpointURL); reason != "" {
			l.fail("YUMDRAKE_AWS_ENDPOINT_URL", reason)
		}
	}
	return c
}

// LoadOrchestrator reads the orchestrator configuration from getenv.
func LoadOrchestrator(getenv func(string) string) (Orchestrator, error) {
	l := &loader{getenv: getenv}
	var cfg Orchestrator
	cfg.Common = l.loadCommon()
	cfg.HTTPAddr = l.hostPort("YUMDRAKE_HTTP_ADDR", ":8080")

	cfg.MigrateTimeout = l.duration("YUMDRAKE_MIGRATE_TIMEOUT", "10m")

	switch raw := l.valueOr("YUMDRAKE_MIGRATE_ON_START", "true"); raw {
	case "true":
		cfg.MigrateOnStart = true
	case "false":
		cfg.MigrateOnStart = false
	default:
		l.fail("YUMDRAKE_MIGRATE_ON_START", `must be "true" or "false"`)
	}

	cfg.PublicURL = l.publicURL("YUMDRAKE_PUBLIC_URL")
	cfg.ValkeyKeyPrefix = l.keyPrefix()
	cfg.LogArchiveDir = l.getenv("YUMDRAKE_LOG_ARCHIVE_DIR")
	if cfg.LogArchiveDir != "" && !filepath.IsAbs(cfg.LogArchiveDir) {
		l.fail("YUMDRAKE_LOG_ARCHIVE_DIR", "must be an absolute path")
	}
	cfg.LogBucket = l.getenv("YUMDRAKE_LOG_BUCKET")
	if cfg.LogBucket != "" {
		if !bucketNamePattern.MatchString(cfg.LogBucket) || strings.Contains(cfg.LogBucket, "..") {
			l.fail("YUMDRAKE_LOG_BUCKET", "must be a valid S3 bucket name (3 to 63 lowercase letters, digits, '-' or '.')")
		}
		if cfg.LogArchiveDir != "" {
			l.fail("YUMDRAKE_LOG_BUCKET", "cannot be combined with YUMDRAKE_LOG_ARCHIVE_DIR; set only one of them")
		}
	}
	cfg.LogArchiveInterval = l.duration("YUMDRAKE_LOG_ARCHIVE_INTERVAL", "1m")
	cfg.LogArchiveAfter = l.duration("YUMDRAKE_LOG_ARCHIVE_AFTER", "1h")
	cfg.SweepInterval = l.duration("YUMDRAKE_SWEEP_INTERVAL", "2s")
	cfg.SlackAPIURL = l.valueOr("YUMDRAKE_SLACK_API_URL", "https://slack.com/api")
	if reason := validateHTTPURL(cfg.SlackAPIURL); reason != "" {
		l.fail("YUMDRAKE_SLACK_API_URL", reason)
	}
	cfg.NotifyInterval = l.duration("YUMDRAKE_NOTIFY_INTERVAL", "5s")
	cfg.LimitPerEnvironment = l.positiveInt("YUMDRAKE_LIMIT_PER_ENVIRONMENT", "10")
	cfg.LimitPerAccount = l.positiveInt("YUMDRAKE_LIMIT_PER_ACCOUNT", "20")
	cfg.TokenSSOMaxAge = l.duration("YUMDRAKE_TOKEN_SSO_MAX_AGE", "168h")
	if cfg.TokenSSOMaxAge > 0 && cfg.TokenSSOMaxAge < minTokenSSOMaxAge {
		l.fail("YUMDRAKE_TOKEN_SSO_MAX_AGE", "must be at least 1h")
	}
	cfg.SyncSettings = l.loadSyncSettings()

	if err := l.err(); err != nil {
		return Orchestrator{}, err
	}
	return cfg, nil
}

// LoadRunner reads the runner configuration from getenv.
func LoadRunner(getenv func(string) string) (Runner, error) {
	l := &loader{getenv: getenv}
	var cfg Runner
	cfg.Common = l.loadCommon()
	cfg.HealthAddr = l.hostPort("YUMDRAKE_HEALTH_ADDR", "127.0.0.1:8081")
	cfg.ValkeyKeyPrefix = l.keyPrefix()

	cfg.RunnerID = getenv("YUMDRAKE_RUNNER_ID")
	if cfg.RunnerID == "" {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			l.fail("YUMDRAKE_RUNNER_ID", "is not set and the host name could not be determined")
		}
		cfg.RunnerID = hostname
	}

	cfg.ArtifactBucket = l.getenv("YUMDRAKE_ARTIFACT_BUCKET")
	if cfg.ArtifactBucket == "" {
		l.fail("YUMDRAKE_ARTIFACT_BUCKET", "is required")
	} else if !bucketNamePattern.MatchString(cfg.ArtifactBucket) || strings.Contains(cfg.ArtifactBucket, "..") {
		l.fail("YUMDRAKE_ARTIFACT_BUCKET", "must be a valid S3 bucket name (3 to 63 lowercase letters, digits, '-' or '.')")
	}
	cfg.ArtifactKMSKeyID = l.getenv("YUMDRAKE_ARTIFACT_KMS_KEY_ID")
	if cfg.ArtifactKMSKeyID != "" && !kmsKeyIDPattern.MatchString(cfg.ArtifactKMSKeyID) {
		l.fail("YUMDRAKE_ARTIFACT_KMS_KEY_ID", "must be a KMS key id, key ARN or alias without spaces")
	}
	cfg.GitHubAPIURL = l.githubAPIURL()
	switch cfg.Env = l.valueOr("YUMDRAKE_ENV", "prod"); cfg.Env {
	case "dev", "prod":
	default:
		l.fail("YUMDRAKE_ENV", `must be "dev" or "prod"`)
	}
	switch raw := l.valueOr("YUMDRAKE_FAKE_STEPS", "false"); raw {
	case "true":
		cfg.FakeSteps = true
	case "false":
	default:
		l.fail("YUMDRAKE_FAKE_STEPS", `must be "true" or "false"`)
	}
	if cfg.FakeSteps && cfg.Env != "dev" {
		l.fail("YUMDRAKE_FAKE_STEPS", `is allowed only with YUMDRAKE_ENV=dev`)
	}
	cfg.Concurrency = l.positiveInt("YUMDRAKE_RUNNER_CONCURRENCY", "8")
	cfg.Lease = l.duration("YUMDRAKE_LEASE", "30s")
	cfg.Heartbeat = l.duration("YUMDRAKE_HEARTBEAT", "10s")
	cfg.Poll = l.duration("YUMDRAKE_RUNNER_POLL", "2s")
	cfg.RolloutPoll = l.duration("YUMDRAKE_ROLLOUT_POLL", "5s")
	cfg.RolloutTimeout = l.duration("YUMDRAKE_ROLLOUT_TIMEOUT", "15m")
	if cfg.Lease > 0 && cfg.Heartbeat >= cfg.Lease {
		l.fail("YUMDRAKE_HEARTBEAT", "must be shorter than YUMDRAKE_LEASE")
	}

	if err := l.err(); err != nil {
		return Runner{}, err
	}
	return cfg, nil
}

// publicURL reads the address users reach the service at. It must be https,
// except for http on localhost or 127.0.0.1 (development), and consist of
// scheme, host and optional port only. A trailing slash is dropped. Messages
// never echo the value because it could carry user info.
func (l *loader) publicURL(variable string) string {
	raw := l.getenv(variable)
	if raw == "" {
		l.fail(variable, "is required")
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		l.fail(variable, "must be a URL such as https://yumdrake.example.com")
		return ""
	}
	loopback := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
	switch {
	case u.Scheme != "https" && (u.Scheme != "http" || !loopback):
		l.fail(variable, "must use https:// (http:// is allowed only for localhost and 127.0.0.1)")
		return ""
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		l.fail(variable, "must be only a scheme, host and optional port")
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (l *loader) valueOr(variable, fallback string) string {
	if v := l.getenv(variable); v != "" {
		return v
	}
	return fallback
}

func (l *loader) logLevel(variable, fallback string) slog.Level {
	switch l.valueOr(variable, fallback) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	l.fail(variable, "must be one of debug, info, warn, error")
	return slog.LevelInfo
}

func (l *loader) duration(variable, fallback string) time.Duration {
	d, err := time.ParseDuration(l.valueOr(variable, fallback))
	if err != nil {
		l.fail(variable, "must be a Go duration such as 20s")
		return 0
	}
	if d <= 0 {
		l.fail(variable, "must be greater than zero")
		return 0
	}
	return d
}

// keyPrefix reads YUMDRAKE_VALKEY_KEY_PREFIX, shared by both services.
func (l *loader) keyPrefix() string {
	prefix := l.getenv("YUMDRAKE_VALKEY_KEY_PREFIX")
	if !keyPrefixPattern.MatchString(prefix) {
		l.fail("YUMDRAKE_VALKEY_KEY_PREFIX", "must be at most 64 letters, digits, '-', '_' or ':'")
	}
	return prefix
}

func (l *loader) positiveInt(variable, fallback string) int {
	n, err := strconv.Atoi(l.valueOr(variable, fallback))
	if err != nil || n < 1 {
		l.fail(variable, "must be a whole number of at least 1")
		return 0
	}
	return n
}

func (l *loader) hostPort(variable, fallback string) string {
	value := l.valueOr(variable, fallback)
	if reason := validateHostPort(value); reason != "" {
		l.fail(variable, reason)
	}
	return value
}

// ValidateHostPort reports whether value is host:port with a numeric port in
// 1-65535. An empty host is allowed.
func ValidateHostPort(value string) error {
	if reason := validateHostPort(value); reason != "" {
		return errors.New(reason)
	}
	return nil
}

// validateHostPort returns a reason when value is not host:port with a numeric
// port in 1-65535. An empty host is allowed.
func validateHostPort(value string) string {
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return "must be host:port"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "port must be a number between 1 and 65535"
	}
	return ""
}

// validateDatabaseURL returns a reason that never contains any part of raw,
// because raw may hold a password.
func validateDatabaseURL(raw string) string {
	if raw == "" {
		return "is required"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "is not a valid URL"
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "must use the postgres:// or postgresql:// scheme"
	}
	if u.Host == "" {
		return "must include a host"
	}
	if _, err := url.ParseQuery(u.RawQuery); err != nil {
		return "has an invalid query string"
	}
	return ""
}

// RedactURL replaces the password in raw with REDACTED, whether it is given
// as user info or as a "password" query parameter. It returns "invalid URL"
// when raw cannot be parsed, so a bad value is never echoed.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid URL"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), redactedPassword)
		}
	}
	u.RawQuery = redactQueryPasswords(u.RawQuery)
	return u.String()
}

// redactQueryPasswords replaces the value of every "password" parameter in
// rawQuery, leaving the order and encoding of the other parameters untouched.
func redactQueryPasswords(rawQuery string) string {
	if rawQuery == "" {
		return rawQuery
	}
	pairs := strings.Split(rawQuery, "&")
	for i, pair := range pairs {
		name, _, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		if decoded, err := url.QueryUnescape(name); err == nil && decoded == "password" {
			pairs[i] = name + "=" + redactedPassword
		}
	}
	return strings.Join(pairs, "&")
}

// Scrub removes the connection URL and every form of its password from text:
// the plain password, and any mix of percent-encoded characters ("%40" or
// "%2f" for "@" or "/", "+" or "%20" for a space). Driver errors may echo any
// of these, so callers use Scrub on error text before logging or returning it.
func Scrub(text, rawURL string) string {
	if rawURL == "" {
		return text
	}
	text = strings.ReplaceAll(text, rawURL, RedactURL(rawURL))
	u, err := url.Parse(rawURL)
	if err != nil {
		return text
	}
	var passwords []string
	if u.User != nil {
		if password, ok := u.User.Password(); ok {
			passwords = append(passwords, password)
		}
	}
	passwords = append(passwords, u.Query()["password"]...)
	for _, password := range passwords {
		if password == "" {
			continue
		}
		text = passwordPattern(password).ReplaceAllLiteralString(text, redactedPassword)
	}
	return text
}

// passwordPattern matches password where each character may appear literally
// or as percent escapes of its UTF-8 bytes (any hex case), and a space may
// also appear as "+".
func passwordPattern(password string) *regexp.Regexp {
	var pattern strings.Builder
	for _, r := range password {
		var escaped strings.Builder
		for _, b := range []byte(string(r)) {
			fmt.Fprintf(&escaped, "%%(?i:%02x)", b)
		}
		alternatives := []string{regexp.QuoteMeta(string(r)), escaped.String()}
		if r == ' ' {
			alternatives = append(alternatives, `\+`)
		}
		pattern.WriteString("(?:" + strings.Join(alternatives, "|") + ")")
	}
	return regexp.MustCompile(pattern.String())
}
