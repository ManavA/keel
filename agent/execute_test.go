package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// The executor is tested inside a synctest bubble, as the keeper is. Every
// execution starts a keeper, whose timer runs on the bubble's clock: that
// clock moves only when every goroutine of the test is blocked, so a
// heartbeat is made when a test lets time pass and at no other moment, and a
// count of store calls is the same on every machine. The bubble also fails a
// test that returns with a keeper, a tool or a watcher still running, as
// deadlocked. Journal times come from the kit's clock, which moves only when
// a test advances it.

var execStart = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

const (
	execTTL       = 30 * time.Second
	execHeartbeat = 10 * time.Second
)

// execID is the nth id made by the engine numbered engine: a UUID, since a
// store keeps nothing else.
func execID(engine, n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%02d%010d", engine, n)
}

// execConfig is what a test says about the engine it wants.
type execConfig struct {
	// defs are the agents registered.
	defs []agent.Definition
	// script is the scripted model's. model, when set, answers instead.
	script agenttest.Script
	model  agent.Model
	guard  agent.Guard
	// tune changes the engine's options after the fixture has set them.
	tune func(*agent.Options)
	// over, when set, wraps the store the engine is given.
	over func(agent.Store) agent.Store
}

// execFixture is one engine, as one process is: over a store that can be
// killed, a clock the test moves, and a bus and a log that keep what they are
// given. Its rivals are other engines over the same store and clock.
type execFixture struct {
	t      *testing.T
	engine *agent.Engine
	memory *agent.MemoryStore
	faults *agenttest.FaultStore
	clock  *agenttest.Clock
	bus    *engineBus
	logs   *engineLogs
	// model is the scripted model, which every engine of a test shares, so
	// that its requests are the run's whatever process sent them.
	model  *agenttest.Model
	number int
	// engines counts the engines built over the store, the first included.
	engines *int
}

func newExecFixture(t *testing.T, cfg execConfig) *execFixture {
	t.Helper()
	engines := 0
	return buildExecFixture(t, cfg, agent.NewMemoryStore(), agenttest.NewClock(execStart),
		agenttest.NewModel(cfg.script), &engines)
}

// rival is another process: an engine of its own over the same store, clock
// and scripted model, with a store wrapper of its own to be killed.
func (f *execFixture) rival(cfg execConfig) *execFixture {
	f.t.Helper()
	model := f.model
	if cfg.script != nil {
		model = agenttest.NewModel(cfg.script)
	}
	return buildExecFixture(f.t, cfg, f.memory, f.clock, model, f.engines)
}

func buildExecFixture(
	t *testing.T, cfg execConfig, memory *agent.MemoryStore, clock *agenttest.Clock,
	model *agenttest.Model, engines *int,
) *execFixture {
	t.Helper()
	*engines++
	f := &execFixture{
		t:       t,
		memory:  memory,
		faults:  agenttest.NewFaultStore(memory),
		clock:   clock,
		bus:     &engineBus{},
		logs:    &engineLogs{},
		model:   model,
		number:  *engines,
		engines: engines,
	}
	var store agent.Store = f.faults
	if cfg.over != nil {
		store = cfg.over(store)
	}
	var answers agent.Model = f.model
	if cfg.model != nil {
		answers = cfg.model
	}
	var ids atomic.Int64
	opts := agent.Options{
		Model:             answers,
		Store:             store,
		Guard:             cfg.guard,
		Clock:             f.clock,
		Events:            f.bus,
		Logger:            slog.New(f.logs),
		NewID:             func() string { return execID(f.number, int(ids.Add(1))) },
		WorkerID:          fmt.Sprintf("worker-%d", f.number),
		LeaseTTL:          execTTL,
		HeartbeatInterval: execHeartbeat,
	}
	if cfg.tune != nil {
		cfg.tune(&opts)
	}
	engine, err := agent.New(opts)
	require.NoError(t, err)
	f.engine = engine
	for _, def := range cfg.defs {
		require.NoError(t, engine.Register(def))
	}
	return f
}

func (f *execFixture) start(name, input string) agent.Run {
	f.t.Helper()
	run, err := f.engine.Start(f.t.Context(), agent.StartRequest{Agent: name, Input: input})
	require.NoError(f.t, err)
	return run
}

// execute runs one execution that must end well: the run ended or parked.
func (f *execFixture) execute(id string) agent.Run {
	f.t.Helper()
	run, err := f.engine.Execute(f.t.Context(), id)
	require.NoError(f.t, err)
	return run
}

// execOutcome is what one call of Execute returned.
type execOutcome struct {
	run agent.Run
	err error
}

// begin starts an execution on a goroutine of its own, for a test that acts
// while it is in flight.
func (f *execFixture) begin(ctx context.Context, id string) <-chan execOutcome {
	out := make(chan execOutcome, 1)
	go func() {
		run, err := f.engine.Execute(ctx, id)
		out <- execOutcome{run: run, err: err}
	}()
	return out
}

// pass moves the kit's clock and the bubble's on together by d, as both move
// for a process running in real time, and returns once everything that woke
// is blocked again.
func (f *execFixture) pass(d time.Duration) {
	f.clock.Advance(d)
	time.Sleep(d)
	synctest.Wait()
}

func (f *execFixture) run(id string) agent.Run {
	f.t.Helper()
	run, err := f.memory.GetRun(context.Background(), id)
	require.NoError(f.t, err)
	return run
}

func (f *execFixture) steps(id string) []agent.Step {
	f.t.Helper()
	steps, err := f.memory.Steps(context.Background(), id)
	require.NoError(f.t, err)
	return steps
}

func (f *execFixture) approvals(id string) []agent.Approval {
	f.t.Helper()
	changes, err := f.memory.Changes(context.Background(), id, 0)
	require.NoError(f.t, err)
	return changes.Approvals
}

func (f *execFixture) children(id string) []agent.Run {
	f.t.Helper()
	children, err := f.memory.ListRuns(context.Background(), agent.RunFilter{ParentID: id, Limit: 200})
	require.NoError(f.t, err)
	return children
}

// journal is the run's journal, a line a step.
func (f *execFixture) journal(id string) []string {
	f.t.Helper()
	return execJournal(f.steps(id))
}

// execJournal writes each step as its seq, what it is and where it stands.
func execJournal(steps []agent.Step) []string {
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

// events is the types of what the engine has published, in order, each with
// the step it is about when it is about one.
func (f *execFixture) events() []string {
	var out []string
	for _, published := range f.bus.published() {
		event, ok := published.(agent.Event)
		if !ok {
			out = append(out, fmt.Sprintf("%T", published))
			continue
		}
		if event.Seq != 0 {
			out = append(out, fmt.Sprintf("%s %d", event.Type, event.Seq))
			continue
		}
		out = append(out, event.Type)
	}
	return out
}

// execCalls records every invocation of the tools made from it.
type execCalls struct {
	mu   sync.Mutex
	seen []agent.Invocation
}

// tool is a tool named name that records each invocation and then does run.
// With no run it returns "<name> ok".
func (c *execCalls) tool(name string, run agent.ToolFunc) agent.Tool {
	return agent.Tool{Name: name, Run: func(ctx context.Context, in agent.Invocation) (string, error) {
		noted := in
		noted.Call.Input = append(json.RawMessage(nil), in.Call.Input...)
		c.mu.Lock()
		c.seen = append(c.seen, noted)
		c.mu.Unlock()
		if run == nil {
			return name + " ok", nil
		}
		return run(ctx, in)
	}}
}

// of returns the invocations of the tool named name, in order.
func (c *execCalls) of(name string) []agent.Invocation {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []agent.Invocation
	for _, in := range c.seen {
		if in.Call.Name == name {
			out = append(out, in)
		}
	}
	return out
}

// execGuard is a guard that answers by the action's target and counts what it
// was asked.
type execGuard struct {
	mu      sync.Mutex
	answers map[string]agent.Decision
	err     error
	asked   []agent.Action
}

func (g *execGuard) Decide(_ context.Context, a agent.Action) (agent.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked = append(g.asked, a)
	if g.err != nil {
		return agent.Decision{}, g.err
	}
	if answer, ok := g.answers[a.Target]; ok {
		return answer, nil
	}
	return agent.Decision{Effect: agent.Allow, Rule: "default-allow"}, nil
}

func (g *execGuard) questions() []agent.Action {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]agent.Action(nil), g.asked...)
}

