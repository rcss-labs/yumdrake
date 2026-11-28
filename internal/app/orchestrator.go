package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/rcss-labs/yamdrake/internal/api"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/awsx/discovery"
	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/db"
	"github.com/rcss-labs/yamdrake/internal/engine"
	"github.com/rcss-labs/yamdrake/internal/health"
	"github.com/rcss-labs/yamdrake/internal/logs"
	"github.com/rcss-labs/yamdrake/internal/metrics"
	"github.com/rcss-labs/yamdrake/internal/notify"
	"github.com/rcss-labs/yamdrake/internal/queue"
	"github.com/rcss-labs/yamdrake/internal/secrets"
	"github.com/rcss-labs/yamdrake/internal/session"
	"github.com/rcss-labs/yamdrake/internal/store"
	yumsync "github.com/rcss-labs/yamdrake/internal/sync"
	"github.com/rcss-labs/yamdrake/internal/wake"
	"github.com/rcss-labs/yamdrake/internal/webui"
)

// OrchestratorServiceName is the name the orchestrator logs under.
const OrchestratorServiceName = "yumdrake-orchestrator"

// metricsOutput is where the metrics emitter writes its lines. Standard output
// is where the service's own JSON log goes too, so CloudWatch Logs gets both
// from one stream and picks out the lines that carry the "_aws" directive.
var metricsOutput io.Writer = os.Stdout

// NewOrchestratorService describes the orchestrator for Run: the shared
// start-up with the schema migration (when cfg.MigrateOnStart is set) and the
// orchestrator's own parts. cmd/orchestrator and the end-to-end tests both
// start it this way.
func NewOrchestratorService(cfg config.Orchestrator) Service {
	return Service{
		Name:   OrchestratorServiceName,
		Common: cfg.Common,
		Addr:   cfg.HTTPAddr,
		Extend: OrchestratorExtension(cfg),
		Web:    webui.Handler(),
		Fields: []any{"migrate_on_start", cfg.MigrateOnStart, "migrate_timeout", cfg.MigrateTimeout.String()},
		Prepare: func(ctx context.Context, log *slog.Logger, _ *pgxpool.Pool) error {
			if !cfg.MigrateOnStart {
				return nil
			}
			log.Info("applying migrations")
			migrateCtx, cancel := context.WithTimeout(ctx, cfg.MigrateTimeout)
			defer cancel()
			return db.Migrate(migrateCtx, cfg.DatabaseURL)
		},
	}
}

