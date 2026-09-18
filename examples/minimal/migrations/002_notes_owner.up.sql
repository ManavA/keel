-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

-- Each note belongs to the account that created it. The column is deliberately
-- not a foreign key to auth_users: the auth package owns its own migrations
-- directory, and every directory in this module replays on its own in CI, so
-- a constraint reaching across directories would fail that replay. Deleting
-- an account therefore leaves its notes behind rather than removing them.
ALTER TABLE notes ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';

-- The listing endpoint pages one owner's notes by (created_at, id)
-- descending; this is the index that seek resolves against.
CREATE INDEX IF NOT EXISTS notes_user_created_at_id_idx ON notes (user_id, created_at DESC, id DESC);
