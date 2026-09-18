# Architecture

## Package independence

Every package is usable on its own. A package may depend on the standard
library, on third-party libraries, and on keel packages below it in the layering
below. It may not depend on a sibling at the same level.

There is no root package. Importing `github.com/ManavA/keel` gets you nothing,
because nothing is declared there.

Where two packages need to work together, the connection is an interface or a
function the caller supplies. `httpx.Health` takes a map of `Check` functions and
`pg.HealthCheck` returns one, so `httpx` does not import `pg` and `pg` does not
import `httpx`.

The test is mechanical: `go get github.com/ManavA/keel/pg` into a service with no
HTTP server must compile.

## Layout

```
config/          environment loading, validation, secret handling
log/             slog setup, request ids, redaction
httpx/           HTTP server, router, middleware, responses, health
  middleware/      realip, cors, ratelimit, recoverer, requestlog, requestid
  buildinfo/       build identity for the running binary
pg/              pgx v5 pool, transactions, paging
  migrate/         migration runner and replay check
  testdb/          Docker Postgres harness for tests
search/          a document index over Postgres
  meili/           the Meilisearch implementation
jobs/            background work and run outcomes
  cloudrun/        triggering another job over Cloud Run
events/          publish and subscribe
  pubsub/          the Cloud Pub/Sub transport
mail/            transactional email
  testing/         a Recorder for tests
  deliverycheck/   reconciling delivery claims against a provider
media/           object storage and image derivatives
geocode/         address to coordinate lookup
perf/            response caching, ETag, gzip, and singleflight middleware
scripts/         developer and CI scripts
deploy/          deployment templates and checks
examples/
  minimal/       a runnable service over Postgres and nothing else
docs/
  deploy.md
```

The rest of the layout is planned and arrives with the remaining extraction
branches. It is fixed in advance so that work on it can happen in parallel:

```
auth/        authentication sources, sessions, tokens
admin/       administrative endpoints and their auth
textpolicy/  one guard for generated and forwarded text
docs/
  testing.md
```

## Import layers

Nothing enforces this at build time, so it is written down. A package may import
anything strictly below it.

1. **`config`, `log`, `httpx/buildinfo`** — no keel imports at all.
2. **`httpx`, `pg`, `events`** — may use level 1.
3. **`pg/migrate`, `pg/testdb`, `search`, `mail`, `jobs`, `media`, `geocode`,
   `perf`** — may use levels 1 and 2. Each owns a dependency on something
   outside the process.

`examples/minimal` sits outside the layers and imports whatever it needs.

## Optional backends

Every package that talks to an external service ships an in-process
implementation as its default, selected by configuration. A project running only
Postgres gets working search, mail, events and background jobs without adding a
dependency.

The pattern is one service type with an interface, not one package per provider.
The heavy dependencies live in subpackages, so importing `search` does not pull
in a Meilisearch client and importing `events` does not pull in Pub/Sub.

## Conventions

**Errors** are wrapped with enough context to locate the call site:
`fmt.Errorf("open pool: %w", err)`. Sentinel errors are exported only where a
caller has a decision to make about them.

**Context** is the first parameter of anything that can block, and it is
honoured.

**Options** are structs whose zero value works, not variadic functional options:
`pg.Open(ctx, pg.Options{URL: url})`.

**Interfaces** are declared by the consumer and are usually one or two methods.

**Logging** goes through `slog`. A package that logs takes a `*slog.Logger` in
its options and falls back to `slog.Default()`. No package calls
`slog.SetDefault`; that is the binary's decision.

Where a signature is fixed by something else — `httpx`'s response helpers take
only `(w, r)`, because that is what a handler holds — the logger travels on the
request context. `httpx.WithLogger` puts it there and `NewRouter` does so for
every request.

**Tests** use testify and table-driven cases. Database-backed tests use
`pg/testdb`.

## Scope

keel is not a framework and not a data layer. There is no lifecycle to adopt and
no plugin registry. `pg` provides a pool, a transaction helper and paging;
queries and repositories belong to the service.

Some things were considered and left out:

- **Shell scripts.** The database harness, migration runner and replay check are
  Go rather than `bash`. A consumer reaches them through `go test` and their own
  `TestMain`, with no shell required.
- **Migration data drift.** `pg/migrate.Replay` detects errors on re-apply. It
  does not snapshot schema and data around each file to detect a migration that
  applies cleanly and leaves a different result; that gap is stated in `Replay`'s
  own documentation.
- **HTTP cache-control middleware.** The mechanism is generic but useful policy
  tiers are not, so there is none rather than a half-generalised one.
