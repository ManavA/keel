-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

-- The batch the coordinator reviews.
CREATE TABLE IF NOT EXISTS documents (
    id    TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    body  TEXT NOT NULL
);

-- One row per save_summary call that took effect. There is deliberately no
-- unique constraint on (run_id, document_id): what keeps a retried call from
-- writing a second row is agent/pg's Once, and a constraint here would hide
-- it if that stopped working.
CREATE TABLE IF NOT EXISTS summaries (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id      TEXT NOT NULL,
    document_id TEXT NOT NULL,
    summary     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS summaries_document_idx ON summaries (document_id);

-- One row per digest sent. Nothing leaves the process: the row is the send.
CREATE TABLE IF NOT EXISTS digests (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id       TEXT NOT NULL,
    recipient    TEXT NOT NULL,
    subject      TEXT NOT NULL,
    body         TEXT NOT NULL,
    document_ids TEXT[] NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO documents (id, title, body) VALUES
    ('doc-1', 'Quarterly plan', 'The plan for the quarter has three goals. Each has an owner and a date.'),
    ('doc-2', 'Incident review', 'A queue backed up for forty minutes on Tuesday. The cause was a missing index.'),
    ('doc-3', 'Hiring update', 'Two offers went out this week. One was accepted and one is pending.')
ON CONFLICT (id) DO NOTHING;
