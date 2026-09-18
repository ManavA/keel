-- Opaque sessions (SessionOpaque, the default SessionMode). The token_hash
-- column stores a hash of the bearer token a client presents, not the token
-- itself, for the same reason verification and reset tokens are hashed: a
-- database dump must not hand out live bearer credentials. SessionStore's
-- interface takes the raw token; an implementation hashes it on the way in
-- and out. JWT sessions (SessionJWT) need no table.
CREATE TABLE IF NOT EXISTS auth_sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_auth_sessions_user_id ON auth_sessions(user_id);
