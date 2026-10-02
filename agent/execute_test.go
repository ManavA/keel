package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

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
	// lax gives the engine a store that takes a call under a context that
	// has ended, as MemoryStore does and a database driver does not.
	lax bool
}

// execFixture is one engine, as one process is: over a store that can be
// killed and that refuses a call under a context that has ended, a clock the
// test moves, and a bus and a log that keep what they are given. Its rivals
// are other engines over the same store and clock.
type execFixture struct {
	t      *testing.T
	engine *agent.Engine
	memory *agent.MemoryStore
	faults *agenttest.FaultStore
	// wire is what the engine is given, around faults and under whatever a
	// test wraps it in.
	wire  *execWire
	clock *agenttest.Clock
	bus   *engineBus
	logs  *engineLogs
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
	f.wire = &execWire{inner: f.faults, lax: cfg.lax}
	var store agent.Store = f.wire
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

	t.Run("a RetryBase above RetryMax waits RetryMax", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk()},
				model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
					return agent.Response{}, errors.New("rate limited")
				}),
				tune: func(o *agent.Options) {
					o.RetryBase = 10 * time.Second
					o.RetryMax = 5 * time.Second
				},
			})
			started := f.start("clerk", "is order 7 paid?")

			for failure := 1; failure <= 2; failure++ {
				got, err := f.engine.Execute(t.Context(), started.ID)
				require.Error(t, err)
				require.NotNil(t, got.NextAttemptAt)
				assert.Equal(t, f.clock.Now().Add(5*time.Second), *got.NextAttemptAt, "after failure %d", failure)
				f.clock.Advance(5 * time.Second)
			}
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

			announced := map[string]bool{}
			for _, published := range f.bus.published() {
				if event := published.(agent.Event); event.Type == agent.EventRunStarted {
					assert.False(t, announced[event.RunID], "run %s is announced once", event.RunID)
					announced[event.RunID] = true
					if child, ok := byID[event.RunID]; ok {
						assert.Equal(t, "reviewer", event.Agent)
						assert.Equal(t, child.CreatedAt, event.At)
					}
				}
			}
			assert.Len(t, announced, 4, "the run and each child it started")
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
			read := f.wire.made("GetRun")

			got := f.execute(started.ID)

			assert.Equal(t, 3+2+1+1, f.wire.made("GetRun")-read,
				"each turn reads the children that waiting steps name and no others, and Execute reads the run back")

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

// errWire is what an execWire returns for the one call it is told to fail.
var errWire = errors.New("the store failed this one call")

// execWire is a store as a process reaches one over a wire, and every engine
// of a fixture is given one. It refuses a call whose context has ended, as a
// database driver does and MemoryStore does not, so a test fails when the
// engine makes a call under a context that is dead. It can fail exactly one
// call, the nth to arrive, before it reaches the store or after the store has
// answered it, and go on working: a failure the process outlives, at any call
// a test chooses. And it can hold a call of a named method up, or act once
// the store has answered one.
type execWire struct {
	inner agent.Store

	// lax makes it take a call under a context that has ended, as
	// MemoryStore does.
	lax bool

	mu    sync.Mutex
	calls int
	// asked counts the calls by the name of their method.
	asked map[string]int
	// at is the call to fail, counted from 1, and none for zero. after says
	// the store is reached first.
	at    int
	after bool
	// holds are run, by the name of a Store method, as a call of it arrives,
	// with the call's context. thens are run once the store has answered.
	holds map[string]func(ctx context.Context)
	thens map[string]func()
	// spawns counts the child runs the store was asked to create.
	spawns int
}

// failAt makes the nth call fail: before it reaches the store, or after the
// store has answered it.
func (s *execWire) failAt(n int, after bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.at, s.after = n, after
}

// hold sets what runs as a call of op arrives, and none for nil.
func (s *execWire) hold(op string, run func(ctx context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == nil {
		s.holds = map[string]func(ctx context.Context){}
	}
	s.holds[op] = run
}

// then sets what runs once the store has answered a call of op.
func (s *execWire) then(op string, run func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.thens == nil {
		s.thens = map[string]func(){}
	}
	s.thens[op] = run
}

func (s *execWire) count() (calls, spawns int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.spawns
}

// made is how many calls of op have arrived, refused ones included.
func (s *execWire) made(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked[op]
}

// total is how many calls have arrived, refused ones included.
func (s *execWire) total() int {
	calls, _ := s.count()
	return calls
}

func wired[T any](ctx context.Context, s *execWire, op string, call func() (T, error)) (T, error) {
	var zero T
	s.mu.Lock()
	s.calls++
	if s.asked == nil {
		s.asked = map[string]int{}
	}
	s.asked[op]++
	due, after := s.calls == s.at, s.after
	hold, then := s.holds[op], s.thens[op]
	s.mu.Unlock()
	if hold != nil {
		hold(ctx)
	}
	if err := ctx.Err(); err != nil && !s.lax {
		return zero, err
	}
	if due && !after {
		return zero, errWire
	}
	got, err := call()
	if then != nil {
		then()
	}
	if due {
		return zero, errWire
	}
	return got, err
}

func wiredErr(ctx context.Context, s *execWire, op string, call func() error) error {
	_, err := wired(ctx, s, op, func() (struct{}, error) { return struct{}{}, call() })
	return err
}

func (s *execWire) CreateRun(ctx context.Context, run agent.Run) (agent.Run, bool, error) {
	type result struct {
		run     agent.Run
		created bool
	}
	if run.ParentID != "" {
		s.mu.Lock()
		s.spawns++
		s.mu.Unlock()
	}
	got, err := wired(ctx, s, "CreateRun", func() (result, error) {
		stored, created, err := s.inner.CreateRun(ctx, run)
		return result{stored, created}, err
	})
	return got.run, got.created, err
}

func (s *execWire) GetRun(ctx context.Context, id string) (agent.Run, error) {
	return wired(ctx, s, "GetRun", func() (agent.Run, error) { return s.inner.GetRun(ctx, id) })
}

func (s *execWire) ListRuns(ctx context.Context, f agent.RunFilter) ([]agent.Run, error) {
	return wired(ctx, s, "ListRuns", func() ([]agent.Run, error) { return s.inner.ListRuns(ctx, f) })
}

func (s *execWire) Claim(ctx context.Context, req agent.ClaimRequest) (*agent.Run, error) {
	return wired(ctx, s, "Claim", func() (*agent.Run, error) { return s.inner.Claim(ctx, req) })
}

func (s *execWire) Heartbeat(ctx context.Context, lease agent.Lease, now time.Time, ttl time.Duration) (bool, error) {
	return wired(ctx, s, "Heartbeat", func() (bool, error) { return s.inner.Heartbeat(ctx, lease, now, ttl) })
}

func (s *execWire) Yield(ctx context.Context, lease agent.Lease, req agent.YieldRequest) error {
	return wiredErr(ctx, s, "Yield", func() error { return s.inner.Yield(ctx, lease, req) })
}

func (s *execWire) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	return wired(ctx, s, "Park", func() (bool, error) { return s.inner.Park(ctx, lease, req) })
}

func (s *execWire) Finish(ctx context.Context, lease agent.Lease, req agent.FinishRequest) error {
	return wiredErr(ctx, s, "Finish", func() error { return s.inner.Finish(ctx, lease, req) })
}

func (s *execWire) Steps(ctx context.Context, runID string) ([]agent.Step, error) {
	return wired(ctx, s, "Steps", func() ([]agent.Step, error) { return s.inner.Steps(ctx, runID) })
}

func (s *execWire) BeginModel(ctx context.Context, lease agent.Lease, seq int, now time.Time) error {
	return wiredErr(ctx, s, "BeginModel", func() error { return s.inner.BeginModel(ctx, lease, seq, now) })
}

func (s *execWire) CompleteModel(ctx context.Context, lease agent.Lease, req agent.CompleteModelRequest) error {
	return wiredErr(ctx, s, "CompleteModel", func() error { return s.inner.CompleteModel(ctx, lease, req) })
}

func (s *execWire) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	return wiredErr(ctx, s, "UpdateStep", func() error { return s.inner.UpdateStep(ctx, lease, req) })
}

func (s *execWire) RequestApproval(ctx context.Context, lease agent.Lease, req agent.ApprovalRequest) (agent.Approval, error) {
	return wired(ctx, s, "RequestApproval", func() (agent.Approval, error) { return s.inner.RequestApproval(ctx, lease, req) })
}

func (s *execWire) GetApproval(ctx context.Context, id string) (agent.Approval, error) {
	return wired(ctx, s, "GetApproval", func() (agent.Approval, error) { return s.inner.GetApproval(ctx, id) })
}

func (s *execWire) ListApprovals(ctx context.Context, f agent.ApprovalFilter) ([]agent.Approval, error) {
	return wired(ctx, s, "ListApprovals", func() ([]agent.Approval, error) { return s.inner.ListApprovals(ctx, f) })
}

func (s *execWire) DecideApproval(ctx context.Context, req agent.DecideRequest) (agent.Approval, error) {
	return wired(ctx, s, "DecideApproval", func() (agent.Approval, error) { return s.inner.DecideApproval(ctx, req) })
}

func (s *execWire) ExpireApprovals(ctx context.Context, now time.Time) (int, error) {
	return wired(ctx, s, "ExpireApprovals", func() (int, error) { return s.inner.ExpireApprovals(ctx, now) })
}

func (s *execWire) RequestCancel(ctx context.Context, req agent.CancelRequest) error {
	return wiredErr(ctx, s, "RequestCancel", func() error { return s.inner.RequestCancel(ctx, req) })
}

func (s *execWire) Changes(ctx context.Context, runID string, since int64) (agent.Changes, error) {
	return wired(ctx, s, "Changes", func() (agent.Changes, error) { return s.inner.Changes(ctx, runID, since) })
}

var _ agent.Store = (*execWire)(nil)

// execHooked is a store a test reaches into: it can change what the engine
// reads, act as a call arrives or after the store has answered it, and
// refuse one. It counts the heartbeats, the claims and the parks it is given.
type execHooked struct {
	agent.Store

	mu sync.Mutex
	// changes edits what Changes returns.
	changes func(*agent.Changes)
	// before and after are run, by the name of a Store method, as a call of
	// it arrives and once the store has answered. They are run for Park,
	// Finish, Yield, ListRuns and RequestCancel.
	before, after map[string]func()
	// parkRefused makes Park report that there is nothing to wait for,
	// without reaching the store.
	parkRefused bool
	// claim, when it returns an error for the nth claim, fails that claim.
	claim func(n int) error
	// approval edits the approval RequestApproval returns.
	approval func(*agent.Approval)

	claims, beats, parks int
}

func (s *execHooked) hook(hooks map[string]func(), op string) {
	s.mu.Lock()
	run := hooks[op]
	s.mu.Unlock()
	if run != nil {
		run()
	}
}

// on sets what runs as a call of op arrives.
func (s *execHooked) on(op string, run func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.before == nil {
		s.before = map[string]func(){}
	}
	s.before[op] = run
}

// once sets what runs after the store has answered a call of op.
func (s *execHooked) once(op string, run func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.after == nil {
		s.after = map[string]func(){}
	}
	s.after[op] = run
}

// refusePark sets whether Park reports that there is nothing to wait for
// without reaching the store, from the call after the one in flight.
func (s *execHooked) refusePark(refused bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.parkRefused = refused
}

func (s *execHooked) count() (claims, beats, parks int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.beats, s.parks
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

func (s *execHooked) Heartbeat(ctx context.Context, lease agent.Lease, now time.Time, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	s.beats++
	s.mu.Unlock()
	return s.Store.Heartbeat(ctx, lease, now, ttl)
}

func (s *execHooked) Park(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	s.mu.Lock()
	s.parks++
	refused := s.parkRefused
	s.mu.Unlock()
	s.hook(s.before, "Park")
	if refused {
		return false, nil
	}
	parked, err := s.Store.Park(ctx, lease, req)
	s.hook(s.after, "Park")
	return parked, err
}

func (s *execHooked) Finish(ctx context.Context, lease agent.Lease, req agent.FinishRequest) error {
	s.hook(s.before, "Finish")
	err := s.Store.Finish(ctx, lease, req)
	s.hook(s.after, "Finish")
	return err
}

func (s *execHooked) Yield(ctx context.Context, lease agent.Lease, req agent.YieldRequest) error {
	s.hook(s.before, "Yield")
	err := s.Store.Yield(ctx, lease, req)
	s.hook(s.after, "Yield")
	return err
}

func (s *execHooked) ListRuns(ctx context.Context, filter agent.RunFilter) ([]agent.Run, error) {
	s.hook(s.before, "ListRuns")
	return s.Store.ListRuns(ctx, filter)
}

func (s *execHooked) RequestCancel(ctx context.Context, req agent.CancelRequest) error {
	s.hook(s.before, "RequestCancel")
	return s.Store.RequestCancel(ctx, req)
}

func (s *execHooked) RequestApproval(ctx context.Context, lease agent.Lease, req agent.ApprovalRequest) (agent.Approval, error) {
	asked, err := s.Store.RequestApproval(ctx, lease, req)
	s.mu.Lock()
	edit := s.approval
	s.mu.Unlock()
	if err == nil && edit != nil {
		edit(&asked)
	}
	return asked, err
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

func TestExecute_AGuardErrorThatIsPermanentRefusesTheCallAndTheRunGoesOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		guard := &execGuard{}
		guard.failWith(fmt.Errorf("guard: %w: the record holds a number no column keeps", agent.ErrPermanent))
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
				agenttest.Say("it could not be refunded"),
			),
		})
		started := f.start("clerk", "refund order 7")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status, "the run goes on")
		assert.Equal(t, "it could not be refunded", got.Output)
		assert.Zero(t, got.Failures, "a decision that can never be recorded is not tried again")
		step := f.steps(started.ID)[1]
		assert.Equal(t, agent.StepBlocked, step.Status)
		assert.Equal(t, agent.Block, step.Decision)
		assert.Equal(t, "decision could not be recorded", step.Rule)
		assert.Equal(t, "not run: the policy decision could not be recorded", step.Result)
		assert.True(t, step.IsError)
		assert.Empty(t, calls.of("refund"), "a call whose decision is on no record is never run")
		assert.Len(t, guard.questions(), 1)
		assert.Equal(t, []agent.Result{
			{CallID: "call-1", Content: "not run: the policy decision could not be recorded", IsError: true},
		}, execResults(f.model.Requests()[1]))
		require.Len(t, f.logs.at(slog.LevelError), 1)
		assert.Contains(t, f.logs.at(slog.LevelError)[0], "agent: a guard's decision can never be recorded")
	})
}

