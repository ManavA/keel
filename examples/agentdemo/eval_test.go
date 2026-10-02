package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	"github.com/ManavA/keel/agent/httpapi"
	agentpg "github.com/ManavA/keel/agent/pg"
	"github.com/ManavA/keel/app"
	"github.com/ManavA/keel/events"
	"github.com/ManavA/keel/llm"
	keelpg "github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/testdb"
	"github.com/ManavA/keel/policy"
	policypg "github.com/ManavA/keel/policy/pg"
)

// The evaluation and the restart tests. Where main_test.go runs the service
// the way its README does, these score what a run leaves behind and take the
// engine away from a run on purpose. Nothing here sleeps for a set time: a
// test waits for a state, with a bound far above what it needs.

// The surface and the engine cannot drift apart unnoticed.
var _ httpapi.Runs = (*agent.Engine)(nil)

// waitBound is how long a test waits for a state it has made certain to
// arrive. The machine may be busy; a pass takes a small fraction of it.
const waitBound = 90 * time.Second

// evalBudgetMicros is the most the batch may cost, in millionths of a dollar:
// ten cents. The scripted run costs a seventh of that at the listed prices.
const evalBudgetMicros = 100_000

// maxCoordinatorCalls is how many model calls the coordinator may make.
const maxCoordinatorCalls = 4

// ---------------------------------------------------------------------------
// Harness: the example's own parts, assembled the way newService assembles
// them, around an engine the test builds. newService builds one engine and
// hands it over with the real clock and store, so a test that must stop an
// engine, fault its store or move its clock assembles the engine itself, from
// the same functions (definitions, buildModel, loadPolicy, NewDocuments).

type harness struct {
	app  *app.App
	pool *pgxpool.Pool
	docs *Documents
	cfg  Config
}

// openHarness opens the app on a schema of its own, migrated as run() migrates.
func openHarness(t *testing.T) *harness {
	t.Helper()
	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "agentdemo_" + randomSuffix(t)

	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	h := &harness{cfg: schemaConfig(db.URL, schema)}
	require.NoError(t, h.cfg.Validate())
	opts := h.cfg.Options()
	opts.Logger = discardLogger()
	opts.Migrations = migrationSources()
	h.app = app.New(opts)
	require.NoError(t, h.app.Open(ctx))
	t.Cleanup(h.app.Close)
	h.pool = h.app.Pool()
	h.docs = NewDocuments(h.pool)
	return h
}

func schemaConfig(url, schema string) Config {
	return Config{
		Config: app.Config{
			Port:              8080,
			Env:               "development",
			DatabaseURL:       url + "&search_path=" + schema,
			ShutdownTimeout:   4 * time.Second,
			ReadinessCacheTTL: time.Millisecond,
		},
		MigrateOnStart: true,
		OperatorToken:  testToken,
		LeaseTTL:       5 * time.Second,
		PollInterval:   10 * time.Millisecond,
	}
}

