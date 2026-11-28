// Command runner is the Yumdrake worker service. It never migrates the schema.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/app"
	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/runnerapp"
	"github.com/rcss-labs/yamdrake/internal/steps/fake"
	"github.com/rcss-labs/yamdrake/internal/version"
	"github.com/rcss-labs/yamdrake/internal/wake"
)

const (
	serviceName       = "yumdrake-runner"
	healthAddrVar     = "YUMDRAKE_HEALTH_ADDR"
	defaultHealthAddr = "127.0.0.1:8081"

	// The log writer stores a leg's lines every logFlushEvery or whenever
	// logMaxBatch of them are waiting.
	logFlushEvery   = 500 * time.Millisecond
	logMaxBatch     = 100
	logCloseTimeout = 10 * time.Second

	// artifactSweepInterval is how often config copies past their retention are deleted.
	artifactSweepInterval = time.Hour
)

var _ runner.LineSink = (*logs.Writer)(nil)

func main() {
	ctx, stop := app.SignalContext(context.Background())
	code := run(ctx, os.Args, os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run executes the subcommand named by args[1] (default: serve) and returns the exit code.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	subcommand := ""
	if len(args) > 1 {
		subcommand = args[1]
	}
	switch subcommand {
	case "":
		return serve(ctx, getenv, stdout, stderr)
	case "version":
		_, _ = fmt.Fprintln(stdout, version.String())
		return app.ExitOK
	case "healthcheck":
		return app.Healthcheck(ctx, getenv, healthAddrVar, defaultHealthAddr, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "usage: %s [version|healthcheck]\n\nWith no subcommand the service starts and is configured through YUMDRAKE_* environment variables.\n", serviceName)
		return app.ExitUsage
	}
}

func serve(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadRunner(getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return app.ExitUsage
	}
	return app.Run(ctx, app.Service{
		Name:   serviceName,
		Common: cfg.Common,
		Addr:   cfg.HealthAddr,
		Fields: []any{"runner_id", cfg.RunnerID, "env", cfg.Env, "fake_steps", cfg.FakeSteps},
		Extend: func(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, client valkey.Client) (app.Extension, error) {
			bus := wake.New(client, log, wake.WithKeyPrefix(cfg.ValkeyKeyPrefix))
			sink := logs.NewWriter(pool, logFlushEvery, logMaxBatch, time.Now, func(runID int64) {
				bus.Publish(context.Background(), wake.TopicLogs, runID)
			}, log)
			r := newRunner(cfg, pool, sink, bus, log)
			if err := wireRealExecutors(cfg, func() error { return runnerapp.RegisterExecutors(ctx, log, cfg, pool, r) }); err != nil {
				return app.Extension{}, err
			}
			background := []func(context.Context){
				func(ctx context.Context) {
					if err := r.Run(ctx, legWakeups(ctx, bus)); err != nil {
						log.Error("runner stopped with an error", "error", err.Error())
					}
					// Every leg has settled or been released: store what is left of their logs.
					closeCtx, cancel := context.WithTimeout(context.Background(), logCloseTimeout)
					defer cancel()
					if err := sink.Close(closeCtx); err != nil {
						log.Error("could not store the last log lines", "error", err.Error())
					}
				},
			}
			if cfg.ArtifactDir != "" {
				dir := cfg.ArtifactDir
				background = append(background, func(ctx context.Context) { runArtifactSweeper(ctx, log, dir) })
			}
			return app.Extension{Background: background}, nil
		},
		Prepare: func(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool) error {
			log.Info("waiting for schema", "want_version", db.LatestVersion())
			return db.WaitSchema(ctx, pool, cfg.StartupTimeout)
		},
	}, stdout)
}

// runArtifactSweeper deletes config copies older than artifacts.Retention, once
// at start and then every artifactSweepInterval, until ctx is done.
func runArtifactSweeper(ctx context.Context, log *slog.Logger, dir string) {
	sweep := func() {
		removed, err := artifacts.Sweep(ctx, dir, artifacts.Retention, time.Now())
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("sweeping config copies failed", "error", err.Error())
			}
			return
		}
		if removed > 0 {
			log.Info("swept config copies", "removed", removed)
		}
	}
	sweep()
	ticker := time.NewTicker(artifactSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// legWakeups turns "legs" wake-ups into the signal Runner.Run waits on. The
// runner claims by id-less poll, so the run id does not matter.
func legWakeups(ctx context.Context, bus *wake.Bus) <-chan struct{} {
	out := make(chan struct{}, 1)
	ids := bus.Subscribe(ctx, wake.TopicLegs)
	go func() {
		for range ids {
			select {
			case out <- struct{}{}:
			default: // one pending signal is enough
			}
		}
	}()
	return out
}

// newRunner builds the worker pool. Lines go to sink; after each settled leg
// the runner announces its run on the "runs" topic so the orchestrator
// advances it at once. The fake steps are registered only when
// the configuration allows them (YUMDRAKE_ENV=dev and YUMDRAKE_FAKE_STEPS=true;
// config.LoadRunner refuses fakes anywhere else). Without fakes the caller
// registers the real "resolve" and "revalidate" (see wireRealExecutors);
// copy-config and ecs-deploy arrive with plan 6, so a runner without fakes
// fails those legs with "no executor for this step".
func newRunner(cfg config.Runner, pool *pgxpool.Pool, sink runner.LineSink, bus *wake.Bus, log *slog.Logger) *runner.Runner {
	q := queue.New(pool, cfg.Lease, nil) // nil: the database clock, shared by every runner and orchestrator
	r := runner.New(q, sink, cfg.RunnerID, cfg.Concurrency, cfg.Heartbeat, cfg.Poll, log).
		WithShutdownTimeout(cfg.ShutdownTimeout).
		WithRegistry(pool, 0).
		WithSettledNotifier(func(runID int64) { bus.Publish(context.Background(), wake.TopicRuns, runID) })
	if cfg.FakeSteps && cfg.Env == "dev" {
		fake.Register(r, fake.Options{StepTime: 2 * time.Second, SlowFor: 30 * time.Second})
		log.Warn("fake steps are registered; nothing real will be changed")
	}
	return r
}

// wireRealExecutors calls register, which installs the real "resolve" and
// "revalidate", unless fake steps are on: then all four fakes stay, so a demo
// or dev stack with no AWS behind it can plan and run.
func wireRealExecutors(cfg config.Runner, register func() error) error {
	if cfg.FakeSteps {
		return nil
	}
	return register()
}
