# minimal

A notes API built out of keel, wiring config, log, httpx, pg, migrate, search,
jobs, events and mail. It needs Postgres and nothing else.

## Running it

```
make run-local
```

That starts a Postgres in Docker, points the service at it and serves on :8080.
Or run it against a database you already have:

```
DATABASE_URL='postgres://user:pass@localhost:5432/db?sslmode=disable' go run ./examples/minimal
```

```
curl -s localhost:8080/readyz

curl -s -X POST localhost:8080/api/notes \
  -H 'Content-Type: application/json' \
  -d '{"title":"Roof repair quote","body":"Slate tiles, south side"}'

curl -s 'localhost:8080/api/notes?limit=2'
curl -s 'localhost:8080/api/notes/search?q=tile'
```

## Configuration

Only `DATABASE_URL` is required. Every other default is chosen so the service
runs correctly with none of them set, which is what lets it come up under a
compose file holding an app and a database.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | Required. Postgres connection string |
| `PORT` | `8080` | Listen port |
| `ENV` | `development` | Anything else turns on Cloud Logging severities |
| `MIGRATE_ON_START` | `true` | Apply pending migrations before serving |
| `TRUSTED_PROXIES` | empty | CIDRs of proxies in front; empty ignores forwarding headers |
| `CORS_ORIGINS` | empty | Browser origins allowed to call the API; empty allows none |
| `AUTH_SOURCES` | `local` | Refused unless `local` |
| `RECONCILE_INTERVAL` | `15m` | How often the search index is rebuilt; `0` disables it |
| `SHUTDOWN_TIMEOUT` | `20s` | How long a graceful shutdown waits |
| `READINESS_CACHE_TTL` | `5s` | How long a readiness result is reused |

The configuration is logged at startup through `config.Redacted`, so
`DATABASE_URL` appears with its host intact and its password replaced.

There is no authentication. `AUTH_SOURCES` is read and refuses anything but
`local`, so a deployment can be written against the variable today and fails
loudly rather than ignoring a value this build cannot honour.

## How the pieces fit

**Notes are written to Postgres and indexed inline.** The listing and the search
results are expected to agree, so indexing is part of the request rather than a
message. The in-memory bus is at-most-once: a dropped message would leave a note
that exists and cannot be found.

**The event carries the side effect that may be missed.** `note.created` is
published after the write, and one subscriber sends a notification through the
configured `mail.Sender` — here, one that writes to the log.

**The reconcile job repairs the index.** It reads every note, indexes them all
and prunes anything the table no longer has, which is why a failed index removal
during a delete can be logged rather than failing the request. Its `Outcome` is
computed from what it counted, so an empty table reports `idle` rather than
`success`, and a read failure reports `Fatal` rather than pruning against a set
it could not confirm.

**Liveness and readiness answer different questions.** `/healthz` checks nothing.
`/readyz` checks the pool and the index.

**Listing pages by keyset**, so a note written while somebody is paging cannot
shift a row across a page boundary. The cursor is opaque: hand back
`next_cursor` and nothing else.

**Errors never quote the input or the underlying error.** A note that does not
exist and an id that is not a UUID both answer 404; the reason is in the log line
the request id leads to.

## Layout

```
main.go        wiring: configuration, logger, pool, migrations, index, router, jobs
config.go      the environment this service reads, and what it refuses
notes.go       the notes table, including the keyset page
handlers.go    the HTTP API
migrations/    embedded, so the binary carries its own schema
```
