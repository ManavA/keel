ALTER TABLE auth_sessions
    DROP COLUMN IF EXISTS last_seen_at,
    DROP COLUMN IF EXISTS created_at;