func TestExecute_APanicInAToolsActionFunctionIsABlockAndNeverAnAllow(t *testing.T) {
	for name, guard := range map[string]*execGuard{
		"with no guard":            nil,
		"with a guard that allows": {},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				refund := calls.tool("refund", nil)
				refund.Action = func(agent.Invocation) agent.Action { panic("no action for this call") }
				cfg := execConfig{
					defs: []agent.Definition{execClerk(refund)},
					script: agenttest.Replies(
						agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
						agenttest.Say("it could not be refunded"),
					),
				}
				if guard != nil {
					cfg.guard = guard
				}
				f := newExecFixture(t, cfg)
				started := f.start("clerk", "refund order 7")

				got := f.execute(started.ID)

				assert.Equal(t, agent.StatusCompleted, got.Status)
				assert.Zero(t, got.Failures)
				step := f.steps(started.ID)[1]
				assert.Equal(t, agent.StepBlocked, step.Status)
				assert.Equal(t, agent.Block, step.Decision)
				assert.Equal(t, "the tool's Action function panicked", step.Rule)
				assert.Equal(t, "blocked by policy: the tool's Action function panicked", step.Result)
				assert.True(t, step.IsError)
				assert.Empty(t, calls.of("refund"))
				if guard != nil {
					assert.Empty(t, guard.questions(), "a guard is not asked about an action nobody could describe")
				}
				logged := f.logs.at(slog.LevelError)
				require.Len(t, logged, 1)
				assert.Contains(t, logged[0], "agent: a tool's Action function panicked")
				assert.Contains(t, logged[0], "no action for this call")
				assert.Contains(t, f.events(), "step.blocked 2")
			})
		})
	}
}

func TestExecute_AReplyWhoseStopIsNotKnownFailsTheRunWithoutBeingJournaled(t *testing.T) {
	for _, stop := range []agent.Stop{"", "length", "END"} {
		t.Run(fmt.Sprintf("stop %q", stop), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				asked := 0
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk(calls.tool("lookup", nil))},
					model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
						asked++
						reply := agenttest.Use(agenttest.Call("call-1", "lookup", `{}`))
						reply.Stop = stop
						return reply, nil
					}),
				})
				started := f.start("clerk", "is order 7 paid?")

				got := f.execute(started.ID)

				assert.Equal(t, agent.StatusFailed, got.Status)
				assert.Equal(t, agent.ReasonError, got.Reason)
				assert.Equal(t, fmt.Sprintf("the model's reply at step 1 stopped for %q, which this build does not know", stop), got.Error)
				assert.Zero(t, got.Failures, "it is not given up and asked for again")
				assert.Equal(t, 1, asked)
				assert.Equal(t, []string{"1 model started"}, f.journal(started.ID), "the reply is not journaled")
				assert.Empty(t, calls.of("lookup"))
			})
		})
	}
}

func TestExecute_AJournalThePlannerCannotReadGivesTheRunUpWithTheLongestWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("lookup", nil))},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)),
				agenttest.Say("never said"),
			),
			over: hooked(&store),
			tune: func(o *agent.Options) { o.RetryMax = 7 * time.Minute },
		})
		// A newer build's journal, as this one reads it.
		store.changes = func(c *agent.Changes) {
			for i := range c.Steps {
				if c.Steps[i].Kind == agent.StepTool {
					c.Steps[i].Status = "mystery"
				}
			}
		}
		started := f.start("clerk", "is order 7 paid?")
		const found = `step 2 has the status "mystery"`

		for failure := 1; failure <= 4; failure++ {
			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorContains(t, err, found)
			assert.Equal(t, agent.StatusRunnable, got.Status, "the run is given up, not ended")
			assert.Equal(t, failure, got.Failures)
			assert.Equal(t, found, got.Error)
			assert.Empty(t, got.LeaseOwner)
			require.NotNil(t, got.NextAttemptAt)
			assert.Equal(t, f.clock.Now().Add(7*time.Minute), *got.NextAttemptAt,
				"failure %d: the longest wait from the first time", failure)
			f.clock.Advance(7 * time.Minute)
		}
		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusFailed, got.Status, "the limit on failed executions ends it")
		assert.Equal(t, agent.ReasonError, got.Reason)
		assert.Equal(t, found, got.Error, "with the same message")
		assert.Equal(t, []string{"1 model completed", "2 lookup proposed"}, f.journal(started.ID),
			"nothing is written to the journal")
		assert.Empty(t, calls.of("lookup"))
		assert.Len(t, f.model.Requests(), 1)
	})
}

