# Changelog

Notable changes to keel. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and versions follow [semantic versioning](https://semver.org/spec/v2.0.0.html).

Until 1.0 the exported API may change between minor versions. Where it does, the
entry below says what moved and why.

## [Unreleased]

### Added

- `auth`, `admin` and `textpolicy`, and `media`, `geocode`, `perf`, `scripts`
  and `deploy`, are still on their branches.
- `examples/minimal` gains its authentication wiring, and the `run-firebase`
  shape, when `auth` lands.

## [0.1.0-pre] — 2026-09-18

The first assembled set of packages. Extracted from a production service, with
each package usable on its own and every external service optional.

### Added

**`config`** — environment loading over envconfig with an optional dotenv step
and a `Validate` hook. Trims whitespace from fields that hold a secret or a URL.
`Redacted` renders a configuration struct as fields safe to log, covering
non-string and collection secrets. `RedactURL` keeps the host and drops the
password.

**`log`** — a JSON `slog` handler that carries the request id off the context
and replaces the value of any attribute whose key names a credential. Matching
is by whole key segments, so `api_token` is hidden and `tokens_used` keeps its
value and its JSON type. `Options.Cloud` maps the level onto the `severity` key
Google Cloud Logging reads.

**`httpx`** — HTTP server with graceful shutdown driven by a context, a chi
router with the middleware stack in a documented order, JSON responses with
generic error bodies, and separate liveness and readiness endpoints.

**`httpx/middleware`** — `RealIP` with a trusted-proxy list, `RequestID`,
`RequestLog` with query-string redaction, `Recoverer`, `CORS` and `RateLimit`.

**`httpx/buildinfo`** — the revision the running binary was built from, from a
linker flag or Go's VCS stamp.

**`pg`** — a pgx v5 pool that proves itself before returning, a transaction
helper, and offset and keyset paging that refuses a pagination parameter it
cannot act on.

**`pg/migrate`** — a migration runner with a ledger, each file applied inside
one transaction with its own ledger row. `Replay` reproduces the failure where a
migration edited after it was applied never runs its new content.

**`pg/testdb`** — a Docker Postgres harness whose readiness probe connects the
way the tests do, over host TCP on a pinned `127.0.0.1`, rather than through
`docker exec`.

**`search`** — a document index behind an `Index` interface. `PostgresIndex` is
the default and needs no external service; `search/meili` is the opt-in upgrade.

**`jobs`** — background work with run outcomes computed from their own counts,
an in-process scheduler, and guards that skip already-completed work.
`jobs/cloudrun` is the opt-in trigger.

**`events`** — `Publisher` and `Subscriber` with an in-memory bus as the
default and `events/pubsub` as the opt-in cross-process transport.

**`mail`** — a `Sender` interface with `LogSender` as the default and Postmark
as the opt-in provider, plus a startup template check and a delivery
reconciliation tool.

**`examples/minimal`** — a runnable service over Postgres and nothing else.

### Notes

- Go 1.25 or later. Docker for `pg/testdb` only.
- `config.Load` does not wrap the underlying envconfig error. That error quotes
  the offending value, and a secret-shaped one is scrubbed out of the message
  before it is returned; wrapping would keep the unscrubbed text reachable
  through `Unwrap`. The cost is that envconfig's error type cannot be inspected
  with `errors.As`.
- `pg.ParsePage` reads `page` as 1-based. `page=0` is an error rather than a
  silent first page.
- `migrate.Run` has no advisory lock yet. Run migrations from one place; two
  instances deploying at once fail loudly rather than corrupting anything.

[Unreleased]: https://github.com/ManavA/keel/compare/v0.1.0-pre...HEAD
[0.1.0-pre]: https://github.com/ManavA/keel/releases/tag/v0.1.0-pre
