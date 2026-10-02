package agenttest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

// RunStoreSuite runs the Store contract against the store newStore builds.
//
// newStore is called once for every case and must return an empty store that
// shares nothing with the others: some cases count what a whole store holds.
// The suite knows the store through the interface alone. Every time it passes
// comes from a Clock it moves in whole milliseconds, far from the wall clock,
// so a store that reads a clock of its own fails; every id it makes is a
// UUID; and every JSON value is compact, so a store may keep them as text.
func RunStoreSuite(t *testing.T, newStore func(t *testing.T) agent.Store) {
	t.Helper()

	groups := []struct {
		name  string
		cases []storeCase
	}{
		{"CreateRun", createRunCases()},
		{"Claim", claimCases()},
		{"Heartbeat", heartbeatCases()},
		{"Yield", yieldCases()},
		{"Fencing", fencingCases()},
		{"Steps", stepsCases()},
		{"BeginModel", beginModelCases()},
		{"CompleteModel", completeModelCases()},
		{"UpdateStep", updateStepCases()},
		{"RequestApproval", requestApprovalCases()},
		{"Park", parkCases()},
		{"DecideApproval", decideApprovalCases()},
		{"ExpireApprovals", expireApprovalsCases()},
		{"Finish", finishCases()},
		{"RequestCancel", requestCancelCases()},
		{"Changes", changesCases()},
		{"ListRuns", listRunsCases()},
		{"ListApprovals", listApprovalsCases()},
		{"NotFound", notFoundCases()},
	}
	for _, group := range groups {
		t.Run(group.name, func(t *testing.T) {
			for _, c := range group.cases {
				t.Run(c.name, func(t *testing.T) {
					c.run(&kit{
						t:     t,
						ctx:   t.Context(),
						store: newStore(t),
						clock: NewClock(suiteStart),
					})
				})
			}
		})
	}
}

// storeCase is one sentence of the contract and the check that it holds.
type storeCase struct {
	name string
	run  func(k *kit)
}

// What the cases are written with. Nothing here means anything to a store.
const (
	suiteTTL = 30 * time.Second

	workerA = "worker-a"
	workerB = "worker-b"

	agentAlpha = "alpha"
	agentBeta  = "beta"

	toolLookup = "lookup"
	toolSend   = "send"

	// personA and personB answer approvals and ask for cancellations.
	personA = "ann"
	personB = "bob"

	suiteModel    = "model-a"
	suiteRule     = "sends need a person"
	suiteStartKey = "start-1"

	lookupInput = `{"id":7}`
	sendInput   = `{"to":"a","n":2}`

	// malformedID is an id no store could have issued.
	malformedID = "no-such-id"
)

// suiteStart is where every case's clock begins. It is a whole second, and
// the cases move on from it in whole milliseconds, because a Postgres
// timestamp keeps microseconds and no more.
var suiteStart = time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)

// suiteAgents is what a case claims for unless it is about the agent filter.
var suiteAgents = []string{agentAlpha, agentBeta}

var finalStatuses = []agent.Status{agent.StatusCompleted, agent.StatusFailed, agent.StatusCancelled}

// kit is one case's store, its clock, and the steps cases share.
type kit struct {
	t     *testing.T
	ctx   context.Context
	store agent.Store
	clock *Clock
}

func (k *kit) now() time.Time { return k.clock.Now() }

// tick moves the clock on a millisecond and reads it, so that each write a
// case makes has an instant of its own.
func (k *kit) tick() time.Time {
	k.clock.Advance(time.Millisecond)
	return k.clock.Now()
}

// lapse moves the clock to the instant a lease taken now would lapse, and so
// past every lease taken before.
func (k *kit) lapse() { k.clock.Advance(suiteTTL) }