func TestExecute_AJournalWithAStepMissingIsNotActedOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("lookup", nil), calls.tool("refund", nil))},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{}`), agenttest.Call("call-2", "refund", `{}`)),
				agenttest.Say("never said"),
			),
			over: hooked(&store),
		})
		// A read that lost the journal's first step.
		store.changes = func(c *agent.Changes) {
			if len(c.Steps) > 1 {
				c.Steps = c.Steps[1:]
			}
		}
		started := f.start("clerk", "is order 7 paid?")

		got, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorContains(t, err, "its journal has step 2 where step 1 belongs")
		assert.Equal(t, 1, got.Failures)
		require.NotNil(t, got.NextAttemptAt)
		assert.Equal(t, execStart.Add(time.Second), *got.NextAttemptAt)
		assert.Equal(t, []string{"1 model completed", "2 lookup proposed", "3 refund proposed"}, f.journal(started.ID))
		assert.Empty(t, calls.seen, "no step is judged or run from a journal that is not whole")
	})
}

func TestExecute_AToolStepWithNoCallIsAnsweredAsMalformed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("lookup", nil))},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)),
				agenttest.Say("done"),
			),
			over: hooked(&store),
		})
		store.changes = func(c *agent.Changes) {
			for i := range c.Steps {
				if c.Steps[i].Status == agent.StepProposed {
					c.Steps[i].Call = nil
				}
			}
		}
		started := f.start("clerk", "is order 7 paid?")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "arguments were not valid JSON", f.steps(started.ID)[1].Result)
		assert.Empty(t, calls.of("lookup"))
	})
}

func TestExecute_Park(t *testing.T) {
	asking := func(t *testing.T, store **execHooked, refund agent.ToolFunc) (*execFixture, *execCalls, agent.Run) {
		calls := &execCalls{}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", refund))},
			guard: &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
				agenttest.Say("the refund is settled"),
			),
			over: hooked(store),
		})
		return f, calls, f.start("clerk", "refund order 7")
	}

	t.Run("an answer that arrives as the run parks is acted on by the same execution", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var store *execHooked
			var f *execFixture
			var started agent.Run
			// The tool takes long enough for a keeper to beat twice.
			f, calls, started := asking(t, &store, func(context.Context, agent.Invocation) (string, error) {
				time.Sleep(2*execHeartbeat + time.Second)
				return "refunded", nil
			})
			answered := false
			store.on("Park", func() {
				if answered {
					return
				}
				answered = true
				_, err := f.engine.Approve(t.Context(), f.approvals(started.ID)[0].ID, "ops@example.test", "")
				assert.NoError(t, err)
			})

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status, "Park reported false, and the loop went round again")
			assert.Equal(t, "the refund is settled", got.Output)
			assert.Len(t, calls.of("refund"), 1)
			_, beats, parks := store.count()
			assert.Equal(t, 1, parks)
			assert.Equal(t, 2, beats, "the lease is kept again once Park has reported false")
			assert.NotContains(t, f.events(), "run.waiting", "a run that did not park is not announced as waiting")
			assert.Empty(t, f.logs.at(slog.LevelWarn))
		})
	})

	t.Run("refused twice with nothing changed, the execution ends as failed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var store *execHooked
			f, calls, started := asking(t, &store, nil)
			store.parkRefused = true

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorContains(t, err, "the run waits for approval, and the store twice found nothing for it to wait for")
			_, _, parks := store.count()
			assert.Equal(t, 2, parks, "it does not spin")
			assert.Equal(t, agent.StatusRunnable, got.Status)
			assert.Equal(t, 1, got.Failures)
			assert.Empty(t, got.LeaseOwner)
			require.NotNil(t, got.NextAttemptAt)
			assert.Equal(t, execStart.Add(time.Second), *got.NextAttemptAt)
			assert.Empty(t, calls.of("refund"))
			assert.Len(t, f.approvals(started.ID), 1, "and asks nothing new on the way round")
		})
	})

	t.Run("refused at one revision and then at another, it goes round again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var store *execHooked
			var f *execFixture
			var started agent.Run
			f, _, started = asking(t, &store, nil)
			store.parkRefused = true
			// Each refusal is followed by a change to the run, until the third.
			refusals := 0
			store.on("Park", func() {
				refusals++
				if refusals < 3 {
					assert.NoError(t, f.memory.RequestCancel(t.Context(), agent.CancelRequest{
						RunID: started.ID, By: "ops@example.test", Reason: "enough", Now: f.clock.Now(),
					}))
				}
			})

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCancelled, got.Status, "the change is read and acted on")
			_, _, parks := store.count()
			assert.Equal(t, 1, parks)
		})
	})
}

func TestExecute_TheKeeperIsStoppedBeforeTheWriteThatLetsTheRunGo(t *testing.T) {
	// The store takes two heartbeats' time to answer the last write. A keeper
	// still running would beat meanwhile, find the lease given back, and say
	// it was lost.
	for _, tc := range []struct {
		name  string
		op    string
		reply agent.Response
		err   error
		ended bool
	}{
		{name: "Finish", op: "Finish", reply: agenttest.Say("done")},
		{name: "Park", op: "Park", reply: agenttest.Use(agenttest.Call("call-1", "send", `{}`))},
		{name: "Yield, for a failure", op: "Yield", err: errors.New("rate limited")},
		{name: "Yield, for a caller that has stopped", op: "Yield", ended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				send := calls.tool("send", nil)
				send.Approval = true
				var store *execHooked
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk(send)},
					model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
						return tc.reply, tc.err
					}),
					over: hooked(&store),
				})
				written := 0
				store.once(tc.op, func() {
					written++
					time.Sleep(2 * execHeartbeat)
				})
				started := f.start("clerk", "hello")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.ended {
					// The caller stops as soon as the run is claimed.
					f.wire.then("Claim", cancel)
				}

				_, _ = f.engine.Execute(ctx, started.ID)

				require.Equal(t, 1, written, "the write was made")
				_, beats, _ := store.count()
				assert.Zero(t, beats, "no heartbeat is made once the run is being let go")
				assert.Empty(t, f.run(started.ID).LeaseOwner)
				for _, line := range f.logs.at(slog.LevelWarn) {
					assert.NotContains(t, line, "lease lost")
				}
			})
		})
	}
}

// execPicky is a store that refuses what Postgres would refuse in a text
// column, a NUL or bytes that are not UTF-8, and notes what it was asked to
// keep.
type execPicky struct {
	agent.Store

	mu      sync.Mutex
	texts   []string
	refused []string
}

func (s *execPicky) keep(what string, texts ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, text := range texts {
		if strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
			s.refused = append(s.refused, fmt.Sprintf("%s: %q", what, text))
			return fmt.Errorf("%s: text a column cannot hold", what)
		}
		if text != "" {
			s.texts = append(s.texts, text)
		}
	}
	return nil
}

func (s *execPicky) kept() (texts, refused []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...), append([]string(nil), s.refused...)
}

func (s *execPicky) CreateRun(ctx context.Context, run agent.Run) (agent.Run, bool, error) {
	if err := s.keep("CreateRun", run.Input); err != nil {
		return agent.Run{}, false, err
	}
	return s.Store.CreateRun(ctx, run)
}

func (s *execPicky) Yield(ctx context.Context, lease agent.Lease, req agent.YieldRequest) error {
	if err := s.keep("Yield", req.Error); err != nil {
		return err
	}
	return s.Store.Yield(ctx, lease, req)
}

func (s *execPicky) Finish(ctx context.Context, lease agent.Lease, req agent.FinishRequest) error {
	if err := s.keep("Finish", req.Reason, req.Output, req.Error); err != nil {
		return err
	}
	return s.Store.Finish(ctx, lease, req)
}

func (s *execPicky) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	result := ""
	if req.Result != nil {
		result = *req.Result
	}
	if err := s.keep("UpdateStep", req.Rule, result); err != nil {
		return err
	}
	return s.Store.UpdateStep(ctx, lease, req)
}

func (s *execPicky) RequestApproval(ctx context.Context, lease agent.Lease, req agent.ApprovalRequest) (agent.Approval, error) {
	if err := s.keep("RequestApproval", req.Rule); err != nil {
		return agent.Approval{}, err
	}
	return s.Store.RequestApproval(ctx, lease, req)
}

func (s *execPicky) RequestCancel(ctx context.Context, req agent.CancelRequest) error {
	if err := s.keep("RequestCancel", req.By, req.Reason); err != nil {
		return err
	}
	return s.Store.RequestCancel(ctx, req)
}

func TestExecute_WritesOnlyTextTheJournalCanHold(t *testing.T) {
	// dirty is text with both things a text column refuses, and clean is what
	// journalText makes of it.
	const dirty, clean = "marked a\x00b \xff\xfe c", "marked ab \uFFFD c"
	lookup := agenttest.Use(agenttest.Call("call-1", "lookup", `{"order":7}`))

	for _, tc := range []struct {
		name string
		// arrange builds the scenario over the picky store and plays it.
		arrange func(t *testing.T, over func(agent.Store) agent.Store)
		// want is what the store must have been given, for text the engine
		// was handed by a model or a guard. Text that reached the engine
		// through the store has none: a store may have repaired it already,
		// in a way of its own, and then all that is asked is that it arrived.
		want string
	}{
		{
			name: "a model's error, given back with the run",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk()}, over: over,
					model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
						return agent.Response{}, errors.New(dirty)
					}),
				})
				_, err := f.engine.Execute(t.Context(), f.start("clerk", "hello").ID)
				require.Error(t, err)
			},
			want: clean,
		},
		{
			name: "a model's permanent error, which ends the run",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk()}, over: over,
					model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
						return agent.Response{}, fmt.Errorf("%s%w", dirty, agent.ErrPermanent)
					}),
				})
				f.execute(f.start("clerk", "hello").ID)
			},
			want: clean + agent.ErrPermanent.Error(),
		},
		{
			name: "a model's last word, which is the run's output",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk()}, over: over,
					script: agenttest.Replies(agenttest.Say(dirty)),
				})
				f.execute(f.start("clerk", "hello").ID)
			},
		},
		{
			name: "a guard's error",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				guard := &execGuard{}
				guard.failWith(errors.New(dirty))
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk((&execCalls{}).tool("lookup", nil))}, over: over,
					guard: guard, script: agenttest.Replies(lookup),
				})
				_, err := f.engine.Execute(t.Context(), f.start("clerk", "hello").ID)
				require.Error(t, err)
			},
			want: clean,
		},
		{
			name: "the rule of a guard that blocks",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk((&execCalls{}).tool("lookup", nil))}, over: over,
					guard:  &execGuard{answers: map[string]agent.Decision{"lookup": {Effect: agent.Block, Rule: dirty}}},
					script: agenttest.Replies(lookup, agenttest.Say("done")),
				})
				f.execute(f.start("clerk", "hello").ID)
			},
			want: "blocked by policy: " + clean,
		},
		{
			name: "the rule of a guard that asks",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk((&execCalls{}).tool("lookup", nil))}, over: over,
					guard:  &execGuard{answers: map[string]agent.Decision{"lookup": {Effect: agent.Ask, Rule: dirty}}},
					script: agenttest.Replies(lookup, agenttest.Say("done")),
				})
				f.execute(f.start("clerk", "hello").ID)
			},
			want: clean,
		},
		{
			name: "the rule of a guard that allows",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk((&execCalls{}).tool("lookup", nil))}, over: over,
					guard:  &execGuard{answers: map[string]agent.Decision{"lookup": {Effect: agent.Allow, Rule: dirty}}},
					script: agenttest.Replies(lookup, agenttest.Say("done")),
				})
				f.execute(f.start("clerk", "hello").ID)
			},
			want: clean,
		},
		{
			name: "who declined a call, and why",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk((&execCalls{}).tool("lookup", nil))}, over: over,
					guard:  &execGuard{answers: map[string]agent.Decision{"lookup": {Effect: agent.Ask, Rule: "ask-first"}}},
					script: agenttest.Replies(lookup, agenttest.Say("done")),
				})
				started := f.start("clerk", "hello")
				f.execute(started.ID)
				// Through the store, which checks nothing about a name.
				_, err := f.memory.DecideApproval(t.Context(), agent.DecideRequest{
					ID: f.approvals(started.ID)[0].ID, By: dirty, Reason: dirty, Now: f.clock.Now(),
				})
				require.NoError(t, err)
				f.execute(started.ID)
			},
		},
		{
			name: "who cancelled a run and why, passed on to its child",
			arrange: func(t *testing.T, over func(agent.Store) agent.Store) {
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execLead(), execReviewer()}, over: over,
					script: execLeadScript(execSummaries),
				})
				started := f.start("lead", "hello")
				f.execute(started.ID)
				require.NoError(t, f.memory.RequestCancel(t.Context(), agent.CancelRequest{
					RunID: started.ID, By: dirty, Reason: dirty, Now: f.clock.Now(),
				}))
				f.execute(started.ID)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var store *execPicky
				tc.arrange(t, func(inner agent.Store) agent.Store {
					store = &execPicky{Store: inner}
					return store
				})

				texts, refused := store.kept()
				assert.Empty(t, refused, "the engine handed the store text it cannot hold")
				if tc.want != "" {
					assert.Contains(t, texts, tc.want)
					return
				}
				assert.True(t, slices.ContainsFunc(texts, func(text string) bool {
					return strings.Contains(text, "marked")
				}), "the text reached the store: %q", texts)
			})
		})
	}
}

func TestExecute_AnApprovalThatWillNeverBeAYes(t *testing.T) {
	// answered makes every pending approval read as status, as it would once
	// its run had ended or its time had run out.
	answered := func(status agent.ApprovalStatus) func(*agent.Changes) {
		return func(c *agent.Changes) {
			for i := range c.Approvals {
				if c.Approvals[i].Status == agent.ApprovalPending {
					c.Approvals[i].Status = status
				}
			}
		}
	}

	t.Run("a cancelled approval declines the call", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			var store *execHooked
			f := newExecFixture(t, execConfig{
				defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
				guard: &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "refund", `{}`)),
					agenttest.Say("it was not refunded"),
				),
				over: hooked(&store),
			})
			store.changes = answered(agent.ApprovalCancelled)
			started := f.start("clerk", "refund order 7")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			step := f.steps(started.ID)[1]
			assert.Equal(t, agent.StepDeclined, step.Status)
			assert.Equal(t, "declined: approval cancelled", step.Result)
			assert.True(t, step.IsError)
			assert.Empty(t, calls.of("refund"))
		})
	})

	for _, status := range []agent.ApprovalStatus{agent.ApprovalExpired, agent.ApprovalCancelled} {
		t.Run(fmt.Sprintf("an interrupted call whose approval is %s is not run again", status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				var first *execFixture
				charge := calls.tool("charge", func(_ context.Context, in agent.Invocation) (string, error) {
					if in.Attempt == 1 {
						first.faults.Kill()
					}
					return "charged", nil
				})
				charge.AtMostOnce = true
				first = newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk(charge)},
					script: agenttest.Replies(
						agenttest.Use(agenttest.Call("call-1", "charge", `{"amount":1250}`)),
						agenttest.Say("the card may have been charged"),
					),
				})
				started := first.start("clerk", "charge the card")
				_, err := first.engine.Execute(t.Context(), started.ID)
				require.ErrorIs(t, err, agenttest.ErrKilled)

				var store *execHooked
				second := first.rival(execConfig{defs: []agent.Definition{execClerk(charge)}, over: hooked(&store)})
				store.changes = answered(status)
				first.clock.Advance(execTTL)
				got := second.execute(started.ID)

				assert.Equal(t, agent.StatusCompleted, got.Status)
				step := first.steps(started.ID)[1]
				assert.Equal(t, agent.StepDeclined, step.Status)
				assert.Equal(t, "interrupted before its result was recorded; not run again", step.Result)
				assert.True(t, step.IsError)
				assert.Len(t, calls.of("charge"), 1)
			})
		})
	}
}

func TestExecute_APanicOnTheExecutionsOwnGoroutineEndsItAsFailed(t *testing.T) {
	t.Run("in the model", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk()},
				model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
					panic("model exploded")
				}),
			})
			started := f.start("clerk", "hello")

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorContains(t, err, "the execution panicked: model exploded")
			assert.Equal(t, agent.StatusRunnable, got.Status)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, "the execution panicked: model exploded", got.Error)
			assert.Empty(t, got.LeaseOwner, "the run is given back, and its keeper stopped")
			require.NotNil(t, got.NextAttemptAt)
			assert.Equal(t, execStart.Add(time.Second), *got.NextAttemptAt, "with the wait of any failed step")
			assert.Equal(t, []string{"1 model started"}, f.journal(started.ID))
			logged := f.logs.at(slog.LevelError)
			require.Len(t, logged, 1)
			assert.Contains(t, logged[0], "agent: an execution panicked")
			assert.Contains(t, logged[0], "model exploded")
		})
	})

	t.Run("in the guard, which is neither an allow nor a block", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("refund", nil))},
				guard: agenttest.GuardFunc(func(context.Context, agent.Action) (agent.Decision, error) {
					panic("guard exploded")
				}),
				script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "refund", `{}`))),
			})
			started := f.start("clerk", "refund order 7")

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorContains(t, err, "the execution panicked: guard exploded")
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, []string{"1 model completed", "2 refund proposed"}, f.journal(started.ID))
			assert.Empty(t, calls.of("refund"))
		})
	})
}

func TestExecute_ARunAbandonedByEveryExecutionIsFinishedBeforeAnyWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execLead(), execReviewer()},
			script: execLeadScript(execSummaries),
			tune:   func(o *agent.Options) { o.MaxFailures = 3 },
		})
		started := f.start("lead", "review the batch")
		ctx := t.Context()
		// A child that is on no step: created just before its parent's
		// execution died.
		orphan, _, err := f.memory.CreateRun(ctx, agent.Run{
			ID: execID(7, 1), Agent: "reviewer", Status: agent.StatusRunnable, Input: "{}",
			ParentID: started.ID, ParentSeq: 2, Depth: 1, CreatedAt: execStart, UpdatedAt: execStart,
		})
		require.NoError(t, err)

		// One execution fails and says why; the next two die holding the run.
		claim := func(owner string) agent.Lease {
			claimed, err := f.memory.Claim(ctx, agent.ClaimRequest{
				Owner: owner, Agents: []string{"lead"}, RunID: started.ID, Now: f.clock.Now(), TTL: execTTL,
			})
			require.NoError(t, err)
			return claimed.Lease()
		}
		require.NoError(t, f.memory.Yield(ctx, claim("worker-a"), agent.YieldRequest{
			Failed: true, Error: "the model was down", Now: f.clock.Now(),
		}))
		claim("worker-b")
		f.clock.Advance(execTTL)
		claim("worker-c")
		f.clock.Advance(execTTL)
		require.Equal(t, 2, f.run(started.ID).Failures)

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusFailed, got.Status)
		assert.Equal(t, agent.ReasonAbandoned, got.Reason)
		assert.Equal(t, 3, got.Failures, "the claim that took the run over is the failure that ends it")
		assert.Equal(t, "abandoned: 3 executions in a row failed or lost the run; the last error recorded: the model was down", got.Error)
		assert.Empty(t, f.journal(started.ID), "none of the run's work is done")
		assert.Empty(t, f.model.Requests())
		child := f.run(orphan.ID)
		assert.True(t, child.CancelRequested, "a child is found by its parent, not from the journal")
		assert.Equal(t, "run "+started.ID, child.CancelBy)
		assert.Equal(t, fmt.Sprintf("parent run %s failed: abandoned", started.ID), child.CancelReason)
		assert.Equal(t, "run.failed", f.events()[len(f.events())-1])
	})
}

func TestExecute_ARunThatIsCancelledAsksEveryChildHoweverMany(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execLead(), execReviewer()},
			script: execLeadScript(execSummaries),
		})
		started := f.start("lead", "review the batch")
		// More children than one listing holds, some of them created at the
		// same instant, and one in every seven already ended.
		const many = 451
		ctx := t.Context()
		for i := range many {
			at := execStart.Add(time.Duration(i/3) * time.Second)
			child, _, err := f.memory.CreateRun(ctx, agent.Run{
				ID: execID(7, i+1), Agent: "reviewer", Status: agent.StatusRunnable, Input: "{}",
				ParentID: started.ID, ParentSeq: 2, Depth: 1, CreatedAt: at, UpdatedAt: at,
			})
			require.NoError(t, err)
			if i%7 == 0 {
				claimed, err := f.memory.Claim(ctx, agent.ClaimRequest{
					Owner: "worker-9", Agents: []string{"reviewer"}, RunID: child.ID, Now: at, TTL: execTTL,
				})
				require.NoError(t, err)
				require.NoError(t, f.memory.Finish(ctx, claimed.Lease(), agent.FinishRequest{Status: agent.StatusCompleted, Now: at}))
			}
		}
		require.NoError(t, f.engine.Cancel(ctx, started.ID, "ops@example.test", "wrong batch"))
		f.bus.forget()

		got := f.execute(started.ID)

		require.Equal(t, agent.StatusCancelled, got.Status)
		assert.Equal(t, 3, f.wire.made("ListRuns"), "a listing shorter than the limit is the last")
		asked := 0
		for i := range many {
			child := f.run(execID(7, i+1))
			if i%7 == 0 {
				assert.False(t, child.CancelRequested, "child %d had ended", i+1)
				continue
			}
			if assert.True(t, child.CancelRequested, "child %d", i+1) {
				asked++
			}
		}
		assert.Equal(t, many-many/7-1, asked)
		assert.Len(t, f.events(), asked+1, "each request is announced once, and then the run's end")
	})
}

func TestExecute_ARunThatFailsAsksItsRunningChildrenToStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		lead := execLead()
		lead.Tools = append(lead.Tools, calls.tool("send", nil))
		lead.Limits = agent.Limits{MaxCostMicros: 500}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{lead, execReviewer()},
			guard: &execGuard{answers: map[string]agent.Decision{"send": {Effect: agent.Ask, Rule: "ask-first"}}},
			script: agenttest.ByAgent(map[string]agenttest.Script{
				"lead": agenttest.Replies(execSpent(agenttest.Use(
					agenttest.Call("call-1", "send", `{}`),
					agenttest.Call("call-2", "review", `{"doc":1}`),
					agenttest.Call("call-3", "review", `{"doc":2}`),
				), agent.Usage{CostMicros: 400})),
				"reviewer": func(agent.Request, int) (agent.Response, error) {
					return execSpent(agenttest.Say("a summary"), agent.Usage{CostMicros: 200}), nil
				},
			}),
		})
		started := f.start("lead", "review the batch")
		f.execute(started.ID)
		steps := f.steps(started.ID)
		// One child ends, and its cost takes the parent past its limit.
		f.execute(steps[2].ChildRunID)
		parked := f.execute(started.ID)
		require.Equal(t, agent.StatusWaiting, parked.Status, "a run with a spent budget and nothing but pending steps still parks")
		require.Equal(t, int64(600), parked.Usage.CostMicros)
		_, err := f.engine.Approve(t.Context(), f.approvals(started.ID)[0].ID, "ops@example.test", "")
		require.NoError(t, err)

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusFailed, got.Status, "woken with work to do, it fails for its budget")
		assert.Equal(t, agent.ReasonCostBudget, got.Reason)
		assert.Empty(t, calls.of("send"))
		running := f.run(steps[3].ChildRunID)
		assert.True(t, running.CancelRequested)
		assert.Equal(t, "run "+started.ID, running.CancelBy)
		assert.Equal(t, fmt.Sprintf("parent run %s failed: cost_budget", started.ID), running.CancelReason)
		assert.False(t, f.run(steps[2].ChildRunID).CancelRequested, "a child that has ended is not asked")
	})
}

func TestExecute_ADelegationWhoseKeyBelongsToAnotherRunIsAnErrorResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execLead(), execReviewer()},
			script: agenttest.ByAgent(map[string]agenttest.Script{
				"lead": agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "review", `{"doc":1}`)),
					agenttest.Say("nobody reviewed it"),
				),
			}),
		})
		started := f.start("lead", "review the batch")
		// Somebody started a run of the same agent under the key the step's
		// child would have.
		other, err := f.engine.Start(t.Context(), agent.StartRequest{
			Agent: "reviewer", Input: "mine", Key: agent.StepKey(started.ID, 2),
		})
		require.NoError(t, err)

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status, "the run does not wait on a run that would never wake it")
		step := f.steps(started.ID)[1]
		assert.Equal(t, agent.StepCompleted, step.Status)
		assert.Equal(t, "delegation refused: its key belongs to another run", step.Result)
		assert.True(t, step.IsError)
		assert.Empty(t, step.ChildRunID)
		assert.Equal(t, other, f.run(other.ID), "the other run is left as it is")
	})
}

func TestExecute_AChildInterruptedBetweenBeingStartedAndBeingRecordedIsCreatedOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execLead(), execReviewer()},
			script: agenttest.ByAgent(map[string]agenttest.Script{
				"lead": agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "review", `{"doc":1}`)),
					agenttest.Say("reviewed"),
				),
				"reviewer": execSummaries,
			}),
		})
		started := f.start("lead", "review the batch")
		// The child is created, and the process never learns of it.
		f.faults.FailAfter("CreateRun", 1)

		_, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorIs(t, err, agenttest.ErrFault)
		require.Equal(t, []string{"1 model completed", "2 review started"}, f.journal(started.ID))
		created := f.children(started.ID)
		require.Len(t, created, 1)

		f.clock.Advance(time.Second)
		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusWaiting, got.Status)
		assert.Equal(t, agent.ReasonChildren, got.Reason)
		children := f.children(started.ID)
		require.Len(t, children, 1, "asked twice under one key, the child is created once")
		step := f.steps(started.ID)[1]
		assert.Equal(t, created[0].ID, step.ChildRunID)
		assert.Equal(t, 1, step.Attempts, "the step was started once; only its child's start was repeated")
	})
}

func TestExecute_AToolIsGivenTheSmallerOfItsTimeoutAndTheTimeLeft(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		limit   time.Duration
		want    time.Duration
	}{
		{name: "two minutes when it sets none", want: 2 * time.Minute},
		{name: "its own", timeout: 30 * time.Second, want: 30 * time.Second},
		{name: "its own when the run has no time limit", timeout: 30 * time.Second, limit: -1, want: 30 * time.Second},
		{name: "two minutes when the run has no time limit either", limit: -1, want: 2 * time.Minute},
		{name: "what is left of the budget when that is less", timeout: 30 * time.Second, limit: 10 * time.Second, want: 10 * time.Second},
		{name: "its own when the budget has more", timeout: 30 * time.Second, limit: time.Hour, want: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				var given time.Duration
				lookup := calls.tool("lookup", func(ctx context.Context, _ agent.Invocation) (string, error) {
					deadline, _ := ctx.Deadline()
					given = time.Until(deadline)
					return "found", nil
				})
				lookup.Timeout = tc.timeout
				def := execClerk(lookup)
				def.Limits = agent.Limits{MaxDuration: tc.limit}
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{def},
					script: agenttest.Replies(
						agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)),
						agenttest.Say("done"),
					),
				})

				got := f.execute(f.start("clerk", "hello").ID)

				require.Equal(t, agent.StatusCompleted, got.Status)
				assert.Equal(t, tc.want, given)
			})
		})
	}

	t.Run("a tool that outlives what is left is timed out and recorded, and the budget then ends the run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			slow := calls.tool("slow", func(ctx context.Context, _ agent.Invocation) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			})
			def := execClerk(slow)
			def.Limits = agent.Limits{MaxDuration: 10 * time.Second}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "slow", `{}`)),
					agenttest.Say("never said"),
				),
			})
			started := f.start("clerk", "take your time")
			// The engine's clock keeps time with the tool's.
			go func() {
				time.Sleep(10*time.Second - time.Nanosecond)
				f.clock.Advance(10 * time.Second)
			}()

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonTimeBudget, got.Reason, "not a run tried again until its failures end it")
			assert.Zero(t, got.Failures)
			step := f.steps(started.ID)[1]
			assert.Equal(t, agent.StepCompleted, step.Status)
			assert.Equal(t, "timed out after 10s", step.Result)
			assert.Len(t, calls.of("slow"), 1)
		})
	})
}

func TestExecute_AModelCallIsGivenTheTimeLeftInTheBudget(t *testing.T) {
	t.Run("a call cut off for the budget fails the run with ReasonTimeBudget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			asked := 0
			stopped := make(chan error, 1)
			def := execClerk()
			def.Limits = agent.Limits{MaxDuration: time.Minute}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				model: execModelFunc(func(ctx context.Context, _ agent.Request) (agent.Response, error) {
					asked++
					<-ctx.Done()
					stopped <- context.Cause(ctx)
					return agent.Response{}, ctx.Err()
				}),
			})
			started := f.start("clerk", "hello")
			at := time.Now()

			got := f.execute(started.ID)

			assert.Equal(t, time.Minute, time.Since(at))
			assert.Same(t, agent.ErrTimeBudget, <-stopped)
			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonTimeBudget, got.Reason)
			assert.Equal(t, "the model call at step 1 was cut off when the time budget ran out", got.Error)
			assert.Zero(t, got.Failures, "tried again it would be cut off again, so it is not")
			assert.Equal(t, 1, asked)
			assert.Equal(t, []string{"1 model started"}, f.journal(started.ID))
		})
	})

	t.Run("a call is given no deadline by a run with no time limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			bounded := true
			def := execClerk()
			def.Limits = agent.Limits{MaxDuration: -1}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				model: execModelFunc(func(ctx context.Context, _ agent.Request) (agent.Response, error) {
					_, bounded = ctx.Deadline()
					return agenttest.Say("done"), nil
				}),
			})

			got := f.execute(f.start("clerk", "hello").ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.False(t, bounded)
		})
	})

	t.Run("a reply that arrives after the time ran out is recorded, and the budget then ends the run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var f *execFixture
			calls := &execCalls{}
			def := execClerk(calls.tool("lookup", nil))
			def.Limits = agent.Limits{MaxDuration: time.Minute}
			f = newExecFixture(t, execConfig{
				defs: []agent.Definition{def},
				model: execModelFunc(func(ctx context.Context, _ agent.Request) (agent.Response, error) {
					<-ctx.Done()
					f.clock.Advance(time.Minute)
					return execSpent(agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)), agent.Usage{CostMicros: 5}), nil
				}),
			})
			started := f.start("clerk", "hello")

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusFailed, got.Status)
			assert.Equal(t, agent.ReasonTimeBudget, got.Reason)
			assert.Empty(t, got.Error)
			assert.Equal(t, int64(5), got.Usage.CostMicros, "what was paid for is counted")
			assert.Equal(t, []string{"1 model completed", "2 lookup proposed"}, f.journal(started.ID))
		})
	})
}

func TestExecute_TheRequestCarriesTheRunsSnapshot(t *testing.T) {
	for _, maxTokens := range []int{0, 2048} {
		t.Run(fmt.Sprintf("max tokens %d", maxTokens), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				def := execClerk()
				def.Output = json.RawMessage(`{"type":"object","required":["total"]}`)
				def.MaxTokens = maxTokens
				f := newExecFixture(t, execConfig{
					defs:   []agent.Definition{def},
					script: agenttest.Replies(agenttest.Say(`{"total":42}`)),
				})

				f.execute(f.start("clerk", "what is the total?").ID)

				requests := f.model.Requests()
				require.Len(t, requests, 1)
				assert.JSONEq(t, `{"type":"object","required":["total"]}`, string(requests[0].Output))
				assert.Equal(t, maxTokens, requests[0].MaxTokens,
					"the snapshot's bound, and none of the engine's own when the snapshot has none")
			})
		})
	}
}

func TestExecute_AReplyIsStoredAsTheAssistantsTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		turn := 0
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("lookup", nil))},
			model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
				turn++
				if turn == 1 {
					// A model that wrote no role on its reply, and one that
					// wrote the wrong one.
					return agent.Response{
						Message: agent.Message{Calls: []agent.Call{agenttest.Call("call-1", "lookup", `{}`)}},
						Stop:    agent.StopToolUse, Model: "model-b",
					}, nil
				}
				return agent.Response{Message: agent.Message{Role: agent.RoleUser, Text: "done"}, Stop: agent.StopEnd}, nil
			}),
		})
		started := f.start("clerk", "hello")

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusCompleted, got.Status)
		steps := f.steps(started.ID)
		assert.Equal(t, agent.RoleAssistant, steps[0].Message.Role)
		assert.Equal(t, agent.RoleAssistant, steps[2].Message.Role)
		assert.Equal(t, "model-b", steps[0].Name)
	})
}

func TestExecute_AnInterruptedCallWhoseActionPanicsIsStillPutToAPerson(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		var first *execFixture
		charge := calls.tool("charge", func(context.Context, agent.Invocation) (string, error) {
			first.faults.Kill()
			return "charged", nil
		})
		charge.AtMostOnce = true
		var described []int
		charge.Action = func(in agent.Invocation) agent.Action {
			described = append(described, in.Attempt)
			if in.Attempt > 1 {
				panic("no action for a second attempt")
			}
			return agent.Action{Kind: "pay", Target: "card"}
		}
		guard := &execGuard{}
		first = newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(charge)}, guard: guard,
			script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "charge", `{}`))),
		})
		started := first.start("clerk", "charge the card")
		_, err := first.engine.Execute(t.Context(), started.ID)
		require.ErrorIs(t, err, agenttest.ErrKilled)
		require.Equal(t, "pay", guard.questions()[0].Kind)

		second := first.rival(execConfig{defs: []agent.Definition{execClerk(charge)}, guard: guard})
		first.clock.Advance(execTTL)
		got := second.execute(started.ID)

		assert.Equal(t, agent.StatusWaiting, got.Status, "the question does not wait on a description")
		approvals := second.approvals(started.ID)
		require.Len(t, approvals, 1)
		assert.Equal(t, agent.CauseInterrupted, approvals[0].Cause)
		assert.Equal(t, "run", approvals[0].Action.Kind, "the call is described as a tool with no Action of its own is")
		assert.Equal(t, "charge", approvals[0].Action.Target)
		assert.Len(t, calls.of("charge"), 1)
		assert.Len(t, guard.questions(), 1, "the guard is not asked about an interrupted call")
		assert.Equal(t, []int{1, 2}, described, "an Action function is given the attempt that would be made")
	})
}

func TestExecute_AnApprovalHandedBackThatIsNotTheOneAskedForEndsTheExecutionAsFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		var store *execHooked
		guard := &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}}
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard:  guard,
			script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "refund", `{}`))),
			over:   hooked(&store),
		})
		// The store answers as it does when the step already has an approval
		// for its attempt: with that one, which is not the one asked for.
		store.approval = func(a *agent.Approval) {
			a.ID = execID(8, 8)
			a.Status = agent.ApprovalDeclined
		}
		started := f.start("clerk", "refund order 7")

		got, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorContains(t, err, "step 2 is proposed and already has approval "+execID(8, 8)+" for attempt 0, which is declined")
		assert.Equal(t, 1, got.Failures)
		require.NotNil(t, got.NextAttemptAt)
		assert.Equal(t, execStart.Add(time.Second), *got.NextAttemptAt)
		assert.Len(t, guard.questions(), 1, "it does not go round and ask again")
		assert.NotContains(t, f.events(), "approval.requested 2")
	})
}

func TestExecute_AToolThisBuildNoLongerHas(t *testing.T) {
	script := agenttest.Replies(
		agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
		agenttest.Say("it could not be refunded"),
	)

	t.Run("a call that was interrupted is answered, not run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			var first *execFixture
			first = newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("refund", func(context.Context, agent.Invocation) (string, error) {
					first.faults.Kill()
					return "refunded", nil
				}))},
				script: script,
			})
			started := first.start("clerk", "refund order 7")
			_, err := first.engine.Execute(t.Context(), started.ID)
			require.ErrorIs(t, err, agenttest.ErrKilled)

			// The build that takes over has no such tool.
			second := first.rival(execConfig{defs: []agent.Definition{execClerk()}})
			first.clock.Advance(execTTL)
			got := second.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			step := first.steps(started.ID)[1]
			assert.Equal(t, agent.StepCompleted, step.Status)
			assert.Equal(t, "tool is not available", step.Result)
			assert.True(t, step.IsError)
			assert.Equal(t, 2, step.Attempts, "the attempt is counted, and its time starts with it")
			assert.Zero(t, got.ActiveMillis, "the time the step lay interrupted is not time worked")
			assert.Len(t, calls.of("refund"), 1)
		})
	})

	t.Run("a call that was approved is answered, not run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			guard := &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}}
			first := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("refund", nil))}, guard: guard, script: script,
			})
			started := first.start("clerk", "refund order 7")
			first.execute(started.ID)
			_, err := first.engine.Approve(t.Context(), first.approvals(started.ID)[0].ID, "ops@example.test", "")
			require.NoError(t, err)

			second := first.rival(execConfig{defs: []agent.Definition{execClerk()}, guard: guard})
			got := second.execute(started.ID)

			assert.Equal(t, agent.StatusCompleted, got.Status)
			step := first.steps(started.ID)[1]
			assert.Equal(t, agent.StepCompleted, step.Status)
			assert.Equal(t, "tool is not available", step.Result)
			assert.Empty(t, calls.of("refund"))
		})
	})
}

// execTakeover is two processes over one store, and a run the first of them
// is executing: its tool is in flight, and returns when it is let go or its
// context ends. The second process's execution of the same call waits to be
// let go too.
type execTakeover struct {
	first, second *execFixture
	calls         *execCalls
	run           agent.Run
	// outcome is what the first process's Execute returned.
	outcome <-chan execOutcome
	// stopped receives the cause of the first tool's context, if it ended.
	stopped          chan error
	release1, began2 chan struct{}
	release2         chan struct{}
}

func newExecTakeover(t *testing.T) *execTakeover {
	t.Helper()
	tk := &execTakeover{
		calls:    &execCalls{},
		stopped:  make(chan error, 1),
		release1: make(chan struct{}),
		began2:   make(chan struct{}),
		release2: make(chan struct{}),
	}
	began1 := make(chan struct{})
	slow := func(ctx context.Context, in agent.Invocation) (string, error) {
		if in.Attempt > 1 {
			close(tk.began2)
			<-tk.release2
			return "the second worker's result", nil
		}
		close(began1)
		select {
		case <-ctx.Done():
			tk.stopped <- context.Cause(ctx)
			return "", ctx.Err()
		case <-tk.release1:
			return "the first worker's result", nil
		}
	}
	defs := func() []agent.Definition { return []agent.Definition{execClerk(tk.calls.tool("slow", slow))} }
	tk.first = newExecFixture(t, execConfig{
		defs: defs(),
		script: agenttest.Replies(
			agenttest.Use(agenttest.Call("call-1", "slow", `{}`)),
			agenttest.Say("done"),
		),
	})
	tk.second = tk.first.rival(execConfig{defs: defs()})
	tk.run = tk.first.start("clerk", "take your time")
	tk.outcome = tk.first.begin(t.Context(), tk.run.ID)
	<-began1
	return tk
}

// take has the second process claim the run after the first was paused past
// its lease, and execute it as far as its own call of the tool.
func (tk *execTakeover) take(t *testing.T) <-chan execOutcome {
	t.Helper()
	// The clock passes the lease and the first process's keeper does not
	// wake: a process that was paused.
	tk.first.clock.Advance(execTTL)
	outcome := tk.second.begin(t.Context(), tk.run.ID)
	<-tk.began2
	return outcome
}

// execState is everything a store holds for a run.
type execState struct {
	run       agent.Run
	steps     []agent.Step
	approvals []agent.Approval
}

func (f *execFixture) state(id string) execState {
	f.t.Helper()
	changes, err := f.memory.Changes(context.Background(), id, 0)
	require.NoError(f.t, err)
	return execState{run: changes.Run, steps: changes.Steps, approvals: changes.Approvals}
}

func TestExecute_ALeaseLostWhileAToolRuns(t *testing.T) {
	t.Run("the heartbeat stops the tool, and nothing more is written", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tk := newExecTakeover(t)
			second := tk.take(t)
			before := tk.first.state(tk.run.ID)
			require.Equal(t, "worker-2", before.run.LeaseOwner)
			require.Equal(t, 1, before.run.Failures, "the takeover is the failure counted")

			// The first process wakes, and its next heartbeat finds the lease
			// gone.
			asked := tk.first.wire.total()
			time.Sleep(execHeartbeat)
			synctest.Wait()
			got := <-tk.outcome

			require.ErrorIs(t, got.err, agent.ErrLeaseLost)
			assert.Equal(t, 2, tk.first.wire.total()-asked,
				"the heartbeat that found the lease gone and Execute's read of the run: no write is so much as tried")
			assert.Same(t, agent.ErrLeaseLost, <-tk.stopped, "the tool's context ends with the lost lease as its cause")
			assert.Equal(t, before, tk.first.state(tk.run.ID),
				"the first worker writes nothing: no result, no failure, and it does not give back a run that is not its own")
			lost := tk.first.logs.at(slog.LevelWarn)
			require.NotEmpty(t, lost)
			assert.Contains(t, lost[len(lost)-1], "agent: the run was lost to another worker; nothing more is written")

			close(tk.release2)
			ended := <-second
			require.NoError(t, ended.err)
			assert.Equal(t, agent.StatusCompleted, ended.run.Status)
			assert.Equal(t, "the second worker's result", tk.first.steps(tk.run.ID)[1].Result)
			ran := tk.calls.of("slow")
			require.Len(t, ran, 2)
			assert.Equal(t, ran[0].Key, ran[1].Key)
			assert.Equal(t, []int{1, 2}, []int{ran[0].Attempt, ran[1].Attempt})
		})
	})

	t.Run("a result that arrives before the heartbeat is refused by the store, and nothing more is written", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tk := newExecTakeover(t)
			second := tk.take(t)
			before := tk.first.state(tk.run.ID)

			// No time passes, so no heartbeat has told the first worker.
			asked := tk.first.wire.total()
			close(tk.release1)
			got := <-tk.outcome

			require.ErrorIs(t, got.err, agent.ErrLeaseLost)
			assert.Equal(t, 2, tk.first.wire.total()-asked,
				"the write the store refused and Execute's read of the run: once told, the worker tries nothing more")
			assert.Equal(t, before, tk.first.state(tk.run.ID))
			assert.Empty(t, tk.first.steps(tk.run.ID)[1].Result, "the first worker's result is on no record")

			close(tk.release2)
			ended := <-second
			require.NoError(t, ended.err)
			assert.Equal(t, "the second worker's result", tk.first.steps(tk.run.ID)[1].Result)
		})
	})

	t.Run("a store that cannot be reached for a whole lease ends the hold the same way", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			tk := newExecTakeover(t)
			tk.first.faults.FailBefore("Heartbeat", 100)
			before := tk.first.state(tk.run.ID)

			for range 3 {
				tk.first.pass(execHeartbeat)
			}
			got := <-tk.outcome

			require.ErrorIs(t, got.err, agent.ErrLeaseLost)
			assert.Same(t, agent.ErrLeaseLost, <-tk.stopped)
			assert.Equal(t, before, tk.first.state(tk.run.ID), "nothing is written, and the run is not given back")
			assert.Equal(t, "worker-1", before.run.LeaseOwner)

			// The lease has lapsed on the clock, and another worker takes it.
			second := tk.second.begin(t.Context(), tk.run.ID)
			<-tk.began2
			close(tk.release2)
			ended := <-second
			require.NoError(t, ended.err)
			assert.Equal(t, agent.StatusCompleted, ended.run.Status)
		})
	})
}

func TestExecute_ALeaseLostWhileTheModelIsAsked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		began, release := make(chan struct{}), make(chan struct{})
		asked := 0
		model := execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
			asked++
			if asked == 1 {
				close(began)
				<-release
				return agenttest.Say("the first worker's answer"), nil
			}
			return agenttest.Say("the second worker's answer"), nil
		})
		first := newExecFixture(t, execConfig{defs: []agent.Definition{execClerk()}, model: model})
		second := first.rival(execConfig{defs: []agent.Definition{execClerk()}, model: model})
		started := first.start("clerk", "what is the total?")
		outcome := first.begin(t.Context(), started.ID)
		<-began

		first.clock.Advance(execTTL)
		ended := second.execute(started.ID)
		require.Equal(t, "the second worker's answer", ended.Output)
		before := first.state(started.ID)

		close(release)
		got := <-outcome

		require.ErrorIs(t, got.err, agent.ErrLeaseLost)
		assert.Equal(t, before, first.state(started.ID), "the reply of a worker that lost the run is not recorded")
		assert.Equal(t, "the second worker's answer", got.run.Output, "Execute returns the run as it stands")
	})
}

func TestExecute_AWorkerThatLostTheRunAsksNothingOfItsChildren(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execLead(), execReviewer()},
			script: execLeadScript(execSummaries),
			over:   hooked(&store),
		})
		started := f.start("lead", "review the batch")
		require.Equal(t, agent.StatusWaiting, f.execute(started.ID).Status)
		require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))

		// The execution that would cancel the run is held up as it looks for
		// the children, for longer than its lease.
		proceed := make(chan struct{})
		store.on("ListRuns", func() { <-proceed })
		outcome := f.begin(t.Context(), started.ID)
		synctest.Wait()
		f.clock.Advance(execTTL)
		_, err := f.memory.Claim(t.Context(), agent.ClaimRequest{
			Owner: "worker-9", Agents: []string{"lead"}, RunID: started.ID, Now: f.clock.Now(), TTL: execTTL,
		})
		require.NoError(t, err)
		time.Sleep(execHeartbeat)
		synctest.Wait()
		before := f.state(started.ID)
		close(proceed)
		got := <-outcome

		require.ErrorIs(t, got.err, agent.ErrLeaseLost)
		for _, child := range f.children(started.ID) {
			assert.False(t, child.CancelRequested, "child %s", child.ID)
		}
		assert.Equal(t, before, f.state(started.ID))
		assert.Equal(t, "worker-9", f.run(started.ID).LeaseOwner)
	})
}

func TestExecute_AStoreThatFailsAndGoesOnWorking(t *testing.T) {
	// asking is a run whose one call the guard asks about; plain is one whose
	// one call is allowed.
	build := func(t *testing.T, guard agent.Guard) (*execFixture, *execCalls, agent.Run) {
		calls := &execCalls{}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
				agenttest.Say("the refund is settled"),
			),
		})
		return f, calls, f.start("clerk", "refund order 7")
	}
	asks := func() agent.Guard {
		return &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}}
	}

	t.Run("beginning a model call: the model is not asked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, started := build(t, nil)
			f.faults.FailBefore("BeginModel", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, "begin the model call at step 1: agenttest: store fault", got.Error)
			assert.Empty(t, got.LeaseOwner)
			require.NotNil(t, got.NextAttemptAt)
			assert.Empty(t, f.journal(started.ID))
			assert.Empty(t, f.model.Requests(), "the call is written down before it is made")
			assert.NotContains(t, f.events(), "step.started 1", "what was not written is not announced")

			f.clock.Advance(time.Second)
			assert.Equal(t, agent.StatusCompleted, f.execute(started.ID).Status)
		})
	})

	t.Run("recording a reply that in fact landed: the failures are counted from what the store holds", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			asked := 0
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("refund", nil))},
				model: execModelFunc(func(_ context.Context, req agent.Request) (agent.Response, error) {
					asked++
					switch {
					case asked <= 4:
						return agent.Response{}, errors.New("rate limited")
					case len(req.Messages) == 1:
						return agenttest.Use(agenttest.Call("call-1", "refund", `{}`)), nil
					}
					return agenttest.Say("the refund is settled"), nil
				}),
			})
			started := f.start("clerk", "refund order 7")
			for range 4 {
				_, err := f.engine.Execute(t.Context(), started.ID)
				require.Error(t, err)
				f.clock.Advance(time.Minute)
			}
			require.Equal(t, 4, f.run(started.ID).Failures, "one more failure would end the run")
			f.faults.FailAfter("CompleteModel", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, agent.StatusRunnable, got.Status,
				"the reply was recorded and reset the count, so this failure is the first and not the fifth")
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, []string{"1 model completed", "2 refund proposed"}, f.journal(started.ID))
			assert.NotContains(t, f.events(), "step.completed 1")

			f.clock.Advance(time.Second)
			got = f.execute(started.ID)
			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, 6, asked, "the reply that landed is not asked for again")
		})
	})

	t.Run("starting a call: the tool is not run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, started := build(t, &execGuard{})
			f.faults.FailBefore("UpdateStep", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, "move step 2 from proposed to started: agenttest: store fault", got.Error)
			assert.Equal(t, []string{"1 model completed", "2 refund proposed"}, f.journal(started.ID))
			assert.Empty(t, calls.of("refund"), "the start is written before the tool is run")
			assert.NotContains(t, f.events(), "step.started 2")

			f.clock.Advance(time.Second)
			assert.Equal(t, agent.StatusCompleted, f.execute(started.ID).Status)
			assert.Len(t, calls.of("refund"), 1)
		})
	})

	t.Run("recording a result: the call is made again with the same key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			var f *execFixture
			f = newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("refund", func(_ context.Context, in agent.Invocation) (string, error) {
					if in.Attempt == 1 {
						f.faults.FailBefore("UpdateStep", 1)
					}
					return "refunded", nil
				}))},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "refund", `{"order":7}`)),
					agenttest.Say("the refund is settled"),
				),
			})
			started := f.start("clerk", "refund order 7")

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, []string{"1 model completed", "2 refund started"}, f.journal(started.ID))

			f.clock.Advance(time.Second)
			got = f.execute(started.ID)
			assert.Equal(t, agent.StatusCompleted, got.Status)
			ran := calls.of("refund")
			require.Len(t, ran, 2)
			assert.Equal(t, ran[0].Key, ran[1].Key)
			assert.Equal(t, 2, ran[1].Attempt)
		})
	})

	t.Run("asking a person: the step stays proposed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, calls, started := build(t, asks())
			f.faults.FailBefore("RequestApproval", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, []string{"1 model completed", "2 refund proposed"}, f.journal(started.ID))
			assert.Empty(t, f.approvals(started.ID))
			assert.Empty(t, calls.of("refund"))
			assert.NotContains(t, f.events(), "approval.requested 2")
		})
	})

	t.Run("asking a person, when the question in fact landed: the same approval is found, not a second", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, started := build(t, asks())
			f.faults.FailAfter("RequestApproval", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, []string{"1 model completed", "2 refund waiting"}, f.journal(started.ID))
			require.Len(t, f.approvals(started.ID), 1)

			f.clock.Advance(time.Second)
			got = f.execute(started.ID)
			assert.Equal(t, agent.StatusWaiting, got.Status)
			assert.Len(t, f.approvals(started.ID), 1)
		})
	})

	t.Run("reading the run: the execution ends failed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, started := build(t, nil)
			f.faults.FailBefore("Changes", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, "read the run: agenttest: store fault", got.Error)
			assert.Empty(t, got.LeaseOwner)
		})
	})

	t.Run("reading a child: the execution ends failed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(execSummaries),
			})
			started := f.start("lead", "review the batch")
			f.execute(started.ID)
			steps := f.steps(started.ID)
			f.execute(steps[1].ChildRunID)
			f.faults.FailBefore("GetRun", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, 1, got.Failures)
			assert.Equal(t, fmt.Sprintf("read child run %s of step 2: agenttest: store fault", steps[1].ChildRunID), got.Error)
			assert.Equal(t, steps, f.steps(started.ID), "no child is collected from a read that failed")
		})
	})

	t.Run("recording a failure: nothing is written, and the lease is left to lapse", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, started := build(t, nil)
			f.faults.FailBefore("BeginModel", 1)
			f.faults.FailBefore("Yield", 1)

			_, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			require.ErrorContains(t, err, "begin the model call at step 1")
			require.ErrorContains(t, err, "give the run back as failed")
			got := f.run(started.ID)
			assert.Zero(t, got.Failures)
			assert.Equal(t, "worker-1", got.LeaseOwner)

			_, err = f.engine.Execute(t.Context(), started.ID)
			require.ErrorIs(t, err, agent.ErrNotClaimable, "until the lease lapses")
			f.clock.Advance(execTTL)
			assert.Equal(t, agent.StatusCompleted, f.execute(started.ID).Status)
		})
	})

	t.Run("reading the count of failures: nothing is written", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, started := build(t, nil)
			f.faults.FailBefore("BeginModel", 1)
			f.faults.FailBefore("GetRun", 1)

			_, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorContains(t, err, "begin the model call at step 1")
			require.ErrorContains(t, err, "read the run to record the failure")
			got := f.run(started.ID)
			assert.Zero(t, got.Failures)
			assert.Equal(t, "worker-1", got.LeaseOwner)
		})
	})

	t.Run("finishing: nothing is tried in its place, and a later execution finishes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execClerk()},
				script: agenttest.Replies(agenttest.Say("the total is 42")),
			})
			started := f.start("clerk", "what is the total?")
			f.faults.FailBefore("Finish", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			require.ErrorContains(t, err, "finish the run as completed")
			assert.Equal(t, agent.StatusRunnable, got.Status)
			assert.Equal(t, "worker-1", got.LeaseOwner, "the run is not given back as failed in place of the write that failed")
			assert.Zero(t, got.Failures)
			assert.Nil(t, got.NextAttemptAt)
			assert.NotContains(t, f.events(), "run.completed")

			f.clock.Advance(execTTL)
			got = f.execute(started.ID)
			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, "the total is 42", got.Output)
			assert.Len(t, f.model.Requests(), 1, "the reply the journal holds is not asked for again")
		})
	})

	t.Run("finishing, when the write in fact landed: Execute returns the run as it stands, and the error", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execClerk()},
				script: agenttest.Replies(agenttest.Say("the total is 42")),
			})
			started := f.start("clerk", "what is the total?")
			f.faults.FailAfter("Finish", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Equal(t, agent.StatusCompleted, got.Status)
			assert.Equal(t, "the total is 42", got.Output)
		})
	})

	t.Run("parking: the run is left to its lease, and a later execution parks", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f, _, started := build(t, asks())
			f.faults.FailBefore("Park", 1)

			got, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agenttest.ErrFault)
			require.ErrorContains(t, err, "park the run")
			assert.Equal(t, agent.StatusRunnable, got.Status)
			assert.Equal(t, "worker-1", got.LeaseOwner)
			assert.Zero(t, got.Failures)
			assert.NotContains(t, f.events(), "run.waiting")

			f.clock.Advance(execTTL)
			got = f.execute(started.ID)
			assert.Equal(t, agent.StatusWaiting, got.Status)
			assert.Len(t, f.approvals(started.ID), 1)
		})
	})

	t.Run("finding the children of a run to be cancelled: the run is not finished without them", func(t *testing.T) {
		for _, op := range []string{"ListRuns", "RequestCancel"} {
			t.Run(op, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newExecFixture(t, execConfig{
						defs:   []agent.Definition{execLead(), execReviewer()},
						script: execLeadScript(execSummaries),
					})
					started := f.start("lead", "review the batch")
					f.execute(started.ID)
					require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))
					f.faults.FailBefore(op, 1)

					got, err := f.engine.Execute(t.Context(), started.ID)

					require.ErrorIs(t, err, agenttest.ErrFault)
					require.ErrorContains(t, err, "ask the run's children to stop")
					assert.Equal(t, agent.StatusRunnable, got.Status,
						"a run that ended with children still running would leave nothing to stop them")
					assert.Equal(t, "worker-1", got.LeaseOwner)

					f.clock.Advance(execTTL)
					got = f.execute(started.ID)
					assert.Equal(t, agent.StatusCancelled, got.Status)
					for _, child := range f.children(started.ID) {
						assert.True(t, child.CancelRequested, "child %s", child.ID)
					}
				})
			})
		}
	})

	t.Run("a child that ended as it was asked to stop is passed over", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var store *execHooked
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(execSummaries),
				over:   hooked(&store),
			})
			started := f.start("lead", "review the batch")
			f.execute(started.ID)
			children := f.steps(started.ID)[1:]
			require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))
			// Every child ends between the listing and the request.
			ended := false
			store.on("RequestCancel", func() {
				if ended {
					return
				}
				ended = true
				other := f.rival(execConfig{defs: []agent.Definition{execReviewer()}})
				for _, st := range children {
					_, err := other.engine.Execute(t.Context(), st.ChildRunID)
					assert.NoError(t, err)
				}
			})

			got := f.execute(started.ID)

			assert.Equal(t, agent.StatusCancelled, got.Status, "ErrFinished from a child is no failure")
			for _, st := range children {
				child := f.run(st.ChildRunID)
				assert.Equal(t, agent.StatusCompleted, child.Status)
				assert.False(t, child.CancelRequested)
			}
		})
	})
}

func TestExecute_ACallersContextThatEnds(t *testing.T) {
	t.Run("before the claim: nothing is claimed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execClerk()},
				script: agenttest.Replies(agenttest.Say("done")),
			})
			started := f.start("clerk", "hello")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			got, err := f.engine.Execute(ctx, started.ID)

			require.ErrorIs(t, err, context.Canceled)
			assert.Zero(t, got)
			assert.Equal(t, started, f.run(started.ID))
		})
	})

	t.Run("as the run is claimed: it is given back untouched, with no failure", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execClerk()},
				script: agenttest.Replies(agenttest.Say("done")),
			})
			started := f.start("clerk", "hello")
			why := errors.New("the service is stopping")
			ctx, cancel := context.WithCancelCause(t.Context())
			f.wire.then("Claim", func() { cancel(why) })

			got, err := f.engine.Execute(ctx, started.ID)

			require.ErrorIs(t, err, why, "Execute says why it stopped")
			assert.Equal(t, agent.StatusRunnable, got.Status)
			assert.Empty(t, got.LeaseOwner)
			assert.Zero(t, got.Failures)
			assert.Nil(t, got.NextAttemptAt)
			assert.Empty(t, f.journal(started.ID))
			assert.Empty(t, f.model.Requests())
			assert.Empty(t, f.logs.at(slog.LevelWarn))
		})
	})

	t.Run("while a tool runs: the tool finishes and is recorded, and the run is given back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			type key struct{}
			began, release := make(chan struct{}), make(chan struct{})
			seen := make(chan any, 1)
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("slow", func(ctx context.Context, _ agent.Invocation) (string, error) {
					seen <- ctx.Value(key{})
					close(began)
					<-release
					return "done", ctx.Err()
				}))},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "slow", `{}`)),
					agenttest.Say("never said by this execution"),
				),
			})
			started := f.start("clerk", "take your time")
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), key{}, "carried"))
			outcome := f.begin(ctx, started.ID)
			<-began

			cancel()
			synctest.Wait()
			close(release)
			got := <-outcome

			require.ErrorIs(t, got.err, context.Canceled)
			assert.Equal(t, "carried", <-seen, "a step runs under its caller's values, without its caller's end")
			assert.Equal(t, agent.StatusRunnable, got.run.Status)
			assert.Empty(t, got.run.LeaseOwner)
			assert.Zero(t, got.run.Failures)
			assert.Equal(t, []string{"1 model completed", "2 slow completed"}, f.journal(started.ID))
			assert.Equal(t, "done", f.steps(started.ID)[1].Result)
			assert.Len(t, f.model.Requests(), 1)
		})
	})

	t.Run("while a tool runs that outlives the drain: nothing is written", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			began := make(chan struct{})
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("slow", func(ctx context.Context, _ agent.Invocation) (string, error) {
					close(began)
					<-ctx.Done()
					return "", ctx.Err()
				}))},
				script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "slow", `{}`))),
				tune:   func(o *agent.Options) { o.DrainTimeout = 3 * time.Second },
			})
			started := f.start("clerk", "take your time")
			ctx, cancel := context.WithCancel(t.Context())
			outcome := f.begin(ctx, started.ID)
			<-began
			before := f.state(started.ID)

			cancel()
			at := time.Now()
			got := <-outcome

			require.ErrorIs(t, got.err, agent.ErrDrained)
			assert.Equal(t, 3*time.Second, time.Since(at))
			assert.Equal(t, before, f.state(started.ID))
			assert.Equal(t, "worker-1", got.run.LeaseOwner)
		})
	})

	t.Run("while the guard is asked: no call is started", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			effect  agent.Effect
			journal []string
		}{
			{"one it allows stays proposed", agent.Allow, []string{"1 model completed", "2 refund proposed"}},
			{"one it blocks is recorded as blocked", agent.Block, []string{"1 model completed", "2 refund blocked"}},
			{"one it asks about is put to a person", agent.Ask, []string{"1 model completed", "2 refund waiting"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					calls := &execCalls{}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					f := newExecFixture(t, execConfig{
						defs: []agent.Definition{execClerk(calls.tool("refund", nil))},
						guard: agenttest.GuardFunc(func(context.Context, agent.Action) (agent.Decision, error) {
							cancel()
							return agent.Decision{Effect: tc.effect, Rule: "a-rule"}, nil
						}),
						script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "refund", `{}`))),
					})
					started := f.start("clerk", "refund order 7")

					got, err := f.engine.Execute(ctx, started.ID)

					require.ErrorIs(t, err, context.Canceled)
					assert.Equal(t, tc.journal, f.journal(started.ID))
					assert.Empty(t, calls.of("refund"), "no call is started that a shutdown would cut off")
					assert.Equal(t, agent.StatusRunnable, got.Status)
					assert.Empty(t, got.LeaseOwner)
					assert.Zero(t, got.Failures)
				})
			})
		}
	})
}

// The two sweeps below play one scripted batch that uses everything an
// execution can do: model calls, a plain tool, an at-most-once tool, a call
// the guard asks a person about, a delegation whose child runs a tool of its
// own, and a final answer built from every result. The first kills the store
// at every call in turn, as a crash would, and lets another process finish.
// The second fails every call in turn and lets the same process go on.

// crashDefinitions are the batch's two agents. Every tool records its
// invocations in calls and returns the same result every time.
func crashDefinitions(calls *execCalls) []agent.Definition {
	charge := calls.tool("charge", nil)
	charge.AtMostOnce = true
	return []agent.Definition{
		{Name: "lead", System: "run the batch", Tools: []agent.Tool{
			calls.tool("lookup", nil), charge, calls.tool("send", nil), {Name: "review", Delegate: "reviewer"},
		}},
		{Name: "reviewer", System: "summarise the document", Tools: []agent.Tool{calls.tool("save", nil)}},
	}
}

// crashScript is the model's part: the lead makes two calls, then two more,
// one of them a delegation, and then answers with every result it was last
// given; the reviewer saves and answers likewise.
func crashScript() agenttest.Script {
	told := func(req agent.Request) string {
		var said []string
		for _, result := range execResults(req) {
			said = append(said, result.Content)
		}
		return strings.Join(said, "; ")
	}
	return agenttest.ByAgent(map[string]agenttest.Script{
		"lead": func(req agent.Request, turn int) (agent.Response, error) {
			switch turn {
			case 0:
				return execSpent(agenttest.Use(
					agenttest.Call("call-1", "lookup", `{"order":7}`),
					agenttest.Call("call-2", "charge", `{"amount":1250}`),
				), agent.Usage{InputTokens: 100, OutputTokens: 20, CostMicros: 300}), nil
			case 1:
				return execSpent(agenttest.Use(
					agenttest.Call("call-3", "send", `{"to":"ops"}`),
					agenttest.Call("call-4", "review", `{"doc":1}`),
				), agent.Usage{InputTokens: 150, OutputTokens: 20, CostMicros: 400}), nil
			}
			return execSpent(agenttest.Say("done: "+told(req)), agent.Usage{InputTokens: 200, OutputTokens: 10, CostMicros: 450}), nil
		},
		"reviewer": func(req agent.Request, turn int) (agent.Response, error) {
			if turn == 0 {
				return execSpent(agenttest.Use(agenttest.Call("call-1", "save", `{"summary":"short"}`)),
					agent.Usage{InputTokens: 10, OutputTokens: 5, CostMicros: 7}), nil
			}
			return execSpent(agenttest.Say("saved: "+told(req)), agent.Usage{InputTokens: 12, OutputTokens: 3, CostMicros: 6}), nil
		},
	})
}

// crashConfig is the engine every process of a sweep runs.
func crashConfig(calls *execCalls) execConfig {
	return execConfig{
		defs:   crashDefinitions(calls),
		guard:  &execGuard{answers: map[string]agent.Decision{"send": {Effect: agent.Ask, Rule: "ask-first"}}},
		script: crashScript(),
	}
}

// claimable reports whether an engine could claim run now.
func (f *execFixture) claimable(run agent.Run) bool {
	now := f.clock.Now()
	return run.Status == agent.StatusRunnable &&
		(run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now)) &&
		(run.NextAttemptAt == nil || !run.NextAttemptAt.After(now))
}

// batch returns the batch's lead run, and false when it was never created.
func (f *execFixture) batch() (agent.Run, bool) {
	f.t.Helper()
	leads, err := f.memory.ListRuns(context.Background(), agent.RunFilter{Agent: "lead"})
	require.NoError(f.t, err)
	require.LessOrEqual(f.t, len(leads), 1, "the batch was started once per key")
	if len(leads) == 0 {
		return agent.Run{}, false
	}
	return leads[0], true
}

// childrenInOrder returns the children of run id in the order of the steps
// that started them, which is the same on every store.
func (f *execFixture) childrenInOrder(id string) []agent.Run {
	f.t.Helper()
	children := f.children(id)
	slices.SortFunc(children, func(a, b agent.Run) int { return a.ParentSeq - b.ParentSeq })
	return children
}

// crashRound is one round of everyone but the engine's own loop: the batch is
// started under its key, the lead is executed if it can be claimed, and
// otherwise each of its children that can be, in the order of their steps,
// and then whatever is asked is approved. It reports whether the lead has
// ended, whether anything could be done, and the first error an engine call
// returned, at which the round stops.
func crashRound(f *execFixture) (ended, acted bool, err error) {
	ctx := f.t.Context()
	lead, err := f.engine.Start(ctx, agent.StartRequest{Agent: "lead", Input: "run the batch", Key: "batch-1"})
	if err != nil {
		return false, true, err
	}
	lead = f.run(lead.ID)
	switch {
	case lead.Terminal():
		return true, false, nil
	case f.claimable(lead):
		_, err := f.engine.Execute(ctx, lead.ID)
		return false, true, err
	}
	for _, child := range f.childrenInOrder(lead.ID) {
		if !f.claimable(child) {
			continue
		}
		acted = true
		if _, err := f.engine.Execute(ctx, child.ID); err != nil {
			return false, true, err
		}
	}
	for _, approval := range f.approvals(lead.ID) {
		if approval.Status != agent.ApprovalPending {
			continue
		}
		acted = true
		if _, err := f.engine.Approve(ctx, approval.ID, "ops@example.test", "go on"); err != nil {
			return false, true, err
		}
	}
	return false, acted, nil
}

// crashDrive plays rounds until the lead ends or an engine call fails.
func crashDrive(f *execFixture) error {
	for range 40 {
		ended, acted, err := crashRound(f)
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

// crashOutcome is what a batch came to: what a resumed batch must match.
type crashOutcome struct {
	status     agent.Status
	output     string
	usage      agent.Usage
	modelCalls int
	// journals is each run's journal, the lead's under "lead" and a child's
	// under the step that started it.
	journals map[string][]string
	// keys is the idempotency key of every tool call that ran, with its
	// run's place in the batch for the run's id.
	keys []string
}

func (f *execFixture) crashOutcome(calls *execCalls) crashOutcome {
	f.t.Helper()
	lead, ok := f.batch()
	require.True(f.t, ok)
	out := crashOutcome{
		status: lead.Status, output: lead.Output, usage: lead.Usage, modelCalls: lead.ModelCalls,
		journals: map[string][]string{"lead": f.journal(lead.ID)},
	}
	// A run's id depends on which process made it, so a key is written with
	// its run's place in the batch in the id's stead.
	replace := []string{lead.ID, "lead"}
	for _, child := range f.childrenInOrder(lead.ID) {
		name := fmt.Sprintf("the child of step %d", child.ParentSeq)
		require.NotContains(f.t, out.journals, name, "one child for each delegating step")
		out.journals[name] = f.journal(child.ID)
		replace = append(replace, child.ID, name)
	}
	names := strings.NewReplacer(replace...)
	for key := range calls.byKey() {
		out.keys = append(out.keys, names.Replace(key))
	}
	slices.Sort(out.keys)
	return out
}

// byKey returns every invocation recorded, in order, under its idempotency
// key.
func (c *execCalls) byKey() map[string][]agent.Invocation {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]agent.Invocation{}
	for _, in := range c.seen {
		out[in.Key] = append(out[in.Key], in)
	}
	return out
}

// crashMoment is where every tool call stood at one moment: its step's
// status and attempts by its key, and how many times its tool had been
// invoked. A call the journal does not hold yet has no status.
type crashMoment struct {
	status   map[string]agent.StepStatus
	attempts map[string]int
	ran      map[string]int
	// children is how many child runs the lead had.
	children int
}

func (f *execFixture) crashMoment(calls *execCalls) crashMoment {
	f.t.Helper()
	at := crashMoment{status: map[string]agent.StepStatus{}, attempts: map[string]int{}, ran: map[string]int{}}
	for key, invocations := range calls.byKey() {
		at.ran[key] = len(invocations)
	}
	lead, ok := f.batch()
	if !ok {
		return at
	}
	children := f.children(lead.ID)
	at.children = len(children)
	for _, run := range append([]agent.Run{lead}, children...) {
		for _, st := range f.steps(run.ID) {
			if st.Kind == agent.StepTool {
				at.status[st.Key] = st.Status
				at.attempts[st.Key] = st.Attempts
			}
		}
	}
	return at
}

// crashReport lists what is wrong with a batch that was interrupted and has
// since ended: nothing, when it kept every promise of 6.8 of the design.
// interruptions is how many times an execution of it was cut short; killed is
// where each call stood when the first process's store died, and nil for a
// batch whose process lived.
func (f *execFixture) crashReport(calls *execCalls, want crashOutcome, killed *crashMoment, interruptions int) []string {
	f.t.Helper()
	var wrong []string
	say := func(format string, args ...any) { wrong = append(wrong, fmt.Sprintf(format, args...)) }

	// The same end as the batch nothing interrupted. Its usage is the same
	// too: a model call lost to a crash is on no record, and nothing is
	// counted twice.
	got := f.crashOutcome(calls)
	if !assert.ObjectsAreEqual(want, got) {
		say("the batch ended as %+v, and uninterrupted it ends as %+v", got, want)
	}

	// The journal has every step once, from 1 with no gaps.
	lead, _ := f.batch()
	end := f.crashMoment(calls)
	interrupted := map[int]int{}
	for _, approval := range f.approvals(lead.ID) {
		if approval.Cause == agent.CauseInterrupted {
			if approval.Status != agent.ApprovalApproved {
				say("the question about interrupted step %d was left %s", approval.Seq, approval.Status)
			}
			interrupted[approval.Seq]++
		}
	}
	for _, run := range append([]agent.Run{lead}, f.children(lead.ID)...) {
		for i, st := range f.steps(run.ID) {
			if st.Seq != i+1 {
				say("run %s has step %d at position %d", run.ID, st.Seq, i+1)
			}
		}
	}

	// No tool ran more often than its kind allows.
	for key, invocations := range calls.byKey() {
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
		// A plain tool runs at most once more for each interruption.
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
	for _, req := range f.model.Requests() {
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

// crashSeen counts, over a whole sweep, the interrupted batches in which each
// thing an interruption can cause was seen. A sweep that never saw one of
// them did not reach the states its assertions are about.
type crashSeen struct {
	// toolAgain: a tool was invoked a second time under one key.
	// askedAgain: a person was asked about an interrupted at-most-once call.
	// modelAgain: the model was sent the same conversation a second time.
	// childFound: a child was asked for a second time, and the one that
	// already existed was found.
	toolAgain, askedAgain, modelAgain, childFound int
}

// note counts what the interrupted batch f has just finished shows.
// childAskedTwice is the sweep's own knowledge of whether the delegating
// step's child was asked for twice.
func (seen *crashSeen) note(f *execFixture, calls *execCalls, childAskedTwice bool) {
	f.t.Helper()
	if childAskedTwice {
		seen.childFound++
	}
	lead, _ := f.batch()
	for _, invocations := range calls.byKey() {
		if len(invocations) > 1 {
			seen.toolAgain++
			break
		}
	}
	for _, approval := range f.approvals(lead.ID) {
		if approval.Cause == agent.CauseInterrupted {
			seen.askedAgain++
			break
		}
	}
	sent := map[string]bool{}
	for _, req := range f.model.Requests() {
		was := fmt.Sprintf("%s after %d messages", req.RunID, len(req.Messages))
		if sent[was] {
			seen.modelAgain++
			break
		}
		sent[was] = true
	}
}

func (seen crashSeen) check(t *testing.T) {
	t.Helper()
	assert.Positive(t, seen.toolAgain, "no interruption made a tool run again")
	assert.Positive(t, seen.askedAgain, "no interruption left the at-most-once call to be asked about")
	assert.Positive(t, seen.modelAgain, "no interruption made a model call again")
	assert.Positive(t, seen.childFound, "no interruption fell between a child being started and its step recording it")
	t.Logf("of those: a tool ran again in %d, a person was asked about the at-most-once call in %d, "+
		"a model call was made again in %d, a child was asked for twice in %d",
		seen.toolAgain, seen.askedAgain, seen.modelAgain, seen.childFound)
}

// crashUninterrupted plays the batch with nothing going wrong, and returns
// what it came to and how many store calls its process made.
func crashUninterrupted(t *testing.T, calls func(*execFixture) int) (want crashOutcome, total int) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		recorded := &execCalls{}
		f := newExecFixture(t, crashConfig(recorded))
		require.NoError(t, crashDrive(f))
		want = f.crashOutcome(recorded)
		total = calls(f)
		assert.Empty(t, f.crashReport(recorded, want, nil, 0), "the uninterrupted batch keeps every promise")
	})
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
	return want, total
}

func TestExecute_CrashAtEveryStoreCall(t *testing.T) {
	want, total := crashUninterrupted(t, func(f *execFixture) int { return f.faults.Calls() })
	require.Greater(t, total, 50, "the batch makes enough store calls for the sweep to mean something")

	points := 0
	var seen crashSeen
	for n := 1; n <= total; n++ {
		for _, flavour := range []string{"before", "after"} {
			points++
			t.Run(fmt.Sprintf("killed %s call %d", flavour, n), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					calls := &execCalls{}
					first := newExecFixture(t, crashConfig(calls))
					if flavour == "before" {
						first.faults.KillBefore(n)
					} else {
						first.faults.KillAfter(n)
					}

					err := crashDrive(first)
					require.ErrorIs(t, err, agenttest.ErrKilled, "the first process runs until its store dies")
					killed := first.crashMoment(calls)

					// Another process, on the same store and behind the same
					// model, once the dead one's leases have lapsed.
					resumed := crashConfig(calls)
					resumed.script = nil
					second := first.rival(resumed)
					first.clock.Advance(execTTL)
					require.NoError(t, crashDrive(second))

					assert.Empty(t, second.crashReport(calls, want, &killed, 1))
					lead, _ := second.batch()
					seen.note(second, calls,
						killed.children == 1 && killed.status[agent.StepKey(lead.ID, 6)] == agent.StepStarted)
				})
			})
		}
	}
	t.Logf("killed the store at %d points: before and after each of the %d store calls of the uninterrupted batch", points, total)
	seen.check(t)
}

func TestExecute_AStoreFailureAtEveryStoreCall(t *testing.T) {
	want, total := crashUninterrupted(t, func(f *execFixture) int {
		calls, _ := f.wire.count()
		return calls
	})
	require.Greater(t, total, 50)

	points := 0
	var seen crashSeen
	for n := 1; n <= total; n++ {
		for _, flavour := range []string{"before", "after"} {
			points++
			t.Run(fmt.Sprintf("failed %s call %d", flavour, n), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					calls := &execCalls{}
					f := newExecFixture(t, crashConfig(calls))
					f.wire.failAt(n, flavour == "after")

					// The same process goes on. What it cannot claim yet it
					// waits for: a back-off, or a lease it left to lapse.
					failures, ended := 0, false
					for round := 0; round < 60 && !ended; round++ {
						var acted bool
						var err error
						ended, acted, err = crashRound(f)
						switch {
						case err != nil:
							require.ErrorIs(t, err, errWire, "the one failure is the store's")
							failures++
						case !ended && !acted:
							f.clock.Advance(time.Minute)
						}
					}
					require.True(t, ended, "the batch ends: a store failure loses no run")
					require.LessOrEqual(t, failures, 1, "one store failure fails one call of the engine at most")

					assert.Empty(t, f.crashReport(calls, want, nil, 1))
					_, spawns := f.wire.count()
					seen.note(f, calls, spawns > 1)
				})
			})
		}
	}
	t.Logf("failed the store at %d points: before and after each of the %d store calls of the uninterrupted batch", points, total)
	seen.check(t)
}

func TestExecute_APanicThatEscapesTheExecutionStillStopsItsKeeper(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk()},
			model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
				panic("model exploded")
			}),
			over: hooked(&store),
		})
		started := f.start("clerk", "hello")
		// The store panics too, as the failure is being recorded.
		f.wire.hold("GetRun", func(context.Context) { panic("store exploded") })

		assert.PanicsWithValue(t, "store exploded", func() { _, _ = f.engine.Execute(t.Context(), started.ID) })

		f.wire.hold("GetRun", nil)
		f.pass(3 * execHeartbeat)
		_, beats, _ := store.count()
		assert.Zero(t, beats, "no keeper is left extending the lease of a run nobody is executing")
		got := f.run(started.ID)
		assert.Equal(t, "worker-1", got.LeaseOwner)
		assert.Equal(t, execStart.Add(execTTL), *got.LeaseExpiresAt, "the lease lapses, and another worker takes the run")
	})
}

func TestExecute_AnExecutionThatLearnsBetweenTwoActionsThatItsLeaseIsGoneBeginsNoOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		guard := &execGuard{}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("lookup", nil), calls.tool("refund", nil))},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{}`), agenttest.Call("call-2", "refund", `{}`)),
				agenttest.Say("never said by this worker"),
			),
		})
		started := f.start("clerk", "refund order 7")
		// As the first call's result is recorded, the process is paused past
		// its lease, another takes the run, and the heartbeat says so.
		taken, asked := false, 0
		f.wire.then("UpdateStep", func() {
			if taken || f.steps(started.ID)[1].Status != agent.StepCompleted {
				return
			}
			taken = true
			f.clock.Advance(execTTL)
			_, err := f.memory.Claim(t.Context(), agent.ClaimRequest{
				Owner: "worker-9", Agents: []string{"clerk"}, RunID: started.ID, Now: f.clock.Now(), TTL: execTTL,
			})
			assert.NoError(t, err)
			time.Sleep(execHeartbeat)
			synctest.Wait()
			asked = f.wire.total()
		})

		_, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorIs(t, err, agent.ErrLeaseLost)
		require.True(t, taken)
		assert.Len(t, guard.questions(), 1, "the guard is not asked about the next call by a worker that knows the run is not its own")
		assert.Equal(t, 1, f.wire.total()-asked, "Execute's read of the run, and no other call so much as tried")
		assert.Equal(t, []string{"1 model completed", "2 lookup completed", "3 refund proposed"}, f.journal(started.ID))
		assert.Equal(t, "worker-9", f.run(started.ID).LeaseOwner)
		assert.Empty(t, calls.of("refund"))
	})
}

