// Package api serves the /api/v1 HTTP API: sign-in, inventory, folders, job
// sources and GitHub connections. Every request is made by a principal (a
// bearer token or a session cookie) and every route names the action it needs.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/auth"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/awsx/discovery"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/notify"
	"github.com/rcss-labs/yamdrake/internal/running"
	"github.com/rcss-labs/yamdrake/internal/secrets"
	"github.com/rcss-labs/yamdrake/internal/session"
	"github.com/rcss-labs/yamdrake/internal/store"
	"github.com/rcss-labs/yamdrake/internal/versions"
)

// DefaultTokenSSOMaxAge is the default of Deps.TokenSSOMaxAge.
const DefaultTokenSSOMaxAge = 168 * time.Hour

// maxBodyBytes limits every request body.
const maxBodyBytes = 1 << 20

// Error codes of the API.
const (
	codeUnauthorized = "unauthorized"
	codeForbidden    = "forbidden"
	codeUnavailable  = "unavailable"
	codeTooMany      = "too_many_requests"
	codeNotFound     = "not_found"
	codeInvalid      = "invalid"
	codeConflict     = "conflict"
	codeInternal     = "internal"
)

// Syncer runs on-demand syncs; *sync.Engine satisfies it. The request context
// is passed through, so a slow sync ends with its request.
type Syncer interface {
	SyncInventory(ctx context.Context) error
	// SetInventorySource stores the source and syncs it as one serialised step.
	SetInventorySource(ctx context.Context, source store.InventorySource, inTx func(context.Context, store.DBTX) error) error
	SyncJob(ctx context.Context, jobSourceID int64) error
}

// DiscoveryCredentials is implemented by *discovery.Provider.
type DiscoveryCredentials interface {
	// ForUncached skips the credential cache: the connection test must see
	// AWS as it is now.
	ForUncached(ctx context.Context, accountID, region, sessionName string) (aws.Config, error)
}

// Deps are the collaborators of the API.
type Deps struct {
	Pool   *pgxpool.Pool
	Engine Syncer
	Cipher secrets.Cipher
	// Slack posts the test message of the Slack settings page; nil means the
	// real Slack API (https://slack.com/api).
	Slack *notify.Slack
	Log   *slog.Logger
	// OnConnectionCreated, when set, is called with the (lower case) owner
	// after a connection is stored so caches for that owner can be dropped.
	OnConnectionCreated func(owner string)

	// Sessions stores browser sessions; Valkey holds the sign-in failure
	// counters. Both are required.
	Sessions *session.Manager
	Valkey   valkey.Client
	// KeyPrefix goes in front of the Valkey keys of the sign-in counters (use
	// the prefix of Sessions); empty in production.
	KeyPrefix string
	// PublicURL is the address users reach the service at; cookie requests that
	// change state must come from it. Required.
	PublicURL string
	// LockoutWindow is how long five failed sign-ins for one email lock it
	// (default 15 minutes). Tests shorten it.
	LockoutWindow time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
	// DevicePollInterval is how often a CLI may poll a device sign-in (default
	// 5 seconds). Tests shorten it.
	DevicePollInterval time.Duration
	// MaxConcurrentLogins bounds the password checks running at once (default
	// 4): each uses 64 MiB, and sign-in is open to anyone. VerifyWait is how
	// long a request waits for a slot before it is answered 503 (default 1s).
	MaxConcurrentLogins int
	VerifyWait          time.Duration

	// Runs carries out people's actions on runs and Logs reads their logs.
	// Both are required.
	Runs *engine.Engine
	Logs *logs.Reader
	// Wake lets the event stream hear about changes at once; nil is allowed
	// (and so is a Valkey outage): the stream also polls on a timer.
	Wake Waker
	// StreamKeepAlive is how often an idle event stream sends a comment line
	// (default 15 seconds); StreamPollInterval is how often it looks for
	// changes without a wake-up (default 1 second). Tests shorten both.
	StreamKeepAlive    time.Duration
	StreamPollInterval time.Duration
	// StreamBatchLines is how many log lines one stream event carries at most
	// (default 200).
	StreamBatchLines int
	// TokenSSOMaxAge is how recent the last SSO sign-in of a SAML user must be
	// for their tokens to work (default 168h). Group memberships from the
	// identity provider refresh only at sign-in, so this bounds how long a
	// person removed there keeps their access through a token.
	TokenSSOMaxAge time.Duration
	// Discovery builds the discovery credentials of an account; the connection
	// test uses it. Optional: New builds one from AWSConfig.
	Discovery DiscoveryCredentials
	// AWSConfig is the orchestrator's own AWS configuration (default
	// credential chain, optional endpoint override), the base of the default
	// Discovery. It may be empty when the service runs without AWS credentials;
	// the connection test then reports a failure.
	AWSConfig aws.Config
	// AWSOptions are the deployment-wide AWS settings (the emulator endpoint
	// in tests); zero for real AWS.
	AWSOptions awsx.Options
	// Versions lists the image versions of a job's services. Optional: New
	// builds one from the default Discovery and Valkey.
	Versions VersionLister
	// Running reads the versions running in ECS. Optional: New builds one from
	// the default Discovery, the pool and Valkey.
	Running RunningReader
	// RunnerGoneAfter is how long a runner may stay silent before the queue
	// and the health page stop counting it (default store.RunnerGoneAfter).
	RunnerGoneAfter time.Duration
}

