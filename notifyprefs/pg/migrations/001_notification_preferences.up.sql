-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

CREATE TABLE IF NOT EXISTS notification_preferences (
    user_id    TEXT PRIMARY KEY,
    -- Opt-outs as {category: {channel: true}}, e.g.
    -- '{"marketing": {"email": true}}'. Security and transactional pairs are
    -- never written here — the store strips them before persisting — so a row
    -- only ever holds suppressible pairs.
    opt_outs   JSONB NOT NULL DEFAULT '{}'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