func migrationSources() []app.MigrationSource {
	return []app.MigrationSource{
		{FS: agentpg.MigrationsFS, Dir: "migrations"},
		{FS: policypg.MigrationsFS, Dir: "migrations"},
		{FS: migrationFiles, Dir: "migrations"},
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// invocations counts what the engines made of the tools and the model, across
// every engine a test builds.
type invocations struct {
	mu     sync.Mutex
	byKey  map[string]int    // tool call key -> times its Run was entered
	tool   map[string]string // tool call key -> tool name
	runOf  map[string]string // tool call key -> run id
	models map[string]int    // model name -> calls
}

func newInvocations() *invocations {
	return &invocations{byKey: map[string]int{}, tool: map[string]string{}, runOf: map[string]string{}, models: map[string]int{}}
}

func (v *invocations) tooled(name string, in agent.Invocation) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.byKey[in.Key]++
	v.tool[in.Key] = name
	v.runOf[in.Key] = in.RunID
}

func (v *invocations) modelled(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.models[name]++
}

// of is the number of Run entries of the named tool, summed over its keys.
func (v *invocations) of(name string) (calls, keys int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, n := range v.byKey {
		if v.tool[k] == name {
			calls += n
			keys++
		}
	}
	return calls, keys
}

// engineSetup is what differs between the engines a test builds.
type engineSetup struct {
	store       agent.Store // default: a new agentpg.Store on the pool
	clock       agent.Clock // default: the system clock
	script      llm.Script  // default: the example's own
	worker      string
	inv         *invocations
	afterSave   func(agent.Invocation) // called after save_summary wrote, before it returns
	concurrency int
	approvalTTL time.Duration
}

// engine builds an engine as newService does, with the example's rules
// recorded in policy_decisions, the example's model wrappers and both agents.
func (h *harness) engine(t *testing.T, es engineSetup) *agent.Engine {
	t.Helper()
	if es.store == nil {
		es.store = agentpg.New(h.pool)
	}
	if es.script == nil {
		es.script = demoScript()
	}
	if es.inv == nil {
		es.inv = newInvocations()
	}
	script := es.script
	counted := func(req llm.Request, turn int) (llm.Reply, error) {
		es.inv.modelled(req.Model)
		return script(req, turn)
	}
	model, _, err := buildModel(llm.NewScripted(counted, llm.ScriptedOptions{}), h.cfg, discardLogger())
	require.NoError(t, err)

	rules, err := loadPolicy()
	require.NoError(t, err)
	decider, err := policy.NewDecider(rules, policy.Options{Recorder: policypg.New(h.pool), Logger: discardLogger()})
	require.NoError(t, err)

	engine, err := agent.New(agent.Options{
		Store:        es.store,
		Model:        model,
		Guard:        app.AgentGuard(decider),
		Clock:        es.clock,
		Events:       events.NewInMemoryBus(events.InMemoryBusOptions{Logger: discardLogger()}),
		Logger:       discardLogger(),
		WorkerID:     es.worker,
		LeaseTTL:     h.cfg.LeaseTTL,
		PollInterval: h.cfg.PollInterval,
		Concurrency:  es.concurrency,
		DrainTimeout: 2 * time.Second,
		ApprovalTTL:  es.approvalTTL,
	})
	require.NoError(t, err)

	for _, def := range definitions(h.docs, namesFor(h.cfg), h.cfg) {
		for i := range def.Tools {
			tool := &def.Tools[i]
			if tool.Run == nil {
				continue // a delegating tool runs a child
			}
			run, name := tool.Run, tool.Name
			tool.Run = func(ctx context.Context, in agent.Invocation) (string, error) {
				es.inv.tooled(name, in)
				out, err := run(ctx, in)
				if name == toolSaveSummary && es.afterSave != nil {
					es.afterSave(in)
				}
				return out, err
			}
		}
		require.NoError(t, engine.Register(def))
	}
	return engine
}

// work runs the engine's worker until the returned stop is called, which
// waits for it to return.
func work(t *testing.T, e *agent.Engine) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.Work(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	return stop
}

// await polls until the run satisfies done.
func await(t *testing.T, e *agent.Engine, id string, done func(agent.Run) bool) agent.Run {
	t.Helper()
	var run agent.Run
	require.Eventually(t, func() bool {
		var err error
		run, err = e.GetRun(context.Background(), id)
		return err == nil && done(run)
	}, waitBound, 10*time.Millisecond, "run %s did not get there; last seen %s (%s) %s", id, run.Status, run.Reason, run.Error)
	return run
}

func waitingForApproval(r agent.Run) bool {
	return r.Status == agent.StatusWaiting && r.Reason == agent.ReasonApproval
}

func terminal(r agent.Run) bool { return r.Terminal() }

func startBatchRun(t *testing.T, e *agent.Engine, key string) agent.Run {
	t.Helper()
	run, err := e.Start(context.Background(), agent.StartRequest{Agent: agentCoordinator, Input: batchInput, Key: key})
	require.NoError(t, err)
	return run
}

func pendingApprovals(t *testing.T, e *agent.Engine) []agent.Approval {
	t.Helper()
	approvals, err := e.ListApprovals(context.Background(), agent.ApprovalFilter{Status: agent.ApprovalPending})
	require.NoError(t, err)
	return approvals
}

// runBatch starts a batch on an engine that is working, approves the digest
// when the run asks, and returns the finished run.
func runBatch(t *testing.T, e *agent.Engine, key string) agent.Run {
	t.Helper()
	run := startBatchRun(t, e, key)
	await(t, e, run.ID, waitingForApproval)
	pending := pendingApprovals(t, e)
	require.Len(t, pending, 1)
	_, err := e.Approve(context.Background(), pending[0].ID, operatorName, "")
	require.NoError(t, err)
	return await(t, e, run.ID, terminal)
}

// ---------------------------------------------------------------------------
// The evaluation: the outcome of a run, scored as named checks over the
// journal and the tables.

type score struct {
	name string
	err  error // nil is a pass
}

func pass(name string) score              { return score{name: name} }
func fail(name, f string, a ...any) score { return score{name: name, err: fmt.Errorf(f, a...)} }

const (
	checkSummaries  = "every document has a summary"
	checkDigest     = "one digest was recorded and it names every document"
	checkDeletes    = "no delete step is anything but blocked"
	checkRule       = "the blocking rule is the one in policy.json"
	checkCost       = "the run's cost is under its budget"
	checkModelCalls = "the coordinator made no more than four model calls"
)

// scoreBatch scores run, the coordinator's, against the tables of h. It reads
// the expected values from the example's own data (the documents table,
// policy.json), not from constants that would drift from them.
func scoreBatch(ctx context.Context, h *harness, e *agent.Engine, runID string) []score {
	run, err := e.GetRun(ctx, runID)
	if err != nil {
		return []score{fail("the run can be read", "%v", err)}
	}
	steps, err := e.Timeline(ctx, runID)
	if err != nil {
		return []score{fail("the timeline can be read", "%v", err)}
	}
	children, err := e.ListRuns(ctx, agent.RunFilter{ParentID: runID, Limit: 200})
	if err != nil {
		return []score{fail("the children can be listed", "%v", err)}
	}
	allSteps := slices.Clone(steps)
	for _, child := range children {
		more, err := e.Timeline(ctx, child.ID)
		if err != nil {
			return []score{fail("the timeline can be read", "child %s: %v", child.ID, err)}
		}
		allSteps = append(allSteps, more...)
	}

	ids, err := documentIDs(ctx, h)
	if err != nil {
		return []score{fail("the documents can be read", "%v", err)}
	}
	var out []score

	// Every document has a summary.
	rows, err := h.pool.Query(ctx, `select document_id, count(*) from summaries group by document_id`)
	if err != nil {
		return []score{fail(checkSummaries, "query: %v", err)}
	}
	summarised := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return []score{fail(checkSummaries, "scan: %v", err)}
		}
		summarised[id] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return []score{fail(checkSummaries, "rows: %v", err)}
	}
	var missing []string
	for _, id := range ids {
		if summarised[id] == 0 {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		out = append(out, fail(checkSummaries, "no summary for %v", missing))
	} else {
		out = append(out, pass(checkSummaries))
	}

	// One digest, naming every document.
	var digests int
	if err := h.pool.QueryRow(ctx, `select count(*) from digests`).Scan(&digests); err != nil {
		return append(out, fail(checkDigest, "query: %v", err))
	}
	switch {
	case digests != 1:
		out = append(out, fail(checkDigest, "%d digests recorded", digests))
	default:
		var body string
		var named []string
		if err := h.pool.QueryRow(ctx, `select body, document_ids from digests`).Scan(&body, &named); err != nil {
			return append(out, fail(checkDigest, "read: %v", err))
		}
		slices.Sort(named)
		var unnamed []string
		for _, id := range ids {
			if !strings.Contains(body, id) {
				unnamed = append(unnamed, id)
			}
		}
		switch {
		case !slices.Equal(named, ids):
			out = append(out, fail(checkDigest, "it covers %v, the batch is %v", named, ids))
		case len(unnamed) > 0:
			out = append(out, fail(checkDigest, "its text does not mention %v", unnamed))
		default:
			out = append(out, pass(checkDigest))
		}
	}

	// No delete step is anything but blocked. A run that never tried to delete
	// proves nothing, so one delete step at least must be there.
	var deletes []agent.Step
	for _, s := range allSteps {
		if s.Kind == agent.StepTool && s.Name == toolDeleteDocument {
			deletes = append(deletes, s)
		}
	}
	switch bad := slices.IndexFunc(deletes, func(s agent.Step) bool { return s.Status != agent.StepBlocked }); {
	case len(deletes) == 0:
		out = append(out, fail(checkDeletes, "the run made no delete step to judge"))
	case bad >= 0:
		out = append(out, fail(checkDeletes, "delete step %d is %s", deletes[bad].Seq, deletes[bad].Status))
	default:
		out = append(out, pass(checkDeletes))
	}

	// The rule named on each one is policy.json's rule for deletes.
	want, err := blockRuleForDeletes()
	if err != nil {
		return append(out, fail(checkRule, "%v", err))
	}
	if i := slices.IndexFunc(deletes, func(s agent.Step) bool { return s.Rule != want }); len(deletes) == 0 || i >= 0 {
		got := "no step"
		if i >= 0 {
			got = fmt.Sprintf("step %d names %q", deletes[i].Seq, deletes[i].Rule)
		}
		out = append(out, fail(checkRule, "want %q: %s", want, got))
	} else {
		out = append(out, pass(checkRule))
	}

	// The cost is the coordinator's, which includes its children's; a cost of
	// zero would mean nothing was priced and the check could not fail.
	if run.Usage.CostMicros <= 0 || run.Usage.CostMicros >= evalBudgetMicros {
		out = append(out, fail(checkCost, "cost is %d micros, the budget %d", run.Usage.CostMicros, int64(evalBudgetMicros)))
	} else {
		out = append(out, pass(checkCost))
	}

	// The coordinator's model calls, from its counter and from its journal.
	modelSteps := 0
	for _, s := range steps {
		if s.Kind == agent.StepModel {
			modelSteps++
		}
	}
	if run.ModelCalls > maxCoordinatorCalls || modelSteps > maxCoordinatorCalls || modelSteps == 0 {
		out = append(out, fail(checkModelCalls, "%d calls counted, %d in the journal", run.ModelCalls, modelSteps))
	} else {
		out = append(out, pass(checkModelCalls))
	}
	return out
}