func (g *execGuard) failWith(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
}

// execModelFunc adapts a function to agent.Model.
type execModelFunc func(ctx context.Context, req agent.Request) (agent.Response, error)

func (f execModelFunc) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	return f(ctx, req)
}

// execClerk is an agent named clerk with the given tools.
func execClerk(tools ...agent.Tool) agent.Definition {
	return agent.Definition{Name: "clerk", System: "be exact", Model: "model-a", MaxTokens: 512, Tools: tools}
}

// execSpent is a reply with what it cost.
func execSpent(resp agent.Response, usage agent.Usage) agent.Response {
	resp.Usage = usage
	return resp
}

// execResults is the tool message a request ends with, or nil when it ends
// with something else.
func execResults(req agent.Request) []agent.Result {
	if len(req.Messages) == 0 {
		return nil
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != agent.RoleTool {
		return nil
	}
	return last.Results
}

func TestExecute_ARunWithNoToolsCompletesWithTheModelsText(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk()},
			script: agenttest.Replies(execSpent(agenttest.Say("the total is 42"), agent.Usage{InputTokens: 30, OutputTokens: 6, CostMicros: 90})),
		})
		started := f.start("clerk", "what is the total?")
		f.clock.Advance(time.Second)

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "the total is 42", got.Output)
		assert.Empty(t, got.Reason)
		assert.Empty(t, got.Error)
		assert.Empty(t, got.LeaseOwner, "a run that ended is held by nobody")
		assert.Nil(t, got.LeaseExpiresAt)
		assert.Equal(t, int64(1), got.LeaseEpoch)
		require.NotNil(t, got.FinishedAt)
		assert.Equal(t, execStart.Add(time.Second), *got.FinishedAt, "times come from the engine's clock")
		assert.Equal(t, agent.Usage{InputTokens: 30, OutputTokens: 6, CostMicros: 90}, got.Usage)
		assert.Equal(t, 1, got.ModelCalls)
		assert.Equal(t, f.run(started.ID), got, "Execute returns the run as it stands")

		steps := f.steps(started.ID)
		require.Len(t, steps, 1)
		assert.Equal(t, agent.StepModel, steps[0].Kind)
		assert.Equal(t, agent.StepCompleted, steps[0].Status)
		assert.Equal(t, agent.StopEnd, steps[0].Stop)
		assert.Equal(t, 1, steps[0].Attempts)
		assert.Equal(t, "model-a", steps[0].Name, "the model that answered")
		require.NotNil(t, steps[0].Message)
		assert.Equal(t, "the total is 42", steps[0].Message.Text)

		requests := f.model.Requests()
		require.Len(t, requests, 1)
		assert.Equal(t, agent.Request{
			RunID:     started.ID,
			Agent:     "clerk",
			Model:     "model-a",
			System:    "be exact",
			Messages:  []agent.Message{{Role: agent.RoleUser, Text: "what is the total?"}},
			MaxTokens: 512,
		}, requests[0])
	})
}

func TestExecute_AToolsResultReachesTheModelInTheNextRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		lookup := calls.tool("lookup", func(_ context.Context, in agent.Invocation) (string, error) {
			return "found " + string(in.Call.Input), nil
		})
		lookup.Description = "finds an order"
		lookup.Schema = json.RawMessage(`{"type":"object"}`)
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("zeta", nil), lookup)},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{"order":7}`)),
				agenttest.Say("order 7 is paid"),
			),
		})
		started := f.start("clerk", "is order 7 paid?")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "order 7 is paid", got.Output)
		assert.Equal(t, []string{"1 model completed", "2 lookup completed", "3 model completed"}, f.journal(started.ID))

		steps := f.steps(started.ID)
		assert.Equal(t, `found {"order":7}`, steps[1].Result)
		assert.False(t, steps[1].IsError)
		assert.Equal(t, agent.Allow, steps[1].Decision)
		assert.Equal(t, agent.RuleNoGuard, steps[1].Rule, "with no guard every call is allowed, and the journal says why")
		assert.Equal(t, 1, steps[1].Attempts)

		ran := calls.of("lookup")
		require.Len(t, ran, 1)
		assert.Equal(t, agent.Invocation{
			RunID: started.ID, Agent: "clerk", Seq: 2, Attempt: 1, Key: agent.StepKey(started.ID, 2),
			Call: agenttest.Call("call-1", "lookup", `{"order":7}`),
		}, ran[0])

		requests := f.model.Requests()
		require.Len(t, requests, 2)
		assert.Equal(t, []agent.ToolSpec{
			{Name: "lookup", Description: "finds an order", Schema: json.RawMessage(`{"type":"object"}`)},
			{Name: "zeta"},
		}, requests[0].Tools, "the snapshot's tools, sorted by name")
		assert.Equal(t, requests[0].Tools, requests[1].Tools)
		assert.Equal(t, []agent.Message{
			{Role: agent.RoleUser, Text: "is order 7 paid?"},
			{Role: agent.RoleAssistant, Calls: []agent.Call{agenttest.Call("call-1", "lookup", `{"order":7}`)}},
			{Role: agent.RoleTool, Results: []agent.Result{{CallID: "call-1", Content: `found {"order":7}`}}},
		}, requests[1].Messages)
	})
}

func TestExecute_AToolErrorReachesTheModelAsAnErrorResultAndTheRunGoesOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("lookup", func(context.Context, agent.Invocation) (string, error) {
				return "", errors.New("no such order")
			}))},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{"order":9}`)),
				agenttest.Say("there is no order 9"),
			),
		})
		started := f.start("clerk", "is order 9 paid?")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "there is no order 9", got.Output)
		assert.Zero(t, got.Failures, "a tool's error is the model's to read, not a failed execution")
		steps := f.steps(started.ID)
		require.Len(t, steps, 3)
		assert.Equal(t, agent.StepCompleted, steps[1].Status)
		assert.Equal(t, "no such order", steps[1].Result)
		assert.True(t, steps[1].IsError)
		require.Len(t, calls.of("lookup"), 1)

		requests := f.model.Requests()
		require.Len(t, requests, 2)
		assert.Equal(t, []agent.Result{{CallID: "call-1", Content: "no such order", IsError: true}}, execResults(requests[1]))
	})
}

