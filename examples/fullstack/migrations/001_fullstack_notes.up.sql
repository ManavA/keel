-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

-- Each note belongs to the account that created it. The column is deliberately
-- not a foreign key to auth_users: the auth package owns its own migrations
-- directory, and every directory in this module replays on its own in CI, so
-- a constraint reaching across directories would fail that replay. Deleting
-- an account therefore leaves its notes behind rather than removing them.
CREATE TABLE IF NOT EXISTS notes (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    TEXT NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The get endpoint reads one owner's note by id, and an operator query by
-- owner stays off a full scan with this behind it.
CREATE INDEX IF NOT EXISTS fullstack_notes_user_created_idx ON notes (user_id, created_at DESC, id DESC);
