-- Parking for poisoned rows (issue #16): once a row exhausts
-- Options.MaxAttempts, Relay stamps parked_at and stops fetching it.
-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

ALTER TABLE outbox_events ADD COLUMN IF NOT EXISTS parked_at TIMESTAMPTZ;