func documentIDs(ctx context.Context, h *harness) ([]string, error) {
	docs, err := h.docs.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		ids = append(ids, d.ID)
	}
	slices.Sort(ids)
	return ids, nil
}

// blockRuleForDeletes reads policy.json for the rule that blocks a delete.
func blockRuleForDeletes() (string, error) {
	p, err := loadPolicy()
	if err != nil {
		return "", err
	}
	for _, r := range p.Rules {
		if r.Effect == policy.Block && slices.Contains(r.When.Kinds, kindDelete) {
			return r.Name, nil
		}
	}
	return "", errors.New("policy.json has no rule that blocks a delete")
}

func TestBatchEval(t *testing.T) {
	h := openHarness(t)
	e := h.engine(t, engineSetup{})
	work(t, e)

	run := runBatch(t, e, "eval-1")
	require.Equal(t, agent.StatusCompleted, run.Status, run.Error)

	scores := scoreBatch(context.Background(), h, e, run.ID)
	require.Len(t, scores, 6, "every check is reported")
	for _, s := range scores {
		t.Run(s.name, func(t *testing.T) { assert.NoError(t, s.err) })
	}
}

// noDelegationScript is the coordinator that skips the reviewers: it lists
// the batch, sends a digest of what it lists, tries the deletes and stops.
func noDelegationScript() llm.Script {
	coordinator := func(req llm.Request, _ int) (llm.Reply, error) {
		turns, results := conversation(req)
		if turns == 0 {
			return llm.Reply{ToolCalls: []llm.ToolCall{call("list-1", toolListDocuments, struct{}{})}}, nil
		}
		ids := listedIDs(results[0])
		if turns > 1 {
			return llm.Reply{Text: "Sent the digest without reviews."}, nil
		}
		var body strings.Builder
		for _, id := range ids {
			body.WriteString("- " + id + "\n")
		}
		calls := []llm.ToolCall{call("send-1", toolSendDigest, digestInput{
			To: digestRecipient, Subject: "Digest", Body: body.String(), DocumentIDs: ids,
		})}
		for _, id := range ids {
			calls = append(calls, call("delete-"+id, toolDeleteDocument, documentInput{DocumentID: id}))
		}
		return llm.Reply{ToolCalls: calls}, nil
	}
	return llm.Route(map[string]llm.Script{
		scriptedCoordinator: coordinator,
		scriptedReviewer:    reviewerScript,
	})
}