func TestExecute_ACallInFlightWhenTheLeaseIsLostIsCutOff(t *testing.T) {
	// Each of these is something an execution does while it works: a call to
	// the store, the model or the guard. Whichever is in flight when another
	// process takes the run is cut off by its context, with the lost lease as
	// the cause, and the execution writes nothing more. The last two are made
	// only by an execution that ends a run with children, which the second
	// execution of the batch does.
	for _, tc := range []struct {
		op     string
		second bool
		// forever gives the run no time limit, so that its model call has no
		// deadline of the budget's.
		forever bool
	}{
		{op: "Changes"}, {op: "BeginModel"}, {op: "Generate"}, {op: "Generate", forever: true},
		{op: "CompleteModel"}, {op: "Decide"},
		{op: "UpdateStep"}, {op: "RequestApproval"}, {op: "CreateRun"}, {op: "GetRun"},
		{op: "ListRuns", second: true}, {op: "RequestCancel", second: true},
	} {
		name := tc.op
		if tc.forever {
			name += ", for a run with no time limit"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var f *execFixture
				var started agent.Run
				cut := make(chan error, 1)
				armed, after := false, 0
				// inFlight is the call, held up until its context ends or an
				// hour has passed. Meanwhile the process is as one paused
				// past its lease: another takes the run, and the heartbeat
				// says so.
				inFlight := func(ctx context.Context) {
					if !armed {
						return
					}
					armed = false
					f.clock.Advance(execTTL)
					_, err := f.memory.Claim(t.Context(), agent.ClaimRequest{
						Owner: "worker-9", Agents: []string{"lead"}, RunID: started.ID, Now: f.clock.Now(), TTL: execTTL,
					})
					assert.NoError(t, err)
					select {
					case <-ctx.Done():
						cut <- context.Cause(ctx)
					case <-time.After(time.Hour):
						cut <- nil
					}
					after = f.wire.total()
				}

				calls := &execCalls{}
				lead := execLead()
				lead.Tools = append(lead.Tools, calls.tool("lookup", nil), calls.tool("send", nil))
				if tc.forever {
					lead.Limits = agent.Limits{MaxDuration: -1}
				}
				script := agenttest.NewModel(agenttest.ByAgent(map[string]agenttest.Script{
					"lead": agenttest.Replies(agenttest.Use(
						agenttest.Call("call-1", "lookup", `{}`),
						agenttest.Call("call-2", "send", `{}`),
						agenttest.Call("call-3", "review", `{"doc":1}`),
					)),
				}))
				answers := &execGuard{answers: map[string]agent.Decision{"send": {Effect: agent.Ask, Rule: "ask-first"}}}
				f = newExecFixture(t, execConfig{
					defs: []agent.Definition{lead, execReviewer()},
					model: execModelFunc(func(ctx context.Context, req agent.Request) (agent.Response, error) {
						if tc.op == "Generate" {
							inFlight(ctx)
						}
						return script.Generate(ctx, req)
					}),
					guard: agenttest.GuardFunc(func(ctx context.Context, a agent.Action) (agent.Decision, error) {
						if tc.op == "Decide" {
							inFlight(ctx)
						}
						return answers.Decide(ctx, a)
					}),
				})
				started = f.start("lead", "run the batch")
				f.wire.hold(tc.op, inFlight)
				if tc.second {
					require.Equal(t, agent.StatusWaiting, f.execute(started.ID).Status)
					require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))
				}
				armed = true

				_, err := f.engine.Execute(t.Context(), started.ID)

				require.False(t, armed, "the execution made the call")
				assert.Same(t, agent.ErrLeaseLost, <-cut, "the call's context ends when the lease is lost, and says so")
				require.ErrorIs(t, err, agent.ErrLeaseLost)
				got := f.run(started.ID)
				assert.Equal(t, "worker-9", got.LeaseOwner, "the run is not given back by a worker that lost it")
				assert.Equal(t, 1, got.Failures, "and no failure is recorded but the takeover's")
				for _, child := range f.children(started.ID) {
					assert.False(t, child.CancelRequested)
				}
				assert.Equal(t, 1, f.wire.total()-after,
					"once the call is cut off the worker asks the store for nothing but the run Execute returns")
			})
		})
	}
}

