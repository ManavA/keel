# worker

A jobs-only service built out of keel, wiring config, log, pg, migrate and
jobs. It serves no HTTP: a scheduler runs a heartbeat job on an interval, and
each run writes a row so an operator can see the last time the worker did
anything. It needs Postgres and nothing else.

## Running it

```
DATABASE_URL='postgres://user:pass@localhost:5432/db?sslmode=disable' go run ./examples/worker
```

The scheduler runs the heartbeat once immediately and then every
`HEARTBEAT_INTERVAL`. The newest row answers the liveness question:

```sql
SELECT job, max(ran_at) FROM worker_heartbeats GROUP BY job;
```

Stop it with SIGINT or SIGTERM; the scheduler stops and the process exits 0.

## Configuration

Only `DATABASE_URL` is required.

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — | Required. Postgres connection string |
| `ENV` | `development` | Anything else turns on Cloud Logging severities |
| `MIGRATE_ON_START` | `true` | Apply pending migrations before running |
| `HEARTBEAT_INTERVAL` | `1m` | How often the heartbeat job runs |
| `HEARTBEAT_JOB` | `heartbeat` | The heartbeat row's job name; name it per deployment when several workers share a database |

The configuration is logged at startup through `config.Redacted`, so
`DATABASE_URL` appears with its host intact and its password replaced.

## How the pieces fit

**The heartbeat row is the liveness signal.** There is no `/readyz` without
an HTTP server, so the job writes proof it ran. A missing recent row means
the worker is down, not that there was nothing to do.

**The outcome is computed from what the run counted.** One attempt and one
success is `success`; a failed write is one attempt and one failure, so the
log line says the work did not land rather than that nothing happened.

## Layout

```
main.go        wiring: configuration, logger, pool, migrations and the scheduler
config.go      the environment this service reads, and what it refuses
heartbeat.go   the job: one row per run
migrations/    embedded, so the binary carries its own schema
```
