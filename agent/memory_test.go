package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

func TestMemoryStore_PassesTheStoreContract(t *testing.T) {
	agenttest.RunStoreSuite(t, func(*testing.T) agent.Store { return agent.NewMemoryStore() })
}

func TestNewMemoryStore_IsEmpty(t *testing.T) {
	store := agent.NewMemoryStore()
	ctx := t.Context()

	runs, err := store.ListRuns(ctx, agent.RunFilter{})
	require.NoError(t, err)
	assert.Empty(t, runs)

	approvals, err := store.ListApprovals(ctx, agent.ApprovalFilter{})
	require.NoError(t, err)
	assert.Empty(t, approvals)

	claimed, err := store.Claim(ctx, agent.ClaimRequest{Owner: "worker-a", Agents: []string{"alpha"}, Now: memoryStart, TTL: time.Minute})
	require.NoError(t, err)
	assert.Nil(t, claimed)
}

func TestMemoryStore_TwoStoresShareNothing(t *testing.T) {
	first, second := agent.NewMemoryStore(), agent.NewMemoryStore()
	ctx := t.Context()

	_, _, err := first.CreateRun(ctx, agent.Run{ID: memoryRunID, Agent: "alpha", Status: agent.StatusRunnable})
	require.NoError(t, err)

	_, err = second.GetRun(ctx, memoryRunID)
	assert.ErrorIs(t, err, agent.ErrNotFound)
}

// A database takes a UUID in several spellings and hands back the canonical
// one. MemoryStore keeps the string it is given, so it takes only that one:
// an id never reads back as anything but what was stored.
func TestMemoryStore_TakesAUUIDInItsCanonicalFormOnly(t *testing.T) {
	spellings := []struct {
		name string
		id   string
		ok   bool
	}{
		{"canonical", "0b0e7b1c-3a57-4c8e-9d2f-5f1a6c9e4d10", true},
		{"upper case", "0B0E7B1C-3A57-4C8E-9D2F-5F1A6C9E4D10", false},
		{"in braces", "{0b0e7b1c-3a57-4c8e-9d2f-5f1a6c9e4d10}", false},
		{"without hyphens", "0b0e7b1c3a574c8e9d2f5f1a6c9e4d10", false},
		{"as a URN", "urn:uuid:0b0e7b1c-3a57-4c8e-9d2f-5f1a6c9e4d10", false},
		{"empty", "", false},
	}
	for _, tt := range spellings {
		t.Run(tt.name, func(t *testing.T) {
			store := agent.NewMemoryStore()

			stored, created, err := store.CreateRun(t.Context(), agent.Run{ID: tt.id, Agent: "alpha", Status: agent.StatusRunnable})

			if !tt.ok {
				require.Error(t, err)
				assert.False(t, created)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.id, stored.ID)
		})
	}
}

func TestMemoryStore_RefusesAttributesJSONCannotHold(t *testing.T) {
	f := newMemoryFixture(t)
	require.NoError(t, f.store.UpdateStep(f.ctx, f.lease, agent.StepUpdate{
		Seq: 3, From: agent.StepWaiting, To: agent.StepStarted, Now: f.tick(),
	}))
	before := f.state()
	req := memoryQuestion(f.tick())
	req.ID, req.From = memoryID("9000", 2), agent.StepStarted
	req.Action.Attrs = map[string]any{"reply": make(chan string)}

	_, err := f.store.RequestApproval(f.ctx, f.lease, req)

	require.Error(t, err)
	assert.Equal(t, before, f.state(), "nothing was recorded")
}

var memoryStart = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

// The ids the fixtures use. A store keeps a run or an approval only under a
// UUID.
const (
	memoryRunID      = "00000000-0000-4000-8000-000000000001"
	memoryOtherRunID = "00000000-0000-4000-8000-000000000002"
	memoryApprovalID = "00000000-0000-4000-9000-000000000001"
)

// memoryID is the nth id of a series, for tests that need many.
func memoryID(series string, n int) string {
	return fmt.Sprintf("00000000-0000-4000-%s-%012d", series, n)
}

// memoryFixture is a store holding one run that has every kind of value a
// caller could keep a reference into: maps, lists, raw JSON and pointers to
// times, on the run, on its steps and on its approval.
type memoryFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *agent.MemoryStore
	clock    *agenttest.Clock
	runID    string
	lease    agent.Lease
	approval agent.Approval
}

