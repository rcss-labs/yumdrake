#!/usr/bin/env bash
# Demo check: runs the demo (deploy/compose/compose.demo.yaml) for real, through the API and the CLI.
set -euo pipefail

usage() {
	cat <<'USAGE'
Usage: scripts/demo-check.sh [-h]

Builds and starts the demo under its own Compose project (yumdrake-demo-check)
on host port ${YUMDRAKE_DEMO_CHECK_PORT:-18380}, with the public demo settings
of deploy/compose/demo/demo.env. Only that project is ever torn down; the
development stack, the demo you may be running (project yumdrake-demo) and
other projects are never touched. Checks:
  - the stack comes up healthy, which means the demo helper has seeded it
  - the admin, the deployer and the viewer each sign in through the API
  - the viewer is refused when planning a run (403) and the deployer is not
  - the CLI, built from this checkout and signed in as the deployer with a
    token made through the API, lists the jobs, plans a job in dev, runs it
    with --yes and watches it succeed on the fake steps
On exit the project is removed with `down -v --remove-orphans`, and the script
checks that no container of the project remains.

Environment (default): YUMDRAKE_DEMO_CHECK_PORT=18380.

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
	echo "demo-check: Docker is not available" >&2
	exit 3
fi
for tool in curl python3 go; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "demo-check: $tool is required" >&2
		exit 1
	fi
done

project="yumdrake-demo-check"
port="${YUMDRAKE_DEMO_CHECK_PORT:-18380}"
compose_dir="$repo_root/deploy/compose"
base_url="http://localhost:$port"

port_in_use() {
	if command -v lsof >/dev/null 2>&1; then
		lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
	else
		(exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
	fi
}
if port_in_use "$port"; then
	echo "demo-check: FAIL: host port $port is already in use; set YUMDRAKE_DEMO_CHECK_PORT" >&2
	exit 1
fi

work_dir="$(mktemp -d)"
env_file="$work_dir/demo.env"
failures=0
started=0

# The public demo settings, with this check's own port. The last assignment in an env file wins.
grep -vE '^(YUMDRAKE_HTTP_PORT|YUMDRAKE_PUBLIC_URL)=' "$compose_dir/demo/demo.env" >"$env_file"
{
	echo "YUMDRAKE_HTTP_PORT=$port"
	echo "YUMDRAKE_PUBLIC_URL=$base_url"
} >>"$env_file"

compose() {
	docker compose -p "$project" --project-directory "$compose_dir" --env-file "$env_file" \
		-f "$compose_dir/compose.yaml" -f "$compose_dir/compose.demo.yaml" "$@"
}

pass() { echo "demo-check: PASS: $1"; }
fail() {
	echo "demo-check: FAIL: $1" >&2
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
	if [ "$started" -eq 1 ]; then
		compose ps >&2 || true
		compose logs --tail 40 >&2 || true
	fi
	exit 1
}

cleanup() {
	local status=$?
	# Only this project, and only here.
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$work_dir"
	local left
	left="$(docker ps -a --filter "label=com.docker.compose.project=$project" -q 2>/dev/null | wc -l | tr -d ' ')"
	if [ "$left" = "0" ]; then
		echo "demo-check: PASS: no container of project $project remains"
	else
		echo "demo-check: FAIL: $left container(s) of project $project remain" >&2
		status=1
	fi
	exit "$status"
}
trap cleanup EXIT

# json_field <field> reads a JSON object on standard input and prints one top-level field.
json_field() {
	python3 -c 'import json,sys
print(json.load(sys.stdin)[sys.argv[1]])' "$1"
}

# sign_in <cookie-jar> <email> <password>: 0 when the API sets a session cookie.
sign_in() {
	local jar="$1" body code
	body="$(printf '{"email":"%s","password":"%s"}' "$2" "$3")"
	rm -f "$jar"
	code="$(curl -sS -o /dev/null -w '%{http_code}' -c "$jar" \
		-H "Origin: $base_url" -H 'Content-Type: application/json' \
		-d "$body" "$base_url/api/v1/auth/login")" || return 1
	[ "$code" = "200" ] && grep -q yumdrake_session "$jar"
}

# status_of <cookie-jar> <method> <path> <json-body>: prints the HTTP status.
status_of() {
	curl -sS -o /dev/null -w '%{http_code}' -b "$1" -X "$2" \
		-H "Origin: $base_url" -H 'Content-Type: application/json' \
		-d "$4" "$base_url$3"
}

# --- loopback only ----------------------------------------------------------
# The demo uses public credentials, so it must publish on 127.0.0.1 even when the
# environment asks for more.
published_host_ip() {
	YUMDRAKE_HTTP_BIND=0.0.0.0 compose config --format json |
		python3 -c 'import json,sys
cfg=json.load(sys.stdin)
for port in cfg["services"]["orchestrator"].get("ports", []):
    print(port.get("host_ip", ""))'
}
host_ips="$(published_host_ip 2>/dev/null)" || host_ips=""
if [ "$host_ips" = "127.0.0.1" ]; then
	pass "the orchestrator publishes only on 127.0.0.1, even with YUMDRAKE_HTTP_BIND=0.0.0.0"
else
	fail "the orchestrator's published host_ip is '$(echo "$host_ips" | tr '\n' ' ')' with YUMDRAKE_HTTP_BIND=0.0.0.0, want only 127.0.0.1"
	exit 1
fi

# --- start ------------------------------------------------------------------
echo "demo-check: building and starting the demo (project $project, port $port)"
started=1
compose up -d --build --wait --wait-timeout 600 >/dev/null 2>&1 || abort "the demo did not become healthy"
pass "the demo is healthy (the helper has seeded it)"

admin_jar="$work_dir/admin.jar"
deployer_jar="$work_dir/deployer.jar"
viewer_jar="$work_dir/viewer.jar"
check "the admin signs in through the API" sign_in "$admin_jar" admin@demo.yumdrake.test demo-admin-password
check "the deployer signs in through the API" sign_in "$deployer_jar" deployer@demo.yumdrake.test demo-deployer-password
check "the viewer signs in through the API" sign_in "$viewer_jar" viewer@demo.yumdrake.test demo-viewer-password
[ "$failures" -eq 0 ] || abort "a demo user could not sign in; the checks that follow need all three"

# --- permissions ------------------------------------------------------------
job_id="$(curl -fsS -b "$admin_jar" "$base_url/api/v1/jobs" |
	python3 -c 'import json,sys
for job in json.load(sys.stdin)["jobs"]:
    if job["job_id"] == "deploy-backend-api":
        print(job["id"])')" || true
[ -n "$job_id" ] || abort "the job deploy-backend-api is not in the demo"
run_body='{"environment":"dev","selection":{"api":{"services":{"backend-api":"v1.4.0"}}},"start":false}'

viewer_status="$(status_of "$viewer_jar" POST "/api/v1/jobs/$job_id/runs" "$run_body")" || viewer_status="error"
if [ "$viewer_status" = "403" ]; then
	pass "the viewer is refused when planning a run (403)"
else
	fail "the viewer planning a run got status $viewer_status, want 403"
fi
planned_run="$(curl -sS -w '\n%{http_code}' -b "$deployer_jar" -X POST \
	-H "Origin: $base_url" -H 'Content-Type: application/json' \
	-d "$run_body" "$base_url/api/v1/jobs/$job_id/runs")" || planned_run=""
deployer_status="${planned_run##*$'\n'}"
if [ "$deployer_status" = "201" ]; then
	pass "the deployer may plan a run in dev (201)"
else
	fail "the deployer planning a run got status $deployer_status, want 201"
fi
planned_run_id="$(printf '%s' "${planned_run%$'\n'*}" | json_field id 2>/dev/null)" || planned_run_id=""
if [ -z "$planned_run_id" ]; then
	fail "no planned run to try starting as the viewer"
else
	viewer_start="$(status_of "$viewer_jar" POST "/api/v1/runs/$planned_run_id/start" '{}')" || viewer_start="error"
	if [ "$viewer_start" = "403" ]; then
		pass "the viewer is refused when starting a planned run (403)"
	else
		fail "the viewer starting a planned run got status $viewer_start, want 403"
	fi
fi

# --- the CLI ----------------------------------------------------------------
cli="$work_dir/yumdrake"
go build -o "$cli" ./cmd/yumdrake || abort "the CLI did not build"
pass "the CLI builds"

# A token made through the session API, as the web app's token page does. It is handed to the CLI
# through the environment and never printed.
token="$(curl -fsS -b "$deployer_jar" -H "Origin: $base_url" -H 'Content-Type: application/json' \
	-d '{"name":"demo-check","ttl_days":1}' "$base_url/api/v1/tokens" | json_field plaintext)" ||
	abort "the deployer could not create a CLI token"
cli_run() {
	# HOME is empty so that no real config file is read or written.
	HOME="$work_dir" YUMDRAKE_SERVER="$base_url" YUMDRAKE_TOKEN="$token" NO_COLOR=1 "$cli" "$@"
}

jobs_out="$(cli_run jobs list 2>&1)" || jobs_out=""
if [[ "$jobs_out" == *deploy-platform* ]]; then
	pass "the CLI lists the demo jobs"
else
	fail "the CLI did not list deploy-platform"
fi

versions=(--set backends=v1.5.0 --set workers=v1.5.0 --set frontend=v1.5.0)
if cli_run plan deploy-platform --env dev "${versions[@]}" >"$work_dir/plan.out" 2>&1; then
	pass "the CLI plans deploy-platform in dev"
else
	fail "the CLI could not plan deploy-platform in dev: $(tail -n 3 "$work_dir/plan.out" | tr '\n' ' ')"
fi

limit=()
if command -v timeout >/dev/null 2>&1; then
	limit=(timeout 240)
fi
if ${limit[@]+"${limit[@]}"} env HOME="$work_dir" YUMDRAKE_SERVER="$base_url" YUMDRAKE_TOKEN="$token" NO_COLOR=1 \
	"$cli" run deploy-platform --env dev "${versions[@]}" --yes --watch >"$work_dir/run.out" 2>&1; then
	pass "the CLI ran deploy-platform in dev and it succeeded on the fake steps"
else
	fail "the run did not succeed: $(tail -n 5 "$work_dir/run.out" | tr '\n' ' ')"
fi

if [ "$failures" -ne 0 ]; then
	echo "demo-check: $failures check(s) failed" >&2
	exit 1
fi
echo "demo-check: all checks passed"
