-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

CREATE TABLE IF NOT EXISTS worker_heartbeats (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job        TEXT NOT NULL,
    ran_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The liveness read is the newest row for one job, and this is the index
-- that lookup resolves against.
CREATE INDEX IF NOT EXISTS worker_heartbeats_job_ran_at_idx ON worker_heartbeats (job, ran_at DESC);