func (f *memoryFixture) tick() time.Time {
	f.clock.Advance(time.Millisecond)
	return f.clock.Now()
}

func newMemoryFixture(t *testing.T) *memoryFixture {
	t.Helper()
	f := &memoryFixture{
		t: t, ctx: t.Context(), store: agent.NewMemoryStore(),
		clock: agenttest.NewClock(memoryStart), runID: memoryRunID,
	}

	now := f.tick()
	_, _, err := f.store.CreateRun(f.ctx, memoryRun(f.runID, now))
	require.NoError(t, err)

	// Claimed, given back with a time to try again at, and claimed again, so
	// the run carries both of its optional times.
	first, err := f.store.Claim(f.ctx, agent.ClaimRequest{Owner: "worker-a", Agents: []string{"alpha"}, Now: f.tick(), TTL: time.Minute})
	require.NoError(t, err)
	require.NotNil(t, first)
	retryAt := f.clock.Now().Add(time.Second)
	require.NoError(t, f.store.Yield(f.ctx, first.Lease(), agent.YieldRequest{NextAttemptAt: &retryAt, Now: f.tick()}))
	f.clock.Advance(time.Second)
	held, err := f.store.Claim(f.ctx, agent.ClaimRequest{Owner: "worker-a", Agents: []string{"alpha"}, Now: f.tick(), TTL: time.Minute})
	require.NoError(t, err)
	require.NotNil(t, held)
	f.lease = held.Lease()

	require.NoError(t, f.store.BeginModel(f.ctx, f.lease, 1, f.tick()))
	require.NoError(t, f.store.CompleteModel(f.ctx, f.lease, agent.CompleteModelRequest{
		Seq: 1, Message: memoryMessage(), Stop: agent.StopToolUse, Model: "model-a", Now: f.tick(),
	}))
	result := "found"
	require.NoError(t, f.store.UpdateStep(f.ctx, f.lease, agent.StepUpdate{
		Seq: 2, From: agent.StepProposed, To: agent.StepStarted, Now: f.tick(),
	}))
	require.NoError(t, f.store.UpdateStep(f.ctx, f.lease, agent.StepUpdate{
		Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Result: &result, Now: f.tick(),
	}))
	f.approval, err = f.store.RequestApproval(f.ctx, f.lease, memoryQuestion(f.tick()))
	require.NoError(t, err)
	return f
}

func memoryRun(id string, now time.Time) agent.Run {
	return agent.Run{
		ID: id, Agent: "alpha", Status: agent.StatusRunnable, Input: "input", Key: "start-1",
		Definition: agent.Snapshot{
			System: "system",
			Tools:  []agent.ToolSpec{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}},
			Output: json.RawMessage(`{"type":"string"}`),
		},
		Metadata:  map[string]string{"batch": "7"},
		CreatedAt: now, UpdatedAt: now,
	}
}

func memoryMessage() agent.Message {
	return agent.Message{
		Role: agent.RoleAssistant,
		Text: "reading it now",
		Calls: []agent.Call{
			agenttest.Call("call-1", "lookup", `{"id":7}`),
			agenttest.Call("call-2", "send", `{"to":"a"}`),
		},
		Results: []agent.Result{{CallID: "call-0", Content: "earlier"}},
		Opaque:  &agent.Opaque{Provider: "provider-a", Data: json.RawMessage(`{"z":1,"a":2}`)},
	}
}

func memoryQuestion(now time.Time) agent.ApprovalRequest {
	expires := now.Add(time.Hour)
	return agent.ApprovalRequest{
		ID: memoryApprovalID, Seq: 3, From: agent.StepProposed, Cause: agent.CauseGuard,
		Action: agent.Action{Kind: "run", Target: "send", Attrs: map[string]any{
			"agent":  "alpha",
			"tags":   []any{"outbound", map[string]any{"level": "high"}},
			"nested": map[string]any{"to": "a", "list": []any{"x"}},
		}},
		Decision: agent.Ask, Rule: "sends need a person", ExpiresAt: &expires, Now: now,
	}
}