func TestExecute_ATransientErrorEndsTheExecutionFailedAndTheNextRunsTheToolAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("lookup", func(_ context.Context, in agent.Invocation) (string, error) {
				if in.Attempt == 1 {
					return "", fmt.Errorf("upstream timed out: %w", agent.ErrTransient)
				}
				return "paid", nil
			}))},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{"order":7}`)),
				agenttest.Say("order 7 is paid"),
			),
		})
		started := f.start("clerk", "is order 7 paid?")

		got, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorIs(t, err, agent.ErrTransient, "the execution ended failed, and Execute says why")
		assert.Equal(t, agent.StatusRunnable, got.Status)
		assert.Equal(t, 1, got.Failures)
		assert.Equal(t, "upstream timed out: agent: transient failure", got.Error)
		assert.Empty(t, got.LeaseOwner, "the run is given back")
		require.NotNil(t, got.NextAttemptAt)
		assert.Equal(t, execStart.Add(time.Second), *got.NextAttemptAt)
		assert.Equal(t, []string{"1 model completed", "2 lookup started"}, f.journal(started.ID),
			"the call stays started, with no result")

		_, err = f.engine.Execute(t.Context(), started.ID)
		require.ErrorIs(t, err, agent.ErrNotClaimable, "the run waits out its back-off")

		f.clock.Advance(time.Second)
		got = f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "order 7 is paid", got.Output)
		assert.Zero(t, got.Failures, "a step that completes resets the count")
		ran := calls.of("lookup")
		require.Len(t, ran, 2)
		assert.Equal(t, agent.StepKey(started.ID, 2), ran[0].Key)
		assert.Equal(t, ran[0].Key, ran[1].Key, "the same key on every attempt")
		assert.Equal(t, 1, ran[0].Attempt)
		assert.Equal(t, 2, ran[1].Attempt)
		assert.Equal(t, ran[0].Call, ran[1].Call)
		steps := f.steps(started.ID)
		assert.Equal(t, 2, steps[1].Attempts)
		assert.Equal(t, "paid", steps[1].Result)
		assert.Len(t, f.model.Requests(), 2, "the reply that made the call is not asked for again")
	})
}

func TestExecute_AMalformedCallAndAMissingToolGetTheirFixedResultsWithoutRunningAnything(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		guard := &execGuard{}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("lookup", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(
					agenttest.Call("call-1", "lookup", `{"order":`),
					agenttest.Call("call-2", "gone", `{"order":7}`),
					agenttest.Call("call-3", "gone", `{"order":`),
				),
				agenttest.Say("neither worked"),
			),
		})
		started := f.start("clerk", "is order 7 paid?")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		steps := f.steps(started.ID)
		require.Len(t, steps, 5)
		for i, want := range []string{
			"arguments were not valid JSON",
			"tool is not available",
			"arguments were not valid JSON",
		} {
			st := steps[i+1]
			assert.Equal(t, agent.StepCompleted, st.Status, "step %d", st.Seq)
			assert.Equal(t, want, st.Result, "step %d", st.Seq)
			assert.True(t, st.IsError, "step %d", st.Seq)
			assert.Zero(t, st.Attempts, "step %d never started", st.Seq)
			assert.Empty(t, st.Decision, "step %d was put to no guard", st.Seq)
		}
		assert.Empty(t, calls.of("lookup"), "nothing ran")
		assert.Empty(t, guard.questions(), "the guard is not asked about a call that cannot run")
		assert.Equal(t, []agent.Result{
			{CallID: "call-1", Content: "arguments were not valid JSON", IsError: true},
			{CallID: "call-2", Content: "tool is not available", IsError: true},
			{CallID: "call-3", Content: "arguments were not valid JSON", IsError: true},
		}, execResults(f.model.Requests()[1]))
	})
}

func TestExecute_TheGuardBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		guard := &execGuard{answers: map[string]agent.Decision{
			"refund": {Effect: agent.Block, Rule: "no-refunds"},
		}}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
				agenttest.Say("I may not refund it"),
			),
		})
		started := f.start("clerk", "refund order 7")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "I may not refund it", got.Output)
		steps := f.steps(started.ID)
		require.Len(t, steps, 3)
		assert.Equal(t, agent.StepBlocked, steps[1].Status)
		assert.Equal(t, agent.Block, steps[1].Decision)
		assert.Equal(t, "no-refunds", steps[1].Rule)
		assert.Equal(t, "blocked by policy: no-refunds", steps[1].Result)
		assert.True(t, steps[1].IsError)
		assert.Zero(t, steps[1].Attempts)
		assert.Empty(t, calls.of("refund"), "a blocked tool never runs")

		assert.Equal(t, []agent.Action{{
			Kind: "run", Target: "refund",
			Attrs: map[string]any{"agent": "clerk", "tool": "refund", "run": started.ID, "seq": 2},
		}}, guard.questions())
		assert.Equal(t, []agent.Result{{CallID: "call-1", Content: "blocked by policy: no-refunds", IsError: true}},
			execResults(f.model.Requests()[1]))
		assert.Contains(t, f.events(), "step.blocked 2")
	})
}

func TestExecute_AnEffectTheEngineDoesNotKnowIsABlock(t *testing.T) {
	for _, effect := range []agent.Effect{"", "maybe", "ALLOW"} {
		t.Run(fmt.Sprintf("effect %q", effect), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				f := newExecFixture(t, execConfig{
					defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
					guard: &execGuard{answers: map[string]agent.Decision{"refund": {Effect: effect, Rule: "odd"}}},
					script: agenttest.Replies(
						agenttest.Use(agenttest.Call("call-1", "refund", `{}`)),
						agenttest.Say("done"),
					),
				})
				started := f.start("clerk", "refund order 7")

				f.execute(started.ID)

				steps := f.steps(started.ID)
				assert.Equal(t, agent.StepBlocked, steps[1].Status)
				assert.Equal(t, agent.Block, steps[1].Decision, "the journal says what was done")
				assert.Equal(t, "odd", steps[1].Rule)
				assert.Equal(t, "blocked by policy: odd", steps[1].Result)
				assert.Empty(t, calls.of("refund"))
			})
		})
	}
}

func TestExecute_TheGuardAsks(t *testing.T) {
	// parked takes a run to where the guard's question leaves it.
	parked := func(t *testing.T) (*execFixture, *execCalls, *execGuard, agent.Run, agent.Approval) {
		calls := &execCalls{}
		guard := &execGuard{answers: map[string]agent.Decision{
			"refund": {Effect: agent.Ask, Rule: "ask-first"},
		}}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7,"amount":1250}`)),
				agenttest.Say("the refund is settled"),
			),
		})
		started := f.start("clerk", "refund order 7")
		run := f.execute(started.ID)
		approvals := f.approvals(started.ID)
		require.Len(t, approvals, 1)
		return f, calls, guard, run, approvals[0]
	}

	t.Run("the run parks for a person", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, _, run, approval := parked(t)

			assert.Equal(t, agent.StatusWaiting, run.Status)
			assert.Equal(t, agent.ReasonApproval, run.Reason)
			assert.Empty(t, run.LeaseOwner, "a parked run is held by nobody")
			assert.Equal(t, []string{"1 model completed", "2 refund waiting"}, f.journal(run.ID))
			step := f.steps(run.ID)[1]
			assert.Equal(t, agent.Ask, step.Decision)
			assert.Equal(t, "ask-first", step.Rule)
			assert.Empty(t, calls.of("refund"))

			assert.Equal(t, agent.Approval{
				ID: execID(1, 2), RunID: run.ID, Seq: 2, Attempt: 0, Cause: agent.CauseGuard,
				Tool: "refund", Input: json.RawMessage(`{"order":7,"amount":1250}`),
				Action: agent.Action{
					Kind: "run", Target: "refund",
					Attrs: map[string]any{"agent": "clerk", "tool": "refund", "run": run.ID, "seq": float64(2)},
				},
				Rule: "ask-first", Status: agent.ApprovalPending, Rev: approval.Rev, RequestedAt: execStart,
			}, approval)
		})
	})

	t.Run("a parked run cannot be executed and nothing new is asked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, guard, run, _ := parked(t)

			_, err := f.engine.Execute(t.Context(), run.ID)

			require.ErrorIs(t, err, agent.ErrNotClaimable)
			assert.Len(t, f.approvals(run.ID), 1)
			assert.Len(t, guard.questions(), 1)
			assert.Empty(t, calls.of("refund"))
			assert.Equal(t, run, f.run(run.ID), "the run is exactly where it was")
		})
	})

	t.Run("approved, the tool runs once with the arguments in the journal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, guard, run, approval := parked(t)
			_, err := f.engine.Approve(t.Context(), approval.ID, "ops@example.test", "within policy")
			require.NoError(t, err)

			got := f.execute(run.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, "the refund is settled", got.Output)
			ran := calls.of("refund")
			require.Len(t, ran, 1)
			assert.Equal(t, 1, ran[0].Attempt)
			assert.JSONEq(t, `{"order":7,"amount":1250}`, string(ran[0].Call.Input))
			step := f.steps(run.ID)[1]
			assert.Equal(t, string(step.Call.Input), string(ran[0].Call.Input), "what was approved is what runs")
			assert.Equal(t, string(approval.Input), string(ran[0].Call.Input))
			assert.Equal(t, agent.StepCompleted, step.Status)
			assert.Equal(t, agent.Ask, step.Decision, "the guard's answer stays on the step")
			assert.Equal(t, "ask-first", step.Rule)
			assert.Len(t, guard.questions(), 1, "an approved call is not put to the guard again")
		})
	})

	t.Run("declined, the tool does not run and the model is told who and why", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, _, run, approval := parked(t)
			_, err := f.engine.Decline(t.Context(), approval.ID, "ops@example.test", "over the limit")
			require.NoError(t, err)

			got := f.execute(run.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Empty(t, calls.of("refund"))
			step := f.steps(run.ID)[1]
			assert.Equal(t, agent.StepDeclined, step.Status)
			assert.Equal(t, "declined by ops@example.test: over the limit", step.Result)
			assert.True(t, step.IsError)
			assert.Zero(t, step.Attempts)
			assert.Equal(t, []agent.Result{
				{CallID: "call-1", Content: "declined by ops@example.test: over the limit", IsError: true},
			}, execResults(f.model.Requests()[1]))
		})
	})

	t.Run("declined with no reason, the result names who and stops there", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, _, run, approval := parked(t)
			_, err := f.engine.Decline(t.Context(), approval.ID, "ops@example.test", "")
			require.NoError(t, err)

			f.execute(run.ID)

			assert.Equal(t, "declined by ops@example.test", f.steps(run.ID)[1].Result)
		})
	})
}