// newRun builds a runnable run of agentName created at the next instant. It
// is not stored.
func (k *kit) newRun(agentName string) agent.Run {
	now := k.tick()
	return agent.Run{
		ID:         uuid.NewString(),
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

func (k *kit) create(agentName string) agent.Run {
	k.t.Helper()
	return k.insert(k.newRun(agentName))
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

func (k *kit) step(runID string, seq int) agent.Step {
	k.t.Helper()
	steps := k.steps(runID)
	require.GreaterOrEqual(k.t, len(steps), seq, "the journal has no step %d", seq)
	require.Equal(k.t, seq, steps[seq-1].Seq)
	return steps[seq-1]
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
		Owner: owner, Agents: suiteAgents, RunID: runID, Now: k.tick(), TTL: suiteTTL,
	})
	require.NoError(k.t, err)
	require.NotNil(k.t, run)
	return run.Lease()
}

// held stores a run of agentName and claims it for workerA.
func (k *kit) held(agentName string) (agent.Run, agent.Lease) {
	k.t.Helper()
	run := k.create(agentName)
	lease := k.claim(workerA, run.ID)
	return k.run(run.ID), lease
}

// reply journals one model step under lease: begun, then completed with a
// turn that makes calls, or a final answer when there are none. It returns
// the step's seq, so the calls are the steps after it.
func (k *kit) reply(lease agent.Lease, calls ...agent.Call) int {
	k.t.Helper()
	seq := len(k.steps(lease.RunID)) + 1
	require.NoError(k.t, k.store.BeginModel(k.ctx, lease, seq, k.tick()))

	resp := Say("done")
	if len(calls) > 0 {
		resp = Use(calls...)
	}
	require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
		Seq: seq, Message: resp.Message, Stop: resp.Stop, Model: suiteModel, Now: k.tick(),
	}))
	return seq
}

// proposed stores a run of agentName held by workerA whose journal is a
// reply and the one call it made: step 2, proposed.
func (k *kit) proposed(agentName string) (agent.Run, agent.Lease) {
	k.t.Helper()
	run, lease := k.held(agentName)
	k.reply(lease, Call("call-1", toolSend, sendInput))
	return k.run(run.ID), lease
}

func (k *kit) update(lease agent.Lease, seq int, from, to agent.StepStatus) {
	k.t.Helper()
	require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
		Seq: seq, From: from, To: to, Now: k.tick(),
	}))
}

// askRequest is the question a guard's Ask puts about the proposed step at
// seq.
func (k *kit) askRequest(seq int) agent.ApprovalRequest {
	return agent.ApprovalRequest{
		ID:       uuid.NewString(),
		Seq:      seq,
		From:     agent.StepProposed,
		Cause:    agent.CauseGuard,
		Action:   agent.Action{Kind: "run", Target: toolSend},
		Decision: agent.Ask,
		Rule:     suiteRule,
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

// parked stores a run of agentName waiting on a person about its one call,
// with an approval that lapses at expires when that is set.
func (k *kit) parked(agentName string, expires *time.Time) (agent.Run, agent.Approval) {
	k.t.Helper()
	run, lease := k.proposed(agentName)
	approval := k.ask(lease, 2, expires)
	k.park(lease, agent.ReasonApproval)
	return k.run(run.ID), approval
}

func (k *kit) decide(approvalID string, approved bool) agent.Approval {
	k.t.Helper()
	approval, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{
		ID: approvalID, Approved: approved, By: personA, Reason: "checked", Now: k.tick(),
	})
	require.NoError(k.t, err)
	return approval
}

func (k *kit) finish(lease agent.Lease, status agent.Status) {
	k.t.Helper()
	require.NoError(k.t, k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: status, Now: k.tick()}))
}

// ended stores a run of agentName that ended with status.
func (k *kit) ended(agentName string, status agent.Status) agent.Run {
	k.t.Helper()
	run, lease := k.held(agentName)
	k.finish(lease, status)
	return k.run(run.ID)
}

// failOnce ends the execution holding lease as failed and claims the run
// again for the same worker, which leaves it held with one failure counted.
func (k *kit) failOnce(lease agent.Lease) agent.Lease {
	k.t.Helper()
	require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Failed: true, Error: "boom", Now: k.tick()}))
	again := k.claim(lease.Owner, lease.RunID)
	require.Equal(k.t, 1, k.run(lease.RunID).Failures)
	return again
}

// snapshot is everything a store holds about one run.
type snapshot struct {
	run       agent.Run
	steps     []agent.Step
	approvals []agent.Approval
}

