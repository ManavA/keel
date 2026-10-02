# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
versions follow [semantic versioning](https://semver.org/spec/v2.0.0.html).

Until 1.0 the exported API may change between minor versions.

## [0.1.0] — unreleased

First release. See the package table in `README.md` for what is included.

Contracts worth knowing before upgrading, because each differs from the obvious
behaviour:

- `config.Load` does not wrap the underlying envconfig error. That error quotes
  the offending value, and a secret-shaped one is scrubbed from the message
  before it is returned; wrapping would keep the unscrubbed text reachable
  through `Unwrap`. envconfig's error type cannot be inspected with `errors.As`.
- `pg.ParsePage` reads `page` as 1-based, and refuses a pagination parameter it
  cannot act on rather than ignoring it.
- `migrate.Run` returns `ErrChecksumDrift` when a file no longer matches the
  ledger. It holds a transaction-scoped advisory lock for the whole run, so two
  deploys at once serialize and the second applies nothing.
- `middleware.CORS` allows nothing when no origins are configured, and panics on
  a `"*"` origin combined with credentials.
- `httpx.NewRouter` returns an error on that same `"*"`-with-credentials
  pairing instead of building a router that fails in every browser.
- `middleware.RateLimit` panics when `Requests` or `Window` is missing, rather
  than returning a limiter that admits everything.
- `httpx.Health` serves liveness and readiness separately. Liveness runs no
  dependency checks.
- "Exactly once" in `agent` covers the journal and nothing outside Postgres. A
  completed step is never executed again and a step is recorded once. An
  interrupted model call is made again. An interrupted tool call is executed
  again with the same `Invocation.Key`, which is at-least-once: the effect
  happens once only if the tool arranges it, with `agent/pg`'s `Once` for a
  write to the same Postgres, with the key as the receiver's idempotency key
  for an effect elsewhere, or by setting `AtMostOnce`, which asks a person
  before an interrupted call is made again. After a worker loses its lease a
  tool it started can keep running; the store's fence stops it writing the
  journal and cannot stop the tool's outside effect.
- `policy` blocks an action no rule matches, unless `Policy.Default` says
  otherwise. A condition on an attribute the action lacks does not hold, `ne`
  included. A condition that cannot be evaluated counts toward the stricter
  outcome: an ask or block rule matches and an allow rule does not.
- `agent` measures lease times, timestamps and budgets on the engine's `Clock`,
  not the database's. Processes sharing runs must keep their clocks within a
  small fraction of `LeaseTTL`. A wrong clock costs repeated work; the journal is
  protected by the lease's epoch.
- A run keeps the system prompt, tool list and limits it started with, as a
  `Snapshot`. A deploy cannot change a conversation already under way; tool code
  still comes from the running build, by name.
- `llm` ships no price table. The caller supplies `llm.Prices`, and a model the
  table lacks is `ErrNoPrice` rather than free.
- Nothing in `llm/anthropic` or `llm/openai` has been run against a real
  provider. Each has a live test behind the `live` build tag that needs an API key.
- `agent/httpapi` does not authenticate or authorise; the code that mounts it
  does.
- A string a store only records keeps U+FFFD in place of a NUL or invalid UTF-8.

[0.1.0]: https://github.com/ManavA/keel/releases/tag/v0.1.0
