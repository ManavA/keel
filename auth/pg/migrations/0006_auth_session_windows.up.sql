-- Idle timeout and absolute lifetime for opaque sessions (see
-- auth.SessionLimits). created_at anchors the absolute lifetime;
-- last_seen_at slides forward on every successful validation and anchors the
-- idle window. Both default to now(), so sessions issued before this
-- migration start both windows at deploy time rather than expiring on first
-- contact after it.
ALTER TABLE auth_sessions
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now();
