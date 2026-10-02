package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// The executor over this store, which is where a service runs it. An engine
// here is a whole process: it claims, keeps its lease, calls the model, the
// guard and the tools, and writes every step through real transactions.
//
// No test here waits for a timer. A keeper's ticker runs on the wall clock,
// so the heartbeat interval is a day and the lease two: no heartbeat is ever
// made, and a count of store calls is the same on every machine. Leases,
// back-offs and journal times are on the tests' own clock, which moves only
// when a test moves it.
const (
	batchTTL       = 48 * time.Hour
	batchHeartbeat = 24 * time.Hour

	// ledgerTable is where the batch's tools make their effect on the world:
	// a row for each thing done, in the database the journal is in.
	ledgerTable = "batch_ledger"
)

// toolLog records every invocation of the tools made from it.
type toolLog struct {
	mu   sync.Mutex
	seen []agent.Invocation
}

func (l *toolLog) note(in agent.Invocation) {
	in.Call.Input = slices.Clone(in.Call.Input)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, in)
}

// byKey returns every invocation recorded, in order, under its idempotency
// key.
func (l *toolLog) byKey() map[string][]agent.Invocation {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string][]agent.Invocation{}
	for _, in := range l.seen {
		out[in.Key] = append(out[in.Key], in)
	}
	return out
}

// plain is a tool that records its invocation and answers "<name> ok". It
// does nothing to the world, so made twice it is as if made once.
func (l *toolLog) plain(name string) agent.Tool {
	return agent.Tool{Name: name, Run: func(_ context.Context, in agent.Invocation) (string, error) {
		l.note(in)
		return name + " ok", nil
	}}
}

// effect is a tool whose effect is a row in this database, made exactly once
// as 6.8 of the design says a tool makes it: Once and the row in one
// transaction, and the row skipped when the key was already recorded.
func (l *toolLog) effect(name string, pool *pgxpool.Pool) agent.Tool {
	return agent.Tool{Name: name, Run: func(ctx context.Context, in agent.Invocation) (string, error) {
		l.note(in)
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			first, err := agentpg.Once(ctx, tx, in.Key)
			if err != nil || !first {
				return err
			}
			_, err = tx.Exec(ctx, "insert into "+ledgerTable+" (key, tool) values ($1, $2)", in.Key, name)
			return err
		})
		if err != nil {
			return "", err
		}
		return name + " ok", nil
	}}
}

// batchWorld is one database and one clock, which every process of a test
// shares, and the raw store the test reads what is true from.
type batchWorld struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *agentpg.Store
	clock *agenttest.Clock
}

func newBatchWorld(t *testing.T) *batchWorld {
	t.Helper()
	pool := newDatabase(t).pool(t)
	_, err := pool.Exec(context.Background(),
		"create table "+ledgerTable+" (key text not null, tool text not null)")
	require.NoError(t, err)
	return &batchWorld{t: t, pool: pool, store: agentpg.New(pool), clock: agenttest.NewClock(testStart)}
}

// empty removes every run, with its steps, approvals and children, and every
// effect, so that the next batch starts from nothing.
func (w *batchWorld) empty() {
	w.t.Helper()
	_, err := w.pool.Exec(context.Background(),
		"delete from "+agentpg.RunsTable+"; delete from "+agentpg.EffectsTable+"; delete from "+ledgerTable)
	require.NoError(w.t, err)
}

// batchProcess is one process: an engine over a store of its own to be
// killed, which is the world's underneath.
type batchProcess struct {
	engine *agent.Engine
	faults *agenttest.FaultStore
}

