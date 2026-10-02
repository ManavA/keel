package pg_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
	"github.com/ManavA/keel/log"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

func TestMain(m *testing.M) {
	slog.SetDefault(log.New(log.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// What the tests are written with. None of it means anything to a store.
const (
	testTTL = 30 * time.Second

	workerA = "worker-a"
	workerB = "worker-b"

	agentAlpha = "alpha"
	agentBeta  = "beta"

	toolSend  = "send"
	sendInput = `{"to":"a","n":2}`

	person = "ann"
)

var testAgents = []string{agentAlpha, agentBeta}

// testStart is where every test's clock begins: far from the wall clock, so
// a store that read a clock of its own would be found out, and a whole
// second, because a timestamp column keeps microseconds and no more.
var testStart = time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)

// waitLimit bounds a wait for something another goroutine or another session
// is about to do. It is how long a test that is going to fail takes to say
// so; no test passes by waiting it out.
const waitLimit = 60 * time.Second

// database is one test's schema on the package's shared server, holding the
// tables of this package's migration and nothing else.
type database struct {
	url    string
	schema string
}

// newDatabase makes a schema for the test, applies the migration to it the
// way a service does, and drops it when the test ends.
func newDatabase(t *testing.T) database {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()

	var suffix [6]byte
	_, err := rand.Read(suffix[:])
	require.NoError(t, err)
	d := database{url: db.URL, schema: "agent_" + hex.EncodeToString(suffix[:])}

	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL, MaxConns: 1})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+d.schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+d.schema+" cascade")
		admin.Close()
	})

	pool := d.pool(t)
	_, err = migrate.Run(ctx, pool, migrate.Options{FS: agentpg.MigrationsFS, Dir: "migrations"})
	require.NoError(t, err)
	return d
}

// pool opens a pool on the schema. Each setting is a run-time parameter given
// to every connection, as "deadlock_timeout=100ms".
//
// Every pool commits without waiting for the disk. That changes what a server
// crash would lose and nothing a session can see, and the tests commit
// thousands of times.
func (d database) pool(t *testing.T, settings ...string) *pgxpool.Pool {
	t.Helper()
	params := url.Values{}
	params.Set("search_path", d.schema)
	params.Set("synchronous_commit", "off")
	for _, setting := range settings {
		name, value, ok := strings.Cut(setting, "=")
		require.True(t, ok, "setting %q is not name=value", setting)
		params.Set(name, value)
	}
	// A space is written %20: the driver reads a + in a setting as a plus.
	query := strings.ReplaceAll(params.Encode(), "+", "%20")
	pool, err := keelpg.Open(context.Background(), keelpg.Options{URL: d.url + "&" + query, MaxConns: 24})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// kit is one test's store and its clock, with the steps tests share. It is
// the suite's own kit over again, which agenttest does not export.
type kit struct {
	t     *testing.T
	ctx   context.Context
	store agent.Store
	clock *agenttest.Clock
}

// newKit gives a test a store over a schema of its own.
func newKit(t *testing.T) (*kit, *pgxpool.Pool) {
	t.Helper()
	pool := newDatabase(t).pool(t)
	return kitOver(t, agentpg.New(pool)), pool
}

func kitOver(t *testing.T, store agent.Store) *kit {
	return &kit{t: t, ctx: t.Context(), store: store, clock: agenttest.NewClock(testStart)}
}

// tick moves the clock on a millisecond and reads it, so that each write a
// test makes has an instant of its own.
func (k *kit) tick() time.Time {
	k.clock.Advance(time.Millisecond)
	return k.clock.Now()
}

// lapse moves the clock past every lease taken so far.
func (k *kit) lapse() { k.clock.Advance(testTTL) }

func (k *kit) newRun(agentName string) agent.Run {
	now := k.tick()
	return agent.Run{
		ID:         newID(),
		Agent:      agentName,
		Status:     agent.StatusRunnable,
		Input:      "input",
		Definition: agent.Snapshot{System: "system"},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

func (k *kit) insert(run agent.Run) agent.Run {
	k.t.Helper()
	stored, created, err := k.store.CreateRun(k.ctx, run)
	require.NoError(k.t, err)
	require.True(k.t, created)
	return stored
}

// create stores a run of agentAlpha.
func (k *kit) create() agent.Run {
	k.t.Helper()
	return k.insert(k.newRun(agentAlpha))
}

// createChild stores a run of agentBeta that parent's step 2 started.
func (k *kit) createChild(parent agent.Run) agent.Run {
	k.t.Helper()
	child := k.newRun(agentBeta)
	child.ParentID, child.ParentSeq, child.Depth = parent.ID, 2, parent.Depth+1
	return k.insert(child)
}

func (k *kit) run(id string) agent.Run {
	k.t.Helper()
	run, err := k.store.GetRun(k.ctx, id)
	require.NoError(k.t, err)
	return run
}

func (k *kit) steps(runID string) []agent.Step {
	k.t.Helper()
	steps, err := k.store.Steps(k.ctx, runID)
	require.NoError(k.t, err)
	return steps
}

func (k *kit) approvals(runID string) []agent.Approval {
	k.t.Helper()
	approvals, err := k.store.ListApprovals(k.ctx, agent.ApprovalFilter{RunID: runID, Limit: 200})
	require.NoError(k.t, err)
	return approvals
}

func (k *kit) approval(id string) agent.Approval {
	k.t.Helper()
	approval, err := k.store.GetApproval(k.ctx, id)
	require.NoError(k.t, err)
	return approval
}

// claim takes runID for owner at the next instant and returns the hold.
func (k *kit) claim(owner, runID string) agent.Lease {
	k.t.Helper()
	run, err := k.store.Claim(k.ctx, agent.ClaimRequest{
		Owner: owner, Agents: testAgents, RunID: runID, Now: k.tick(), TTL: testTTL,
	})
	require.NoError(k.t, err)
	require.NotNil(k.t, run)
	return run.Lease()
}

// held stores a run of agentAlpha and claims it for workerA.
func (k *kit) held() (agent.Run, agent.Lease) {
	k.t.Helper()
	run := k.create()
	lease := k.claim(workerA, run.ID)
	return k.run(run.ID), lease
}

// reply journals one model step under lease, begun and then completed with a
// turn that makes calls, which are the steps after it.
func (k *kit) reply(lease agent.Lease, calls ...agent.Call) {
	k.t.Helper()
	seq := len(k.steps(lease.RunID)) + 1
	require.NoError(k.t, k.store.BeginModel(k.ctx, lease, seq, k.tick()))

	resp := agenttest.Say("done")
	if len(calls) > 0 {
		resp = agenttest.Use(calls...)
	}
	require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
		Seq: seq, Message: resp.Message, Stop: resp.Stop, Model: "model-a", Now: k.tick(),
	}))
}

