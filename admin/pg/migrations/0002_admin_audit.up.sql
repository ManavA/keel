-- Append-only record of privileged admin actions. Rows are only ever
-- inserted: this package's Go code exposes no update or delete, so the table
-- is a record of what happened, not a mutable table.
CREATE TABLE IF NOT EXISTS admin_audit (
    id         BIGSERIAL PRIMARY KEY,
    actor      TEXT NOT NULL,
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    outcome    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_actor_created
    ON admin_audit (actor, created_at);
