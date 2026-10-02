package agenttest

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

func requestApprovalCases() []storeCase {
	return []storeCase{
		{"sets the step waiting and records the question", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			proposed := k.step(run.ID, 2)
			now := k.tick()
			expires := now.Add(time.Hour)
			req := agent.ApprovalRequest{
				ID:    uuid.NewString(),
				Seq:   2,
				From:  agent.StepProposed,
				Cause: agent.CauseGuard,
				Action: agent.Action{
					Kind: "run", Target: toolSend,
					Attrs: map[string]any{agent.AttrAgent: agentAlpha, agent.AttrTool: toolSend},
				},
				Decision:  agent.Ask,
				Rule:      suiteRule,
				ExpiresAt: &expires,
				Now:       now,
			}

			got, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err)
			after := k.run(run.ID)
			want := agent.Approval{
				ID: req.ID, RunID: run.ID, Seq: 2, Attempt: 0, Cause: agent.CauseGuard,
				Tool: toolSend, Input: raw(sendInput), Action: req.Action, Rule: suiteRule,
				Status: agent.ApprovalPending, Rev: after.Rev,
				RequestedAt: now, ExpiresAt: &expires,
			}
			k.equalApproval(want, got)
			k.equalApproval(want, k.approval(req.ID))

			wantStep := proposed
			wantStep.Status, wantStep.Decision, wantStep.Rule, wantStep.Rev = agent.StepWaiting, agent.Ask, suiteRule, after.Rev
			k.equalSteps([]agent.Step{wantStep}, k.steps(run.ID)[1:])
			wantRun := run
			wantRun.Rev, wantRun.UpdatedAt = run.Rev+1, now
			k.equalRun(wantRun, after)
		}},
		{"records a question that has no attributes and never lapses", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			req := k.askRequest(2)

			got, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err)
			want := agent.Approval{
				ID: req.ID, RunID: run.ID, Seq: 2, Cause: agent.CauseGuard,
				Tool: toolSend, Input: raw(sendInput), Action: req.Action, Rule: suiteRule,
				Status: agent.ApprovalPending, Rev: run.Rev + 1, RequestedAt: req.Now,
			}
			k.equalApproval(want, got)
			k.equalApproval(want, k.approval(req.ID))
		}},
		{"asked again for the same step and attempt, returns the approval already recorded", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			first := k.ask(lease, 2, nil)
			before := k.snapshot(run.ID)

			// The same question from an execution that does not know it was
			// asked: under an id of its own and with other words.
			again := k.askRequest(2)
			again.Cause, again.Rule = agent.CauseTool, agent.RuleToolApproval
			got, err := k.store.RequestApproval(k.ctx, lease, again)

			require.NoError(k.t, err)
			k.equalApproval(first, got)
			k.unchanged(before)
			_, err = k.store.GetApproval(k.ctx, again.ID)
			assert.ErrorIs(k.t, err, agent.ErrNotFound, "no second approval was recorded")
		}},
		{"asked again after the answer, returns the answer", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			first := k.ask(lease, 2, nil)
			decided := k.decide(first.ID, true)
			before := k.snapshot(run.ID)

			got, err := k.store.RequestApproval(k.ctx, lease, k.askRequest(2))

			require.NoError(k.t, err)
			k.equalApproval(decided, got)
			assert.Equal(k.t, agent.ApprovalApproved, got.Status)
			k.unchanged(before)
		}},
		{"an interrupted call is asked about under the attempt that was interrupted", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			req := k.askRequest(2)
			req.From, req.Cause, req.Rule = agent.StepStarted, agent.CauseInterrupted, agent.RuleInterrupted

			got, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err)
			assert.Equal(k.t, 1, got.Attempt)
			assert.Equal(k.t, agent.CauseInterrupted, got.Cause)
			assert.Equal(k.t, agent.RuleInterrupted, got.Rule)
			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepWaiting, step.Status)
			assert.Equal(k.t, 1, step.Attempts, "asking does not count as an attempt")
		}},
		{"a step interrupted after it was approved is asked about again", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			first := k.ask(lease, 2, nil)
			k.decide(first.ID, true)
			k.update(lease, 2, agent.StepWaiting, agent.StepStarted)
			req := k.askRequest(2)
			req.From, req.Cause, req.Rule = agent.StepStarted, agent.CauseInterrupted, agent.RuleInterrupted

			second, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err)
			assert.Equal(k.t, req.ID, second.ID, "a new question, for the new attempt")
			assert.Equal(k.t, 1, second.Attempt)
			assert.Equal(k.t, agent.ApprovalPending, second.Status)
			approvals := k.approvals(run.ID)
			require.Len(k.t, approvals, 2)
			assert.Equal(k.t, []string{first.ID, second.ID}, approvalIDs(approvals))
			assert.Equal(k.t, agent.ApprovalApproved, approvals[0].Status, "the first answer stands")
		}},
		{"a step that is not in From is ErrConflict", func(k *kit) {
			run, lease := k.proposed(agentAlpha)

			tests := []struct {
				name string
				seq  int
				from agent.StepStatus
			}{
				{"a proposed step, asked about as started", 2, agent.StepStarted},
				{"a model step", 1, agent.StepCompleted},
				{"no such step", 3, agent.StepProposed},
			}
			for _, tt := range tests {
				before := k.snapshot(run.ID)
				req := k.askRequest(tt.seq)
				req.From = tt.from

				_, err := k.store.RequestApproval(k.ctx, lease, req)

				require.ErrorIs(k.t, err, agent.ErrConflict, tt.name)
				k.unchanged(before)
				_, err = k.store.GetApproval(k.ctx, req.ID)
				assert.ErrorIs(k.t, err, agent.ErrNotFound, tt.name)
			}
		}},
	}
}

