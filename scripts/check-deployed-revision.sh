#!/usr/bin/env bash
#
# Compare a deployed service's reported build revision against a git
# revision.
#
# A deployment succeeding at build time and a deployment serving that build
# are two different facts. This script checks the second one: it calls the
# service's health endpoint, reads the build revision it reports, and
# compares it against a given git revision (the current checkout's HEAD by
# default).
#
# Exit codes:
#   0  current      - the deployed revision matches the expected revision
#   1  stale        - the deployed revision is a different, known commit
#   2  unknowable   - the service could not be reached, or reports no
#                     build identity, or reports a dirty build
#
# 2 is a separate case from 1 on purpose: "the deployment is behind" and "it
# is not possible to tell what the deployment is running" call for
# different responses, and collapsing them would hide which one is true.
#
# Usage:
#   scripts/check-deployed-revision.sh SERVICE_URL [EXPECTED_REVISION]
#
# EXPECTED_REVISION defaults to this checkout's HEAD.
#
# The service is expected to answer GET /healthz (override with
# HEALTH_PATH) with JSON containing a "build" object:
#   {"build": {"revision": "<git sha>", "dirty": false}}
# This is the shape httpx.Health produces when wired to httpx/buildinfo.

set -euo pipefail

usage() {
    cat <<'EOF'
Usage: check-deployed-revision.sh SERVICE_URL [EXPECTED_REVISION]
       check-deployed-revision.sh --self-test
       check-deployed-revision.sh --help

Compares a deployed service's reported build revision against a git
revision. See the header comment in this file for exit codes and the
expected health endpoint shape.

Environment:
  HEALTH_PATH   health endpoint path (default /healthz)
EOF
}

check_one() {
    # Runs this script against $base with HEALTH_PATH=$1, expected
    # revision $2, and asserts the exit code equals $3. Used only by
    # --self-test.
    local path="$1" expected="$2" want_code="$3" desc="$4"
    local got_code=0
    HEALTH_PATH="$path" bash "$SELF" "$base" "$expected" >/dev/null 2>&1 || got_code=$?
    if [[ "$got_code" == "$want_code" ]]; then
        echo "ok   - $desc"
        pass=$((pass + 1))
    else
        echo "FAIL - $desc (want exit $want_code, got $got_code)"
        fail=$((fail + 1))
    fi
}

self_test() {
    command -v python3 >/dev/null || { echo "self-test requires python3" >&2; exit 2; }

    local workdir server_pid server_log port base pass=0 fail=0
    workdir="$(mktemp -d)"
    server_log="$workdir/server.log"
    trap 'kill "${server_pid:-}" 2>/dev/null || true; rm -rf "${workdir:-}"' EXIT

    printf '%s' '{"build":{"revision":"abc123deadbeef","dirty":false}}' > "$workdir/healthz"
    printf '%s' '{"status":"ok"}' > "$workdir/healthz-no-build"
    printf '%s' '{"build":{"revision":"abc123deadbeef","dirty":true}}' > "$workdir/healthz-dirty"

    # A fixed high port, retried on collision, rather than parsing the
    # server's startup log: http.server's log format and buffering are not
    # stable enough across Python versions to parse reliably.
    port=$((20000 + RANDOM % 20000))
    for _ in $(seq 1 10); do
        (cd "$workdir" && exec python3 -m http.server "$port" --bind 127.0.0.1) >"$server_log" 2>&1 &
        server_pid=$!
        sleep 0.3
        if kill -0 "$server_pid" 2>/dev/null && curl -fsS --max-time 1 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1; then
            break
        fi
        kill "$server_pid" 2>/dev/null || true
        port=$((port + 1))
    done
    if ! kill -0 "$server_pid" 2>/dev/null; then
        echo "FAIL: could not start the self-test server" >&2
        cat "$server_log" >&2
        exit 1
    fi
    base="http://127.0.0.1:$port"

    check_one /healthz            abc123deadbeef       0 "matching revision reports current (0)"
    check_one /healthz            mismatchedsha000000  1 "mismatched revision reports stale (1) -- this is the required control: it must be able to fail"
    check_one /healthz-no-build   abc123deadbeef       2 "a response with no build field reports unknowable (2)"
    check_one /healthz-dirty      abc123deadbeef       2 "a dirty build reports unknowable (2)"
    check_one /does-not-exist     abc123deadbeef       2 "a 404 from the health path reports unknowable (2)"

    local unreachable_code=0
    bash "$SELF" "http://127.0.0.1:1" abc123deadbeef >/dev/null 2>&1 || unreachable_code=$?
    if [[ "$unreachable_code" == "2" ]]; then
        echo "ok   - an unreachable service reports unknowable (2)"
        pass=$((pass + 1))
    else
        echo "FAIL - an unreachable service reports unknowable (2) (got $unreachable_code)"
        fail=$((fail + 1))
    fi

    echo "$pass passed, $fail failed"
    [[ "$fail" -eq 0 ]]
}

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
    usage
    exit 0
fi

if [[ "${1:-}" == "--self-test" ]]; then
    self_test
    exit $?
fi

SERVICE_URL="${1:-}"
EXPECTED="${2:-$(git rev-parse HEAD 2>/dev/null || echo '')}"
HEALTH_PATH="${HEALTH_PATH:-/healthz}"

if [[ -z "$SERVICE_URL" ]]; then
    usage >&2
    exit 2
fi

say() { printf '%s\n' "$*"; }

body="$(curl -fsS --max-time 20 "${SERVICE_URL%/}${HEALTH_PATH}" 2>/dev/null || true)"
if [[ -z "$body" ]]; then
    say "unknowable: ${SERVICE_URL%/}${HEALTH_PATH} did not answer."
    exit 2
fi

deployed="$(printf '%s' "$body" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); sys.exit()
b = d.get("build")
print((b or {}).get("revision", "") if isinstance(b, dict) else "")
' 2>/dev/null)"

dirty="$(printf '%s' "$body" | python3 -c '
import json, sys
try:
    b = json.load(sys.stdin).get("build") or {}
    print("yes" if isinstance(b, dict) and b.get("dirty") else "no")
except Exception:
    print("no")
' 2>/dev/null)"

if [[ -z "$deployed" ]]; then
    say "unknowable: the service answered but reports no build revision."
    say "            Expected: ${EXPECTED:-<no git HEAD given>}"
    exit 2
fi

if [[ -z "$EXPECTED" ]]; then
    say "unknowable: no revision to compare against."
    say "            Deployed: $deployed"
    exit 2
fi

if [[ "$dirty" == "yes" ]]; then
    say "unknowable: the deployed build reports a dirty tree, so its revision"
    say "            does not name what is actually running."
    say "            Deployed: $deployed (dirty)"
    exit 2
fi

if [[ "$deployed" == "$EXPECTED" ]]; then
    say "current: the service is running ${deployed:0:12}, which matches the expected revision."
    exit 0
fi

say "stale: the service is not running the expected revision."
say "       Deployed: ${deployed:0:12}"
say "       Expected: ${EXPECTED:0:12}"
exit 1
