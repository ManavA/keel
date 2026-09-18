# Testing

## Running the tests

```
make test          # everything that needs no Docker
make test-db       # the database-backed packages, with Docker required
```

`make test-db` sets `KEEL_REQUIRE_DB=1`, which turns "Docker is unavailable"
from a skip into a failure. CI runs both: the ordinary suite under `-race`, and
a second job with `KEEL_REQUIRE_DB=1` so a database-backed test cannot stop
running unnoticed.

## The database harness

`pg/testdb` starts a real Postgres in Docker. There is no in-memory substitute
and no mock: the things worth testing about a database layer — transaction
semantics, constraint violations, what a migration actually does to a schema —
are exactly the things a substitute gets wrong.

One container per package, started from `TestMain`:

```go
func TestMain(m *testing.M) {
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

func TestSomething(t *testing.T) {
	db := testdb.Shared(t)
	pool, err := pg.Open(context.Background(), pg.Options{URL: db.URL})
	// ...
}
```

`testdb.New(t, testdb.Options{})` starts one container for a single test
instead, and removes it when the test ends. It costs a couple of seconds, so
prefer `RunMain` and `Shared` for anything with more than a handful of tests.

Every caller of `Shared` gets the same database, so a test that writes needs its
own schema, its own table names, or a transaction it rolls back. The migration
tests create a schema per test and drop it in `t.Cleanup`.

### What `RunMain` refuses

A run where every test passed and not one of them asked for the database fails,
even though nothing went red. That combination is what a package looks like
after a build tag, a changed skip condition or a renamed environment variable
stops the tests reaching the database: the summary is green and nothing was
verified.

It counts calls to `Shared`, because the testing package does not expose a test
count. A test that starts its own database with `New`, or needs none, is
invisible to it.

## The readiness probe

`testdb` will not report a database ready before it is, and the reason is worth
knowing because the obvious probe is wrong.

`docker exec <container> pg_isready` talks to the container's Unix socket. The
official Postgres entrypoint starts the server twice on first run — once on that
socket only, to execute the init scripts, then again on the published TCP port —
so a probe going in that way can see the first server while the tests, which
connect over the published port, cannot. The window is about a second and a
half. Long enough for the first migration to fail in a way that reads as a
migration bug.

`testdb.Ready` therefore connects the way the tests do: host TCP, the same
connection string, pinned to a literal `127.0.0.1`. The pinning matters on its
own, because `docker run -p` can publish the IPv4 side before the IPv6 side, and
on a host where `localhost` resolves to `::1` the probe and the application
would be talking to different sockets.

It then reads `pg_postmaster_start_time()` twice, a beat apart, and requires the
two to agree. That catches a restart after the port is already up — a `docker
restart`, a crash-recovery cycle. It does not catch the init sequence above,
which host TCP cannot observe at all. The two are worth keeping straight: the
first fix is the one that matters, and the second is defence in depth.

## Migrations

`pg/migrate` has two checks and they answer different questions.

`Run` applies pending migrations. It returns `ErrChecksumDrift` when a file no
longer matches what the ledger recorded, which a deploy step can distinguish:

```go
result, err := migrate.Run(ctx, pool, opts)
if err != nil && !errors.Is(err, migrate.ErrChecksumDrift) {
	return err
}
```

`Replay` answers whether an edited migration would actually apply. It takes the
migration set at an earlier revision and the set now, applies the first to an
empty database, the second on top, the second again, then round-trips each
changed file down and up. The failure it exists for: the ledger keys on the file
name, so a migration edited while unmerged never runs its new content on a
database that applied the old content — it is skipped, and the application runs
against a schema the file does not describe.

Wire it into CI with the two revisions read out of git:

```go
previous := os.DirFS(previousCheckout)  // e.g. the merge base
current := os.DirFS("migrations")
_, err := migrate.Replay(ctx, pool, migrate.ReplayOptions{
	Previous: previous,
	Current:  current,
})
```

A nil `Previous` is the empty set, which is what a repository's first revision
has.

What `Replay` does not catch: a changed migration that applies cleanly and
leaves a different schema. `add column if not exists x text` at the earlier
revision and `... x integer` now is a no-op the second time, the column stays
`text`, and nothing errors. Detecting that needs a schema dump comparison.

## Writing tests

Table-driven, with testify, naming the case rather than the index.

Where a check could pass without exercising what it names, write the case that
must fail and confirm you have seen it fail. `pg/testdb`'s own tests do this
explicitly: `TestTheNaiveProbeAcceptsTheRestartFixture` asserts that a
two-tries-and-done probe accepts the restart fixture, so if that fixture ever
stops discriminating between the real oracle and a naive one, the suite says so
rather than passing quietly.

The same idea, applied by hand, is how this repository is reviewed: change one
guard at a time and check that a named test goes red. A guard no test defends is
a guard that will be deleted by someone refactoring in good faith.

## What a green run does not prove

A suite that runs against a deployed environment proves nothing unless you know
which build it ran against. `httpx/buildinfo` exists for this: `/healthz` and
`/readyz` carry the revision, so a test run can record what answered it. Inject
the revision at link time:

```
go build -ldflags "-X github.com/ManavA/keel/httpx/buildinfo.Revision=$(git rev-parse HEAD)"
```

Without it, `buildinfo.Get()` falls back to Go's VCS stamp, and reports
`unknown` when there is neither — never a value that could be mistaken for an
answer.
