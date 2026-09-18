# Contributing

## Before you push

```
make fmt test lint vuln
```

`make` with no target runs all four. CI runs the same commands plus a secret
scan, so a clean local run and a green pipeline mean the same thing.

Run `gitleaks detect` before pushing anything that touched configuration,
deployment or test fixtures. The CI job is a backstop; by the time it fails, the
secret is in the remote's history.

## What belongs here

Infrastructure any web backend would otherwise write itself. Nothing in this
repository may carry a domain: no customer names, no product copy, no schema
from a particular application, no credentials, no host names that resolve to
something real.

A package that talks to an external service must also work without it. Provide
an in-process default — Postgres, memory, the local filesystem, the log — and
select the external provider by configuration.

## Style

Follow [Effective Go](https://go.dev/doc/effective_go) and the
[code review comments](https://go.dev/wiki/CodeReviewComments).

Documentation is plain and direct. A package doc comment says what the package
does, when to use it, and the one or two decisions that are not obvious from the
signatures. It is not a summary of the API; `go doc` already prints that. Where
an unusual design choice comes from an incident, one factual clause is enough:
"migrations are re-run on every deploy, so each file must be idempotent." No
narratives, no aphorisms.

Comments inside a function explain what a careful reader would otherwise have to
reconstruct. Code that says what it does needs no comment repeating it.

Interfaces are small and declared where they are used. Errors are wrapped with
context. `context.Context` comes first. Options are structs whose zero value
works.

## Tests

Table-driven, with testify. Name the case, not the index.

Where a check could pass without exercising what it names, add the case that
must fail and confirm you have seen it fail. `pg/testdb`'s readiness tests are
built this way.

Database-backed tests use `pg/testdb`. They may skip when Docker is unavailable.
`KEEL_REQUIRE_DB=1` turns that skip into a failure; `make test-db` sets it, and
so does the "Database-backed tests" job in `.github/workflows/ci.yml`.

## Commits

`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `ci:`, optionally scoped:
`feat(pg): keyset paging`. The subject says what changed; the body says why, if
why is not obvious.