// process starts a process named name. Its tools record in log, and its
// model is model, which the processes of one batch share so that its
// requests are the run's whatever process sent them.
func (w *batchWorld) process(name string, log *toolLog, model agent.Model, over func(agent.Store) agent.Store) *batchProcess {
	w.t.Helper()
	p := &batchProcess{faults: agenttest.NewFaultStore(w.store)}
	var store agent.Store = p.faults
	if over != nil {
		store = over(store)
	}
	engine, err := agent.New(agent.Options{
		Model: model,
		Store: store,
		Guard: agenttest.GuardFunc(func(_ context.Context, a agent.Action) (agent.Decision, error) {
			if a.Target == "send" {
				return agent.Decision{Effect: agent.Ask, Rule: "ask-first"}, nil
			}
			return agent.Decision{Effect: agent.Allow, Rule: "default-allow"}, nil
		}),
		Clock:             w.clock,
		WorkerID:          name,
		LeaseTTL:          batchTTL,
		HeartbeatInterval: batchHeartbeat,
	})
	require.NoError(w.t, err)

	charge := log.plain("charge")
	charge.AtMostOnce = true
	for _, def := range []agent.Definition{
		{Name: "lead", System: "run the batch", Tools: []agent.Tool{
			log.plain("lookup"), charge, log.effect("send", w.pool), {Name: "review", Delegate: "reviewer"},
		}},
		{Name: "reviewer", System: "summarise the document", Tools: []agent.Tool{log.effect("save", w.pool)}},
	} {
		require.NoError(w.t, engine.Register(def))
	}
	p.engine = engine
	return p
}

// batchScript is the model's part: the lead makes two calls, then two more,
// one of them a delegation, and then answers with every result it was last
// given; the reviewer saves and answers likewise.
func batchScript() agenttest.Script {
	told := func(req agent.Request) string {
		var said []string
		if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == agent.RoleTool {
			for _, result := range req.Messages[n-1].Results {
				said = append(said, result.Content)
			}
		}
		return strings.Join(said, "; ")
	}
	spent := func(resp agent.Response, in, out, cost int64) agent.Response {
		resp.Usage = agent.Usage{InputTokens: in, OutputTokens: out, CostMicros: cost}
		return resp
	}
	return agenttest.ByAgent(map[string]agenttest.Script{
		"lead": func(req agent.Request, turn int) (agent.Response, error) {
			switch turn {
			case 0:
				return spent(agenttest.Use(
					agenttest.Call("call-1", "lookup", `{"order":7}`),
					agenttest.Call("call-2", "charge", `{"amount":1250}`),
				), 100, 20, 300), nil
			case 1:
				return spent(agenttest.Use(
					agenttest.Call("call-3", "send", `{"to":"ops"}`),
					agenttest.Call("call-4", "review", `{"doc":1}`),
				), 150, 20, 400), nil
			}
			return spent(agenttest.Say("done: "+told(req)), 200, 10, 450), nil
		},
		"reviewer": func(req agent.Request, turn int) (agent.Response, error) {
			if turn == 0 {
				return spent(agenttest.Use(agenttest.Call("call-1", "save", `{"summary":"short"}`)), 10, 5, 7), nil
			}
			return spent(agenttest.Say("saved: "+told(req)), 12, 3, 6), nil
		},
	})
}

func (w *batchWorld) run(id string) agent.Run {
	w.t.Helper()
	run, err := w.store.GetRun(context.Background(), id)
	require.NoError(w.t, err)
	return run
}

func (w *batchWorld) changes(id string) agent.Changes {
	w.t.Helper()
	changes, err := w.store.Changes(context.Background(), id, 0)
	require.NoError(w.t, err)
	return changes
}

// asked returns the approvals of run id by the seq of the step each is about.
// Two asked at one instant are listed in the order of their ids, which says
// nothing of their steps.
func (w *batchWorld) asked(id string) map[int]agent.Approval {
	w.t.Helper()
	out := map[int]agent.Approval{}
	for _, approval := range w.changes(id).Approvals {
		out[approval.Seq] = approval
	}
	return out
}

// lead returns the batch's lead run, and false when it was never created.
func (w *batchWorld) lead() (agent.Run, bool) {
	w.t.Helper()
	leads, err := w.store.ListRuns(context.Background(), agent.RunFilter{Agent: "lead"})
	require.NoError(w.t, err)
	require.LessOrEqual(w.t, len(leads), 1, "the batch was started once per key")
	if len(leads) == 0 {
		return agent.Run{}, false
	}
	return leads[0], true
}

// children returns the children of run id in the order of the steps that
// started them.
func (w *batchWorld) children(id string) []agent.Run {
	w.t.Helper()
	children, err := w.store.ListRuns(context.Background(), agent.RunFilter{ParentID: id, Limit: 200})
	require.NoError(w.t, err)
	slices.SortFunc(children, func(a, b agent.Run) int { return a.ParentSeq - b.ParentSeq })
	return children
}

