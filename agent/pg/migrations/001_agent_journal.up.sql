-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

-- One row per run. A run is runnable, waiting, or ended; a runnable run whose
-- lease has not lapsed is being executed by lease_owner.
CREATE TABLE IF NOT EXISTS agent_runs (
    id               UUID PRIMARY KEY,
    agent            TEXT NOT NULL,
    status           TEXT NOT NULL
                     CHECK (status IN ('runnable', 'waiting', 'completed', 'failed', 'cancelled')),
    reason           TEXT NOT NULL DEFAULT '',
    input            TEXT NOT NULL,
    output           TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',

    parent_id        UUID REFERENCES agent_runs (id) ON DELETE CASCADE,
    parent_seq       INTEGER NOT NULL DEFAULT 0,
    depth            INTEGER NOT NULL DEFAULT 0,

    start_key        TEXT,
    -- JSON, not JSONB: the snapshot is sent to the model on every call, and
    -- JSON gives back the text that was stored.
    definition       JSON NOT NULL,
    metadata         JSONB NOT NULL DEFAULT '{}'::jsonb,

    input_tokens     BIGINT NOT NULL DEFAULT 0,
    output_tokens    BIGINT NOT NULL DEFAULT 0,
    cost_micros      BIGINT NOT NULL DEFAULT 0,
    model_calls      INTEGER NOT NULL DEFAULT 0,
    active_ms        BIGINT NOT NULL DEFAULT 0,

    rev              BIGINT NOT NULL DEFAULT 0,

    lease_owner      TEXT NOT NULL DEFAULT '',
    lease_epoch      BIGINT NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ,
    failures         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ,

    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    cancel_by        TEXT NOT NULL DEFAULT '',
    cancel_reason    TEXT NOT NULL DEFAULT '',

    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ
);

-- Start is idempotent per agent on the caller's key.
CREATE UNIQUE INDEX IF NOT EXISTS agent_runs_start_key_idx
    ON agent_runs (agent, start_key) WHERE start_key IS NOT NULL;

-- Claim reads the oldest runnable run; the lease and backoff columns are
-- filtered on top of this.
CREATE INDEX IF NOT EXISTS agent_runs_runnable_idx
    ON agent_runs (created_at) WHERE status = 'runnable';

-- Park and Cancel look up a run's children.
CREATE INDEX IF NOT EXISTS agent_runs_parent_idx
    ON agent_runs (parent_id) WHERE parent_id IS NOT NULL;

-- The run listing is newest first, with id as the tie-break.
CREATE INDEX IF NOT EXISTS agent_runs_created_idx
    ON agent_runs (created_at DESC, id DESC);

-- The journal: one row per model call and per tool call, in order.
CREATE TABLE IF NOT EXISTS agent_steps (
    run_id        UUID NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('model', 'tool')),
    status        TEXT NOT NULL
                  CHECK (status IN ('proposed', 'waiting', 'started', 'completed', 'blocked', 'declined')),
    name          TEXT NOT NULL DEFAULT '',

    -- A model step's reply. JSON, not JSONB: the provider's own form of the
    -- turn is inside it and goes back to the provider as it was stored.
    message       JSON,
    stop          TEXT NOT NULL DEFAULT '',

    -- A tool step: the model step that proposed it, the call as written, the
    -- guard's answer, and what was returned to the model.
    turn          INTEGER NOT NULL DEFAULT 0,
    call          JSON,
    idem_key      TEXT NOT NULL DEFAULT '',
    decision      TEXT NOT NULL DEFAULT '',
    rule          TEXT NOT NULL DEFAULT '',
    result        TEXT NOT NULL DEFAULT '',
    is_error      BOOLEAN NOT NULL DEFAULT FALSE,
    child_run_id  UUID,

    attempts      INTEGER NOT NULL DEFAULT 0,
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cost_micros   BIGINT NOT NULL DEFAULT 0,
    rev           BIGINT NOT NULL,

    created_at    TIMESTAMPTZ NOT NULL,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,

    PRIMARY KEY (run_id, seq)
);

-- Changes reads the steps touched since a revision.
CREATE INDEX IF NOT EXISTS agent_steps_rev_idx ON agent_steps (run_id, rev);

-- A question put to a person about one tool step.
CREATE TABLE IF NOT EXISTS agent_approvals (
    id           UUID PRIMARY KEY,
    run_id       UUID NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    attempt      INTEGER NOT NULL,
    cause        TEXT NOT NULL CHECK (cause IN ('guard', 'tool', 'interrupted')),
    tool         TEXT NOT NULL,
    input        JSON NOT NULL,
    action       JSONB NOT NULL,
    rule         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL
                 CHECK (status IN ('pending', 'approved', 'declined', 'expired', 'cancelled')),
    decided_by   TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    rev          BIGINT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    decided_at   TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,

    -- One question per attempt of a step, so asking again is a no-op.
    UNIQUE (run_id, seq, attempt)
);

-- The operator's queue is the pending approvals, oldest first.
CREATE INDEX IF NOT EXISTS agent_approvals_pending_idx
    ON agent_approvals (requested_at) WHERE status = 'pending';

-- Idempotency keys of tool effects that have been applied. See Once.
CREATE TABLE IF NOT EXISTS agent_tool_effects (
    key        TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
