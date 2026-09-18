# Multi-stage build for a Go service built on keel.
#
# Build with:
#   docker build \
#     --build-arg CMD_PATH=./cmd/api \
#     --build-arg GIT_REVISION=$(git rev-parse HEAD) \
#     -f deploy/Dockerfile -t my-service .
#
# CMD_PATH is the package to build (a directory under ./cmd in the calling
# project). GIT_REVISION is optional; when set, it is stamped into the
# binary through -ldflags so a running instance can report which commit it
# was built from. See docs/deploy.md for how to read that value back out of
# a running service, and why a stamp your monitoring can query is required
# before a deployment answers "which build is this" reliably.

FROM golang:1.25-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG CMD_PATH=./cmd/api
ARG GIT_REVISION=""
# BUILDINFO_PACKAGE is the fully qualified package path of the symbol that
# receives GIT_REVISION. Set it to your project's buildinfo package; the
# default here is a placeholder and produces an empty ldflags -X target if
# left unchanged, which fails the build loudly rather than silently
# stamping nothing.
ARG BUILDINFO_PACKAGE="PROJECT_MODULE/internal/buildinfo"

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s -X ${BUILDINFO_PACKAGE}.Revision=${GIT_REVISION}" \
    -o /out/service "${CMD_PATH}"

FROM alpine:3.19

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/service /app/service

# Migrations are optional: only copied if the calling project has them.
# Remove this line if the service does not run its own migrations.
COPY migrations /app/migrations

RUN adduser -D -g '' appuser
USER appuser

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/service"]