func (w *batchWorld) claimable(run agent.Run) bool {
	now := w.clock.Now()
	return run.Status == agent.StatusRunnable &&
		(run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now)) &&
		(run.NextAttemptAt == nil || !run.NextAttemptAt.After(now))
}

// round is one round of everyone but the engine's own loop: the batch is
// started under its key, the lead is executed if it can be claimed, and
// otherwise each of its children that can be, and then whatever is asked is
// approved. It reports whether the lead has ended, whether anything could be
// done, and the first error a call of the engine returned, at which the
// round stops.
func (w *batchWorld) round(p *batchProcess) (ended, acted bool, err error) {
	ctx := w.t.Context()
	lead, err := p.engine.Start(ctx, agent.StartRequest{Agent: "lead", Input: "run the batch", Key: "batch-1"})
	if err != nil {
		return false, true, err
	}
	lead = w.run(lead.ID)
	switch {
	case lead.Terminal():
		return true, false, nil
	case w.claimable(lead):
		_, err := p.engine.Execute(ctx, lead.ID)
		return false, true, err
	}
	for _, child := range w.children(lead.ID) {
		if !w.claimable(child) {
			continue
		}
		acted = true
		if _, err := p.engine.Execute(ctx, child.ID); err != nil {
			return false, true, err
		}
	}
	for _, approval := range w.changes(lead.ID).Approvals {
		if approval.Status != agent.ApprovalPending {
			continue
		}
		acted = true
		if _, err := p.engine.Approve(ctx, approval.ID, "ops@example.test", "go on"); err != nil {
			return false, true, err
		}
	}
	return false, acted, nil
}

// drive plays rounds until the lead ends or a call of the engine fails.
func (w *batchWorld) drive(p *batchProcess) error {
	for range 40 {
		ended, acted, err := w.round(p)
		switch {
		case err != nil:
			return err
		case ended:
			return nil
		case !acted:
			return errors.New("the batch is stuck: its lead has not ended, and nothing can be executed or approved")
		}
	}
	return errors.New("the batch did not end in forty rounds")
}

// batchOutcome is what a batch came to: what a resumed batch must match.
type batchOutcome struct {
	status     agent.Status
	output     string
	usage      agent.Usage
	modelCalls int
	// journals is each run's journal, a line a step, the lead's under "lead"
	// and a child's under the step that started it.
	journals map[string][]string
	// keys is the idempotency key of every tool call that ran, and ledger
	// the key and tool of every effect made, each with its run's place in
	// the batch for the run's id.
	keys, ledger []string
}

func journalLines(steps []agent.Step) []string {
	out := make([]string, len(steps))
	for i, st := range steps {
		what := "model"
		if st.Kind == agent.StepTool {
			what = st.Name
		}
		out[i] = fmt.Sprintf("%d %s %s", st.Seq, what, st.Status)
	}
	return out
}

func (w *batchWorld) outcome(log *toolLog) batchOutcome {
	w.t.Helper()
	lead, ok := w.lead()
	require.True(w.t, ok)
	out := batchOutcome{
		status: lead.Status, output: lead.Output, usage: lead.Usage, modelCalls: lead.ModelCalls,
		journals: map[string][]string{"lead": journalLines(w.changes(lead.ID).Steps)},
	}
	// A run's id is new in every batch, so a key is written with its run's
	// place in the batch in the id's stead.
	replace := []string{lead.ID, "lead"}
	for _, child := range w.children(lead.ID) {
		name := fmt.Sprintf("the child of step %d", child.ParentSeq)
		require.NotContains(w.t, out.journals, name, "one child for each delegating step")
		out.journals[name] = journalLines(w.changes(child.ID).Steps)
		replace = append(replace, child.ID, name)
	}
	names := strings.NewReplacer(replace...)
	for key := range log.byKey() {
		out.keys = append(out.keys, names.Replace(key))
	}
	slices.Sort(out.keys)

	rows, err := w.pool.Query(context.Background(), "select key, tool from "+ledgerTable)
	require.NoError(w.t, err)
	defer rows.Close()
	for rows.Next() {
		var key, tool string
		require.NoError(w.t, rows.Scan(&key, &tool))
		out.ledger = append(out.ledger, names.Replace(key)+" "+tool)
	}
	require.NoError(w.t, rows.Err())
	slices.Sort(out.ledger)
	return out
}

