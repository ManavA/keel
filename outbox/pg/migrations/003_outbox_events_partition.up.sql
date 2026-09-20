-- Optional per-aggregate ordering (issue #36): a partition key groups rows
-- whose delivery order matters, and Relay's ordered mode publishes one
-- partition's rows in creation order. Empty stays the default: a row
-- enqueued without a key publishes exactly as before.
-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS partition_key TEXT NOT NULL DEFAULT '';

-- Lets the ordered fetch find a row's older unpublished siblings without a
-- sequential scan.
CREATE INDEX IF NOT EXISTS outbox_events_partition_unpublished_idx
    ON outbox_events (partition_key, created_at)
    WHERE published_at IS NULL AND parked_at IS NULL;
