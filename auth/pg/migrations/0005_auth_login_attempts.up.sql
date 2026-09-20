-- Login-attempt audit trail for the per-account throttle (see
-- Options.AccountRateLimit). One row per POST /login outcome, success and
-- failure. There is deliberately no password column — not the guess, not its
-- hash — so this table can never become a credential store.
--
-- email is CITEXT like auth_users.email, so the throttle counts "Person" and
-- "person" together even for a row the handlers never normalized. It is not
-- a foreign key to auth_users: attempts against unregistered addresses are
-- recorded too, which is what makes enumeration attempts visible.
--
-- Pinned to the public schema explicitly, the same as 0001: the extension
-- installs wherever the connecting role's search_path puts it unless told
-- otherwise, so both the install and the use are qualified.
CREATE TABLE IF NOT EXISTS auth_login_attempts (
    id           BIGSERIAL PRIMARY KEY,
    email        public.CITEXT NOT NULL,
    success      BOOLEAN NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_auth_login_attempts_email_time
    ON auth_login_attempts(email, attempted_at);
