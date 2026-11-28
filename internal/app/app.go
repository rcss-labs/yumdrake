// Package app holds the service lifecycle shared by the orchestrator and runner:
// serving HTTP servers with graceful shutdown, the common start-up sequence and
// the container health-check subcommand.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/cache"
	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/health"
	"github.com/rcss-labs/yamdrake/internal/logging"
)

// Exit codes returned by the service entry points.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
	// ExitForced is the exit code when a second shutdown signal cuts a
	// graceful shutdown short (128 + SIGINT, the shell convention). The
	// published codes are therefore 0, 1, 2 and 130.
	ExitForced = 130
)

// ShutdownContext returns a context that is cancelled by the first value on
// signals, which starts the graceful shutdown. A second value while the
// shutdown is running calls exit(ExitForced) so an operator can always force a
// stuck process down. The returned stop function releases the watcher; call it
// when the service has returned.
func ShutdownContext(parent context.Context, signals <-chan os.Signal, exit func(code int)) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		select {
		case <-signals:
			cancel()
		case <-ctx.Done():
			return
		case <-stopped:
			return
		}
		select {
		case <-signals:
			// select picks randomly when stop and a signal are both ready;
			// stop wins because a stopped service is no longer shutting down.
			select {
			case <-stopped:
			default:
				exit(ExitForced)
			}
		case <-stopped:
		}
	}()
	return ctx, func() {
		stopOnce.Do(func() { close(stopped) })
		cancel()
	}
}

// SignalContext is ShutdownContext fed by SIGINT and SIGTERM, with os.Exit as
// the forced exit.
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	ctx, stop := ShutdownContext(parent, signals, os.Exit)
	return ctx, func() {
		signal.Stop(signals)
		stop()
	}
}

const probeTimeout = 3 * time.Second

// Serve starts every server and blocks until ctx is cancelled or a server fails.
// All listeners are bound before any server starts, so a listen error is
// returned immediately. On cancellation every server is shut down gracefully
// within shutdownTimeout. It returns nil for a clean shutdown.
func Serve(ctx context.Context, log *slog.Logger, shutdownTimeout time.Duration, servers ...*http.Server) error {
	listeners := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, bound := range listeners {
				_ = bound.Close()
			}
			return fmt.Errorf("listen on %q: %w", srv.Addr, err)
		}
		listeners = append(listeners, ln)
	}

	serveErrs := make(chan error, len(servers))
	for i, srv := range servers {
		log.Info("listening", "addr", listeners[i].Addr().String())
		go func() {
			if err := srv.Serve(listeners[i]); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrs <- fmt.Errorf("serve on %q: %w", srv.Addr, err)
			}
		}()
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveErrs:
	}

	log.Info("shutting down", "timeout", shutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	results := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			err := srv.Shutdown(shutdownCtx)
			if err != nil {
				// Shutdown gave up waiting; drop the remaining connections.
				_ = srv.Close()
				err = fmt.Errorf("shut down %q: %w", srv.Addr, err)
			}
			results <- err
		}()
	}
	errs := []error{serveErr}
	for range servers {
		errs = append(errs, <-results)
	}
	return errors.Join(errs...)
}

// CacheOptions builds the Valkey client options from the configuration: the
// address, the optional password and, when ValkeyTLS is set, TLS verified
// against the system roots.
func CacheOptions(c config.Common) cache.Options {
	o := cache.Options{Addr: c.ValkeyAddr, Password: c.ValkeyPassword.Reveal()}
	if c.ValkeyTLS {
		o.TLS = cache.TLSConfig(c.ValkeyAddr)
	}
	return o
}

// newHTTPServer returns the server with the shared timeouts. It deliberately
// sets no WriteTimeout: a server-wide write deadline would cut off long-lived
// responses such as Server-Sent Events streams.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// PostgresCheck is the readiness check for the database pool. databaseURL is
// the configured URL; it is used only to scrub the password from errors,
// because the health handler logs check errors verbatim.
func PostgresCheck(pool *pgxpool.Pool, databaseURL string) health.Check {
	return postgresCheck(pool.Ping, databaseURL)
}

func postgresCheck(ping func(context.Context) error, databaseURL string) health.Check {
	return health.Check{Name: "postgres", Fn: func(ctx context.Context) error {
		if err := ping(ctx); err != nil {
			return errors.New("postgres ping failed: " + config.Scrub(err.Error(), databaseURL))
		}
		return nil
	}}
}

// ValkeyCheck is the readiness check for the Valkey client.
func ValkeyCheck(client valkey.Client) health.Check {
	return health.Check{Name: "valkey", Fn: func(ctx context.Context) error {
		if err := cache.Ping(ctx, client); err != nil {
			return fmt.Errorf("valkey ping failed: %w", err)
		}
		return nil
	}}
}

// Service describes one service binary for Run.
type Service struct {
	Name   string
	Common config.Common
	// Addr is the address of the health/HTTP server.
	Addr string
	// Fields are extra attributes for the "starting" log line.
	Fields []any
	// Prepare runs after the database is open and before serving (migrate or
	// wait for the schema). It receives the service context and must bound
	// itself with its own timeout.
	Prepare func(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool) error
	// Extend runs after Prepare and returns what the service adds to the
	// shared health server: extra routes and background loops.
	// The Valkey client is open and owned by Run.
	Extend func(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, client valkey.Client) (Extension, error)
	// Web, when set, answers every path the health handler and the extension
	// do not claim (the orchestrator serves the web application this way).
	// /healthz, /readyz and the API prefixes never reach it.
	Web http.Handler
}