// batchMoment is where every tool call stood at one moment: its step's
// status and attempts by its key, and how many times its tool had been
// invoked. A call the journal does not hold yet has no status.
type batchMoment struct {
	status   map[string]agent.StepStatus
	attempts map[string]int
	ran      map[string]int
	// children is how many child runs the lead had.
	children int
}

func (w *batchWorld) moment(log *toolLog) batchMoment {
	w.t.Helper()
	at := batchMoment{status: map[string]agent.StepStatus{}, attempts: map[string]int{}, ran: map[string]int{}}
	for key, invocations := range log.byKey() {
		at.ran[key] = len(invocations)
	}
	lead, ok := w.lead()
	if !ok {
		return at
	}
	children := w.children(lead.ID)
	at.children = len(children)
	for _, run := range append([]agent.Run{lead}, children...) {
		for _, st := range w.changes(run.ID).Steps {
			if st.Kind == agent.StepTool {
				at.status[st.Key] = st.Status
				at.attempts[st.Key] = st.Attempts
			}
		}
	}
	return at
}

// report lists what is wrong with a batch that has ended: nothing, when it
// kept every promise of 6.8 of the design. interruptions is how many times an
// execution of it was cut short; killed is where each call stood when the
// first process's store died, and nil for a batch whose process lived.
func (w *batchWorld) report(log *toolLog, model *agenttest.Model, want batchOutcome, killed *batchMoment, interruptions int) []string {
	w.t.Helper()
	var wrong []string
	say := func(format string, args ...any) { wrong = append(wrong, fmt.Sprintf(format, args...)) }

	// The same end as the batch nothing interrupted: status, output, usage,
	// journals, the calls that ran and the effects they made. An effect made
	// under Once is in the ledger once however often its tool ran.
	got := w.outcome(log)
	if !assert.ObjectsAreEqual(want, got) {
		say("the batch ended as %+v, and uninterrupted it ends as %+v", got, want)
	}

	// The journal has every step once, from 1 with no gaps.
	lead, _ := w.lead()
	end := w.moment(log)
	interrupted := map[int]int{}
	for _, approval := range w.changes(lead.ID).Approvals {
		if approval.Cause == agent.CauseInterrupted {
			if approval.Status != agent.ApprovalApproved {
				say("the question about interrupted step %d was left %s", approval.Seq, approval.Status)
			}
			interrupted[approval.Seq]++
		}
	}
	for _, run := range append([]agent.Run{lead}, w.children(lead.ID)...) {
		for i, st := range w.changes(run.ID).Steps {
			if st.Seq != i+1 {
				say("run %s has step %d at position %d", run.ID, st.Seq, i+1)
			}
		}
	}

	// No tool ran more often than its kind allows.
	for key, invocations := range log.byKey() {
		tool, ran := invocations[0].Call.Name, len(invocations)
		for i, in := range invocations {
			if i > 0 && in.Attempt <= invocations[i-1].Attempt {
				say("%s (%s) ran as attempt %d after attempt %d", tool, key, in.Attempt, invocations[i-1].Attempt)
			}
			if !assert.ObjectsAreEqual(invocations[0].Call, in.Call) {
				say("%s (%s) was given other arguments on attempt %d", tool, key, in.Attempt)
			}
		}
		if last := invocations[ran-1].Attempt; last != end.attempts[key] {
			say("%s (%s) last ran as attempt %d, and its step counts %d", tool, key, last, end.attempts[key])
		}
		// A tool runs at most once more for each interruption.
		if ran > 1+interruptions {
			say("%s (%s) ran %d times across %d interruptions", tool, key, ran, interruptions)
		}
		// An at-most-once tool never runs again without a person's yes in
		// between.
		if tool == "charge" && ran > 1+interrupted[invocations[0].Seq] {
			say("charge, which is at-most-once, ran %d times with %d approvals of an interrupted call",
				ran, interrupted[invocations[0].Seq])
		}
		if killed == nil {
			continue
		}
		before := killed.ran[key]
		switch status := killed.status[key]; {
		case status.Done() && ran != before:
			say("%s (%s) was %s when the store died, having run %d times, and has now run %d times: a completed step was executed again",
				tool, key, status, before, ran)
		case status == agent.StepStarted && ran != before+1:
			say("%s (%s) was started when the store died, having run %d times, and has now run %d times: an interrupted call is made again once",
				tool, key, before, ran)
		case !status.Done() && status != agent.StepStarted && (before != 0 || ran != 1):
			say("%s (%s) had not started when the store died (%q), had run %d times, and has now run %d times",
				tool, key, status, before, ran)
		}
	}
	if killed != nil {
		// A person is asked about the at-most-once call exactly when the
		// crash left it started.
		status := killed.status[agent.StepKey(lead.ID, 3)]
		if asked := interrupted[3]; (status == agent.StepStarted) != (asked == 1) {
			say("charge was %q when the store died, and a person was asked about it %d times", status, asked)
		}
	}

	// What the model was sent only ever grew: every request of a run starts
	// with every earlier request of that run, under the same prompt and
	// tools.
	sent := map[string][]agent.Request{}
	for _, req := range model.Requests() {
		for _, earlier := range sent[req.RunID] {
			if req.System != earlier.System || !assert.ObjectsAreEqual(earlier.Tools, req.Tools) {
				say("run %s was sent a request under another prompt or other tools", req.RunID)
			}
			if len(req.Messages) < len(earlier.Messages) ||
				!assert.ObjectsAreEqual(earlier.Messages, req.Messages[:len(earlier.Messages)]) {
				say("run %s was sent a conversation that does not start with one it was sent before", req.RunID)
			}
		}
		sent[req.RunID] = append(sent[req.RunID], req)
	}
	return wrong
}

