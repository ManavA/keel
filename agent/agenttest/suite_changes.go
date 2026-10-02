package agenttest

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

func changesCases() []storeCase {
	changes := func(k *kit, runID string, since int64) agent.Changes {
		k.t.Helper()
		c, err := k.store.Changes(k.ctx, runID, since)
		require.NoError(k.t, err)
		return c
	}
	return []storeCase{
		{"since 0, returns the run, its whole journal and every approval", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolLookup, lookupInput), Call("call-2", toolSend, sendInput))
			k.update(lease, 2, agent.StepProposed, agent.StepCompleted)
			k.ask(lease, 3, nil)

			got := changes(k, run.ID, 0)

			k.equalRun(k.run(run.ID), got.Run)
			k.equalSteps(k.steps(run.ID), got.Steps)
			k.equalApprovals(k.approvals(run.ID), got.Approvals)
			assert.Len(k.t, got.Steps, 3)
			assert.Len(k.t, got.Approvals, 1)
		}},
		{"a run with no journal has only itself to return", func(k *kit) {
			run := k.create(agentAlpha)

			got := changes(k, run.ID, 0)

			k.equalRun(run, got.Run)
			assert.Empty(k.t, got.Steps)
			assert.Empty(k.t, got.Approvals)
		}},
		{"returns only the steps touched after the revision", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolLookup, lookupInput), Call("call-2", toolSend, sendInput))
			since := k.run(run.ID).Rev

			k.update(lease, 3, agent.StepProposed, agent.StepStarted)

			got := changes(k, run.ID, since)
			assert.Equal(k.t, since+1, got.Run.Rev)
			assert.Equal(k.t, []int{3}, stepSeqs(got.Steps), "the reply and its other call were not touched")
			k.equalSteps([]agent.Step{k.step(run.ID, 3)}, got.Steps)
			assert.Empty(k.t, got.Approvals)
		}},
		{"an answer touches its approval and not the step", func(k *kit) {
			run, pending := k.parked(agentAlpha, nil)
			since := run.Rev

			decided := k.decide(pending.ID, true)

			got := changes(k, run.ID, since)
			assert.Empty(k.t, got.Steps)
			k.equalApprovals([]agent.Approval{decided}, got.Approvals)
		}},
		{"a question touches its step and its approval", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			since := run.Rev

			asked := k.ask(lease, 2, nil)

			got := changes(k, run.ID, since)
			assert.Equal(k.t, []int{2}, stepSeqs(got.Steps))
			k.equalApprovals([]agent.Approval{asked}, got.Approvals)
		}},
		{"a change to the lease touches the run alone", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			since := run.Rev

			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Now: k.tick()}))
			k.claim(workerB, run.ID)

			got := changes(k, run.ID, since)
			assert.Equal(k.t, since+2, got.Run.Rev)
			assert.Empty(k.t, got.Steps)
			assert.Empty(k.t, got.Approvals)
		}},
		{"at the run's revision or past it, returns the run and nothing else", func(k *kit) {
			run, _ := k.parked(agentAlpha, nil)

			for _, since := range []int64{run.Rev, run.Rev + 10} {
				got := changes(k, run.ID, since)

				k.equalRun(run, got.Run)
				assert.Empty(k.t, got.Steps, "since %d", since)
				assert.Empty(k.t, got.Approvals, "since %d", since)
			}
		}},
		{"returns steps in journal order and approvals oldest first, whatever order they changed in", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease,
				Call("call-1", toolSend, sendInput), Call("call-2", toolSend, sendInput), Call("call-3", toolSend, sendInput))
			since := k.run(run.ID).Rev
			// The question recorded first carries the later time: oldest is
			// by RequestedAt, which is the caller's, and not by arrival.
			late := k.askRequest(4)
			late.Now = k.now().Add(time.Minute)
			_, err := k.store.RequestApproval(k.ctx, lease, late)
			require.NoError(k.t, err)
			early := k.askRequest(2)
			early.Now = k.now().Add(time.Second)
			_, err = k.store.RequestApproval(k.ctx, lease, early)
			require.NoError(k.t, err)
			k.update(lease, 3, agent.StepProposed, agent.StepStarted)
			// The older question is the one touched last.
			k.decide(late.ID, true)
			k.decide(early.ID, false)

			got := changes(k, run.ID, since)

			assert.Equal(k.t, []int{2, 3, 4}, stepSeqs(got.Steps))
			assert.Equal(k.t, []string{early.ID, late.ID}, approvalIDs(got.Approvals))
			assert.Equal(k.t, []int{1, 2, 3, 4}, stepSeqs(changes(k, run.ID, 0).Steps))
		}},
		{"one run's changes hold nothing of another's", func(k *kit) {
			other, _ := k.parked(agentAlpha, nil)
			run := k.create(agentAlpha)

			got := changes(k, run.ID, 0)

			assert.Empty(k.t, got.Steps)
			assert.Empty(k.t, got.Approvals)
			assert.Len(k.t, changes(k, other.ID, 0).Steps, 2)
		}},
		{"every change adds one to Rev, and reading the changes since rebuilds the run", func(k *kit) {
			followRevisions(k)
		}},
		{"a reader following a run while it is written to misses nothing", func(k *kit) {
			followWhileWritten(k)
		}},
	}
}