func parkCases() []storeCase {
	park := func(k *kit, lease agent.Lease, reason string) (bool, time.Time) {
		k.t.Helper()
		now := k.tick()
		parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: reason, Now: now})
		require.NoError(k.t, err)
		return parked, now
	}
	cases := []storeCase{
		{"with a pending approval, sets the run waiting and releases the lease", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.ask(lease, 2, nil)
			run = k.run(run.ID)

			parked, now := park(k, lease, agent.ReasonApproval)

			require.True(k.t, parked)
			want := run
			want.Status, want.Reason = agent.StatusWaiting, agent.ReasonApproval
			want.LeaseOwner, want.LeaseExpiresAt = "", nil
			want.Rev, want.UpdatedAt = run.Rev+1, now
			after := k.run(run.ID)
			k.equalRun(want, after)
			assert.Equal(k.t, lease.Epoch, after.LeaseEpoch)
		}},
		{"with a child run that has not ended, sets the run waiting", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			child := k.createChild(run)
			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepWaiting, ChildRunID: child.ID, Now: k.tick(),
			}))

			parked, _ := park(k, lease, agent.ReasonChildren)

			require.True(k.t, parked)
			after := k.run(run.ID)
			assert.Equal(k.t, agent.StatusWaiting, after.Status)
			assert.Equal(k.t, agent.ReasonChildren, after.Reason)
			assert.Empty(k.t, after.LeaseOwner)
		}},
		{"a child being executed is still a child that has not ended", func(k *kit) {
			run, lease := k.held(agentAlpha)
			child := k.createChild(run)
			k.claim(workerB, child.ID)

			parked, _ := park(k, lease, agent.ReasonChildren)

			assert.True(k.t, parked)
		}},
		{"with nothing to wait for, changes nothing and reports false", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			before := k.snapshot(run.ID)

			parked, _ := park(k, lease, agent.ReasonApproval)

			assert.False(k.t, parked)
			k.unchanged(before)
			// The execution still holds the run and carries on.
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
		}},
		{"an approval that has its answer is nothing to wait for", func(k *kit) {
			answers := []struct {
				name   string
				answer func(k *kit, approval agent.Approval)
			}{
				{"approved", func(k *kit, a agent.Approval) { k.decide(a.ID, true) }},
				{"declined", func(k *kit, a agent.Approval) { k.decide(a.ID, false) }},
				{"lapsed", func(k *kit, a agent.Approval) {
					n, err := k.store.ExpireApprovals(k.ctx, *a.ExpiresAt)
					require.NoError(k.t, err)
					require.Equal(k.t, 1, n)
				}},
			}
			for _, tt := range answers {
				run, lease := k.proposed(agentAlpha)
				expires := k.now().Add(time.Hour)
				tt.answer(k, k.ask(lease, 2, &expires))
				before := k.snapshot(run.ID)

				parked, _ := park(k, lease, agent.ReasonApproval)

				assert.False(k.t, parked, tt.name)
				k.unchanged(before)
			}
		}},
		{"one approval without its answer is enough to wait for", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolSend, sendInput), Call("call-2", toolSend, sendInput))
			answered := k.ask(lease, 2, nil)
			k.ask(lease, 3, nil)
			k.decide(answered.ID, true)

			parked, _ := park(k, lease, agent.ReasonApproval)

			assert.True(k.t, parked)
			assert.Equal(k.t, agent.StatusWaiting, k.run(run.ID).Status)
		}},
		{"a child run that has ended is nothing to wait for", func(k *kit) {
			for _, status := range finalStatuses {
				run, lease := k.held(agentAlpha)
				child := k.createChild(run)
				k.finish(k.claim(workerB, child.ID), status)
				before := k.snapshot(run.ID)

				parked, _ := park(k, lease, agent.ReasonChildren)

				assert.False(k.t, parked, status)
				k.unchanged(before)
			}
		}},
		{"another run's approvals and children are nothing to wait for", func(k *kit) {
			other, otherLease := k.proposed(agentAlpha)
			k.ask(otherLease, 2, nil)
			k.createChild(other)
			run, lease := k.held(agentAlpha)
			before := k.snapshot(run.ID)

			parked, _ := park(k, lease, agent.ReasonApproval)

			assert.False(k.t, parked)
			k.unchanged(before)
		}},
		{"with cancellation requested, reports false whatever is pending", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.ask(lease, 2, nil)
			k.createChild(run)
			require.NoError(k.t, k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: run.ID, By: personA, Now: k.tick()}))
			before := k.snapshot(run.ID)

			parked, _ := park(k, lease, agent.ReasonApproval)

			assert.False(k.t, parked)
			k.unchanged(before)
		}},
		{"once parked, the lease is spent", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.ask(lease, 2, nil)
			k.park(lease, agent.ReasonApproval)
			before := k.snapshot(run.ID)

			err := k.store.BeginModel(k.ctx, lease, 3, k.tick())
			require.ErrorIs(k.t, err, agent.ErrLeaseLost)
			_, err = k.store.Heartbeat(k.ctx, lease, k.tick(), suiteTTL)
			require.ErrorIs(k.t, err, agent.ErrLeaseLost)
			_, err = k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: k.tick()})
			require.ErrorIs(k.t, err, agent.ErrLeaseLost)
			k.unchanged(before)
		}},
	}
	return append(cases, parkRaceCases()...)
}

