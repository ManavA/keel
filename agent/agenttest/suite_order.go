package agenttest

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

// orderCases pin the order a store makes its checks in, so that two stores
// give the same error for a call that is wrong in two ways. What can be
// judged from the request alone is refused first, before the store is
// touched. Then a run that does not exist is ErrNotFound. Then a lease that
// is not the run's is ErrLeaseLost. Only then does the store look at the
// state of the run, its steps and its approvals.
func orderCases() []storeCase {
	type refusal struct {
		name string
		// prepare journals, under the lease held, what the call needs to
		// reach the check it is here for.
		prepare func(k *kit, held agent.Lease)
		call    func(k *kit, lease agent.Lease) error
	}
	oneCall := func(k *kit, held agent.Lease) { k.reply(held, Call("call-1", toolSend, sendInput)) }

	// Calls on a method that takes a Lease, refused for their arguments.
	arguments := []refusal{
		{"Heartbeat with a TTL of zero", nil, func(k *kit, lease agent.Lease) error {
			_, err := k.store.Heartbeat(k.ctx, lease, k.tick(), 0)
			return err
		}},
		{"Finish with a status that is not final", nil, func(k *kit, lease agent.Lease) error {
			return k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: agent.StatusRunnable, Now: k.tick()})
		}},
		{"UpdateStep with a To that is no step status", oneCall, func(k *kit, lease agent.Lease) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{Seq: 2, From: agent.StepProposed, To: "done", Now: k.tick()})
		}},
		{"UpdateStep with a child run id that is not a UUID", oneCall, func(k *kit, lease agent.Lease) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepWaiting, ChildRunID: malformedID, Now: k.tick(),
			})
		}},
		{"RequestApproval with an id that is not a UUID", oneCall, func(k *kit, lease agent.Lease) error {
			req := k.askRequest(2)
			req.ID = malformedID
			_, err := k.store.RequestApproval(k.ctx, lease, req)
			return err
		}},
		{"RequestApproval with attributes JSON cannot hold", oneCall, func(k *kit, lease agent.Lease) error {
			req := k.askRequest(2)
			req.Action.Attrs = map[string]any{"reply": make(chan string)}
			_, err := k.store.RequestApproval(k.ctx, lease, req)
			return err
		}},
	}

	// Calls on a method that takes a Lease, refused for the state they find,
	// or, for the last two, answered without a change.
	states := []refusal{
		{"BeginModel at a wrong seq", nil, func(k *kit, lease agent.Lease) error {
			return k.store.BeginModel(k.ctx, lease, 5, k.tick())
		}},
		{"CompleteModel of a step that was never begun", nil, func(k *kit, lease agent.Lease) error {
			return k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Say("done").Message, Stop: agent.StopEnd, Model: suiteModel, Now: k.tick(),
			})
		}},
		{"UpdateStep of a step that is not in From", oneCall, func(k *kit, lease agent.Lease) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Now: k.tick(),
			})
		}},
		{"UpdateStep of a step that does not exist", nil, func(k *kit, lease agent.Lease) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 9, From: agent.StepProposed, To: agent.StepStarted, Now: k.tick(),
			})
		}},
		{"RequestApproval of a step that is not in From", oneCall, func(k *kit, lease agent.Lease) error {
			req := k.askRequest(2)
			req.From = agent.StepStarted
			_, err := k.store.RequestApproval(k.ctx, lease, req)
			return err
		}},
		{
			"RequestApproval asked again",
			func(k *kit, held agent.Lease) {
				oneCall(k, held)
				k.ask(held, 2, nil)
			},
			func(k *kit, lease agent.Lease) error {
				_, err := k.store.RequestApproval(k.ctx, lease, k.askRequest(2))
				return err
			},
		},
		{"Park with nothing to wait for", nil, func(k *kit, lease agent.Lease) error {
			_, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: k.tick()})
			return err
		}},
	}

	// stale gives a run its journal under one lease and then has another
	// worker take it over.
	stale := func(k *kit, prepare func(k *kit, held agent.Lease)) (agent.Run, agent.Lease) {
		run, held := k.held(agentAlpha)
		if prepare != nil {
			prepare(k, held)
		}
		k.lapse()
		k.claim(workerB, run.ID)
		return run, held
	}

	var cases []storeCase
	for _, r := range arguments {
		cases = append(cases,
			storeCase{r.name + ", under a lease that was lost, is refused for the argument", func(k *kit) {
				run, lost := stale(k, r.prepare)
				before := k.snapshot(run.ID)

				err := r.call(k, lost)

				require.Error(k.t, err)
				assert.NotErrorIs(k.t, err, agent.ErrLeaseLost, "the argument is judged before the lease")
				k.unchanged(before)
			}},
			storeCase{r.name + ", for a run that does not exist, is refused for the argument", func(k *kit) {
				err := r.call(k, agent.Lease{RunID: newID(), Owner: workerA, Epoch: 1})

				require.Error(k.t, err)
				assert.NotErrorIs(k.t, err, agent.ErrNotFound, "the argument is judged before the run is looked for")
				assert.NotErrorIs(k.t, err, agent.ErrLeaseLost)
			}},
		)
	}
	for _, r := range states {
		cases = append(cases, storeCase{r.name + ", under a lease that was lost, is ErrLeaseLost", func(k *kit) {
			run, lost := stale(k, r.prepare)
			before := k.snapshot(run.ID)

			err := r.call(k, lost)

			require.ErrorIs(k.t, err, agent.ErrLeaseLost, "the lease is judged before the run's state")
			k.unchanged(before)
		}})
	}

	cases = append(cases,
		storeCase{"Claim for a run that does not exist, with an argument it refuses, is refused for the argument", func(k *kit) {
			tests := []struct {
				name string
				req  agent.ClaimRequest
			}{
				{"a TTL of zero", agent.ClaimRequest{Owner: workerA, Agents: suiteAgents, RunID: newID(), TTL: 0}},
				{"no owner", agent.ClaimRequest{Agents: suiteAgents, RunID: newID(), TTL: suiteTTL}},
			}
			for _, tt := range tests {
				tt.req.Now = k.tick()

				got, err := k.store.Claim(k.ctx, tt.req)

				require.Error(k.t, err, tt.name)
				assert.NotErrorIs(k.t, err, agent.ErrNotFound, tt.name)
				assert.Nil(k.t, got, tt.name)
			}
		}},
		storeCase{"CreateRun that would find its key is still refused for what the request gets wrong", func(k *kit) {
			first := k.newRun(agentAlpha)
			first.Key = suiteStartKey
			stored := k.insert(first)

			tests := []struct {
				name string
				edit func(run *agent.Run)
			}{
				{"an id that is not a UUID", func(run *agent.Run) { run.ID = malformedID }},
				{"a parent id that is not a UUID", func(run *agent.Run) { run.ParentID = malformedID }},
				{"a status that is not runnable", func(run *agent.Run) { run.Status = agent.StatusWaiting }},
			}
			for _, tt := range tests {
				again := k.newRun(agentAlpha)
				again.Key = suiteStartKey
				tt.edit(&again)

				_, created, err := k.store.CreateRun(k.ctx, again)

				require.Error(k.t, err, tt.name)
				assert.False(k.t, created, tt.name)
				k.equalRun(stored, k.run(first.ID))
			}
		}},
	)
	return cases
}