// OrchestratorExtension wires the orchestrator's own parts into Run: the
// secret cipher, the sync engine with its polling loop, the run engine's loop
// (woken through Valkey, with a timer as fallback) and the /api/v1 API with
// its sessions. Without an inventory source the loop finds nothing to do.
func OrchestratorExtension(cfg config.Orchestrator) func(context.Context, *slog.Logger, *pgxpool.Pool, valkey.Client) (Extension, error) {
	return func(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, client valkey.Client) (Extension, error) {
		awsConfig, err := awsx.BaseConfig(ctx, cfg.AWS)
		if err != nil {
			if cfg.SecretsMode == "kms" || cfg.LogBucket != "" {
				return Extension{}, err
			}
			// Without KMS the AWS configuration is only for discovery (the connection test).
			log.Warn("the default AWS configuration could not be loaded; testing AWS accounts will fail", "error", err.Error())
		}
		cipher, err := NewCipher(cfg.Common, awsConfig)
		if err != nil {
			return Extension{}, err
		}
		sources := newGitHubSources(pool, cipher, cfg.GitHubAPIURL, log)
		syncEngine := yumsync.NewEngine(pool, sources.For, log, time.Now)
		bus := wake.New(client, log, wake.WithKeyPrefix(cfg.ValkeyKeyPrefix))
		runEngine := engine.New(engine.Deps{
			DB:        pool,
			Limits:    engine.Limits{PerEnvironment: cfg.LimitPerEnvironment, PerAccount: cfg.LimitPerAccount},
			Log:       log,
			Publisher: bus,
			AWS:       cfg.AWS,
			PublicURL: cfg.PublicURL,
		})
		// Archived legs are read back from the bucket or directory they were written to.
		var archive logs.Archive
		var archiver *logs.Archiver
		switch {
		case cfg.LogBucket != "":
			// Yumdrake's own account and identity, not a connected account's.
			archive = logs.NewS3Archive(s3.NewFromConfig(awsConfig, func(o *s3.Options) { o.UsePathStyle = cfg.AWS.UsePathStyleS3() }), cfg.LogBucket)
		case cfg.LogArchiveDir != "":
			if archive, err = logs.NewDirArchive(cfg.LogArchiveDir, cipher); err != nil {
				return Extension{}, fmt.Errorf("log archive: %w", err)
			}
		}
		if archive != nil {
			archiver = logs.NewArchiver(pool, archive, cfg.LogArchiveAfter, nil, log)
		}
		slack := notify.NewSlack(cfg.SlackAPIURL, nil)
		sender := notify.NewSender(pool, slack, notify.SlackTokenSource(pool, cipher), time.Now, log)
		handler := api.New(api.Deps{
			Pool:                pool,
			Engine:              syncEngine,
			Cipher:              cipher,
			Slack:               slack,
			Log:                 log,
			OnConnectionCreated: sources.Forget,
			Sessions:            session.NewManager(client, time.Now, session.WithKeyPrefix(cfg.ValkeyKeyPrefix)),
			Valkey:              client,
			KeyPrefix:           cfg.ValkeyKeyPrefix,
			PublicURL:           cfg.PublicURL,
			Runs:                runEngine,
			Logs:                logs.NewReader(pool, archive),
			Wake:                bus,
			TokenSSOMaxAge:      cfg.TokenSSOMaxAge,
			RunnerGoneAfter:     cfg.RunnerGoneAfter,
			AWSConfig:           awsConfig,
			AWSOptions:          cfg.AWS,
			Discovery:           discovery.New(pool, cipher, awsConfig, cfg.AWS).WithLogger(log),
		})
		// The queue is used only for Sweep, which compares against the database
		// clock (nil) and does not need a lease length.
		sweepQueue := queue.New(pool, 0, nil)
		background := []func(context.Context){func(ctx context.Context) {
			log.Info("sync loop started", "interval", cfg.SyncInterval.String(), "github_api_url", cfg.GitHubAPIURL)
			syncEngine.Run(ctx, cfg.SyncInterval)
			log.Info("sync loop stopped")
		}, func(ctx context.Context) {
			log.Info("engine loop started", "sweep_interval", cfg.SweepInterval.String())
			runEngine.Loop(ctx, bus.Subscribe(ctx, wake.TopicRuns), cfg.SweepInterval, sweepQueue)
			log.Info("engine loop stopped")
		}, func(ctx context.Context) {
			log.Info("notification sender started", "interval", cfg.NotifyInterval.String(), "slack_api_url", cfg.SlackAPIURL)
			sender.Run(ctx, cfg.NotifyInterval)
			log.Info("notification sender stopped")
		}}
		if archiver != nil {
			background = append(background, func(ctx context.Context) {
				log.Info("log archive loop started", "interval", cfg.LogArchiveInterval.String(), "after", cfg.LogArchiveAfter.String())
				archiveLoop(ctx, archiver, cfg.LogArchiveInterval, log)
				log.Info("log archive loop stopped")
			})
		}
		if cfg.Metrics == "emf" {
			emitter := metrics.NewEmitter(pool, metricsOutput, metrics.Namespace, cfg.MetricsDimensions, metrics.Interval, log)
			background = append(background, func(ctx context.Context) {
				log.Info("metrics emitter started", "interval", metrics.Interval.String())
				emitter.Run(ctx)
				log.Info("metrics emitter stopped")
			})
		}
		return Extension{
			Handlers:   map[string]http.Handler{"/api/v1/": handler, "/saml/": handler},
			Background: background,
			Health:     []health.Option{health.WithDetail("notifications", notificationStats(pool))},
		}, nil
	}
}

// NewCipher builds the secret cipher of the configured mode. KMS needs a
// region (from the default AWS chain, for example AWS_REGION) and, when it is
// used, credentials; the error never carries key material.
func NewCipher(cfg config.Common, awsConfig aws.Config) (secrets.Cipher, error) {
	if cfg.SecretsMode == "kms" {
		if awsConfig.Region == "" {
			return nil, errors.New("secret cipher: kms mode needs an AWS region (set AWS_REGION)")
		}
		return secrets.NewKMS(kms.NewFromConfig(awsConfig), cfg.KMSKeyID), nil
	}
	cipher, err := secrets.NewLocal([]byte(cfg.LocalSecretKey.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("secret cipher: %w", err)
	}
	return cipher, nil
}

// notificationStats reports the Slack outbox for /readyz: how many messages
// are pending and failed, when the oldest pending one was created (null when
// none is) and whether Slack refused the stored token.
func notificationStats(pool *pgxpool.Pool) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		pending, failed, oldest, err := store.NotificationStats(ctx, pool)
		if err != nil {
			return nil, err
		}
		slackStatus, err := notify.LoadSlackStatus(ctx, pool)
		if err != nil {
			return nil, err
		}
		var oldestAt *time.Time
		if oldest != nil {
			at := oldest.UTC()
			oldestAt = &at
		}
		return struct {
			Pending         int        `json:"pending"`
			Failed          int        `json:"failed"`
			OldestPendingAt *time.Time `json:"oldest_pending_at"`
			TokenInvalid    bool       `json:"token_invalid"`
		}{pending, failed, oldestAt, slackStatus.InvalidSince != nil}, nil
	}
}

// settledLegArchiver is what archiveLoop drives; *logs.Archiver satisfies it.
type settledLegArchiver interface {
	ArchiveSettledLegs(ctx context.Context) (int, error)
}

// archiveLoop archives settled legs every interval until ctx ends. A failed
// round is logged and the next one tries again: archiving never takes the
// orchestrator down.
func archiveLoop(ctx context.Context, archiver settledLegArchiver, every time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if n, err := archiver.ArchiveSettledLegs(ctx); err != nil && ctx.Err() == nil {
			log.Error("log archive round failed", "error", err.Error())
		} else if n > 0 {
			log.Info("archived settled legs", "legs", n)
		}
	}
}
