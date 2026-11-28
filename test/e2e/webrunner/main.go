// Command webrunner is the runner the browser test (web/e2e) starts: the real
// runner service with the fake step executors for resolve, revalidate,
// copy-config and ecs-deploy, so runs plan and finish in seconds with no AWS
// behind them.
//
// It is a test program. It is not built by make build, not in any image and not
// started by anything but the webstack helper. cmd/runner now keeps all four
// fakes under YUMDRAKE_FAKE_STEPS=true too; this program stays for its shorter
// step times. The fakes are refused unless YUMDRAKE_ENV=dev and
// YUMDRAKE_FAKE_STEPS=true (config.LoadRunner enforces it), as for any runner.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/app"
	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/steps/fake"
	"github.com/rcss-labs/yamdrake/internal/wake"
)

const (
	serviceName   = "yumdrake-web-e2e-runner"
	logFlushEvery = 500 * time.Millisecond
	logMaxBatch   = 100
	// The fake step times the browser test is written for: 2s a step, 30s for a "slow" version.
	stepTime = 2 * time.Second
	slowFor  = 30 * time.Second
)

func main() {
	ctx, stop := app.SignalContext(context.Background())
	code := serve(ctx)
	stop()
	os.Exit(code)
}

func serve(ctx context.Context) int {
	cfg, err := config.LoadRunner(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return app.ExitUsage
	}
	if !cfg.FakeSteps || cfg.Env != "dev" {
		_, _ = fmt.Fprintln(os.Stderr, "webrunner runs only fake steps: set YUMDRAKE_ENV=dev and YUMDRAKE_FAKE_STEPS=true")
		return app.ExitUsage
	}
	return app.Run(ctx, app.Service{
		Name:   serviceName,
		Common: cfg.Common,
		Addr:   cfg.HealthAddr,
		Fields: []any{"runner_id", cfg.RunnerID, "env", cfg.Env, "fake_steps", cfg.FakeSteps},
		Extend: func(_ context.Context, log *slog.Logger, pool *pgxpool.Pool, client valkey.Client) (app.Extension, error) {
			bus := wake.New(client, log, wake.WithKeyPrefix(cfg.ValkeyKeyPrefix))
			sink := logs.NewWriter(pool, logFlushEvery, logMaxBatch, time.Now, func(runID int64) {
				bus.Publish(context.Background(), wake.TopicLogs, runID)
			}, log)
			q := queue.New(pool, cfg.Lease, nil)
			r := runner.New(q, sink, cfg.RunnerID, cfg.Concurrency, cfg.Heartbeat, cfg.Poll, log).
				WithShutdownTimeout(cfg.ShutdownTimeout).
				WithRegistry(pool, 0).
				WithSettledNotifier(func(runID int64) { bus.Publish(context.Background(), wake.TopicRuns, runID) })
			fake.Register(r, fake.Options{StepTime: stepTime, SlowFor: slowFor})
			log.Warn("every step is fake; nothing real will be changed")
			return app.Extension{Background: []func(context.Context){
				func(ctx context.Context) {
					if err := r.Run(ctx, legWakeups(ctx, bus)); err != nil {
						log.Error("runner stopped with an error", "error", err.Error())
					}
					closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := sink.Close(closeCtx); err != nil {
						log.Error("could not store the last log lines", "error", err.Error())
					}
				},
			}}, nil
		},
		Prepare: func(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool) error {
			log.Info("waiting for schema", "want_version", db.LatestVersion())
			return db.WaitSchema(ctx, pool, cfg.StartupTimeout)
		},
	}, os.Stdout)
}

// legWakeups turns "legs" wake-ups into the signal Runner.Run waits on, as cmd/runner does.
func legWakeups(ctx context.Context, bus *wake.Bus) <-chan struct{} {
	out := make(chan struct{}, 1)
	ids := bus.Subscribe(ctx, wake.TopicLegs)
	go func() {
		for range ids {
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out
}
