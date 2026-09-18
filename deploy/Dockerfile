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
# BUILDINFO_PACKAGE being *set* is checked at build time; BUILDINFO_PACKAGE
# pointing at the *wrong* package is not, and cannot be from inside this
# Dockerfile: the Go linker treats an unknown -X target as a silent no-op,
# not a build error, and the binary's own embedded build metadata still
# contains the literal ldflags string regardless — so grepping the compiled
# binary for the revision value proves nothing (it will be found either
# way). scripts/test-dockerfile.sh demonstrates and asserts both cases,
# including this one. The reliable place to catch a wrong-but-plausible
# BUILDINFO_PACKAGE is after a real deploy, by asking the running service
# what it reports: scripts/check-deployed-revision.sh does exactly that.
#
# Migrations are copied if the calling project has a migrations/ directory,
# and skipped otherwise; the wildcard COPY below is what makes that
# genuinely optional rather than failing the build when it is absent.

FROM golang:1.25-alpine AS builder

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

RUN if [ -n "$BUILDINFO_PACKAGE" ]; then \
        LDFLAGS="-w -s -X ${BUILDINFO_PACKAGE}.Revision=${GIT_REVISION}"; \
    else \
        LDFLAGS="-w -s"; \
    fi; \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="$LDFLAGS" -o /out/service "${CMD_PATH}"

# Alpine version pinned explicitly for reproducibility; bump this
# periodically (Alpine's supported-release window is roughly two years).
FROM alpine:3.22

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/service /app/service

# Optional: copied only if the calling project has a migrations/ directory.
# The trailing "*" makes this a glob that matches zero paths (a no-op) when
# migrations/ does not exist, rather than a literal path that fails the
# build when it is missing.
COPY --from=builder /src/migrations* /app/migrations

RUN adduser -D -g '' appuser
USER appuser

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/service"]
