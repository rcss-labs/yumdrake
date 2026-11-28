# Yumdrake

Yumdrake is a purpose-built deployment tool for ECS. It puts the right config files in place and rolls a new image version onto ECS services, a narrow job that general-purpose CI servers handle slowly.

DevOps describe jobs and inventory in YAML. QA, developers and DevOps run those jobs from a web page or a CLI and see what a run will change before it starts.

See the [design spec](docs/superpowers/specs/2026-11-27-yumdrake-design.md).

## Prerequisites

- Go 1.26
- Docker with Compose
- golangci-lint v2 (v2.15 is used in CI)

## Local services

```sh
make dev-up     # start PostgreSQL 17, Valkey 8 and Floci and wait until healthy
make dev-down   # stop them and delete their volumes
```

PostgreSQL listens on `localhost:5432` (database, user and password `yumdrake`). Valkey listens on `localhost:6379`. Floci listens on `localhost:4577`.

### Floci (local AWS)

[Floci](https://floci.io) emulates AWS (STS, IAM, ECR, ECS, S3, KMS) for the tests that need it. It runs as the `floci` Compose service on `127.0.0.1:4577` (port 4566 is deliberately not published, because another Floci may own it). It mounts the Docker socket: ECS tasks and the ECR registry run as sibling containers named `floci-aws-*`, and they are not removed when Floci stops.

```sh
make test-floci    # run every test, including the ones tagged floci (needs make dev-up first)
make floci-clean   # remove the floci-aws-* containers that our Floci created
```

`make test-floci` points the tests at `YUMDRAKE_TEST_AWS_ENDPOINT` (default `http://localhost:4577`). The helpers in `test/awstest` seed accounts, roles, buckets, images and services, and offer a proxy that records calls and injects failures. `make floci-clean` only removes containers attached to the network of the Floci that publishes the endpoint port, never another Floci's. Floci keeps its state in memory: restart it for a clean slate.

## Running the services locally

Build both binaries into `bin/`. `make build` also builds the web application and embeds it, so it needs Node 24 and a one-time `make web-install` first:

```sh
make web-install   # once per clone
make build
```

Point both services at the local services:

```sh
export YUMDRAKE_DATABASE_URL='postgres://yumdrake:yumdrake@localhost:5432/yumdrake?sslmode=disable'
export YUMDRAKE_VALKEY_ADDR=localhost:6379
export YUMDRAKE_LOCAL_SECRET_KEY="$(openssl rand -base64 32)"   # required; keep it the same between runs
export YUMDRAKE_PUBLIC_URL=http://localhost:8080                  # orchestrator only; the address the browser uses

bin/yumdrake-orchestrator   # serves on :8080; applies migrations on start
bin/yumdrake-runner         # health endpoint on 127.0.0.1:8081; waits for the schema
```

- The orchestrator applies database migrations on start (`YUMDRAKE_MIGRATE_ON_START=true` by default).
- The runner never migrates. It waits until the schema is at the expected version.
- The orchestrator serves `/readyz` on port 8080, which reports status `ok` when the service is ready.
- Both services serve `GET /healthz` (liveness, always 200, runs no checks) and `GET /readyz` (readiness, 200 when the `postgres` and `valkey` checks pass, 503 otherwise): the orchestrator on `:8080`, the runner on `127.0.0.1:8081` by default.
- Both binaries accept subcommands: `version` prints the build version, and `healthcheck` runs a health check against the service's own address.

```sh
bin/yumdrake-orchestrator version
bin/yumdrake-orchestrator healthcheck
bin/yumdrake-runner healthcheck
```

## API

The orchestrator serves a JSON API under `/api/v1`. Every request is made by a signed-in user: either a session cookie (from `POST /api/v1/auth/login`, used by the browser) or `Authorization: Bearer <token>` (used by the CLI). See [Signing in](#signing-in) and [CLI tokens and device sign-in](#cli-tokens-and-device-sign-in).

Errors look like `{"error":{"code":"...","message":"..."}}` with codes `unauthorized`, `forbidden`, `not_found`, `invalid`, `conflict`, `too_many_requests`, `unavailable` and `internal`. Request bodies are limited to 1 MB and unknown JSON fields are rejected. No response contains a token, a private key or ciphertext.

| Method and path | Does |
| --- | --- |
| `GET /api/v1/inventory` | Active commit and activation time, last check time, the sync error and its time, a newer rejected commit with its problems, environments, and services with the environments they exist in. |
| `GET /api/v1/inventory/services/{name}` | The full parsed service. |
| `PUT /api/v1/admin/inventory-source` | Set `repo`, `branch` and `path` of the inventory, then sync once. |
| `POST /api/v1/inventory/sync` | Sync the inventory now. |
| `GET /api/v1/folders`, `POST /api/v1/folders` | List folders; create one by `name`. |
| `GET /api/v1/jobs` | Every job source with its folder, commit, sync and check times, runnable flag, problem count and sync error. |
| `POST /api/v1/auth/login`, `POST /api/v1/auth/logout` | Sign in with `email` and `password` (sets the session cookie) and sign out. After 5 failed attempts for one email within 15 minutes, sign-in answers `429` with `Retry-After` until the window passes. |
| `GET /api/v1/me` | The signed-in user: id, email, name, admin flag and grants. |
| `POST /api/v1/jobs` | Register a job file (`folder_id`, `repo`, `branch`, `path`), sync it once and return it. |
| `GET /api/v1/jobs/{id}` | The source, its last good `definition` and its current `problems`. |
| `PATCH /api/v1/jobs/{id}` | Move to another folder (`folder_id`). |
| `POST /api/v1/jobs/{id}/sync` | Sync that job now. |
| `GET /api/v1/admin/github-connections`, `POST /api/v1/admin/github-connections` | List connections without secrets; create one for an `owner` (stored in lower case) with `method` `token` (and `token`) or `app` (and `app_id`, `installation_id`, `private_key`). The secret is encrypted before it is stored. |

```sh
curl -H "Authorization: Bearer $YUMDRAKE_TOKEN" localhost:8080/api/v1/inventory
```

The orchestrator also polls GitHub every `YUMDRAKE_SYNC_INTERVAL` (inventory first, then each job source). A failure to reach GitHub is kept as `sync_error` next to the last good data, which stays in use.

## Signing in

Every `/api/v1` route needs a signed-in user or a CLI token. Without one the API answers `401` with code `unauthorized`. Without the needed permission it answers `403` with code `forbidden`.

### First admin

Create the first admin on the orchestrator's command line, with the same environment as the orchestrator. The password is read from standard input, never from an argument or a variable. It must be at least 12 characters. Take it from your secret store rather than typing it into the shell, so it stays out of shell history.

```sh
printf '%s\n' "$PASSWORD" | yumdrake-orchestrator create-admin --email admin@example.com --name Admin
```

The command creates a local user in the admin group `Administrators` and prints only `created`. It exits 0 on success, 1 on failure (for example, the email already exists) and 2 on a usage error.

### Unlocking an account

Anyone can keep an account locked by sending 5 wrong passwords within 15 minutes, which includes the break-glass admin. An operator clears the failure counter of one email on the orchestrator's command line, with the same environment as the orchestrator:

```sh
yumdrake-orchestrator unlock --email admin@example.com
```

It prints only `unlocked`, writes an `auth.unlock` audit entry, and exits 0 on success, 1 on failure (for example, no user has that email) and 2 on a usage error. The email is matched ignoring case and surrounding spaces.

### Password sign-in

- `POST /api/v1/auth/login` with `{"email": ..., "password": ...}` sets the `yumdrake_session` cookie (`Secure`, `HttpOnly`, `SameSite=Lax`). The session has an idle timeout of 8 hours and a maximum age of 24 hours.
- `POST /api/v1/auth/logout` ends the session.
- `GET /api/v1/me` shows the signed-in user. `GET /api/v1/auth/methods` shows which sign-in methods are on.
- Only `local` and `service` users can use password sign-in.
- After 5 failed attempts for one email within 15 minutes, sign-in answers `429` with `Retry-After` until the window passes.
- State-changing requests that carry the session cookie must come from the public URL: the `Origin` header must equal `YUMDRAKE_PUBLIC_URL`. A script that signs in with a password must send that `Origin` header. Bearer-token requests do not need it.
- If Valkey is unavailable, cookie sign-in answers `503` with "Sign-in is temporarily unavailable". CLI tokens keep working.

### Rate limiting at the edge

These routes need no sign-in and should be rate-limited at the load balancer or ingress: `POST /api/v1/auth/login`, `POST /api/v1/cli/login`, `POST /api/v1/cli/login/token`, `GET /api/v1/auth/saml/start` and `POST /saml/acs`. Yumdrake limits password sign-in per email and caps concurrent password checks, but has no per-client limit.

## CLI tokens and device sign-in

Tokens look like `ymd_...`. They are shown once and stored only as a SHA-256 hash. They expire (90 days by default, at most 365 days) and can be revoked. A token carries its owner's permissions. Send it as `Authorization: Bearer <token>`.

- `GET /api/v1/tokens` and `POST /api/v1/tokens` (body `{"name": ..., "ttl_days": ...}`) list and create your own tokens. `DELETE /api/v1/tokens/{id}` revokes one. Creating and revoking tokens needs a browser session: a request made with a token answers `403` "Sign in with your browser to manage tokens", so a token cannot renew itself. A token can give up its own power: `DELETE /api/v1/tokens/{id}` with the token's own id, or `POST /api/v1/auth/logout`, revokes that token (and only that one); any other id answers `403`.
- A service account has no browser, so a global admin mints its token with `POST /api/v1/admin/users/{id}/tokens` (body `{"name": ..., "ttl_days": ...}`) from a browser session. It works only for users whose source is `service` (otherwise `422` `invalid`), is audited with the token's id and name (never its value) and returns the plaintext once, in the shape of `POST /api/v1/tokens`.
- A token of a SAML user works only while that user's last SSO sign-in is at most `YUMDRAKE_TOKEN_SSO_MAX_AGE` old (default 168 hours). After that it answers `401` "Sign in again with SSO: yumdrake login". Tokens of `local` and `service` users are not affected.
- Disabling a user revokes all of that user's tokens; enabling the user again does not bring them back.

Device sign-in for the CLI:

1. `POST /api/v1/cli/login` returns `device_code`, `user_code`, `verify_url` and `interval`.
2. A signed-in user confirms the 8-character `user_code` in the browser (`POST /api/v1/cli/login/confirm`).
3. The CLI polls `POST /api/v1/cli/login/token` with the `device_code`, no faster than every `interval` seconds. It receives a token once.

Codes expire after 10 minutes. A session may make at most 10 confirm attempts per 10 minutes; more answer `429` with `Retry-After`.

## Single sign-on (SAML)

A global admin configures SSO with `PUT /api/v1/admin/sso`; `manage_access` alone is not enough, because whoever controls the identity provider can sign in as anyone (a non-admin gets `403`). Holders of `manage_access` can read the settings (`GET /api/v1/admin/sso`) and check them (`POST /api/v1/admin/sso/test`).

Give the identity provider:

- Service-provider metadata: `<YUMDRAKE_PUBLIC_URL>/saml/metadata`
- Assertion consumer service URL (HTTP-POST binding): `<YUMDRAKE_PUBLIC_URL>/saml/acs`
- Entity ID (default `<YUMDRAKE_PUBLIC_URL>/saml/metadata`)

Paste the identity provider's metadata XML into the settings as `idp_metadata_xml`. Yumdrake does not fetch it from a URL.

Attributes: groups come from the `groups` attribute and the display name from `displayName` (both configurable). The email comes from the NameID unless an email attribute is set.

Requirements on the identity provider:

- It must sign the response or the assertion with SHA-256 or stronger. SHA-1 is refused.
- Encrypted assertions are not supported.
- Only sign-ins started from Yumdrake are accepted. Identity-provider-initiated sign-in is refused.

Group mapping: map identity-provider groups to Yumdrake groups at `/api/v1/admin/idp-mappings`. Memberships from the identity provider are replaced at every SSO sign-in. Unmapped groups are ignored, and memberships added by an admin by hand are kept. Sessions last at most 24 hours, so a person removed from a group at the identity provider loses it a day later at the latest.

The service-provider signing key and certificate are generated on first use and stored encrypted.

Keep the local admin from `create-admin` as a break-glass account.

## Access administration

Users, groups, grants, identity-provider mappings and the audit log live under `/api/v1/admin/`. `users`, `groups`, `grants` and `idp-mappings` need `manage_access`; `audit` needs `view_audit`.

Actions: `view`, `view_logs`, `run`, `cancel`, `approve`, `restore`, `manage_jobs`, `manage_connections`, `manage_access` and `view_audit`. Every signed-in user has `view`. A group marked admin has every action.

`manage_access` is not admin. Only a global admin may touch admin groups, their members and mappings, or the SSO settings. A holder of `manage_access` who is not an admin may create or delete a grant, add a user to a group, or map an identity-provider group to a group only for an action and scope they hold themselves (a grant wider than their own answers `403`), and grants of `manage_access`, `manage_connections` and `view_audit` can be created or deleted only by an admin.

A grant gives a group one action, optionally limited to a label (`dev`, `preprod` or `prod`) or to one environment, and optionally to one folder.

At least one enabled admin must remain; otherwise the change is refused with `409`.

Every change is written to the append-only audit log, which never contains passwords, tokens or keys.

## Web application

The web application lives in `web/` (React, TypeScript and Vite). It needs Node 24 and npm.

```sh
make web-install   # install dependencies from the lockfile
make web-lint      # ESLint and the TypeScript check
make web-test      # unit tests, run once
make web-build     # production build, copied into internal/webui/dist for embedding
```

For development, run the orchestrator on port 8080 and start the dev server with `npm --prefix web run dev`. The dev server proxies `/api` and `/saml` to the orchestrator on `localhost:8080`.

With the dev server on `http://localhost:5173`, sign-in answers 403 unless the orchestrator runs with `YUMDRAKE_PUBLIC_URL=http://localhost:5173`, because the API checks the `Origin` of every write against the public URL.

### Browser test

```sh
make dev-up                                          # PostgreSQL and Valkey
(cd web && npx playwright install chromium)          # once; add --with-deps on a bare Linux machine
make web-e2e                                         # builds, then runs the test in Chromium
```

`make web-e2e` runs `web/e2e/foundation.spec.ts` against the real orchestrator binary (`bin/yumdrake-orchestrator`, with the web application embedded), not the Vite dev server. The helper `test/e2e/webstack` starts it on free ports with its own database (`yumdrake_web_e2e_*`, dropped at the end) and its own Valkey key prefix, plus the fake GitHub from `test/e2e/fakegithub` holding `internal/spec/testdata/valid`. It needs the Compose services (or `YUMDRAKE_TEST_DATABASE_URL` and `YUMDRAKE_TEST_VALKEY_ADDR` pointing at others). The helper stops the orchestrator and drops the database when the test ends, fails or is interrupted. Set `YUMDRAKE_E2E_SHOTS_DIR` to a directory to keep the eight full-page screenshots the test takes.

## Testing

```sh
make lint               # gofmt, go vet and golangci-lint
make test               # unit tests with the race detector
make test-integration   # integration tests; needs `make dev-up` first
make test-e2e           # end-to-end tests: in-process orchestrators and runners with fake steps, a fake GitHub; needs `make dev-up` first
make smoke              # builds the images and starts the Compose "app" profile, then checks that it is healthy
```

`make smoke` needs Docker. It removes its containers and volumes on exit. See `scripts/smoke.sh -h` for its exit codes.

## Trying a run with fake steps

The runner runs the real `copy-config` and `ecs-deploy` steps; with fake steps only those two pretend. Resolving and revalidating are real either way, so a dev run still needs AWS access (Floci works), a GitHub connection for the config owner and `YUMDRAKE_ARTIFACT_BUCKET`. Start the runner with both of these (it refuses `YUMDRAKE_FAKE_STEPS=true` in any other environment, and production defaults keep them off):

```sh
YUMDRAKE_ENV=dev YUMDRAKE_FAKE_STEPS=true bin/yumdrake-runner
```

Then plan and start a run from the web application or `POST /api/v1/jobs/{id}/runs`. The version you choose for a service decides how its `ecs-deploy` step ends:

| Version contains | The step |
| --- | --- |
| `fail` | fails with `health checks failed` |
| `unknown` | ends `unknown`; the run waits in `needs_reconciliation` holding its locks until a person reconciles the leg |
| `slow` | takes 30 seconds (useful for watching the live log, cancelling, or stopping a runner mid-step) |
| anything else | succeeds after about 2 seconds |

An environment whose name contains `stale` fails the revalidation just before the steps run, so the run ends `invalidated` without changing anything. Every run keeps its live log (`GET /api/v1/runs/{id}/events`) and its stored log (`GET /api/v1/runs/{id}/log`).

The Compose "app" profile runs the runner this way only under `make smoke`, through `YUMDRAKE_RUNNER_ENV` and `YUMDRAKE_RUNNER_FAKE_STEPS` (default `prod` and `false`).

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Clean shutdown. |
| `1` | Start-up or serve failure. |
| `2` | Configuration or usage error. |
| `130` | A second SIGINT or SIGTERM arrived during graceful shutdown and forced an immediate exit. |

## Environment variables

Every variable starts with `YUMDRAKE_`. Secrets are never logged; a database URL is only printed with its password replaced by `REDACTED`.

| Variable | Used by | Default | Rule |
| --- | --- | --- | --- |
| `YUMDRAKE_DATABASE_URL` | orchestrator, runner | none (required) | Must use the `postgres://` or `postgresql://` scheme and include a host. The query string must parse. |
| `YUMDRAKE_VALKEY_ADDR` | orchestrator, runner | none (required) | `host:port`, with a port from 1 to 65535. |
| `YUMDRAKE_LOG_LEVEL` | orchestrator, runner | `info` | One of `debug`, `info`, `warn`, `error`. |
| `YUMDRAKE_SHUTDOWN_TIMEOUT` | orchestrator, runner | `20s` | Go duration greater than zero. |
| `YUMDRAKE_STARTUP_TIMEOUT` | orchestrator, runner | `60s` | Go duration greater than zero. Bounds the database wait (both) and the schema wait (runner). |
| `YUMDRAKE_MIGRATE_TIMEOUT` | orchestrator | `10m` | Go duration greater than zero. Bounds the schema migration, separately from `YUMDRAKE_STARTUP_TIMEOUT`. |
| `YUMDRAKE_HTTP_ADDR` | orchestrator | `:8080` | `host:port`, with a port from 1 to 65535. Also read by `healthcheck`. |
| `YUMDRAKE_MIGRATE_ON_START` | orchestrator | `true` | Must be `true` or `false`. |
| `YUMDRAKE_HEALTH_ADDR` | runner | `127.0.0.1:8081` | `host:port`, with a port from 1 to 65535. Also read by `healthcheck`. |
| `YUMDRAKE_RUNNER_ID` | runner | the host name | Identifies the runner in its logs. If unset and the host name cannot be determined, startup fails. |
| `YUMDRAKE_ARTIFACT_BUCKET` | runner | none, required | S3 bucket (3 to 63 lowercase letters, digits, `-`, `.`) that holds encrypted copies of the config files a run touches, under `manifests/<run>/<service>/<kind>/`. |
| `YUMDRAKE_ARTIFACT_KMS_KEY_ID` | runner | empty (S3-managed encryption) | KMS key id, ARN or alias used to encrypt those copies instead of S3-managed keys. |
| `YUMDRAKE_SECRETS_MODE` | orchestrator, runner | `local` | `local` or `kms`. With `kms` the secrets are encrypted by an AWS KMS key (`YUMDRAKE_KMS_KEY_ID`) using the default AWS credential chain and region (`AWS_REGION`); the purpose of each secret is the KMS encryption context. Any other value is a config error. |
| `YUMDRAKE_LOCAL_SECRET_KEY` | orchestrator, runner | none (required when the mode is `local`) | Base64 of exactly 32 bytes (for example `openssl rand -base64 32`). Encrypts stored GitHub credentials. For development and tests only; never printed. |
| `YUMDRAKE_KMS_KEY_ID` | orchestrator, runner | none (required when the mode is `kms`) | Id, ARN or alias of the KMS key that encrypts stored secrets (GitHub and AWS credentials). In `kms` mode the orchestrator role's `kms:Decrypt` grant must carry a condition on `kms:EncryptionContext:purpose` that excludes `aws-deploy`; only the runner's grant may decrypt `aws-deploy`. |
| `YUMDRAKE_PUBLIC_URL` | orchestrator | none (required) | Address users reach the service at, such as `https://yumdrake.example.com`: scheme, host and optional port only. Must be `https://`, except `http://` for `localhost` and `127.0.0.1`. Requests that carry a session cookie and change state must come from this origin. |
| `YUMDRAKE_VALKEY_KEY_PREFIX` | orchestrator | empty | At most 64 of letters, digits, `-`, `_`, `:`. Put in front of the Valkey keys of sessions and sign-in counters; set it only to separate services (or test runs) that share one Valkey. |
| `YUMDRAKE_TOKEN_SSO_MAX_AGE` | orchestrator | `168h` | Go duration of at least `1h`. How recent a SAML user's last SSO sign-in must be for their CLI tokens to work. |
| `YUMDRAKE_SYNC_INTERVAL` | orchestrator | `60s` | Go duration of at least `10s`. |
| `YUMDRAKE_GITHUB_API_URL` | orchestrator, runner | `https://api.github.com` | An `http://` or `https://` URL. Overridden in tests. |
| `YUMDRAKE_AWS_ENDPOINT_URL` | orchestrator, runner | empty (real AWS) | An `http://` or `https://` URL that all AWS calls go to, for a local emulator such as Floci. |
| `YUMDRAKE_SWEEP_INTERVAL` | orchestrator | `2s` | Go duration greater than zero. How often the run engine's fallback timer gives expired leases to another runner, expires waiting runs and advances every active run. Wake-ups through Valkey make runs move sooner; the timer alone is enough when Valkey is down. |
| `YUMDRAKE_LIMIT_PER_ENVIRONMENT` | orchestrator | `10` | Positive integer. At most this many legs of other runs run at once in one environment. A soft limit: orchestrators advancing two runs at the same moment may overshoot it briefly. |
| `YUMDRAKE_LIMIT_PER_ACCOUNT` | orchestrator | `20` | Positive integer. The same limit for one AWS account. |
| `YUMDRAKE_LOG_ARCHIVE_DIR` | orchestrator | empty (no archiving) | Absolute path of a directory. When set, the logs of legs settled longer than `YUMDRAKE_LOG_ARCHIVE_AFTER` are moved there from PostgreSQL and are still served by the API. |
| `YUMDRAKE_LOG_BUCKET` | orchestrator | empty (no S3 archive) | S3 bucket (3 to 63 lowercase letters, digits, `-`, `.`) that settled legs' logs are archived to, under `runs/<run>/legs/<leg>.ndjson.gz`, encrypted with S3-managed keys. Uses Yumdrake's own AWS identity. Replaces `YUMDRAKE_LOG_ARCHIVE_DIR`: setting both is a configuration error. |
| `YUMDRAKE_LOG_ARCHIVE_INTERVAL` | orchestrator | `1m` | Go duration greater than zero. How often settled legs are archived. Used only with `YUMDRAKE_LOG_ARCHIVE_DIR` or `YUMDRAKE_LOG_BUCKET`. |
| `YUMDRAKE_LOG_ARCHIVE_AFTER` | orchestrator | `1h` | Go duration. How long a leg must have been settled before its log is archived. Used only with `YUMDRAKE_LOG_ARCHIVE_DIR` or `YUMDRAKE_LOG_BUCKET`. |
| `YUMDRAKE_SLACK_API_URL` | orchestrator | `https://slack.com/api` | An `http://` or `https://` URL with a host. The Slack Web API that notifications are posted to. Overridden in tests. |
| `YUMDRAKE_NOTIFY_INTERVAL` | orchestrator | `5s` | Go duration greater than zero. How often the sender looks for notifications that are due. |
| `YUMDRAKE_ENV` | runner | `prod` | `dev` or `prod`. Fake steps are allowed only with `dev`. |
| `YUMDRAKE_FAKE_STEPS` | runner | `false` | `true` or `false`. When `true` (only with `YUMDRAKE_ENV=dev`; the runner refuses to start otherwise) the runner executes pretend steps that change nothing real. See [Trying a run with fake steps](#trying-a-run-with-fake-steps). |
| `YUMDRAKE_RUNNER_CONCURRENCY` | runner | `8` | Positive integer. How many legs one runner executes at once. |
| `YUMDRAKE_LEASE` | runner | `30s` | Go duration. How long a claimed leg belongs to a runner without a heartbeat. After that another runner may take it (at most 5 times, then the leg becomes `unknown`). |
| `YUMDRAKE_HEARTBEAT` | runner | `10s` | Go duration; must be shorter than `YUMDRAKE_LEASE`. How often the runner renews the leases of its legs. |
| `YUMDRAKE_RUNNER_POLL` | runner | `2s` | Go duration. How often the runner looks for queued legs when no wake-up arrived. |
| `YUMDRAKE_ROLLOUT_POLL` | runner | `5s` | Go duration greater than zero. How often `ecs-deploy` checks a service's rollout. |
| `YUMDRAKE_ROLLOUT_TIMEOUT` | runner | `15m` | Go duration greater than zero. How long `ecs-deploy` waits for a rollout before it reports the rollout unconfirmed. |
| `YUMDRAKE_TEST_DATABASE_URL` | integration tests | none (required with `-tags integration`) | Same format as `YUMDRAKE_DATABASE_URL`. Set by `make test-integration` and CI. |
| `YUMDRAKE_TEST_VALKEY_ADDR` | integration tests | none (required with `-tags integration`) | Same format as `YUMDRAKE_VALKEY_ADDR`. Set by `make test-integration` and CI. |
| `YUMDRAKE_TEST_AWS_ENDPOINT` | Floci tests | none (required with `-tags floci`) | URL of Floci, `http://localhost:4577`. Set by `make test-floci`. |

## Validating YAML

`make build` (after `make web-install`, see above) produces `bin/yumdrake`, the CLI that checks an inventory before it is synced:

```sh
bin/yumdrake validate <inventory-dir> [--job <file>]...
```

The inventory directory holds `environments.yaml` and one `services/*.yaml` file per service. Each `--job` file is parsed and checked against that inventory. Job files are cross-checked only when the inventory itself is valid.

- Each problem is printed to stderr as `file:line: message`, sorted by file then line.
- On success stdout shows `OK: 5 environments, 2 services, 1 jobs`. On failure it shows `FAILED: N problems`.

Exit codes: `0` valid, `1` problems found or the directory is missing, `2` usage error (no directory, an unknown command, or bad flags).

```sh
make web-install   # once, if not done yet
make build
bin/yumdrake validate internal/spec/testdata/valid --job internal/spec/testdata/valid/jobs/deploy-backend.yaml
```

The JSON Schemas in `schemas/` (`environments.schema.json`, `service.schema.json`, `job.schema.json`) exist for editor completion only. The Go validator is the authority: a file the schema accepts can still fail `validate`. To get completion in an editor with the YAML language server, put a modeline at the top of a file:

```yaml
# yaml-language-server: $schema=https://yumdrake.dev/schemas/service.schema.json
apiVersion: yumdrake/v1
kind: Service
```

## CI

The GitHub Actions workflow (`.github/workflows/ci.yml`) runs lint, test, integration, floci, smoke and web jobs on pull requests and on pushes to `main`.
