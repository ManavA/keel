-- citext makes email comparisons and the uniqueness constraint below
-- case-insensitive at the database level. Handlers already lowercase an
-- email before using it, but a row written some other way (a data import, a
-- direct SQL fix, a future caller that forgets) must not be able to create
-- two accounts for "Person@example.com" and "person@example.com".
--
-- Pinned to the public schema explicitly, and referenced below as
-- public.citext rather than bare CITEXT: an extension can only be
-- installed once per database, in one schema — CREATE EXTENSION IF NOT
-- EXISTS with no SCHEMA clause installs into whichever schema is first on
-- the CONNECTING ROLE'S search_path, which is not necessarily public for a
-- connection scoped to a per-tenant schema. Two schemas racing to install
-- the same extension into different locations, or a role whose search_path
-- never includes public at all, would otherwise get an extension neither
-- expected. Qualifying both the install and every use makes this migration
-- correct regardless of the connecting role's search_path.
CREATE EXTENSION IF NOT EXISTS citext SCHEMA public;

CREATE TABLE IF NOT EXISTS auth_users (
    id             TEXT PRIMARY KEY,
    email          public.CITEXT UNIQUE,
    name           TEXT NOT NULL DEFAULT '',
    password_hash  TEXT NOT NULL DEFAULT '',
    external_uid   TEXT UNIQUE,
    email_verified BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