// batchSeen counts, over a whole sweep, the interrupted batches in which each
// thing an interruption can cause was seen. A sweep that never saw one of
// them did not reach the states its assertions are about.
type batchSeen struct {
	// toolAgain: a tool was invoked a second time under one key.
	// effectAgain: one of those was a tool whose effect is made under Once.
	// askedAgain: a person was asked about an interrupted at-most-once call.
	// modelAgain: the model was sent the same conversation a second time.
	// childFound: the delegating step was interrupted with its child made.
	toolAgain, effectAgain, askedAgain, modelAgain, childFound int
}

func (seen *batchSeen) note(w *batchWorld, log *toolLog, model *agenttest.Model, killed batchMoment) {
	w.t.Helper()
	lead, _ := w.lead()
	if killed.children == 1 && killed.status[agent.StepKey(lead.ID, 6)] == agent.StepStarted {
		seen.childFound++
	}
	again, effect := false, false
	for _, invocations := range log.byKey() {
		if len(invocations) > 1 {
			again = true
			if name := invocations[0].Call.Name; name == "send" || name == "save" {
				effect = true
			}
		}
	}
	if again {
		seen.toolAgain++
	}
	if effect {
		seen.effectAgain++
	}
	for _, approval := range w.changes(lead.ID).Approvals {
		if approval.Cause == agent.CauseInterrupted {
			seen.askedAgain++
			break
		}
	}
	sent := map[string]bool{}
	for _, req := range model.Requests() {
		was := fmt.Sprintf("%s after %d messages", req.RunID, len(req.Messages))
		if sent[was] {
			seen.modelAgain++
			break
		}
		sent[was] = true
	}
}

