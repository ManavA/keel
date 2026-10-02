package pg

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/agent"
)

const stepsSQL = `select ` + stepColumns + ` from ` + StepsTable + ` where run_id = $1 order by seq`

// Steps implements agent.Store.
func (s *Store) Steps(ctx context.Context, runID string) ([]agent.Step, error) {
	const op = "steps"
	if !isUUID(runID) {
		return nil, notFound(op)
	}
	steps := []agent.Step{}
	err := s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, runExistsSQL, runID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return agent.ErrNotFound
		}
		rows, err := tx.Query(ctx, stepsSQL, runID)
		found, err := collect(rows, err, scanStep)
		if err != nil {
			return err
		}
		steps = append(steps, found...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return steps, nil
}

// isSeq reports whether seq could name a step: a journal counts from 1, in a
// four-byte column. One that could not is answered as a step that does not
// exist, without being sent.
func isSeq(seq int) bool {
	return seq >= 1 && seq <= math.MaxInt32
}

const (
	lastSeqSQL = `select coalesce(max(seq), 0) from ` + StepsTable + ` where run_id = $1`

	stepStateSQL = `select kind, status, attempts, name, call, started_at from ` + StepsTable + ` where run_id = $1 and seq = $2`
)

// stepState is what the journal methods read of a step before they move it.
type stepState struct {
	kind      agent.StepKind
	status    agent.StepStatus
	attempts  int
	name      string
	call      []byte
	startedAt *time.Time
}

// readStep reads the step at seq, and returns ErrConflict when there is none:
// a seq is a position, not an id. The caller holds the run's row, so the
// step is as it will be when the caller writes it.
func readStep(ctx context.Context, tx pgx.Tx, runID string, seq int) (stepState, error) {
	if !isSeq(seq) {
		return stepState{}, agent.ErrConflict
	}
	var (
		st           stepState
		kind, status string
	)
	err := tx.QueryRow(ctx, stepStateSQL, runID, seq).Scan(&kind, &status, &st.attempts, &st.name, &st.call, &st.startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return stepState{}, agent.ErrConflict
	}
	if err != nil {
		return stepState{}, err
	}
	st.kind, st.status = agent.StepKind(kind), agent.StepStatus(status)
	return st, nil
}

// activeMillis is the time a step that started at start and finished at end
// spent working, as the memory store measures it: in whole milliseconds, and
// not clamped. It is measured in Go, between the start as the column kept it
// and the end as the column will.
func activeMillis(start *time.Time, end time.Time) int64 {
	if start == nil {
		return 0
	}
	return end.Truncate(time.Microsecond).Sub(*start).Milliseconds()
}

const (
	insertModelStepSQL = `
insert into ` + StepsTable + ` (run_id, seq, kind, status, attempts, rev, created_at, started_at)
values ($1, $2, 'model', 'started', 1, $3, $4, $4)`

	restartModelStepSQL = `
update ` + StepsTable + `
set attempts = attempts + 1, started_at = $4, rev = $3
where run_id = $1 and seq = $2`
)

// BeginModel implements agent.Store.
func (s *Store) BeginModel(ctx context.Context, lease agent.Lease, seq int, now time.Time) error {
	return s.fenced(ctx, "begin model", lease, func(tx pgx.Tx, _ heldRun) error {
		if !isSeq(seq) {
			return agent.ErrConflict
		}
		var last int
		if err := tx.QueryRow(ctx, lastSeqSQL, lease.RunID).Scan(&last); err != nil {
			return err
		}

		write := restartModelStepSQL
		if seq == last+1 {
			write = insertModelStepSQL
		} else {
			st, err := readStep(ctx, tx, lease.RunID, seq)
			if err != nil {
				return err
			}
			if st.kind != agent.StepModel || st.status != agent.StepStarted {
				return agent.ErrConflict
			}
		}
		rev, err := touch(ctx, tx, lease.RunID, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, write, lease.RunID, seq, rev, now)
		return err
	})
}

const (
	replyRunSQL = `
update ` + RunsTable + `
set input_tokens = input_tokens + $2, output_tokens = output_tokens + $3, cost_micros = cost_micros + $4,
    model_calls = model_calls + 1, active_ms = active_ms + $5, failures = 0,
    rev = rev + 1, updated_at = $6
where id = $1
returning rev`

	completeModelStepSQL = `
update ` + StepsTable + `
set status = 'completed', name = $3, message = $4, stop = $5,
    input_tokens = $6, output_tokens = $7, cost_micros = $8,
    finished_at = $9, rev = $10
where run_id = $1 and seq = $2`

	// One proposed tool step for each call of a reply, from four lists of
	// one length: each call's seq, the tool it names, the call as written
	// and its idempotency key.
	insertToolStepsSQL = `
insert into ` + StepsTable + ` (run_id, seq, kind, status, name, turn, call, idem_key, rev, created_at)
select $1, c.seq, 'tool', 'proposed', c.name, $2, c.call::json, c.key, $3, $4
from unnest($5::integer[], $6::text[], $7::text[], $8::text[]) as c (seq, name, call, key)`
)

