-- Pinned to the public schema explicitly, and referenced below as
-- public.citext rather than bare CITEXT — see auth/pg's identical migration
-- for why: an extension installs into whichever schema is first on the
-- connecting role's search_path unless told otherwise, which is not
-- necessarily public for a connection scoped to a per-tenant schema.
CREATE EXTENSION IF NOT EXISTS citext SCHEMA public;

CREATE TABLE IF NOT EXISTS admin_users (
    id             TEXT PRIMARY KEY,
    email          public.CITEXT UNIQUE NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    role           TEXT NOT NULL DEFAULT 'admin',
    password_hash  TEXT NOT NULL,
    last_login_at  TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