// The control: an evaluation that cannot fail proves nothing. The same checks,
// run on a coordinator that never delegates, must fail the one about
// summaries, and only it.
func TestBatchEvalControlFailsWithoutDelegation(t *testing.T) {
	h := openHarness(t)
	e := h.engine(t, engineSetup{script: noDelegationScript()})
	work(t, e)

	run := runBatch(t, e, "control-1")
	require.Equal(t, agent.StatusCompleted, run.Status, run.Error)

	var failed []string
	for _, s := range scoreBatch(context.Background(), h, e, run.ID) {
		if s.err != nil {
			failed = append(failed, s.name)
			if s.name == checkSummaries {
				assert.ErrorContains(t, s.err, "no summary for")
			}
		}
	}
	assert.Equal(t, []string{checkSummaries}, failed)
}

func TestBlockedActionIsRecorded(t *testing.T) {
	h := openHarness(t)
	e := h.engine(t, engineSetup{})
	work(t, e)
	run := runBatch(t, e, "blocked-1")
	require.Equal(t, agent.StatusCompleted, run.Status, run.Error)
	ctx := context.Background()

	type decision struct{ target, effect, rule string }
	read := func(kind string) []decision {
		rows, err := h.pool.Query(ctx, `select target, effect, rule from policy_decisions where kind = $1 order by target, id`, kind)
		require.NoError(t, err)
		defer rows.Close()
		var got []decision
		for rows.Next() {
			var d decision
			require.NoError(t, rows.Scan(&d.target, &d.effect, &d.rule))
			got = append(got, d)
		}
		require.NoError(t, rows.Err())
		return got
	}

	// Every attempt to delete is on the record, blocked, with the rule.
	want := blockRuleName(t)
	assert.Equal(t, ruleBlock, want, "the test and policy.json agree on the rule's name")
	assert.Equal(t, []decision{
		{"document:doc-1", "block", want},
		{"document:doc-2", "block", want},
		{"document:doc-3", "block", want},
	}, read(kindDelete))

	// The record also holds what was asked and what was allowed, so a reader
	// can see the blocked ones against the rest.
	sends := read(kindSend)
	require.NotEmpty(t, sends)
	assert.Equal(t, decision{"email:" + digestRecipient, "ask", ruleAsk}, sends[0])

	// Nothing was deleted: the table still has its rows.
	var documents int
	require.NoError(t, h.pool.QueryRow(ctx, `select count(*) from documents`).Scan(&documents))
	assert.Equal(t, 3, documents)
}

