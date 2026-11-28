#!/usr/bin/env bash
# Compose check: runs the production Compose setup (deploy/compose) for real.
set -euo pipefail

usage() {
	cat <<'USAGE'
Usage: scripts/compose-check.sh [-h]

Builds the orchestrator and runner images locally (as `make images` does,
tagged yumdrake-check/yumdrake-*:check) and the Caddy front, writes a throwaway
.env with generated secrets (never printed) and runs deploy/compose under its
own Compose project (yumdrake-compose-check). Only that project is ever torn
down; the development stack and other projects are never touched.

Plain mode (compose.yaml only, public URL http://localhost:<port>):
  - the stack comes up healthy
  - an admin is created through `create-admin` (password on stdin)
  - the admin signs in through the API and /readyz reports ok
  - only the orchestrator publishes a port
  - `down` (volumes kept) and `up` again: the same admin still signs in
TLS mode (compose.yaml + compose.tls.yaml, host name localhost, Caddy's
internal CA):
  - /readyz and the sign-in page answer over HTTPS (verified against Caddy's
    own root certificate)
  - only caddy publishes ports; the orchestrator publishes none
On exit the project is removed with `down -v --remove-orphans`.

Environment (defaults): YUMDRAKE_COMPOSE_CHECK_PORT=18180,
YUMDRAKE_COMPOSE_CHECK_TLS_PORT=18543,
YUMDRAKE_COMPOSE_CHECK_REDIRECT_PORT=18081.

Exit codes:
  0  all checks passed
  1  a check failed
  2  usage error
  3  Docker is not available
USAGE
}

while getopts ":h" opt; do
	case "$opt" in
	h)
		usage
		exit 0
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
done
if [ "$OPTIND" -le "$#" ]; then
	usage >&2
	exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if ! docker info >/dev/null 2>&1; then
	echo "compose-check: Docker is not available" >&2
	exit 3
fi
for tool in openssl curl; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "compose-check: $tool is required" >&2
		exit 1
	fi
done

project="yumdrake-compose-check"
http_port="${YUMDRAKE_COMPOSE_CHECK_PORT:-18180}"
tls_port="${YUMDRAKE_COMPOSE_CHECK_TLS_PORT:-18543}"
redirect_port="${YUMDRAKE_COMPOSE_CHECK_REDIRECT_PORT:-18081}"
compose_dir="$repo_root/deploy/compose"

