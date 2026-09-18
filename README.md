# keel

A Go toolkit for the parts of a web backend that are the same in every project:
loading configuration, setting up a logger, running an HTTP server that shuts
down cleanly, talking to Postgres, running migrations, and testing against a
real database.

The packages were extracted from a production service. Where a design choice
here differs from the obvious one, the package documentation says why in a
sentence.

## What it is not

keel is not a framework. There is no root package, no plugin registry and no
lifecycle to adopt. You can take `httpx` without `pg`, or `pg/testdb` on its
own. Packages meet through interfaces the caller supplies, not through imports
of each other.

It is not a data layer either. `pg` gives you a pool, a transaction helper and
paging. Queries and repositories are yours.

## Every external service is optional

Each package that talks to something outside the process has an in-process
default, so a project with nothing but Postgres gets a complete backend:

| Package | In-process default | Optional backends |
|---|---|---|
| `auth` | `local` — email and password, verification, reset, sessions in Postgres | `firebase` (ID tokens), `oidc` (any issuer) |
| `search` | Postgres full-text | Meilisearch |
| `mail` | a sender that writes to the log | Postmark |
| `events` | in-memory | Cloud Pub/Sub |
| `media` | local filesystem | Google Cloud Storage |

Adding a service is configuration, not a rewrite: `AUTH_SOURCES=local,firebase`
enables Firebase alongside the local source rather than replacing it.

## Quick start

The smallest useful service needs Postgres and nothing else.

```go
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/ManavA/keel/config"
	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
)

type Config struct {
	Port        int    `envconfig:"PORT" default:"8080"`
	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`
}

func main() {
	slog.SetDefault(log.New(log.Options{}))

	var cfg Config
	if err := config.Load(&cfg); err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := pg.Open(ctx, pg.Options{URL: cfg.DatabaseURL})
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	r := httpx.NewRouter(httpx.RouterOptions{})
	r.Mount("/", httpx.Health(httpx.HealthOptions{
		Checks: map[string]httpx.Check{"database": pg.HealthCheck(pool)},
	}))

	srv := httpx.NewServer(httpx.ServerOptions{Addr: ":8080", Handler: r})
	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
```

`examples/minimal` is a runnable version of this with authentication added. It
runs in two shapes:

```
make run-local      # Postgres only
make run-firebase   # the same app with AUTH_SOURCES=local,firebase
```

## Package map

| Package | What it provides |
|---|---|
| `config` | Environment loading over envconfig, optional dotenv, `Validate()`, secret trimming and redaction |
| `log` | A JSON `slog` handler that carries request ids and redacts credential-shaped keys |
| `httpx` | HTTP server, chi router, middleware, JSON responses, health endpoints |
| `httpx/buildinfo` | The revision the running binary was built from |
| `pg` | pgx v5 pool, transaction helper, offset and keyset paging |
| `pg/migrate` | Migration runner with a ledger, and a replay check |
| `pg/testdb` | Docker Postgres harness for tests |
| `search` | Search over Postgres or Meilisearch |
| `jobs` | Scheduled and one-shot background work, with run records |
| `events` | Publish and subscribe, in memory or over Pub/Sub |
| `mail` | Transactional email |
| `auth` | Authentication with pluggable sources, sessions, verification |
| `admin` | Administrative endpoints and their separate authentication |
| `textpolicy` | One guard for generated and forwarded text |
| `media` | Object storage and image derivatives |
| `geocode` | Address to coordinate lookup |
| `perf` | Timing, budgets, profiling handlers |
| `scripts` | Developer and CI scripts |
| `deploy` | Deployment templates and checks |

## Documentation

`ARCHITECTURE.md` describes the layout and the import rules. `docs/deploy.md`
covers running a keel service on a container platform. `docs/testing.md` covers
the database harness.

## Requirements

Go 1.25 or later. Docker, for `pg/testdb` only.