func (k *kit) snapshot(runID string) snapshot {
	k.t.Helper()
	return snapshot{
		run:       normalRun(k.run(runID)),
		steps:     normalSteps(k.steps(runID)),
		approvals: normalApprovals(k.approvals(runID)),
	}
}

// unchanged asserts that the run is as it was when before was taken.
func (k *kit) unchanged(before snapshot) {
	k.t.Helper()
	assert.Equal(k.t, before, k.snapshot(before.run.ID), "the call changed something")
}

func (k *kit) equalRun(want, got agent.Run) {
	k.t.Helper()
	assert.Equal(k.t, normalRun(want), normalRun(got))
}

func (k *kit) equalSteps(want, got []agent.Step) {
	k.t.Helper()
	assert.Equal(k.t, normalSteps(want), normalSteps(got))
}

func (k *kit) equalApproval(want, got agent.Approval) {
	k.t.Helper()
	assert.Equal(k.t, normalApproval(want), normalApproval(got))
}

func (k *kit) equalApprovals(want, got []agent.Approval) {
	k.t.Helper()
	assert.Equal(k.t, normalApprovals(want), normalApprovals(got))
}

// timeIs asserts that got is set and is the instant want.
func (k *kit) timeIs(want time.Time, got *time.Time, what string) {
	k.t.Helper()
	if assert.NotNil(k.t, got, what) {
		assert.True(k.t, want.Equal(*got), "%s: want %s, got %s", what, want, *got)
	}
}

// The normal forms below are what two stores must agree on. A store may hand
// back an instant in another zone, or an empty map or list where it was given
// none; neither is a difference.

func normalTime(t time.Time) time.Time { return t.Round(0).UTC() }

func normalTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	n := normalTime(*t)
	return &n
}

func normalRun(r agent.Run) agent.Run {
	r.CreatedAt = normalTime(r.CreatedAt)
	r.UpdatedAt = normalTime(r.UpdatedAt)
	r.FinishedAt = normalTimePtr(r.FinishedAt)
	r.LeaseExpiresAt = normalTimePtr(r.LeaseExpiresAt)
	r.NextAttemptAt = normalTimePtr(r.NextAttemptAt)
	if len(r.Metadata) == 0 {
		r.Metadata = nil
	}
	if len(r.Definition.Tools) == 0 {
		r.Definition.Tools = nil
	}
	return r
}

func normalStep(s agent.Step) agent.Step {
	s.CreatedAt = normalTime(s.CreatedAt)
	s.StartedAt = normalTimePtr(s.StartedAt)
	s.FinishedAt = normalTimePtr(s.FinishedAt)
	if s.Message != nil {
		m := normalMessage(*s.Message)
		s.Message = &m
	}
	return s
}

func normalMessage(m agent.Message) agent.Message {
	if len(m.Calls) == 0 {
		m.Calls = nil
	}
	if len(m.Results) == 0 {
		m.Results = nil
	}
	return m
}

func normalSteps(steps []agent.Step) []agent.Step {
	out := make([]agent.Step, len(steps))
	for i, s := range steps {
		out[i] = normalStep(s)
	}
	return out
}

func normalApproval(a agent.Approval) agent.Approval {
	a.RequestedAt = normalTime(a.RequestedAt)
	a.DecidedAt = normalTimePtr(a.DecidedAt)
	a.ExpiresAt = normalTimePtr(a.ExpiresAt)
	if len(a.Action.Attrs) == 0 {
		a.Action.Attrs = nil
	}
	return a
}

func normalApprovals(approvals []agent.Approval) []agent.Approval {
	out := make([]agent.Approval, len(approvals))
	for i, a := range approvals {
		out[i] = normalApproval(a)
	}
	return out
}

func runIDs(runs []agent.Run) []string {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids
}

func approvalIDs(approvals []agent.Approval) []string {
	ids := make([]string, len(approvals))
	for i, a := range approvals {
		ids[i] = a.ID
	}
	return ids
}

func stepSeqs(steps []agent.Step) []int {
	seqs := make([]int, len(steps))
	for i, s := range steps {
		seqs[i] = s.Seq
	}
	return seqs
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }
