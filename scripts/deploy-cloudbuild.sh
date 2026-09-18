#!/usr/bin/env bash
#
# Submit a Cloud Build config with the current git revision stamped in, and
# verify the deployed revision afterward.
#
# Cloud Build's built-in $COMMIT_SHA substitution is set only for a
# trigger-driven build; a manual `gcloud builds submit` leaves it empty.
# This script passes the git revision explicitly as _GIT_REVISION so a
# manual submit still produces a build whose deployed revision can be
# checked.
#
# Usage:
#   deploy-cloudbuild.sh CONFIG --project PROJECT [options]
#
# Options:
#   --project PROJECT           GCP project (required)
#   --substitution KEY=VALUE    additional substitution (repeatable)
#   --skip-verify               submit only, do not verify the deployment
#   --service-url URL           URL to verify against (passed to
#                                check-deployed-revision.sh)
#   --help                      show this message

set -euo pipefail

usage() {
    cat <<'EOF'
Usage: deploy-cloudbuild.sh CONFIG --project PROJECT [options]
       deploy-cloudbuild.sh --self-test

Options:
  --project PROJECT           GCP project (required)
  --substitution KEY=VALUE    additional substitution (repeatable)
  --skip-verify               submit only, do not verify the deployment
  --service-url URL           URL to verify against
  --help                      show this message
EOF
}

self_test() {
    # This script's real work (gcloud builds submit) needs live GCP
    # credentials and cannot be exercised here. This checks the argument
    # parsing and validation that runs before that point: missing
    # required arguments and a missing config file must both be rejected,
    # not silently accepted.
    local pass=0 fail=0

    check() {
        local desc="$1" want_code="$2"; shift 2
        local got_code=0
        bash "$SELF" "$@" >/dev/null 2>&1 || got_code=$?
        if [[ "$got_code" == "$want_code" ]]; then
            echo "ok   - $desc"; pass=$((pass + 1))
        else
            echo "FAIL - $desc (want exit $want_code, got $got_code)"; fail=$((fail + 1))
        fi
    }

    check "no arguments is rejected (2)" 2
    check "missing --project is rejected (2) -- this is the control: it must be able to fail" 2 deploy/cloudbuild.yaml
    check "a nonexistent config file is rejected (1)" 1 no-such-config.yaml --project p
    check "--help exits 0 without requiring other arguments" 0 --help

    echo "$pass passed, $fail failed"
    [[ "$fail" -eq 0 ]]
}

SELF="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

if [[ "${1:-}" == "--self-test" ]]; then
    self_test
    exit $?
fi

CONFIG=""
PROJECT=""
SERVICE_URL=""
SKIP_VERIFY=""
SUBSTITUTIONS=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --project) PROJECT="$2"; shift 2 ;;
        --substitution) SUBSTITUTIONS+=("$2"); shift 2 ;;
        --service-url) SERVICE_URL="$2"; shift 2 ;;
        --skip-verify) SKIP_VERIFY=1; shift ;;
        -*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
        *)
            if [[ -z "$CONFIG" ]]; then CONFIG="$1"; else
                echo "unexpected argument: $1" >&2; exit 2
            fi
            shift
            ;;
    esac
done

if [[ -z "$CONFIG" || -z "$PROJECT" ]]; then
    usage >&2
    exit 2
fi
if [[ ! -f "$CONFIG" ]]; then
    echo "no such config: $CONFIG" >&2
    exit 1
fi

REV="$(git rev-parse HEAD)"

# A dirty tree means the stamped revision would not match what is actually
# built. This is a warning, not a refusal: buildinfo reports a "dirty" flag
# for exactly this case, and check-deployed-revision.sh treats a dirty
# build as unknowable rather than as a false match.
if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "warning: working tree is dirty; the image will be stamped $REV, which is not exactly what is being built" >&2
fi

SUBST="_GIT_REVISION=$REV"
for kv in "${SUBSTITUTIONS[@]:-}"; do
    [[ -n "$kv" ]] && SUBST="$SUBST,$kv"
done

echo "==> submitting $CONFIG to $PROJECT at $REV"
gcloud builds submit --config "$CONFIG" --project "$PROJECT" \
    --substitutions="$SUBST" .

if [[ -n "$SKIP_VERIFY" ]]; then
    echo "==> skipping deployment verification (--skip-verify)"
    exit 0
fi
if [[ -z "$SERVICE_URL" ]]; then
    echo "==> skipping deployment verification (no --service-url given)"
    exit 0
fi

echo "==> verifying the deployed revision"
exec "$(dirname "${BASH_SOURCE[0]}")/check-deployed-revision.sh" "$SERVICE_URL" "$REV"
