#!/usr/bin/env bash
#
# Build deploy/Dockerfile against a throwaway minimal Go project and check
# what a template Dockerfile cannot prove on its own: that the migrations/
# COPY is genuinely optional and does not match anything else named with a
# migrations prefix, that a revision with no declared destination fails the
# build instead of being silently dropped, that a BUILDINFO_PACKAGE this
# build never actually imports also fails the build, and that a stamped
# revision reaches a running container — plus one honestly-documented
# limitation: BUILDINFO_PACKAGE naming a real, reachable package with no
# variable actually named Revision cannot be caught here (see the
# Dockerfile's own comment on why).
#
# This never builds against keel itself: keel's only main package outside the
# example is cmd/keel, a scaffolding CLI rather than a service, so there is
# nothing for CMD_PATH to point at here. The throwaway project below plays
# that role.
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

Builds deploy/Dockerfile against a throwaway minimal Go project under
several combinations of migrations/, GIT_REVISION and BUILDINFO_PACKAGE,
asserting the outcome each one requires.

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

# The fixture module's own go directive must be at least what deploy/Dockerfile's
# base image provides, or every check below fails for a reason that has
# nothing to do with what this script tests: a toolchain mismatch, not a
# real Dockerfile defect. Read it from this repository's own go.mod rather
# than a literal, so a future go.mod bump (like the one that made this
# script's previous hard-coded "go 1.25" build against a go:1.26 image and
# fail on a version this repository no longer supports) cannot go unnoticed
# — this fixture always matches what the rest of the repository actually
# requires.
GO_DIRECTIVE="$(grep -m1 '^go ' "$REPO_ROOT/go.mod" | awk '{print $2}')"
[[ -n "$GO_DIRECTIVE" ]] || { echo "could not read a go directive from $REPO_ROOT/go.mod" >&2; exit 1; }

setup_project() {
    # A minimal Go module: a buildinfo package with a settable Revision var,
    # and a cmd/api that prints it, standing in for any real project's own
    # buildinfo package (keel's httpx/buildinfo included) without depending
    # on one existing at a specific import path.
    mkdir -p "$WORKDIR/project/internal/buildinfo" "$WORKDIR/project/cmd/api"
    cat > "$WORKDIR/project/go.mod" <<EOF
module dockerfile-selftest

go $GO_DIRECTIVE
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

check "the correct build actually copied migrations/" 0 \
    bash -c "docker run --rm --entrypoint ls keel-dockerfile-selftest:with-migrations /app/migrations/0001_init.sql"

check "fails the build when GIT_REVISION is set but BUILDINFO_PACKAGE is not -- this is a required control: it must be able to fail" 1 \
    build keel-dockerfile-selftest:missing-package deadbeefcafe ""

check "fails the build when BUILDINFO_PACKAGE names a package this build never imports -- another required control" 1 \
    build keel-dockerfile-selftest:wrong-package deadbeefcafe dockerfile-selftest/internal/nonexistent

echo "== the one misconfiguration this Dockerfile cannot catch, demonstrated =="
# A real, reachable package with no variable actually named Revision is not,
# and cannot be, a build-time error here: the Go linker treats -X targeting
# a missing symbol in an otherwise valid package as a no-op, not a failure,
# the same as it does for a missing package. Confirmed by building this
# case and inspecting the result, not assumed.
mkdir -p "$WORKDIR/project/internal/novar"
cat > "$WORKDIR/project/internal/novar/novar.go" <<'EOF'
package novar

var SomethingElse = "x"
EOF
# novar must actually be imported by cmd/api, or go list -deps correctly
# refuses it for the OTHER reason (not reachable) and this case never
# reaches the one it is meant to demonstrate.
cat > "$WORKDIR/project/cmd/api/main.go" <<'EOF'
package main

import (
	"fmt"

	"dockerfile-selftest/internal/buildinfo"
	_ "dockerfile-selftest/internal/novar"
)

func main() {
	fmt.Println("revision:", buildinfo.Revision)
}
EOF

check "builds successfully even when BUILDINFO_PACKAGE has no Revision variable (documented limitation, not a bug)" 0 \
    build keel-dockerfile-selftest:no-revision-var deadbeefcafe dockerfile-selftest/internal/novar

check "a BUILDINFO_PACKAGE with no Revision variable reports the honest default, not the intended revision" 0 \
    bash -c "docker run --rm keel-dockerfile-selftest:no-revision-var | grep -q 'revision: unset'"

echo "== rebuilding without migrations/, with a similarly-named file present instead =="
rm -rf "$WORKDIR/project/migrations"
echo "not a migration" > "$WORKDIR/project/migrations.md"

check "builds when migrations/ is absent (the COPY must be a genuine no-op, not a failure)" 0 \
    build keel-dockerfile-selftest:no-migrations deadbeefcafe dockerfile-selftest/internal/buildinfo

check "a similarly-named file (migrations.md) is not swept into /app/migrations" 1 \
    bash -c "docker run --rm --entrypoint ls keel-dockerfile-selftest:no-migrations /app/migrations/migrations.md"

echo "$pass passed, $fail failed"

docker rmi -f \
    keel-dockerfile-selftest:with-migrations \
    keel-dockerfile-selftest:missing-package \
    keel-dockerfile-selftest:wrong-package \
    keel-dockerfile-selftest:no-revision-var \
    keel-dockerfile-selftest:no-migrations \
    >/dev/null 2>&1 || true

[[ "$fail" -eq 0 ]]