func TestExecute_ARunWokenWithAnApprovalStillPendingParksAgainAndAsksNothingNew(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		guard := &execGuard{answers: map[string]agent.Decision{
			"refund": {Effect: agent.Ask, Rule: "ask-first"},
		}}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(
					agenttest.Call("call-1", "refund", `{"order":7}`),
					agenttest.Call("call-2", "refund", `{"order":8}`),
				),
				agenttest.Say("both settled"),
			),
		})
		started := f.start("clerk", "refund orders 7 and 8")
		f.execute(started.ID)
		approvals := f.approvals(started.ID)
		require.Len(t, approvals, 2, "both calls are asked about before the run parks")
		require.Len(t, guard.questions(), 2)

		_, err := f.engine.Approve(t.Context(), approvals[1].ID, "ops@example.test", "")
		require.NoError(t, err)
		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusWaiting, got.Status)
		assert.Equal(t, agent.ReasonApproval, got.Reason)
		assert.Equal(t, []string{"1 model completed", "2 refund waiting", "3 refund completed"}, f.journal(started.ID))
		assert.Len(t, f.approvals(started.ID), 2, "nothing new is asked")
		assert.Len(t, guard.questions(), 2, "and the guard is not asked again")
		require.Len(t, calls.of("refund"), 1)
		assert.Equal(t, 3, calls.of("refund")[0].Seq)
		assert.Len(t, f.model.Requests(), 1, "the model is not called while a call of its reply is open")
	})
}

func TestExecute_AToolMarkedApprovalParksThoughTheGuardAllowed(t *testing.T) {
	for name, guard := range map[string]agent.Guard{
		"with no guard":            nil,
		"with a guard that allows": &execGuard{},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				send := calls.tool("send", nil)
				send.Approval = true
				f := newExecFixture(t, execConfig{
					defs:  []agent.Definition{execClerk(send)},
					guard: guard,
					script: agenttest.Replies(
						agenttest.Use(agenttest.Call("call-1", "send", `{"to":"all"}`)),
						agenttest.Say("sent"),
					),
				})
				started := f.start("clerk", "send the digest")

				got := f.execute(started.ID)

				assert.Equal(t, agent.StatusWaiting, got.Status)
				assert.Equal(t, agent.ReasonApproval, got.Reason)
				assert.Empty(t, calls.of("send"))
				step := f.steps(started.ID)[1]
				assert.Equal(t, agent.StepWaiting, step.Status)
				assert.Equal(t, agent.Allow, step.Decision, "the guard's answer")
				assert.Equal(t, agent.RuleToolApproval, step.Rule)
				approvals := f.approvals(started.ID)
				require.Len(t, approvals, 1)
				assert.Equal(t, agent.CauseTool, approvals[0].Cause)
				assert.Equal(t, agent.RuleToolApproval, approvals[0].Rule)
				assert.Equal(t, agent.ApprovalPending, approvals[0].Status)

				_, err := f.engine.Approve(t.Context(), approvals[0].ID, "ops@example.test", "")
				require.NoError(t, err)
				got = f.execute(started.ID)
				assert.Equal(t, agent.StatusCompleted, got.Status)
				assert.Len(t, calls.of("send"), 1)
			})
		})
	}
}

func TestExecute_AToolMarkedApprovalThatTheGuardAsksAboutIsAskedForTheGuard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		send := calls.tool("send", nil)
		send.Approval = true
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk(send)},
			guard:  &execGuard{answers: map[string]agent.Decision{"send": {Effect: agent.Ask, Rule: "external"}}},
			script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "send", `{}`))),
		})
		started := f.start("clerk", "send the digest")

		f.execute(started.ID)

		approvals := f.approvals(started.ID)
		require.Len(t, approvals, 1)
		assert.Equal(t, agent.CauseGuard, approvals[0].Cause)
		assert.Equal(t, "external", approvals[0].Rule)
		assert.Equal(t, agent.Ask, f.steps(started.ID)[1].Decision)
	})
}

func TestExecute_TwoCallsTheFirstAskingAndTheSecondAllowed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("refund", nil), calls.tool("lookup", nil))},
			guard: &execGuard{answers: map[string]agent.Decision{
				"refund": {Effect: agent.Ask, Rule: "ask-first"},
			}},
			script: agenttest.Replies(
				agenttest.Use(
					agenttest.Call("call-1", "refund", `{"order":7}`),
					agenttest.Call("call-2", "lookup", `{"order":7}`),
				),
				agenttest.Say("done"),
			),
		})
		started := f.start("clerk", "refund order 7")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusWaiting, got.Status)
		assert.Equal(t, agent.ReasonApproval, got.Reason)
		assert.Equal(t, []string{"1 model completed", "2 refund waiting", "3 lookup completed"}, f.journal(started.ID),
			"the second call runs before the run parks")
		assert.Len(t, calls.of("lookup"), 1)
		assert.Empty(t, calls.of("refund"))
		assert.Len(t, f.model.Requests(), 1)
	})
}

func TestExecute_AtMostOnce(t *testing.T) {
	// interrupted takes a run to where a killed execution leaves an
	// at-most-once call: started, with the tool's work perhaps done. The
	// process that takes over is returned.
	interrupted := func(t *testing.T) (*execFixture, *execCalls, agent.Run) {
		calls := &execCalls{}
		var first *execFixture
		charge := func(*execFixture) agent.Tool {
			tool := calls.tool("charge", func(_ context.Context, in agent.Invocation) (string, error) {
				if in.Attempt == 1 {
					first.faults.Kill()
				}
				return "charged", nil
			})
			tool.AtMostOnce = true
			return tool
		}
		script := agenttest.Replies(
			agenttest.Use(agenttest.Call("call-1", "charge", `{"amount":1250}`)),
			agenttest.Say("the card is settled"),
		)
		first = newExecFixture(t, execConfig{script: script, defs: []agent.Definition{execClerk(charge(nil))}})
		started := first.start("clerk", "charge the card")

		_, err := first.engine.Execute(t.Context(), started.ID)
		require.ErrorIs(t, err, agenttest.ErrKilled)
		require.Equal(t, []string{"1 model completed", "2 charge started"}, first.journal(started.ID))
		require.Len(t, calls.of("charge"), 1)

		second := first.rival(execConfig{defs: []agent.Definition{execClerk(charge(nil))}})
		first.clock.Advance(execTTL)
		return second, calls, started
	}

	t.Run("an interrupted call is asked about, not run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, started := interrupted(t)

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusWaiting, got.Status)
			assert.Equal(t, agent.ReasonApproval, got.Reason)
			assert.Len(t, calls.of("charge"), 1, "the call is not made again")
			step := f.steps(started.ID)[1]
			assert.Equal(t, agent.StepWaiting, step.Status)
			assert.Equal(t, agent.Allow, step.Decision, "the guard's answer is not replaced by the question")
			assert.Equal(t, agent.RuleNoGuard, step.Rule)
			assert.Equal(t, 1, step.Attempts)
			approvals := f.approvals(started.ID)
			require.Len(t, approvals, 1)
			assert.Equal(t, agent.CauseInterrupted, approvals[0].Cause)
			assert.Equal(t, agent.RuleInterrupted, approvals[0].Rule)
			assert.Equal(t, 1, approvals[0].Attempt)
			assert.Equal(t, "charge", approvals[0].Tool)
			assert.JSONEq(t, `{"amount":1250}`, string(approvals[0].Input))
			assert.Equal(t, agent.Action{
				Kind: "run", Target: "charge",
				Attrs: map[string]any{"agent": "clerk", "tool": "charge", "run": started.ID, "seq": float64(2)},
			}, approvals[0].Action)
		})
	})

	t.Run("approved, it runs again with the same key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, started := interrupted(t)
			f.execute(started.ID)
			_, err := f.engine.Approve(t.Context(), f.approvals(started.ID)[0].ID, "ops@example.test", "it did not go through")
			require.NoError(t, err)

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, "the card is settled", got.Output)
			ran := calls.of("charge")
			require.Len(t, ran, 2)
			assert.Equal(t, 2, ran[1].Attempt)
			assert.Equal(t, ran[0].Key, ran[1].Key)
			step := f.steps(started.ID)[1]
			assert.Equal(t, agent.StepCompleted, step.Status)
			assert.Equal(t, "charged", step.Result)
			assert.Equal(t, 2, step.Attempts)
		})
	})

	t.Run("declined, it gets the fixed result with who and why", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, started := interrupted(t)
			f.execute(started.ID)
			_, err := f.engine.Decline(t.Context(), f.approvals(started.ID)[0].ID, "ops@example.test", "it went through")
			require.NoError(t, err)

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Len(t, calls.of("charge"), 1)
			step := f.steps(started.ID)[1]
			assert.Equal(t, agent.StepDeclined, step.Status)
			assert.Equal(t,
				"interrupted before its result was recorded; not run again: declined by ops@example.test: it went through",
				step.Result)
			assert.True(t, step.IsError)
			assert.Equal(t, step.Result, execResults(f.model.Requests()[len(f.model.Requests())-1])[0].Content)
		})
	})
}

