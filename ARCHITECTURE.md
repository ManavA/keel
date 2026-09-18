# Architecture

## Package independence

Every package in keel is usable on its own. A package may depend on the standard
library, on third-party libraries, and on keel packages below it in the layering
described further down. It may not depend on a sibling at the same level.

There is no root package. Importing `github.com/ManavA/keel` gets you nothing,
because nothing is declared there.

Where two packages need to work together, the connection is an interface or a
function the caller supplies. `httpx.Health` takes a map of `Check` functions;
`pg.HealthCheck` returns one. `httpx` does not import `pg`, and `pg` does not
import `httpx`.

The test is mechanical: `go get github.com/ManavA/keel/pg` into a service with no
HTTP server must compile.

## Layout

```
config/      environment loading, validation, secret handling
log/         slog setup, request ids, redaction
httpx/       HTTP server, router, middleware, responses, health
  middleware/  realip, cors, ratelimit, recoverer, requestlog, requestid
  buildinfo/   build identity for the running binary
pg/          pgx v5 pool, transactions, paging
  migrate/     migration runner and replay check
  testdb/      Docker Postgres harness for tests
```

The rest of the layout is planned and arrives with the extraction branches. It
is fixed in advance so that work on it can happen in parallel:

```
search/      search over Postgres or a search index
jobs/        background work and run records
events/      publish and subscribe
mail/        transactional email
auth/        authentication sources, sessions, tokens
admin/       administrative endpoints and their auth
textpolicy/  one guard for generated and forwarded text
media/       object storage and image derivatives
geocode/     address to coordinate lookup
perf/        timing, budgets, profiling
scripts/     developer and CI scripts
deploy/      deployment templates and checks
examples/
  minimal/   a runnable service wiring config, log, httpx, pg and auth
docs/
  deploy.md
  testing.md
```

## Layers

Nothing enforces this at build time, so it is written down. A package may import
anything strictly below it.

1. **`config`, `log`, `httpx/buildinfo`** — no keel imports at all.
2. **`httpx`, `pg`, `events`, `textpolicy`** — may use level 1.
3. **`pg/migrate`, `pg/testdb`, `search`, `mail`, `media`, `geocode`, `jobs`,
   `perf`** — may use levels 1 and 2. Each owns a dependency on something
   outside the process.
4. **`auth`, `admin`** — may use levels 1 to 3. These are the only packages that
   know about a user, so everything else stays usable by a service that has
   none.

`examples/minimal` sits outside the layers and imports whatever it needs.

## Optional external services

Every package at level 3 or 4 that talks to an external service ships an
in-process implementation as its default, selected by configuration. A project
running only Postgres gets working authentication, search, mail, events and
media without adding a dependency. See the table in `README.md`.

The pattern is one service type with a source interface, not one package per
provider:

```go
type Source interface {
	Name() string
	Authenticate(ctx context.Context, credentials Credentials) (Identity, error)
}
```

Providers are registered by name and enabled by configuration
(`AUTH_SOURCES=local,firebase`). Adding one does not change the service's API,
and removing one does not break a caller that never used it.

## Deliberately not here

**Shell scripts.** The service these packages came from kept its database
harness, migration runner and migration-replay check in `bash`. They are Go here
instead. A consumer gets keel through `go get` and never sees a `scripts/`
directory, and Go code in a package is reachable from a consumer's own
`TestMain`, runs without a shell, and is testable by `go test`. The
`scripts/` directory that arrives later holds this repository's own developer and
CI scripts, not something a dependent is expected to run.

**The migration data-drift detector.** The original harness seeded a corpus,
snapshotted schema and data around each file, and measured whether a re-apply
changed anything, with a declared per-file budget for the one migration that
could not be made convergent. `pg/migrate.Replay` detects *errors* on re-apply
and nothing else. The gap is stated in `Replay`'s own documentation.

**HTTP cache-control middleware.** The original had one, with policy tiers per
kind of response. The mechanism is generic and the tiers were not, so it was
left out rather than shipped half-generalised.

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

Where a function's signature is fixed by something else — `httpx`'s response
helpers take only `(w, r)`, because that is what a handler holds — the logger
travels on the request context instead. `httpx.WithLogger` puts it there and
`NewRouter` does so for every request.

**Tests** use testify and table-driven cases. Database-backed tests use
`pg/testdb`.
