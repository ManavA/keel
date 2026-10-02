package pg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/internal/storerule"
	keelpg "github.com/ManavA/keel/pg"
)

// Table names the migrations create.
const (
	RunsTable      = "agent_runs"
	StepsTable     = "agent_steps"
	ApprovalsTable = "agent_approvals"
	EffectsTable   = "agent_tool_effects"
)

// Store is an agent.Store backed by Postgres. The zero value is not usable;
// build one with New.
type Store struct {
	db keelpg.Beginner
}

// New builds a Store over db, usually a *pgxpool.Pool, which must already
// have the schema from agent/pg/migrations applied. keelpg is Keel's own pg
// package: every method here is one pg.InTx.
func New(db keelpg.Beginner) *Store {
	return &Store{db: db}
}

var _ agent.Store = (*Store)(nil)

// readCommitted is what every transaction that writes asks for by name. The
// writers lock a run's row and then read what the lock protects in a later
// statement, which sees what was committed while they waited only under read
// committed. A database whose default is stricter would otherwise have them
// decide on a snapshot taken before the wait.
var readCommitted = pgx.TxOptions{IsoLevel: pgx.ReadCommitted}

// oneView is what Changes reads under: every statement sees the database as
// it stood at the first.
var oneView = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// inTx runs fn in one transaction and names op in whatever error comes back.
func (s *Store) inTx(ctx context.Context, op string, opts pgx.TxOptions, fn func(tx pgx.Tx) error) error {
	if err := keelpg.InTxOptions(ctx, s.db, opts, fn); err != nil {
		return fmt.Errorf("agent/pg: %s: %w", op, err)
	}
	return nil
}

// refused is the error for a request that is wrong in itself. It is made
// before a transaction is opened, and is none of the package's sentinels.
func refused(op, format string, args ...any) error {
	return fmt.Errorf("agent/pg: "+op+": "+format, args...)
}

// notFound is the answer for an id that is not a UUID in the one form, which
// names nothing and is never sent to the database.
func notFound(op string) error {
	return fmt.Errorf("agent/pg: %s: %w", op, agent.ErrNotFound)
}

// fitsColumn reports whether n fits a four-byte integer column.
func fitsColumn(n int) bool {
	return n >= math.MinInt32 && n <= math.MaxInt32
}

// The id columns, named with their tables for an order by. The rows are read
// with the id as text, under the column's own name, and a bare name in an
// order by means the column read before it means the column stored: so a
// bare id would sort the text, in the database's collation and past the
// index, where these sort the uuid.
const (
	runIDColumn      = RunsTable + ".id"
	approvalIDColumn = ApprovalsTable + ".id"
)

// ended is the statuses of a run that has ended, as SQL.
const ended = `('completed', 'failed', 'cancelled')`

// wake is the part of an update that makes a waiting run runnable and leaves
// any other as it is. It no longer waits, so the reason it waited goes too.
const wake = `status = case when status = 'waiting' then 'runnable' else status end,
    reason = case when status = 'waiting' then '' else reason end`

const insertRunSQL = `
insert into ` + RunsTable + ` (
    id, agent, status, reason, input, output, error,
    parent_id, parent_seq, depth, start_key, definition, metadata,
    input_tokens, output_tokens, cost_micros, model_calls, active_ms, rev,
    lease_owner, lease_epoch, lease_expires_at, failures, next_attempt_at,
    cancel_requested, cancel_by, cancel_reason, created_at, updated_at, finished_at)
select $1::uuid, $2::text, $3::text, $4::text, $5::text, $6::text, $7::text,
    $8::uuid, $9::integer, $10::integer, $11::text, $12::json, $13::jsonb,
    $14::bigint, $15::bigint, $16::bigint, $17::integer, $18::bigint, 1,
    $19::text, $20::bigint, $21::timestamptz, $22::integer, $23::timestamptz,
    $24::boolean, $25::text, $26::text, $27::timestamptz, $28::timestamptz, $29::timestamptz
where ($8::uuid is null and $10::integer = 0)
   or exists (select 1 from ` + RunsTable + ` where id = $8::uuid and depth = $10::integer - 1)
on conflict do nothing
returning ` + runColumns

