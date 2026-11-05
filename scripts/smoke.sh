#!/bin/sh
# Smoke test of a running stack (make up && make smoke): the probes, a user,
# a monitor that the worker actually checks, and the metrics on both ops
# ports. It leaves no monitor behind; the throwaway user stays (there is no
# delete-user API).
#
# Needs curl. Addresses can be overridden:
#   BASE_URL=http://127.0.0.1:8080 API_OPS_URL=... WORKER_OPS_URL=... sh scripts/smoke.sh
#
# POSIX sh only (no bash, no jq): it also runs from Git for Windows' sh.

set -eu

BASE_URL=${BASE_URL:-http://127.0.0.1:8080}
API_OPS_URL=${API_OPS_URL:-http://127.0.0.1:9090}
WORKER_OPS_URL=${WORKER_OPS_URL:-http://127.0.0.1:9091}
# Seconds to wait for the services and for the first check.
TIMEOUT=${SMOKE_TIMEOUT:-90}
# A public site that answers 200; the stack must reach the internet.
TARGET=${SMOKE_TARGET:-https://example.com}

failures=0
pass() { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; failures=$((failures + 1)); }
die() { printf 'FAIL  %s\n' "$1"; exit 1; }

# status URL -> HTTP status code ("000" if unreachable)
status() { curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" || true; }

# json_field NAME: the first string value of "NAME":"..." on stdin.
json_field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -n 1; }

# wait_for DESCRIPTION COMMAND...: retries COMMAND every 2 s until TIMEOUT.
wait_for() {
	what=$1
	shift
	waited=0
	until "$@"; do
		if [ "$waited" -ge "$TIMEOUT" ]; then
			die "$what (gave up after ${TIMEOUT}s)"
		fi
		sleep 2
		waited=$((waited + 2))
	done
	pass "$what"
}

is_200() { [ "$(status "$1")" = 200 ]; }

echo "Smoke test of $BASE_URL"

# --- probes ------------------------------------------------------------------
wait_for "api /readyz is 200" is_200 "$BASE_URL/readyz"
wait_for "worker /readyz is 200" is_200 "$WORKER_OPS_URL/readyz"
[ "$(status "$BASE_URL/healthz")" = 200 ] && pass "api /healthz is 200" || fail "api /healthz is not 200"

version=$(curl -s --max-time 5 "$BASE_URL/version")
case $version in
*'"version":'*'"commit":'*'"built_at":'*) pass "/version: $version" ;;
*) fail "/version: unexpected answer: $version" ;;
esac

code=$(status "$BASE_URL/metrics")
[ "$code" = 404 ] && pass "public port does not serve /metrics" || fail "public /metrics answered $code, want 404"

# --- a user and a monitor ----------------------------------------------------
email="smoke-$(date +%s)-$$@example.com"
password="smoke-password-1"
creds="{\"email\":\"$email\",\"password\":\"$password\"}"

code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$BASE_URL/api/v1/auth/register" -d "$creds")
[ "$code" = 201 ] && pass "register $email" || die "register answered $code, want 201"

token=$(curl -s --max-time 10 -X POST "$BASE_URL/api/v1/auth/login" -d "$creds" | json_field access_token)
[ -n "$token" ] && pass "login returns a token" || die "login returned no access_token"
auth="Authorization: Bearer $token"

monitor=$(curl -s --max-time 10 -X POST "$BASE_URL/api/v1/monitors" -H "$auth" \
	-d "{\"name\":\"smoke\",\"url\":\"$TARGET\",\"interval_seconds\":30}" | json_field id)
[ -n "$monitor" ] && pass "create monitor $monitor for $TARGET" || die "monitor was not created"
# From here on, remove the monitor however the script ends.
trap 'curl -s -o /dev/null --max-time 10 -X DELETE "$BASE_URL/api/v1/monitors/$monitor" -H "$auth" || true' EXIT

checked() {
	curl -s --max-time 5 "$BASE_URL/api/v1/monitors/$monitor/checks?limit=1" -H "$auth" | grep -q '"checked_at"'
}
wait_for "the worker checked the monitor" checked

last=$(curl -s --max-time 5 "$BASE_URL/api/v1/monitors/$monitor/checks?limit=1" -H "$auth")
case $last in
*'"success":true'*) pass "the check succeeded" ;;
*) fail "the check failed (does the stack reach $TARGET?): $last" ;;
esac

# --- metrics -----------------------------------------------------------------
api_metrics=$(curl -s --max-time 5 "$API_OPS_URL/metrics")
worker_metrics=$(curl -s --max-time 5 "$WORKER_OPS_URL/metrics")

for m in uptime_build_info uptime_http_requests_total uptime_http_request_duration_seconds uptime_db_pool_max_connections; do
	printf '%s\n' "$api_metrics" | grep -q "^$m" && pass "api metric $m" || fail "api metric $m is missing"
done
for m in uptime_build_info uptime_checks_total uptime_scheduler_lag_seconds uptime_worker_pool_size \
	uptime_notifications_total uptime_monitors uptime_retention_last_success_timestamp_seconds; do
	printf '%s\n' "$worker_metrics" | grep -q "^$m" && pass "worker metric $m" || fail "worker metric $m is missing"
done
printf '%s\n' "$api_metrics" | grep -q 'route="/api/v1/monitors/{id}/checks"' &&
	pass "route label is the template, not the ID" || fail "no series for route /api/v1/monitors/{id}/checks"
printf '%s\n' "$api_metrics" | grep -q "$monitor" &&
	fail "a monitor ID leaked into the metrics" || pass "no IDs in the metrics"

echo
if [ "$failures" -gt 0 ]; then
	echo "smoke test FAILED: $failures check(s)"
	exit 1
fi
echo "smoke test passed"
