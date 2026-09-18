# keel

A Go toolkit for the parts of a web backend that are the same everywhere: loading
configuration, setting up a logger, standing up an HTTP server that shuts down
cleanly, talking to Postgres, running migrations, and testing all of it against a
real database.

Every package here was pulled out of a production service that had already been
burned by the thing the package now prevents. The package documentation says
which burn, because a decision with its reason attached survives refactoring and
one without it does not.

## What it is not

keel is not a framework. There is no `keel.New()`, no plugin registry, no
lifecycle you have to adopt, and no package that imports every other package.
You take `httpx` without taking `pg`, or `pg/testdb` without taking anything
else. If a package here does not suit you, deleting it from your imports is the
whole cost.

It is also not a data layer. `pg` gives you a pool, a transaction helper and
paging arithmetic. It does not generate queries, map structs to rows, or know
what your tables are called.

## Quick start

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
	Env         string `envconfig:"ENV" default:"development"`
	DatabaseURL string `envconfig:"DATABASE_URL" required:"true" secret:"true"`
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

A complete, runnable version of this lives in `examples/minimal`.

## Package map

| Package | What it gives you |
|---|---|
| `config` | Environment loading over `envconfig`, optional dotenv, `Validate()`, secret trimming and redaction |
| `log` | A JSON `slog` handler that carries request ids and refuses to print the value of anything named like a secret |
| `httpx` | HTTP server with graceful shutdown, a chi router, middleware, JSON responses, health endpoints |
| `httpx/buildinfo` | Which build is this — the answer `/healthz` needs so a green check means something |
| `pg` | pgx v5 pool, transaction helper, offset and keyset paging |
| `pg/migrate` | A migration runner with a ledger, built for a job that re-runs every file |
| `pg/testdb` | A Docker Postgres harness for tests, with a readiness probe that does not lie |
| `search` | Search-index client and indexing helpers |
| `jobs` | Scheduled and one-shot background work, with run records |
| `events` | In-process publish and subscribe |
| `mail` | Transactional email over a provider interface |
| `auth` | Password and token authentication, sessions, verification |
| `admin` | Administrative endpoints and their separate authentication |
| `textpolicy` | A single guard every piece of generated or forwarded text passes through |
| `media` | Object storage, image derivatives |
| `geocode` | Address to coordinate lookup with a fallback chain |
| `perf` | Timing, budgets, profiling handlers |
| `scripts` | Developer and CI scripts |
| `deploy` | Deployment templates and checks |

Every package stands alone. The only edges between them are the obvious ones:
`httpx` can serve a `pg` health check, `jobs` can log through `log`.

## Deploying

`docs/deploy.md` covers running a keel service on a container platform, the
migration job ordering that matters, and how to make a deployed build name
itself. `docs/testing.md` covers the database harness and what a green test run
does and does not prove.

## Requirements

Go 1.25 or later. Docker, for the `pg/testdb` harness only.
