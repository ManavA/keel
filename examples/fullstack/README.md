# fullstack

A notes API wiring keel's standard stack: app lifecycle, local auth over
Postgres, an admin service with its own sessions, an outbox relay delivering
domain events, and pg-backed idempotency on the write route. It needs
Postgres and nothing else.

Where `examples/minimal` is the smallest starting point, this is the second
one to read: the differences between the two are exactly what a real service
adds next.

## Running it

```
DATABASE_URL='postgres://user:pass@localhost:5432/db?sslmode=disable' \
ADMIN_SECRET='a-secret-at-least-sixteen-bytes' \
ADMIN_SEED_EMAIL='you@example.com' \
ADMIN_SEED_PASSWORD='a-strong-password' \
go run ./examples/fullstack
```

```
curl -s localhost:8080/readyz

TOKEN=$(curl -s -X POST localhost:8080/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"password123"}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')

curl -s localhost:8080/api/notes \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: first-note' \
  -d '{"title":"Roof repair quote","body":"Slate tiles, south side"}'

curl -s -X POST localhost:8080/admin/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"a-strong-password"}'
```

Every notes request needs the bearer token from signup or login; without one
the API answers 401. There is no mail sender, so signup reports verification
as not sent and the account stays unverified — the auth package's own docs
describe the flow once a sender is configured.

## Configuration

Only `DATABASE_URL` and `ADMIN_SECRET` are required.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | Required. Postgres connection string |
| `ADMIN_SECRET` | — | Required. Signs the admin sessions; must differ from any end-user auth secret |
| `PORT` | `8080` | Listen port |
| `ENV` | `development` | Anything else turns on Cloud Logging severities |
| `MIGRATE_ON_START` | `true` | Apply pending migrations before serving |
| `SITE_URL` | `http://localhost:8080` | Base URL for verification and reset links, once a mail sender is configured |
| `ADMIN_SEED_EMAIL` | empty | Seeds the first admin at startup when set together with the password |
| `ADMIN_SEED_PASSWORD` | empty | Seeds the first admin at startup when set together with the email |
| `RELAY_INTERVAL` | `5s` | How often the outbox relay polls; `0` disables it |
| `SHUTDOWN_TIMEOUT` | `20s` | How long a graceful shutdown waits |
| `READINESS_CACHE_TTL` | `5s` | How long a readiness result is reused |

The configuration is logged at startup through `config.Redacted`, so
`DATABASE_URL` appears with its host intact and its password replaced.

## How the pieces fit

**The row and the event commit together.** Creating a note inserts it and
enqueues its `note.created` event in one transaction. The relay job then
publishes what the transaction left behind, onto the in-process bus, where
one subscriber logs the delivery. A crash between the commit and the publish
leaves a row the next tick still delivers; a delivery the process dies
halfway through is delivered again, carrying the same outbox id, so the
subscriber must tolerate seeing an event twice.

**A retried create replays.** The POST route sits behind the pg-backed
idempotency middleware: the same `Idempotency-Key` and body returns the
first response again, with `Idempotency-Replayed: true`, instead of writing
a second note. The claim lives in Postgres rather than in memory, so it
holds across restarts and across instances.

**Auth and admin stay separate.** End-user sessions come from the auth
package's local source; operator sessions come from the admin package, with
a different secret and a different token audience, so a token from one never
validates against the other. The seeded admin logs in at `/admin/login`;
operator-only routes would hang off the admin router behind its
`RequireAdmin`, the way `/admin/refresh` already does.

**Liveness and readiness answer different questions.** `/healthz` checks
nothing. `/readyz` checks the pool plus the two tables this service cannot
run without — the outbox and the idempotency keys — so a migration the
deployment forgot to apply answers at the probe instead of at the first
write.

## Layout

```
main.go        wiring: configuration, logger, and an app holding the pool,
               migrations, auth, admin, routes, checks and the relay job
config.go      the environment this service reads, and what it refuses
auth.go        the auth service: local accounts over Postgres, no mail sender
admin.go       the admin service: operator accounts over Postgres
notes.go       the notes table, writing each row with its outbox event
handlers.go    the HTTP API, with idempotency on the create route
migrations/    embedded, so the binary carries its own schema
```
