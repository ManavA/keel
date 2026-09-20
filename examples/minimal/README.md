# minimal

A notes API built out of keel, wiring config, log, httpx, pg, migrate, search,
jobs, events, mail and auth. It needs Postgres and nothing else.

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
service log, so the signup → verify → login flow can be walked through from
the terminal (see `SITE_URL` below).

## Browser UI

The same service also serves HTML: a landing page at `/`, auth shells
(`/login`, `/signup`, `/verify-sent`, `/reset`, `/health`), a notes page at
`/notes` and a dashboard at `/app`, with fragments under `/ui/notes` for
htmx. Pages are shells composed out of blocks (`templates/blocks/` holds the
blocks); a fragment is a block rendered on its own, never a second copy. One
stylesheet holds both themes (`static/css/tokens.css` scopes them with
`[data-theme=landing]` and `[data-theme=dashboard]`), and htmx is vendored
under `static/vendor/` with its version pinned in `static/vendor/VERSION`.

The browser signs in through the same auth endpoints as the API: the
login and signup forms call them in-process and store the returned token in
an `HttpOnly` `SameSite=Lax` session cookie. A middleware copies that cookie
into `Authorization` where no bearer token is present, so `RequireAuth` runs
unchanged and the cookie opens exactly the rows the token would. Pages
redirect to `/login` when the session is missing; fragments keep the 401. The
notes blocks are `note_form`, `note_card`, `note_list`, `flash` and
`empty_state`; the form writes through the same store, index and event path
as a JSON create, so the two surfaces list and search the same rows, and a
missing title is 422 with the form re-rendered around a flash.

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

## Authentication

The `local` source works with nothing but the database: email and password
accounts, verification and password-reset links, and opaque sessions whose
tokens are hashed before they are stored. The auth routes live under `/auth`
(signup, login, verify-email, forgot/reset-password, resend-verification,
refresh, logout, delete-account); the auth package's own docs describe each
one. Verification and reset links go through the same mail sender as the
note notifications — the log in development, a real provider when one is
configured — built from `SITE_URL`, which must therefore be reachable from
an inbox rather than from this process.

`firebase` and `oidc` are selected through `AUTH_SOURCES` and need only
their settings (`FIREBASE_PROJECT_ID`, or `OIDC_ISSUER_URL` plus
`OIDC_AUDIENCE`); the two cannot be selected together, because one service
holds one identity-token verifier. Each composes with `local`: a federated
sign-in with a verified email links to the existing local account for that
address instead of opening a second one.

Every note belongs to the account that created it, and every notes query —
listing, search and all — is scoped to the caller, so one account's notes
are invisible to another's. Search stays correct because the index carries
the owner alongside each document and filters on it. One limit is stated
rather than hidden: the notes table does not hold a foreign key to the auth
tables (each migrations directory replays on its own in CI, and a constraint
reaching across directories would fail that replay), so deleting an account
leaves its notes behind instead of removing them.

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
main.go        wiring: configuration, logger, and an app holding the pool,
               migrations, auth, routes, checks and jobs
config.go      the environment this service reads, and what it refuses
auth.go        the auth service: DB-backed local accounts, optional Firebase/OIDC
notes.go       the notes table, including the keyset page
handlers.go    the HTTP API
ui.go          the landing shell and the static assets, parsed once at startup
ui_notes.go    the notes page and its fragments, over the same store as the API
ui_auth.go     the auth shells and the session cookie, over the same auth service
templates/     pages and the blocks they compose; a fragment is a block alone
static/        stylesheets, the favicon, and vendored htmx with a VERSION pin
migrations/    embedded, so the binary carries its own schema
```