// valkeyTimeout bounds every Valkey call made for a request: against a dead
// server valkey-go keeps redialling until its context ends.
var valkeyTimeout = 3 * time.Second

type server struct {
	Deps
	// dummyHash is verified when the account does not exist (or cannot sign in
	// with a password), so every sign-in costs one argon2 verification.
	dummyHash string
	// verifySlots has one token per allowed concurrent password check.
	verifySlots chan struct{}
	// keys caches the decrypted SAML keys (see keyCacheTTL).
	keys keyCache
}

// dummyPasswordHash is computed once per process, with the production parameters.
var dummyPasswordHash = sync.OnceValues(func() (string, error) {
	return auth.HashPassword("a password nobody has chosen")
})

// New returns the handler for the whole /api/v1 tree and the /saml endpoints
// (full paths are registered, so mount it at "/api/v1/" and "/saml/"). Every route except sign-in needs an
// authenticated principal, and every route states the action it needs.
func New(deps Deps) http.Handler {
	if deps.Pool == nil || deps.Sessions == nil || deps.Valkey == nil || deps.PublicURL == "" || deps.Runs == nil || deps.Logs == nil {
		panic("api.New: Pool, Sessions, Valkey, PublicURL, Runs and Logs are required")
	}
	if deps.StreamKeepAlive <= 0 {
		deps.StreamKeepAlive = defaultStreamKeepAlive
	}
	if deps.StreamPollInterval <= 0 {
		deps.StreamPollInterval = defaultStreamPollInterval
	}
	if deps.StreamBatchLines <= 0 {
		deps.StreamBatchLines = defaultStreamBatchLines
	}
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if deps.LockoutWindow <= 0 {
		deps.LockoutWindow = defaultLockoutWindow
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.DevicePollInterval <= 0 {
		deps.DevicePollInterval = defaultDevicePollInterval
	}
	if deps.Discovery == nil {
		deps.Discovery = discovery.New(deps.Pool, deps.Cipher, deps.AWSConfig, deps.AWSOptions).WithLogger(deps.Log)
	}
	if deps.Versions == nil {
		// The cached provider is the one that builds version-list credentials;
		// a Discovery that cannot (a test double) leaves the route unavailable.
		if provider, ok := deps.Discovery.(versions.Discovery); ok {
			deps.Versions = versions.New(versions.Config{
				Discovery: provider, Valkey: deps.Valkey, KeyPrefix: deps.KeyPrefix, AWS: deps.AWSOptions})
		}
	}
	if deps.Running == nil {
		if provider, ok := deps.Discovery.(running.Discovery); ok {
			deps.Running = running.New(running.Config{
				Discovery: provider, Valkey: deps.Valkey, KeyPrefix: deps.KeyPrefix, AWS: deps.AWSOptions, DB: deps.Pool,
				Checks: connectionChecks{pool: deps.Pool, log: deps.Log}})
		}
	}
	if deps.RunnerGoneAfter <= 0 {
		deps.RunnerGoneAfter = store.RunnerGoneAfter
	}
	dummy, err := dummyPasswordHash()
	if err != nil {
		panic("api.New: dummy password hash: " + err.Error())
	}
	if deps.MaxConcurrentLogins <= 0 {
		deps.MaxConcurrentLogins = defaultConcurrentLogins
	}
	if deps.TokenSSOMaxAge <= 0 {
		deps.TokenSSOMaxAge = DefaultTokenSSOMaxAge
	}
	if deps.VerifyWait <= 0 {
		deps.VerifyWait = defaultVerifyWait
	}
	s := &server{Deps: deps, dummyHash: dummy, verifySlots: make(chan struct{}, deps.MaxConcurrentLogins)}

	authenticated := s.authenticatedMux(nil)

	root := http.NewServeMux()
	// Sign-in is the one route that is open to everyone; it carries no session
	// cookie to check, so it has its own origin check (login CSRF).
	root.HandleFunc("POST /api/v1/auth/login", s.login)
	// Single sign-on. These are open to everyone by nature: the identity
	// provider's browser post carries no session of ours, so /saml/acs is exempt
	// from the same-origin check (and only from that); it is protected by the
	// signed response, the relay state cookie and the replay check instead.
	// Mount this handler at "/saml/" as well as "/api/v1/".
	root.HandleFunc("GET /api/v1/auth/methods", s.authMethods)
	root.HandleFunc("GET /saml/metadata", s.samlMetadata)
	root.HandleFunc("POST /saml/acs", s.samlACS)
	root.HandleFunc("GET /api/v1/auth/saml/start", s.samlStart)
	// The CLI's device sign-in starts and finishes without credentials (the
	// CLI has none yet). Neither route reads a cookie, so neither can be used
	// cross-site to act as a signed-in browser user; confirm is authenticated.
	root.HandleFunc("POST /api/v1/cli/login", s.startDeviceLogin)
	root.HandleFunc("POST /api/v1/cli/login/token", s.pollDeviceLogin)
	root.Handle("/", s.authenticate(authenticated))
	return root
}

// authenticatedMux serves the routes of routes(), each behind require, and 404
// for everything else. extra lets a test register one more pattern; it exists
// to prove the guard below: a pattern that is not in routes() is never served,
// whoever registered it, so no route can skip the permission decision.
func (s *server) authenticatedMux(extra func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	allowed := map[string]bool{"/": true}
	for _, route := range s.routes() {
		allowed[route.pattern] = true
		mux.Handle(route.pattern, s.require(route.action, nil)(route.handler))
	}
	// Unknown paths: 401 without credentials, 404 with them.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "not found")
	})
	if extra != nil {
		extra(mux)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refusedText(r) {
			invalid(w, "the request path or query contains a control character or invalid text")
			return
		}
		if _, pattern := mux.Handler(r); pattern != "" && !allowed[pattern] {
			s.Log.Error("refusing a route that has no permission decision", "pattern", pattern)
			writeError(w, http.StatusNotFound, codeNotFound, "not found")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// refusedText reports whether the decoded path or any query name or value has
// text the database cannot store (see hasControlText).
func refusedText(r *http.Request) bool {
	if hasControlText(r.URL.Path, false) {
		return true
	}
	for name, values := range r.URL.Query() {
		if hasControlText(name, false) {
			return true
		}
		for _, value := range values {
			if hasControlText(value, false) {
				return true
			}
		}
	}
	return false
}

// routeDef is a route of the authenticated part of the API with the action it
// needs. Every route is listed here, so each has an explicit decision.
type routeDef struct {
	pattern string
	action  auth.Action
	handler http.HandlerFunc
}

func (s *server) routes() []routeDef {
	return append([]routeDef{
		{"GET /api/v1/me", auth.View, s.me},
		{"POST /api/v1/auth/logout", auth.View, s.logout},
		{"GET /api/v1/inventory", auth.View, s.getInventory},
		{"GET /api/v1/inventory/services/{name}", auth.View, s.getService},
		{"POST /api/v1/inventory/sync", auth.ManageConnections, s.syncInventory},
		{"GET /api/v1/admin/inventory-source", auth.ManageConnections, s.getInventorySource},
		{"PUT /api/v1/admin/inventory-source", auth.ManageConnections, s.putInventorySource},
		{"GET /api/v1/admin/github-connections", auth.ManageConnections, s.listConnections},
		{"POST /api/v1/admin/github-connections", auth.ManageConnections, s.createConnection},
		{"GET /api/v1/admin/aws-accounts", auth.ManageConnections, s.listAWSAccounts},
		{"POST /api/v1/admin/aws-accounts", auth.ManageConnections, s.createAWSAccount},
		{"POST /api/v1/admin/aws-accounts/{id}/test", auth.ManageConnections, s.testAWSAccount},
		{"GET /api/v1/admin/sso", auth.ManageAccess, s.getSSO},
		{"PUT /api/v1/admin/sso", auth.ManageAccess, s.putSSO},
		{"POST /api/v1/admin/sso/test", auth.ManageAccess, s.testSSO},
		{"GET /api/v1/folders", auth.View, s.listFolders},
		{"POST /api/v1/folders", auth.ManageJobs, s.createFolder},
		{"GET /api/v1/jobs", auth.View, s.listJobs},
		{"POST /api/v1/jobs", auth.ManageJobs, s.registerJob},
		{"GET /api/v1/jobs/{id}", auth.View, s.getJob},
		{"PATCH /api/v1/jobs/{id}", auth.ManageJobs, s.moveJob},
		{"POST /api/v1/jobs/{id}/sync", auth.ManageJobs, s.syncJob},
		{"GET /api/v1/jobs/{id}/versions", auth.View, s.getJobVersions},
	}, append(s.runRoutes(), append(s.accessRoutes(), append(s.freezeRoutes(), append(s.slackRoutes(), append(s.overviewRoutes(), append(s.versionRoutes(),
		append(s.queueRoutes(), append(s.serviceRoutes(), s.healthRoutes()...)...)...)...)...)...)...)...)...)
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

// internalError logs the detail and answers with a generic message.
func (s *server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("api request failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
	writeError(w, http.StatusInternalServerError, codeInternal, "internal error")
}

func invalid(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, codeInvalid, message)
}

// decodeBody reads one JSON object into dst, rejecting unknown fields,
// trailing data and bodies over 1 MB. It writes the error response itself and
// reports whether decoding succeeded.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !decodeBodyUnchecked(w, r, dst) {
		return false
	}
	if problem := controlProblem(dst); problem != "" {
		invalid(w, problem)
		return false
	}
	return true
}

// decodeBodyUnchecked is decodeBody without the control-character check, for
// the sign-in route, which answers such text as it answers any wrong
// credential.
func decodeBodyUnchecked(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(dst)
	if err == nil {
		if more := decoder.Decode(&struct{}{}); !errors.Is(more, io.EOF) {
			err = errors.New("the body must contain exactly one JSON object")
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, codeInvalid, "request body is larger than 1 MB")
		return false
	}
	invalid(w, "invalid request body: "+err.Error())
	return false
}

// pathID parses the {id} path value.
func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func notFound(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotFound, codeNotFound, what+" not found")
}
