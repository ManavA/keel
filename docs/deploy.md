# Deploying a keel service

keel does not deploy anything for you. This is what a service built on it needs
from its platform, and the handful of places where getting it wrong produces a
failure that does not look like its cause.

## Configuration

Everything comes from the environment. `config.Load` reads it into your struct,
calls `Validate` if you implement it, and trims whitespace off anything that
looks like a secret or a URL.

That trimming is load-bearing. A secret manager returns exactly the bytes it was
given, and a value stored with `echo` carries a trailing newline; the
authentication failure that follows reads as a wrong credential rather than a
malformed one. Use `printf` rather than `echo` when you store one, and keep the
trimming as the second line of defence.

Log what you loaded, at startup, through `config.Redacted`:

```go
for _, f := range config.Redacted(&cfg) {
	slog.Info("config", "name", f.Name, "value", f.Value)
}
```

Secrets are replaced, collections report a count rather than their contents, and
URLs keep their host and lose their password. It is the quickest answer to "is
this deployment reading the variable I set", and the reason services skip it —
that a dump might contain a password — no longer applies.

## Which build is running

Inject the revision at link time:

```
go build -ldflags "-X github.com/ManavA/keel/httpx/buildinfo.Revision=$SHA" ./cmd/api
```

`/healthz` and `/readyz` then report it. Without this, a green check against a
deployed environment describes a build nobody can name, and a deployment that
has silently stopped updating looks exactly like a healthy one. `buildinfo` falls
back to Go's VCS stamp and reports `unknown` when there is neither — never
something that could be mistaken for an answer.

## Health checks

Point them at the right endpoint. They are different questions:

| Endpoint | Question | A failure means |
|---|---|---|
| `/healthz` | Is the process running? | Restart it |
| `/readyz` | Can it serve? | Route traffic elsewhere for now |

`/healthz` runs no dependency checks. `/readyz` runs them and answers 503 when
one fails. Point a restart policy at `/readyz` and a database blip restarts
every instance you have, at once, which turns a blip into an outage.

Readiness results are cached for `CacheTTL` (5 seconds by default), because
every prober and load balancer polls on its own schedule and all of it would
otherwise reach the database. A failure is cached too, including a timeout: a
dependency that cannot answer inside the endpoint's budget is not ready by that
endpoint's definition. The one result not cached is a check that failed because
the *prober* hung up, which says nothing about the dependency.

Leave `ExposeCheckErrors` off unless the endpoint is genuinely unreachable from
outside. A driver error names your host, your database, and sometimes your
credentials.

## Behind a proxy

`httpx/middleware.RealIP` ignores forwarding headers unless you configure it,
which is the only safe default: `X-Forwarded-For` is a request header like any
other, and a service reachable directly will believe whatever a client writes.

```go
RealIP: middleware.RealIPOptions{TrustedProxies: []string{"10.0.0.0/8"}}
```

The client is the rightmost entry no trusted proxy could have written. Reading
from the left — which several widely used implementations do — lets any client
pick its own rate-limit bucket on every request, since each hop appends and the
leftmost entry is whatever the original client sent.

On a managed container runtime where the front end's address range is not
enumerable, set `TrustAnyPeer: true`. It is a real loosening: anything else that
can reach the port can then claim any client address. Only set it where the
platform guarantees nothing else can.

RFC 7239 `Forwarded:` is not supported. A deployment behind a proxy that emits
only that header gets no client address, silently.

## Shutdown

`ListenAndServe` stops when its context does, so signal handling stays in `main`
where you can see it:

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

if err := srv.ListenAndServe(ctx); err != nil {
	slog.Error("server stopped", "error", err)
	os.Exit(1)
}
```

It returns only after in-flight requests have finished, so a clean return means
nothing is still running.

Keep `ShutdownTimeout` under the platform's own termination grace period. A
shutdown the platform kills partway through is not a graceful one, and the
requests it drops reach the client as 502s with nothing in your logs to explain
them.

## Migrations

Run them from one place.

`migrate.Run` applies pending files, each inside one transaction with its own
ledger row, and stops at the first error. There is no advisory lock yet, so two
instances deploying at once can both reach the same pending file. The outcome is
safe — one transaction wins, the other fails on the ledger's primary key or on
the DDL, and that run stops naming the file — but in a rolling deploy it shows
up as one replica failing while another succeeds, which costs an investigation.

A separate migration step, or a single-replica job, avoids it entirely.

Two failure modes worth knowing:

**Migrations are re-run against any database without a matching ledger row**, so
each file must be safe to apply twice. A new database, a restored one, or a
dropped ledger table replays everything.

**A migration edited after it was applied never runs its new content.** The
ledger keys on the file name. `Run` reports this as `ErrChecksumDrift`, which a
deploy step can distinguish from a clean run:

```go
result, err := migrate.Run(ctx, pool, opts)
if err != nil && !errors.Is(err, migrate.ErrChecksumDrift) {
	return err
}
```

Embed the files so the binary carries its own, and there is no question of which
migrations shipped with which build:

```go
//go:embed migrations/*.sql
var migrations embed.FS
```

## The pool

Size `MaxConns` against the database's connection limit divided by the number of
instances you will run, not against what one instance would like. `MaxConns × replicas`
is what the database sees.

`MaxConnLifetime` (an hour by default) is what lets a failover, a credential
rotation or a resized instance take effect without a restart.

Settings in the connection string are honoured, and the fields in `pg.Options`
override them where set, so a deployment can tune `pool_max_conns`,
`pool_max_conn_lifetime`, `pool_max_conn_idle_time` and `connect_timeout`
through the environment without a code change.

`pg.Open` runs a query before returning. `pgxpool.New` on its own contacts
nothing, so a service with a wrong password or an unreachable host would
otherwise start cleanly and fail on its first request instead of at startup.

## Logging

`log.New(log.Options{Cloud: true})` maps the level onto the `severity` field
Google Cloud Logging reads. Without it every entry lands at DEFAULT severity and
alert policies that match on severity match nothing — quietly, because the logs
themselves look fine.

Logs go to stdout. A container platform collects both streams, and tools that
separate them treat stderr as a problem, so ordinary operation belongs on
stdout.

## Running it

An example service and the configuration it needs are in `examples/minimal`. It
needs Postgres and nothing else: search is Postgres-backed, mail writes to the
log, events are in-process. Each of those has an opt-in external backend when
you want one, selected by configuration rather than by a rewrite.
