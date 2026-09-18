# Contributing

## Requirements

Go 1.26 or later. Docker, for the database-backed tests.

## Building and testing

```
make                    # fmt, test, lint, vuln
make test               # everything that needs no Docker
make test-db            # database-backed packages, Docker required
make migrations-check   # replay every migrations directory in the module
make secrets            # scan for credentials
```

CI runs the same commands, so a clean local run and a green pipeline mean the
same thing.

Run `make secrets` before pushing anything that touched configuration,
deployment or test fixtures. The CI job is a backstop; by the time it fails, the
secret is in the remote's history.

## Scope

Code belongs in keel when it is infrastructure that any web backend would
otherwise write itself.

Nothing in this repository may carry a domain: no customer names, no product
copy, no schema from a particular application, no credentials, no host names that
resolve to something real.

A package that talks to an external service must also work without it. Provide
an in-process default — Postgres, memory, the local filesystem, the log — and
select the external provider by configuration. Keep the heavy dependency in a
subpackage so importing the package does not pull in a client nobody asked for.

## Style

Follow [Effective Go](https://go.dev/doc/effective_go) and the
[code review comments](https://go.dev/wiki/CodeReviewComments).

A package doc comment says what the package does, when to use it, and the one or
two decisions that are not obvious from the signatures. It is not a summary of
the API; `go doc` already prints that. Where an unusual choice has a reason, one
factual clause is enough: "migrations are re-run on every deploy, so each file
must be idempotent."

Comments explain what a careful reader would otherwise have to reconstruct, and
describe the code rather than the process that produced it. Code that says what
it does needs no comment repeating it.

Interfaces are small and declared where they are used. Errors are wrapped with
context. `context.Context` comes first. Options are structs whose zero value
works.

## Tests

Table-driven, with testify. Name the case, not the index.

Where a check could pass without exercising what it names, write the case that
must fail and confirm you have seen it fail. `pg/testdb` does this explicitly:
`TestTheNaiveProbeAcceptsTheRestartFixture` asserts that a two-tries-and-done
probe accepts the restart fixture, so if that fixture stops discriminating
between the real oracle and a naive one, the suite says so.

Database-backed tests use `pg/testdb`. They may skip when Docker is unavailable;
`KEEL_REQUIRE_DB=1` turns the skip into a failure, and CI sets it.

Migrations are replayed by `make migrations-check`, which finds every directory
named `migrations` in the module. A new package with migrations needs no wiring.

## Commit messages

Imperative mood, one line summarising the change, optionally scoped:

```
add keyset paging to pg
fix realip to read the rightmost untrusted entry
```

The body says why, when why is not obvious from the diff.
