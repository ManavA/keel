# apionly

A notes JSON API built out of keel, wiring config, log, httpx, pg, migrate,
search, jobs, events, mail and auth. It needs Postgres and nothing else. It is
the minimal profile without the browser UI: no landing page, no dashboard, no
fragments, no static assets — only the JSON API and auth.

## Running it

```
DATABASE_URL='postgres://user:pass@localhost:5432/db?sslmode=disable' go run ./examples/apionly
```

```
curl -s localhost:8080/readyz

TOKEN=$(curl -s -X POST localhost:8080/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"password123"}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')

curl -s localhost:8080/api/notes \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Roof repair quote","body":"Slate tiles, south side"}'

curl -s 'localhost:8080/api/notes?limit=2' -H "Authorization: Bearer $TOKEN"
curl -s 'localhost:8080/api/notes/search?q=tile' -H "Authorization: Bearer $TOKEN"
```

Every notes request needs the bearer token from signup or login; without one
the API answers 401. In development the verification link is written to the
service log (see `SITE_URL` below).

## Configuration

Only `DATABASE_URL` is required. Every other default is chosen so the service
runs correctly with none of them set.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | Required. Postgres connection string |
| `PORT` | `8080` | Listen port |
| `ENV` | `development` | Anything else turns on Cloud Logging severities |
| `MIGRATE_ON_START` | `true` | Apply pending migrations before serving |
| `TRUSTED_PROXIES` | empty | CIDRs of proxies in front; empty ignores forwarding headers |
| `CORS_ORIGINS` | empty | Browser origins allowed to call the API; empty allows none |
| `AUTH_SOURCES` | `local` | Which auth sources the service accepts: `local`, `firebase`, `oidc` |
| `SITE_URL` | `http://localhost:8080` | Base URL for the verification and reset links mailed to users |
| `AUTH_TOKEN_TTL` | auth default (7 days) | How long an issued session token stays valid |
| `AUTH_RATE_LIMIT_REQUESTS` | auth default (15) | Requests per window allowed per IP on the auth routes |
| `AUTH_RATE_LIMIT_WINDOW` | auth default (1 minute) | The window those requests are counted in |
| `FIREBASE_PROJECT_ID` | empty | Required to select the `firebase` source |
| `OIDC_ISSUER_URL` | empty | Required to select the `oidc` source |
| `OIDC_AUDIENCE` | empty | Required to select the `oidc` source |
| `OIDC_JWKS_URL` | empty | Overrides OIDC discovery when set |
| `RECONCILE_INTERVAL` | `15m` | How often the search index is rebuilt; `0` disables it |
| `SHUTDOWN_TIMEOUT` | `20s` | How long a graceful shutdown waits |
| `READINESS_CACHE_TTL` | `5s` | How long a readiness result is reused |

The configuration is logged at startup through `config.Redacted`, so
`DATABASE_URL` appears with its host intact and its password replaced.

## How the pieces fit

**Notes are written to Postgres and indexed inline.** The listing and the search
results are expected to agree, so indexing is part of the request rather than a
message. The in-memory bus is at-most-once: a dropped message would leave a note
that exists and cannot be found.

**The event carries the side effect that may be missed.** `note.created` is
published after the write, and one subscriber sends a notification through the
configured `mail.Sender` — here, one that writes to the log.

**The reconcile job repairs the index.** It reads every note, indexes them all
and prunes anything the table no longer has.

**Liveness and readiness answer different questions.** `/healthz` checks nothing.
`/readyz` checks the pool and the index.

**Listing pages by keyset**, so a note written while somebody is paging cannot
shift a row across a page boundary.

**Errors never quote the input or the underlying error.** A note that does not
exist and an id that is not a UUID both answer 404.

## Layout

```
main.go        wiring: configuration, logger, and an app holding the pool,
               migrations, auth, routes, checks and jobs
config.go      the environment this service reads, and what it refuses
auth.go        the auth service: DB-backed local accounts, optional Firebase/OIDC
notes.go       the notes table, including the keyset page
handlers.go    the HTTP API
migrations/    embedded, so the binary carries its own schema
```