const (
	runSQL       = `select ` + runColumns + ` from ` + RunsTable + ` where id = $1`
	runByKeySQL  = `select ` + runColumns + ` from ` + RunsTable + ` where agent = $1 and start_key = $2`
	runExistsSQL = `select exists (select 1 from ` + RunsTable + ` where id = $1)`
	runDepthSQL  = `select depth from ` + RunsTable + ` where id = $1`
)

// CreateRun implements agent.Store.
//
// The insert passes over a run it conflicts with, on its key or on its id,
// and a run that is not one level below a parent that exists, or not at
// depth zero with none. Which of those it was is read afterwards, key first:
// so a create that finds its key answers for the run that has it whatever
// else the store would refuse, and two creates of one key at once make one
// run, the second waiting on the first's row.
func (s *Store) CreateRun(ctx context.Context, run agent.Run) (agent.Run, bool, error) {
	const op = "create run"
	if !storerule.IsUUID(run.ID) {
		return agent.Run{}, false, refused(op, "id %q is not a UUID", run.ID)
	}
	if run.ParentID != "" && !storerule.IsUUID(run.ParentID) {
		return agent.Run{}, false, refused(op, "parent id %q is not a UUID", run.ParentID)
	}
	if run.Status != agent.StatusRunnable {
		return agent.Run{}, false, refused(op, "status is %q, not %q", run.Status, agent.StatusRunnable)
	}
	if !storerule.Comparable(run.Agent) || !storerule.Comparable(run.Key) || !storerule.Comparable(run.LeaseOwner) {
		return agent.Run{}, false, refused(op, "the agent %q, the key %q or the owner %q holds a character no column keeps",
			run.Agent, run.Key, run.LeaseOwner)
	}
	if !fitsColumn(run.ParentSeq) || !fitsColumn(run.Depth) || !fitsColumn(run.ModelCalls) || !fitsColumn(run.Failures) {
		return agent.Run{}, false, refused(op, "a count does not fit its column: parent seq %d, depth %d, model calls %d, failures %d",
			run.ParentSeq, run.Depth, run.ModelCalls, run.Failures)
	}
	definition, err := encodeSnapshot(run.Definition)
	if err != nil {
		return agent.Run{}, false, refused(op, "definition: %w", err)
	}
	metadata, err := encodeMetadata(run.Metadata)
	if err != nil {
		return agent.Run{}, false, refused(op, "metadata: %w", err)
	}

	var (
		stored  agent.Run
		created bool
	)
	err = s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) error {
		// The insert, and then the look for why it stored nothing. Between
		// the two the reason can go away: the parent it did not find is
		// stored, or the run it conflicted with is purged. The insert is
		// then made once more, and not answered with a reason that is no
		// longer so.
		for try := 1; ; try++ {
			inserted, err := scanRun(tx.QueryRow(ctx, insertRunSQL,
				run.ID, run.Agent, string(run.Status), storerule.Kept(run.Reason), storerule.Kept(run.Input), storerule.Kept(run.Output), storerule.Kept(run.Error),
				nullable(run.ParentID), run.ParentSeq, run.Depth, nullable(run.Key), definition, metadata,
				run.Usage.InputTokens, run.Usage.OutputTokens, run.Usage.CostMicros, run.ModelCalls, run.ActiveMillis,
				run.LeaseOwner, run.LeaseEpoch, run.LeaseExpiresAt, run.Failures, run.NextAttemptAt,
				run.CancelRequested, storerule.Kept(run.CancelBy), storerule.Kept(run.CancelReason), run.CreatedAt, run.UpdatedAt, run.FinishedAt))
			if err == nil {
				stored, created = inserted, true
				return nil
			}
			// The parent was there when the insert looked and was removed
			// before Postgres checked the reference: it does not exist.
			var failure *pgconn.PgError
			if errors.As(err, &failure) && failure.Code == foreignKeyViolation {
				return fmt.Errorf("parent %s does not exist", run.ParentID)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}

			first, err := notStored(ctx, tx, run)
			if err != nil {
				return err
			}
			if first != nil {
				stored = *first
				return nil
			}
			if try == 2 {
				return errors.New("the runs this one depends on changed twice while it was being stored")
			}
		}
	})
	if err != nil {
		return agent.Run{}, false, err
	}
	return stored, created, nil
}

