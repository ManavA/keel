package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

var engineStart = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// The methods agent/httpapi asks of an engine, as its Runs interface lists
// them. That package's own check arrives with the example; this one fails
// here, where a changed signature would be made.
var _ interface {
	GetRun(ctx context.Context, id string) (agent.Run, error)
	ListRuns(ctx context.Context, f agent.RunFilter) ([]agent.Run, error)
	Changes(ctx context.Context, runID string, since int64) (agent.Changes, error)
	ListApprovals(ctx context.Context, f agent.ApprovalFilter) ([]agent.Approval, error)
	Approve(ctx context.Context, approvalID, by, reason string) (agent.Approval, error)
	Decline(ctx context.Context, approvalID, by, reason string) (agent.Approval, error)
	Cancel(ctx context.Context, runID, by, reason string) error
} = (*agent.Engine)(nil)

// engineID is the nth id of a test: a UUID, since a store keeps nothing else.
func engineID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// engineBus records what an engine publishes, and fails while err is set.
type engineBus struct {
	mu     sync.Mutex
	err    error
	topics []string
	events []any
}

func (b *engineBus) Publish(_ context.Context, topic string, event any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	b.topics = append(b.topics, topic)
	b.events = append(b.events, event)
	return nil
}

func (b *engineBus) published() []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]any(nil), b.events...)
}

func (b *engineBus) forget() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topics, b.events = nil, nil
}

func (b *engineBus) fail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// engineLogs is a slog.Handler that keeps every record at every level.
type engineLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *engineLogs) Enabled(context.Context, slog.Level) bool { return true }

func (l *engineLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	return nil
}

func (l *engineLogs) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *engineLogs) WithGroup(string) slog.Handler      { return l }

// at returns the records logged at level, each as its message and attributes
// on one line.
func (l *engineLogs) at(level slog.Level) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.records {
		if r.Level != level {
			continue
		}
		line := r.Message
		r.Attrs(func(a slog.Attr) bool {
			line += " " + a.String()
			return true
		})
		out = append(out, line)
	}
	return out
}

// engineFixture is an engine over a store that can be made to fail, a clock
// the test moves, and a bus and a log that keep what they are given.
type engineFixture struct {
	engine *agent.Engine
	store  *agenttest.FaultStore
	clock  *agenttest.Clock
	bus    *engineBus
	logs   *engineLogs
	model  *agenttest.Model
}

func newEngineFixture(t *testing.T, defs ...agent.Definition) *engineFixture {
	t.Helper()
	f := &engineFixture{
		store: agenttest.NewFaultStore(agent.NewMemoryStore()),
		clock: agenttest.NewClock(engineStart),
		bus:   &engineBus{},
		logs:  &engineLogs{},
		model: agenttest.NewModel(nil),
	}
	var ids atomic.Int64
	engine, err := agent.New(agent.Options{
		Model:  f.model,
		Store:  f.store,
		Clock:  f.clock,
		Events: f.bus,
		Logger: slog.New(f.logs),
		NewID:  func() string { return engineID(int(ids.Add(1))) },
	})
	require.NoError(t, err)
	f.engine = engine
	for _, def := range defs {
		require.NoError(t, engine.Register(def))
	}
	return f
}

func engineTool(context.Context, agent.Invocation) (string, error) { return "ok", nil }

// engineDefinition is an agent with its tools out of alphabetical order.
func engineDefinition(name string) agent.Definition {
	return agent.Definition{
		Name:   name,
		System: "be exact",
		Model:  "model-a",
		Tools: []agent.Tool{
			{Name: "zeta", Description: "the last tool", Schema: json.RawMessage(`{"type":"object","required":["order"]}`), Run: engineTool},
			{Name: "alpha", Description: "the first tool", Run: engineTool, Approval: true, AtMostOnce: true, Timeout: time.Second},
			{Name: "mid", Description: "hands the work on", Delegate: "helper"},
		},
		Output:    json.RawMessage(`{"type":"object","required":["total"]}`),
		MaxTokens: 2048,
	}
}

// parked starts a run of the agent clerk and takes it, through the store, to
// where an executor would leave it: one tool call put to a person, who has
// an hour to answer, and the run parked. What was published on the way is
// forgotten.
func (f *engineFixture) parked(t *testing.T) (agent.Run, agent.Approval) {
	t.Helper()
	ctx := t.Context()
	run, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk", Input: "refund order 7"})
	require.NoError(t, err)

	now := f.clock.Now()
	claimed, err := f.store.Claim(ctx, agent.ClaimRequest{
		Owner: "executor", Agents: []string{"clerk"}, RunID: run.ID, Now: now, TTL: time.Minute,
	})
	require.NoError(t, err)
	lease := claimed.Lease()
	require.NoError(t, f.store.BeginModel(ctx, lease, 1, now))
	reply := agenttest.Use(agenttest.Call("call-1", "alpha", `{"order":7}`))
	require.NoError(t, f.store.CompleteModel(ctx, lease, agent.CompleteModelRequest{
		Seq: 1, Message: reply.Message, Stop: reply.Stop, Model: "model-a", Now: now,
	}))
	lapses := now.Add(time.Hour)
	approval, err := f.store.RequestApproval(ctx, lease, agent.ApprovalRequest{
		ID: engineID(900_000), Seq: 2, From: agent.StepProposed, Cause: agent.CauseGuard,
		Action: agent.Action{Kind: "run", Target: "alpha"}, Decision: agent.Ask, Rule: "ask-first",
		ExpiresAt: &lapses, Now: now,
	})
	require.NoError(t, err)
	parked, err := f.store.Park(ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
	require.NoError(t, err)
	require.True(t, parked)

	run, err = f.store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, agent.StatusWaiting, run.Status)
	f.bus.forget()
	return run, approval
}

func TestNew(t *testing.T) {
	t.Run("a nil Model is refused", func(t *testing.T) {
		engine, err := agent.New(agent.Options{Store: agent.NewMemoryStore()})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "Model")
		assert.Nil(t, engine)
	})

	t.Run("a Model is all it needs", func(t *testing.T) {
		engine, err := agent.New(agent.Options{Model: agenttest.NewModel(nil)})
		require.NoError(t, err)
		require.NoError(t, engine.Register(engineDefinition("clerk")))

		run, err := engine.Start(t.Context(), agent.StartRequest{Agent: "clerk", Input: "refund order 7"})
		require.NoError(t, err)

		parsed, err := uuid.Parse(run.ID)
		require.NoError(t, err, "the default NewID gave %q", run.ID)
		assert.Equal(t, parsed.String(), run.ID, "the id is not in the form a store keeps")
		assert.False(t, run.CreatedAt.IsZero(), "the default clock gave no time")
		assert.Equal(t, time.UTC, run.CreatedAt.Location())

		got, err := engine.GetRun(t.Context(), run.ID)
		require.NoError(t, err, "the default store did not keep the run")
		assert.Equal(t, run, got)
	})
}

func TestEngine_Register_Refuses(t *testing.T) {
	tool := func(change func(*agent.Tool)) agent.Definition {
		def := agent.Definition{Name: "clerk", Tools: []agent.Tool{{Name: "alpha", Run: engineTool}}}
		change(&def.Tools[0])
		return def
	}
	cases := []struct {
		name string
		def  agent.Definition
		want string
	}{
		{
			name: "an empty name",
			def:  agent.Definition{Tools: []agent.Tool{{Name: "alpha", Run: engineTool}}},
			want: "name is empty",
		},
		{
			name: "a tool with no name",
			def:  tool(func(tl *agent.Tool) { tl.Name = "" }),
			want: `tool "": name does not match`,
		},
		{
			name: "a tool name with a space in it",
			def:  tool(func(tl *agent.Tool) { tl.Name = "look up" }),
			want: `tool "look up": name does not match`,
		},
		{
			name: "a tool name with a dot in it",
			def:  tool(func(tl *agent.Tool) { tl.Name = "orders.lookup" }),
			want: `tool "orders.lookup": name does not match`,
		},
		{
			name: "a tool name that only starts well",
			def:  tool(func(tl *agent.Tool) { tl.Name = "lookup!" }),
			want: `tool "lookup!": name does not match`,
		},
		{
			name: "a tool name that only ends well",
			def:  tool(func(tl *agent.Tool) { tl.Name = "!lookup" }),
			want: `tool "!lookup": name does not match`,
		},
		{
			name: "a tool name of 65 characters",
			def:  tool(func(tl *agent.Tool) { tl.Name = strings.Repeat("a", 65) }),
			want: "name does not match",
		},
		{
			name: "a tool name that appears twice",
			def: agent.Definition{Name: "clerk", Tools: []agent.Tool{
				{Name: "alpha", Run: engineTool},
				{Name: "beta", Run: engineTool},
				{Name: "alpha", Delegate: "helper"},
			}},
			want: `tool "alpha": name appears twice`,
		},
		{
			name: "a tool with both Run and Delegate",
			def:  tool(func(tl *agent.Tool) { tl.Delegate = "helper" }),
			want: `tool "alpha": has both Run and Delegate`,
		},
		{
			name: "a tool with neither Run nor Delegate",
			def:  tool(func(tl *agent.Tool) { tl.Run = nil }),
			want: `tool "alpha": has neither Run nor Delegate`,
		},
		{
			name: "a tool schema that is not valid JSON",
			def:  tool(func(tl *agent.Tool) { tl.Schema = json.RawMessage(`{"type":`) }),
			want: `tool "alpha": schema is not valid JSON`,
		},
		{
			name: "a second tool that is wrong, after a first that is right",
			def: agent.Definition{Name: "clerk", Tools: []agent.Tool{
				{Name: "alpha", Run: engineTool},
				{Name: "beta"},
			}},
			want: `tool "beta": has neither Run nor Delegate`,
		},
		{
			name: "an output schema that is not valid JSON",
			def: agent.Definition{
				Name:   "clerk",
				Tools:  []agent.Tool{{Name: "alpha", Run: engineTool}},
				Output: json.RawMessage(`{"type":"object"`),
			},
			want: "output schema is not valid JSON",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEngineFixture(t)

			err := f.engine.Register(tc.def)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			_, err = f.engine.Start(t.Context(), agent.StartRequest{Agent: tc.def.Name})
			assert.ErrorIs(t, err, agent.ErrUnknownAgent, "the refused definition was registered")
		})
	}

	t.Run("a name already registered, and the first definition stands", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		second := engineDefinition("clerk")
		second.System = "be vague"

		err := f.engine.Register(second)

		require.Error(t, err)
		assert.Contains(t, err.Error(), `"clerk"`)
		assert.Contains(t, err.Error(), "already registered")
		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})
		require.NoError(t, err)
		assert.Equal(t, "be exact", run.Definition.System)
	})
}