func TestExecute_EachBudgetStopsTheRunWithItsReason(t *testing.T) {
	t.Run("time, on the clock, by a tool that takes too long", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var f *execFixture
			calls := &execCalls{}
			def := execClerk(calls.tool("slow", func(context.Context, agent.Invocation) (string, error) {
				f.clock.Advance(2 * time.Minute)
				return "at last", nil
			}))
			def.Limits = agent.Limits{MaxDuration: time.Minute}
			f = newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "slow", `{}`)),
					agenttest.Say("never said"),
				),
			})
			started := f.start("clerk", "take your time")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonTimeBudget, got.Reason)
			assert.Empty(t, got.Output)
			assert.Equal(t, int64(120_000), got.ActiveMillis)
			assert.Equal(t, []string{"1 model completed", "2 slow completed"}, f.journal(started.ID),
				"the step that passed the limit is recorded, and no other is begun")
			assert.Len(t, f.model.Requests(), 1)
		})
	})

	t.Run("cost", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			def := execClerk(calls.tool("lookup", nil))
			def.Limits = agent.Limits{MaxCostMicros: 100}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				script: agenttest.Replies(
					execSpent(agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)), agent.Usage{CostMicros: 100}),
					agenttest.Say("never said"),
				),
			})
			started := f.start("clerk", "is order 7 paid?")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonCostBudget, got.Reason)
			assert.Equal(t, []string{"1 model completed", "2 lookup proposed"}, f.journal(started.ID))
			assert.Empty(t, calls.of("lookup"), "no work is done once the budget is spent")
		})
	})

	t.Run("tokens", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			def := execClerk(calls.tool("lookup", nil))
			def.Limits = agent.Limits{MaxTokens: 50}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				script: agenttest.Replies(
					execSpent(agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)), agent.Usage{InputTokens: 30, OutputTokens: 20}),
					agenttest.Say("never said"),
				),
			})
			started := f.start("clerk", "is order 7 paid?")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonTokenBudget, got.Reason)
			assert.Empty(t, calls.of("lookup"))
		})
	})

	t.Run("model calls", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			def := execClerk(calls.tool("lookup", nil))
			def.Limits = agent.Limits{MaxModelCalls: 2}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)),
					agenttest.Use(agenttest.Call("call-2", "lookup", `{}`)),
					agenttest.Say("never said"),
				),
			})
			started := f.start("clerk", "is order 7 paid?")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonModelCalls, got.Reason)
			assert.Equal(t, 2, got.ModelCalls)
			assert.Len(t, f.model.Requests(), 2)
			assert.Len(t, calls.of("lookup"), 2, "the limit on model calls stops a model call and nothing else")
			assert.Contains(t, f.events(), "run.failed")
		})
	})
}

func TestExecute_AReplysStop(t *testing.T) {
	refusal := agent.Response{Message: agent.Message{Role: agent.RoleAssistant}, Stop: agent.StopRefusal}
	cut := agenttest.Use(agenttest.Call("call-1", "lookup", `{"order":7}`))
	cut.Stop = agent.StopMaxTokens
	full := agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Text: "so far"}, Stop: agent.StopContextWindow}

	for _, tc := range []struct {
		name    string
		reply   agent.Response
		reason  string
		journal []string
	}{
		{"a refusal fails the run", refusal, agent.ReasonRefusal, []string{"1 model completed"}},
		{"a reply cut at its token limit fails the run and its calls are left proposed", cut, agent.ReasonTruncated,
			[]string{"1 model completed", "2 lookup proposed"}},
		{"a reply out of context window fails the run", full, agent.ReasonContextWindow, []string{"1 model completed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				guard := &execGuard{}
				f := newExecFixture(t, execConfig{
					defs:   []agent.Definition{execClerk(calls.tool("lookup", nil))},
					guard:  guard,
					script: agenttest.Replies(tc.reply, agenttest.Say("never said")),
				})
				started := f.start("clerk", "is order 7 paid?")

				got := f.execute(started.ID)

				assert.Equal(t, agent.StatusFailed, got.Status)
				assert.Equal(t, tc.reason, got.Reason)
				assert.Empty(t, got.Output)
				assert.Equal(t, tc.journal, f.journal(started.ID))
				assert.Len(t, f.model.Requests(), 1)
				assert.Empty(t, calls.of("lookup"), "its calls are never run")
				assert.Empty(t, guard.questions(), "or judged")
			})
		})
	}

	t.Run("a pause calls the model again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			paused := agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Text: "working"}, Stop: agent.StopPause}
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execClerk()},
				script: agenttest.Replies(paused, agenttest.Say("order 7 is paid")),
			})
			started := f.start("clerk", "is order 7 paid?")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, "order 7 is paid", got.Output)
			assert.Equal(t, []string{"1 model completed", "2 model completed"}, f.journal(started.ID))
			requests := f.model.Requests()
			require.Len(t, requests, 2)
			assert.Equal(t, []agent.Message{
				{Role: agent.RoleUser, Text: "is order 7 paid?"},
				{Role: agent.RoleAssistant, Text: "working"},
			}, requests[1].Messages)
		})
	})
}

func TestExecute_AGuardErrorEndsTheExecutionFailedAndTheStepIsStillProposed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		guard := &execGuard{}
		guard.failWith(errors.New("policy store is down"))
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
				agenttest.Say("refunded"),
			),
		})
		started := f.start("clerk", "refund order 7")

		got, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorContains(t, err, "policy store is down")
		assert.Equal(t, agent.StatusRunnable, got.Status)
		assert.Equal(t, 1, got.Failures)
		assert.Contains(t, got.Error, "policy store is down")
		assert.Empty(t, got.LeaseOwner)
		require.NotNil(t, got.NextAttemptAt)
		assert.Equal(t, []string{"1 model completed", "2 refund proposed"}, f.journal(started.ID),
			"an error is read neither as allow nor as block")
		step := f.steps(started.ID)[1]
		assert.Empty(t, step.Decision)
		assert.Empty(t, step.Result)
		assert.Empty(t, calls.of("refund"))

		// The guard comes back, and the next execution asks it again.
		guard.failWith(nil)
		f.clock.Advance(time.Second)
		got = f.execute(started.ID)
		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Len(t, calls.of("refund"), 1)
		assert.Len(t, guard.questions(), 2)
	})
}