// state is everything the store holds about the fixture's run, as text, so
// that nothing in it can be reached through a value the test still holds.
func (f *memoryFixture) state() string {
	f.t.Helper()
	run, err := f.store.GetRun(f.ctx, f.runID)
	require.NoError(f.t, err)
	steps, err := f.store.Steps(f.ctx, f.runID)
	require.NoError(f.t, err)
	approvals, err := f.store.ListApprovals(f.ctx, agent.ApprovalFilter{RunID: f.runID})
	require.NoError(f.t, err)

	out, err := json.Marshal(map[string]any{"run": run, "steps": steps, "approvals": approvals})
	require.NoError(f.t, err)
	return string(out)
}

// scribbleRun changes everything a Run lets its holder reach.
func scribbleRun(r *agent.Run) {
	r.Input = "scribbled"
	r.Rev = 99
	r.Metadata["batch"] = "scribbled"
	r.Metadata["added"] = "scribbled"
	for i := range r.Definition.Tools {
		r.Definition.Tools[i].Name = "scribbled"
		scribbleRaw(r.Definition.Tools[i].Schema)
	}
	scribbleRaw(r.Definition.Output)
	scribbleTime(r.LeaseExpiresAt)
	scribbleTime(r.NextAttemptAt)
	scribbleTime(r.FinishedAt)
}

// scribbleMessage changes everything a Message lets its holder reach.
func scribbleMessage(m *agent.Message) {
	m.Text = "scribbled"
	for i := range m.Calls {
		m.Calls[i].Name = "scribbled"
		scribbleRaw(m.Calls[i].Input)
	}
	for i := range m.Results {
		m.Results[i].Content = "scribbled"
	}
	if m.Opaque != nil {
		m.Opaque.Provider = "scribbled"
		scribbleRaw(m.Opaque.Data)
	}
}

// scribbleStep changes everything a Step lets its holder reach.
func scribbleStep(s *agent.Step) {
	s.Result = "scribbled"
	s.Attempts = 99
	if s.Message != nil {
		scribbleMessage(s.Message)
	}
	if s.Call != nil {
		s.Call.Name = "scribbled"
		scribbleRaw(s.Call.Input)
	}
	scribbleTime(s.StartedAt)
	scribbleTime(s.FinishedAt)
}

// scribbleAttrs changes an action's attributes at every depth.
func scribbleAttrs(attrs map[string]any) {
	attrs["agent"] = "scribbled"
	attrs["added"] = "scribbled"
	if tags, ok := attrs["tags"].([]any); ok {
		tags[0] = "scribbled"
		if inner, ok := tags[1].(map[string]any); ok {
			inner["level"] = "scribbled"
		}
	}
	if nested, ok := attrs["nested"].(map[string]any); ok {
		nested["to"] = "scribbled"
		if list, ok := nested["list"].([]any); ok {
			list[0] = "scribbled"
		}
	}
}

// scribbleApproval changes everything an Approval lets its holder reach.
func scribbleApproval(a *agent.Approval) {
	a.Tool = "scribbled"
	a.Status = agent.ApprovalCancelled
	scribbleRaw(a.Input)
	scribbleAttrs(a.Action.Attrs)
	scribbleTime(a.DecidedAt)
	scribbleTime(a.ExpiresAt)
}

func scribbleRaw(b json.RawMessage) {
	for i := range b {
		b[i] = 'X'
	}
}

func scribbleTime(t *time.Time) {
	if t != nil {
		*t = time.Time{}
	}
}