func TestExecute_AWaitingStepWhoseChildIsNotThereGivesTheRunUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execLead(), execReviewer()},
			script: agenttest.ByAgent(map[string]agenttest.Script{
				"lead":     agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "review", `{"doc":1}`))),
				"reviewer": execSummaries,
			}),
			over: hooked(&store),
		})
		started := f.start("lead", "review the batch")
		f.execute(started.ID)
		f.execute(f.steps(started.ID)[1].ChildRunID)
		// The step names a run no store has.
		missing := execID(9, 9)
		store.changes = func(c *agent.Changes) {
			for i := range c.Steps {
				if c.Steps[i].ChildRunID != "" {
					c.Steps[i].ChildRunID = missing
				}
			}
		}

		got, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorContains(t, err, "step 2 waits on child run "+missing+", which is not among the run's children")
		assert.Equal(t, agent.StatusRunnable, got.Status, "a run that parked on a child nobody can find would never be woken")
		assert.Equal(t, 1, got.Failures)
		require.NotNil(t, got.NextAttemptAt)
		assert.Equal(t, execStart.Add(time.Minute), *got.NextAttemptAt, "the planner's give-up, with the longest wait")
		assert.Equal(t, []string{"1 model completed", "2 review waiting"}, f.journal(started.ID))
	})
}

