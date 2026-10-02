package pg

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/agent"
)

// The columns each row is read by, in the order its scan function takes
// them. An id is read as text, in the one form a store knows it by, and a
// column that may be null where the Go field is a string is read as empty.

const runColumns = `id::text, agent, status, reason, input, output, error,
    coalesce(parent_id::text, ''), parent_seq, depth, coalesce(start_key, ''),
    definition, metadata,
    input_tokens, output_tokens, cost_micros, model_calls, active_ms, rev,
    lease_owner, lease_epoch, lease_expires_at, failures, next_attempt_at,
    cancel_requested, cancel_by, cancel_reason,
    created_at, updated_at, finished_at`

const stepColumns = `run_id::text, seq, kind, status, name, message, stop,
    turn, call, idem_key, decision, rule, result, is_error, coalesce(child_run_id::text, ''),
    attempts, input_tokens, output_tokens, cost_micros, rev,
    created_at, started_at, finished_at`

const approvalColumns = `id::text, run_id::text, seq, attempt, cause, tool, input, action, rule,
    status, decided_by, reason, rev, requested_at, decided_at, expires_at`

// nullable is s for a column that holds null where the Go field is empty.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// utc is t as a store hands a time back: the instant the column holds, in
// UTC, whatever zone the session or the process is in.
func utc(t time.Time) time.Time { return t.UTC() }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func scanRun(row pgx.Row) (agent.Run, error) {
	var (
		r                    agent.Run
		status               string
		definition, metadata []byte
	)
	err := row.Scan(&r.ID, &r.Agent, &status, &r.Reason, &r.Input, &r.Output, &r.Error,
		&r.ParentID, &r.ParentSeq, &r.Depth, &r.Key,
		&definition, &metadata,
		&r.Usage.InputTokens, &r.Usage.OutputTokens, &r.Usage.CostMicros, &r.ModelCalls, &r.ActiveMillis, &r.Rev,
		&r.LeaseOwner, &r.LeaseEpoch, &r.LeaseExpiresAt, &r.Failures, &r.NextAttemptAt,
		&r.CancelRequested, &r.CancelBy, &r.CancelReason,
		&r.CreatedAt, &r.UpdatedAt, &r.FinishedAt)
	if err != nil {
		return agent.Run{}, err
	}
	r.Status = agent.Status(status)
	if err := json.Unmarshal(definition, &r.Definition); err != nil {
		return agent.Run{}, fmt.Errorf("decode the definition of run %s: %w", r.ID, err)
	}
	if err := json.Unmarshal(metadata, &r.Metadata); err != nil {
		return agent.Run{}, fmt.Errorf("decode the metadata of run %s: %w", r.ID, err)
	}
	if r.Metadata == nil {
		r.Metadata = map[string]string{}
	}
	r.LeaseExpiresAt = utcPtr(r.LeaseExpiresAt)
	r.NextAttemptAt = utcPtr(r.NextAttemptAt)
	r.CreatedAt, r.UpdatedAt = utc(r.CreatedAt), utc(r.UpdatedAt)
	r.FinishedAt = utcPtr(r.FinishedAt)
	return r, nil
}

func scanStep(row pgx.Row) (agent.Step, error) {
	var (
		s                            agent.Step
		kind, status, stop, decision string
		message, call                []byte
	)
	err := row.Scan(&s.RunID, &s.Seq, &kind, &status, &s.Name, &message, &stop,
		&s.Turn, &call, &s.Key, &decision, &s.Rule, &s.Result, &s.IsError, &s.ChildRunID,
		&s.Attempts, &s.Usage.InputTokens, &s.Usage.OutputTokens, &s.Usage.CostMicros, &s.Rev,
		&s.CreatedAt, &s.StartedAt, &s.FinishedAt)
	if err != nil {
		return agent.Step{}, err
	}
	s.Kind, s.Status, s.Stop, s.Decision = agent.StepKind(kind), agent.StepStatus(status), agent.Stop(stop), agent.Effect(decision)
	if message != nil {
		s.Message = new(agent.Message)
		if err := json.Unmarshal(message, s.Message); err != nil {
			return agent.Step{}, fmt.Errorf("decode the message of step %d of run %s: %w", s.Seq, s.RunID, err)
		}
	}
	if call != nil {
		s.Call = new(agent.Call)
		if err := json.Unmarshal(call, s.Call); err != nil {
			return agent.Step{}, fmt.Errorf("decode the call of step %d of run %s: %w", s.Seq, s.RunID, err)
		}
	}
	s.CreatedAt = utc(s.CreatedAt)
	s.StartedAt, s.FinishedAt = utcPtr(s.StartedAt), utcPtr(s.FinishedAt)
	return s, nil
}

func scanApproval(row pgx.Row) (agent.Approval, error) {
	var (
		a             agent.Approval
		cause, status string
		input, action []byte
	)
	err := row.Scan(&a.ID, &a.RunID, &a.Seq, &a.Attempt, &cause, &a.Tool, &input, &action, &a.Rule,
		&status, &a.DecidedBy, &a.Reason, &a.Rev, &a.RequestedAt, &a.DecidedAt, &a.ExpiresAt)
	if err != nil {
		return agent.Approval{}, err
	}
	a.Cause, a.Status = agent.ApprovalCause(cause), agent.ApprovalStatus(status)
	// The arguments are the column's text: exactly what runs if approved.
	a.Input = json.RawMessage(input)
	if err := json.Unmarshal(action, &a.Action); err != nil {
		return agent.Approval{}, fmt.Errorf("decode the action of approval %s: %w", a.ID, err)
	}
	if a.Action.Attrs == nil {
		a.Action.Attrs = map[string]any{}
	}
	a.RequestedAt = utc(a.RequestedAt)
	a.DecidedAt, a.ExpiresAt = utcPtr(a.DecidedAt), utcPtr(a.ExpiresAt)
	return a, nil
}

// collect reads every row of a query with scan. It returns nil for none.
func collect[T any](rows pgx.Rows, err error, scan func(pgx.Row) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
