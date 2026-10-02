package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/internal/storerule"
)

const (
	approvalSQL = `select ` + approvalColumns + ` from ` + ApprovalsTable + ` where id = $1`

	// One question per attempt of a step.
	askedSQL = `
select ` + approvalColumns + ` from ` + ApprovalsTable + `
where run_id = $1 and seq = $2 and attempt = $3`

	// The question about an interrupted call carries no decision, and leaves
	// the guard's answer on the step.
	waitStepSQL = `
update ` + StepsTable + `
set status = 'waiting',
    decision = case when $3::boolean then $4 else decision end,
    rule = case when $3::boolean then $5 else rule end,
    rev = $6
where run_id = $1 and seq = $2`

	insertApprovalSQL = `
insert into ` + ApprovalsTable + ` (
    id, run_id, seq, attempt, cause, tool, input, action, rule, status, rev, requested_at, expires_at)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', $10, $11, $12)
on conflict (id) do nothing
returning ` + approvalColumns
)

// RequestApproval implements agent.Store.
func (s *Store) RequestApproval(ctx context.Context, lease agent.Lease, req agent.ApprovalRequest) (agent.Approval, error) {
	const op = "request approval"
	if !storerule.IsUUID(req.ID) {
		return agent.Approval{}, refused(op, "id %q is not a UUID", req.ID)
	}
	if !storerule.IsCause(string(req.Cause)) {
		return agent.Approval{}, refused(op, "%q is not a cause", req.Cause)
	}
	action, err := encodeAction(req.Action)
	if err != nil {
		return agent.Approval{}, refused(op, "%w", err)
	}

	var approval agent.Approval
	err = s.fenced(ctx, op, lease, func(tx pgx.Tx, _ heldRun) error {
		st, err := readStep(ctx, tx, lease.RunID, req.Seq)
		if err != nil {
			return err
		}
		if st.kind != agent.StepTool {
			return agent.ErrConflict
		}

		// Asked before, for this attempt: the approval recorded is the
		// answer, whatever its status and whatever else this request says.
		asked, err := scanApproval(tx.QueryRow(ctx, askedSQL, lease.RunID, req.Seq, st.attempts))
		if err == nil {
			approval = asked
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if st.status != req.From {
			return agent.ErrConflict
		}
		input, err := callInput(st.call)
		if err != nil {
			return err
		}

		rev, err := touch(ctx, tx, lease.RunID, req.Now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, waitStepSQL, lease.RunID, req.Seq, req.Decision != "", storerule.Kept(string(req.Decision)), storerule.Kept(req.Rule), rev)
		if err != nil {
			return err
		}
		approval, err = scanApproval(tx.QueryRow(ctx, insertApprovalSQL,
			req.ID, lease.RunID, req.Seq, st.attempts, string(req.Cause), st.name, input, action, storerule.Kept(req.Rule),
			rev, req.Now, req.ExpiresAt))
		if errors.Is(err, pgx.ErrNoRows) {
			// Another run's approval has the id. Returning the error undoes
			// the two writes above with it.
			return fmt.Errorf("id %q is already in use", req.ID)
		}
		return err
	})
	if err != nil {
		return agent.Approval{}, err
	}
	return approval, nil
}

// callInput is the arguments of a step's call as its column holds them: the
// bytes the model wrote, and JSON's null for a step with no call or a call
// with none.
func callInput(call []byte) (string, error) {
	if call == nil {
		return "null", nil
	}
	var c agent.Call
	if err := json.Unmarshal(call, &c); err != nil {
		return "", fmt.Errorf("decode the step's call: %w", err)
	}
	if len(c.Input) == 0 {
		return "null", nil
	}
	return string(c.Input), nil
}

// GetApproval implements agent.Store.
func (s *Store) GetApproval(ctx context.Context, id string) (agent.Approval, error) {
	const op = "get approval"
	if !storerule.IsUUID(id) {
		return agent.Approval{}, notFound(op)
	}
	var approval agent.Approval
	err := s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) (err error) {
		approval, err = scanApproval(tx.QueryRow(ctx, approvalSQL, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return agent.ErrNotFound
		}
		return err
	})
	if err != nil {
		return agent.Approval{}, err
	}
	return approval, nil
}

// ListApprovals implements agent.Store.
func (s *Store) ListApprovals(ctx context.Context, f agent.ApprovalFilter) ([]agent.Approval, error) {
	// A filter is not a lookup: a run's id or a status that no approval
	// could have lists nothing, and is not sent.
	if f.RunID != "" && !storerule.IsUUID(f.RunID) || !storerule.Comparable(string(f.Status)) {
		return nil, nil
	}

	var c conditions
	if f.Status != "" {
		c.and("status = " + c.arg(string(f.Status)))
	}
	if f.RunID != "" {
		c.and("run_id = " + c.arg(f.RunID))
	}
	query := `select ` + approvalColumns + ` from ` + ApprovalsTable + c.where() +
		` order by requested_at, ` + approvalIDColumn + ` limit ` + c.arg(storerule.ListLimit(f.Limit))

	var approvals []agent.Approval
	err := s.inTx(ctx, "list approvals", readCommitted, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, c.args...)
		approvals, err = collect(rows, err, scanApproval)
		return err
	})
	if err != nil {
		return nil, err
	}
	return approvals, nil
}