// followRevisions takes one run through a whole life, and after every store
// call does what a reader of the event stream does: asks for the changes
// since the last revision it saw and lays them over what it has. What it has
// must then be what the store holds, and the revision must have moved by one
// for a call that changed something and not at all for one that did not.
func followRevisions(k *kit) {
	created := k.create(agentAlpha)
	runID := created.ID
	child := agent.Run{}
	var lease agent.Lease
	var approval agent.Approval
	result := "sent"

	claim := func(owner string) func(now time.Time) error {
		return func(now time.Time) error {
			run, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: owner, Agents: suiteAgents, RunID: runID, Now: now, TTL: suiteTTL,
			})
			if err == nil {
				lease = run.Lease()
			}
			return err
		}
	}
	// refused runs a call the store must refuse with want, which is then no
	// failure of the life being followed.
	refused := func(want error, call func(now time.Time) error) func(now time.Time) error {
		return func(now time.Time) error {
			err := call(now)
			if errors.Is(err, want) {
				return nil
			}
			return errors.Join(errors.New("the call was not refused as expected"), err)
		}
	}

	life := []struct {
		name string
		// changes says whether the call changes the run.
		changes bool
		call    func(now time.Time) error
	}{
		{"Claim", true, claim(workerA)},
		{"Heartbeat", false, func(now time.Time) error {
			_, err := k.store.Heartbeat(k.ctx, lease, now, suiteTTL)
			return err
		}},
		{"BeginModel", true, func(now time.Time) error { return k.store.BeginModel(k.ctx, lease, 1, now) }},
		{"BeginModel again", true, func(now time.Time) error { return k.store.BeginModel(k.ctx, lease, 1, now) }},
		{"BeginModel at a wrong seq", false, refused(agent.ErrConflict, func(now time.Time) error {
			return k.store.BeginModel(k.ctx, lease, 5, now)
		})},
		{"CompleteModel", true, func(now time.Time) error {
			return k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1,
				Message: Use(
					Call("call-1", toolLookup, lookupInput),
					Call("call-2", toolSend, sendInput),
					Call("call-3", toolLookup, lookupInput),
				).Message,
				Stop:  agent.StopToolUse,
				Model: suiteModel,
				Usage: agent.Usage{InputTokens: 100, OutputTokens: 20, CostMicros: 300},
				Now:   now,
			})
		}},
		{"CompleteModel again", false, refused(agent.ErrConflict, func(now time.Time) error {
			return k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Say("done").Message, Stop: agent.StopEnd, Model: suiteModel, Now: now,
			})
		})},
		{"UpdateStep, the first call started", true, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepStarted, Decision: agent.Allow, Rule: suiteRule, Now: now,
			})
		}},
		{"UpdateStep, the first call completed", true, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Result: &result, Now: now,
			})
		}},
		{"UpdateStep from a status the step is not in", false, refused(agent.ErrConflict, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Result: &result, Now: now,
			})
		})},
		{"RequestApproval", true, func(now time.Time) error {
			req := k.askRequest(3)
			req.Now = now
			var err error
			approval, err = k.store.RequestApproval(k.ctx, lease, req)
			return err
		}},
		{"RequestApproval again", false, func(now time.Time) error {
			req := k.askRequest(3)
			req.Now = now
			_, err := k.store.RequestApproval(k.ctx, lease, req)
			return err
		}},
		{"UpdateStep, the third call started", true, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 4, From: agent.StepProposed, To: agent.StepStarted, Now: now,
			})
		}},
		{"CreateRun of a child", false, func(now time.Time) error {
			child = agent.Run{
				ID: uuid.NewString(), Agent: agentBeta, Status: agent.StatusRunnable, Input: lookupInput,
				ParentID: runID, ParentSeq: 4, Depth: 1, Key: agent.StepKey(runID, 4),
				Definition: agent.Snapshot{System: "system"}, CreatedAt: now, UpdatedAt: now,
			}
			_, _, err := k.store.CreateRun(k.ctx, child)
			return err
		}},
		{"UpdateStep, the third call waiting on the child", true, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 4, From: agent.StepStarted, To: agent.StepWaiting, ChildRunID: child.ID, Now: now,
			})
		}},
		{"Park", true, func(now time.Time) error {
			parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
			if err == nil && !parked {
				return errNotParked
			}
			return err
		}},
		{"Park under the spent lease", false, refused(agent.ErrLeaseLost, func(now time.Time) error {
			_, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
			return err
		})},
		{"DecideApproval", true, func(now time.Time) error {
			_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{
				ID: approval.ID, Approved: true, By: personA, Reason: "checked", Now: now,
			})
			return err
		}},
		{"DecideApproval again", false, refused(agent.ErrAlreadyDecided, func(now time.Time) error {
			_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: approval.ID, By: personB, Now: now})
			return err
		})},
		{"ExpireApprovals with nothing due", false, func(now time.Time) error {
			_, err := k.store.ExpireApprovals(k.ctx, now)
			return err
		}},
		{"Claim by another worker", true, claim(workerB)},
		{"UpdateStep, the approved call started", true, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 3, From: agent.StepWaiting, To: agent.StepStarted, Now: now,
			})
		}},
		{"UpdateStep, the approved call completed", true, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 3, From: agent.StepStarted, To: agent.StepCompleted, Result: &result, Now: now,
			})
		}},
		{"Park with only the child to wait for", true, func(now time.Time) error {
			parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonChildren, Now: now})
			if err == nil && !parked {
				return errNotParked
			}
			return err
		}},
		{"Finish of the child", true, func(now time.Time) error {
			held, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: suiteAgents, RunID: child.ID, Now: now, TTL: suiteTTL,
			})
			if err != nil {
				return err
			}
			return k.store.Finish(k.ctx, held.Lease(), agent.FinishRequest{
				Status: agent.StatusCompleted, Output: "found", Now: now,
			})
		}},
		{"Claim once the child has ended", true, claim(workerA)},
		{"UpdateStep, the child collected", true, func(now time.Time) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 4, From: agent.StepWaiting, To: agent.StepCompleted, Result: &result,
				Usage: agent.Usage{InputTokens: 40, OutputTokens: 8, CostMicros: 90}, Now: now,
			})
		}},
		{"BeginModel for the next reply", true, func(now time.Time) error { return k.store.BeginModel(k.ctx, lease, 5, now) }},
		{"CompleteModel with another question", true, func(now time.Time) error {
			return k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 5, Message: Use(Call("call-4", toolSend, sendInput)).Message, Stop: agent.StopToolUse, Model: suiteModel, Now: now,
			})
		}},
		{"RequestApproval for it", true, func(now time.Time) error {
			req := k.askRequest(6)
			req.Now = now
			_, err := k.store.RequestApproval(k.ctx, lease, req)
			return err
		}},
		{"RequestCancel", true, func(now time.Time) error {
			return k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: runID, By: personA, Reason: "wrong batch", Now: now})
		}},
		{"RequestCancel again", false, func(now time.Time) error {
			return k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: runID, By: personB, Reason: "asked twice", Now: now})
		}},
		{"Park once cancellation is requested", false, func(now time.Time) error {
			parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
			if err == nil && parked {
				return errors.New("park reported true")
			}
			return err
		}},
		{"Yield", true, func(now time.Time) error { return k.store.Yield(k.ctx, lease, agent.YieldRequest{Now: now}) }},
		{"Claim to end it", true, claim(workerB)},
		{"Finish", true, func(now time.Time) error {
			return k.store.Finish(k.ctx, lease, agent.FinishRequest{
				Status: agent.StatusCancelled, Reason: agent.ReasonCancelled, Now: now,
			})
		}},
		{"RequestCancel of the ended run", false, refused(agent.ErrFinished, func(now time.Time) error {
			return k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: runID, By: personA, Now: now})
		})},
	}

	// What the reader has, and the last revision it saw.
	seen := created.Rev
	steps := map[int]agent.Step{}
	approvals := map[string]agent.Approval{}
	updatedAt := created.UpdatedAt

	for _, op := range life {
		now := k.tick()
		require.NoError(k.t, op.call(now), op.name)

		got, err := k.store.Changes(k.ctx, runID, seen)
		require.NoError(k.t, err, op.name)
		if op.changes {
			require.Equal(k.t, seen+1, got.Run.Rev, "%s changes the run once", op.name)
			assert.True(k.t, now.Equal(got.Run.UpdatedAt), "%s stamps the run with the time it was given", op.name)
			updatedAt = now
		} else {
			require.Equal(k.t, seen, got.Run.Rev, "%s changes nothing", op.name)
			assert.True(k.t, updatedAt.Equal(got.Run.UpdatedAt), "%s leaves UpdatedAt alone", op.name)
			assert.Empty(k.t, got.Steps, op.name)
			assert.Empty(k.t, got.Approvals, op.name)
		}
		for _, s := range got.Steps {
			assert.Equal(k.t, got.Run.Rev, s.Rev, "%s: step %d carries the revision that touched it", op.name, s.Seq)
			steps[s.Seq] = s
		}
		for _, a := range got.Approvals {
			assert.Equal(k.t, got.Run.Rev, a.Rev, "%s: approval carries the revision that touched it", op.name)
			approvals[a.ID] = a
		}
		seen = got.Run.Rev

		journal := k.steps(runID)
		require.Len(k.t, steps, len(journal), op.name)
		for _, s := range journal {
			assert.Equal(k.t, normalStep(s), normalStep(steps[s.Seq]), "%s: step %d", op.name, s.Seq)
			assert.LessOrEqual(k.t, s.Rev, seen, "%s: no step is ahead of its run", op.name)
		}
		asked := k.approvals(runID)
		require.Len(k.t, approvals, len(asked), op.name)
		for _, a := range asked {
			assert.Equal(k.t, normalApproval(a), normalApproval(approvals[a.ID]), "%s: approval %s", op.name, a.ID)
			assert.LessOrEqual(k.t, a.Rev, seen, "%s: no approval is ahead of its run", op.name)
			assert.Equal(k.t, a.Status == agent.ApprovalPending, a.DecidedAt == nil,
				"%s: an approval has no DecidedAt exactly while it is pending (it is %s)", op.name, a.Status)
		}
	}

	ended := k.run(runID)
	assert.Equal(k.t, agent.StatusCancelled, ended.Status)
	assert.Equal(k.t, []int{1, 2, 3, 4, 5, 6}, stepSeqs(k.steps(runID)))
}

