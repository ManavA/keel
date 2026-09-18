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
  ledger. It has no advisory lock, so migrations should run from one place.
- `middleware.CORS` allows nothing when no origins are configured, and panics on
  a `"*"` origin combined with credentials.
- `middleware.RateLimit` panics when `Requests` or `Window` is missing, rather
  than returning a limiter that admits everything.
- `httpx.Health` serves liveness and readiness separately. Liveness runs no
  dependency checks.

[0.1.0]: https://github.com/ManavA/keel/releases/tag/v0.1.0