func TestEngine_Register_Accepts(t *testing.T) {
	cases := []struct {
		name string
		defs []agent.Definition
	}{
		{
			name: "an agent with no tools",
			defs: []agent.Definition{{Name: "clerk"}},
		},
		{
			name: "a tool name of 64 characters, with every kind it may hold",
			defs: []agent.Definition{{Name: "clerk", Tools: []agent.Tool{
				{Name: "aZ09_-" + strings.Repeat("x", 58), Run: engineTool},
			}}},
		},
		{
			name: "a tool name of one character",
			defs: []agent.Definition{{Name: "clerk", Tools: []agent.Tool{{Name: "a", Run: engineTool}}}},
		},
		{
			name: "a tool with no schema, and an agent with no output schema",
			defs: []agent.Definition{{Name: "clerk", Tools: []agent.Tool{{Name: "alpha", Run: engineTool}}}},
		},
		{
			name: "a tool that delegates to an agent not registered",
			defs: []agent.Definition{{Name: "clerk", Tools: []agent.Tool{{Name: "alpha", Delegate: "nobody"}}}},
		},
		{
			name: "two agents with a tool of the same name",
			defs: []agent.Definition{
				{Name: "clerk", Tools: []agent.Tool{{Name: "alpha", Run: engineTool}}},
				{Name: "helper", Tools: []agent.Tool{{Name: "alpha", Run: engineTool}}},
			},
		},
		{
			name: "an agent whose name no tool could have",
			defs: []agent.Definition{{Name: "front desk / refunds"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEngineFixture(t)

			for _, def := range tc.defs {
				require.NoError(t, f.engine.Register(def))
			}

			for _, def := range tc.defs {
				run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: def.Name})
				require.NoError(t, err)
				assert.Equal(t, def.Name, run.Agent)
			}
		})
	}
}

func TestEngine_Register_KeepsWhatItWasGivenWhateverTheCallerDoesNext(t *testing.T) {
	f := newEngineFixture(t)
	def := engineDefinition("clerk")
	require.NoError(t, f.engine.Register(def))
	want, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})
	require.NoError(t, err)

	def.Tools[0].Name = "renamed"
	def.Tools[0].Schema[2] = 'X'
	def.Tools[1] = agent.Tool{Name: "swapped", Run: engineTool}
	def.Output[2] = 'X'

	got, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})
	require.NoError(t, err)
	assert.Equal(t, want.Definition, got.Definition)
}

