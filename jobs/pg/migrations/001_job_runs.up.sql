-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

CREATE TABLE IF NOT EXISTS job_runs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name        TEXT NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,
    attempted   INTEGER NOT NULL DEFAULT 0,
    succeeded   INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    fatal       BOOLEAN NOT NULL DEFAULT FALSE,
    status      TEXT NOT NULL,
    exit_code   INTEGER NOT NULL DEFAULT 0
);

-- The query API reads one entry's newest runs first; this index keeps that
-- read off a full table scan as the table grows.
CREATE INDEX IF NOT EXISTS job_runs_name_id_idx ON job_runs (name, id DESC);