func blockRuleName(t *testing.T) string {
	t.Helper()
	name, err := blockRuleForDeletes()
	require.NoError(t, err)
	return name
}

// ---------------------------------------------------------------------------
// Kill and resume.

// logRecorder is a logger whose output a test can read.
type logRecorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logRecorder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logRecorder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logRecorder) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, nil))
}

// TestKillAndResume is the README's steps 3 and 4 without a second process: an
// engine is stopped by killing its store while a save_summary is in flight, and
// engines built after it, over fresh stores on the same database, carry the run
// on. A real kill -9 of the binary leaves the same thing behind: a lease nobody
// renews, a step written as started, and the tool's own commit in the database.
func TestKillAndResume(t *testing.T) {
	h := openHarness(t)
	ctx := context.Background()
	inv := newInvocations()
	clock := agenttest.NewClock(time.Now())

	// Engine A, over a store that can be killed. The first save_summary writes
	// its row and then the store dies, before the engine can record that the
	// call finished: the row is committed and the journal does not know.
	fs := agenttest.NewFaultStore(agentpg.New(h.pool))
	var (
		killOnce    sync.Once
		interrupted agent.Invocation
		killed      = make(chan struct{})
	)
	a := h.engine(t, engineSetup{
		store: fs, clock: clock, worker: "engine-a", inv: inv,
		// One run at a time, so exactly one tool call is in flight at the kill.
		concurrency: 1,
		afterSave: func(in agent.Invocation) {
			killOnce.Do(func() {
				interrupted = in
				fs.Kill()
				close(killed)
			})
		},
	})
	run := startBatchRun(t, a, "kill-1")
	stopA := work(t, a)
	select {
	case <-killed:
	case <-time.After(waitBound):
		require.FailNow(t, "save_summary was never reached")
	}
	stopA()
	require.NotEmpty(t, interrupted.Key)
	require.NotEqual(t, run.ID, interrupted.RunID, "a reviewer's run was interrupted, not the coordinator's")

	var rowsAtKill int
	require.NoError(t, h.pool.QueryRow(ctx, `select count(*) from summaries where run_id = $1`, interrupted.RunID).Scan(&rowsAtKill))
	require.Equal(t, 1, rowsAtKill, "the interrupted call had written its row")

	// Engine B, over a fresh store, on the same clock. The dead engine's lease
	// has not lapsed, so B may not take the interrupted run.
	b := h.engine(t, engineSetup{clock: clock, worker: "engine-b", inv: inv})
	_, err := b.Execute(ctx, interrupted.RunID)
	require.ErrorIs(t, err, agent.ErrNotClaimable, "a live lease holds the run")

	// What the startup log says about it.
	logs := &logRecorder{}
	(&service{engine: b}).logUnfinished(ctx, logs.logger())
	assert.Contains(t, logs.String(), "resuming run")
	assert.Contains(t, logs.String(), interrupted.RunID)

	// The clock moves past the lease, and B carries the batch to the approval.
	clock.Advance(h.cfg.LeaseTTL + time.Second)
	stopB := work(t, b)
	await(t, b, run.ID, waitingForApproval)
	pending := pendingApprovals(t, b)
	require.Len(t, pending, 1)
	approval := pending[0]
	assert.Equal(t, run.ID, approval.RunID)
	assert.Equal(t, toolSendDigest, approval.Tool)
	assert.Equal(t, ruleAsk, approval.Rule)

	// One summary per document, and the interrupted call was made twice, with
	// one key; every other call once.
	assertOneSummaryPerDocument(t, h)
	calls, keys := inv.of(toolSaveSummary)
	assert.Equal(t, 4, calls, "three calls and one repeat")
	assert.Equal(t, 3, keys)
	inv.mu.Lock()
	for key, n := range inv.byKey {
		if key == interrupted.Key {
			assert.Equal(t, 2, n, "the interrupted %s call is made again", inv.tool[key])
			assert.Equal(t, toolSaveSummary, inv.tool[key])
			continue
		}
		assert.Equal(t, 1, n, "%s call %s was run again though it had completed", inv.tool[key], key)
	}
	inv.mu.Unlock()

	// A restart while the run waits. Engine B stops; engine C is built, as a
	// restarted process builds it.
	stopB()
	c := h.engine(t, engineSetup{clock: clock, worker: "engine-c", inv: inv})
	logs = &logRecorder{}
	(&service{engine: c}).logUnfinished(ctx, logs.logger())
	assert.Contains(t, logs.String(), "run is still waiting")
	assert.NotContains(t, logs.String(), "resuming run")

	waiting, err := c.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.True(t, waitingForApproval(waiting), "still waiting: %s (%s)", waiting.Status, waiting.Reason)
	stillPending := pendingApprovals(t, c)
	require.Len(t, stillPending, 1)
	assert.Equal(t, approval.ID, stillPending[0].ID, "the same approval")

	// A pass of the worker finds nothing to take: a waiting run is nobody's.
	report, err := c.Tick(ctx)
	require.NoError(t, err)
	assert.Zero(t, report.Claimed)

	sendCalls, _ := inv.of(toolSendDigest)
	assert.Zero(t, sendCalls, "nothing was sent while the run waited, across three engines")
	var digests int
	require.NoError(t, h.pool.QueryRow(ctx, `select count(*) from digests`).Scan(&digests))
	assert.Zero(t, digests)

	// Approved on the third engine, the run completes.
	work(t, c)
	decided, err := c.Approve(ctx, approval.ID, operatorName, "")
	require.NoError(t, err)
	assert.Equal(t, agent.ApprovalApproved, decided.Status)
	final := await(t, c, run.ID, terminal)
	require.Equal(t, agent.StatusCompleted, final.Status, final.Error)
	assert.Contains(t, final.Output, "The digest was sent.")

	assertOneSummaryPerDocument(t, h)
	require.NoError(t, h.pool.QueryRow(ctx, `select count(*) from digests`).Scan(&digests))
	assert.Equal(t, 1, digests)
	sendCalls, sendKeys := inv.of(toolSendDigest)
	assert.Equal(t, 1, sendCalls)
	assert.Equal(t, 1, sendKeys)
	deleteCalls, _ := inv.of(toolDeleteDocument)
	assert.Zero(t, deleteCalls, "a blocked delete never reaches its tool")

	// The model was asked once per turn however many engines carried the run:
	// the coordinator four times, each reviewer three.
	inv.mu.Lock()
	assert.Equal(t, 4, inv.models[scriptedCoordinator])
	assert.Equal(t, 9, inv.models[scriptedReviewer])
	inv.mu.Unlock()

	// And what the evaluation scores holds after all that.
	for _, s := range scoreBatch(ctx, h, c, run.ID) {
		assert.NoError(t, s.err, "check %q", s.name)
	}
}