// foreignKeyViolation is the SQLSTATE of a reference to a row that is not
// there.
const foreignKeyViolation = "23503"

// notStored says why the insert of run stored nothing, in the order the
// contract gives: the run that has its key, which is returned; or an error
// for an id in use, a parent that does not exist, or a depth that is not the
// parent's plus one. It returns neither when none of these is so any more.
func notStored(ctx context.Context, tx pgx.Tx, run agent.Run) (*agent.Run, error) {
	if run.Key != "" {
		first, err := scanRun(tx.QueryRow(ctx, runByKeySQL, run.Agent, run.Key))
		if err == nil {
			return &first, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	var taken bool
	if err := tx.QueryRow(ctx, runExistsSQL, run.ID).Scan(&taken); err != nil {
		return nil, err
	}
	if taken {
		return nil, fmt.Errorf("id %q is already in use", run.ID)
	}
	// A child is one level below its parent, and a run nobody started is at
	// none: the order rows are locked in, children before parents, is taken
	// from the depth.
	if run.ParentID == "" {
		if run.Depth != 0 {
			return nil, fmt.Errorf("depth is %d for a run no other started", run.Depth)
		}
		return nil, nil
	}
	var depth int
	err := tx.QueryRow(ctx, runDepthSQL, run.ParentID).Scan(&depth)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("parent %s does not exist", run.ParentID)
	}
	if err != nil {
		return nil, err
	}
	if run.Depth != depth+1 {
		return nil, fmt.Errorf("depth is %d under a parent at depth %d", run.Depth, depth)
	}
	return nil, nil
}

// getRun reads run id, which is known to be a UUID.
func getRun(ctx context.Context, tx pgx.Tx, id string) (agent.Run, error) {
	run, err := scanRun(tx.QueryRow(ctx, runSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return agent.Run{}, agent.ErrNotFound
	}
	return run, err
}

// GetRun implements agent.Store.
func (s *Store) GetRun(ctx context.Context, id string) (agent.Run, error) {
	const op = "get run"
	if !storerule.IsUUID(id) {
		return agent.Run{}, notFound(op)
	}
	var run agent.Run
	err := s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) (err error) {
		run, err = getRun(ctx, tx, id)
		return err
	})
	if err != nil {
		return agent.Run{}, err
	}
	return run, nil
}

// conditions builds the where clause of a listing from the filters that are
// set, so that each listing asks only for what narrows it and can use the
// index made for that.
type conditions struct {
	clauses []string
	args    []any
}

// arg adds a value and returns its placeholder.
func (c *conditions) arg(v any) string {
	c.args = append(c.args, v)
	return "$" + strconv.Itoa(len(c.args))
}

func (c *conditions) and(clause string) { c.clauses = append(c.clauses, clause) }

func (c *conditions) where() string {
	if len(c.clauses) == 0 {
		return ""
	}
	return " where " + strings.Join(c.clauses, " and ")
}

// listCursor is the position a listing starts after, or nil for the start.
// storerule.Cursor says which cursors are refused.
func listCursor(c *agent.Cursor) (*agent.Cursor, error) {
	if c == nil {
		return nil, nil
	}
	start, err := storerule.Cursor(c.ID, c.CreatedAt)
	if err != nil || start {
		return nil, err
	}
	return c, nil
}

