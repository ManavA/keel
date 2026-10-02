package agenttest

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

func finishCases() []storeCase {
	return []storeCase{
		{"ends the run as given and releases the lease", func(k *kit) {
			tests := []struct {
				name string
				req  agent.FinishRequest
			}{
				{"completed", agent.FinishRequest{Status: agent.StatusCompleted, Output: "the answer"}},
				{"failed", agent.FinishRequest{Status: agent.StatusFailed, Reason: agent.ReasonError, Error: "model unavailable"}},
				{"failed, with what it had so far", agent.FinishRequest{
					Status: agent.StatusFailed, Reason: agent.ReasonCostBudget, Output: "partial", Error: "budget spent",
				}},
				{"cancelled", agent.FinishRequest{Status: agent.StatusCancelled, Reason: agent.ReasonCancelled}},
			}
			for _, tt := range tests {
				run, lease := k.held(agentAlpha)
				now := k.tick()
				tt.req.Now = now

				require.NoError(k.t, k.store.Finish(k.ctx, lease, tt.req), tt.name)

				want := run
				want.Status, want.Reason, want.Output, want.Error = tt.req.Status, tt.req.Reason, tt.req.Output, tt.req.Error
				want.FinishedAt = &now
				want.LeaseOwner, want.LeaseExpiresAt = "", nil
				want.Rev, want.UpdatedAt = run.Rev+1, now
				after := k.run(run.ID)
				k.equalRun(want, after)
				assert.True(k.t, after.Terminal(), tt.name)
				assert.Equal(k.t, lease.Epoch, after.LeaseEpoch, tt.name)
			}
		}},
		{"cancels the run's pending approvals and leaves the answered ones", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease,
				Call("call-1", toolSend, sendInput), Call("call-2", toolSend, sendInput), Call("call-3", toolSend, sendInput))
			approved := k.decide(k.ask(lease, 2, nil).ID, true)
			declined := k.decide(k.ask(lease, 3, nil).ID, false)
			pending := k.ask(lease, 4, nil)
			other, otherPending := k.parked(agentAlpha, nil)
			beforeOther := k.snapshot(other.ID)
			journal := k.steps(run.ID)

			k.finish(lease, agent.StatusFailed)

			after := k.run(run.ID)
			// Nobody answered it, so it has a status and no answer.
			want := pending
			want.Status, want.Rev = agent.ApprovalCancelled, after.Rev
			k.equalApproval(want, k.approval(pending.ID))
			k.equalApproval(approved, k.approval(approved.ID))
			k.equalApproval(declined, k.approval(declined.ID))
			k.equalSteps(journal, k.steps(run.ID))

			k.unchanged(beforeOther)
			assert.Equal(k.t, agent.ApprovalPending, k.approval(otherPending.ID).Status, "another run's question still stands")
		}},
		{"makes a waiting parent runnable", func(k *kit) {
			for _, status := range finalStatuses {
				parent, parentLease := k.held(agentAlpha)
				child := k.createChild(parent)
				k.park(parentLease, agent.ReasonChildren)
				parent = k.run(parent.ID)
				childLease := k.claim(workerB, child.ID)
				now := k.tick()

				require.NoError(k.t, k.store.Finish(k.ctx, childLease, agent.FinishRequest{Status: status, Now: now}))

				want := parent
				want.Status, want.Reason = agent.StatusRunnable, ""
				want.Rev, want.UpdatedAt = parent.Rev+1, now
				k.equalRun(want, k.run(parent.ID))
				lease := k.claim(workerA, parent.ID)
				assert.Equal(k.t, parent.LeaseEpoch+1, lease.Epoch, "the parent can be picked up again")
			}
		}},
		{"makes a parent runnable that waits on a person as well", func(k *kit) {
			parent, parentLease := k.proposed(agentAlpha)
			pending := k.ask(parentLease, 2, nil)
			child := k.createChild(parent)
			k.park(parentLease, agent.ReasonApproval)

			k.finish(k.claim(workerB, child.ID), agent.StatusCompleted)

			// Its next execution collects the child and parks again.
			assert.Equal(k.t, agent.StatusRunnable, k.run(parent.ID).Status)
			assert.Equal(k.t, agent.ApprovalPending, k.approval(pending.ID).Status)
		}},
		{"leaves a parent that is not waiting alone", func(k *kit) {
			tests := []struct {
				name   string
				parent func() agent.Run
			}{
				{"a parent being executed", func() agent.Run {
					parent, _ := k.held(agentAlpha)
					return parent
				}},
				{"a parent nobody holds", func() agent.Run { return k.create(agentAlpha) }},
				{"a parent that has ended", func() agent.Run { return k.ended(agentAlpha, agent.StatusCancelled) }},
			}
			for _, tt := range tests {
				parent := tt.parent()
				child := k.createChild(parent)
				childLease := k.claim(workerB, child.ID)
				before := k.snapshot(parent.ID)

				k.finish(childLease, agent.StatusCompleted)

				k.unchanged(before)
			}
		}},
		{"a run with no parent ends without touching another run", func(k *kit) {
			other, _ := k.parked(agentAlpha, nil)
			before := k.snapshot(other.ID)
			_, lease := k.held(agentAlpha)

			k.finish(lease, agent.StatusCompleted)

			k.unchanged(before)
		}},
		{"a status that is not final is refused", func(k *kit) {
			run, lease := k.held(agentAlpha)
			for _, status := range []agent.Status{agent.StatusRunnable, agent.StatusWaiting, ""} {
				before := k.snapshot(run.ID)

				err := k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: status, Output: "never recorded", Now: k.tick()})

				require.Error(k.t, err, "status %q", status)
				k.unchanged(before)
			}
			k.finish(lease, agent.StatusCompleted)
		}},
		{"once ended, the lease is spent and the run stays as it ended", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.Finish(k.ctx, lease, agent.FinishRequest{
				Status: agent.StatusCompleted, Output: "the answer", Now: k.tick(),
			}))
			before := k.snapshot(run.ID)

			err := k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: agent.StatusFailed, Error: "too late", Now: k.tick()})
			require.ErrorIs(k.t, err, agent.ErrLeaseLost)
			err = k.store.BeginModel(k.ctx, lease, 1, k.tick())
			require.ErrorIs(k.t, err, agent.ErrLeaseLost)
			err = k.store.Yield(k.ctx, lease, agent.YieldRequest{Now: k.tick()})
			require.ErrorIs(k.t, err, agent.ErrLeaseLost)
			k.unchanged(before)
		}},
	}
}