// The crash sweep of 6.15 of the design, over Postgres. One scripted batch
// uses everything an execution can do: model calls, a tool the guard allows,
// an at-most-once tool, a tool the guard asks a person about, a delegation
// whose child runs a tool of its own, and a final answer built from every
// result. The first process's store is killed before every store call in
// turn, and after every one, as a crash would; a second process on the same
// database finishes the batch once the first one's leases have lapsed.
func TestExecutor_CrashAtEveryStoreCallOverPostgres(t *testing.T) {
	w := newBatchWorld(t)

	// The batch with nothing going wrong: what it comes to, and how many
	// store calls its process makes.
	log, model := &toolLog{}, agenttest.NewModel(batchScript())
	whole := w.process("worker-0", log, model, nil)
	require.NoError(t, w.drive(whole))
	want, total := w.outcome(log), whole.faults.Calls()
	require.Empty(t, w.report(log, model, want, nil, 0), "the uninterrupted batch keeps every promise")
	require.Equal(t, agent.StatusCompleted, want.status)
	require.Equal(t, "done: send ok; saved: save ok", want.output)
	require.Equal(t, map[string][]string{
		"lead": {
			"1 model completed", "2 lookup completed", "3 charge completed",
			"4 model completed", "5 send completed", "6 review completed", "7 model completed",
		},
		"the child of step 6": {"1 model completed", "2 save completed", "3 model completed"},
	}, want.journals)
	require.Equal(t, []string{"lead:2", "lead:3", "lead:5", "the child of step 6:2"}, want.keys,
		"lookup, charge, send and the child's save each ran")
	require.Equal(t, []string{"lead:5 send", "the child of step 6:2 save"}, want.ledger)
	require.Greater(t, total, 50, "the batch makes enough store calls for the sweep to mean something")

	points := 0
	var seen batchSeen
	for n := 1; n <= total; n++ {
		for _, flavour := range []string{"before", "after"} {
			points++
			t.Run(fmt.Sprintf("killed %s call %d", flavour, n), func(t *testing.T) {
				w.t = t
				w.empty()
				log, model := &toolLog{}, agenttest.NewModel(batchScript())
				first := w.process("worker-1", log, model, nil)
				if flavour == "before" {
					first.faults.KillBefore(n)
				} else {
					first.faults.KillAfter(n)
				}

				err := w.drive(first)
				require.ErrorIs(t, err, agenttest.ErrKilled, "the first process runs until its store dies")
				killed := w.moment(log)

				// Another process, on the same database and behind the same
				// model, once the dead one's leases have lapsed.
				second := w.process("worker-2", log, model, nil)
				w.clock.Advance(batchTTL)
				require.NoError(t, w.drive(second))

				assert.Empty(t, w.report(log, model, want, &killed, 1))
				seen.note(w, log, model, killed)
			})
		}
	}
	w.t = t
	t.Logf("killed the store at %d points: before and after each of the %d store calls of the uninterrupted batch", points, total)
	assert.Positive(t, seen.toolAgain, "no interruption made a tool run again")
	assert.Positive(t, seen.effectAgain, "no interruption made a tool whose effect is made under Once run again")
	assert.Positive(t, seen.askedAgain, "no interruption left the at-most-once call to be asked about")
	assert.Positive(t, seen.modelAgain, "no interruption made a model call again")
	assert.Positive(t, seen.childFound, "no interruption fell between a child being started and its step recording it")
	t.Logf("of those: a tool ran again in %d (one whose effect is made under Once in %d), a person was asked about "+
		"the at-most-once call in %d, a model call was made again in %d, a child was asked for twice in %d",
		seen.toolAgain, seen.effectAgain, seen.askedAgain, seen.modelAgain, seen.childFound)
}

// parkHook is a store that acts as a Park arrives, once.
type parkHook struct {
	agent.Store
	once   sync.Once
	before func()
}

func (s *parkHook) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	s.once.Do(s.before)
	return s.Store.Park(ctx, lease, req)
}

