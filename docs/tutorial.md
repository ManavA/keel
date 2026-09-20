# From zero to a running keel service

This tutorial takes a blank machine to a deployed keel service: install,
scaffold a notes API with `keel new`, run it against Postgres, sign up and
log in, create a note, run the same service under compose, and ship it to
Cloud Run. Every command below was run in order against a fresh checkout
while writing; the output shown is real output with tokens and ids shortened
(yours will differ).

What you build: the `minimal` profile, a Postgres-backed notes API with
local auth, search and a background job. It needs Postgres
and nothing else: search is a Postgres index, mail goes to the log in
development, events are in-process.

## Prerequisites

- Go 1.26 or later (`go version`).
- Docker, for Postgres and the compose step.
- Git, with access to clone `github.com/ManavA/keel` (the repository is
  private; `gh` or an SSH key that can read it).

Check the Go version first. The module requires 1.26.0, and anything older
fails at load:

```
$ go version
go version go1.26.5 linux/amd64
```

## Install

There is no published release yet, so `go get` does not resolve:

```
$ go mod init scratch >/dev/null && go get github.com/ManavA/keel@v0.1.0
go: github.com/ManavA/keel@v0.1.0: reading https://proxy.golang.org/.../v0.1.0.info: 404 Not Found
	server response: not found: github.com/ManavA/keel@v0.1.0: invalid version: unknown revision v0.1.0
```

Until the first tag exists, clone the repository and point new projects at
the checkout with a `replace` directive (the next section shows how). The
`scaffold` command lives in the checkout itself:

```
$ git clone https://github.com/ManavA/keel.git keel
$ cd keel
$ go run ./cmd/keel --help
keel scaffolds projects built on keel.

Usage:
	keel new <name> [-module <module-path>] [-profile <profile>]

	new creates <name>/ from one of the maintained examples.
	<module-path> sets the go.mod module; it defaults to <name>.
	-profile picks the template; the default is minimal.

Profiles:

	minimal
		a Postgres-backed notes API with local auth, search and a background job.
		packages: app, auth, config, events, httpx, jobs, log, mail, pg, search
		migrations: auth: 0001_auth_users through 0005_auth_login_attempts; 001_notes; 002_notes_owner

	standard
		the minimal API plus operator auth, an outbox relay and idempotent writes.
		packages: admin, app, auth, config, events, httpx, idempotency, jobs, log, outbox, pg
		migrations: auth: 0001_auth_users through 0005_auth_login_attempts; admin: 0001_admin_users, 0002_admin_audit; outbox: 001_outbox_events, 002_outbox_events_parked; idempotency: 001_idempotency_keys; 001_fullstack_notes
```

## Scaffold with keel new

From inside the checkout, scaffold a project. The template is copied
verbatim from the maintained example, so this is the same code the
repository's own tests exercise:

```
$ go run ./cmd/keel new /tmp/mynotes
created /tmp/mynotes/ from keel's minimal profile: a Postgres-backed notes API with local auth, search and a background job.

next:
	cd /tmp/mynotes
	go mod tidy
	DATABASE_URL=postgres://keel:keel@127.0.0.1:5432/keel?sslmode=disable go run .

until github.com/ManavA/keel has a published release, point the require at a checkout instead:
	go mod edit -replace github.com/ManavA/keel=<path-to-keel>
```

Wire the checkout in and fetch dependencies:

```
$ cd /tmp/mynotes
$ go mod edit -replace github.com/ManavA/keel=$PWD/../keel
$ go mod tidy
$ ls
auth.go  config.go  go.mod  go.sum  handlers.go  main.go  main_test.go  migrations  notes.go  README.md
```

`go.mod` names `module mynotes`, Go 1.26.0, and requires keel v0.1.0 with
the replace above standing in until the release exists.

## Start Postgres and run the service

Start a throwaway Postgres in Docker:

```
$ docker run -d --rm --name keel-tut-db \
    -e POSTGRES_USER=keel -e POSTGRES_PASSWORD=keel -e POSTGRES_DB=keel \
    -p 127.0.0.1:5432:5432 postgres:16-alpine
$ until docker exec keel-tut-db pg_isready -U keel >/dev/null 2>&1; do sleep 1; done; echo ready
ready
```

Run the service against it:

```
$ DATABASE_URL='postgres://keel:keel@127.0.0.1:5432/keel?sslmode=disable' go run .
```

The log shows the configuration it loaded (secrets redacted, the database
URL with its host intact and its password replaced), the migrations it
applied, and the listen address:

```
{"level":"INFO","msg":"database connected","host":"127.0.0.1","database":"keel","max_conns":32}
{"level":"INFO","msg":"applying migration","migration":"0001_auth_users.up.sql"}
...
{"level":"INFO","msg":"migrations complete","applied":6,"already_applied":0}
{"level":"INFO","msg":"applying migration","migration":"001_notes.up.sql"}
{"level":"INFO","msg":"applying migration","migration":"002_notes_owner.up.sql"}
{"level":"INFO","msg":"migrations complete","applied":2,"already_applied":0}
{"level":"INFO","msg":"http server listening","addr":"[::]:8080"}
```

Confirm readiness in a second terminal. `/healthz` checks nothing;
`/readyz` checks the pool and the index:

```
$ curl -s localhost:8080/readyz
{"status":"ok","build":{"revision":"unknown"},"checks":{"database":"ok","search":"ok"}}
```

`revision` is `unknown` here because `go run` does not stamp a revision.
The compose step below shows the stamped form.

## Sign up and log in

The minimal profile serves a JSON API. Create an account:

```
$ curl -s -X POST localhost:8080/auth/signup \
    -H 'Content-Type: application/json' \
    -d '{"email":"you@example.com","password":"password123"}'
{"token":"172474be...","user":{"id":"f932e4e6-...","email":"you@example.com","email_verified":false},"verification_sent":true}
```

Save the token; every notes request needs it as a bearer token, and
without one the API answers 401.

Logging in again returns a fresh token for the same account:

```
$ curl -s -X POST localhost:8080/auth/login \
    -H 'Content-Type: application/json' \
    -d '{"email":"you@example.com","password":"password123"}'
{"token":"a232e293...","user":{"id":"f932e4e6-...","email":"you@example.com","email_verified":false}}
```

### Verify the address

Signup marks the address unverified and mails a link. In development
there is no mail provider, so the link is written to the service log:

```
{"level":"DEBUG","msg":"mail not sent: recipient and template model","to":"you@example.com","template":"verify-email","model":{"email":"you@example.com","url":"http://localhost:8080/verify-email?token=806e5865..."}}
```

Copy the token out of that URL and confirm it:

```
$ curl -s -X POST localhost:8080/auth/verify-email \
    -H 'Content-Type: application/json' \
    -d '{"token":"806e5865..."}'
{"email_verified":true}
```

`SITE_URL` builds these links and defaults to `http://localhost:8080`.
Set it to a URL reachable from an inbox before configuring a real mail
provider, or the links in real mail point at localhost.

## Create and read notes

Create a note with the login token:

```
$ TOKEN=$(curl -s -X POST localhost:8080/auth/login \
    -H 'Content-Type: application/json' \
    -d '{"email":"you@example.com","password":"password123"}' \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
$ curl -s -X POST localhost:8080/api/notes \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"title":"Roof repair quote","body":"Slate tiles, south side"}'
{"id":"d80e719c-...","user_id":"f932e4e6-...","title":"Roof repair quote","body":"Slate tiles, south side","created_at":"2026-09-19T21:29:27-07:00"}
```

List and search. Listing pages by keyset, so hand back `next_cursor` and
nothing else; search runs over the Postgres index:

```
$ curl -s 'localhost:8080/api/notes?limit=2' -H "Authorization: Bearer $TOKEN"
{"notes":[{"id":"d80e719c-...","user_id":"f932e4e6-...","title":"Roof repair quote","body":"Slate tiles, south side","created_at":"2026-09-19T21:29:27-07:00"}]}
$ curl -s 'localhost:8080/api/notes/search?q=tile' -H "Authorization: Bearer $TOKEN"
{"notes":[{"id":"d80e719c-...","user_id":"f932e4e6-...","title":"Roof repair quote","body":"Slate tiles, south side","created_at":"2026-09-19T21:29:27-07:00"}]}
```

Every notes query is scoped to the caller: notes belong to the account
that created them, and one account's notes are invisible to another's.
Without a token the API refuses:

```
$ curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/api/notes
401
```

## Run it under compose