func TestExecute_ARunThatCannotBeReadBackIsAnError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk()},
			script: agenttest.Replies(agenttest.Say("done")),
		})
		started := f.start("clerk", "hello")
		// The one read of the run this execution makes is Execute's, of the
		// run it returns.
		f.faults.FailBefore("GetRun", 1)

		got, err := f.engine.Execute(t.Context(), started.ID)

		require.ErrorIs(t, err, agenttest.ErrFault)
		require.ErrorContains(t, err, "read it back")
		assert.Zero(t, got)
		assert.Equal(t, agent.StatusCompleted, f.run(started.ID).Status, "the execution itself went well")
	})
}

func TestExecute_TheDrainRunsOut(t *testing.T) {
	const drain = 3 * time.Second

	t.Run("while a result is being recorded: once it is, the run is given back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("lookup", nil))},
				script: agenttest.Replies(
					agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)),
					agenttest.Say("never said by this execution"),
				),
				tune: func(o *agent.Options) { o.DrainTimeout = drain },
			})
			started := f.start("clerk", "hello")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// The caller stops as the result is written, and the store takes
			// longer than the drain to say that it was.
			f.wire.then("UpdateStep", func() {
				if f.steps(started.ID)[1].Status == agent.StepCompleted {
					cancel()
					time.Sleep(drain + time.Second)
				}
			})

			got, err := f.engine.Execute(ctx, started.ID)

			require.ErrorIs(t, err, agent.ErrDrained)
			assert.Equal(t, []string{"1 model completed", "2 lookup completed"}, f.journal(started.ID))
			assert.Empty(t, got.LeaseOwner, "no tool is running, so the run is given back")
			assert.Zero(t, got.Failures)
			assert.Nil(t, got.NextAttemptAt)
		})
	})

	t.Run("while a store call is in flight: the run is given back", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execClerk()},
				script: agenttest.Replies(agenttest.Say("never said by this execution")),
				tune:   func(o *agent.Options) { o.DrainTimeout = drain },
			})
			started := f.start("clerk", "hello")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// The store does not answer until the call's context ends.
			f.wire.hold("BeginModel", func(held context.Context) {
				cancel()
				<-held.Done()
			})
			at := time.Now()

			got, err := f.engine.Execute(ctx, started.ID)

			require.ErrorIs(t, err, agent.ErrDrained)
			assert.Equal(t, drain, time.Since(at))
			assert.Empty(t, got.LeaseOwner)
			assert.Zero(t, got.Failures, "a call the shutdown cut off is not the run's failure")
			assert.Empty(t, f.journal(started.ID))
		})
	})

	t.Run("while the guard is asked: the run is given back, and the step stays proposed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("refund", nil))},
				guard: agenttest.GuardFunc(func(asked context.Context, _ agent.Action) (agent.Decision, error) {
					cancel()
					<-asked.Done()
					return agent.Decision{}, asked.Err()
				}),
				script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "refund", `{}`))),
				tune:   func(o *agent.Options) { o.DrainTimeout = drain },
			})
			started := f.start("clerk", "refund order 7")

			got, err := f.engine.Execute(ctx, started.ID)

			require.ErrorIs(t, err, agent.ErrDrained)
			assert.Equal(t, []string{"1 model completed", "2 refund proposed"}, f.journal(started.ID))
			assert.Empty(t, got.LeaseOwner)
			assert.Zero(t, got.Failures)
			assert.Empty(t, calls.of("refund"))
		})
	})
}

