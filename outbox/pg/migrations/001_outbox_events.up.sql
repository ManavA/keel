-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

CREATE TABLE IF NOT EXISTS outbox_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic           TEXT NOT NULL,
    payload         BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ,
    last_error      TEXT
);

-- Relay's fetch reads unpublished rows in this order; next_attempt_at is
-- filtered on top of it.
CREATE INDEX IF NOT EXISTS outbox_events_unpublished_idx
    ON outbox_events (created_at)
    WHERE published_at IS NULL;