`deploy/compose.yaml` in the checkout runs the service and Postgres with
`docker compose`. It builds the same code `keel new` copies (the compose
file's `CMD_PATH` points at the maintained example), so this step shows
the service as containers rather than as `go run`. Validate the render
first, then start it:

```
$ docker compose -f deploy/compose.yaml config >/dev/null && echo valid
valid
$ docker compose -f deploy/compose.yaml up --build
```

The app waits on the database healthcheck, applies migrations at startup,
and serves on :8080. Against the composed app, readiness now carries the
build identity, because the image stamps the git revision at build time:

```
$ curl -s localhost:8080/readyz
{"status":"ok","build":{"revision":"8dc01745226b...","built_at":"2026-09-20T03:14:24Z"},"checks":{"database":"ok","search":"ok"}}
```

The same API flow works unchanged against the composed service: signup,
create a note, list it.

Two scripts in `scripts/` answer the two questions every deploy raises.
`verify-deployed.sh` checks reachability; `check-deployed-revision.sh`
checks identity — that the running service is the build just pushed:

```
$ bash scripts/verify-deployed.sh http://localhost:8080 /readyz
ok   - http://localhost:8080/healthz -> 200
ok   - http://localhost:8080/readyz -> 200
$ bash scripts/check-deployed-revision.sh http://localhost:8080
current: the service is running 8dc01745226b, which matches the expected revision.
```

When the compose run is done, `docker compose -f deploy/compose.yaml
down` stops it. (If ports 8080 or 5432 are already taken on the machine,
remap the published ports in a throwaway override file; the stock file
assumes a clean host.)

## Deploy to Cloud Run

`deploy/` holds the Cloud Run path: a multi-stage `Dockerfile`, a
`cloudbuild.yaml` that builds, pushes and deploys, and the two scripts
above for confirming the result. `docs/deploy.md` is the full reference
for this section — health checks, proxies, pool sizing, migrations — and
`deploy/cloudrun.md` covers the Cloud Run specifics.

Build the service image the way Cloud Build will. The scaffolded project
keeps its main package at the project root, so `CMD_PATH` is `.`:

```
docker build \
  --build-arg CMD_PATH=. \
  --build-arg GIT_REVISION=$(git rev-parse HEAD) \
  --build-arg BUILDINFO_PACKAGE=github.com/ManavA/keel/httpx/buildinfo \
  -f /path/to/keel/deploy/Dockerfile -t mynotes .
```

(`BUILDINFO_PACKAGE` names the package holding the `Revision` variable
the revision is stamped into; the build fails loudly when it is missing
or unreachable from the built binary, rather than stamping nothing.)

The compose step already proved this image shape: same Dockerfile, same
revision stamping, same migration-at-startup behavior, verified serving
traffic above.

Submit the build with the repository's script, which stamps the current
revision explicitly (a manual `gcloud builds submit` leaves Cloud Build's
`$COMMIT_SHA` empty) and then verifies the deployed revision when given
the service URL:

```
scripts/deploy-cloudbuild.sh deploy/cloudbuild.yaml --project PROJECT \
  --substitution _SERVICE_NAME=mynotes \
  --substitution _CMD_PATH=. \
  --substitution _BUILDINFO_PACKAGE=github.com/ManavA/keel/httpx/buildinfo \
  --service-url https://mynotes-xxxx-uc.a.run.app
```

Cloud Build's substitutions carry the per-service values (`_SERVICE_NAME`,
`_REGION`, `_REPOSITORY`, `_CMD_PATH`, `_BUILDINFO_PACKAGE`); secrets
travel through Secret Manager and `--set-secrets` on the deploy step,
never through substitutions. The script refuses a missing config or a
missing `--project` before contacting GCP, and its parsing is covered by
a self-test (`scripts/deploy-cloudbuild.sh --self-test`, 4 passed).

Three Cloud Run details from `deploy/cloudrun.md` that decide whether
this works first try:

- `--args` on `gcloud run deploy` appends to the image's `ENTRYPOINT`,
  it does not replace it. A flag the binary does not understand fails
  startup with a generic "container failed to start".
- The binary reads the environment at runtime. Anything it needs
  (`DATABASE_URL`, and for a companion frontend any server-read
  variable) must be set on the Cloud Run service, not only passed as a
  Docker build argument.
- Run migrations as a Cloud Run job from a freshly built image, in the
  same deploy step. A job image that predates a new migration file
  applies nothing and still reports success.

This last step is the one part of the tutorial that needs a GCP project
of its own: everything up to it — scaffold, local run, auth and notes
flows, compose build, revision and health verification — runs without
any cloud account, and the Cloud Build submission above is the first
command that spends money.

## Where to go next

- `docs/deploy.md` promotes the local compose file toward production
  with `deploy/compose.production.yaml`: a least-privilege database
  role, required-variable passwords, resource limits, `/readyz`
  readiness, and unpublished database ports.
- `docs/backup.md` is the backup and restore runbook.
- The `standard` profile (`keel new svc -profile standard`) adds
  operator auth, an outbox relay and idempotent writes; it boots with
  `ADMIN_SECRET` set alongside `DATABASE_URL`.
- Each package's backend is chosen by configuration with an in-process
  default: Postgres search or Meilisearch (`--profile search` starts
  one under compose), log mail or Postmark, in-memory events or Pub/Sub.
  Nothing above needs rewriting to switch one on.
