-- CASCADE: the verification, password-reset and session tables (migrations
-- 0002-0004) all reference auth_users via a foreign key. migrate.Replay
-- applies every down migration in ascending order, so this one runs before
-- theirs and a plain DROP TABLE fails with 2BP01 (dependent objects still
-- exist).
DROP TABLE IF EXISTS auth_users CASCADE;