// Agents are registered while runs are started and looked up: run under
// -race, this is the check that the definitions sit behind their mutex.
func TestEngine_Register_IsSafeWhileRunsStart(t *testing.T) {
	f := newEngineFixture(t, engineDefinition("clerk"))
	ctx := t.Context()

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 8 {
		wg.Go(func() {
			errs <- f.engine.Register(engineDefinition(fmt.Sprintf("helper-%d", i)))
		})
		wg.Go(func() {
			_, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	for i := range 8 {
		_, err := f.engine.Start(ctx, agent.StartRequest{Agent: fmt.Sprintf("helper-%d", i)})
		require.NoError(t, err)
	}
	runs, err := f.engine.ListRuns(ctx, agent.RunFilter{})
	require.NoError(t, err)
	assert.Len(t, runs, 16)
}

func TestEngine_Start(t *testing.T) {
	t.Run("an agent that is not registered is ErrUnknownAgent, and nothing is stored or published", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))

		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "nobody", Input: "refund order 7"})

		require.ErrorIs(t, err, agent.ErrUnknownAgent)
		assert.Contains(t, err.Error(), `"nobody"`)
		assert.Zero(t, run)
		assert.Zero(t, f.store.Calls(), "the store was reached")
		assert.Empty(t, f.bus.published())
	})

	t.Run("the run is stored runnable, as it was asked for, at the clock's time", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		f.clock.Advance(90 * time.Minute)
		now := f.clock.Now()

		run, err := f.engine.Start(t.Context(), agent.StartRequest{
			Agent:    "clerk",
			Input:    "refund order 7",
			Key:      "order-7",
			Metadata: map[string]string{"tenant": "north"},
		})
		require.NoError(t, err)

		assert.Equal(t, engineID(1), run.ID, "the id is not the one NewID gave")
		assert.Equal(t, "clerk", run.Agent)
		assert.Equal(t, agent.StatusRunnable, run.Status)
		assert.Equal(t, "refund order 7", run.Input)
		assert.Equal(t, "order-7", run.Key)
		assert.Equal(t, map[string]string{"tenant": "north"}, run.Metadata)
		assert.Equal(t, now, run.CreatedAt)
		assert.Equal(t, now, run.UpdatedAt)
		assert.Equal(t, int64(1), run.Rev)
		assert.Empty(t, run.ParentID)
		assert.Zero(t, run.Depth)
		assert.Empty(t, run.LeaseOwner, "Start claimed the run")

		stored, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, stored, run)
	})

	t.Run("it executes nothing", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))

		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk", Input: "refund order 7"})
		require.NoError(t, err)

		assert.Equal(t, 1, f.store.Calls(), "Start made more than its one write")
		assert.Empty(t, f.model.Requests(), "the model was called")
		steps, err := f.engine.Timeline(t.Context(), run.ID)
		require.NoError(t, err)
		assert.Empty(t, steps)
	})

	t.Run("the snapshot is what the model is to see, with the tools sorted by name", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))

		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})
		require.NoError(t, err)

		assert.Equal(t, agent.Snapshot{
			System: "be exact",
			Model:  "model-a",
			Tools: []agent.ToolSpec{
				{Name: "alpha", Description: "the first tool"},
				{Name: "mid", Description: "hands the work on"},
				{Name: "zeta", Description: "the last tool", Schema: json.RawMessage(`{"type":"object","required":["order"]}`)},
			},
			Output:    json.RawMessage(`{"type":"object","required":["total"]}`),
			MaxTokens: 2048,
			Limits:    agent.Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: -1, MaxTokens: -1, MaxModelCalls: 50},
		}, run.Definition)
	})

	t.Run("an agent with no tools and no output schema has neither in its snapshot", func(t *testing.T) {
		f := newEngineFixture(t, agent.Definition{Name: "clerk", Tools: []agent.Tool{}, Output: json.RawMessage{}})

		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})
		require.NoError(t, err)

		assert.Nil(t, run.Definition.Tools)
		assert.Nil(t, run.Definition.Output)
	})
}

func TestEngine_Start_Limits(t *testing.T) {
	defaults := agent.Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: -1, MaxTokens: -1, MaxModelCalls: 50}
	cases := []struct {
		name string
		def  agent.Limits
		req  *agent.Limits
		want agent.Limits
	}{
		{
			name: "none set: the defaults, with no limit written as -1",
			want: defaults,
		},
		{
			name: "the definition's own are kept",
			def:  agent.Limits{MaxDuration: time.Minute, MaxCostMicros: 2_000_000, MaxTokens: 90_000, MaxModelCalls: 7},
			want: agent.Limits{MaxDuration: time.Minute, MaxCostMicros: 2_000_000, MaxTokens: 90_000, MaxModelCalls: 7},
		},
		{
			name: "each zero field of the definition's takes its default",
			def:  agent.Limits{MaxCostMicros: 2_000_000},
			want: agent.Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: 2_000_000, MaxTokens: -1, MaxModelCalls: 50},
		},
		{
			name: "a negative field stays no limit",
			def:  agent.Limits{MaxDuration: -1, MaxCostMicros: -1, MaxTokens: -7, MaxModelCalls: -1},
			want: agent.Limits{MaxDuration: -1, MaxCostMicros: -1, MaxTokens: -7, MaxModelCalls: -1},
		},
		{
			name: "the request's replace the definition's",
			def:  agent.Limits{MaxDuration: time.Minute, MaxCostMicros: 2_000_000, MaxTokens: 90_000, MaxModelCalls: 7},
			req:  &agent.Limits{MaxDuration: time.Hour, MaxCostMicros: 5, MaxTokens: 6, MaxModelCalls: 8},
			want: agent.Limits{MaxDuration: time.Hour, MaxCostMicros: 5, MaxTokens: 6, MaxModelCalls: 8},
		},
		{
			name: "the request's replace them whole: a field it leaves zero takes the default, not the definition's",
			def:  agent.Limits{MaxDuration: time.Minute, MaxCostMicros: 2_000_000, MaxTokens: 90_000, MaxModelCalls: 7},
			req:  &agent.Limits{MaxTokens: 6},
			want: agent.Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: -1, MaxTokens: 6, MaxModelCalls: 50},
		},
		{
			name: "an empty request's are the defaults",
			def:  agent.Limits{MaxDuration: time.Minute, MaxModelCalls: 7},
			req:  &agent.Limits{},
			want: defaults,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEngineFixture(t, agent.Definition{Name: "clerk", Limits: tc.def})

			run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk", Limits: tc.req})
			require.NoError(t, err)

			assert.Equal(t, tc.want, run.Definition.Limits)
		})
	}
}