const (
	// lockApprovalRunSQL locks the run an approval belongs to, waiting for
	// whoever holds its row.
	lockApprovalRunSQL = `
select r.id::text from ` + RunsTable + ` r
join ` + ApprovalsTable + ` a on a.run_id = r.id
where a.id = $1
for update of r`

	answerRunSQL = `
update ` + RunsTable + `
set ` + wake + `,
    rev = rev + 1, updated_at = $2
where id = $1
returning rev`

	decideSQL = `
update ` + ApprovalsTable + `
set status = $2, decided_by = $3, reason = $4, decided_at = $5, rev = $6
where id = $1
returning ` + approvalColumns
)

// DecideApproval implements agent.Store.
//
// It takes the run's row before it reads the approval, as Park does before
// it looks for something pending. So of several answers at once the first
// to hold the row decides, and each of the others then reads an approval
// that has its answer and changes nothing; and an answer and a Park meet in
// one order or the other, never in between.
func (s *Store) DecideApproval(ctx context.Context, req agent.DecideRequest) (agent.Approval, error) {
	const op = "decide approval"
	if !storerule.IsUUID(req.ID) {
		return agent.Approval{}, notFound(op)
	}

	// Set when the approval was read, whether or not it could be decided:
	// one already answered comes back with the error.
	var approval agent.Approval
	err := s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) error {
		var runID string
		err := tx.QueryRow(ctx, lockApprovalRunSQL, req.ID).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return agent.ErrNotFound
		}
		if err != nil {
			return err
		}
		standing, err := scanApproval(tx.QueryRow(ctx, approvalSQL, req.ID))
		if errors.Is(err, pgx.ErrNoRows) {
			return agent.ErrNotFound
		}
		if err != nil {
			return err
		}
		if standing.Status != agent.ApprovalPending {
			approval = standing
			return agent.ErrAlreadyDecided
		}

		var rev int64
		if err := tx.QueryRow(ctx, answerRunSQL, runID, req.Now).Scan(&rev); err != nil {
			return err
		}
		status := agent.ApprovalDeclined
		if req.Approved {
			status = agent.ApprovalApproved
		}
		approval, err = scanApproval(tx.QueryRow(ctx, decideSQL, req.ID, string(status), storerule.Kept(req.By), storerule.Kept(req.Reason), req.Now, rev))
		return err
	})
	if err != nil {
		if errors.Is(err, agent.ErrAlreadyDecided) {
			return approval, err
		}
		return agent.Approval{}, err
	}
	return approval, nil
}

const (
	// lockDueRunsSQL locks every run that has an approval past its time, in
	// one order: children before parents, and by id within a depth. It is
	// the order a child's Finish takes its own row and then its parent's, so
	// a lapse and a child's end, or two lapses, never hold a row each that
	// the other wants. It waits for a row that is held and passes over none.
	lockDueRunsSQL = `
select id::text from ` + RunsTable + `
where id in (select run_id from ` + ApprovalsTable + ` where status = 'pending' and expires_at <= $1)
order by depth desc, ` + runIDColumn + `
for update`

	// lapseSQL lapses what is due among the approvals of the runs locked,
	// read again now that the rows are held: an approval answered or
	// cancelled while the lock was waited for is no longer pending. Each is
	// stamped with the Rev its run is about to be given.
	lapseSQL = `
update ` + ApprovalsTable + ` a
set status = 'expired', decided_at = $1, rev = r.rev + 1
from ` + RunsTable + ` r
where r.id = a.run_id
  and a.run_id = any($2::text[]::uuid[])
  and a.status = 'pending' and a.expires_at <= $1
returning a.run_id::text`

	lapseRunsSQL = `
update ` + RunsTable + `
set ` + wake + `,
    rev = rev + 1, updated_at = $1
where id = any($2::text[]::uuid[])`
)

// ExpireApprovals implements agent.Store.
func (s *Store) ExpireApprovals(ctx context.Context, now time.Time) (int, error) {
	var lapsed int
	err := s.inTx(ctx, "expire approvals", readCommitted, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, lockDueRunsSQL, now)
		locked, err := collect(rows, err, scanText)
		if err != nil || len(locked) == 0 {
			return err
		}

		rows, err = tx.Query(ctx, lapseSQL, now, locked)
		touched, err := collect(rows, err, scanText)
		if err != nil || len(touched) == 0 {
			return err
		}
		lapsed = len(touched)

		// One Rev for each run, however many of its approvals lapsed.
		seen := map[string]bool{}
		var runs []string
		for _, id := range touched {
			if !seen[id] {
				seen[id] = true
				runs = append(runs, id)
			}
		}
		_, err = tx.Exec(ctx, lapseRunsSQL, now, runs)
		return err
	})
	if err != nil {
		return 0, err
	}
	return lapsed, nil
}

func scanText(row pgx.Row) (string, error) {
	var s string
	err := row.Scan(&s)
	return s, err
}