func TestMemoryStore_KeepsWhatItWasGivenWhateverTheCallerDoesNext(t *testing.T) {
	t.Run("a run changed after it was stored", func(t *testing.T) {
		store := agent.NewMemoryStore()
		ctx := t.Context()
		run := memoryRun(memoryRunID, memoryStart)
		want := memoryRun(memoryRunID, memoryStart)
		want.Rev = 1

		_, _, err := store.CreateRun(ctx, run)
		require.NoError(t, err)
		scribbleRun(&run)

		got, err := store.GetRun(ctx, memoryRunID)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})

	t.Run("a message changed after it was journaled", func(t *testing.T) {
		f := newMemoryFixture(t)
		require.NoError(t, f.store.UpdateStep(f.ctx, f.lease, agent.StepUpdate{
			Seq: 3, From: agent.StepWaiting, To: agent.StepDeclined, Now: f.tick(),
		}))
		require.NoError(t, f.store.BeginModel(f.ctx, f.lease, 4, f.tick()))
		message := memoryMessage()

		require.NoError(t, f.store.CompleteModel(f.ctx, f.lease, agent.CompleteModelRequest{
			Seq: 4, Message: message, Stop: agent.StopToolUse, Model: "model-a", Now: f.tick(),
		}))
		scribbleMessage(&message)

		steps, err := f.store.Steps(f.ctx, f.runID)
		require.NoError(t, err)
		require.Len(t, steps, 6)
		assert.Equal(t, memoryMessage(), *steps[3].Message, "the reply")
		assert.Equal(t, memoryMessage().Calls[0], *steps[4].Call, "the tool step made from its first call")
		assert.Equal(t, memoryMessage().Calls[1], *steps[5].Call, "the tool step made from its second call")
	})

	t.Run("an action and a time changed after the question was asked", func(t *testing.T) {
		store := agent.NewMemoryStore()
		ctx := t.Context()
		_, _, err := store.CreateRun(ctx, memoryRun(memoryRunID, memoryStart))
		require.NoError(t, err)
		held, err := store.Claim(ctx, agent.ClaimRequest{Owner: "worker-a", Agents: []string{"alpha"}, Now: memoryStart, TTL: time.Minute})
		require.NoError(t, err)
		require.NoError(t, store.BeginModel(ctx, held.Lease(), 1, memoryStart))
		require.NoError(t, store.CompleteModel(ctx, held.Lease(), agent.CompleteModelRequest{
			Seq: 1, Message: memoryMessage(), Stop: agent.StopToolUse, Now: memoryStart,
		}))
		req := memoryQuestion(memoryStart)

		_, err = store.RequestApproval(ctx, held.Lease(), req)
		require.NoError(t, err)
		scribbleAttrs(req.Action.Attrs)
		scribbleTime(req.ExpiresAt)

		got, err := store.GetApproval(ctx, req.ID)
		require.NoError(t, err)
		want := memoryQuestion(memoryStart)
		assert.Equal(t, want.Action, got.Action)
		assert.Equal(t, want.ExpiresAt, got.ExpiresAt)
	})

	t.Run("a time to try again at changed after the run was given back", func(t *testing.T) {
		f := newMemoryFixture(t)
		retryAt := f.clock.Now().Add(time.Minute)
		want := retryAt

		require.NoError(t, f.store.Yield(f.ctx, f.lease, agent.YieldRequest{NextAttemptAt: &retryAt, Now: f.tick()}))
		retryAt = time.Time{}

		run, err := f.store.GetRun(f.ctx, f.runID)
		require.NoError(t, err)
		require.NotNil(t, run.NextAttemptAt)
		assert.Equal(t, want, *run.NextAttemptAt)
	})

	t.Run("a result changed after it was recorded", func(t *testing.T) {
		f := newMemoryFixture(t)
		require.NoError(t, f.store.UpdateStep(f.ctx, f.lease, agent.StepUpdate{
			Seq: 3, From: agent.StepWaiting, To: agent.StepStarted, Now: f.tick(),
		}))
		result := "sent"

		require.NoError(t, f.store.UpdateStep(f.ctx, f.lease, agent.StepUpdate{
			Seq: 3, From: agent.StepStarted, To: agent.StepCompleted, Result: &result, Now: f.tick(),
		}))
		result = "scribbled"

		steps, err := f.store.Steps(f.ctx, f.runID)
		require.NoError(t, err)
		assert.Equal(t, "sent", steps[2].Result)
	})
}