port_in_use() {
	if command -v lsof >/dev/null 2>&1; then
		lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
	else
		(exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
	fi
}

for p in "$http_port" "$tls_port" "$redirect_port"; do
	if port_in_use "$p"; then
		echo "compose-check: FAIL: host port $p is already in use; set YUMDRAKE_COMPOSE_CHECK_PORT, YUMDRAKE_COMPOSE_CHECK_TLS_PORT or YUMDRAKE_COMPOSE_CHECK_REDIRECT_PORT" >&2
		exit 1
	fi
done

work_dir="$(mktemp -d)"
env_file="$work_dir/.env"
cookie_jar="$work_dir/cookies"
failures=0

compose() {
	docker compose -p "$project" --project-directory "$compose_dir" --env-file "$env_file" "$@"
}
compose_plain() { compose -f "$compose_dir/compose.yaml" "$@"; }
compose_tls() { compose -f "$compose_dir/compose.yaml" -f "$compose_dir/compose.tls.yaml" "$@"; }

cleanup() {
	# Only this project, and only here.
	compose_tls down -v --remove-orphans >/dev/null 2>&1 || true
	compose_plain down -v --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$work_dir"
}
trap cleanup EXIT

pass() { echo "compose-check: PASS: $1"; }
fail() {
	echo "compose-check: FAIL: $1" >&2
	failures=$((failures + 1))
}
# check "description" command...   (runs the command, records PASS or FAIL)
check() {
	local what="$1"
	shift
	if "$@" >/dev/null 2>&1; then
		pass "$what"
	else
		fail "$what"
	fi
}
# abort stops the run at once: later checks would only repeat the same failure.
abort() {
	fail "$1"
	# The TLS file set is a superset, so this also shows the front when it ran.
	compose_tls ps >&2 || true
	compose_tls logs --tail 40 >&2 || true
	exit 1
}

# --- images -----------------------------------------------------------------
echo "compose-check: building the images"
for svc in orchestrator runner; do
	docker build -q -f "deploy/docker/$svc.Dockerfile" \
		--build-arg VERSION=check --build-arg COMMIT=check \
		-t "yumdrake-check/yumdrake-$svc:check" . >/dev/null || {
		fail "build of the $svc image"
		exit 1
	}
done

# --- throwaway .env ---------------------------------------------------------
admin_email="admin@example.test"
admin_password="$(openssl rand -hex 16)"
umask 077
{
	echo "YUMDRAKE_IMAGE_REPO=yumdrake-check"
	echo "YUMDRAKE_VERSION=check"
	echo "YUMDRAKE_PUBLIC_HOST=localhost"
	echo "YUMDRAKE_HTTP_BIND=127.0.0.1"
	echo "YUMDRAKE_HTTP_PORT=$http_port"
	echo "YUMDRAKE_HTTPS_PORT=$tls_port"
	echo "YUMDRAKE_HTTP_REDIRECT_PORT=$redirect_port"
	echo "YUMDRAKE_DATABASE_PASSWORD=$(openssl rand -hex 16)"
	echo "YUMDRAKE_VALKEY_PASSWORD=$(openssl rand -hex 16)"
	echo "YUMDRAKE_LOCAL_SECRET_KEY=$(openssl rand -base64 32)"
	echo "YUMDRAKE_RUNNER_REPLICAS=2"
} >"$env_file"

set_public_url() { echo "YUMDRAKE_PUBLIC_URL=$1" >>"$env_file"; }

# --- helpers ----------------------------------------------------------------
# sign_in <base-url> [curl args...]: 0 when the API sets a session cookie.
sign_in() {
	local base="$1"
	shift
	rm -f "$cookie_jar"
	local body
	body="$(printf '{"email":"%s","password":"%s"}' "$admin_email" "$admin_password")"
	local code
	code="$(curl -sS -o /dev/null -w '%{http_code}' -c "$cookie_jar" \
		-H "Origin: $base" -H 'Content-Type: application/json' \
		-d "$body" "$@" "$base/api/v1/auth/login")" || return 1
	[ "$code" = "200" ] && grep -q yumdrake_session "$cookie_jar"
}

readyz_ok() {
	local out
	out="$(curl -fsS "$@")" || return 1
	[[ "$out" == *'"status":"ok"'* || "$out" == *'"status": "ok"'* ]]
}

published_services() {
	# Services that publish at least one port, one per line.
	compose "$@" config --format json |
		python3 -c 'import json,sys
cfg=json.load(sys.stdin)
for name,svc in sorted(cfg["services"].items()):
    if svc.get("ports"):
        print(name)'
}

# --- plain mode -------------------------------------------------------------
plain_url="http://localhost:$http_port"
set_public_url "$plain_url"

echo "compose-check: plain mode: starting the stack"
compose_plain up -d --wait --wait-timeout 180 >/dev/null 2>&1 || abort "plain mode: the stack did not become healthy"
pass "plain mode: the stack is healthy"

if printf '%s\n' "$admin_password" |
	compose_plain exec -T orchestrator /yumdrake-orchestrator create-admin --email "$admin_email" --name "Compose Check" >/dev/null 2>&1; then
	pass "plain mode: admin created from stdin"
else
	abort "plain mode: create-admin failed"
fi

check "plain mode: the admin signs in through the API" sign_in "$plain_url"
check "plain mode: /readyz reports ok" readyz_ok "$plain_url/readyz"

if [ "$(published_services -f "$compose_dir/compose.yaml")" = "orchestrator" ]; then
	pass "plain mode: only the orchestrator publishes a port"
else
	fail "plain mode: services publishing ports: $(published_services -f "$compose_dir/compose.yaml" | tr '\n' ' ')"
fi

echo "compose-check: plain mode: down and up again (volumes kept)"
compose_plain down >/dev/null 2>&1 || abort "plain mode: down failed"
compose_plain up -d --wait --wait-timeout 180 >/dev/null 2>&1 || abort "plain mode: the stack did not come back healthy"
pass "plain mode: the stack came back healthy"
check "plain mode: the same admin signs in after down and up" sign_in "$plain_url"

# --- TLS mode ---------------------------------------------------------------
echo "compose-check: TLS mode: starting the stack with the Caddy front"
tls_url="https://localhost:$tls_port"
# The last assignment in an env file wins.
set_public_url "$tls_url"
compose_tls up -d --build --wait --wait-timeout 180 >/dev/null 2>&1 || abort "TLS mode: the stack did not become healthy"
pass "TLS mode: the stack is healthy"

# Verify against Caddy's own internal root, copied out of its data volume.
root_pem="$work_dir/caddy-root.pem"
for _ in $(seq 1 20); do
	if compose_tls cp caddy:/data/caddy/pki/authorities/local/root.crt "$root_pem" >/dev/null 2>&1; then
		break
	fi
	sleep 1
done
if [ -s "$root_pem" ]; then
	pass "TLS mode: Caddy's internal root certificate extracted"
	tls_args=(--cacert "$root_pem" --resolve "localhost:$tls_port:127.0.0.1")
else
	fail "TLS mode: could not extract Caddy's root certificate"
	tls_args=(-k)
fi

check "TLS mode: /readyz reports ok over HTTPS" readyz_ok "${tls_args[@]}" "$tls_url/readyz"
check "TLS mode: the sign-in page answers over HTTPS" \
	curl -fsS -o /dev/null -H 'Accept: text/html' "${tls_args[@]}" "$tls_url/"
check "TLS mode: the admin signs in over HTTPS" sign_in "$tls_url" "${tls_args[@]}"
hsts_sent() {
	curl -fsSI "$@" | grep -qi '^strict-transport-security:'
}
check "TLS mode: HSTS header is sent" hsts_sent "${tls_args[@]}" "$tls_url/"

tls_published="$(published_services -f "$compose_dir/compose.yaml" -f "$compose_dir/compose.tls.yaml")"
if [ "$tls_published" = "caddy" ]; then
	pass "TLS mode: only caddy publishes ports"
else
	fail "TLS mode: services publishing ports: $(echo "$tls_published" | tr '\n' ' ')"
fi

if [ "$failures" -ne 0 ]; then
	echo "compose-check: $failures check(s) failed" >&2
	exit 1
fi
echo "compose-check: all checks passed"
