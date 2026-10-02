package agenttest

import (
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

// What a store does with a string no database column can hold: one with a
// NUL character in it, or a byte that is not UTF-8. A model, a tool or a
// person can put either in anything they write.
//
// A string a store only records is kept, with each such character as the
// replacement character, so that no journal write fails for what a model or
// a tool wrote. A string a store compares names something, and is refused,
// since it could only be kept as another name. Inside JSON a NUL is kept, as
// JSON has an escape for it.
const (
	// unkeepable holds a NUL and a byte that is not UTF-8.
	unkeepable = "a\x00b\xff"
	// keptAs is what unkeepable reads back as where it is only recorded.
	keptAs = "a�b�"
	// keptInJSON is what unkeepable reads back as inside a message or a
	// definition.
	keptInJSON = "a\x00b�"
)

// unkeepables are a string with each of the two characters on its own.
var unkeepables = []string{"a\x00b", "caf\xff"}

func stringsCases() []storeCase {
	result := func(s string) *string { return &s }
	return []storeCase{
		{"keeps what a run is stored with: its input, reason, output, error, and who cancelled it and why", func(k *kit) {
			run := k.newRun(agentAlpha)
			run.Input, run.Reason, run.Output, run.Error = unkeepable, unkeepable, unkeepable, unkeepable
			run.CancelBy, run.CancelReason = unkeepable, unkeepable

			stored, _, err := k.store.CreateRun(k.ctx, run)

			require.NoError(k.t, err)
			want := run
			want.Rev, want.Metadata = 1, map[string]string{}
			want.Input, want.Reason, want.Output, want.Error = keptAs, keptAs, keptAs, keptAs
			want.CancelBy, want.CancelReason = keptAs, keptAs
			k.equalRun(want, stored)
			k.equalRun(want, k.run(run.ID))
		}},
		{"keeps metadata, its keys and its values", func(k *kit) {
			run := k.newRun(agentAlpha)
			run.Metadata = map[string]string{unkeepable: unkeepable, "plain": "kept"}
			want := map[string]string{keptAs: keptAs, "plain": "kept"}

			stored, _, err := k.store.CreateRun(k.ctx, run)

			require.NoError(k.t, err)
			assert.Equal(k.t, want, stored.Metadata, "as returned")
			assert.Equal(k.t, want, k.run(run.ID).Metadata, "as read")
		}},
		{"keeps the definition: a NUL as it is, and a byte that is not UTF-8 as the replacement character", func(k *kit) {
			run := k.newRun(agentAlpha)
			run.Definition = agent.Snapshot{
				System: unkeepable,
				Model:  unkeepable,
				Tools:  []agent.ToolSpec{{Name: unkeepable, Description: unkeepable, Schema: raw(`{"default":"a\u0000b"}`)}},
				Output: raw(`{"const":"a\u0000b"}`),
			}

			stored, _, err := k.store.CreateRun(k.ctx, run)

			require.NoError(k.t, err)
			want := run.Definition
			want.System, want.Model = keptInJSON, keptInJSON
			want.Tools = []agent.ToolSpec{{Name: keptInJSON, Description: keptInJSON, Schema: raw(`{"default":"a\u0000b"}`)}}
			assert.Equal(k.t, want, stored.Definition, "as returned")
			assert.Equal(k.t, want, k.run(run.ID).Definition, "as read")
		}},
		{"keeps the error of an execution that failed", func(k *kit) {
			run, lease := k.held(agentAlpha)

			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Failed: true, Error: unkeepable, Now: k.tick()}))

			assert.Equal(k.t, keptAs, k.run(run.ID).Error)
		}},
		{"keeps the reason a run waits", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.ask(lease, 2, nil)

			parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: unkeepable, Now: k.tick()})

			require.NoError(k.t, err)
			require.True(k.t, parked)
			assert.Equal(k.t, keptAs, k.run(run.ID).Reason)
		}},
		{"keeps how a run ended: its reason, its output and its error", func(k *kit) {
			run, lease := k.held(agentAlpha)

			require.NoError(k.t, k.store.Finish(k.ctx, lease, agent.FinishRequest{
				Status: agent.StatusFailed, Reason: unkeepable, Output: unkeepable, Error: unkeepable, Now: k.tick(),
			}))

			after := k.run(run.ID)
			assert.Equal(k.t, agent.StatusFailed, after.Status)
			assert.Equal(k.t, keptAs, after.Reason)
			assert.Equal(k.t, keptAs, after.Output, "the model's last words are not what stops a run from ending")
			assert.Equal(k.t, keptAs, after.Error)
		}},
		{"keeps who asked for a run to be cancelled, and why", func(k *kit) {
			run := k.create(agentAlpha)

			require.NoError(k.t, k.store.RequestCancel(k.ctx, agent.CancelRequest{
				RunID: run.ID, By: unkeepable, Reason: unkeepable, Now: k.tick(),
			}))

			after := k.run(run.ID)
			assert.True(k.t, after.CancelRequested)
			assert.Equal(k.t, keptAs, after.CancelBy)
			assert.Equal(k.t, keptAs, after.CancelReason)
		}},
		{"keeps a reply: the model's name and why it stopped, and inside the message a NUL as it is", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			message := agent.Message{
				Role:    agent.RoleAssistant,
				Text:    unkeepable,
				Results: []agent.Result{{CallID: unkeepable, Content: unkeepable}},
				Opaque:  &agent.Opaque{Provider: unkeepable, Data: raw(`{"sig":"a\u0000b"}`)},
			}

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: message, Stop: agent.Stop(unkeepable), Model: unkeepable, Now: k.tick(),
			}))

			step := k.step(run.ID, 1)
			assert.Equal(k.t, agent.StepCompleted, step.Status)
			assert.Equal(k.t, keptAs, step.Name)
			assert.Equal(k.t, agent.Stop(keptAs), step.Stop)
			require.NotNil(k.t, step.Message)
			want := agent.Message{
				Role:    agent.RoleAssistant,
				Text:    keptInJSON,
				Results: []agent.Result{{CallID: keptInJSON, Content: keptInJSON}},
				Opaque:  &agent.Opaque{Provider: keptInJSON, Data: raw(`{"sig":"a\u0000b"}`)},
			}
			assert.Equal(k.t, normalMessage(want), normalMessage(*step.Message))
		}},
		{"keeps a call to a tool whose name cannot be kept: the name on the step and on its approval", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			call := agent.Call{ID: unkeepable, Name: unkeepable, Input: raw(`{"to":"a\u0000b"}`)}

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Use(call).Message, Stop: agent.StopToolUse, Model: suiteModel, Now: k.tick(),
			}))

			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepProposed, step.Status, "the model's turn was journaled, and the call can be answered")
			assert.Equal(k.t, keptAs, step.Name)
			// The call is JSON: its NUL is kept, in its strings and in its
			// arguments.
			want := agent.Call{ID: keptInJSON, Name: keptInJSON, Input: raw(`{"to":"a\u0000b"}`)}
			require.NotNil(k.t, step.Call)
			assert.Equal(k.t, want, *step.Call)
			assert.Equal(k.t, []agent.Call{want}, k.step(run.ID, 1).Message.Calls)

			approval := k.ask(lease, 2, nil)
			assert.Equal(k.t, keptAs, approval.Tool)
			assert.Equal(k.t, `{"to":"a\u0000b"}`, string(approval.Input))
			assert.Equal(k.t, keptAs, k.approval(approval.ID).Tool)
		}},
		{"keeps what a tool returned, and the decision and rule on its step", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted,
				Decision: agent.Effect(unkeepable), Rule: unkeepable, Result: result(unkeepable), Now: k.tick(),
			}))

			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepCompleted, step.Status, "the result was journaled, so the call is never made again")
			assert.Equal(k.t, keptAs, step.Result)
			assert.Equal(k.t, agent.Effect(keptAs), step.Decision)
			assert.Equal(k.t, keptAs, step.Rule)
		}},
		{"keeps a question: its rule, and the decision and rule it puts on the step", func(k *kit) {
			run, lease := k.proposed(agentAlpha)
			req := k.askRequest(2)
			req.Decision, req.Rule = agent.Effect(unkeepable), unkeepable

			got, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err)
			assert.Equal(k.t, keptAs, got.Rule, "as returned")
			assert.Equal(k.t, keptAs, k.approval(req.ID).Rule, "as read")
			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepWaiting, step.Status)
			assert.Equal(k.t, agent.Effect(keptAs), step.Decision)
			assert.Equal(k.t, keptAs, step.Rule)
		}},
		{"keeps an action: its kind, its target, and the keys and values of its attributes", func(k *kit) {
			_, lease := k.proposed(agentAlpha)
			req := k.askRequest(2)
			req.Action = agent.Action{Kind: unkeepable, Target: unkeepable, Attrs: map[string]any{
				unkeepable: unkeepable,
				"nested":   map[string]any{unkeepable: []any{unkeepable, 7, nil}},
				// The six characters of JSON's escape for a NUL, which are
				// not a NUL.
				"written": `\u0000`,
			}}
			want := agent.Action{Kind: keptAs, Target: keptAs, Attrs: map[string]any{
				keptAs:    keptAs,
				"nested":  map[string]any{keptAs: []any{keptAs, float64(7), nil}},
				"written": `\u0000`,
			}}

			got, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err)
			assert.Equal(k.t, want, got.Action, "as returned")
			assert.Equal(k.t, want, k.approval(req.ID).Action, "as read")
		}},
		{"keeps who answered an approval, and why", func(k *kit) {
			_, pending := k.parked(agentAlpha, nil)

			got, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{
				ID: pending.ID, Approved: true, By: unkeepable, Reason: unkeepable, Now: k.tick(),
			})

			require.NoError(k.t, err)
			assert.Equal(k.t, agent.ApprovalApproved, got.Status)
			assert.Equal(k.t, keptAs, got.DecidedBy, "as returned")
			assert.Equal(k.t, keptAs, got.Reason, "as returned")
			read := k.approval(pending.ID)
			assert.Equal(k.t, keptAs, read.DecidedBy, "as read")
			assert.Equal(k.t, keptAs, read.Reason, "as read")
		}},
		{"refuses a run whose agent, start key or owner cannot be kept", func(k *kit) {
			edits := []struct {
				name string
				edit func(run *agent.Run, bad string)
			}{
				{"its agent", func(run *agent.Run, bad string) { run.Agent = bad }},
				{"its start key", func(run *agent.Run, bad string) { run.Key = bad }},
				{"the owner it is stored with", func(run *agent.Run, bad string) { run.LeaseOwner = bad }},
			}
			for _, bad := range unkeepables {
				for _, e := range edits {
					run := k.newRun(agentAlpha)
					e.edit(&run, bad)

					_, created, err := k.store.CreateRun(k.ctx, run)

					require.Error(k.t, err, "%s, given %q", e.name, bad)
					assert.False(k.t, created, "%s, given %q", e.name, bad)
					_, err = k.store.GetRun(k.ctx, run.ID)
					assert.ErrorIs(k.t, err, agent.ErrNotFound, "%s, given %q: nothing was stored", e.name, bad)
				}
			}
		}},
		{"refuses a claim under an owner that cannot be kept", func(k *kit) {
			run := k.create(agentAlpha)
			before := k.snapshot(run.ID)

			for _, bad := range unkeepables {
				got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: bad, Agents: suiteAgents, Now: k.tick(), TTL: suiteTTL})
				require.Error(k.t, err, "owner %q", bad)
				assert.Nil(k.t, got)

				got, err = k.store.Claim(k.ctx, agent.ClaimRequest{
					Owner: bad, Agents: suiteAgents, RunID: run.ID, Now: k.tick(), TTL: suiteTTL,
				})
				require.Error(k.t, err, "owner %q, by id", bad)
				assert.NotErrorIs(k.t, err, agent.ErrNotClaimable, "the claim is refused, not the run")
				assert.Nil(k.t, got)
			}
			k.unchanged(before)
		}},
		{"a lease under an owner that cannot be kept is no run's", func(k *kit) {
			run, lease := k.held(agentAlpha)
			before := k.snapshot(run.ID)

			for _, bad := range unkeepables {
				err := k.store.BeginModel(k.ctx, agent.Lease{RunID: run.ID, Owner: bad, Epoch: lease.Epoch}, 1, k.tick())
				require.ErrorIs(k.t, err, agent.ErrLeaseLost, "owner %q", bad)
			}
			k.unchanged(before)
		}},
		{"an agent whose name cannot be kept has no runs to claim or to list", func(k *kit) {
			run := k.create(agentAlpha)
			before := k.snapshot(run.ID)

			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: unkeepables, Now: k.tick(), TTL: suiteTTL})
			require.NoError(k.t, err)
			assert.Nil(k.t, got)
			got, err = k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: unkeepables, RunID: run.ID, Now: k.tick(), TTL: suiteTTL,
			})
			require.ErrorIs(k.t, err, agent.ErrNotClaimable)
			assert.Nil(k.t, got)
			k.unchanged(before)

			for _, bad := range unkeepables {
				runs, err := k.store.ListRuns(k.ctx, agent.RunFilter{Agent: bad})
				require.NoError(k.t, err, "agent %q", bad)
				assert.Empty(k.t, runs, "agent %q", bad)
				runs, err = k.store.ListRuns(k.ctx, agent.RunFilter{Status: agent.Status(bad)})
				require.NoError(k.t, err, "status %q", bad)
				assert.Empty(k.t, runs, "status %q", bad)
				approvals, err := k.store.ListApprovals(k.ctx, agent.ApprovalFilter{Status: agent.ApprovalStatus(bad)})
				require.NoError(k.t, err, "approval status %q", bad)
				assert.Empty(k.t, approvals, "approval status %q", bad)
			}

			// Named beside one that can be kept, it is passed over.
			got, err = k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: append([]string{agentAlpha}, unkeepables...), Now: k.tick(), TTL: suiteTTL,
			})
			require.NoError(k.t, err)
			require.NotNil(k.t, got)
			assert.Equal(k.t, run.ID, got.ID)
		}},
	}
}

// notJSON are raw values no store keeps: JSON cut short, and JSON with a
// byte in it that is not UTF-8.
var notJSON = []struct {
	name string
	json string
}{
	{"cut short", `{"id":`},
	{"not UTF-8", "{\"a\":\"caf\xff\"}"},
	{"a control character written raw", "{\"a\":\"x\x00y\"}"},
}

func rawJSONCases() []storeCase {
	return []storeCase{
		{"a reply whose call arguments or provider's form are not JSON is refused", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			before := k.snapshot(run.ID)

			for _, bad := range notJSON {
				messages := []struct {
					name    string
					message agent.Message
				}{
					{"a call's arguments", agent.Message{
						Role: agent.RoleAssistant, Calls: []agent.Call{{ID: "call-1", Name: toolSend, Input: raw(bad.json)}},
					}},
					{"the provider's form", agent.Message{
						Role: agent.RoleAssistant, Text: "done", Opaque: &agent.Opaque{Provider: "provider-a", Data: raw(bad.json)},
					}},
				}
				for _, m := range messages {
					err := k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
						Seq: 1, Message: m.message, Stop: agent.StopToolUse, Model: suiteModel, Now: k.tick(),
					})

					require.Error(k.t, err, "%s, %s", m.name, bad.name)
					assert.NotErrorIs(k.t, err, agent.ErrConflict, "%s, %s: the step was there to complete", m.name, bad.name)
					k.unchanged(before)
				}
			}

			// A call the model wrote badly is still journaled: its arguments
			// are held as one JSON string.
			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Use(Call("call-1", toolSend, `{"id":`)).Message, Stop: agent.StopToolUse, Model: suiteModel, Now: k.tick(),
			}))
			assert.True(k.t, k.step(run.ID, 2).Call.Malformed)
		}},
		{"a run whose definition holds a schema that is not JSON is refused", func(k *kit) {
			for _, bad := range notJSON {
				definitions := []struct {
					name       string
					definition agent.Snapshot
				}{
					{"a tool's schema", agent.Snapshot{System: "system", Tools: []agent.ToolSpec{{Name: toolSend, Schema: raw(bad.json)}}}},
					{"the output schema", agent.Snapshot{System: "system", Output: raw(bad.json)}},
				}
				for _, d := range definitions {
					run := k.newRun(agentAlpha)
					run.Definition = d.definition

					_, created, err := k.store.CreateRun(k.ctx, run)

					require.Error(k.t, err, "%s, %s", d.name, bad.name)
					assert.False(k.t, created, "%s, %s", d.name, bad.name)
					_, err = k.store.GetRun(k.ctx, run.ID)
					assert.ErrorIs(k.t, err, agent.ErrNotFound, "%s, %s: nothing was stored", d.name, bad.name)
				}
			}
		}},
	}
}