func TestEngine_Start_Key(t *testing.T) {
	t.Run("the same key returns the same run, and publishes once", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		ctx := t.Context()

		first, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk", Input: "refund order 7", Key: "order-7"})
		require.NoError(t, err)
		f.clock.Advance(time.Minute)
		second, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk", Input: "refund order 7, again", Key: "order-7"})
		require.NoError(t, err)

		assert.Equal(t, first, second)
		runs, err := f.engine.ListRuns(ctx, agent.RunFilter{})
		require.NoError(t, err)
		assert.Len(t, runs, 1)
		assert.Len(t, f.bus.published(), 1)
	})

	t.Run("the same key under another agent is another run", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"), engineDefinition("helper"))
		ctx := t.Context()

		first, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk", Key: "order-7"})
		require.NoError(t, err)
		second, err := f.engine.Start(ctx, agent.StartRequest{Agent: "helper", Key: "order-7"})
		require.NoError(t, err)

		assert.NotEqual(t, first.ID, second.ID)
		assert.Len(t, f.bus.published(), 2)
	})

	t.Run("no key starts a run every time", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		ctx := t.Context()

		first, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk", Input: "refund order 7"})
		require.NoError(t, err)
		second, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk", Input: "refund order 7"})
		require.NoError(t, err)

		assert.NotEqual(t, first.ID, second.ID)
		assert.Len(t, f.bus.published(), 2)
	})
}

func TestEngine_Start_Publishes(t *testing.T) {
	t.Run("EventRunStarted, for the run, at the time it was created", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		f.clock.Advance(time.Hour)

		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk", Input: "refund order 7"})
		require.NoError(t, err)

		assert.Equal(t, []string{agent.TopicRuns}, f.bus.topics)
		assert.Equal(t, []any{agent.Event{
			Type: agent.EventRunStarted, RunID: run.ID, Agent: "clerk", At: engineStart.Add(time.Hour),
		}}, f.bus.published())
	})

	t.Run("a store that fails: the error comes back and nothing is published", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		f.store.FailBefore("CreateRun", 1)

		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})

		require.ErrorIs(t, err, agenttest.ErrFault)
		assert.Zero(t, run)
		assert.Empty(t, f.bus.published())
	})

	t.Run("an id the store cannot keep: the store's refusal comes back", func(t *testing.T) {
		engine, err := agent.New(agent.Options{
			Model: agenttest.NewModel(nil),
			NewID: func() string { return "run-1" },
		})
		require.NoError(t, err)
		require.NoError(t, engine.Register(engineDefinition("clerk")))

		_, err = engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})

		require.Error(t, err)
		assert.Contains(t, err.Error(), `"run-1"`)
	})

	t.Run("a publisher that fails: the run is started all the same, and the failure is logged at Debug", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		f.bus.fail(errors.New("bus is down"))

		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})

		require.NoError(t, err)
		stored, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, stored, run)

		debug := f.logs.at(slog.LevelDebug)
		require.Len(t, debug, 1)
		assert.Contains(t, debug[0], "bus is down")
		assert.Contains(t, debug[0], run.ID)
		assert.Contains(t, debug[0], agent.EventRunStarted)
		assert.Empty(t, f.logs.at(slog.LevelInfo))
		assert.Empty(t, f.logs.at(slog.LevelWarn))
		assert.Empty(t, f.logs.at(slog.LevelError))
	})

	t.Run("no publisher: the run is started", func(t *testing.T) {
		engine, err := agent.New(agent.Options{Model: agenttest.NewModel(nil)})
		require.NoError(t, err)
		require.NoError(t, engine.Register(engineDefinition("clerk")))

		run, err := engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})

		require.NoError(t, err)
		assert.Equal(t, agent.StatusRunnable, run.Status)
	})
}