func TestMemoryStore_HandsOutCopies(t *testing.T) {
	// Each makes one call and returns what scribbles on everything the call
	// handed back.
	handouts := []struct {
		name string
		call func(f *memoryFixture) (scribble func())
	}{
		{"GetRun", func(f *memoryFixture) func() {
			run, err := f.store.GetRun(f.ctx, f.runID)
			require.NoError(f.t, err)
			return func() { scribbleRun(&run) }
		}},
		{"GetRun, of a run that has ended", func(f *memoryFixture) func() {
			require.NoError(f.t, f.store.Finish(f.ctx, f.lease, agent.FinishRequest{Status: agent.StatusCompleted, Now: f.tick()}))
			run, err := f.store.GetRun(f.ctx, f.runID)
			require.NoError(f.t, err)
			require.NotNil(f.t, run.FinishedAt)
			return func() { scribbleRun(&run) }
		}},
		{"ListRuns", func(f *memoryFixture) func() {
			runs, err := f.store.ListRuns(f.ctx, agent.RunFilter{})
			require.NoError(f.t, err)
			require.Len(f.t, runs, 1)
			return func() { scribbleRun(&runs[0]) }
		}},
		{"CreateRun, given a key already used", func(f *memoryFixture) func() {
			run, created, err := f.store.CreateRun(f.ctx, memoryRun(memoryOtherRunID, f.tick()))
			require.NoError(f.t, err)
			require.False(f.t, created)
			return func() { scribbleRun(&run) }
		}},
		{"Claim", func(f *memoryFixture) func() {
			require.NoError(f.t, f.store.Yield(f.ctx, f.lease, agent.YieldRequest{Now: f.tick()}))
			run, err := f.store.Claim(f.ctx, agent.ClaimRequest{Owner: "worker-b", Agents: []string{"alpha"}, Now: f.tick(), TTL: time.Minute})
			require.NoError(f.t, err)
			require.NotNil(f.t, run)
			return func() { scribbleRun(run) }
		}},
		{"Steps", func(f *memoryFixture) func() {
			steps, err := f.store.Steps(f.ctx, f.runID)
			require.NoError(f.t, err)
			require.Len(f.t, steps, 3)
			return func() {
				for i := range steps {
					scribbleStep(&steps[i])
				}
			}
		}},
		{"Changes", func(f *memoryFixture) func() {
			changes, err := f.store.Changes(f.ctx, f.runID, 0)
			require.NoError(f.t, err)
			require.Len(f.t, changes.Steps, 3)
			require.Len(f.t, changes.Approvals, 1)
			return func() {
				scribbleRun(&changes.Run)
				for i := range changes.Steps {
					scribbleStep(&changes.Steps[i])
				}
				scribbleApproval(&changes.Approvals[0])
			}
		}},
		{"RequestApproval", func(f *memoryFixture) func() {
			return func() { scribbleApproval(&f.approval) }
		}},
		{"RequestApproval, asked again", func(f *memoryFixture) func() {
			approval, err := f.store.RequestApproval(f.ctx, f.lease, memoryQuestion(f.tick()))
			require.NoError(f.t, err)
			return func() { scribbleApproval(&approval) }
		}},
		{"GetApproval", func(f *memoryFixture) func() {
			approval, err := f.store.GetApproval(f.ctx, f.approval.ID)
			require.NoError(f.t, err)
			return func() { scribbleApproval(&approval) }
		}},
		{"ListApprovals", func(f *memoryFixture) func() {
			approvals, err := f.store.ListApprovals(f.ctx, agent.ApprovalFilter{})
			require.NoError(f.t, err)
			require.Len(f.t, approvals, 1)
			return func() { scribbleApproval(&approvals[0]) }
		}},
		{"DecideApproval", func(f *memoryFixture) func() {
			approval, err := f.store.DecideApproval(f.ctx, agent.DecideRequest{ID: f.approval.ID, Approved: true, By: "ann", Now: f.tick()})
			require.NoError(f.t, err)
			return func() { scribbleApproval(&approval) }
		}},
		{"DecideApproval, already decided", func(f *memoryFixture) func() {
			_, err := f.store.DecideApproval(f.ctx, agent.DecideRequest{ID: f.approval.ID, Approved: true, By: "ann", Now: f.tick()})
			require.NoError(f.t, err)
			approval, err := f.store.DecideApproval(f.ctx, agent.DecideRequest{ID: f.approval.ID, By: "bob", Now: f.tick()})
			require.ErrorIs(f.t, err, agent.ErrAlreadyDecided)
			return func() { scribbleApproval(&approval) }
		}},
	}
	for _, tt := range handouts {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemoryFixture(t)
			scribble := tt.call(f)
			before := f.state()

			scribble()

			assert.Equal(t, before, f.state())
		})
	}
}