func requestCancelCases() []storeCase {
	return []storeCase{
		{"marks the run with who and why", func(k *kit) {
			tests := []struct {
				name string
				run  func() agent.Run
			}{
				{"a run being executed", func() agent.Run {
					run, _ := k.held(agentAlpha)
					return run
				}},
				{"a run nobody holds", func() agent.Run { return k.create(agentAlpha) }},
			}
			for _, tt := range tests {
				run := tt.run()
				now := k.tick()

				require.NoError(k.t, k.store.RequestCancel(k.ctx, agent.CancelRequest{
					RunID: run.ID, By: personA, Reason: "wrong batch", Now: now,
				}), tt.name)

				// The status and the lease are as they were: whoever holds
				// the run learns of the request from its next heartbeat.
				want := run
				want.CancelRequested, want.CancelBy, want.CancelReason = true, personA, "wrong batch"
				want.Rev, want.UpdatedAt = run.Rev+1, now
				k.equalRun(want, k.run(run.ID))
			}
		}},
		{"makes a waiting run runnable, and leaves its approvals for the execution that ends it", func(k *kit) {
			run, pending := k.parked(agentAlpha, nil)
			journal := k.steps(run.ID)
			now := k.tick()

			require.NoError(k.t, k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: run.ID, By: personA, Now: now}))

			want := run
			want.Status, want.Reason = agent.StatusRunnable, ""
			want.CancelRequested, want.CancelBy = true, personA
			want.Rev, want.UpdatedAt = run.Rev+1, now
			k.equalRun(want, k.run(run.ID))
			k.equalApproval(pending, k.approval(pending.ID))
			k.equalSteps(journal, k.steps(run.ID))

			lease := k.claim(workerB, run.ID)
			assert.Equal(k.t, run.LeaseEpoch+1, lease.Epoch, "the run can be picked up, to be ended")
		}},
		{"a run that has ended is ErrFinished", func(k *kit) {
			for _, status := range finalStatuses {
				run := k.ended(agentAlpha, status)
				before := k.snapshot(run.ID)

				err := k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: run.ID, By: personA, Reason: "too late", Now: k.tick()})

				require.ErrorIs(k.t, err, agent.ErrFinished, status)
				k.unchanged(before)
			}
		}},
	}
}
