-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

CREATE TABLE IF NOT EXISTS notes (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The listing endpoint pages by (created_at, id) descending, and this is the
-- index that lets the seek predicate resolve in one lookup rather than a scan.
-- The id is in the key because created_at is not unique: two notes written in
-- the same millisecond would otherwise let a page boundary skip a row.
CREATE INDEX IF NOT EXISTS notes_created_at_id_idx ON notes (created_at DESC, id DESC);