func TestEngine_Reads(t *testing.T) {
	t.Run("GetRun returns the run as the store has it", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, _ := f.parked(t)

		got, err := f.engine.GetRun(t.Context(), run.ID)

		require.NoError(t, err)
		assert.Equal(t, run, got)
		assert.Equal(t, agent.ReasonApproval, got.Reason)
	})

	t.Run("ListRuns lists newest first, and narrows by the filter", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"), engineDefinition("helper"))
		ctx := t.Context()
		var ids []string
		for _, name := range []string{"clerk", "helper", "clerk"} {
			run, err := f.engine.Start(ctx, agent.StartRequest{Agent: name})
			require.NoError(t, err)
			ids = append(ids, run.ID)
			f.clock.Advance(time.Minute)
		}

		all, err := f.engine.ListRuns(ctx, agent.RunFilter{})
		require.NoError(t, err)
		require.Len(t, all, 3)
		assert.Equal(t, []string{ids[2], ids[1], ids[0]}, []string{all[0].ID, all[1].ID, all[2].ID})

		clerks, err := f.engine.ListRuns(ctx, agent.RunFilter{Agent: "clerk", Limit: 1})
		require.NoError(t, err)
		require.Len(t, clerks, 1)
		assert.Equal(t, ids[2], clerks[0].ID)
	})

	t.Run("Timeline returns the journal in order", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, _ := f.parked(t)

		steps, err := f.engine.Timeline(t.Context(), run.ID)

		require.NoError(t, err)
		require.Len(t, steps, 2)
		assert.Equal(t, agent.StepModel, steps[0].Kind)
		assert.Equal(t, agent.StepTool, steps[1].Kind)
		assert.Equal(t, agent.StepWaiting, steps[1].Status)
		want, err := f.store.Steps(t.Context(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, want, steps)
	})

	t.Run("Changes returns what happened after a revision", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, approval := f.parked(t)

		all, err := f.engine.Changes(t.Context(), run.ID, 0)
		require.NoError(t, err)
		assert.Equal(t, run, all.Run)
		assert.Len(t, all.Steps, 2)
		assert.Equal(t, []agent.Approval{approval}, all.Approvals)

		none, err := f.engine.Changes(t.Context(), run.ID, run.Rev)
		require.NoError(t, err)
		assert.Equal(t, run, none.Run)
		assert.Empty(t, none.Steps)
		assert.Empty(t, none.Approvals)
	})

	t.Run("ListApprovals lists what the filter asks for", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		_, approval := f.parked(t)

		pending, err := f.engine.ListApprovals(t.Context(), agent.ApprovalFilter{Status: agent.ApprovalPending})
		require.NoError(t, err)
		assert.Equal(t, []agent.Approval{approval}, pending)

		declined, err := f.engine.ListApprovals(t.Context(), agent.ApprovalFilter{Status: agent.ApprovalDeclined})
		require.NoError(t, err)
		assert.Empty(t, declined)
	})

	t.Run("a run that does not exist is ErrNotFound", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		ctx := t.Context()

		_, err := f.engine.GetRun(ctx, engineID(404))
		assert.ErrorIs(t, err, agent.ErrNotFound)
		_, err = f.engine.Timeline(ctx, engineID(404))
		assert.ErrorIs(t, err, agent.ErrNotFound)
		_, err = f.engine.Changes(ctx, engineID(404), 0)
		assert.ErrorIs(t, err, agent.ErrNotFound)
	})

	reads := []struct {
		op   string
		read func(ctx context.Context, e *agent.Engine, runID string) error
	}{
		{"GetRun", func(ctx context.Context, e *agent.Engine, runID string) error {
			_, err := e.GetRun(ctx, runID)
			return err
		}},
		{"ListRuns", func(ctx context.Context, e *agent.Engine, _ string) error {
			_, err := e.ListRuns(ctx, agent.RunFilter{})
			return err
		}},
		{"Steps", func(ctx context.Context, e *agent.Engine, runID string) error {
			_, err := e.Timeline(ctx, runID)
			return err
		}},
		{"Changes", func(ctx context.Context, e *agent.Engine, runID string) error {
			_, err := e.Changes(ctx, runID, 0)
			return err
		}},
		{"ListApprovals", func(ctx context.Context, e *agent.Engine, _ string) error {
			_, err := e.ListApprovals(ctx, agent.ApprovalFilter{})
			return err
		}},
	}
	for _, tc := range reads {
		t.Run("a store that fails "+tc.op+" is an error", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			run, _ := f.parked(t)
			f.store.FailBefore(tc.op, 1)

			err := tc.read(t.Context(), f.engine, run.ID)

			assert.ErrorIs(t, err, agenttest.ErrFault)
		})
	}
}