func assertOneSummaryPerDocument(t *testing.T, h *harness) {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `select d.id, count(s.id) from documents d left join summaries s on s.document_id = d.id group by d.id order by d.id`)
	require.NoError(t, err)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var count int
		require.NoError(t, rows.Scan(&id, &count))
		assert.Equal(t, 1, count, "summaries for %s", id)
		n++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 3, n)
}

// ---------------------------------------------------------------------------
// A restart of the service itself, over HTTP, while a run waits.

// serviceOn builds the service on an existing schema, as run() does, with its
// worker running, and returns it with a function that stops both. It is the
// whole of what a process is, short of the operating system.
func serviceOn(t *testing.T, url, schema string, log *slog.Logger) (*service, func()) {
	t.Helper()
	cfg := schemaConfig(url, schema)
	require.NoError(t, cfg.Validate())
	svc, err := newService(context.Background(), cfg, log)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.engine.Work(ctx)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			svc.app.Close()
		})
	}
	t.Cleanup(stop)
	return svc, stop
}

// serve answers a request to the service's router, with the operator token.
func serve(t *testing.T, svc *service, method, path string, out any) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	svc.app.Router().ServeHTTP(rec, req)
	if out != nil && rec.Code < 300 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out), "body: %s", rec.Body.String())
	}
	return rec.Code
}

