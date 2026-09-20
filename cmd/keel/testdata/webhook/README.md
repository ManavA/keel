# webhook

A minimal webhook receiver built out of keel, wiring config, log, httpx, pg,
migrate and webhooks. It verifies each delivery's HMAC signature, stores the
delivery, and answers 202. It needs Postgres and nothing else.

## Running it

```
DATABASE_URL='postgres://user:pass@localhost:5432/db?sslmode=disable' WEBHOOK_SECRET='a-long-random-secret' go run ./examples/webhook
```

```
BODY='{"id":"abc"}'
SIG=$(BODY="$BODY" SECRET='a-long-random-secret' python3 -c 'import hmac,hashlib,os; print("sha256=" + hmac.new(os.environ["SECRET"].encode(), os.environ["BODY"].encode(), hashlib.sha256).hexdigest())')

curl -s -X POST localhost:8080/hooks/events \
  -H 'Content-Type: application/json' \
  -H 'X-Keel-Topic: note.created' \
  -H "X-Keel-Signature: $SIG" \
  -d "$BODY"
```

A delivery without a valid signature is answered 401 and stored nowhere; one
without a topic, or with a body that is not JSON, is answered 400. The newest
deliveries for a topic read straight out of the table:

```sql
SELECT topic, payload, received_at FROM webhook_deliveries WHERE topic = 'note.created' ORDER BY received_at DESC;
```

## Configuration

Only `DATABASE_URL` and `WEBHOOK_SECRET` are required.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | Required. Postgres connection string |
| `WEBHOOK_SECRET` | — | Required. The secret deliveries are signed with |
| `PORT` | `8080` | Listen port |
| `ENV` | `development` | Anything else turns on Cloud Logging severities |
| `MIGRATE_ON_START` | `true` | Apply pending migrations before serving |
| `TRUSTED_PROXIES` | empty | CIDRs of proxies in front; empty ignores forwarding headers |
| `SHUTDOWN_TIMEOUT` | `20s` | How long a graceful shutdown waits |
| `READINESS_CACHE_TTL` | `5s` | How long a readiness result is reused |

The configuration is logged at startup through `config.Redacted`, so
`DATABASE_URL` and `WEBHOOK_SECRET` appear with their values replaced.

## How the pieces fit

**Verified before parsed.** The signature is checked over the raw bytes
before the body is read as JSON, and the topic is required before the
signature, so a signed body for no topic cannot be stored under an empty one.

**Liveness and readiness answer different questions.** `/healthz` checks
nothing. `/readyz` checks the pool.

## Layout

```
main.go        wiring: configuration, logger, and an app holding the pool,
               migrations, routes and checks
config.go      the environment this service reads, and what it refuses
receiver.go    the receiver: verify, store, answer 202
migrations/    embedded, so the binary carries its own schema
```