func TestExecute_AModelError(t *testing.T) {
	t.Run("one that is permanent fails the run at once", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asked := 0
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk()},
				model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
					asked++
					return agent.Response{}, fmt.Errorf("the budget is spent: %w", agent.ErrPermanent)
				}),
			})
			started := f.start("clerk", "is order 7 paid?")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonError, got.Reason)
			assert.Equal(t, "the budget is spent: agent: permanent failure", got.Error)
			assert.Zero(t, got.Failures, "it is the run that failed, not an execution of it")
			assert.Equal(t, 1, asked)
			assert.Equal(t, []string{"1 model started"}, f.journal(started.ID))
			assert.Equal(t, []string{"run.started", "step.started 1", "run.failed"}, f.events())
		})
	})

	t.Run("any other yields with a wait that doubles, and at MaxFailures fails the run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asked := 0
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk()},
				model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
					asked++
					return agent.Response{}, fmt.Errorf("rate limited, try %d", asked)
				}),
			})
			started := f.start("clerk", "is order 7 paid?")

			for failure, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
				got, err := f.engine.Execute(t.Context(), started.ID)

				require.ErrorContains(t, err, fmt.Sprintf("rate limited, try %d", failure+1))
				assert.Equal(t, agent.StatusRunnable, got.Status)
				assert.Equal(t, failure+1, got.Failures)
				assert.Equal(t, fmt.Sprintf("rate limited, try %d", failure+1), got.Error)
				assert.Empty(t, got.LeaseOwner)
				require.NotNil(t, got.NextAttemptAt)
				assert.Equal(t, f.clock.Now().Add(wait), *got.NextAttemptAt, "after failure %d", failure+1)

				f.clock.Advance(wait - time.Nanosecond)
				_, err = f.engine.Execute(t.Context(), started.ID)
				require.ErrorIs(t, err, agent.ErrNotClaimable, "the run is hidden until its wait is over")
				f.clock.Advance(time.Nanosecond)
			}

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status, "failure number MaxFailures ends the run")
			assert.Equal(t, agent.ReasonError, got.Reason)
			assert.Equal(t, "rate limited, try 5", got.Error)
			assert.Equal(t, 5, asked)
			steps := f.steps(started.ID)
			require.Len(t, steps, 1)
			assert.Equal(t, agent.StepStarted, steps[0].Status)
			assert.Equal(t, 5, steps[0].Attempts, "each execution made the interrupted call again")
		})
	})

	t.Run("the wait is capped at RetryMax", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk()},
				model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
					return agent.Response{}, errors.New("rate limited")
				}),
				tune: func(o *agent.Options) {
					o.RetryBase = 2 * time.Second
					o.RetryMax = 5 * time.Second
					o.MaxFailures = 6
				},
			})
			started := f.start("clerk", "is order 7 paid?")

			for failure, wait := range []time.Duration{2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second} {
				got, err := f.engine.Execute(t.Context(), started.ID)
				require.Error(t, err)
				require.NotNil(t, got.NextAttemptAt)
				assert.Equal(t, f.clock.Now().Add(wait), *got.NextAttemptAt, "after failure %d", failure+1)
				f.clock.Advance(wait)
			}
			assert.Equal(t, agent.StatusFailed, f.execute(started.ID).Status)
		})
	})
}

// execLead is an agent named lead that hands documents to the agent named
// reviewer, and execReviewer the agent it hands them to.
func execLead() agent.Definition {
	return agent.Definition{
		Name: "lead", System: "hand each document on",
		Tools: []agent.Tool{{Name: "review", Delegate: "reviewer"}},
	}
}

func execReviewer() agent.Definition {
	return agent.Definition{Name: "reviewer", System: "summarise the document"}
}

// execLeadScript has the lead delegate three documents in one reply and then
// say so, and each reviewer answer with a summary of what it was given.
func execLeadScript(reviewer agenttest.Script) agenttest.Script {
	return agenttest.ByAgent(map[string]agenttest.Script{
		"lead": agenttest.Replies(
			execSpent(agenttest.Use(
				agenttest.Call("call-1", "review", `{"doc":1}`),
				agenttest.Call("call-2", "review", `{"doc":2}`),
				agenttest.Call("call-3", "review", `{"doc":3}`),
			), agent.Usage{InputTokens: 100, OutputTokens: 20, CostMicros: 400}),
			execSpent(agenttest.Say("all three are reviewed"), agent.Usage{InputTokens: 150, OutputTokens: 10, CostMicros: 500}),
		),
		"reviewer": reviewer,
	})
}

func execSummaries(req agent.Request, _ int) (agent.Response, error) {
	return execSpent(agenttest.Say("summary of "+req.Messages[0].Text), agent.Usage{InputTokens: 10, OutputTokens: 5, CostMicros: 7}), nil
}

