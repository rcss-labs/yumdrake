# Operating Yumdrake

How to run, upgrade, back up and repair a Yumdrake installation. Commands for
Docker Compose are run in `deploy/compose` (see its README for the first
setup). The ECS setup is described at the end.

## Upgrade

1. Read the release notes. Do not skip a release whose notes mention a breaking
   migration: upgrade through each one in order.
2. Back up first (see Backup and restore).
3. Check out (or download) the new release tag, so the Compose files match the
   release. Change `YUMDRAKE_VERSION` in `.env` to the new tag, then:

   ```sh
   docker compose pull --ignore-buildable && docker compose up -d --build
   ```

   `--build` is needed: Caddy is built locally from `deploy/compose/caddy` and
   tagged `yumdrake-caddy:local`; it is not published anywhere, so a plain
   `docker compose pull` would try to fetch that name from a registry.
   `--ignore-buildable` leaves it out. If the images are in a private ghcr.io
   package, run `docker login ghcr.io` first.

The orchestrator applies the schema migrations once, when it starts. The runners
start only after the orchestrator is healthy, and a runner also waits for the
schema version it needs. Migrations are backward compatible across one release,
so the old runners that are still draining during the switch keep working.

**Log archives are encrypted on the volume as of this release.** An archive
directory written by an earlier build (plain gzip) cannot be read. When upgrading
from a pre-release build, empty the log archive directory first:

```sh
docker compose stop orchestrator
docker run --rm -v yumdrake-compose_log-archive:/v alpine:3.22.2 sh -c 'rm -rf /v/* /v/.[!.]*'
```

### Upgrade check

Done by hand on 2026-11-28, once, with the images of the merge base with `main`
(`c04a051`, schema up to migration 00008) as `prev` and this branch as `next`.
CI does not repeat it; repeat it before a release that adds a migration.

What was done:

1. `docker build` both images from each tree as `yumdrake-upgrade/yumdrake-{orchestrator,runner}:{prev,next}`,
   and the demo helper from this branch as `:next` (it serves the fake GitHub and seeds the admin, the
   jobs and the inventory, the cheapest way to get a working system).
2. `docker compose -p yumdrake-upgrade-check` with `deploy/compose/compose.yaml`, `compose.demo.yaml`
   (fake steps, `YUMDRAKE_ENV=dev`, the fake GitHub), plain HTTP on port 18480, a throwaway env file
   and a small override (`pull_policy: never`, demo helper pinned to `:next`), at `YUMDRAKE_VERSION=prev`.
3. Signed in as the admin and started a run of `deploy-backend-api` in `dev` with version
   `v1.4.0-slow` (a fake step that takes 30 seconds).
4. Set `YUMDRAKE_VERSION=next` and ran `up -d --no-build --wait` (38 seconds to all healthy).
5. `down -v --remove-orphans` for that project only, removed the temporary worktree and the images.

Seen:

- Before: migrations 0 to 8 applied. After: 0 to 8 and 10, each once (`goose_db_version`; 00009
  does not exist on this branch). The orchestrator log shows only `applying migrations`, not the
  version numbers, so the table is the evidence.
- All five services healthy after the switch; the runner logged `waiting for schema` (want 10) until
  the new orchestrator had migrated, then started.
- The run was in flight when the services were replaced. The new runner took the leg over as
  attempt 2, and because the fake steps keep their pretend world in memory, which a restart loses, it
  could not see the change and settled the leg `unknown`. The run ended `needs_reconciliation`: not
  lost, and waiting for a person, which is the designed outcome for an interrupted change.
- The admin signed in with the same password after the upgrade.

Limits and finding:

- The `prev` images read no Valkey password (that setting arrived with the Compose setup), so the
  `prev` phase ran Valkey without `requirepass` through an extra override, and the switch to `next`
  recreated Valkey with the password. A real upgrade from a release built before this
  `compose.yaml` therefore does not exist; the next upgrade (from a release that already runs
  under this file) has the password on both sides.
- No real steps and no AWS: the check ran on fake steps only.

## Backup and restore

What holds state: the `postgres-data` database, the `artifacts` volume (config
copies, encrypted, deleted after 30 days) and the `log-archive` volume
(encrypted). The `valkey-data` volume holds only a cache and wake-up signals and
needs no backup. **Also keep `YUMDRAKE_LOCAL_SECRET_KEY` (in `.env`) in a
separate safe place: without it the encrypted data cannot be read.**

Volume names are the Compose project name plus the volume:
`yumdrake-compose_artifacts` and `yumdrake-compose_log-archive`.

### Back up

The dump holds password hashes, encrypted connection secrets and personal data.
Write backups to a private directory outside the checkout:

```sh
BACKUP_DIR=/var/backups/yumdrake
(umask 077 && mkdir -p "$BACKUP_DIR")
docker compose exec -T postgres pg_dump -U yumdrake -Fc yumdrake > "$BACKUP_DIR/yumdrake-db.dump"
for v in artifacts log-archive; do
  docker run --rm -v "yumdrake-compose_$v:/v:ro" -v "$BACKUP_DIR:/b" alpine:3.22.2 \
    tar -czf "/b/yumdrake-$v.tar.gz" -C /v .
done
```

### Restore

Into a stopped application and an empty database:

```sh
docker compose stop orchestrator runner
docker compose exec -T postgres psql -U yumdrake -d postgres \
  -c 'DROP DATABASE yumdrake' -c 'CREATE DATABASE yumdrake'
docker compose exec -T postgres pg_restore -U yumdrake -d yumdrake --no-owner < "$BACKUP_DIR/yumdrake-db.dump"
for v in artifacts log-archive; do
  docker run --rm -v "yumdrake-compose_$v:/v" -v "$BACKUP_DIR:/b" alpine:3.22.2 \
    sh -c "find /v -mindepth 1 -delete && tar -xzf /b/yumdrake-$v.tar.gz -C /v && chown -R 65532:65532 /v"
done
docker compose up -d
```

Use the same `YUMDRAKE_LOCAL_SECRET_KEY` that was in use when the backup was
taken.

## Restarts and reboots

All services restart with the host (`restart: unless-stopped`). Data lives on
named volumes, so `docker compose down` followed by `up -d` keeps runs, logs,
config copies and settings. Never use `down -v`: it deletes the volumes.

## First admin and locked accounts

Create the first admin as in the Compose README. An admin who locked themselves
out by failed sign-ins can be unlocked from the host:

```sh
docker compose exec -T orchestrator /yumdrake-orchestrator unlock --email admin@example.com
```

## Secrets in process arguments

Valkey is started with `--requirepass`, so its password is visible in the Valkey
container's process list. Anyone who can run `docker exec` or `docker top` on
this host can read it; treat access to the Docker socket as full access to
Yumdrake.

## Troubleshooting

| Symptom | Likely cause and fix |
| --- | --- |
| A service is `unhealthy` | `docker compose logs <service>`; most often a wrong password in `.env`. |
| Cannot sign in, "origin" error | `YUMDRAKE_PUBLIC_URL` differs from the browser address. |
| Admin is locked | Run `unlock` (above). |
| No certificate, Caddy ACME errors | DNS or ports 80/443; `docker compose logs caddy`. |
| Old log archives unreadable | Archives from a pre-release build are plain; see Upgrade. |
| After a restore, connections cannot be decrypted | Wrong `YUMDRAKE_LOCAL_SECRET_KEY`. |
| A run was in flight during a restart | It is resumed or reported as failed with its reason; check the run page. |

## ECS

For AWS (ECS Fargate with managed PostgreSQL and Valkey) see
[deploy/terraform/example/README.md](../deploy/terraform/example/README.md).