func TestExecute_ParkRefusedAtOneRevisionAndThenAtAnotherIsNotASpin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
			guard: &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}},
			script: agenttest.Replies(agenttest.Use(
				agenttest.Call("call-1", "refund", `{"order":7}`),
				agenttest.Call("call-2", "refund", `{"order":8}`),
				agenttest.Call("call-3", "refund", `{"order":9}`),
			)),
			over: hooked(&store),
		})
		started := f.start("clerk", "refund three orders")
		// Twice the run is about to park and a person declines one of the
		// calls first, which the store answers by refusing the park. The
		// third time nothing has changed, and the run parks.
		store.refusePark(true)
		parks := 0
		store.on("Park", func() {
			parks++
			if parks > 2 {
				return
			}
			_, err := f.engine.Decline(t.Context(), f.approvals(started.ID)[parks-1].ID, "ops@example.test", "not this one")
			assert.NoError(t, err)
			store.refusePark(parks < 2)
		})

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusWaiting, got.Status, "each refusal came with a change, so none of them is a spin")
		assert.Zero(t, got.Failures)
		assert.Equal(t, 3, parks)
		assert.Equal(t, []string{"1 model completed", "2 refund declined", "3 refund declined", "4 refund waiting"},
			f.journal(started.ID))
	})
}