// proposed stores a run held by workerA whose journal is a reply and the one
// call it made: step 2, proposed.
func (k *kit) proposed() (agent.Run, agent.Lease) {
	k.t.Helper()
	run, lease := k.held()
	k.reply(lease, agenttest.Call("call-1", toolSend, sendInput))
	return k.run(run.ID), lease
}

// askRequest is the question a guard's Ask puts about the proposed step at
// seq.
func (k *kit) askRequest(seq int) agent.ApprovalRequest {
	return agent.ApprovalRequest{
		ID:       newID(),
		Seq:      seq,
		From:     agent.StepProposed,
		Cause:    agent.CauseGuard,
		Action:   agent.Action{Kind: "run", Target: toolSend},
		Decision: agent.Ask,
		Rule:     "sends need a person",
		Now:      k.tick(),
	}
}

// ask puts the proposed step at seq to a person, to lapse at expires when
// that is set.
func (k *kit) ask(lease agent.Lease, seq int, expires *time.Time) agent.Approval {
	k.t.Helper()
	req := k.askRequest(seq)
	req.ExpiresAt = expires
	approval, err := k.store.RequestApproval(k.ctx, lease, req)
	require.NoError(k.t, err)
	return approval
}

func (k *kit) park(lease agent.Lease, reason string) {
	k.t.Helper()
	parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: reason, Now: k.tick()})
	require.NoError(k.t, err)
	require.True(k.t, parked)
}

// parked stores a run waiting on a person about its one call, with an
// approval that lapses at expires when that is set.
func (k *kit) parked(expires *time.Time) (agent.Run, agent.Approval) {
	k.t.Helper()
	run, lease := k.proposed()
	approval := k.ask(lease, 2, expires)
	k.park(lease, agent.ReasonApproval)
	return k.run(run.ID), approval
}

func (k *kit) finish(lease agent.Lease, status agent.Status) {
	k.t.Helper()
	require.NoError(k.t, k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: status, Now: k.tick()}))
}

// newID makes an id as a store wants one.
func newID() string { return uuid.NewString() }

