-- Session epochs for admin session revocation: RevokeSessions moves an
-- admin's epoch forward, and tokens carrying an older epoch stop validating.
-- Existing rows keep epoch 0, which is also what their outstanding tokens
-- carry, so the migration revokes nothing by itself.
ALTER TABLE admin_users ADD COLUMN IF NOT EXISTS session_epoch BIGINT NOT NULL DEFAULT 0;
