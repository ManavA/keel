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

Without a trusted header the default rate-limit key buckets by the proxy's
address, so every client shares one bucket: a few active users spend the whole
budget and everyone else gets 429s that read as abuse rather than
misconfiguration. `httpx.NewRouter` logs a warning at startup when a limit is
enabled in that state.

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

## The deploy/ and scripts/ directories

`deploy/` and `scripts/` are templates and checks for the two shapes above:
running locally with no cloud account, and running on Cloud Run.

### Running locally

`deploy/compose.yaml` runs the service and Postgres with `docker compose`.
This is the default: it requires no cloud account and no external service
beyond Docker. Start it with:

```
docker compose -f deploy/compose.yaml up
```

Add `--profile search` to also start Meilisearch, for a service that uses
keel's `search` package. A service that only uses Postgres does not need
this profile.

### Promoting the local compose file toward production

`deploy/compose.yaml` is a local-development shape: fixed credentials, no
resource limits, every port published, no health state beyond the image's own
liveness probe. `deploy/compose.production.yaml` is an overlay that replaces
those defaults without touching the local file. Render the merged stack
without starting anything:

```
docker compose -f deploy/compose.yaml -f deploy/compose.production.yaml config
```

To start it, export the variables the overlay names first — each one fails
closed at config time with a message saying which. In a real deployment the
deploy step writes them from the secret manager (with `printf`, not `echo` —
see "Configuration" above), never from a committed file. A project-root
`.env` also feeds compose interpolation and is already gitignored, which
makes it a convenient local stand-in for the secret manager.

Every production delta, next to the local default it replaces:

| Area | Local default (`compose.yaml`) | Production delta (`compose.production.yaml`) |
|---|---|---|
| Container user | The image's `USER appuser`, implicit | Pinned as `user: appuser`, so a rebuilt image that drops the directive fails loudly instead of running as root |
| Database role | The app connects as the bootstrap superuser (`keel`) | A least-privilege role (`NOSUPERUSER`, no `CREATEDB`), created once — see below |
| Passwords | `keel`/`keel` and `development-only-key`, committed in the file | Required variables (`POSTGRES_PASSWORD`, `APP_DB_PASSWORD`, `MEILI_MASTER_KEY`); a missing one means the stack does not render, let alone start |
| Resource limits | None | `deploy.resources` limits and reservations on every service |
| Postgres persistence | A named volume and nothing else | The same named volume, plus scheduled `pg_dump` (or volume snapshots with WAL archiving for point-in-time recovery) and a restore that has actually been rehearsed |
| Meilisearch persistence | A named volume, a dev master key, default dev mode | The same named volume, `MEILI_ENV=production`, master key from a secret; the index is derived data, so keep the reindex path working and a lost volume costs time, not data |
| Readiness | The image probes `/healthz` (liveness only) | A compose healthcheck on `/readyz`, so traffic stops while a dependency is down; restarts stay on process exit (`restart: unless-stopped`), never on readiness — see "Health checks" above |
| Published ports | Postgres and Meilisearch publish to the host | Publish only the app port, ideally none of them behind a reverse proxy; the overlay leaves the local mappings in place, so unpublish the database and search ports in your own file |

The least-privilege role, once:

```sql
CREATE ROLE svc_api WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD '...';
GRANT CONNECT ON DATABASE keel TO svc_api;
```

then, after migrations have run, the data-access grants the service needs on
the tables migrations created, plus `ALTER DEFAULT PRIVILEGES` so tables
future migrations create inherit them. Which grants those are depends on the
service; the point is they are data-access grants, not ownership.

One constraint decides where the grants stop. `app.Open` applies `Migrations`
at startup, so a service that migrates at `Open` needs the DDL privileges its
own migration files require on its runtime role. To keep the runtime role
DDL-free instead, ship with `Migrations` empty and run `migrate.Run` against
the same sources as a separate step — the two share the ledger, so each file
still applies exactly once (see "Migrations" above).

### Running on Cloud Run

`deploy/Dockerfile` and `deploy/cloudbuild.yaml` build and deploy a service
to Cloud Run. `deploy/cloudrun.md` documents the Cloud Run-specific details:
how `--args` interacts with the container's `ENTRYPOINT`, the difference
between build-time and runtime environment variables, why a migration job
must be run against a freshly built image, and how to confirm a scheduled
job is enabled, not only created.

`scripts/deploy-cloudbuild.sh` submits a build with the current git revision
stamped into the image, and then runs `scripts/check-deployed-revision.sh`
to confirm the deployed service reports that same revision.

### Verifying a deployment

Two scripts answer two different questions:

- `scripts/check-deployed-revision.sh` answers "is this service running the
  code I think it is running." It compares the deployed service's reported
  build revision against a git revision and exits 0 (current), 1 (a
  different, known revision is deployed), or 2 (the deployed revision
  cannot be determined).
- `scripts/verify-deployed.sh` answers "is this service healthy and
  reachable." It checks the service's health endpoint and any additional
  endpoints passed to it, and exits nonzero if any check fails.

Both scripts support `--help` and are safe to run against a service that
is not a keel service; `check-deployed-revision.sh` will report an unknown
revision rather than a false match.

### Building the migration job image

A migration job runs the same migration files on every execution, so
running it against an image that predates a new migration file applies
nothing new and still reports success. Rebuild the job's image immediately
before running the job, as one step, rather than assuming a previously
built image is current.
