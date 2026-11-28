// Command yumdrake-demo is the helper of the demo setup (deploy/compose/compose.demo.yaml). It is a
// development tool and is never part of the product images.
//
// It serves a fake GitHub holding the sample data, makes a fresh Yumdrake ready to try (an admin, a
// GitHub connection, the inventory source, folders, jobs, two more people with fewer permissions)
// and answers /healthz with 200 once that is done. Running it again changes nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2

	usage = `usage: yumdrake-demo [healthcheck]

Serves a fake GitHub with the sample data and prepares Yumdrake for the demo.
With "healthcheck" it asks the running helper whether it has finished (for the image HEALTHCHECK).

Environment:
  YUMDRAKE_DEMO_LISTEN            address to listen on (default :9000)
  YUMDRAKE_DEMO_DATA              directory with the sample data (default /demo)
  YUMDRAKE_DEMO_ORCHESTRATOR_URL  where the orchestrator answers, for example http://orchestrator:8080 (required)
  YUMDRAKE_PUBLIC_URL             the address people open (required)
  YUMDRAKE_DATABASE_URL, PGPASSWORD  the orchestrator's database (required)

Exit codes: 0 done (or healthy), 1 failure (or not ready), 2 usage.
`
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("yumdrake-demo", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.WriteString(stdout, usage)
			return exitOK
		}
		_, _ = io.WriteString(stderr, "yumdrake-demo: "+err.Error()+"\n\n"+usage)
		return exitUsage
	}
	listen := valueOr(getenv, "YUMDRAKE_DEMO_LISTEN", ":9000")
	switch flags.Arg(0) {
	case "":
		if flags.NArg() > 0 {
			break
		}
		return serve(getenv, listen, stdout, stderr)
	case "healthcheck":
		if flags.NArg() == 1 {
			return healthcheck(listen, stderr)
		}
	case "help":
		_, _ = io.WriteString(stdout, usage)
		return exitOK
	}
	_, _ = io.WriteString(stderr, "yumdrake-demo: unknown arguments\n\n"+usage)
	return exitUsage
}

func valueOr(getenv func(string) string, name, fallback string) string {
	if v := getenv(name); v != "" {
		return v
	}
	return fallback
}

func healthcheck(listen string, stderr io.Writer) int {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "yumdrake-demo: YUMDRAKE_DEMO_LISTEN is not host:port")
		return exitFailure
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if get(ctx, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz") != http.StatusOK {
		_, _ = fmt.Fprintln(stderr, "yumdrake-demo: not ready")
		return exitFailure
	}
	return exitOK
}

func serve(getenv func(string) string, listen string, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))
	orchestratorURL, publicURL, databaseURL := getenv("YUMDRAKE_DEMO_ORCHESTRATOR_URL"), getenv("YUMDRAKE_PUBLIC_URL"), getenv("YUMDRAKE_DATABASE_URL")
	if orchestratorURL == "" || publicURL == "" || databaseURL == "" {
		_, _ = io.WriteString(stderr, "yumdrake-demo: YUMDRAKE_DEMO_ORCHESTRATOR_URL, YUMDRAKE_PUBLIC_URL and YUMDRAKE_DATABASE_URL are required\n\n"+usage)
		return exitUsage
	}
	gh, err := newDemoGitHub(valueOr(getenv, "YUMDRAKE_DEMO_DATA", "/demo"))
	if err != nil {
		log.Error("fake GitHub", "error", err.Error())
		return exitFailure
	}

	var seeded atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !seeded.Load() {
			http.Error(w, "seeding", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.Handle("/", gh.Handler())
	// Listening first: the orchestrator reads the fake GitHub while the seeding waits for it.
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		log.Error("listen", "error", err.Error())
		return exitFailure
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	seedDone := make(chan error, 1)
	go func() {
		seedDone <- seed(ctx, seedConfig{OrchestratorURL: orchestratorURL, PublicURL: publicURL, DatabaseURL: databaseURL, Log: log})
	}()

	code := exitOK
	for waiting := true; waiting; {
		select {
		case err := <-seedDone:
			if err != nil {
				log.Error("seeding failed", "error", err.Error())
				code, waiting = exitFailure, false
				break
			}
			seeded.Store(true)
			printSignIns(stdout, publicURL)
			seedDone = nil // finished: keep serving until told to stop
		case err := <-serverDone:
			log.Error("the fake GitHub stopped", "error", err.Error())
			code, waiting = exitFailure, false
		case <-ctx.Done():
			waiting = false
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	return code
}

func printSignIns(w io.Writer, publicURL string) {
	_, _ = fmt.Fprintf(w, `
Yumdrake demo is ready: %s

  admin     %s   %s    everything
  deployer  %s   %s   run in dev and qa
  viewer    %s   %s     look only

These passwords are public. The demo works only on localhost.
`, publicURL,
		adminEmail, adminPassword, deployerEmail, deployerPassword, viewerEmail, viewerPassword)
}