func TestExecute_ChildRuns(t *testing.T) {
	t.Run("three delegations in one reply are all created before the parent parks", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(execSummaries),
			})
			started := f.start("lead", "review the batch")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusWaiting, got.Status)
			assert.Equal(t, agent.ReasonChildren, got.Reason)
			assert.Empty(t, got.LeaseOwner)
			assert.Equal(t, []string{"1 model completed", "2 review waiting", "3 review waiting", "4 review waiting"},
				f.journal(started.ID))

			steps := f.steps(started.ID)
			children := f.children(started.ID)
			require.Len(t, children, 3)
			byID := map[string]agent.Run{}
			for _, child := range children {
				byID[child.ID] = child
			}
			for i, st := range steps[1:] {
				child, ok := byID[st.ChildRunID]
				require.True(t, ok, "step %d names a child that exists", st.Seq)
				assert.Equal(t, "reviewer", child.Agent)
				assert.Equal(t, agent.StatusRunnable, child.Status)
				assert.Equal(t, fmt.Sprintf(`{"doc":%d}`, i+1), child.Input, "its input is the call's arguments")
				assert.Equal(t, started.ID, child.ParentID)
				assert.Equal(t, st.Seq, child.ParentSeq)
				assert.Equal(t, 1, child.Depth)
				assert.Equal(t, agent.StepKey(started.ID, st.Seq), child.Key, "its key is the step's")
				assert.Equal(t, "summarise the document", child.Definition.System, "it starts with its own agent's snapshot")
				assert.Equal(t, 1, st.Attempts)
				assert.Equal(t, agent.Allow, st.Decision)
			}
			assert.Len(t, f.model.Requests(), 1, "no child has run yet")
		})
	})

	t.Run("executing the children and then the parent collects their outputs and their usage", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(execSummaries),
			})
			started := f.start("lead", "review the batch")
			f.execute(started.ID)
			for _, st := range f.steps(started.ID)[1:] {
				child := f.execute(st.ChildRunID)
				require.Equal(t, agent.StatusCompleted, child.Status)
			}
			require.Equal(t, agent.StatusRunnable, f.run(started.ID).Status, "a child that ends wakes its parent")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, "all three are reviewed", got.Output)
			steps := f.steps(started.ID)
			for i, st := range steps[1:4] {
				assert.Equal(t, agent.StepCompleted, st.Status, "step %d", st.Seq)
				assert.Equal(t, fmt.Sprintf(`summary of {"doc":%d}`, i+1), st.Result, "step %d", st.Seq)
				assert.False(t, st.IsError)
				assert.Equal(t, agent.Usage{InputTokens: 10, OutputTokens: 5, CostMicros: 7}, st.Usage,
					"the child's usage is on the step that started it")
			}
			assert.Equal(t, agent.Usage{InputTokens: 280, OutputTokens: 45, CostMicros: 921}, got.Usage,
				"the parent's own two replies and its three children")
			assert.Equal(t, 2, got.ModelCalls, "a child's model calls are its own")

			var last agent.Request
			for _, req := range f.model.Requests() {
				if req.RunID == started.ID {
					last = req
				}
			}
			assert.Equal(t, []agent.Result{
				{CallID: "call-1", Content: `summary of {"doc":1}`},
				{CallID: "call-2", Content: `summary of {"doc":2}`},
				{CallID: "call-3", Content: `summary of {"doc":3}`},
			}, execResults(last))
		})
	})

	t.Run("a parent executed with one child still running collects the others and parks again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(execSummaries),
			})
			started := f.start("lead", "review the batch")
			f.execute(started.ID)
			steps := f.steps(started.ID)
			f.execute(steps[1].ChildRunID)
			f.execute(steps[3].ChildRunID)

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusWaiting, got.Status)
			assert.Equal(t, agent.ReasonChildren, got.Reason)
			assert.Equal(t, []string{"1 model completed", "2 review completed", "3 review waiting", "4 review completed"},
				f.journal(started.ID))
		})
	})

	t.Run("a failed child is an error result", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			refuses := agenttest.Replies(agent.Response{
				Message: agent.Message{Role: agent.RoleAssistant}, Stop: agent.StopRefusal,
				Usage: agent.Usage{InputTokens: 10, CostMicros: 3},
			})
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(refuses),
			})
			started := f.start("lead", "review the batch")
			f.execute(started.ID)
			for _, st := range f.steps(started.ID)[1:] {
				require.Equal(t, agent.StatusFailed, f.execute(st.ChildRunID).Status)
			}

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status, "the model is told, and the run goes on")
			for _, st := range f.steps(started.ID)[1:4] {
				assert.Equal(t, agent.StepCompleted, st.Status)
				assert.Equal(t, "reviewer failed: refusal", st.Result)
				assert.True(t, st.IsError)
				assert.Equal(t, agent.Usage{InputTokens: 10, CostMicros: 3}, st.Usage, "a failed child's usage counts too")
			}
		})
	})

	t.Run("a child's cost limit is the smaller of its own and the parent's remainder", func(t *testing.T) {
		for _, tc := range []struct {
			name           string
			parent, child  int64
			want           int64
			parentDuration time.Duration
		}{
			{name: "the parent's remainder when the child has no limit", parent: 1000, child: 0, want: 600},
			{name: "the parent's remainder when it is the smaller", parent: 1000, child: 900, want: 600},
			{name: "the child's own when it is the smaller", parent: 1000, child: 250, want: 250},
			{name: "the child's own when the parent has no limit", parent: -1, child: 250, want: 250},
			{name: "none when neither has one", parent: 0, child: -1, want: -1},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					lead, reviewer := execLead(), execReviewer()
					lead.Limits = agent.Limits{MaxCostMicros: tc.parent}
					reviewer.Limits = agent.Limits{MaxCostMicros: tc.child, MaxModelCalls: 9}
					f := newExecFixture(t, execConfig{
						defs:   []agent.Definition{lead, reviewer},
						script: execLeadScript(execSummaries),
					})
					started := f.start("lead", "review the batch")

					f.execute(started.ID)

					children := f.children(started.ID)
					require.Len(t, children, 3)
					for _, child := range children {
						assert.Equal(t, tc.want, child.Definition.Limits.MaxCostMicros)
						assert.Equal(t, 9, child.Definition.Limits.MaxModelCalls, "its other limits are its agent's")
						assert.Equal(t, 15*time.Minute, child.Definition.Limits.MaxDuration)
					}
				})
			})
		}
	})

	t.Run("a delegation past MaxDepth is an error result", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			digger := agent.Definition{Name: "digger", Tools: []agent.Tool{{Name: "dig", Delegate: "digger"}}}
			script := func(req agent.Request, turn int) (agent.Response, error) {
				if turn == 0 {
					return agenttest.Use(agenttest.Call("call-1", "dig", `{}`)), nil
				}
				return agenttest.Say("below me: " + execResults(req)[0].Content), nil
			}
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{digger},
				script: script,
				tune:   func(o *agent.Options) { o.MaxDepth = 2 },
			})
			root := f.start("digger", "dig")

			f.execute(root.ID)
			first := f.steps(root.ID)[1].ChildRunID
			f.execute(first)
			second := f.steps(first)[1].ChildRunID
			deepest := f.execute(second)

			assert.Equal(t, 2, deepest.Depth)
			assert.Equal(t, agent.StatusCompleted, deepest.Status, "the run that may not delegate is told so and goes on")
			step := f.steps(second)[1]
			assert.Equal(t, agent.StepCompleted, step.Status)
			assert.Equal(t, "delegation refused: the depth limit of 2 is reached", step.Result)
			assert.True(t, step.IsError)
			assert.Empty(t, step.ChildRunID)
			assert.Empty(t, f.children(second), "no run is created past the limit")

			f.execute(first)
			got := f.execute(root.ID)
			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t,
				"below me: below me: below me: delegation refused: the depth limit of 2 is reached", got.Output)
		})
	})

	t.Run("a delegation to an agent that is not registered is an error result", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			lead := agent.Definition{Name: "lead", Tools: []agent.Tool{{Name: "review", Delegate: "nobody"}}}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{lead},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "review", `{"doc":1}`)),
					agenttest.Say("nobody can review it"),
				),
			})
			started := f.start("lead", "review the batch")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			step := f.steps(started.ID)[1]
			assert.Equal(t, agent.StepCompleted, step.Status)
			assert.Equal(t, `delegation refused: agent "nobody" is not registered`, step.Result)
			assert.True(t, step.IsError)
			assert.Empty(t, f.children(started.ID))
		})
	})

	t.Run("a delegation is put to the guard as a delegate action", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			guard := &execGuard{answers: map[string]agent.Decision{"reviewer": {Effect: agent.Ask, Rule: "ask-to-delegate"}}}
			f := newExecFixture(t, execConfig{
				defs:  []agent.Definition{execLead(), execReviewer()},
				guard: guard,
				script: agenttest.ByAgent(map[string]agenttest.Script{
					"lead": agenttest.Replies(
						agenttest.Use(agenttest.Call("call-1", "review", `{"doc":1}`)),
						agenttest.Say("reviewed"),
					),
					"reviewer": execSummaries,
				}),
			})
			started := f.start("lead", "review the batch")

			got := f.execute(started.ID)

			assert.Equal(t, agent.ReasonApproval, got.Reason)
			require.Len(t, guard.questions(), 1)
			assert.Equal(t, "delegate", guard.questions()[0].Kind)
			assert.Equal(t, "reviewer", guard.questions()[0].Target)
			assert.Empty(t, f.children(started.ID), "no child is started before a person says yes")

			// Approved, the step is started and its child with it.
			_, err := f.engine.Approve(t.Context(), f.approvals(started.ID)[0].ID, "ops@example.test", "")
			require.NoError(t, err)
			got = f.execute(started.ID)
			assert.Equal(t, agent.StatusWaiting, got.Status)
			assert.Equal(t, agent.ReasonChildren, got.Reason)
			step := f.steps(started.ID)[1]
			require.NotEmpty(t, step.ChildRunID)
			f.execute(step.ChildRunID)
			got = f.execute(started.ID)
			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, `summary of {"doc":1}`, f.steps(started.ID)[1].Result)
		})
	})
}

func TestExecute_Cancellation(t *testing.T) {
	t.Run("a waiting run is cancelled on its next execution, with its approval and its children", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			lead := execLead()
			lead.Tools = append(lead.Tools, calls.tool("send", nil))
			f := newExecFixture(t, execConfig{
				defs:  []agent.Definition{lead, execReviewer()},
				guard: &execGuard{answers: map[string]agent.Decision{"send": {Effect: agent.Ask, Rule: "ask-first"}}},
				script: agenttest.ByAgent(map[string]agenttest.Script{
					"lead": agenttest.Replies(
						agenttest.Use(
							agenttest.Call("call-1", "send", `{"to":"all"}`),
							agenttest.Call("call-2", "review", `{"doc":1}`),
							agenttest.Call("call-3", "review", `{"doc":2}`),
						),
						agenttest.Say("never said"),
					),
					"reviewer": execSummaries,
				}),
			})
			started := f.start("lead", "review the batch")
			require.Equal(t, agent.StatusWaiting, f.execute(started.ID).Status)
			steps := f.steps(started.ID)
			// One child ends before the request, and is collected; it is not
			// asked to stop.
			done := f.execute(steps[2].ChildRunID)
			require.Equal(t, agent.StatusCompleted, done.Status)
			require.Equal(t, agent.StatusWaiting, f.execute(started.ID).Status)
			f.bus.forget()
			before := f.journal(started.ID)
			require.Equal(t, []string{"1 model completed", "2 send waiting", "3 review completed", "4 review waiting"}, before)

			require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))
			require.Equal(t, agent.StatusRunnable, f.run(started.ID).Status, "the request wakes the run")
			f.clock.Advance(time.Minute)
			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCancelled, got.Status)
			assert.Equal(t, agent.ReasonCancelled, got.Reason)
			assert.Empty(t, got.LeaseOwner)
			require.NotNil(t, got.FinishedAt)
			assert.Equal(t, before, f.journal(started.ID), "no step is written for a run that is cancelled")
			assert.Empty(t, calls.of("send"))

			approvals := f.approvals(started.ID)
			require.Len(t, approvals, 1)
			assert.Equal(t, agent.ApprovalCancelled, approvals[0].Status)

			running := f.run(steps[3].ChildRunID)
			assert.True(t, running.CancelRequested, "a child that has not ended is asked to stop")
			assert.Equal(t, "ops@example.test", running.CancelBy)
			assert.Equal(t, fmt.Sprintf("parent run %s was cancelled: wrong batch", started.ID), running.CancelReason)
			assert.Equal(t, agent.StatusRunnable, running.Status)
			assert.Equal(t, done, f.run(steps[2].ChildRunID), "one that has ended is left as it is")

			assert.Equal(t, []string{"run.cancel_requested", "run.cancel_requested", "run.cancelled"}, f.events(),
				"the request, the request passed to the child, and the end")

			child := f.execute(steps[3].ChildRunID)
			assert.Equal(t, agent.StatusCancelled, child.Status)
			assert.Equal(t, agent.StatusCancelled, f.run(started.ID).Status, "the child ending does not wake a run that has ended")
		})
	})

	t.Run("a run being executed stops through the heartbeat and no later step is written", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			began, stopped := make(chan struct{}), make(chan error, 1)
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("slow", func(ctx context.Context, _ agent.Invocation) (string, error) {
					close(began)
					<-ctx.Done()
					stopped <- context.Cause(ctx)
					return "too late", nil
				}))},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "slow", `{}`), agenttest.Call("call-2", "slow", `{}`)),
					agenttest.Say("never said"),
				),
			})
			started := f.start("clerk", "take your time")
			outcome := f.begin(t.Context(), started.ID)
			<-began

			require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "enough"))
			synctest.Wait()
			require.Equal(t, agent.StatusRunnable, f.run(started.ID).Status, "nothing stops before the heartbeat")
			require.Equal(t, "worker-1", f.run(started.ID).LeaseOwner)
			f.pass(execHeartbeat)

			got := <-outcome
			require.NoError(t, got.err)
			assert.Equal(t, agent.StatusCancelled, got.run.Status)
			assert.Equal(t, agent.ReasonCancelled, got.run.Reason)
			assert.Empty(t, got.run.LeaseOwner)
			assert.Same(t, agent.ErrCancelRequested, <-stopped, "the step in flight is stopped, and told why")
			assert.Equal(t, []string{"1 model completed", "2 slow started", "3 slow proposed"}, f.journal(started.ID),
				"the step in flight is abandoned and no later step is written")
			assert.Len(t, calls.of("slow"), 1)
			assert.Equal(t, "run.cancelled", f.events()[len(f.events())-1])
		})
	})
}

