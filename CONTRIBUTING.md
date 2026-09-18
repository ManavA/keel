# Contributing

## Before you push

```
make fmt test lint vuln
```

`make` with no target runs all four. CI runs the same commands plus a secret
scan, so a clean local run and a green pipeline mean the same thing. That
equivalence is the point: a check that only exists in CI gets worked around, and
a check that only exists locally gets skipped.

Run `gitleaks detect` yourself before pushing anything that touched
configuration, deployment or test fixtures. The CI job is the backstop, not the
gate — by the time it fires, the secret is already in the remote's history.

## What belongs here

Code earns a place in keel when it is infrastructure that any web backend would
write, and when its current form was shaped by something going wrong. Both
halves matter. Generic code with no scar tissue is a worse version of a library
somebody else already maintains; specific code with a great story belongs in the
service it came from.

Nothing in this repository may carry a domain. No customer names, no product
copy, no schema from a particular application, no credentials, no host names
that resolve to something real.

## Style

Follow [Effective Go](https://go.dev/doc/effective_go) and the
[code review comments](https://go.dev/wiki/CodeReviewComments). Beyond that:

A package doc comment explains **why the package exists** and the one or two
decisions that are not obvious from the signatures. It is not a summary of the
API — `go doc` already prints that. If the package exists because a particular
failure kept happening, name the failure.

Comments inside a function are for the parts a careful reader would otherwise
have to reconstruct: why the retry compares two values instead of retrying, why
this loop walks backwards, why an error is deliberately discarded. Code that
says what it does needs no comment saying the same thing again.

Interfaces are small and declared where they are used. Errors are wrapped with
context. `context.Context` comes first. Options are structs whose zero value
works.

## Tests

Table-driven, with `testify` for assertions. Name the case, not the index.

A test that can pass without exercising the thing it names is worse than no
test. If a check has a control — a case that must fail — write the control too,
and make sure you have seen it fail. `pg/testdb`'s own self-test is built this
way, and it is the pattern to copy.

Database-backed tests use `pg/testdb`. They may skip when Docker is unavailable,
but the skip must be visible and the package must refuse to report a pass when
it ran nothing.

## Commits

`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `ci:`, optionally scoped:
`feat(pg): keyset paging`. The subject line says what changed; the body says
why, if why is not obvious.
