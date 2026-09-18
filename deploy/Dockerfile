# Multi-stage build for a Go service built on keel.
#
# Build with:
#   docker build \
#     --build-arg CMD_PATH=./cmd/api \
#     --build-arg GIT_REVISION=$(git rev-parse HEAD) \
#     --build-arg BUILDINFO_PACKAGE=your/module/internal/buildinfo \
#     -f deploy/Dockerfile -t my-service .
#
# CMD_PATH is the package to build (a directory under ./cmd in the calling
# project). GIT_REVISION is optional; when set, it is stamped into the
# binary through -ldflags so a running instance can report which commit it
# was built from. BUILDINFO_PACKAGE is the fully qualified package path of
# the symbol that receives it (a package with a settable `var Revision
# string`) — required whenever GIT_REVISION is set, since a revision with
# nowhere to go would otherwise be silently dropped.
#
# BUILDINFO_PACKAGE is checked twice: that it is set at all (below), and
# that it names a package actually reachable from CMD_PATH's build (via
# `go list -deps`, below) — a build fails on either. Both checks exist
# because the Go linker itself gives none: -X targeting a package that does
# not exist, or one that is never imported by this particular binary, is a
# silent no-op, not a build error — confirmed by building both cases and
# inspecting the result, not assumed. The binary's own embedded build
# metadata contains the literal ldflags string regardless of whether the
# target was valid, so grepping the compiled binary for the revision value
# proves nothing (it is found either way); `go list -deps` is what actually
# distinguishes them. scripts/test-dockerfile.sh asserts this.
#
# What this still does not catch: BUILDINFO_PACKAGE naming a real package
# that IS reachable from this build but has no variable actually named
# `Revision` — the linker no-ops on that too, silently, the same as the
# other two cases. Catching it needs a second check (`go doc
# "${PACKAGE}.Revision"`) that this Dockerfile does not add, to keep the
# build-time check to the misconfiguration most likely in practice (the
# package path being wrong or stale) rather than every way this could fail.
#
# Migrations are copied if the calling project has a migrations/ directory,
# and skipped otherwise; the builder stage below stages them into an
# always-present location first, so the runtime COPY needs no glob and
# cannot accidentally match something else named with a migrations prefix.

FROM golang:1.26-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG CMD_PATH=./cmd/api
ARG GIT_REVISION=""
ARG BUILDINFO_PACKAGE=""

# Fails loudly when a revision was given but has nowhere documented to go —
# the one misconfiguration this Dockerfile can actually detect. See the
# header comment for what this does not (and cannot) catch.
RUN if [ -n "$GIT_REVISION" ] && [ -z "$BUILDINFO_PACKAGE" ]; then \
        echo "GIT_REVISION is set but BUILDINFO_PACKAGE is not." >&2; \
        echo "Pass --build-arg BUILDINFO_PACKAGE=your/module/internal/buildinfo," >&2; \
        echo "a package with a settable 'var Revision string' — otherwise the" >&2; \
        echo "revision has nowhere to be stamped and is silently dropped." >&2; \
        exit 1; \
    fi

# Fails loudly when BUILDINFO_PACKAGE names a package that this build never
# actually imports — a typo, a stale path after a refactor, or a package
# that exists somewhere in the module but not on CMD_PATH's own import
# graph. `go list -deps` lists every package CMD_PATH actually pulls in;
# grep against that, not against whether the package merely exists.
RUN if [ -n "$BUILDINFO_PACKAGE" ]; then \
        go list -deps "${CMD_PATH}" | grep -qx "$BUILDINFO_PACKAGE" || { \
            echo "BUILDINFO_PACKAGE ($BUILDINFO_PACKAGE) is not imported by ${CMD_PATH}." >&2; \
            echo "The -X flag would silently stamp nothing." >&2; \
            exit 1; \
        }; \
    fi

RUN if [ -n "$BUILDINFO_PACKAGE" ]; then \
        LDFLAGS="-w -s -X ${BUILDINFO_PACKAGE}.Revision=${GIT_REVISION}"; \
    else \
        LDFLAGS="-w -s"; \
    fi; \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="$LDFLAGS" -o /out/service "${CMD_PATH}"

# Stages the exact migrations/ directory into an always-present location, so
# the runtime stage's COPY below is unconditional: no glob is involved, so
# nothing named migrations.md or migrations-old/ can be matched by accident,
# and a project with no migrations/ at all still produces an (empty)
# /out/migrations for that COPY to find.
RUN mkdir -p /out/migrations && [ ! -d migrations ] || cp -r migrations/. /out/migrations/

# Alpine version pinned explicitly for reproducibility; bump this
# periodically (Alpine's supported-release window is roughly two years).
FROM alpine:3.22

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/service /app/service

# /out/migrations always exists in the builder stage (empty if the calling
# project had no migrations/ of its own), so this COPY is unconditional —
# no glob, so nothing but the migrations/ directory itself can land here.
COPY --from=builder /out/migrations /app/migrations

RUN adduser -D -g '' appuser
USER appuser

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/service"]