// Two calls of one reply are both put to a person, and one is approved after
// the execution has read the journal and before it parks. The execution does
// not park on the other question with the approved call unrun: the store
// refuses the park, the execution reads the answer, runs the call, and parks
// afterwards on what is left.
func TestExecutor_AnAnswerThatLandsAsARunWithTwoQuestionsParksIsActedOnAtOnce(t *testing.T) {
	w := newBatchWorld(t)
	log := &toolLog{}
	model := agenttest.NewModel(agenttest.ByAgent(map[string]agenttest.Script{
		"lead": agenttest.Replies(
			agenttest.Use(
				agenttest.Call("call-1", "send", `{"to":"ops"}`),
				agenttest.Call("call-2", "send", `{"to":"legal"}`),
			),
			agenttest.Say("both are sent"),
		),
	}))
	var lead agent.Run
	p := w.process("worker-1", log, model, func(inner agent.Store) agent.Store {
		return &parkHook{Store: inner, before: func() {
			asked := w.asked(lead.ID)
			if !assert.Len(t, asked, 2) {
				return
			}
			_, err := w.store.DecideApproval(context.Background(), agent.DecideRequest{
				ID: asked[2].ID, Approved: true, By: "ops@example.test", Now: w.clock.Now(),
			})
			assert.NoError(t, err)
		}}
	})
	lead, err := p.engine.Start(t.Context(), agent.StartRequest{Agent: "lead", Input: "send both"})
	require.NoError(t, err)

	got, err := p.engine.Execute(t.Context(), lead.ID)

	require.NoError(t, err)
	assert.Equal(t, agent.StatusWaiting, got.Status, "the second question is still pending")
	assert.Equal(t, agent.ReasonApproval, got.Reason)
	assert.Empty(t, got.LeaseOwner)
	changes := w.changes(lead.ID)
	assert.Equal(t, []string{"1 model completed", "2 send completed", "3 send waiting"}, journalLines(changes.Steps),
		"the approved call ran in the execution that was about to park")
	assert.Len(t, log.byKey()[agent.StepKey(lead.ID, 2)], 1)
	assert.Empty(t, log.byKey()[agent.StepKey(lead.ID, 3)])
	asked := w.asked(lead.ID)
	require.Len(t, asked, 2)
	assert.Equal(t, agent.ApprovalApproved, asked[2].Status)
	assert.Equal(t, agent.ApprovalPending, asked[3].Status)

	// The second answer wakes the run, and it ends.
	_, err = p.engine.Approve(t.Context(), asked[3].ID, "ops@example.test", "")
	require.NoError(t, err)
	got, err = p.engine.Execute(t.Context(), lead.ID)
	require.NoError(t, err)
	assert.Equal(t, agent.StatusCompleted, got.Status)
	assert.Equal(t, "both are sent", got.Output)
}

// A reply whose arguments or provider form are not JSON, or not UTF-8, is
// one this store refuses, and would refuse on every retry. The executor
// journals it as a store can keep it: the arguments as one JSON string on a
// malformed call, which is answered and never run, and the provider form
// dropped.
func TestExecutor_AReplyNoColumnCanKeepIsJournaledAsOneItCan(t *testing.T) {
	tests := []struct {
		name  string
		input string
		held  string
	}{
		{"not JSON", `{"to":`, `{"to":`},
		{"JSON, and not UTF-8", "{\"to\":\"\xff\"}", "{\"to\":\"�\"}"},
		{"a NUL character", "{\x00}", "{\x00}"},
	}
	w := newBatchWorld(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w.t = t
			w.empty()
			log := &toolLog{}
			model := agenttest.NewModel(agenttest.ByAgent(map[string]agenttest.Script{
				"lead": func(_ agent.Request, turn int) (agent.Response, error) {
					if turn > 0 {
						return agenttest.Say("it could not be read"), nil
					}
					return agent.Response{
						Message: agent.Message{
							Role: agent.RoleAssistant,
							Calls: []agent.Call{
								{ID: "call-1", Name: "lookup", Input: []byte(tt.input)},
								{ID: "call-2", Name: "lookup", Input: []byte(`{"order":7}`)},
							},
							Opaque: &agent.Opaque{Provider: "vendor", Data: []byte(tt.input)},
						},
						Stop: agent.StopToolUse,
					}, nil
				},
			}))
			p := w.process("worker-1", log, model, nil)
			lead, err := p.engine.Start(t.Context(), agent.StartRequest{Agent: "lead", Input: "look it up"})
			require.NoError(t, err)

			got, err := p.engine.Execute(t.Context(), lead.ID)

			require.NoError(t, err)
			require.Equal(t, agent.StatusCompleted, got.Status, got.Error)
			assert.Zero(t, got.Failures)
			steps := w.changes(lead.ID).Steps
			require.Equal(t, []string{"1 model completed", "2 lookup completed", "3 lookup completed", "4 model completed"},
				journalLines(steps))
			assert.Nil(t, steps[0].Message.Opaque)
			require.NotNil(t, steps[1].Call)
			assert.True(t, steps[1].Call.Malformed)
			var held string
			require.NoError(t, json.Unmarshal(steps[1].Call.Input, &held))
			assert.Equal(t, tt.held, held)
			assert.Equal(t, "arguments were not valid JSON", steps[1].Result)
			assert.True(t, steps[1].IsError)
			assert.Empty(t, log.byKey()[agent.StepKey(lead.ID, 2)], "a malformed call is never run")
			assert.Len(t, log.byKey()[agent.StepKey(lead.ID, 3)], 1)
		})
	}
}
