#!/usr/bin/env bash
#
# Check that a deployed service is reachable and healthy.
#
# This checks liveness, not code identity: a service can answer this check
# successfully while running an old build. Use
# scripts/check-deployed-revision.sh to check which revision is running.
#
# Usage:
#   verify-deployed.sh SERVICE_URL [EXTRA_PATH ...]
#
# SERVICE_URL is checked at HEALTH_PATH (default /healthz) and must answer
# with a 2xx status. Each EXTRA_PATH is also requested and must answer with
# a 2xx status; use this for endpoints beyond the health check that should
# be reachable after a deployment (a specific route the deployment is
# expected to serve, for example).
#
# Exit codes:
#   0  every check passed
#   1  at least one check failed
#   2  usage error

set -euo pipefail

usage() {
    cat <<'EOF'
Usage: verify-deployed.sh SERVICE_URL [EXTRA_PATH ...]
       verify-deployed.sh --self-test
       verify-deployed.sh --help

Checks that SERVICE_URL is reachable and healthy at HEALTH_PATH (default
/healthz), plus any EXTRA_PATH given. Each check requires a 2xx response.

Environment:
  HEALTH_PATH   health endpoint path (default /healthz)
EOF
}

check_path() {
    local base="$1" path="$2"
    local code
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "${base%/}${path}" || echo 000)"
    if [[ "$code" -ge 200 && "$code" -lt 300 ]]; then
        echo "ok   - ${base%/}${path} -> $code"
        return 0
    fi
    echo "FAIL - ${base%/}${path} -> $code"
    return 1
}

self_test() {
    command -v python3 >/dev/null || { echo "self-test requires python3" >&2; exit 2; }

    local workdir server_pid port base failed=0
    workdir="$(mktemp -d)"
    trap 'kill "${server_pid:-}" 2>/dev/null || true; rm -rf "${workdir:-}"' EXIT

    printf '%s' '{"status":"ok"}' > "$workdir/healthz"
    printf '%s' '{"status":"ok"}' > "$workdir/ready"

    port=$((20000 + RANDOM % 20000))
    for _ in $(seq 1 10); do
        (cd "$workdir" && exec python3 -m http.server "$port" --bind 127.0.0.1) >/dev/null 2>&1 &
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
        exit 1
    fi
    base="http://127.0.0.1:$port"

    echo "-- expect success --"
    if ! bash "$SELF" "$base" /ready; then
        echo "FAIL - a healthy service with a passing extra path was reported unhealthy"
        failed=1
    fi

    echo "-- expect failure: this is the required control --"
    if bash "$SELF" "$base" /this-path-does-not-exist >/dev/null 2>&1; then
        echo "FAIL - a missing extra path was reported healthy"
        failed=1
    else
        echo "ok   - a missing extra path is correctly reported as a failure"
    fi

    echo "-- expect failure: unreachable service --"
    if bash "$SELF" "http://127.0.0.1:1" >/dev/null 2>&1; then
        echo "FAIL - an unreachable service was reported healthy"
        failed=1
    else
        echo "ok   - an unreachable service is correctly reported as a failure"
    fi

    [[ "$failed" -eq 0 ]]
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
if [[ -z "$SERVICE_URL" ]]; then
    usage >&2
    exit 2
fi
shift || true

HEALTH_PATH="${HEALTH_PATH:-/healthz}"

FAILED=0
check_path "$SERVICE_URL" "$HEALTH_PATH" || FAILED=1
for extra in "$@"; do
    check_path "$SERVICE_URL" "$extra" || FAILED=1
done

exit "$FAILED"