// followWhileWritten has one goroutine journal a run under its lease while
// the test reads the changes since the last revision it saw, as the event
// stream does from another process. Once the writer stops, what the reader
// has pieced together must be the journal: a change the reader's revision
// moved past without having read it would be lost for good. A step may be
// delivered more than once, and newer each time, never older.
//
// It does not assert that a step is never ahead of the run it came with: a
// store that reads the run and then its steps may return a step written in
// between, which the next read delivers again and nothing loses.
func followWhileWritten(k *kit) {
	const turns = 30
	run, lease := k.held(agentAlpha)

	written := make(chan error, 1)
	go func() {
		written <- func() error {
			for turn := range turns {
				seq := turn*3 + 1
				if err := k.store.BeginModel(k.ctx, lease, seq, k.tick()); err != nil {
					return err
				}
				if err := k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
					Seq:     seq,
					Message: Use(Call("call-1", toolLookup, lookupInput), Call("call-2", toolSend, sendInput)).Message,
					Stop:    agent.StopToolUse,
					Model:   suiteModel,
					Now:     k.tick(),
				}); err != nil {
					return err
				}
				for _, call := range []int{seq + 1, seq + 2} {
					for _, move := range [][2]agent.StepStatus{
						{agent.StepProposed, agent.StepStarted},
						{agent.StepStarted, agent.StepCompleted},
					} {
						if err := k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
							Seq: call, From: move[0], To: move[1], Now: k.tick(),
						}); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}()
	}()

	var since int64
	have := map[int]agent.Step{}
	read := func() {
		got, err := k.store.Changes(k.ctx, run.ID, since)
		require.NoError(k.t, err)
		for _, s := range got.Steps {
			if earlier, ok := have[s.Seq]; ok {
				assert.GreaterOrEqual(k.t, s.Rev, earlier.Rev, "step %d was delivered again as it was before", s.Seq)
			}
			have[s.Seq] = s
		}
		assert.GreaterOrEqual(k.t, got.Run.Rev, since, "a run's revision never goes back")
		since = got.Run.Rev
	}
	for writing := true; writing; {
		select {
		case err := <-written:
			require.NoError(k.t, err)
			writing = false
		default:
			read()
		}
	}
	read()

	journal := k.steps(run.ID)
	require.Len(k.t, journal, turns*3)
	require.Len(k.t, have, len(journal))
	for _, s := range journal {
		assert.Equal(k.t, normalStep(s), normalStep(have[s.Seq]), "step %d", s.Seq)
	}
	assert.Equal(k.t, k.run(run.ID).Rev, since)
}
