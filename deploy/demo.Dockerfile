# Demo image: the notes example with Postgres in the same container.
#
# This is the live-demo shape, not the production shape. The database lives
# on the container filesystem, so a restart, a new revision, or a
# scale-to-zero wipes every row. Use it to click through the product, never
# for anything that must survive. Production is deploy/Dockerfile (stateless
# service) plus a managed database.
#
# Build from the repo root:
#   docker build --build-arg GIT_REVISION=$(git rev-parse HEAD) \
#     --build-arg BUILDINFO_PACKAGE=github.com/ManavA/keel/httpx/buildinfo \
#     -f deploy/demo.Dockerfile -t keel-demo .
#
# Run locally the way Cloud Run will:
#   docker run --rm -p 8080:8080 keel-demo
# Then open http://localhost:8080 for the landing page.

FROM golang:1.26-alpine AS builder

WORKDIR /src

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG GIT_REVISION=""
ARG BUILDINFO_PACKAGE="github.com/ManavA/keel/httpx/buildinfo"

RUN go list -deps ./examples/minimal | grep -qx "$BUILDINFO_PACKAGE" || { \
        echo "BUILDINFO_PACKAGE ($BUILDINFO_PACKAGE) is not imported by ./examples/minimal." >&2; \
        exit 1; \
    }; \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s -X ${BUILDINFO_PACKAGE}.Revision=${GIT_REVISION}" -o /out/service ./examples/minimal

FROM postgres:16-alpine

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata openssl

COPY --from=builder /out/service /app/service
COPY deploy/demo-start.sh /app/demo-start.sh

# Postgres refuses to run as root; the service needs no privileges either.
RUN mkdir -p /var/lib/postgresql/data /var/run/postgresql && \
    chown -R postgres:postgres /var/lib/postgresql /var/run/postgresql /app && \
    chmod +x /app/demo-start.sh

USER postgres

ENV PORT=8080
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=20s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/app/demo-start.sh"]