// ListRuns implements agent.Store.
//
// A cursor that is refused is refused before anything is asked of the
// database. One that is taken is a position: a run is older than it when its
// (CreatedAt, ID) sorts below the cursor's.
func (s *Store) ListRuns(ctx context.Context, f agent.RunFilter) ([]agent.Run, error) {
	const op = "list runs"
	before, err := listCursor(f.Before)
	if err != nil {
		return nil, refused(op, "%w", err)
	}
	// A filter is not a lookup: a parent's id, an agent's name or a status
	// that no run could have lists nothing, and is not sent.
	if f.ParentID != "" && !storerule.IsUUID(f.ParentID) || !storerule.Comparable(f.Agent) || !storerule.Comparable(string(f.Status)) {
		return []agent.Run{}, nil
	}

	var c conditions
	if f.Status != "" {
		c.and("status = " + c.arg(string(f.Status)))
	}
	if f.Agent != "" {
		c.and("agent = " + c.arg(f.Agent))
	}
	if f.ParentID != "" {
		c.and("parent_id = " + c.arg(f.ParentID))
	}
	if before != nil {
		c.and("(created_at, id) < (" + c.arg(before.CreatedAt) + ", " + c.arg(before.ID) + "::uuid)")
	}
	query := `select ` + runColumns + ` from ` + RunsTable + c.where() +
		` order by created_at desc, ` + runIDColumn + ` desc limit ` + c.arg(storerule.ListLimit(f.Limit))

	runs := []agent.Run{}
	err = s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, c.args...)
		found, err := collect(rows, err, scanRun)
		if err != nil {
			return err
		}
		runs = append(runs, found...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return runs, nil
}

// claimable is the condition under which a run can be taken at the time and
// for the agents its two placeholders name. Both forms of the claim use it.
func claimable(now, agents string) string {
	return `status = 'runnable'
      and agent = any(` + agents + `)
      and (lease_expires_at is null or lease_expires_at <= ` + now + `)
      and (next_attempt_at is null or next_attempt_at <= ` + now + `)`
}

// claimSet is what a claim changes: $1 is the claim's time, $2 its owner
// and $3 when the new lease lapses. An owner still on the run means its
// lease lapsed and was never given back, which counts as a failure.
const claimSet = `
set failures         = failures + case when lease_owner <> '' then 1 else 0 end,
    lease_owner      = $2,
    lease_epoch      = lease_epoch + 1,
    lease_expires_at = $3,
    rev              = rev + 1,
    updated_at       = $1`

var (
	// claimNextSQL takes the oldest run that is free. A run whose row is
	// locked is being written to or claimed by someone else, and is passed
	// over: two claims at once take different runs and neither waits. Runs
	// of one instant are taken in the order of their ids.
	claimNextSQL = `
with next as (
    select id as next_id from ` + RunsTable + `
    where ` + claimable("$1", "$4") + `
    order by created_at, id
    for update skip locked
    limit 1
)
update ` + RunsTable + claimSet + `
from next
where id = next_id
returning ` + runColumns

	// lockClaimSQL locks one run, waiting for whoever holds its row, and
	// says whether it can be taken as it then stands.
	lockClaimSQL = `select ` + claimable("$2", "$3") + ` from ` + RunsTable + ` where id = $1 for update`

	claimRunSQL = `update ` + RunsTable + claimSet + ` where id = $4 returning ` + runColumns
)