// parkRaceCases are Park against each thing that can take away what a run
// was about to wait for. Both lock the run, so whichever goes first the run
// ends up runnable: parked and then woken, or never parked.
func parkRaceCases() []storeCase {
	const rounds = 50
	races := []struct {
		name   string
		reason string
		// arm gives the held run something to wait for, and returns what
		// takes it away.
		arm func(k *kit, run agent.Run, lease agent.Lease) (event func(now time.Time) error)
	}{
		{"an answer", agent.ReasonApproval, func(k *kit, _ agent.Run, lease agent.Lease) func(time.Time) error {
			approval := k.ask(lease, 2, nil)
			return func(now time.Time) error {
				_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: approval.ID, Approved: true, By: personA, Now: now})
				return err
			}
		}},
		{"its approval lapsing", agent.ReasonApproval, func(k *kit, _ agent.Run, lease agent.Lease) func(time.Time) error {
			due := k.now().Add(time.Second)
			k.ask(lease, 2, &due)
			return func(time.Time) error {
				_, err := k.store.ExpireApprovals(k.ctx, due)
				return err
			}
		}},
		{"its child ending", agent.ReasonChildren, func(k *kit, run agent.Run, _ agent.Lease) func(time.Time) error {
			childLease := k.claim(workerB, k.createChild(run).ID)
			return func(now time.Time) error {
				return k.store.Finish(k.ctx, childLease, agent.FinishRequest{Status: agent.StatusCompleted, Now: now})
			}
		}},
		{"a request to cancel", agent.ReasonApproval, func(k *kit, run agent.Run, lease agent.Lease) func(time.Time) error {
			k.ask(lease, 2, nil)
			return func(now time.Time) error {
				return k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: run.ID, By: personA, Now: now})
			}
		}},
	}

	var cases []storeCase
	for _, race := range races {
		cases = append(cases, storeCase{"racing " + race.name + ", never leaves a run waiting with nothing to wait for", func(k *kit) {
			for range rounds {
				run, lease := k.proposed(agentAlpha)
				event := race.arm(k, run, lease)
				now := k.tick()

				var (
					wg                sync.WaitGroup
					parked            bool
					parkErr, eventErr error
					gate              = make(chan struct{})
				)
				wg.Go(func() {
					<-gate
					parked, parkErr = k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: race.reason, Now: now})
				})
				wg.Go(func() {
					<-gate
					eventErr = event(now)
				})
				close(gate)
				wg.Wait()

				require.NoError(k.t, parkErr)
				require.NoError(k.t, eventErr)
				after := k.run(run.ID)
				require.Equal(k.t, agent.StatusRunnable, after.Status, "parked: %t", parked)
				if parked {
					assert.Empty(k.t, after.LeaseOwner, "parked, it gave up its lease before it was woken")
				} else {
					assert.Equal(k.t, lease.Owner, after.LeaseOwner, "not parked, it still holds the run")
				}
			}
		}})
	}
	return cases
}