func TestExecute_ACallInFlightWhenCancellationIsRequestedIsCutOffAndTheRunIsCancelled(t *testing.T) {
	// The same calls as when a lease is lost. A request to cancel ends the
	// call's context too, with its own cause, and then the run is the
	// execution's to finish.
	for _, op := range []string{
		"Changes", "BeginModel", "Generate", "CompleteModel", "Decide",
		"UpdateStep", "RequestApproval", "CreateRun", "GetRun",
	} {
		t.Run(op, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var f *execFixture
				var started agent.Run
				cut := make(chan error, 1)
				armed := false
				inFlight := func(ctx context.Context) {
					if !armed {
						return
					}
					armed = false
					assert.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))
					select {
					case <-ctx.Done():
						cut <- context.Cause(ctx)
					case <-time.After(time.Hour):
						cut <- nil
					}
				}

				calls := &execCalls{}
				lead := execLead()
				lead.Tools = append(lead.Tools, calls.tool("lookup", nil), calls.tool("send", nil))
				script := agenttest.NewModel(agenttest.ByAgent(map[string]agenttest.Script{
					"lead": agenttest.Replies(agenttest.Use(
						agenttest.Call("call-1", "lookup", `{}`),
						agenttest.Call("call-2", "send", `{}`),
						agenttest.Call("call-3", "review", `{"doc":1}`),
					)),
				}))
				answers := &execGuard{answers: map[string]agent.Decision{"send": {Effect: agent.Ask, Rule: "ask-first"}}}
				f = newExecFixture(t, execConfig{
					defs: []agent.Definition{lead, execReviewer()},
					model: execModelFunc(func(ctx context.Context, req agent.Request) (agent.Response, error) {
						if op == "Generate" {
							inFlight(ctx)
						}
						return script.Generate(ctx, req)
					}),
					guard: agenttest.GuardFunc(func(ctx context.Context, a agent.Action) (agent.Decision, error) {
						if op == "Decide" {
							inFlight(ctx)
						}
						return answers.Decide(ctx, a)
					}),
				})
				started = f.start("lead", "run the batch")
				f.wire.hold(op, inFlight)
				armed = true

				got, err := f.engine.Execute(t.Context(), started.ID)

				require.False(t, armed, "the execution made the call")
				assert.Same(t, agent.ErrCancelRequested, <-cut, "the call's context ends when cancellation is requested, and says so")
				require.NoError(t, err)
				assert.Equal(t, agent.StatusCancelled, got.Status, "the execution that was told finishes the run")
				assert.Empty(t, got.LeaseOwner)
				assert.Zero(t, got.Failures)
				for _, child := range f.children(started.ID) {
					assert.True(t, child.CancelRequested, "a child already started is asked to stop")
				}
			})
		})
	}
}

func TestExecute_ALastWriteRefusedForALostLeaseIsALostLeaseAndNoMoreIsTried(t *testing.T) {
	for _, tc := range []struct {
		op    string
		reply agent.Response
		err   error
	}{
		{op: "Finish", reply: agenttest.Say("done")},
		{op: "Park", reply: agenttest.Use(agenttest.Call("call-1", "send", `{}`))},
		{op: "Yield", err: errors.New("rate limited")},
	} {
		t.Run(tc.op, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				send := calls.tool("send", nil)
				send.Approval = true
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk(send)},
					model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
						return tc.reply, tc.err
					}),
				})
				started := f.start("clerk", "hello")
				// Another process takes the run as the write is on its way. The
				// keeper has been stopped by then, so only the store can say.
				asked := 0
				f.wire.hold(tc.op, func(context.Context) {
					f.clock.Advance(execTTL)
					_, err := f.memory.Claim(t.Context(), agent.ClaimRequest{
						Owner: "worker-9", Agents: []string{"clerk"}, RunID: started.ID, Now: f.clock.Now(), TTL: execTTL,
					})
					assert.NoError(t, err)
					asked = f.wire.total()
				})

				got, err := f.engine.Execute(t.Context(), started.ID)

				require.ErrorIs(t, err, agent.ErrLeaseLost)
				assert.Equal(t, "worker-9", got.LeaseOwner)
				assert.Equal(t, agent.StatusRunnable, got.Status)
				assert.Equal(t, 1, f.wire.total()-asked, "Execute's read of the run, and no other call")
				warned := f.logs.at(slog.LevelWarn)
				require.NotEmpty(t, warned)
				assert.Contains(t, warned[len(warned)-1], "agent: the run was lost to another worker; nothing more is written")
			})
		})
	}
}

func TestExecute_OverAStoreThatIgnoresItsContext(t *testing.T) {
	// MemoryStore takes a call whatever has become of its context. Over such
	// a store nothing stops an execution that has been told to stop but its
	// own look at its hold.

	t.Run("a worker that lost the run while reading it does not go looking for its children", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(execSummaries),
				lax:    true,
			})
			started := f.start("lead", "review the batch")
			require.Equal(t, agent.StatusWaiting, f.execute(started.ID).Status)
			require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))
			taken := false
			f.wire.hold("Changes", func(context.Context) {
				if taken {
					return
				}
				taken = true
				f.clock.Advance(execTTL)
				_, err := f.memory.Claim(t.Context(), agent.ClaimRequest{
					Owner: "worker-9", Agents: []string{"lead"}, RunID: started.ID, Now: f.clock.Now(), TTL: execTTL,
				})
				assert.NoError(t, err)
				time.Sleep(execHeartbeat)
				synctest.Wait()
			})
			listed := f.wire.made("ListRuns")

			_, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agent.ErrLeaseLost)
			assert.Equal(t, listed, f.wire.made("ListRuns"), "the children are not so much as listed")
			for _, child := range f.children(started.ID) {
				assert.False(t, child.CancelRequested, "child %s", child.ID)
			}
			assert.Equal(t, "worker-9", f.run(started.ID).LeaseOwner)
		})
	})

	t.Run("a worker that loses the run between two children asks nothing of the second", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{execLead(), execReviewer()},
				script: execLeadScript(execSummaries),
				lax:    true,
			})
			started := f.start("lead", "review the batch")
			require.Equal(t, agent.StatusWaiting, f.execute(started.ID).Status)
			require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "wrong batch"))
			f.wire.then("RequestCancel", func() {
				f.wire.then("RequestCancel", nil)
				f.clock.Advance(execTTL)
				_, err := f.memory.Claim(t.Context(), agent.ClaimRequest{
					Owner: "worker-9", Agents: []string{"lead"}, RunID: started.ID, Now: f.clock.Now(), TTL: execTTL,
				})
				assert.NoError(t, err)
				time.Sleep(execHeartbeat)
				synctest.Wait()
			})
			asked := f.wire.made("RequestCancel")

			_, err := f.engine.Execute(t.Context(), started.ID)

			require.ErrorIs(t, err, agent.ErrLeaseLost)
			assert.Equal(t, 1, f.wire.made("RequestCancel")-asked)
			marked := 0
			for _, child := range f.children(started.ID) {
				if child.CancelRequested {
					marked++
				}
			}
			assert.Equal(t, 1, marked, "the child asked before the run was lost, and no other")
			assert.Equal(t, agent.StatusRunnable, f.run(started.ID).Status)
		})
	})

	t.Run("a reply that arrives after cancellation was requested is not recorded", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			began, release := make(chan struct{}), make(chan struct{})
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk()},
				model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
					close(began)
					<-release
					return agenttest.Say("too late"), nil
				}),
				lax: true,
			})
			started := f.start("clerk", "hello")
			outcome := f.begin(t.Context(), started.ID)
			<-began
			require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "enough"))
			f.pass(execHeartbeat)

			close(release)
			got := <-outcome

			require.NoError(t, got.err)
			assert.Equal(t, agent.StatusCancelled, got.run.Status)
			assert.Equal(t, []string{"1 model started"}, f.journal(started.ID))
			assert.Zero(t, got.run.ModelCalls)
		})
	})

	t.Run("a decision that arrives after cancellation was requested starts nothing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			calls := &execCalls{}
			began, release := make(chan struct{}), make(chan struct{})
			f := newExecFixture(t, execConfig{
				defs: []agent.Definition{execClerk(calls.tool("refund", nil))},
				guard: agenttest.GuardFunc(func(context.Context, agent.Action) (agent.Decision, error) {
					close(began)
					<-release
					return agent.Decision{Effect: agent.Allow, Rule: "allow-all"}, nil
				}),
				script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "refund", `{}`))),
				lax:    true,
			})
			started := f.start("clerk", "refund order 7")
			outcome := f.begin(t.Context(), started.ID)
			<-began
			require.NoError(t, f.engine.Cancel(t.Context(), started.ID, "ops@example.test", "enough"))
			f.pass(execHeartbeat)

			close(release)
			got := <-outcome

			require.NoError(t, got.err)
			assert.Equal(t, agent.StatusCancelled, got.run.Status)
			assert.Equal(t, []string{"1 model completed", "2 refund proposed"}, f.journal(started.ID),
				"the call is not started for a run that is being cancelled")
			assert.Empty(t, calls.of("refund"))
		})
	})
}