// Claim implements agent.Store.
func (s *Store) Claim(ctx context.Context, req agent.ClaimRequest) (*agent.Run, error) {
	const op = "claim"
	// A hold with no owner is no hold: the run would read as released, and
	// every write under it would be refused.
	if req.Owner == "" {
		return nil, refused(op, "owner is empty")
	}
	if !storerule.Comparable(req.Owner) {
		return nil, refused(op, "owner %q holds a character no column keeps", req.Owner)
	}
	if req.TTL <= 0 {
		return nil, refused(op, "ttl is %s, not more than zero", req.TTL)
	}
	if req.RunID != "" && !storerule.IsUUID(req.RunID) {
		return nil, notFound(op)
	}
	// Never null: a null list makes the condition null, not false. A name
	// no column keeps is no run's agent, and is not sent.
	agents := []string{}
	for _, name := range req.Agents {
		if storerule.Comparable(name) {
			agents = append(agents, name)
		}
	}
	// To the microsecond above: the column keeps no more, and kept to the
	// one below the lease would be free to take before its holder, who added
	// the same numbers, counts it lapsed.
	expires := storerule.Expiry(req.Now.Add(req.TTL))

	var claimed *agent.Run
	err := s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) error {
		var row pgx.Row
		if req.RunID == "" {
			row = tx.QueryRow(ctx, claimNextSQL, req.Now, req.Owner, expires, agents)
		} else {
			var free bool
			err := tx.QueryRow(ctx, lockClaimSQL, req.RunID, req.Now, agents).Scan(&free)
			if errors.Is(err, pgx.ErrNoRows) {
				return agent.ErrNotFound
			}
			if err != nil {
				return err
			}
			if !free {
				return agent.ErrNotClaimable
			}
			row = tx.QueryRow(ctx, claimRunSQL, req.Now, req.Owner, expires, req.RunID)
		}
		run, err := scanRun(row)
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing to take.
			return nil
		}
		if err != nil {
			return err
		}
		claimed = &run
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// lockRunSQL is the fence's first half: it locks the run's row, waiting for
// whoever holds it, and reads the hold the run records as it then stands.
const lockRunSQL = `
select lease_owner, lease_epoch, cancel_requested, coalesce(parent_id::text, '')
from ` + RunsTable + ` where id = $1 for update`

// heldRun is what a method that takes a Lease knows of the run once it holds
// its row.
type heldRun struct {
	cancelRequested bool
	parentID        string
}

// fenced runs fn in a transaction that holds the row of the run lease names,
// and only if lease is the hold the run records: its owner and its epoch,
// and never the time. Otherwise it returns ErrLeaseLost, or ErrNotFound for
// a run that does not exist, and fn is not called.
//
// Every method that takes a Lease does its work in here. So everything it
// reads, it reads under the lock, and nothing it writes can land in a
// journal another process now owns: a claim takes the same row and raises
// the epoch, and so comes wholly before the write or wholly after it.
func (s *Store) fenced(ctx context.Context, op string, lease agent.Lease, fn func(tx pgx.Tx, run heldRun) error) error {
	if !storerule.IsUUID(lease.RunID) {
		return notFound(op)
	}
	return s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) error {
		run, err := held(ctx, tx, lease, lockRunSQL)
		if err != nil {
			return err
		}
		return fn(tx, run)
	})
}

// held is the fence: it locks the run with lockSQL and compares the hold the
// row records with lease.
func held(ctx context.Context, tx pgx.Tx, lease agent.Lease, lockSQL string) (heldRun, error) {
	var (
		run   heldRun
		owner string
		epoch int64
	)
	err := tx.QueryRow(ctx, lockSQL, lease.RunID).Scan(&owner, &epoch, &run.cancelRequested, &run.parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return heldRun{}, agent.ErrNotFound
	}
	if err != nil {
		return heldRun{}, err
	}
	// A run nobody holds records an empty owner, which a lease with no owner
	// would otherwise match.
	if lease.Owner == "" || owner != lease.Owner || epoch != lease.Epoch {
		return heldRun{}, agent.ErrLeaseLost
	}
	return run, nil
}

const touchSQL = `update ` + RunsTable + ` set rev = rev + 1, updated_at = $2 where id = $1 returning rev`

// touch records a change to the run at now and returns the run's new Rev,
// which the caller stamps on each step and approval it changed. The caller
// holds the run's row.
func touch(ctx context.Context, tx pgx.Tx, runID string, now time.Time) (int64, error) {
	var rev int64
	err := tx.QueryRow(ctx, touchSQL, runID, now).Scan(&rev)
	return rev, err
}

const heartbeatSQL = `update ` + RunsTable + ` set lease_expires_at = $2 where id = $1`