func decideApprovalCases() []storeCase {
	return []storeCase{
		{"records the answer with who, why and when", func(k *kit) {
			tests := []struct {
				name     string
				approved bool
				want     agent.ApprovalStatus
			}{
				{"approved", true, agent.ApprovalApproved},
				{"declined", false, agent.ApprovalDeclined},
			}
			for _, tt := range tests {
				run, pending := k.parked(agentAlpha, nil)
				now := k.tick()

				got, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{
					ID: pending.ID, Approved: tt.approved, By: personA, Reason: "checked the address", Now: now,
				})

				require.NoError(k.t, err, tt.name)
				after := k.run(run.ID)
				want := pending
				want.Status, want.DecidedBy, want.Reason, want.DecidedAt = tt.want, personA, "checked the address", &now
				want.Rev = after.Rev
				k.equalApproval(want, got)
				k.equalApproval(want, k.approval(pending.ID))
				assert.Equal(k.t, run.Rev+1, after.Rev, tt.name)
			}
		}},
		{"makes a waiting run runnable", func(k *kit) {
			for _, approved := range []bool{true, false} {
				run, pending := k.parked(agentAlpha, nil)
				now := k.tick()

				_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: pending.ID, Approved: approved, By: personA, Now: now})
				require.NoError(k.t, err)

				want := run
				want.Status, want.Reason = agent.StatusRunnable, ""
				want.Rev, want.UpdatedAt = run.Rev+1, now
				k.equalRun(want, k.run(run.ID))

				lease := k.claim(workerB, run.ID)
				assert.Equal(k.t, run.LeaseEpoch+1, lease.Epoch)
				assert.Zero(k.t, k.run(run.ID).Failures, "a run that was parked did not fail")
			}
		}},
		{"writes nothing to the journal", func(k *kit) {
			run, pending := k.parked(agentAlpha, nil)
			journal := k.steps(run.ID)

			k.decide(pending.ID, false)

			k.equalSteps(journal, k.steps(run.ID))
			assert.Equal(k.t, agent.StepWaiting, k.step(run.ID, 2).Status, "the next execution reads the answer and acts")
		}},
		{"leaves a run that is not waiting as it is, and under its lease", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			pending := k.ask(lease, 2, nil)
			run = k.run(run.ID)
			now := k.tick()

			_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: pending.ID, Approved: true, By: personA, Now: now})
			require.NoError(k.t, err)

			want := run
			want.Rev, want.UpdatedAt = run.Rev+1, now
			k.equalRun(want, k.run(run.ID))
			k.update(lease, 2, agent.StepWaiting, agent.StepStarted)
		}},
		{"with another approval still pending, makes the run runnable all the same", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolSend, sendInput), Call("call-2", toolSend, sendInput))
			first := k.ask(lease, 2, nil)
			second := k.ask(lease, 3, nil)
			k.park(lease, agent.ReasonApproval)

			k.decide(first.ID, true)

			assert.Equal(k.t, agent.StatusRunnable, k.run(run.ID).Status)
			assert.Equal(k.t, agent.ApprovalPending, k.approval(second.ID).Status)
		}},
		{"a second answer returns the first with ErrAlreadyDecided", func(k *kit) {
			tests := []struct {
				name          string
				first, second bool
			}{
				{"approved, then declined", true, false},
				{"declined, then approved", false, true},
				{"approved twice", true, true},
			}
			for _, tt := range tests {
				run, pending := k.parked(agentAlpha, nil)
				first := k.decide(pending.ID, tt.first)
				before := k.snapshot(run.ID)

				got, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{
					ID: pending.ID, Approved: tt.second, By: personB, Reason: "too late", Now: k.tick(),
				})

				require.ErrorIs(k.t, err, agent.ErrAlreadyDecided, tt.name)
				k.equalApproval(first, got)
				assert.Equal(k.t, personA, got.DecidedBy, tt.name)
				k.unchanged(before)
			}
		}},
		{"an approval that lapsed or was cancelled cannot be answered", func(k *kit) {
			expires := k.now().Add(time.Hour)
			lapsedRun, lapsed := k.parked(agentAlpha, &expires)
			n, err := k.store.ExpireApprovals(k.ctx, expires)
			require.NoError(k.t, err)
			require.Equal(k.t, 1, n)

			endedRun, lease := k.proposed(agentAlpha)
			cancelled := k.ask(lease, 2, nil)
			k.finish(lease, agent.StatusFailed)

			tests := []struct {
				name  string
				runID string
				id    string
				want  agent.ApprovalStatus
			}{
				{"lapsed", lapsedRun.ID, lapsed.ID, agent.ApprovalExpired},
				{"cancelled when its run ended", endedRun.ID, cancelled.ID, agent.ApprovalCancelled},
			}
			for _, tt := range tests {
				standing := k.approval(tt.id)
				before := k.snapshot(tt.runID)

				got, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: tt.id, Approved: true, By: personA, Now: k.tick()})

				require.ErrorIs(k.t, err, agent.ErrAlreadyDecided, tt.name)
				assert.Equal(k.t, tt.want, got.Status, tt.name)
				k.equalApproval(standing, got)
				k.equalApproval(standing, k.approval(tt.id))
				k.unchanged(before)
			}
		}},
		{"eight answers at once: one is recorded, and the rest are told which", func(k *kit) {
			run, pending := k.parked(agentAlpha, nil)
			now := k.tick()

			var wg sync.WaitGroup
			got := make([]agent.Approval, 8)
			errs := make([]error, 8)
			gate := make(chan struct{})
			for i := range 8 {
				wg.Go(func() {
					<-gate
					got[i], errs[i] = k.store.DecideApproval(k.ctx, agent.DecideRequest{
						ID: pending.ID, Approved: i%2 == 0, By: workerName(i), Now: now,
					})
				})
			}
			close(gate)
			wg.Wait()

			recorded := k.approval(pending.ID)
			won := 0
			for i := range 8 {
				if errs[i] == nil {
					won++
					assert.Equal(k.t, workerName(i), recorded.DecidedBy)
				} else {
					require.ErrorIs(k.t, errs[i], agent.ErrAlreadyDecided)
				}
				k.equalApproval(recorded, got[i])
			}
			assert.Equal(k.t, 1, won, "an approval is decided once")
			assert.Equal(k.t, run.Rev+1, k.run(run.ID).Rev, "and changes the run once")
		}},
	}
}

