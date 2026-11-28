// Package runnerapp wires the runner's real step executors (resolve,
// revalidate, copy-config and ecs-deploy). cmd/runner and the
// end-to-end tests both call RegisterExecutors, so the tests cover the same
// wiring production uses. Only the runner may import it: it builds deployment
// credentials (internal/awsx/deploy), which the orchestrator must never reach.
package runnerapp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rcss-labs/yamdrake/internal/app"
	"github.com/rcss-labs/yamdrake/internal/artifacts"
	"github.com/rcss-labs/yamdrake/internal/awsx"
	"github.com/rcss-labs/yamdrake/internal/awsx/deploy"
	"github.com/rcss-labs/yamdrake/internal/config"
	"github.com/rcss-labs/yamdrake/internal/runner"
	"github.com/rcss-labs/yamdrake/internal/secrets"
	"github.com/rcss-labs/yamdrake/internal/spec"
	"github.com/rcss-labs/yamdrake/internal/steps/copyconfig"
	"github.com/rcss-labs/yamdrake/internal/steps/ecsdeploy"
	"github.com/rcss-labs/yamdrake/internal/steps/resolve"
	"github.com/rcss-labs/yamdrake/internal/steps/revalidate"
	"github.com/rcss-labs/yamdrake/internal/store"
)

// Registrar is what RegisterExecutors needs; *runner.Runner satisfies it.
type Registrar interface {
	Register(kind string, e runner.Executor)
}

// artifactStore holds the encrypted config copies the steps share. *artifacts.Store
// satisfies it.
type artifactStore interface {
	Put(ctx context.Context, runID int64, service, kind, name string, previous bool, data []byte) (key, sha256hex string, err error)
	Get(ctx context.Context, key, wantSHA256 string) ([]byte, error)
}

// RegisterExecutors registers the real executors on r, replacing any fakes
// registered earlier under those names: "resolve", "revalidate",
// "copy-config" and "ecs-deploy". The artifact store is built once and shared
// by the steps that read or write config copies.
func RegisterExecutors(ctx context.Context, log *slog.Logger, cfg config.Runner, pool *pgxpool.Pool, r Registrar) error {
	id, err := newIdentity(ctx, log, cfg, pool)
	if err != nil {
		return err
	}
	artifactClient := s3.NewFromConfig(id.awsConfig, func(o *s3.Options) { o.UsePathStyle = cfg.AWS.UsePathStyleS3() })
	copies := artifacts.New(artifactClient, cfg.ArtifactBucket, cfg.ArtifactKMSKeyID)
	deps := resolveDeps(cfg, pool, id, copies, log)
	r.Register("resolve", resolve.New(deps))
	r.Register("revalidate", revalidate.New(deps))
	r.Register("copy-config", copyconfig.New(copyconfig.Deps{
		AWS:       id.credentials.For,
		Artifacts: copies,
		Options:   cfg.AWS,
	}))
	r.Register("ecs-deploy", ecsdeploy.New(ecsdeploy.Deps{
		AWS:            id.credentials.For,
		Options:        cfg.AWS,
		PollEvery:      cfg.RolloutPoll,
		DefaultTimeout: cfg.RolloutTimeout,
	}))
	return nil
}

// identity is what the runner's real executors are built from: the runner's
// own AWS identity, the secret cipher and the provider of deployment
// credentials.
type identity struct {
	awsConfig   aws.Config
	cipher      secrets.Cipher
	credentials *deploy.Provider
}

// newIdentity builds the provider of deployment credentials from the
// runner's own AWS identity. Only the runner binary imports internal/awsx/deploy
// (internal/awsx/boundary_test.go). Nothing is resolved here, so a runner
// without AWS credentials still starts; a missing KMS region does fail it, as
// it fails the orchestrator, because the secret cipher cannot work.
func newIdentity(ctx context.Context, log *slog.Logger, cfg config.Runner, pool *pgxpool.Pool) (identity, error) {
	awsConfig, err := awsx.BaseConfig(ctx, cfg.AWS)
	if err != nil {
		if cfg.SecretsMode == "kms" {
			return identity{}, err
		}
		log.Warn("the default AWS configuration could not be loaded; deploying to AWS accounts will fail", "error", err.Error())
	}
	cipher, err := app.NewCipher(cfg.Common, awsConfig)
	if err != nil {
		return identity{}, err
	}
	return identity{
		awsConfig:   awsConfig,
		cipher:      cipher,
		credentials: deploy.New(pool, cipher, awsConfig, cfg.AWS).WithLogger(log),
	}, nil
}

// resolveDeps wires the resolve and revalidate executors. It reads the run's own
// inventory snapshot (not the active one), uses the deployment credentials of
// the run's account for ECS, ECR and S3, GitHub through the stored connections,
// and writes config copies to the shared artifact store with the runner's own identity.
func resolveDeps(cfg config.Runner, pool *pgxpool.Pool, id identity, copies artifactStore, log *slog.Logger) resolve.Deps {
	githubClient := app.GitHubClients(pool, id.cipher, cfg.GitHubAPIURL, log)
	return resolve.Deps{
		AWS: id.credentials.For,
		GitHub: func(ctx context.Context, owner string) (resolve.Source, error) {
			client, err := githubClient(ctx, owner)
			if err != nil {
				return nil, err // a fixed message that names the owner and never a credential
			}
			return client, nil
		},
		Artifacts: copies,
		Inventory: func(ctx context.Context, snapshotID int64) (spec.Inventory, error) {
			snapshot, err := store.SnapshotByID(ctx, pool, snapshotID)
			if err != nil {
				return spec.Inventory{}, err
			}
			var inventory spec.Inventory
			if err := json.Unmarshal(snapshot.Inventory, &inventory); err != nil {
				return spec.Inventory{}, fmt.Errorf("inventory snapshot %d does not decode: %w", snapshotID, err)
			}
			return inventory, nil
		},
		SourceRun: func(ctx context.Context, runID int64) (store.Run, error) { return store.GetRun(ctx, pool, runID) },
		Options:   cfg.AWS,
	}
}
