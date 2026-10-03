# agentdemo

A durable, governed agent service built out of keel, to be read and run. It
needs Postgres and nothing else: with no API key the model is scripted, so the
same run happens the same way every time.

Two agents review a batch of three documents.

| Agent | Tool | Action | Under `policy.json` |
|---|---|---|---|
| `coordinator` | `list_documents` | `read` | Allowed |
| | `review_document`, delegating to `reviewer` | `delegate` | Allowed |
| | `send_digest` | `send`, with `external`, `documents` (how many it covers) and `text_rule` when the content check matched | Asks a person above two documents |
| | `delete_document` | `delete` | Blocked |
| `reviewer` | `read_document` | `read` | Allowed |
| | `save_summary` | `write` | Allowed |

## The files

```
main.go        wiring: configuration, app, engine, worker, routes
config.go      the environment this service reads, and what it refuses
agents.go      the two definitions and their tools
documents.go   the tables: list, read, save a summary, record a digest
policy.json    the rules
policy.go      loading them; the one spelling of each kind, target and attribute;
               the textpolicy check that feeds send_digest's action
model.go       building the model: scripted, anthropic or openai, and the wrappers
script.go      the scripted model's replies, as a function of the request
prices.go      the price table, with the date and address it was read from
handlers.go    POST /api/batches; the operator-token middleware
migrations/    documents, summaries and digests, seeded
main_test.go   the service as this file runs it
```

## The two-minute script

The commands assume port 8080. If something else has it, put `PORT=18080` in
front of `make` and use that port below.

**1. Start it.** One command, no key. It starts a Postgres in Docker, or reuses
the one it started before.

```
make run-agent
```

That is `OPERATOR_TOKEN=demo LEASE_TTL=5s go run ./examples/agentdemo` against
that database. The database outlives the service, so that step 3 can kill the
service and keep the run; `make stop-agent` removes it. In a second terminal:

```
export T='Authorization: Bearer demo'
```

**2. Start a batch, and watch it.**

```
curl -s -X POST -H "$T" -H 'Idempotency-Key: batch-1' localhost:8080/api/batches
```

The answer is 202 and the run. Sending it again with the same key returns the
same run. Follow the run with its id:

```
curl -N -H "$T" localhost:8080/agent/runs/<id>/events
```

**3. Kill it half way.** Each tool sleeps for `STEP_DELAY` (750ms) so there is
time. Start another batch, and while the reviewers are working, `kill -9` the
`agentdemo` process (ctrl-c is a clean shutdown, which is not the point):

```
pkill -9 -x agentdemo
```

Start it again; the database is still there:

```
make run-agent
```

The log says `resuming run`, with the run and the step it had reached. Once the
dead process's lease lapses, five seconds here, the worker takes the run over.
The timeline shows each tool call once, and `summaries` has one row per
document for the run: `save_summary` writes inside a transaction that begins
with `agentpg.Once` on the call's key, so a call that is made again writes
nothing.

**4. Approve the digest.** The run is waiting:

```
curl -s -H "$T" 'localhost:8080/agent/approvals?status=pending'
```

shows the digest it wants to send, exactly as it would be sent, and the rule
that asked. Restart the process (ctrl-c, then `make run-agent`); the log says
`run is still waiting`, and it is. Approve it:

```
curl -s -X POST -H "$T" localhost:8080/agent/approvals/<approval id>/approve
```

The run carries on, the digest is recorded in `digests`, and the run completes.
`/decline` instead tells the model the call was declined, and nothing is sent.

**5. Read what was refused.**

```
curl -s -H "$T" localhost:8080/agent/runs/<id>/timeline
```

Each `delete_document` step is `blocked`, with the rule that blocked it:
"Documents are never deleted by an agent". The same decisions, with every
allow, are in the `policy_decisions` table.

When you are done, `make stop-agent` removes the database.

**6. Run the tests.** They need Docker, for `pg/testdb`.

```
go test ./examples/agentdemo/
```

## Configuration

| Variable | Default | |
|---|---|---|
| `DATABASE_URL` | required | |
| `OPERATOR_TOKEN` | required | The bearer token for every route but the health checks |
| `LLM_PROVIDER` | | `scripted`, `anthropic` or `openai`. Empty is `anthropic` when `ANTHROPIC_API_KEY` is set and `scripted` otherwise |
| `ANTHROPIC_API_KEY` | | |
| `COORDINATOR_MODEL` | `claude-opus-5-5` | |
| `REVIEWER_MODEL` | `claude-haiku-4-5-20251001` | |
| `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL` | | An OpenAI-compatible server; a local runtime wants the URL and the model and no key |
| `LEASE_TTL` | `5s` | How long a killed run waits to be taken over |
| `POLL_INTERVAL` | `1s` | How often the worker looks for a run when it found none |
| `STEP_DELAY` | `750ms` | Slept by each tool, so there is a run to kill |
| `RUN_MAX_COST_MICROS` | `0` | One run's cost limit in millionths of a dollar; zero is none |
| `MIGRATE_ON_START` | `true` | |
| `PORT`, `ENV`, `SHUTDOWN_TIMEOUT` | `8080`, `development`, `20s` | As the other examples |

The configuration is logged at startup with the token, the keys and the
database password replaced. No key is logged anywhere else.

## With a real model

Set `ANTHROPIC_API_KEY` and the same service runs on Claude. The refusal
fallback is on, the reply bound is 4096 tokens, and the process stops calling
the model at $50 in total. `prices.go` lists the two default models only: a
model it does not list fails its run on the first reply instead of running
unpriced, so add a row when you change `COORDINATOR_MODEL` or `REVIEWER_MODEL`.
A model writes its own digest and may behave differently from the script; the
rules decide what it may do either way.

## What a real service does differently

- **Who may approve.** One shared token names every caller `operator`. Mount
  `agent/httpapi` behind `admin.Service.RequireAdmin` and return the admin's
  id from `Actor`, so the record says who decided.
- **Timeouts.** The router is built with no request timeout, because one would
  cut the event stream off; `POST /api/batches` takes its own. Give the stream
  its own router, or every other route its own deadline.
- **The lease.** Five seconds is for the demo. See "Running the worker" in
  `docs/agents.md` for how to size it.
- **Sending.** `send_digest` writes a row. An effect outside Postgres passes
  the call's key to the receiver as its idempotency key, or goes through the
  outbox.