// Heartbeat implements agent.Store.
func (s *Store) Heartbeat(ctx context.Context, lease agent.Lease, now time.Time, ttl time.Duration) (bool, error) {
	const op = "heartbeat"
	if ttl <= 0 {
		return false, refused(op, "ttl is %s, not more than zero", ttl)
	}

	var cancelRequested bool
	err := s.fenced(ctx, op, lease, func(tx pgx.Tx, run heldRun) error {
		cancelRequested = run.cancelRequested
		_, err := tx.Exec(ctx, heartbeatSQL, lease.RunID, storerule.Expiry(now.Add(ttl)))
		return err
	})
	if err != nil {
		return false, err
	}
	return cancelRequested, nil
}

const yieldSQL = `
update ` + RunsTable + `
set lease_owner = '', lease_expires_at = null, next_attempt_at = $2,
    failures = failures + case when $3::boolean then 1 else 0 end,
    error = case when $3::boolean then $4 else error end,
    rev = rev + 1, updated_at = $5
where id = $1`

// Yield implements agent.Store.
func (s *Store) Yield(ctx context.Context, lease agent.Lease, req agent.YieldRequest) error {
	return s.fenced(ctx, "yield", lease, func(tx pgx.Tx, _ heldRun) error {
		_, err := tx.Exec(ctx, yieldSQL, lease.RunID, req.NextAttemptAt, req.Failed, storerule.Kept(req.Error), req.Now)
		return err
	})
}

const (
	// waitsSQL says whether a run has something to wait for and nothing to
	// do. Something to wait for is an approval nobody has answered, or a
	// child run that has not ended. Something to do is a waiting step that
	// has what it waited for: the approval for its current attempt has its
	// answer, or its child has ended.
	waitsSQL = `
select (exists (select 1 from ` + ApprovalsTable + ` where run_id = $1 and status = 'pending')
     or exists (select 1 from ` + RunsTable + ` where parent_id = $1 and status not in ` + ended + `))
   and not exists (
        select 1 from ` + StepsTable + ` s
        join ` + ApprovalsTable + ` a on a.run_id = s.run_id and a.seq = s.seq and a.attempt = s.attempts
        where s.run_id = $1 and s.kind = 'tool' and s.status = 'waiting'
          and s.child_run_id is null and a.status <> 'pending')
   and not exists (
        select 1 from ` + StepsTable + ` s
        join ` + RunsTable + ` c on c.id = s.child_run_id
        where s.run_id = $1 and s.kind = 'tool' and s.status = 'waiting'
          and c.status in ` + ended + `)`

	parkSQL = `
update ` + RunsTable + `
set status = 'waiting', reason = $2, lease_owner = '', lease_expires_at = null,
    rev = rev + 1, updated_at = $3
where id = $1`
)

// Park implements agent.Store.
func (s *Store) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	var parked bool
	err := s.fenced(ctx, "park", lease, func(tx pgx.Tx, run heldRun) (err error) {
		parked, err = park(ctx, tx, lease.RunID, run, req)
		return err
	})
	if err != nil {
		return false, err
	}
	return parked, nil
}

// park sets the run waiting if it has something to wait for and no waiting
// step that already has what it waited for. The caller holds the run's row,
// and that is what makes the look and the write one step: an answer, a
// lapse, a child's end and a request to cancel each take the same row before
// they change what this reads, so each lands wholly before the look, and is
// seen, or wholly after the write, and finds a waiting run to wake.
//
// Seen, it stops the park even when something else is still pending: the
// execution read its journal before the answer landed, and parked on the
// other question it would leave an approved call unrun until that question
// was answered too.
func park(ctx context.Context, tx pgx.Tx, runID string, run heldRun, req agent.ParkRequest) (bool, error) {
	if run.cancelRequested {
		return false, nil
	}
	var waits bool
	if err := tx.QueryRow(ctx, waitsSQL, runID).Scan(&waits); err != nil {
		return false, err
	}
	if !waits {
		return false, nil
	}
	if _, err := tx.Exec(ctx, parkSQL, runID, storerule.Kept(req.Reason), req.Now); err != nil {
		return false, err
	}
	return true, nil
}

