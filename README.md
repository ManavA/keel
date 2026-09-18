# keel

Reusable Go packages for building a web backend: configuration, structured
logging, HTTP serving, Postgres access and migrations, search, background jobs,
events and transactional email. Each package is usable on its own, and every
package that talks to an external service ships an in-process default, so a
project running only Postgres gets a complete backend.

## Install

```
go get github.com/ManavA/keel
```

Go 1.25 or later. Docker is needed only by `pg/testdb`.

## Quick start

A service needing Postgres and nothing else:

```go
package main

import (
	"context"
	"fmt"
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

	srv := httpx.NewServer(httpx.ServerOptions{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: r,
	})
	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
```

`examples/minimal` is a runnable service built this way, with a notes API over
Postgres. Run it with `make run-local`.

## Packages

| Package | Description |
|---|---|
| `config` | Environment loading over envconfig, optional dotenv, validation, secret trimming and redaction |
| `log` | A JSON `slog` handler carrying request ids, with credential-shaped keys redacted |
| `httpx` | HTTP server with graceful shutdown, chi router, middleware, JSON responses, health endpoints |
| `httpx/middleware` | Real client address, request id, request log, panic recovery, CORS, rate limiting |
| `httpx/buildinfo` | The revision the running binary was built from |
| `pg` | pgx v5 pool, transaction helper, offset and keyset paging |
| `pg/migrate` | Migration runner with a ledger, and a replay check |
| `pg/testdb` | Docker Postgres harness for tests |
| `search` | Document index over Postgres, or Meilisearch via `search/meili` |
| `jobs` | Background work with run outcomes, an in-process scheduler, and `jobs/cloudrun` triggers |
| `events` | Publish and subscribe in memory, or over Cloud Pub/Sub via `events/pubsub` |
| `mail` | Transactional email over a log sender or Postmark |

## Configuration

`config.Load` reads a struct from the environment, calls `Validate` if the
struct implements it, and trims whitespace from fields holding a secret or a
URL.

```go
type Config struct {
	Port        int    `envconfig:"PORT" default:"8080"`
	DatabaseURL string `envconfig:"DATABASE_URL" required:"true"`
	APIToken    string `envconfig:"API_TOKEN"`
}

var cfg Config
if err := config.Load(&cfg); err != nil {
	return err
}
```

`config.Redacted(&cfg)` renders the struct as name and value pairs safe to log
at startup: secrets are replaced, collections report a count, and URLs keep
their host and lose their password.

Each package that talks to an external service selects its backend by
configuration and falls back to an in-process default:

| Package | Default | External option |
|---|---|---|
| `search` | Postgres full-text (`PostgresIndex`) | Meilisearch (`search/meili`) |
| `mail` | log sender (`LogSender`) | Postmark |
| `events` | in-memory bus (`InMemoryBus`) | Cloud Pub/Sub (`events/pubsub`) |
| `jobs` | in-process scheduler | Cloud Run triggers (`jobs/cloudrun`) |

## Testing

```
make test               # everything that needs no Docker
make test-db            # database-backed packages, Docker required
make migrations-check   # replay every migrations directory in the module
```

`pg/testdb` starts a real Postgres in Docker for tests. `KEEL_REQUIRE_DB=1`
turns a missing Docker from a skip into a failure. See `docs/testing.md`.

## Deployment

See `docs/deploy.md` for health checks, proxy configuration, migrations,
shutdown, pool sizing and build identification.

## Contributing

See `CONTRIBUTING.md`. `ARCHITECTURE.md` describes the package layout and the
import rules.

## License

Apache License 2.0. See `LICENSE`.
