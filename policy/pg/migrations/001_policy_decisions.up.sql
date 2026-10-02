-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

-- The decision log. Append-only: there is no update and no delete, so it is
-- a record of what was decided and why.
CREATE TABLE IF NOT EXISTS policy_decisions (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    decided_at     TIMESTAMPTZ NOT NULL,
    kind           TEXT NOT NULL,
    target         TEXT NOT NULL DEFAULT '',
    attrs          JSONB NOT NULL DEFAULT '{}'::jsonb,
    effect         TEXT NOT NULL CHECK (effect IN ('allow', 'ask', 'block')),
    rule           TEXT NOT NULL,
    rule_index     INTEGER NOT NULL,
    matched        JSONB NOT NULL DEFAULT '[]'::jsonb,
    uncertain      JSONB NOT NULL DEFAULT '[]'::jsonb,
    policy_version TEXT NOT NULL DEFAULT ''
);

-- List reads newest first, optionally narrowed by effect or rule.
CREATE INDEX IF NOT EXISTS policy_decisions_decided_idx ON policy_decisions (decided_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS policy_decisions_rule_idx ON policy_decisions (rule, id DESC);