func expireApprovalsCases() []storeCase {
	expire := func(k *kit, now time.Time) int {
		k.t.Helper()
		n, err := k.store.ExpireApprovals(k.ctx, now)
		require.NoError(k.t, err)
		return n
	}
	return []storeCase{
		{"with nothing pending, lapses nothing", func(k *kit) {
			assert.Zero(k.t, expire(k, k.tick()))
		}},
		{"lapses only the approvals past their time, and makes their runs runnable", func(k *kit) {
			soon := k.now().Add(time.Hour)
			later := k.now().Add(2 * time.Hour)
			dueRun, due := k.parked(agentAlpha, &soon)
			laterRun, notYet := k.parked(agentAlpha, &later)
			neverRun, never := k.parked(agentAlpha, nil)

			k.clock.Advance(soon.Sub(k.now()) - time.Millisecond)
			beforeDue := k.snapshot(dueRun.ID)
			assert.Zero(k.t, expire(k, k.now()), "a millisecond before the first is due")
			k.unchanged(beforeDue)

			beforeLater, beforeNever := k.snapshot(laterRun.ID), k.snapshot(neverRun.ID)
			now := k.tick()
			assert.Equal(k.t, 1, expire(k, now), "at the instant it is due")

			after := k.run(dueRun.ID)
			want := due
			want.Status, want.DecidedAt, want.Rev = agent.ApprovalExpired, &now, after.Rev
			k.equalApproval(want, k.approval(due.ID))
			wantRun := dueRun
			wantRun.Status, wantRun.Reason = agent.StatusRunnable, ""
			wantRun.Rev, wantRun.UpdatedAt = dueRun.Rev+1, now
			k.equalRun(wantRun, after)

			k.unchanged(beforeLater)
			k.unchanged(beforeNever)
			assert.Equal(k.t, agent.ApprovalPending, k.approval(notYet.ID).Status)
			assert.Equal(k.t, agent.ApprovalPending, k.approval(never.ID).Status)
		}},
		{"lapses every approval that is due, across runs", func(k *kit) {
			due := k.now().Add(time.Hour)
			var runs []agent.Run
			for range 3 {
				run, _ := k.parked(agentAlpha, &due)
				runs = append(runs, run)
			}
			k.clock.Advance(2 * time.Hour)

			assert.Equal(k.t, 3, expire(k, k.now()))

			for _, run := range runs {
				assert.Equal(k.t, agent.StatusRunnable, k.run(run.ID).Status)
				assert.Equal(k.t, agent.ApprovalExpired, k.approvals(run.ID)[0].Status)
			}
		}},
		{"two approvals of one run lapse under one new Rev", func(k *kit) {
			due := k.now().Add(time.Hour)
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolSend, sendInput), Call("call-2", toolSend, sendInput))
			k.ask(lease, 2, &due)
			k.ask(lease, 3, &due)
			k.park(lease, agent.ReasonApproval)
			run = k.run(run.ID)

			assert.Equal(k.t, 2, expire(k, due))

			after := k.run(run.ID)
			assert.Equal(k.t, run.Rev+1, after.Rev, "a call changes a run once")
			for _, approval := range k.approvals(run.ID) {
				assert.Equal(k.t, agent.ApprovalExpired, approval.Status)
				assert.Equal(k.t, after.Rev, approval.Rev)
			}
		}},
		{"leaves an approval that already has its answer", func(k *kit) {
			due := k.now().Add(time.Hour)
			run, pending := k.parked(agentAlpha, &due)
			k.decide(pending.ID, true)
			before := k.snapshot(run.ID)

			assert.Zero(k.t, expire(k, due.Add(time.Hour)))
			k.unchanged(before)
		}},
		{"leaves a run that is not waiting as it is, and under its lease", func(k *kit) {
			due := k.now().Add(time.Hour)
			run, lease := k.proposed(agentAlpha)
			k.ask(lease, 2, &due)
			run = k.run(run.ID)

			assert.Equal(k.t, 1, expire(k, due))

			want := run
			want.Rev, want.UpdatedAt = run.Rev+1, due
			k.equalRun(want, k.run(run.ID))
			assert.Equal(k.t, agent.ApprovalExpired, k.approvals(run.ID)[0].Status)
		}},
		{"called again, lapses nothing more", func(k *kit) {
			due := k.now().Add(time.Hour)
			run, _ := k.parked(agentAlpha, &due)
			require.Equal(k.t, 1, expire(k, due))
			before := k.snapshot(run.ID)

			assert.Zero(k.t, expire(k, due.Add(time.Hour)))
			k.unchanged(before)
		}},
	}
}