func TestExecute_OnARunAnotherEngineHoldsReturnsErrNotClaimable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		began, release := make(chan struct{}), make(chan struct{})
		tools := func() agent.Definition {
			return execClerk(calls.tool("slow", func(context.Context, agent.Invocation) (string, error) {
				close(began)
				<-release
				return "done", nil
			}))
		}
		script := agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "slow", `{}`)), agenttest.Say("done"))
		first := newExecFixture(t, execConfig{defs: []agent.Definition{tools()}, script: script})
		second := first.rival(execConfig{defs: []agent.Definition{tools()}})
		started := first.start("clerk", "take your time")
		outcome := first.begin(t.Context(), started.ID)
		<-began

		got, err := second.engine.Execute(t.Context(), started.ID)

		require.ErrorIs(t, err, agent.ErrNotClaimable)
		assert.Zero(t, got)
		assert.Equal(t, "worker-1", first.run(started.ID).LeaseOwner)

		close(release)
		ended := <-outcome
		require.NoError(t, ended.err)
		assert.Equal(t, agent.StatusCompleted, ended.run.Status)
		assert.Len(t, calls.of("slow"), 1)
	})
}

func TestExecute_RefusesWhatItCannotClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk()},
			script: agenttest.Replies(agenttest.Say("done")),
		})
		stranger := f.rival(execConfig{defs: []agent.Definition{{Name: "other"}}})
		started := f.start("clerk", "hello")

		_, err := f.engine.Execute(t.Context(), execID(9, 9))
		require.ErrorIs(t, err, agent.ErrNotFound, "a run that does not exist")

		_, err = stranger.engine.Execute(t.Context(), started.ID)
		require.ErrorIs(t, err, agent.ErrNotClaimable, "a run of an agent this process has not registered")
		assert.Empty(t, f.journal(started.ID))

		ended := f.execute(started.ID)
		_, err = f.engine.Execute(t.Context(), started.ID)
		require.ErrorIs(t, err, agent.ErrNotClaimable, "a run that has ended")
		assert.Equal(t, ended, f.run(started.ID))
	})
}

func TestExecute_Events(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
				agenttest.Say("the refund is settled"),
			),
		})
		started := f.start("clerk", "refund order 7")
		f.clock.Advance(time.Second)
		f.execute(started.ID)
		f.clock.Advance(time.Second)
		_, err := f.engine.Approve(t.Context(), f.approvals(started.ID)[0].ID, "ops@example.test", "")
		require.NoError(t, err)
		f.clock.Advance(time.Second)
		f.execute(started.ID)

		assert.Equal(t, []string{
			"run.started",
			"step.started 1",
			"step.completed 1",
			"approval.requested 2",
			"run.waiting",
			"approval.decided 2",
			"step.started 2",
			"step.completed 2",
			"step.started 3",
			"step.completed 3",
			"run.completed",
		}, f.events())

		at := map[string]time.Time{}
		for _, published := range f.bus.published() {
			event := published.(agent.Event)
			assert.Equal(t, started.ID, event.RunID)
			assert.Equal(t, "clerk", event.Agent)
			at[event.Type] = event.At
		}
		assert.Equal(t, execStart.Add(time.Second), at[agent.EventRunWaiting], "an event carries the time of its write")
		assert.Equal(t, execStart.Add(3*time.Second), at[agent.EventRunCompleted])
		for _, topic := range f.bus.topics {
			assert.Equal(t, agent.TopicRuns, topic)
		}
	})
}

func TestExecute_APublisherThatFailsOrPanicsChangesNothing(t *testing.T) {
	for name, breakBus := range map[string]func(*engineBus){
		"fails":  func(b *engineBus) { b.fail(errors.New("bus is down")) },
		"panics": func(b *engineBus) { b.panicWith("bus exploded") },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk(calls.tool("lookup", nil))},
					script: agenttest.Replies(
						agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)),
						agenttest.Say("done"),
					),
				})
				started := f.start("clerk", "hello")
				breakBus(f.bus)

				got := f.execute(started.ID)

				assert.Equal(t, agent.StatusCompleted, got.Status)
				assert.Zero(t, got.Failures)
				assert.Len(t, calls.of("lookup"), 1)
			})
		})
	}
}

// execHooked is a store a test reaches into: it can change what the engine
// reads, act just before a call reaches the store, and refuse one.
type execHooked struct {
	agent.Store

	mu sync.Mutex
	// changes edits what Changes returns.
	changes func(*agent.Changes)
	// beforePark runs as Park arrives, before it reaches the store.
	beforePark func()
	// parkRefused makes Park report that there is nothing to wait for,
	// without reaching the store.
	parkRefused bool
	parks       int
	// claim, when it returns an error for the nth claim, fails that claim.
	claim  func(n int) error
	claims int
}

func (s *execHooked) Changes(ctx context.Context, runID string, since int64) (agent.Changes, error) {
	changes, err := s.Store.Changes(ctx, runID, since)
	s.mu.Lock()
	edit := s.changes
	s.mu.Unlock()
	if err == nil && edit != nil {
		edit(&changes)
	}
	return changes, err
}

func (s *execHooked) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	s.mu.Lock()
	s.parks++
	before, refused := s.beforePark, s.parkRefused
	s.mu.Unlock()
	if before != nil {
		before()
	}
	if refused {
		return false, nil
	}
	return s.Store.Park(ctx, lease, req)
}

func (s *execHooked) Claim(ctx context.Context, req agent.ClaimRequest) (*agent.Run, error) {
	s.mu.Lock()
	s.claims++
	n, claim := s.claims, s.claim
	s.mu.Unlock()
	if claim != nil {
		if err := claim(n); err != nil {
			return nil, err
		}
	}
	return s.Store.Claim(ctx, req)
}

// hooked returns a store wrapper for execConfig.over that keeps the wrapper
// where the test can reach it.
func hooked(into **execHooked) func(agent.Store) agent.Store {
	return func(inner agent.Store) agent.Store {
		*into = &execHooked{Store: inner}
		return *into
	}
}
