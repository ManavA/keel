# minimal

A small service built out of keel: a notes API with search, a background job and
a notification. It exists to be read alongside the packages, and to fail to
compile if one of their signatures drifts.

It needs Postgres and nothing else.

## Running it

```
make run-local
```

That starts a Postgres in Docker, points the service at it and serves on
:8080. Or run it against a database you already have:

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

Only `DATABASE_URL` is required.

| Variable | Default | What it does |
|---|---|---|
| `DATABASE_URL` | — | Required. Postgres connection string |
| `PORT` | `8080` | Listen port |
| `ENV` | `development` | Anything else turns on Cloud Logging severities |
| `MIGRATE_ON_START` | `true` | Apply pending migrations before serving |
| `TRUSTED_PROXIES` | empty | CIDRs of proxies in front; empty ignores forwarding headers |
| `CORS_ORIGINS` | empty | Browser origins allowed to call the API; empty allows none |
| `AUTH_SOURCES` | `local` | Refused unless `local`, until the `auth` package lands |
| `RECONCILE_INTERVAL` | `15m` | How often the search index is rebuilt; `0` disables it |
| `SHUTDOWN_TIMEOUT` | `20s` | How long a graceful shutdown waits |
| `READINESS_CACHE_TTL` | `5s` | How long a readiness result is reused |

The configuration is logged at startup through `config.Redacted`, so
`DATABASE_URL` appears with its host intact and its password replaced.

Every default is chosen so the service runs correctly with none of them set.
That is what lets it come up under a compose file holding an app and a database.

## About authentication

There is none yet. The `auth` package is on its own branch, and this example
will gain its endpoints and a `run-firebase` shape when it lands.

`AUTH_SOURCES` is read now, and refuses anything but `local`, so a deployment can
be written against the variable today and will fail loudly rather than quietly
ignoring `firebase` on a build that cannot do it.

## What is wired to what

**Notes are written to Postgres and indexed inline.** The listing and the search
results are expected to agree, so indexing is part of the request rather than a
message. The in-memory bus is at-most-once: a dropped message would leave a note
that exists and cannot be found, with nothing saying so.

**The event carries the side effect that may be missed.** `note.created` is
published after the write, and one subscriber sends a notification through the
configured `mail.Sender` — here, one that writes to the log. Losing a
notification costs a notification.

**The reconcile job repairs the index.** It reads every note, indexes them all
and prunes anything the table no longer has. It is why a failed index removal
during a delete can be logged rather than failing the request: something comes
along later and fixes it. Its `Outcome` is computed from what it counted, so an
empty table reports `idle` rather than `success`, and a read failure reports
`Fatal` rather than pruning against a set it could not confirm.

**Liveness and readiness answer different questions.** `/healthz` checks
nothing. `/readyz` checks the pool and the index. Pointing a restart policy at
the second restarts every instance during a database blip.

## Things worth copying

`pg.ParsePage` refuses a pagination parameter it cannot act on rather than
ignoring it, so `?per_page=5` is a 400 naming what the endpoint does read:

```
curl -s 'localhost:8080/api/notes?per_page=5'
{"error":"bad request","request_id":"..."}
```

The reason is in the log line the request id leads to, not in the body. Error
responses never quote the input or the underlying error, and a note that does
not exist and one that is not a UUID both answer 404 — a 400 distinguishing them
would confirm which identifiers are well formed.

Listing pages by keyset rather than by offset, so a note written while somebody
is paging cannot shift a row across a page boundary. The cursor is opaque: hand
back `next_cursor` and nothing else.

`decodeJSON` rejects unknown fields. A client sending `tilte` has made a
mistake, and a 201 that silently dropped what they meant is worse than a 400.

## Layout

```
main.go        wiring: configuration, logger, pool, migrations, index, router, jobs
config.go      the environment this service reads, and what it refuses
notes.go       the notes table, including the keyset page
handlers.go    the HTTP API
migrations/    embedded, so the binary carries its own schema
```