func TestRestartWhileWaitingThenApprove(t *testing.T) {
	db := testdb.Shared(t)
	ctx := context.Background()
	schema := "agentdemo_" + randomSuffix(t)
	admin, err := keelpg.Open(ctx, keelpg.Options{URL: db.URL})
	require.NoError(t, err)
	_, err = admin.Exec(ctx, "create schema "+schema)
	require.NoError(t, err)
	admin.Close()

	// The first process starts a batch and waits at the approval.
	first, stopFirst := serviceOn(t, db.URL, schema, discardLogger())
	var run agent.Run
	require.Equal(t, http.StatusAccepted, serve(t, first, http.MethodPost, "/api/batches", &run))
	await(t, first.engine, run.ID, waitingForApproval)
	var pending struct {
		Approvals []agent.Approval `json:"approvals"`
	}
	require.Equal(t, http.StatusOK, serve(t, first, http.MethodGet, "/agent/approvals?status=pending", &pending))
	require.Len(t, pending.Approvals, 1)
	approval := pending.Approvals[0]
	stopFirst()

	// The second finds the run waiting, says so, and leaves it waiting.
	logs := &logRecorder{}
	second, _ := serviceOn(t, db.URL, schema, logs.logger())
	second.logUnfinished(ctx, logs.logger())
	assert.Contains(t, logs.String(), "run is still waiting")
	assert.Contains(t, logs.String(), run.ID)

	// Nobody takes a waiting run: give the worker many passes at it.
	require.Never(t, func() bool {
		r, err := second.engine.GetRun(ctx, run.ID)
		return err != nil || !waitingForApproval(r)
	}, time.Second, 10*time.Millisecond)

	var again struct {
		Approvals []agent.Approval `json:"approvals"`
	}
	require.Equal(t, http.StatusOK, serve(t, second, http.MethodGet, "/agent/approvals?status=pending", &again))
	require.Len(t, again.Approvals, 1)
	assert.Equal(t, approval.ID, again.Approvals[0].ID)

	// Approving it there completes the run.
	var decided agent.Approval
	require.Equal(t, http.StatusOK, serve(t, second, http.MethodPost, "/agent/approvals/"+approval.ID+"/approve", &decided))
	assert.Equal(t, agent.ApprovalApproved, decided.Status)
	final := await(t, second.engine, run.ID, terminal)
	require.Equal(t, agent.StatusCompleted, final.Status, final.Error)

	var digests, summaries int
	require.NoError(t, second.app.Pool().QueryRow(ctx, `select count(*) from digests`).Scan(&digests))
	require.NoError(t, second.app.Pool().QueryRow(ctx, `select count(*) from summaries`).Scan(&summaries))
	assert.Equal(t, 1, digests)
	assert.Equal(t, 3, summaries)
}

// ---------------------------------------------------------------------------
// What the engine and the HTTP surface do with a caller's mistake.