// CompleteModel implements agent.Store.
func (s *Store) CompleteModel(ctx context.Context, lease agent.Lease, req agent.CompleteModelRequest) error {
	const op = "complete model"
	message, err := encodeMessage(req.Message)
	if err != nil {
		return refused(op, "message: %w", err)
	}
	calls := make([]string, len(req.Message.Calls))
	names := make([]string, len(req.Message.Calls))
	for i, call := range req.Message.Calls {
		// It was encoded once already, inside the message.
		calls[i], _ = encodeCall(call)
		names[i] = kept(call.Name)
	}

	return s.fenced(ctx, op, lease, func(tx pgx.Tx, _ heldRun) error {
		st, err := readStep(ctx, tx, lease.RunID, req.Seq)
		if err != nil {
			return err
		}
		if st.kind != agent.StepModel || st.status != agent.StepStarted {
			return agent.ErrConflict
		}
		// The calls go in at the sequence numbers after the reply, which
		// only a reply at the end of the journal has free.
		if len(calls) > 0 {
			var last int
			if err := tx.QueryRow(ctx, lastSeqSQL, lease.RunID).Scan(&last); err != nil {
				return err
			}
			if req.Seq != last || len(calls) > math.MaxInt32-req.Seq {
				return agent.ErrConflict
			}
		}

		var rev int64
		err = tx.QueryRow(ctx, replyRunSQL, lease.RunID,
			req.Usage.InputTokens, req.Usage.OutputTokens, req.Usage.CostMicros,
			activeMillis(st.startedAt, req.Now), req.Now).Scan(&rev)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, completeModelStepSQL, lease.RunID, req.Seq, kept(req.Model), message, kept(string(req.Stop)),
			req.Usage.InputTokens, req.Usage.OutputTokens, req.Usage.CostMicros, req.Now, rev)
		if err != nil || len(calls) == 0 {
			return err
		}

		seqs := make([]int32, len(calls))
		keys := make([]string, len(calls))
		for i := range calls {
			seq := req.Seq + 1 + i
			seqs[i] = int32(seq) //nolint:gosec // G115: checked against the column's range above
			keys[i] = agent.StepKey(lease.RunID, seq)
		}
		_, err = tx.Exec(ctx, insertToolStepsSQL, lease.RunID, req.Seq, rev, req.Now, seqs, names, calls, keys)
		return err
	})
}

func isStepStatus(status agent.StepStatus) bool {
	switch status {
	case agent.StepProposed, agent.StepWaiting, agent.StepStarted, agent.StepCompleted, agent.StepBlocked, agent.StepDeclined:
		return true
	}
	return false
}

const (
	moveRunSQL = `
update ` + RunsTable + `
set input_tokens = input_tokens + $2, output_tokens = output_tokens + $3, cost_micros = cost_micros + $4,
    active_ms = active_ms + $5,
    failures = case when $6::boolean then 0 else failures end,
    rev = rev + 1, updated_at = $7
where id = $1
returning rev`

	// A request records only what it carries: a decision when it names one,
	// a result when it has one, a child run when it names one.
	moveStepSQL = `
update ` + StepsTable + `
set status = $3,
    decision = case when $4::boolean then $5 else decision end,
    rule = case when $4::boolean then $6 else rule end,
    result = case when $7::boolean then $8 else result end,
    is_error = case when $7::boolean then $9 else is_error end,
    child_run_id = coalesce($10::uuid, child_run_id),
    input_tokens = input_tokens + $11, output_tokens = output_tokens + $12, cost_micros = cost_micros + $13,
    attempts = attempts + case when $14::boolean then 1 else 0 end,
    started_at = case when $14::boolean then $16 else started_at end,
    finished_at = case when $15::boolean then $16 else finished_at end,
    rev = $17
where run_id = $1 and seq = $2`
)

// UpdateStep implements agent.Store.
func (s *Store) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	const op = "update step"
	if !isStepStatus(req.To) {
		return refused(op, "%q is not a step status", req.To)
	}
	if req.ChildRunID != "" && !isUUID(req.ChildRunID) {
		return refused(op, "child run id %q is not a UUID", req.ChildRunID)
	}

	return s.fenced(ctx, op, lease, func(tx pgx.Tx, _ heldRun) error {
		st, err := readStep(ctx, tx, lease.RunID, req.Seq)
		if err != nil {
			return err
		}
		if st.kind != agent.StepTool || st.status != req.From {
			return agent.ErrConflict
		}

		starts, final := req.To == agent.StepStarted, req.To.Done()
		// Only a step that was working adds to the time spent working: one
		// that waited on a person or a child did not.
		var active int64
		if final && req.From == agent.StepStarted {
			active = activeMillis(st.startedAt, req.Now)
		}
		var rev int64
		err = tx.QueryRow(ctx, moveRunSQL, lease.RunID,
			req.Usage.InputTokens, req.Usage.OutputTokens, req.Usage.CostMicros, active, final, req.Now).Scan(&rev)
		if err != nil {
			return err
		}

		var result string
		if req.Result != nil {
			result = *req.Result
		}
		_, err = tx.Exec(ctx, moveStepSQL, lease.RunID, req.Seq, string(req.To),
			req.Decision != "", kept(string(req.Decision)), kept(req.Rule),
			req.Result != nil, kept(result), req.IsError,
			nullable(req.ChildRunID),
			req.Usage.InputTokens, req.Usage.OutputTokens, req.Usage.CostMicros,
			starts, final, req.Now, rev)
		return err
	})
}