const (
	finishSQL = `
update ` + RunsTable + `
set status = $2, reason = $3, output = $4, error = $5, finished_at = $6,
    lease_owner = '', lease_expires_at = null,
    rev = rev + 1, updated_at = $6
where id = $1
returning rev`

	// A cancelled approval records when and nobody: its run ended.
	cancelApprovalsSQL = `
update ` + ApprovalsTable + `
set status = 'cancelled', decided_at = $2, rev = $3
where run_id = $1 and status = 'pending'`

	lockRowSQL = `select 1 from ` + RunsTable + ` where id = $1 for update`

	wakeParentSQL = `
update ` + RunsTable + `
set status = 'runnable', reason = '', rev = rev + 1, updated_at = $2
where id = $1 and status = 'waiting'`
)

// Finish implements agent.Store.
func (s *Store) Finish(ctx context.Context, lease agent.Lease, req agent.FinishRequest) error {
	const op = "finish"
	if !(agent.Run{Status: req.Status}).Terminal() {
		return refused(op, "status %q does not end a run", req.Status)
	}

	return s.fenced(ctx, op, lease, func(tx pgx.Tx, run heldRun) error {
		var rev int64
		err := tx.QueryRow(ctx, finishSQL,
			lease.RunID, string(req.Status), storerule.Kept(req.Reason), storerule.Kept(req.Output), storerule.Kept(req.Error), req.Now).Scan(&rev)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, cancelApprovalsSQL, lease.RunID, req.Now, rev); err != nil {
			return err
		}
		if run.parentID == "" {
			return nil
		}
		// The parent's row is taken whether or not the parent waits, and
		// after this run's own: child before parent, always. A parent that
		// is about to park holds its row while it looks at its children, so
		// it either parks before this and is woken here, or looks after
		// this has committed and sees that the child has ended. Without the
		// lock a parent that is not waiting yet would be passed by, and
		// could then park on a child it saw as still running.
		if _, err := tx.Exec(ctx, lockRowSQL, run.parentID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, wakeParentSQL, run.parentID, req.Now)
		return err
	})
}

const (
	lockCancelSQL = `select status, cancel_requested from ` + RunsTable + ` where id = $1 for update`

	cancelSQL = `
update ` + RunsTable + `
set cancel_requested = true, cancel_by = $2, cancel_reason = $3,
    ` + wake + `,
    rev = rev + 1, updated_at = $4
where id = $1`
)

// RequestCancel implements agent.Store.
func (s *Store) RequestCancel(ctx context.Context, req agent.CancelRequest) error {
	const op = "request cancel"
	if !storerule.IsUUID(req.RunID) {
		return notFound(op)
	}
	return s.inTx(ctx, op, readCommitted, func(tx pgx.Tx) error {
		var (
			status string
			marked bool
		)
		err := tx.QueryRow(ctx, lockCancelSQL, req.RunID).Scan(&status, &marked)
		if errors.Is(err, pgx.ErrNoRows) {
			return agent.ErrNotFound
		}
		if err != nil {
			return err
		}
		if (agent.Run{Status: agent.Status(status)}).Terminal() {
			return agent.ErrFinished
		}
		// The first request stands: who asked, why, and the one revision.
		if marked {
			return nil
		}
		_, err = tx.Exec(ctx, cancelSQL, req.RunID, storerule.Kept(req.By), storerule.Kept(req.Reason), req.Now)
		return err
	})
}

const (
	stepsSinceSQL = `
select ` + stepColumns + ` from ` + StepsTable + `
where run_id = $1 and rev > $2
order by seq`

	approvalsSinceSQL = `
select ` + approvalColumns + ` from ` + ApprovalsTable + `
where run_id = $1 and rev > $2
order by requested_at, ` + approvalIDColumn
)

