-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic      TEXT NOT NULL,
    payload    JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The operator read is the newest deliveries for one topic, and this is the
-- index that lookup resolves against.
CREATE INDEX IF NOT EXISTS webhook_deliveries_topic_received_at_idx ON webhook_deliveries (topic, received_at DESC);