func cursorCases() []storeCase {
	// stored puts three runs at each of three instants and returns them with
	// the instants.
	stored := func(k *kit) ([]agent.Run, []time.Time) {
		instants := []time.Time{k.tick(), k.tick(), k.tick()}
		var runs []agent.Run
		for _, at := range instants {
			for range 3 {
				run := k.newRun(agentAlpha)
				run.CreatedAt, run.UpdatedAt = at, at
				runs = append(runs, k.insert(run))
			}
		}
		return runs, instants
	}
	return []storeCase{
		{"a cursor whose id is not a UUID in the one form is refused", func(k *kit) {
			runs, instants := stored(k)
			ids := map[string]string{
				"no id at all":                 malformedID,
				"a run's id in upper case":     upperCase(runs[4].ID),
				"a run's id without hyphens":   noHyphens(runs[4].ID),
				"a run's id with space around": " " + runs[4].ID + " ",
			}
			for name, id := range ids {
				listed, err := k.store.ListRuns(k.ctx, agent.RunFilter{Before: &agent.Cursor{CreatedAt: instants[1], ID: id}})

				require.Error(k.t, err, name)
				assert.NotErrorIs(k.t, err, agent.ErrNotFound, "%s: a cursor is an argument, not a lookup", name)
				assert.Empty(k.t, listed, name)
			}
		}},
		{"a cursor with a time and no id is refused", func(k *kit) {
			_, instants := stored(k)

			listed, err := k.store.ListRuns(k.ctx, agent.RunFilter{Before: &agent.Cursor{CreatedAt: instants[1]}})

			require.Error(k.t, err)
			assert.Empty(k.t, listed)
		}},
		{"a cursor is judged before the filters", func(k *kit) {
			// The parent filter alone would list nothing and say nothing.
			_, err := k.store.ListRuns(k.ctx, agent.RunFilter{
				ParentID: malformedID, Before: &agent.Cursor{CreatedAt: k.now(), ID: malformedID},
			})
			require.Error(k.t, err)
		}},
		{"the zero cursor lists from the start", func(k *kit) {
			stored(k)
			all, err := k.store.ListRuns(k.ctx, agent.RunFilter{})
			require.NoError(k.t, err)
			require.Len(k.t, all, 9)

			listed, err := k.store.ListRuns(k.ctx, agent.RunFilter{Before: &agent.Cursor{}})

			require.NoError(k.t, err)
			assert.Equal(k.t, runIDs(all), runIDs(listed))
		}},
		{"a cursor that names no run is a position all the same", func(k *kit) {
			runs, instants := stored(k)
			created := map[string]time.Time{}
			for _, run := range runs {
				created[run.ID] = run.CreatedAt
			}
			ids := map[string]string{
				"below every id a run has": "00000000-0000-0000-0000-000000000000",
				"above every one":          "ffffffff-ffff-ffff-ffff-ffffffffffff",
				"among them":               newID(),
			}
			times := map[string]time.Time{
				"at an instant runs were created at":  instants[1],
				"between two such instants":           instants[1].Add(500 * time.Microsecond),
				"before every run":                    instants[0].Add(-time.Hour),
				"after every run":                     instants[2].Add(time.Hour),
				"at the instant of the newest of all": instants[2],
			}
			for idName, id := range ids {
				for timeName, at := range times {
					// Older than the position, newest first and then by id
					// descending.
					all, err := k.store.ListRuns(k.ctx, agent.RunFilter{})
					require.NoError(k.t, err)
					want := []string{}
					for _, run := range all {
						if run.CreatedAt.Before(at) || run.CreatedAt.Equal(at) && run.ID < id {
							want = append(want, run.ID)
						}
					}

					listed, err := k.store.ListRuns(k.ctx, agent.RunFilter{Before: &agent.Cursor{CreatedAt: at, ID: id}})

					require.NoError(k.t, err, "%s, %s", idName, timeName)
					assert.Equal(k.t, want, runIDs(listed), "%s, %s", idName, timeName)
				}
			}
		}},
	}
}