// Changes implements agent.Store.
//
// The run, its steps and its approvals are read in one snapshot, so the
// answer is the run as it stood at one moment: every change up to the Rev it
// returns and none after. Read one statement at a time under read committed,
// steps before the run, a write landing in between would be skipped for
// good, since the reader would be handed a Rev newer than the steps it got.
func (s *Store) Changes(ctx context.Context, runID string, since int64) (agent.Changes, error) {
	const op = "changes"
	if !storerule.IsUUID(runID) {
		return agent.Changes{}, notFound(op)
	}
	var changes agent.Changes
	err := s.inTx(ctx, op, oneView, func(tx pgx.Tx) error {
		run, err := getRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, stepsSinceSQL, runID, since)
		steps, err := collect(rows, err, scanStep)
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx, approvalsSinceSQL, runID, since)
		approvals, err := collect(rows, err, scanApproval)
		if err != nil {
			return err
		}
		changes = agent.Changes{Run: run, Steps: steps, Approvals: approvals}
		return nil
	})
	if err != nil {
		return agent.Changes{}, err
	}
	return changes, nil
}

const (
	// lockDoomedSQL locks every run a purge will remove: the runs nobody
	// started that ended before the cut, and every run under them. It takes
	// the rows in the order every other writer does, children before
	// parents, so that a child that is ending as its parent is purged waits
	// for nothing the purge holds.
	lockDoomedSQL = `
with recursive doomed as (
    select id from ` + RunsTable + `
    where parent_id is null and status in ` + ended + ` and finished_at < $1
  union all
    select child.id from ` + RunsTable + ` child join doomed on child.parent_id = doomed.id
)
select id::text, parent_id is null from ` + RunsTable + `
where id in (select id from doomed)
order by depth desc, ` + runIDColumn + `
for update`

	// A tool effect's key is its step's: the run's id, a colon, the seq.
	purgeEffectsSQL = `delete from ` + EffectsTable + ` where left(key, 37) = any($1::text[])`

	// Steps, approvals and child runs go with the run they belong to.
	purgeRunsSQL = `delete from ` + RunsTable + ` where id = any($1::text[]::uuid[])`
)

// deadlockDetected is the SQLSTATE Postgres ends a transaction with when it
// and another each hold a lock the other waits for.
const deadlockDetected = "40P01"

// Purge deletes runs that ended before olderThan and were not started by
// another run, with their steps, approvals and child runs, and reports how
// many it removed.
//
// The count is of the runs nobody started. A child run goes with the run
// that started it, ended or not, and is not counted; one whose parent
// remains is left. The keys Once recorded for a removed run's steps go with
// it: those that begin with the run's id and a colon, as Invocation.Key
// does.
//
// A purge locks the runs it will remove in the order every writer takes
// rows, so it does not deadlock with a run that is ending. Should Postgres
// report a deadlock all the same, the purge is tried once more before the
// error is returned: nothing of the first try was kept, and what held the
// other lock has gone on.
func (s *Store) Purge(ctx context.Context, olderThan time.Time) (int64, error) {
	removed, err := s.purge(ctx, olderThan)
	var failure *pgconn.PgError
	if errors.As(err, &failure) && failure.Code == deadlockDetected {
		removed, err = s.purge(ctx, olderThan)
	}
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func (s *Store) purge(ctx context.Context, olderThan time.Time) (int64, error) {
	var removed int64
	err := s.inTx(ctx, "purge", readCommitted, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, lockDoomedSQL, olderThan)
		if err != nil {
			return err
		}
		defer rows.Close()
		var roots, keys []string
		for rows.Next() {
			var (
				id   string
				root bool
			)
			if err := rows.Scan(&id, &root); err != nil {
				return err
			}
			keys = append(keys, id+":")
			if root {
				roots = append(roots, id)
			}
		}
		if err := rows.Err(); err != nil || len(roots) == 0 {
			return err
		}

		if _, err := tx.Exec(ctx, purgeEffectsSQL, keys); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, purgeRunsSQL, roots)
		if err != nil {
			return err
		}
		removed = tag.RowsAffected()
		return nil
	})
	return removed, err
}
