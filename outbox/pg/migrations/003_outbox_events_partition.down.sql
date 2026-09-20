DROP INDEX IF EXISTS outbox_events_partition_unpublished_idx;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS partition_key;