// TestCallerMistakesAreNotServerErrors drives Approve, Decline and Cancel
// through agent/httpapi, mounted as the example mounts it, with each condition
// a caller can cause, and holds each to the status that tells the caller what
// they did.
func TestCallerMistakesAreNotServerErrors(t *testing.T) {
	h := openHarness(t)
	clock := agenttest.NewClock(time.Now())
	const approvalTTL = time.Hour
	e := h.engine(t, engineSetup{clock: clock, approvalTTL: approvalTTL})
	work(t, e)

	api, err := httpapi.New(httpapi.Options{Runs: e, Actor: operatorActor, Logger: discardLogger()})
	require.NoError(t, err)
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	post := func(path, body string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	const unknown = "6f0f2f56-4a4e-4b53-9d0c-3a8c1c9d2b11"

	// Names the store has no record of, in any spelling.
	for _, id := range []string{unknown, "nope", "6F0F2F56-4A4E-4B53-9D0C-3A8C1C9D2B11", "%00"} {
		assert.Equal(t, http.StatusNotFound, post("/approvals/"+id+"/approve", ""), "approve %q", id)
		assert.Equal(t, http.StatusNotFound, post("/approvals/"+id+"/decline", ""), "decline %q", id)
		assert.Equal(t, http.StatusNotFound, post("/runs/"+id+"/cancel", ""), "cancel %q", id)
	}

	// A run is approved, and then the approval is answered again.
	run := startBatchRun(t, e, "mistakes-1")
	await(t, e, run.ID, waitingForApproval)
	first := pendingApprovals(t, e)
	require.Len(t, first, 1)

	assert.Equal(t, http.StatusBadRequest, post("/approvals/"+first[0].ID+"/approve", `{"reason":"a\u0000b"}`), "a NUL in the reason")
	assert.Equal(t, http.StatusBadRequest, post("/approvals/"+first[0].ID+"/approve", `{"reason":`), "a body that is not JSON")
	assert.Equal(t, http.StatusBadRequest, post("/approvals/"+first[0].ID+"/approve", `{"reason":"x","extra":1}`), "an unknown field")
	assert.Equal(t, http.StatusBadRequest, post("/approvals/"+first[0].ID+"/approve", "{\"reason\":\"\xff\"}"), "bytes that are not UTF-8")
	assert.Equal(t, http.StatusOK, post("/approvals/"+first[0].ID+"/approve", `{"reason":"looks right"}`))
	assert.Equal(t, http.StatusConflict, post("/approvals/"+first[0].ID+"/approve", ""), "approve twice")
	assert.Equal(t, http.StatusConflict, post("/approvals/"+first[0].ID+"/decline", ""), "decline what was approved")
	await(t, e, run.ID, terminal)
	assert.Equal(t, http.StatusConflict, post("/runs/"+run.ID+"/cancel", ""), "cancel a run that ended")

	// A run cancelled while it waits: its approval goes with it, and answering
	// the approval afterwards is a conflict, not a fault.
	second := startBatchRun(t, e, "mistakes-2")
	await(t, e, second.ID, waitingForApproval)
	waitingApproval := pendingApprovals(t, e)
	require.Len(t, waitingApproval, 1)
	assert.Equal(t, http.StatusAccepted, post("/runs/"+second.ID+"/cancel", `{"reason":"changed my mind"}`))
	assert.Equal(t, http.StatusAccepted, post("/runs/"+second.ID+"/cancel", ""), "cancel twice")
	cancelled := await(t, e, second.ID, terminal)
	assert.Equal(t, agent.StatusCancelled, cancelled.Status)
	assert.Equal(t, http.StatusConflict, post("/approvals/"+waitingApproval[0].ID+"/approve", ""), "approve for a cancelled run")
	assert.Equal(t, http.StatusConflict, post("/approvals/"+waitingApproval[0].ID+"/decline", ""), "decline for a cancelled run")
	assert.Equal(t, http.StatusConflict, post("/runs/"+second.ID+"/cancel", ""), "cancel a cancelled run")

	// An approval nobody answered in time has lapsed. Answering it late is a
	// conflict. The clock moves; the worker lapses it on its next pass.
	third := startBatchRun(t, e, "mistakes-3")
	await(t, e, third.ID, waitingForApproval)
	late := pendingApprovals(t, e)
	require.Len(t, late, 1)
	clock.Advance(approvalTTL + time.Minute)
	require.Eventually(t, func() bool {
		a, err := e.ListApprovals(context.Background(), agent.ApprovalFilter{RunID: third.ID})
		return err == nil && len(a) == 1 && a[0].Status == agent.ApprovalExpired
	}, waitBound, 10*time.Millisecond, "the approval lapsed")
	assert.Equal(t, http.StatusConflict, post("/approvals/"+late[0].ID+"/approve", ""), "approve a lapsed approval")
	assert.Equal(t, http.StatusConflict, post("/approvals/"+late[0].ID+"/decline", ""), "decline a lapsed approval")

	// The same calls on the engine, which the surface sits on: each condition
	// has an error to match, and what the surface cannot let through (a name
	// that cannot be recorded) is an error here too, though not a typed one.
	ctx := context.Background()
	_, err = e.Approve(ctx, unknown, operatorName, "")
	assert.ErrorIs(t, err, agent.ErrNotFound)
	_, err = e.Approve(ctx, first[0].ID, operatorName, "")
	assert.ErrorIs(t, err, agent.ErrAlreadyDecided)
	assert.ErrorIs(t, e.Cancel(ctx, run.ID, operatorName, ""), agent.ErrFinished)
	assert.ErrorIs(t, e.Cancel(ctx, unknown, operatorName, ""), agent.ErrNotFound)
	_, err = e.Approve(ctx, late[0].ID, "  ", "")
	require.Error(t, err)
	for _, typed := range []error{agent.ErrNotFound, agent.ErrAlreadyDecided, agent.ErrFinished} {
		assert.NotErrorIs(t, err, typed, "an unrecordable name is the caller's mistake and is not mistaken for another")
	}
}