// stepped is a pg.Beginner whose transactions call before ahead of every
// statement, with the statement's place in its transaction, from 1, its text
// and its arguments. A test stops one transaction there to let another go
// first, which puts two real transactions in an order a race would reach
// only by chance.
type stepped struct {
	keelpg.Beginner
	before func(st statement)
	// failing, when set, is asked before each statement whether the
	// statement is to fail, and with what: the statement is then not made.
	failing func(st statement) error
}

// statement is one statement a store made.
type statement struct {
	n    int
	sql  string
	args []any
}

func (s stepped) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	tx, err := s.Beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &steppedTx{Tx: tx, before: s.before, failing: s.failing}, nil
}

type steppedTx struct {
	pgx.Tx
	n       int
	before  func(st statement)
	failing func(st statement) error
}

// step tells the hooks of a statement, and returns the error it is to fail
// with, if any.
func (t *steppedTx) step(sql string, args []any) error {
	t.n++
	st := statement{n: t.n, sql: sql, args: args}
	if t.before != nil {
		t.before(st)
	}
	if t.failing != nil {
		return t.failing(st)
	}
	return nil
}

// Commit counts as a statement whose text is "commit", so that a transaction
// can be stopped with all its work done and none of it visible.
func (t *steppedTx) Commit(ctx context.Context) error {
	if err := t.step(commitStep, nil); err != nil {
		return err
	}
	return t.Tx.Commit(ctx)
}

func (t *steppedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := t.step(sql, args); err != nil {
		return pgconn.CommandTag{}, err
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *steppedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := t.step(sql, args); err != nil {
		return nil, err
	}
	return t.Tx.Query(ctx, sql, args...)
}

func (t *steppedTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := t.step(sql, args); err != nil {
		return failedRow{err}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

// failedRow is the row of a statement that was made to fail.
type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

// commitStep is the text a stepped transaction's hook is given for its
// commit.
const commitStep = "commit"

// beforeCommit returns a hook for stepped that stops a transaction at g when
// it is about to commit.
func beforeCommit(g *gate) func(statement) {
	return func(st statement) {
		if st.sql == commitStep {
			g.wait()
		}
	}
}

// begin opens a transaction of the test's own, for a session that is not the
// store's. It is rolled back when the test ends, if the test has not ended
// it: a test that failed with one open would otherwise wait for its
// connection for ever.
func begin(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// gate stops a goroutine at a point and tells the test it got there.
type gate struct {
	reached chan struct{}
	open    chan struct{}
	once    sync.Once
}

// newGate makes a gate that is opened when the test ends, if the test has
// not opened it: a test that fails with a transaction stopped at its gate
// would otherwise wait for that transaction's connection for ever.
func newGate(t *testing.T) *gate {
	g := &gate{reached: make(chan struct{}), open: make(chan struct{})}
	t.Cleanup(g.release)
	return g
}

// wait is called by the goroutine to be stopped. It returns once the gate
// is opened.
func (g *gate) wait() {
	close(g.reached)
	<-g.open
}

// arrived returns once the goroutine is stopped at the gate.
func (g *gate) arrived(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(waitLimit):
		t.Fatal("the transaction never reached the statement it was to stop at")
	}
}

func (g *gate) release() { g.once.Do(func() { close(g.open) }) }

// waits returns once the call that will send on answered is waiting for a
// lock another session holds, and fails at once if the call answers instead:
// it was to wait, and did not. The package's tests run one at a time on a
// server of their own, so a session that waits is one of the calling
// test's.
func waits[T any](t *testing.T, pool *pgxpool.Pool, answered <-chan T) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for {
		select {
		case got := <-answered:
			t.Fatalf("the call answered where it should have waited for the row: %+v", got)
		default:
		}
		var waiting int
		require.NoError(t, pool.QueryRow(context.Background(),
			`select count(*) from pg_stat_activity where datname = current_database() and wait_event_type = 'Lock'`,
		).Scan(&waiting))
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no session is waiting for a lock; expected the call to be blocked")
		}
		time.Sleep(time.Millisecond)
	}
}

// result waits for what a goroutine sends on ch.
func result[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitLimit):
		t.Fatal("the call never returned")
		panic("unreachable")
	}
}

// afterLock returns a hook for stepped that stops a transaction at g once it
// has made a statement that locks a row, just before whatever it does next:
// the transaction then holds the row and has not committed.
func afterLock(g *gate) func(statement) {
	locked, stopped := false, false
	return func(st statement) {
		if locked && !stopped {
			stopped = true
			g.wait()
		}
		if strings.Contains(st.sql, "for update") {
			locked = true
		}
	}
}