// Approve and Decline are one operation with two answers, so every case is
// asked of both.
func TestEngine_ApproveAndDecline(t *testing.T) {
	answers := []struct {
		name   string
		status agent.ApprovalStatus
		decide func(e *agent.Engine, ctx context.Context, id, by, reason string) (agent.Approval, error)
	}{
		{"Approve", agent.ApprovalApproved, (*agent.Engine).Approve},
		{"Decline", agent.ApprovalDeclined, (*agent.Engine).Decline},
	}
	for _, answer := range answers {
		t.Run(answer.name+": the answer reaches the store, wakes the run and is published", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			run, approval := f.parked(t)
			f.clock.Advance(10 * time.Minute)
			now := f.clock.Now()

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "ops@example.test", "checked the order")
			require.NoError(t, err)

			assert.Equal(t, approval.ID, got.ID)
			assert.Equal(t, answer.status, got.Status)
			assert.Equal(t, "ops@example.test", got.DecidedBy)
			assert.Equal(t, "checked the order", got.Reason)
			require.NotNil(t, got.DecidedAt)
			assert.Equal(t, now, *got.DecidedAt)

			stored, err := f.store.GetApproval(t.Context(), approval.ID)
			require.NoError(t, err)
			assert.Equal(t, stored, got)
			woken, err := f.store.GetRun(t.Context(), run.ID)
			require.NoError(t, err)
			assert.Equal(t, agent.StatusRunnable, woken.Status)
			steps, err := f.store.Steps(t.Context(), run.ID)
			require.NoError(t, err)
			assert.Equal(t, agent.StepWaiting, steps[1].Status, "the answer was written to the journal: that is the next execution's to do")

			assert.Equal(t, []string{agent.TopicRuns}, f.bus.topics)
			assert.Equal(t, []any{agent.Event{
				Type: agent.EventApprovalDecided, RunID: run.ID, Agent: "clerk", Seq: 2, At: now,
			}}, f.bus.published())
		})

		t.Run(answer.name+": no reason is needed", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			_, approval := f.parked(t)

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "ops@example.test", "")

			require.NoError(t, err)
			assert.Equal(t, answer.status, got.Status)
			assert.Empty(t, got.Reason)
		})

		t.Run(answer.name+": an empty by is refused before the store is reached", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			_, approval := f.parked(t)
			calls := f.store.Calls()

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "", "checked the order")

			require.Error(t, err)
			assert.Contains(t, err.Error(), "by is empty")
			assert.Zero(t, got)
			assert.Equal(t, calls, f.store.Calls(), "the store was reached")
			assert.Empty(t, f.bus.published())
			stored, err := f.store.GetApproval(t.Context(), approval.ID)
			require.NoError(t, err)
			assert.Equal(t, agent.ApprovalPending, stored.Status)
		})

		t.Run(answer.name+": an approval already answered is ErrAlreadyDecided with the answer that stands, and nothing is published", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			_, approval := f.parked(t)
			first, err := f.engine.Decline(t.Context(), approval.ID, "ops@example.test", "wrong account")
			require.NoError(t, err)
			f.bus.forget()
			f.clock.Advance(time.Minute)

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "late@example.test", "me too")

			require.ErrorIs(t, err, agent.ErrAlreadyDecided)
			assert.Equal(t, first, got)
			assert.Equal(t, "ops@example.test", got.DecidedBy)
			assert.Empty(t, f.bus.published())
		})

		t.Run(answer.name+": an approval that lapsed is ErrAlreadyDecided, as it stands", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			_, approval := f.parked(t)
			f.clock.Advance(2 * time.Hour)
			lapsed, err := f.store.ExpireApprovals(t.Context(), f.clock.Now())
			require.NoError(t, err)
			require.Equal(t, 1, lapsed)

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "late@example.test", "me too")

			require.ErrorIs(t, err, agent.ErrAlreadyDecided)
			assert.Equal(t, agent.ApprovalExpired, got.Status)
			assert.Empty(t, got.DecidedBy)
			assert.Empty(t, f.bus.published())
		})

		t.Run(answer.name+": an approval that does not exist is ErrNotFound", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			f.parked(t)

			_, err := answer.decide(f.engine, t.Context(), engineID(404), "ops@example.test", "")
			assert.ErrorIs(t, err, agent.ErrNotFound)
			_, err = answer.decide(f.engine, t.Context(), "approval-1", "ops@example.test", "")
			assert.ErrorIs(t, err, agent.ErrNotFound)
			assert.Empty(t, f.bus.published())
		})

		t.Run(answer.name+": a store that fails is an error, and nothing is published", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			_, approval := f.parked(t)
			f.store.FailBefore("DecideApproval", 1)

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "ops@example.test", "")

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Zero(t, got)
			assert.Empty(t, f.bus.published())
		})

		t.Run(answer.name+": a publisher that fails does not undo the answer", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			_, approval := f.parked(t)
			f.bus.fail(errors.New("bus is down"))

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "ops@example.test", "")

			require.NoError(t, err)
			assert.Equal(t, answer.status, got.Status)
			debug := f.logs.at(slog.LevelDebug)
			require.Len(t, debug, 1)
			assert.Contains(t, debug[0], "bus is down")
			assert.Contains(t, debug[0], agent.EventApprovalDecided)
		})

		t.Run(answer.name+": a run that cannot be read for the event costs the event, not the answer", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			_, approval := f.parked(t)
			f.store.FailBefore("GetRun", 1)

			got, err := answer.decide(f.engine, t.Context(), approval.ID, "ops@example.test", "")

			require.NoError(t, err)
			assert.Equal(t, answer.status, got.Status)
			assert.Empty(t, f.bus.published())
			debug := f.logs.at(slog.LevelDebug)
			require.Len(t, debug, 1)
			assert.Contains(t, debug[0], agenttest.ErrFault.Error())
			assert.Contains(t, debug[0], agent.EventApprovalDecided)
		})
	}

	t.Run("no publisher: the run is not read for an event nobody will get", func(t *testing.T) {
		store := agenttest.NewFaultStore(agent.NewMemoryStore())
		clock := agenttest.NewClock(engineStart)
		engine, err := agent.New(agent.Options{Model: agenttest.NewModel(nil), Store: store, Clock: clock})
		require.NoError(t, err)
		require.NoError(t, engine.Register(engineDefinition("clerk")))
		f := &engineFixture{engine: engine, store: store, clock: clock, bus: &engineBus{}}
		_, approval := f.parked(t)
		calls := store.Calls()

		_, err = engine.Approve(t.Context(), approval.ID, "ops@example.test", "")

		require.NoError(t, err)
		assert.Equal(t, calls+1, store.Calls(), "Approve made more than its one store call")
	})
}