// Run under -race. Each worker takes runs through a whole life while the
// others do the same and readers list, follow and expire across all of them.
func TestMemoryStore_IsSafeForConcurrentUse(t *testing.T) {
	const (
		workers       = 8
		runsPerWorker = 20
	)
	store := agent.NewMemoryStore()
	ctx := t.Context()
	clock := agenttest.NewClock(memoryStart)
	tick := func() time.Time {
		clock.Advance(time.Millisecond)
		return clock.Now()
	}

	for i := range workers * runsPerWorker {
		now := tick()
		_, _, err := store.CreateRun(ctx, agent.Run{
			ID: memoryID("a000", i), Agent: "alpha", Status: agent.StatusRunnable, CreatedAt: now, UpdatedAt: now,
		})
		require.NoError(t, err)
	}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 2 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				runs, _ := store.ListRuns(ctx, agent.RunFilter{Limit: 200})
				for _, run := range runs {
					_, _ = store.Changes(ctx, run.ID, 0)
				}
				_, _ = store.ListApprovals(ctx, agent.ApprovalFilter{})
				_, _ = store.ExpireApprovals(ctx, memoryStart)
			}
		})
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Go(func() {
			owner := fmt.Sprintf("worker-%d", w)
			for {
				run, err := store.Claim(ctx, agent.ClaimRequest{Owner: owner, Agents: []string{"alpha"}, Now: tick(), TTL: time.Hour})
				if err != nil {
					errs <- err
					return
				}
				if run == nil {
					return
				}
				if err := liveOneLife(ctx, store, run.Lease(), strings.Replace(run.ID, "-a000-", "-b000-", 1), tick); err != nil {
					errs <- fmt.Errorf("%s: %w", run.ID, err)
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	runs, err := store.ListRuns(ctx, agent.RunFilter{Status: agent.StatusCompleted, Limit: 200})
	require.NoError(t, err)
	assert.Len(t, runs, workers*runsPerWorker, "every run was executed to its end, once")
	for _, run := range runs {
		assert.Equal(t, int64(1), run.LeaseEpoch, "%s was claimed once", run.ID)
		steps, err := store.Steps(ctx, run.ID)
		require.NoError(t, err)
		assert.Len(t, steps, 3, run.ID)
	}
}

// liveOneLife journals a reply with one call, has it approved under
// approvalID, runs it, and ends the run, all under lease.
func liveOneLife(ctx context.Context, store agent.Store, lease agent.Lease, approvalID string, tick func() time.Time) error {
	if err := store.BeginModel(ctx, lease, 1, tick()); err != nil {
		return err
	}
	if err := store.CompleteModel(ctx, lease, agent.CompleteModelRequest{
		Seq: 1, Message: agenttest.Use(agenttest.Call("call-1", "send", `{"to":"a"}`)).Message, Stop: agent.StopToolUse, Now: tick(),
	}); err != nil {
		return err
	}
	approval, err := store.RequestApproval(ctx, lease, agent.ApprovalRequest{
		ID: approvalID, Seq: 2, From: agent.StepProposed, Cause: agent.CauseGuard, Decision: agent.Ask, Now: tick(),
	})
	if err != nil {
		return err
	}
	if _, err := store.DecideApproval(ctx, agent.DecideRequest{ID: approval.ID, Approved: true, By: "ann", Now: tick()}); err != nil {
		return err
	}
	if _, err := store.Heartbeat(ctx, lease, tick(), time.Hour); err != nil {
		return err
	}
	if err := store.UpdateStep(ctx, lease, agent.StepUpdate{Seq: 2, From: agent.StepWaiting, To: agent.StepStarted, Now: tick()}); err != nil {
		return err
	}
	result := "sent"
	if err := store.UpdateStep(ctx, lease, agent.StepUpdate{
		Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Result: &result, Now: tick(),
	}); err != nil {
		return err
	}
	if err := store.BeginModel(ctx, lease, 3, tick()); err != nil {
		return err
	}
	if err := store.CompleteModel(ctx, lease, agent.CompleteModelRequest{
		Seq: 3, Message: agenttest.Say("done").Message, Stop: agent.StopEnd, Now: tick(),
	}); err != nil {
		return err
	}
	return store.Finish(ctx, lease, agent.FinishRequest{Status: agent.StatusCompleted, Output: "done", Now: tick()})
}
