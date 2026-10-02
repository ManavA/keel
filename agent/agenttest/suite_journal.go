package agenttest

import (
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

func stepsCases() []storeCase {
	return []storeCase{
		{"a run with no journal has no steps", func(k *kit) {
			run := k.create(agentAlpha)
			assert.Empty(k.t, k.steps(run.ID))
		}},
		{"returns the journal in order, from 1 with no gaps", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolLookup, lookupInput), Call("call-2", toolSend, sendInput))
			k.update(lease, 3, agent.StepProposed, agent.StepBlocked)
			k.update(lease, 2, agent.StepProposed, agent.StepCompleted)
			k.reply(lease, Call("call-3", toolLookup, lookupInput))
			k.update(lease, 5, agent.StepProposed, agent.StepCompleted)
			k.reply(lease)

			steps := k.steps(run.ID)

			assert.Equal(k.t, []int{1, 2, 3, 4, 5, 6}, stepSeqs(steps))
			kinds := make([]agent.StepKind, len(steps))
			for i, s := range steps {
				kinds[i] = s.Kind
				assert.Equal(k.t, run.ID, s.RunID)
			}
			assert.Equal(k.t, []agent.StepKind{
				agent.StepModel, agent.StepTool, agent.StepTool, agent.StepModel, agent.StepTool, agent.StepModel,
			}, kinds)
		}},
		{"one run's journal holds nothing of another's", func(k *kit) {
			first, firstLease := k.held(agentAlpha)
			second, secondLease := k.held(agentAlpha)
			k.reply(firstLease, Call("call-1", toolLookup, lookupInput))
			k.reply(secondLease)

			assert.Len(k.t, k.steps(first.ID), 2)
			assert.Len(k.t, k.steps(second.ID), 1)
		}},
	}
}

func beginModelCases() []storeCase {
	return []storeCase{
		{"starts the first step of an empty journal", func(k *kit) {
			run, lease := k.held(agentAlpha)
			now := k.tick()

			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, now))

			after := k.run(run.ID)
			k.equalSteps([]agent.Step{{
				RunID: run.ID, Seq: 1, Kind: agent.StepModel, Status: agent.StepStarted,
				Attempts: 1, Rev: after.Rev, CreatedAt: now, StartedAt: &now,
			}}, k.steps(run.ID))
			want := run
			want.Rev, want.UpdatedAt = run.Rev+1, now
			k.equalRun(want, after)
		}},
		{"called again for a step still started, counts another attempt", func(k *kit) {
			run, lease := k.held(agentAlpha)
			first := k.tick()
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, first))
			k.clock.Advance(time.Minute)
			second := k.now()

			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, second))

			after := k.run(run.ID)
			k.equalSteps([]agent.Step{{
				RunID: run.ID, Seq: 1, Kind: agent.StepModel, Status: agent.StepStarted,
				Attempts: 2, Rev: after.Rev, CreatedAt: first, StartedAt: &second,
			}}, k.steps(run.ID))
			assert.Equal(k.t, run.Rev+2, after.Rev)

			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			assert.Equal(k.t, 3, k.step(run.ID, 1).Attempts)
		}},
		{"starts the step one past the last", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolLookup, lookupInput), Call("call-2", toolSend, sendInput))
			now := k.tick()

			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 4, now))

			step := k.step(run.ID, 4)
			assert.Equal(k.t, agent.StepModel, step.Kind)
			assert.Equal(k.t, agent.StepStarted, step.Status)
			assert.Equal(k.t, 1, step.Attempts)
			assert.Equal(k.t, k.run(run.ID).Rev, step.Rev)
			assert.Len(k.t, k.steps(run.ID), 4)
		}},
		{"a wrong seq is ErrConflict", func(k *kit) {
			empty, emptyLease := k.held(agentAlpha)
			// A journal of a completed reply, its one call, and a reply
			// under way.
			journal, journalLease := k.proposed(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, journalLease, 3, k.tick()))
			// A journal whose one call has started: started is the status a
			// model step is begun again in, and this is not a model step.
			running, runningLease := k.proposed(agentAlpha)
			k.update(runningLease, 2, agent.StepProposed, agent.StepStarted)

			tests := []struct {
				name  string
				run   agent.Run
				lease agent.Lease
				seq   int
			}{
				{"zero", empty, emptyLease, 0},
				{"a negative seq", empty, emptyLease, -1},
				{"a gap after an empty journal", empty, emptyLease, 2},
				{"a model step already completed", journal, journalLease, 1},
				{"a tool step", journal, journalLease, 2},
				{"a tool step that has started", running, runningLease, 2},
				{"a gap after the last step", journal, journalLease, 5},
			}
			for _, tt := range tests {
				before := k.snapshot(tt.run.ID)

				err := k.store.BeginModel(k.ctx, tt.lease, tt.seq, k.tick())

				require.ErrorIs(k.t, err, agent.ErrConflict, tt.name)
				k.unchanged(before)
			}
		}},
	}
}

func completeModelCases() []storeCase {
	usage := agent.Usage{InputTokens: 120, OutputTokens: 30, CostMicros: 450}
	return []storeCase{
		{"records the reply on the step and its cost on the run", func(k *kit) {
			run, lease := k.held(agentAlpha)
			started := k.tick()
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, started))
			k.clock.Advance(1500 * time.Millisecond)
			finished := k.now()
			message := agent.Message{Role: agent.RoleAssistant, Text: "all done"}

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: message, Stop: agent.StopEnd, Model: suiteModel, Usage: usage, Now: finished,
			}))

			after := k.run(run.ID)
			k.equalSteps([]agent.Step{{
				RunID: run.ID, Seq: 1, Kind: agent.StepModel, Status: agent.StepCompleted, Name: suiteModel,
				Message: &message, Stop: agent.StopEnd,
				Attempts: 1, Usage: usage, Rev: after.Rev,
				CreatedAt: started, StartedAt: &started, FinishedAt: &finished,
			}}, k.steps(run.ID))
			want := run
			want.Usage, want.ModelCalls, want.ActiveMillis = usage, 1, 1500
			want.Rev, want.UpdatedAt = run.Rev+2, finished
			k.equalRun(want, after)
		}},
		{"appends one proposed tool step for each call, in the order written", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			calls := []agent.Call{
				Call("call-1", toolLookup, lookupInput),
				Call("call-2", toolSend, sendInput),
				Call("call-3", toolLookup, `{"id":`),
			}
			require.True(k.t, calls[2].Malformed)
			now := k.tick()

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Use(calls...).Message, Stop: agent.StopToolUse, Model: suiteModel, Now: now,
			}))

			after := k.run(run.ID)
			steps := k.steps(run.ID)
			require.Len(k.t, steps, 4)
			assert.Equal(k.t, after.Rev, steps[0].Rev)
			want := make([]agent.Step, len(calls))
			for i, call := range calls {
				want[i] = agent.Step{
					RunID: run.ID, Seq: i + 2, Kind: agent.StepTool, Status: agent.StepProposed, Name: call.Name,
					Turn: 1, Call: &call, Key: agent.StepKey(run.ID, i+2),
					Rev: after.Rev, CreatedAt: now,
				}
			}
			k.equalSteps(want, steps[1:])
		}},
		{"a reply that makes no calls appends nothing", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease)

			assert.Len(k.t, k.steps(run.ID), 1)
		}},
		{"the calls of a later reply follow it, each under its own turn and key", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.reply(lease, Call("call-1", toolLookup, lookupInput))
			k.update(lease, 2, agent.StepProposed, agent.StepCompleted)
			turn := k.reply(lease, Call("call-2", toolSend, sendInput), Call("call-3", toolSend, sendInput))

			require.Equal(k.t, 3, turn)
			for _, seq := range []int{4, 5} {
				step := k.step(run.ID, seq)
				assert.Equal(k.t, agent.StepTool, step.Kind)
				assert.Equal(k.t, 3, step.Turn)
				assert.Equal(k.t, agent.StepKey(run.ID, seq), step.Key)
			}
			assert.Equal(k.t, 1, k.step(run.ID, 2).Turn, "the first reply's call is as it was")
		}},
		{"adds to the totals the run already has", func(k *kit) {
			run, lease := k.held(agentAlpha)
			for range 2 {
				seq := len(k.steps(run.ID)) + 1
				require.NoError(k.t, k.store.BeginModel(k.ctx, lease, seq, k.tick()))
				k.clock.Advance(250 * time.Millisecond)
				require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
					Seq: seq, Message: Say("thinking").Message, Stop: agent.StopPause, Model: suiteModel, Usage: usage, Now: k.now(),
				}))
			}

			after := k.run(run.ID)
			assert.Equal(k.t, usage.Add(usage), after.Usage)
			assert.Equal(k.t, 2, after.ModelCalls)
			assert.Equal(k.t, int64(500), after.ActiveMillis)
			assert.Equal(k.t, usage, k.step(run.ID, 2).Usage, "a step carries its own cost")
		}},
		{"measures the active time from the attempt that was answered", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			k.clock.Advance(10 * time.Second)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.now()))
			k.clock.Advance(2 * time.Second)

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Say("done").Message, Stop: agent.StopEnd, Model: suiteModel, Now: k.now(),
			}))

			assert.Equal(k.t, int64(2000), k.run(run.ID).ActiveMillis)
			assert.Equal(k.t, 2, k.step(run.ID, 1).Attempts)
			assert.Equal(k.t, 1, k.run(run.ID).ModelCalls, "a call lost to an interruption is on no record")
		}},
		{"resets Failures", func(k *kit) {
			run, lease := k.held(agentAlpha)
			lease = k.failOnce(lease)

			k.reply(lease)

			assert.Zero(k.t, k.run(run.ID).Failures)
		}},
		{"keeps the message as it was given, its provider's form included", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			message := agent.Message{
				Role:  agent.RoleAssistant,
				Text:  "reading it now",
				Calls: []agent.Call{Call("call-1", toolLookup, lookupInput)},
				// Keys out of alphabetical order: a provider is sent this
				// back and may compare it with what it wrote.
				Opaque: &agent.Opaque{Provider: "provider-a", Data: raw(`{"z":1,"a":[2,3],"m":{"y":true,"b":null}}`)},
			}

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: message, Stop: agent.StopToolUse, Model: suiteModel, Now: k.tick(),
			}))

			step := k.step(run.ID, 1)
			require.NotNil(k.t, step.Message)
			assert.Equal(k.t, normalMessage(message), normalMessage(*step.Message))
		}},
		{"a wrong seq is ErrConflict", func(k *kit) {
			empty, emptyLease := k.held(agentAlpha)
			// A journal of a completed reply and its one call: the totals
			// are on the run once.
			journal, journalLease := k.proposed(agentAlpha)
			// A journal whose one call has started: started is the status a
			// reply is recorded in, and this is not a model step.
			running, runningLease := k.proposed(agentAlpha)
			k.update(runningLease, 2, agent.StepProposed, agent.StepStarted)

			tests := []struct {
				name  string
				run   agent.Run
				lease agent.Lease
				seq   int
			}{
				{"a step that was never begun", empty, emptyLease, 1},
				{"zero", empty, emptyLease, 0},
				{"a model step already completed", journal, journalLease, 1},
				{"a tool step", journal, journalLease, 2},
				{"a tool step that has started", running, runningLease, 2},
				{"past the last step", journal, journalLease, 3},
			}
			for _, tt := range tests {
				before := k.snapshot(tt.run.ID)

				err := k.store.CompleteModel(k.ctx, tt.lease, agent.CompleteModelRequest{
					Seq:     tt.seq,
					Message: Use(Call("call-9", toolSend, sendInput)).Message,
					Stop:    agent.StopToolUse,
					Model:   suiteModel,
					Usage:   usage,
					Now:     k.tick(),
				})

				require.ErrorIs(k.t, err, agent.ErrConflict, tt.name)
				k.unchanged(before)
			}
		}},
	}
}

