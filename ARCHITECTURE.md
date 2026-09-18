# Architecture

## The one rule

Every package in keel is usable on its own.

A package may depend on the standard library, on third-party libraries, and on
keel packages *below* it in the list further down. It may not depend on a
sibling at the same level, and nothing may depend on a package that sits above
it. There is no root package that wires everything together, and there never
will be — importing `github.com/ManavA/keel` gets you nothing, because nothing
is declared there.

This is not tidiness for its own sake. A toolkit whose packages reach for each
other becomes a framework the first time somebody wants two of the five things
it does, and a framework is a much larger commitment than the problem it solves.
The test is mechanical: if you can `go get github.com/ManavA/keel/pg` into a
service that has no HTTP server at all and it compiles, the rule holds.

Where two packages genuinely want to meet, the meeting point is an interface or
a plain function that the *caller* supplies, not an import. `httpx.Health` takes
a map of `Check` functions; `pg.HealthCheck` returns one. `httpx` does not know
Postgres exists, and `pg` does not know it is being served over HTTP.

## Layout

```
config/      environment loading, validation, secret handling
log/         slog setup, request ids, redaction
httpx/       HTTP server, router, middleware, responses, health
  middleware/  realip, cors, ratelimit, recoverer, requestlog
  buildinfo/   build identity for the running binary
pg/          pgx v5 pool, transactions, paging
  migrate/     migration runner
  testdb/      Docker Postgres harness for tests
search/      search index client and indexing
jobs/        background work and run records
events/      in-process publish and subscribe
mail/        transactional email
auth/        authentication, sessions, tokens
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

Nothing enforces this at build time — `go vet` has no opinion about it — so it
is written down instead. Read it as: a package may import anything strictly
below it.

1. **`config`, `log`, `httpx/buildinfo`.** No keel imports at all. These are the
   things every binary needs in its first ten lines, before it has decided what
   kind of program it is.
2. **`httpx`, `pg`, `events`, `textpolicy`.** May use `log`. They are the shapes
   a service is built out of.
3. **`pg/migrate`, `pg/testdb`, `search`, `mail`, `media`, `geocode`, `jobs`,
   `perf`.** May use level 2. Each one owns a dependency on something outside
   the process — a database, an index, an SMTP provider, a bucket, an API.
4. **`auth`, `admin`.** May use levels 1 through 3. These are the only packages
   that know about a *user*, and they are at the top because everything else
   should stay usable by a service that has none.

`examples/minimal` sits outside the layers and imports whatever it likes; it is
there to be read, and to fail loudly at compile time if a signature drifts.

## Conventions

**Errors** are wrapped with enough context to locate the call site without a
stack trace: `fmt.Errorf("open pool: %w", err)`. Sentinel errors are exported
only where a caller has a decision to make about them.

**Context** is the first parameter of anything that can block, and it is
honoured — a function that takes a `context.Context` and ignores it is worse
than one that does not take it at all.

**Options** are structs, not variadic functional options. `pg.Open(ctx,
pg.Options{URL: url})` reads better than four `WithX` calls and, more
importantly, a zero `Options` is a documented, working default rather than a
panic. Functional options appear only where the option set is genuinely open —
so far, nowhere.

**Interfaces** are declared by the consumer and are usually one or two methods.
No package here exports a `Service` interface with twelve methods on it.

**Logging** goes through `slog`. A package that logs takes a `*slog.Logger` in
its options and falls back to `slog.Default()`; it never calls
`slog.SetDefault` itself, because that is the binary's decision.

**Tests** use `testify` and table-driven cases. Anything needing a real Postgres
uses `pg/testdb` and skips — loudly, and only when the harness cannot run at
all — rather than quietly passing.