func TestEngine_Cancel(t *testing.T) {
	t.Run("a waiting run is marked, made runnable, and the request published", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, approval := f.parked(t)
		f.clock.Advance(10 * time.Minute)
		now := f.clock.Now()

		err := f.engine.Cancel(t.Context(), run.ID, "ops@example.test", "customer withdrew")
		require.NoError(t, err)

		got, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		assert.True(t, got.CancelRequested)
		assert.Equal(t, "ops@example.test", got.CancelBy)
		assert.Equal(t, "customer withdrew", got.CancelReason)
		assert.Equal(t, agent.StatusRunnable, got.Status, "the run was ended: that is the next execution's to do")
		assert.Equal(t, now, got.UpdatedAt)
		pending, err := f.store.GetApproval(t.Context(), approval.ID)
		require.NoError(t, err)
		assert.Equal(t, agent.ApprovalPending, pending.Status)

		assert.Equal(t, []string{agent.TopicRuns}, f.bus.topics)
		assert.Equal(t, []any{agent.Event{
			Type: agent.EventRunCancelled, RunID: run.ID, Agent: "clerk", At: now,
		}}, f.bus.published())
	})

	t.Run("a run not yet executed is marked and stays runnable", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, err := f.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk"})
		require.NoError(t, err)

		require.NoError(t, f.engine.Cancel(t.Context(), run.ID, "ops@example.test", ""))

		got, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		assert.True(t, got.CancelRequested)
		assert.Empty(t, got.CancelReason)
		assert.Equal(t, agent.StatusRunnable, got.Status)
	})

	t.Run("an empty by is refused before the store is reached", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, _ := f.parked(t)
		calls := f.store.Calls()

		err := f.engine.Cancel(t.Context(), run.ID, "", "customer withdrew")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "by is empty")
		assert.Equal(t, calls, f.store.Calls(), "the store was reached")
		assert.Empty(t, f.bus.published())
		got, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		assert.False(t, got.CancelRequested)
	})

	t.Run("asked twice: the first request stands, and it is published once", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, _ := f.parked(t)
		require.NoError(t, f.engine.Cancel(t.Context(), run.ID, "ops@example.test", "customer withdrew"))
		first, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		f.clock.Advance(time.Minute)

		err = f.engine.Cancel(t.Context(), run.ID, "late@example.test", "me too")

		require.NoError(t, err)
		got, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, first, got)
		assert.Len(t, f.bus.published(), 1)
	})

	t.Run("a run that has ended is ErrFinished, and nothing is published", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		ctx := t.Context()
		run, err := f.engine.Start(ctx, agent.StartRequest{Agent: "clerk"})
		require.NoError(t, err)
		claimed, err := f.store.Claim(ctx, agent.ClaimRequest{
			Owner: "executor", Agents: []string{"clerk"}, RunID: run.ID, Now: f.clock.Now(), TTL: time.Minute,
		})
		require.NoError(t, err)
		require.NoError(t, f.store.Finish(ctx, claimed.Lease(), agent.FinishRequest{
			Status: agent.StatusCompleted, Output: "done", Now: f.clock.Now(),
		}))
		f.bus.forget()

		err = f.engine.Cancel(ctx, run.ID, "ops@example.test", "customer withdrew")

		require.ErrorIs(t, err, agent.ErrFinished)
		assert.Empty(t, f.bus.published())
	})

	t.Run("a run that does not exist is ErrNotFound", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))

		err := f.engine.Cancel(t.Context(), engineID(404), "ops@example.test", "")
		assert.ErrorIs(t, err, agent.ErrNotFound)
		err = f.engine.Cancel(t.Context(), "run-1", "ops@example.test", "")
		assert.ErrorIs(t, err, agent.ErrNotFound)
		assert.Empty(t, f.bus.published())
	})

	for _, op := range []string{"GetRun", "RequestCancel"} {
		t.Run("a store that fails "+op+" is an error, and nothing is published", func(t *testing.T) {
			f := newEngineFixture(t, engineDefinition("clerk"))
			run, _ := f.parked(t)
			f.store.FailBefore(op, 1)

			err := f.engine.Cancel(t.Context(), run.ID, "ops@example.test", "")

			require.ErrorIs(t, err, agenttest.ErrFault)
			assert.Empty(t, f.bus.published())
		})
	}

	t.Run("a publisher that fails does not undo the request", func(t *testing.T) {
		f := newEngineFixture(t, engineDefinition("clerk"))
		run, _ := f.parked(t)
		f.bus.fail(errors.New("bus is down"))

		err := f.engine.Cancel(t.Context(), run.ID, "ops@example.test", "")

		require.NoError(t, err)
		got, err := f.store.GetRun(t.Context(), run.ID)
		require.NoError(t, err)
		assert.True(t, got.CancelRequested)
		debug := f.logs.at(slog.LevelDebug)
		require.Len(t, debug, 1)
		assert.Contains(t, debug[0], "bus is down")
		assert.Contains(t, debug[0], agent.EventRunCancelled)
	})
}
