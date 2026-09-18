-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key          TEXT PRIMARY KEY,
    request_hash TEXT NOT NULL,
    claim_id     TEXT NOT NULL,
    done         BOOLEAN NOT NULL DEFAULT FALSE,
    status       INTEGER NOT NULL DEFAULT 0,
    header       JSONB NOT NULL DEFAULT '{}'::jsonb,
    body         BYTEA NOT NULL DEFAULT ''::bytea,
    expires_at   TIMESTAMPTZ NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Claim reclaims an expired key by scanning for it; this index keeps that
-- scan off a full table scan as the table grows.
CREATE INDEX IF NOT EXISTS idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