// Extension is what a service adds to the health server.
type Extension struct {
	// Handlers maps a ServeMux pattern (for example "/api/v1/") to its handler.
	// The health handler keeps every path not claimed here.
	Handlers map[string]http.Handler
	// Background functions run in their own goroutines until the context they
	// receive is cancelled; Run waits for them before it closes the pool.
	Background []func(ctx context.Context)
	// Health adds fields to the /readyz response (see health.WithDetail).
	Health []health.Option
}

// Run executes the shared start-up sequence and serves until ctx is cancelled.
// It returns ExitOK on a clean shutdown (including a signal during start-up)
// and ExitFailure otherwise.
//
// Waiting for the database is bounded by Common.StartupTimeout. Prepare bounds
// itself (the orchestrator's migration has its own, longer timeout).
func Run(ctx context.Context, svc Service, stdout io.Writer) int {
	log := logging.New(stdout, svc.Common.LogLevel, svc.Name)
	log.Info("starting", append([]any{
		"database", config.RedactURL(svc.Common.DatabaseURL),
		"valkey", svc.Common.ValkeyAddr,
		"addr", svc.Addr,
	}, svc.Fields...)...)

	startCtx, cancelStart := context.WithTimeout(ctx, svc.Common.StartupTimeout)
	defer cancelStart()

	pool, err := db.WaitOpen(startCtx, svc.Common.DatabaseURL, svc.Common.StartupTimeout, log)
	if err != nil {
		if ctx.Err() != nil {
			log.Info("stopped during start-up")
			return ExitOK
		}
		log.Error("database unavailable", "error", err.Error())
		return ExitFailure
	}
	defer pool.Close()

	if svc.Prepare != nil {
		if err := svc.Prepare(ctx, log, pool); err != nil {
			if ctx.Err() != nil {
				log.Info("stopped during start-up")
				return ExitOK
			}
			log.Error("start-up failed", "error", err.Error())
			return ExitFailure
		}
	}

	client, err := cache.Open(CacheOptions(svc.Common))
	if err != nil {
		log.Error("valkey client unavailable", "error", err.Error())
		return ExitFailure
	}
	defer client.Close()

	var ext Extension
	if svc.Extend != nil {
		ext, err = svc.Extend(ctx, log, pool, client)
		if err != nil {
			log.Error("start-up failed", "error", err.Error())
			return ExitFailure
		}
	}

	healthHandler := health.Handler(
		[]health.Check{PostgresCheck(pool, svc.Common.DatabaseURL), ValkeyCheck(client)},
		append([]health.Option{health.WithLogger(log)}, ext.Health...)...,
	)
	handler := serviceHandler(healthHandler, ext, svc.Web)

	background, stopBackground := context.WithCancel(ctx)
	var running sync.WaitGroup
	for _, fn := range ext.Background {
		running.Add(1)
		go func() {
			defer running.Done()
			fn(background)
		}()
	}
	server := newHTTPServer(svc.Addr, handler)
	// Requests must not see the shutdown signal: Serve drains them first. Their
	// context is cancelled once Serve has returned (drained, or timed out and
	// closed), so none is still running when the pool closes.
	requests, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	server.BaseContext = func(net.Listener) context.Context { return requests }
	err = Serve(ctx, log, svc.Common.ShutdownTimeout, server)
	cancelRequests()
	// Stop the loops and wait for them while the pool is still open.
	stopBackground()
	running.Wait()
	if err != nil {
		log.Error("serve failed", "error", err.Error())
		return ExitFailure
	}
	log.Info("stopped")
	return ExitOK
}

// Healthcheck implements the healthcheck subcommand: it reads only addrVar
// (falling back to defaultAddr), probes /healthz on loopback at that port and
// returns ExitOK or ExitFailure; an invalid address returns ExitUsage.
func Healthcheck(ctx context.Context, getenv func(string) string, addrVar, defaultAddr string, stderr io.Writer) int {
	addr := getenv(addrVar)
	if addr == "" {
		addr = defaultAddr
	}
	if err := config.ValidateHostPort(addr); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", addrVar, err)
		return ExitUsage
	}
	host, port, _ := net.SplitHostPort(addr) // validated above
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := health.Probe(probeCtx, "http://"+net.JoinHostPort(host, port)+"/healthz"); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return ExitFailure
	}
	return ExitOK
}

// webReservedPrefixes never reach the web handler: an unknown path under them
// gets the health handler's 404 instead of index.html.
var webReservedPrefixes = []string{"/api/", "/saml/"}

// serviceHandler assembles the service's mux. The extension's handlers claim
// their patterns; with a web handler, /healthz and /readyz go to the health
// handler and every other path goes to web. Without one, the health handler
// keeps every unclaimed path.
func serviceHandler(healthHandler http.Handler, ext Extension, web http.Handler) http.Handler {
	if len(ext.Handlers) == 0 && web == nil {
		return healthHandler
	}
	mux := http.NewServeMux()
	for pattern, extra := range ext.Handlers {
		mux.Handle(pattern, extra)
	}
	if web == nil {
		mux.Handle("/", healthHandler)
		return mux
	}
	mux.Handle("/healthz", healthHandler)
	mux.Handle("/readyz", healthHandler)
	for _, prefix := range webReservedPrefixes {
		if _, claimed := ext.Handlers[prefix]; !claimed {
			mux.Handle(prefix, healthHandler)
		}
	}
	mux.Handle("/", web)
	return mux
}