func listApprovalsCases() []storeCase {
	list := func(k *kit, f agent.ApprovalFilter) []string {
		k.t.Helper()
		approvals, err := k.store.ListApprovals(k.ctx, f)
		require.NoError(k.t, err)
		return approvalIDs(approvals)
	}
	return []storeCase{
		{"an empty store lists nothing", func(k *kit) {
			assert.Empty(k.t, list(k, agent.ApprovalFilter{}))
		}},
		{"lists every approval, oldest first", func(k *kit) {
			_, first := k.parked(agentAlpha, nil)
			_, second := k.parked(agentBeta, nil)
			_, third := k.parked(agentAlpha, nil)

			approvals, err := k.store.ListApprovals(k.ctx, agent.ApprovalFilter{})

			require.NoError(k.t, err)
			assert.Equal(k.t, []string{first.ID, second.ID, third.ID}, approvalIDs(approvals))
			k.equalApproval(first, approvals[0])
		}},
		{"oldest is by RequestedAt, not by the order they were recorded in", func(k *kit) {
			base := k.now()
			// Recorded in this order, each under the time it is given.
			var ids []string
			for _, after := range []time.Duration{3 * time.Second, time.Second, 2 * time.Second} {
				_, lease := k.proposed(agentAlpha)
				req := k.askRequest(2)
				req.Now = base.Add(after)
				_, err := k.store.RequestApproval(k.ctx, lease, req)
				require.NoError(k.t, err)
				ids = append(ids, req.ID)
			}

			assert.Equal(k.t, []string{ids[1], ids[2], ids[0]}, list(k, agent.ApprovalFilter{}))
		}},
		{"approvals requested at one instant are listed by id", func(k *kit) {
			_, earlier := k.parked(agentAlpha, nil)
			_, lease := k.held(agentAlpha)
			k.reply(lease,
				Call("call-1", toolSend, sendInput), Call("call-2", toolSend, sendInput),
				Call("call-3", toolSend, sendInput), Call("call-4", toolSend, sendInput))
			instant := k.tick()
			var same []string
			for seq := 2; seq <= 5; seq++ {
				req := k.askRequest(seq)
				req.Now = instant
				_, err := k.store.RequestApproval(k.ctx, lease, req)
				require.NoError(k.t, err)
				same = append(same, req.ID)
			}
			_, later := k.parked(agentAlpha, nil)
			slices.Sort(same)

			want := append(append([]string{earlier.ID}, same...), later.ID)
			assert.Equal(k.t, want, list(k, agent.ApprovalFilter{}))
		}},
		{"narrows by status and by run", func(k *kit) {
			pendingRun, pending := k.parked(agentAlpha, nil)
			_, approved := k.parked(agentAlpha, nil)
			k.decide(approved.ID, true)
			_, declined := k.parked(agentAlpha, nil)
			k.decide(declined.ID, false)
			due := k.now().Add(time.Hour)
			_, lapsed := k.parked(agentAlpha, &due)
			_, err := k.store.ExpireApprovals(k.ctx, due)
			require.NoError(k.t, err)
			cancelledRun, lease := k.proposed(agentAlpha)
			cancelled := k.ask(lease, 2, nil)
			k.finish(lease, agent.StatusCancelled)
			empty := k.create(agentAlpha)

			tests := []struct {
				name   string
				filter agent.ApprovalFilter
				want   []string
			}{
				{"pending", agent.ApprovalFilter{Status: agent.ApprovalPending}, []string{pending.ID}},
				{"approved", agent.ApprovalFilter{Status: agent.ApprovalApproved}, []string{approved.ID}},
				{"declined", agent.ApprovalFilter{Status: agent.ApprovalDeclined}, []string{declined.ID}},
				{"expired", agent.ApprovalFilter{Status: agent.ApprovalExpired}, []string{lapsed.ID}},
				{"cancelled", agent.ApprovalFilter{Status: agent.ApprovalCancelled}, []string{cancelled.ID}},
				{"one run", agent.ApprovalFilter{RunID: cancelledRun.ID}, []string{cancelled.ID}},
				{"a run and a status it has", agent.ApprovalFilter{RunID: pendingRun.ID, Status: agent.ApprovalPending}, []string{pending.ID}},
				{"a run and a status it has not", agent.ApprovalFilter{RunID: pendingRun.ID, Status: agent.ApprovalApproved}, []string{}},
				{"a run with no approvals", agent.ApprovalFilter{RunID: empty.ID}, []string{}},
			}
			for _, tt := range tests {
				assert.Equal(k.t, tt.want, list(k, tt.filter), tt.name)
			}
		}},
		{"returns 50 by default and never more than 200", func(k *kit) {
			const total = 205
			_, lease := k.held(agentAlpha)
			calls := make([]agent.Call, total)
			for i := range calls {
				calls[i] = Call(fmt.Sprintf("call-%d", i+1), toolSend, sendInput)
			}
			k.reply(lease, calls...)
			all := make([]string, total)
			for i := range total {
				all[i] = k.ask(lease, i+2, nil).ID
			}

			tests := []struct {
				name  string
				limit int
				want  int
			}{
				{"no limit given", 0, 50},
				{"a negative limit", -1, 50},
				{"a small limit", 3, 3},
				{"the most allowed", 200, 200},
				{"more than allowed", 1000, 200},
			}
			for _, tt := range tests {
				assert.Equal(k.t, all[:tt.want], list(k, agent.ApprovalFilter{Limit: tt.limit}), tt.name)
			}
		}},
	}
}