func updateStepCases() []storeCase {
	result := func(s string) *string { return &s }
	return []storeCase{
		{"to started, counts an attempt and sets StartedAt", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			proposed := k.step(run.ID, 2)
			now := k.tick()

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepStarted, Decision: agent.Allow, Rule: suiteRule, Now: now,
			}))

			after := k.run(run.ID)
			want := proposed
			want.Status, want.Decision, want.Rule = agent.StepStarted, agent.Allow, suiteRule
			want.Attempts, want.StartedAt, want.Rev = 1, &now, after.Rev
			k.equalSteps([]agent.Step{want}, k.steps(run.ID)[1:])
			wantRun := run
			wantRun.Rev, wantRun.UpdatedAt = run.Rev+1, now
			k.equalRun(wantRun, after)
		}},
		{"to started again, counts another attempt and resets StartedAt", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			k.clock.Advance(time.Minute)
			now := k.now()

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepStarted, Now: now,
			}))

			step := k.step(run.ID, 2)
			assert.Equal(k.t, 2, step.Attempts)
			k.timeIs(now, step.StartedAt, "StartedAt")
			assert.Nil(k.t, step.FinishedAt)
		}},
		{"to started from waiting, for a call a person approved, counts the attempt", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.ask(lease, 2, nil)

			k.update(lease, 2, agent.StepWaiting, agent.StepStarted)

			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepStarted, step.Status)
			assert.Equal(k.t, 1, step.Attempts)
		}},
		{"to completed from started, records the result and adds the active time", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			started := k.step(run.ID, 2)
			activeBefore := k.run(run.ID).ActiveMillis
			k.clock.Advance(2500 * time.Millisecond)
			now := k.now()

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Result: result("sent"), Now: now,
			}))

			after := k.run(run.ID)
			want := started
			want.Status, want.Result, want.FinishedAt, want.Rev = agent.StepCompleted, "sent", &now, after.Rev
			k.equalSteps([]agent.Step{want}, k.steps(run.ID)[1:])
			assert.Equal(k.t, activeBefore+2500, after.ActiveMillis)
		}},
		{"a final status from anything but started adds no active time", func(k *kit) {
			tests := []struct {
				name string
				to   agent.StepStatus
				ask  bool
			}{
				{"blocked by the guard", agent.StepBlocked, false},
				{"completed without running", agent.StepCompleted, false},
				{"declined by a person", agent.StepDeclined, true},
			}
			for _, tt := range tests {
				run, lease := k.proposed(agentAlpha)
				from := agent.StepProposed
				if tt.ask {
					k.ask(lease, 2, nil)
					from = agent.StepWaiting
				}
				activeBefore := k.run(run.ID).ActiveMillis
				k.clock.Advance(time.Minute)
				now := k.now()

				require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
					Seq: 2, From: from, To: tt.to, Result: result("blocked by policy: " + suiteRule), IsError: true, Now: now,
				}), tt.name)

				step := k.step(run.ID, 2)
				assert.Equal(k.t, tt.to, step.Status, tt.name)
				assert.Equal(k.t, "blocked by policy: "+suiteRule, step.Result, tt.name)
				assert.True(k.t, step.IsError, tt.name)
				k.timeIs(now, step.FinishedAt, tt.name)
				assert.Nil(k.t, step.StartedAt, tt.name)
				assert.Zero(k.t, step.Attempts, tt.name)
				assert.Equal(k.t, activeBefore, k.run(run.ID).ActiveMillis, tt.name)
			}
		}},
		{"time spent waiting on a child or a person is not time working", func(k *kit) {
			waits := []struct {
				name string
				wait func(k *kit, run agent.Run, lease agent.Lease)
			}{
				{"on a child run", func(k *kit, run agent.Run, lease agent.Lease) {
					child := k.createChild(run)
					require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
						Seq: 2, From: agent.StepStarted, To: agent.StepWaiting, ChildRunID: child.ID, Now: k.tick(),
					}))
				}},
				{"on a person, about an interrupted call", func(k *kit, _ agent.Run, lease agent.Lease) {
					req := k.askRequest(2)
					req.From, req.Cause, req.Rule = agent.StepStarted, agent.CauseInterrupted, agent.RuleInterrupted
					_, err := k.store.RequestApproval(k.ctx, lease, req)
					require.NoError(k.t, err)
				}},
			}
			for _, tt := range waits {
				for _, to := range []agent.StepStatus{agent.StepCompleted, agent.StepDeclined} {
					run, lease := k.proposed(agentAlpha)
					// The step has a StartedAt, and it is an hour old by the
					// time the step is final.
					k.update(lease, 2, agent.StepProposed, agent.StepStarted)
					tt.wait(k, run, lease)
					activeBefore := k.run(run.ID).ActiveMillis
					k.clock.Advance(time.Hour)

					k.update(lease, 2, agent.StepWaiting, to)

					assert.Equal(k.t, activeBefore, k.run(run.ID).ActiveMillis, "%s, then %s", tt.name, to)
				}
			}
		}},
		{"a final status resets Failures", func(k *kit) {
			for _, to := range []agent.StepStatus{agent.StepCompleted, agent.StepBlocked, agent.StepDeclined} {
				run, lease := k.proposed(agentAlpha)
				lease = k.failOnce(lease)

				k.update(lease, 2, agent.StepProposed, to)

				assert.Zero(k.t, k.run(run.ID).Failures, to)
			}
		}},
		{"a status that is not final leaves Failures and sets no FinishedAt", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			lease = k.failOnce(lease)

			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			assert.Equal(k.t, 1, k.run(run.ID).Failures)

			k.update(lease, 2, agent.StepStarted, agent.StepWaiting)
			assert.Equal(k.t, 1, k.run(run.ID).Failures)
			assert.Nil(k.t, k.step(run.ID, 2).FinishedAt)
		}},
		{"records the child run a delegating call started", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			child := k.createChild(run)
			activeBefore := k.run(run.ID).ActiveMillis
			k.clock.Advance(time.Second)

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepWaiting, ChildRunID: child.ID, Now: k.now(),
			}))

			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepWaiting, step.Status)
			assert.Equal(k.t, child.ID, step.ChildRunID)
			assert.Equal(k.t, 1, step.Attempts)
			assert.Equal(k.t, activeBefore, k.run(run.ID).ActiveMillis, "waiting on a child is not time working")
		}},
		{"adds usage to the step and to the run", func(k *kit) {
			run, lease := k.held(agentAlpha)
			seq := len(k.steps(run.ID)) + 1
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, seq, k.tick()))
			modelUsage := agent.Usage{InputTokens: 100, OutputTokens: 20, CostMicros: 300}
			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq:     seq,
				Message: Use(Call("call-1", toolSend, sendInput)).Message,
				Stop:    agent.StopToolUse,
				Model:   suiteModel,
				Usage:   modelUsage,
				Now:     k.tick(),
			}))
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			first := agent.Usage{InputTokens: 10, OutputTokens: 5, CostMicros: 77}
			second := agent.Usage{InputTokens: 1, OutputTokens: 2, CostMicros: 3}

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepWaiting, Usage: first, Now: k.tick(),
			}))
			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepWaiting, To: agent.StepCompleted, Result: result("child output"), Usage: second, Now: k.tick(),
			}))

			assert.Equal(k.t, first.Add(second), k.step(run.ID, 2).Usage)
			assert.Equal(k.t, modelUsage.Add(first).Add(second), k.run(run.ID).Usage)
			assert.Equal(k.t, modelUsage, k.step(run.ID, 1).Usage, "the reply's own cost is as it was")
			assert.Equal(k.t, 1, k.run(run.ID).ModelCalls)
		}},
		{"records only what the request carries", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			child := k.createChild(run)

			// IsError with no Result is not a result.
			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepStarted,
				Decision: agent.Allow, Rule: suiteRule, IsError: true, Now: k.tick(),
			}))
			step := k.step(run.ID, 2)
			assert.False(k.t, step.IsError)
			assert.Empty(k.t, step.Result)
			assert.Empty(k.t, step.ChildRunID)

			// Rule with no Decision is not a decision.
			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepWaiting, Rule: "another rule", ChildRunID: child.ID, Now: k.tick(),
			}))
			step = k.step(run.ID, 2)
			assert.Equal(k.t, agent.Allow, step.Decision)
			assert.Equal(k.t, suiteRule, step.Rule)

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepWaiting, To: agent.StepCompleted, Result: result("beta failed: error"), IsError: true, Now: k.tick(),
			}))
			step = k.step(run.ID, 2)
			assert.Equal(k.t, agent.Allow, step.Decision)
			assert.Equal(k.t, suiteRule, step.Rule)
			assert.Equal(k.t, child.ID, step.ChildRunID)
			assert.Equal(k.t, "beta failed: error", step.Result)
			assert.True(k.t, step.IsError)
			assert.Equal(k.t, sendInput, string(step.Call.Input), "the call is as the model wrote it")
		}},
		{"an empty result is still a result", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Result: result(""), IsError: true, Now: k.tick(),
			}))

			step := k.step(run.ID, 2)
			assert.Empty(k.t, step.Result)
			assert.True(k.t, step.IsError)
		}},
		{"a step that is not in From is ErrConflict", func(k *kit) {
			run, lease := k.proposed(agentAlpha)

			tests := []struct {
				name string
				seq  int
				from agent.StepStatus
			}{
				{"a proposed step, moved from started", 2, agent.StepStarted},
				{"a proposed step, moved from no status", 2, ""},
				{"a model step, named by its own status", 1, agent.StepCompleted},
				{"no such step", 3, agent.StepProposed},
				{"zero", 0, agent.StepProposed},
			}
			for _, tt := range tests {
				before := k.snapshot(run.ID)

				err := k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
					Seq: tt.seq, From: tt.from, To: agent.StepCompleted,
					Result: result("never recorded"), Usage: agent.Usage{CostMicros: 99}, Now: k.tick(),
				})

				require.ErrorIs(k.t, err, agent.ErrConflict, tt.name)
				k.unchanged(before)
			}
		}},
		{"a move made twice is refused the second time", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)
			complete := agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted,
				Result: result("sent"), Usage: agent.Usage{CostMicros: 5}, Now: k.tick(),
			}
			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, complete))
			before := k.snapshot(run.ID)

			complete.Now = k.tick()
			err := k.store.UpdateStep(k.ctx, lease, complete)

			require.ErrorIs(k.t, err, agent.ErrConflict, "a step is recorded once")
			k.unchanged(before)
		}},
		{"checks nothing about the move but From", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepBlocked)

			// No rule of the engine makes this move. Which moves are legal
			// is the engine's business.
			k.update(lease, 2, agent.StepBlocked, agent.StepProposed)

			assert.Equal(k.t, agent.StepProposed, k.step(run.ID, 2).Status)
		}},
	}
}
