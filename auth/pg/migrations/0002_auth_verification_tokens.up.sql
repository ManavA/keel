-- Stored by hash only. The raw token is the one-time credential mailed to
-- the user and is never persisted; see HashVerificationToken.
CREATE TABLE IF NOT EXISTS auth_verification_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed   BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_auth_verification_tokens_user_id
    ON auth_verification_tokens(user_id);
