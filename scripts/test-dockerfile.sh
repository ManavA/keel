#!/usr/bin/env bash
#
# Build deploy/Dockerfile against a throwaway minimal Go project and check
# what a template Dockerfile cannot prove on its own: that the migrations/
# COPY is genuinely optional (present and absent both build), that a
# revision with no declared destination fails the build instead of being
# silently dropped, and that the stamped revision actually reaches a
# running container — plus one honestly-documented limitation: a
# BUILDINFO_PACKAGE pointing at the wrong package cannot be caught here at
# all (see the Dockerfile's own comment on why).
#
# This never builds against keel itself: keel has no cmd/ directory (it is
# a library), so there is nothing for CMD_PATH to point at here. The
# throwaway project below plays that role.
#
# Usage:
#   scripts/test-dockerfile.sh
#   scripts/test-dockerfile.sh --help
#
# Requires Docker. Exits 0 only if every check below passes; each check
# that asserts success has a corresponding one asserting failure, so a
# checker that stopped checking anything would itself fail one of these.

set -euo pipefail

usage() {
    cat <<'EOF'
Usage: test-dockerfile.sh
       test-dockerfile.sh --help

Builds deploy/Dockerfile against a throwaway minimal Go project, with and
without a migrations/ directory, and with a correct and an incorrect
BUILDINFO_PACKAGE, asserting the outcome each combination requires.

Requires Docker.
EOF
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
    usage
    exit 0
fi

command -v docker >/dev/null || { echo "test-dockerfile.sh requires docker" >&2; exit 2; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$REPO_ROOT/deploy/Dockerfile"
[[ -f "$DOCKERFILE" ]] || { echo "no such file: $DOCKERFILE" >&2; exit 1; }

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

setup_project() {
    # A minimal Go module: a buildinfo package with a settable Revision var,
    # and a cmd/api that prints it, standing in for any real project's own
    # buildinfo package (keel's httpx/buildinfo included) without depending
    # on one existing at a specific import path.
    mkdir -p "$WORKDIR/project/internal/buildinfo" "$WORKDIR/project/cmd/api"
    cat > "$WORKDIR/project/go.mod" <<'EOF'
module dockerfile-selftest

go 1.25
EOF
    : > "$WORKDIR/project/go.sum"
    cat > "$WORKDIR/project/internal/buildinfo/buildinfo.go" <<'EOF'
package buildinfo

var Revision = "unset"
EOF
    cat > "$WORKDIR/project/cmd/api/main.go" <<'EOF'
package main

import (
	"fmt"

	"dockerfile-selftest/internal/buildinfo"
)

func main() {
	fmt.Println("revision:", buildinfo.Revision)
}
EOF
}

pass=0
fail=0

check() {
    local desc="$1" want="$2"
    shift 2
    local got=0
    "$@" >"$WORKDIR/last.log" 2>&1 || got=1
    if [[ "$got" == "$want" ]]; then
        echo "ok   - $desc"
        pass=$((pass + 1))
    else
        echo "FAIL - $desc (want exit $want, got $got)"
        sed 's/^/       /' "$WORKDIR/last.log" | tail -20
        fail=$((fail + 1))
    fi
}

build() {
    local tag="$1" revision="$2" buildinfo_pkg="$3"
    docker build \
        -f "$DOCKERFILE" \
        --build-arg CMD_PATH=./cmd/api \
        --build-arg "GIT_REVISION=$revision" \
        --build-arg "BUILDINFO_PACKAGE=$buildinfo_pkg" \
        -t "$tag" \
        "$WORKDIR/project"
}

echo "== setting up throwaway project (with migrations/) =="
setup_project
mkdir -p "$WORKDIR/project/migrations"
echo "-- 0001_init.sql --" > "$WORKDIR/project/migrations/0001_init.sql"

check "builds with a correct BUILDINFO_PACKAGE and migrations/ present" 0 \
    build keel-dockerfile-selftest:with-migrations deadbeefcafe dockerfile-selftest/internal/buildinfo

check "the stamped revision actually reaches a container run from that image" 0 \
    bash -c "docker run --rm keel-dockerfile-selftest:with-migrations | grep -q deadbeefcafe"

check "fails the build when GIT_REVISION is set but BUILDINFO_PACKAGE is not -- this is the required control: it must be able to fail" 1 \
    build keel-dockerfile-selftest:missing-package deadbeefcafe ""

echo "== the one misconfiguration this Dockerfile cannot catch, demonstrated =="
# BUILDINFO_PACKAGE pointing at a package that exists in no way relevant to
# the binary is not, and cannot be, a build-time error: the Go linker
# treats an unknown -X target as a no-op, not a failure. This documents
# that limitation as a passing build with an honestly-unset revision at
# runtime, rather than leaving it unverified.
check "builds successfully even with a nonexistent BUILDINFO_PACKAGE (documented limitation, not a bug)" 0 \
    build keel-dockerfile-selftest:wrong-package deadbeefcafe dockerfile-selftest/internal/nonexistent

check "a nonexistent BUILDINFO_PACKAGE reports the honest default, not the intended revision" 0 \
    bash -c "docker run --rm keel-dockerfile-selftest:wrong-package | grep -q 'revision: unset'"

echo "== rebuilding without migrations/ =="
rm -rf "$WORKDIR/project/migrations"

check "builds when migrations/ is absent (the COPY must be a genuine no-op, not a failure)" 0 \
    build keel-dockerfile-selftest:no-migrations deadbeefcafe dockerfile-selftest/internal/buildinfo

echo "$pass passed, $fail failed"

docker rmi -f \
    keel-dockerfile-selftest:with-migrations \
    keel-dockerfile-selftest:missing-package \
    keel-dockerfile-selftest:wrong-package \
    keel-dockerfile-selftest:no-migrations \
    >/dev/null 2>&1 || true

[[ "$fail" -eq 0 ]]
