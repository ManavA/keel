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
//
// The replacement is made byte by byte, as encoding/json makes it: each byte
// that is no part of a valid encoding is one replacement character, however
// many stand together. A string cut at a byte limit ends in such bytes.
type unkeepable struct {
	name  string
	given string
	// kept is what given reads back as where it is only recorded.
	kept string
	// inJSON is what given reads back as inside a message or a definition.
	inJSON string
}

var unkeepables = []unkeepable{
	{"a NUL and a byte that is not UTF-8", "a\x00b\xff", "a\ufffdb\ufffd", "a\x00b\ufffd"},
	{"a three-byte character cut after two", "x\xe2\x82y", "x\ufffd\ufffdy", "x\ufffd\ufffdy"},
	{"a four-byte character cut after three", "x\xf0\x9f\x98", "x\ufffd\ufffd\ufffd", "x\ufffd\ufffd\ufffd"},
	{"a NUL beside a byte that is not UTF-8", "x\x00\xffy", "x\ufffd\ufffdy", "x\x00\ufffdy"},
}

// each runs check once for every string no column holds.
func each(check func(k *kit, u unkeepable)) func(k *kit) {
	return func(k *kit) {
		for _, u := range unkeepables {
			check(k, u)
		}
	}
}

func stringsCases() []storeCase {
	result := func(s string) *string { return &s }
	return []storeCase{
		{"keeps what a run is stored with: its input, reason, output, error, and who cancelled it and why", each(func(k *kit, u unkeepable) {
			run := k.newRun(agentAlpha)
			run.Input, run.Reason, run.Output, run.Error = u.given, u.given, u.given, u.given
			run.CancelBy, run.CancelReason = u.given, u.given

			stored, _, err := k.store.CreateRun(k.ctx, run)

			require.NoError(k.t, err, u.name)
			want := run
			want.Rev, want.Metadata = 1, map[string]string{}
			want.Input, want.Reason, want.Output, want.Error = u.kept, u.kept, u.kept, u.kept
			want.CancelBy, want.CancelReason = u.kept, u.kept
			k.equalRun(want, stored)
			k.equalRun(want, k.run(run.ID))
		})},
		{"keeps metadata, its keys and its values", each(func(k *kit, u unkeepable) {
			run := k.newRun(agentAlpha)
			run.Metadata = map[string]string{u.given: u.given, "plain": "kept"}
			want := map[string]string{u.kept: u.kept, "plain": "kept"}

			stored, _, err := k.store.CreateRun(k.ctx, run)

			require.NoError(k.t, err, u.name)
			assert.Equal(k.t, want, stored.Metadata, "%s, as returned", u.name)
			assert.Equal(k.t, want, k.run(run.ID).Metadata, "%s, as read", u.name)
		})},
		{"keeps the definition: a NUL as it is, and a byte that is not UTF-8 as the replacement character", each(func(k *kit, u unkeepable) {
			run := k.newRun(agentAlpha)
			run.Definition = agent.Snapshot{
				System: u.given,
				Model:  u.given,
				Tools:  []agent.ToolSpec{{Name: u.given, Description: u.given, Schema: raw(`{"default":"a\u0000b"}`)}},
				Output: raw(`{"const":"a\u0000b"}`),
			}

			stored, _, err := k.store.CreateRun(k.ctx, run)

			require.NoError(k.t, err, u.name)
			want := run.Definition
			want.System, want.Model = u.inJSON, u.inJSON
			want.Tools = []agent.ToolSpec{{Name: u.inJSON, Description: u.inJSON, Schema: raw(`{"default":"a\u0000b"}`)}}
			assert.Equal(k.t, want, stored.Definition, "%s, as returned", u.name)
			assert.Equal(k.t, want, k.run(run.ID).Definition, "%s, as read", u.name)
		})},
		{"keeps the error of an execution that failed", each(func(k *kit, u unkeepable) {
			run, lease := k.held(agentAlpha)

			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Failed: true, Error: u.given, Now: k.tick()}), u.name)

			assert.Equal(k.t, u.kept, k.run(run.ID).Error, u.name)
		})},
		{"keeps the reason a run waits", each(func(k *kit, u unkeepable) {
			run, lease := k.proposed(agentAlpha)
			k.ask(lease, 2, nil)

			parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: u.given, Now: k.tick()})

			require.NoError(k.t, err, u.name)
			require.True(k.t, parked, u.name)
			assert.Equal(k.t, u.kept, k.run(run.ID).Reason, u.name)
		})},
		{"keeps how a run ended: its reason, its output and its error", each(func(k *kit, u unkeepable) {
			run, lease := k.held(agentAlpha)

			require.NoError(k.t, k.store.Finish(k.ctx, lease, agent.FinishRequest{
				Status: agent.StatusFailed, Reason: u.given, Output: u.given, Error: u.given, Now: k.tick(),
			}), u.name)

			after := k.run(run.ID)
			assert.Equal(k.t, agent.StatusFailed, after.Status, u.name)
			assert.Equal(k.t, u.kept, after.Reason, u.name)
			assert.Equal(k.t, u.kept, after.Output, "%s: the model's last words are not what stops a run from ending", u.name)
			assert.Equal(k.t, u.kept, after.Error, u.name)
		})},
		{"keeps who asked for a run to be cancelled, and why", each(func(k *kit, u unkeepable) {
			run := k.create(agentAlpha)

			require.NoError(k.t, k.store.RequestCancel(k.ctx, agent.CancelRequest{
				RunID: run.ID, By: u.given, Reason: u.given, Now: k.tick(),
			}), u.name)

			after := k.run(run.ID)
			assert.True(k.t, after.CancelRequested, u.name)
			assert.Equal(k.t, u.kept, after.CancelBy, u.name)
			assert.Equal(k.t, u.kept, after.CancelReason, u.name)
		})},
		{"keeps a reply: the model's name and why it stopped, and inside the message a NUL as it is", each(func(k *kit, u unkeepable) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			message := agent.Message{
				Role:    agent.RoleAssistant,
				Text:    u.given,
				Results: []agent.Result{{CallID: u.given, Content: u.given}},
				Opaque:  &agent.Opaque{Provider: u.given, Data: raw(`{"sig":"a\u0000b"}`)},
			}

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: message, Stop: agent.Stop(u.given), Model: u.given, Now: k.tick(),
			}), u.name)

			step := k.step(run.ID, 1)
			assert.Equal(k.t, agent.StepCompleted, step.Status, u.name)
			assert.Equal(k.t, u.kept, step.Name, u.name)
			assert.Equal(k.t, agent.Stop(u.kept), step.Stop, u.name)
			require.NotNil(k.t, step.Message, u.name)
			want := agent.Message{
				Role:    agent.RoleAssistant,
				Text:    u.inJSON,
				Results: []agent.Result{{CallID: u.inJSON, Content: u.inJSON}},
				Opaque:  &agent.Opaque{Provider: u.inJSON, Data: raw(`{"sig":"a\u0000b"}`)},
			}
			assert.Equal(k.t, normalMessage(want), normalMessage(*step.Message), u.name)
		})},
		{"keeps a call to a tool whose name cannot be kept: the name on the step and on its approval", each(func(k *kit, u unkeepable) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
			call := agent.Call{ID: u.given, Name: u.given, Input: raw(`{"to":"a\u0000b"}`)}

			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Use(call).Message, Stop: agent.StopToolUse, Model: suiteModel, Now: k.tick(),
			}), u.name)

			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepProposed, step.Status, "%s: the model's turn was journaled, and the call can be answered", u.name)
			assert.Equal(k.t, u.kept, step.Name, u.name)
			// The call is JSON: its NUL is kept, in its strings and in its
			// arguments.
			want := agent.Call{ID: u.inJSON, Name: u.inJSON, Input: raw(`{"to":"a\u0000b"}`)}
			require.NotNil(k.t, step.Call, u.name)
			assert.Equal(k.t, want, *step.Call, u.name)
			assert.Equal(k.t, []agent.Call{want}, k.step(run.ID, 1).Message.Calls, u.name)

			approval := k.ask(lease, 2, nil)
			assert.Equal(k.t, u.kept, approval.Tool, u.name)
			assert.Equal(k.t, `{"to":"a\u0000b"}`, string(approval.Input), u.name)
			assert.Equal(k.t, u.kept, k.approval(approval.ID).Tool, u.name)
		})},
		{"keeps what a tool returned, and the decision and rule on its step", each(func(k *kit, u unkeepable) {
			run, lease := k.proposed(agentAlpha)
			k.update(lease, 2, agent.StepProposed, agent.StepStarted)

			require.NoError(k.t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepCompleted,
				Decision: agent.Effect(u.given), Rule: u.given, Result: result(u.given), Now: k.tick(),
			}), u.name)

			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepCompleted, step.Status, "%s: the result was journaled, so the call is never made again", u.name)
			assert.Equal(k.t, u.kept, step.Result, u.name)
			assert.Equal(k.t, agent.Effect(u.kept), step.Decision, u.name)
			assert.Equal(k.t, u.kept, step.Rule, u.name)
		})},
		{"keeps a question: its rule, and the decision and rule it puts on the step", each(func(k *kit, u unkeepable) {
			run, lease := k.proposed(agentAlpha)
			req := k.askRequest(2)
			req.Decision, req.Rule = agent.Effect(u.given), u.given

			got, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err, u.name)
			assert.Equal(k.t, u.kept, got.Rule, "%s, as returned", u.name)
			assert.Equal(k.t, u.kept, k.approval(req.ID).Rule, "%s, as read", u.name)
			step := k.step(run.ID, 2)
			assert.Equal(k.t, agent.StepWaiting, step.Status, u.name)
			assert.Equal(k.t, agent.Effect(u.kept), step.Decision, u.name)
			assert.Equal(k.t, u.kept, step.Rule, u.name)
		})},
		{"keeps an action: its kind, its target, and the keys and values of its attributes", each(func(k *kit, u unkeepable) {
			_, lease := k.proposed(agentAlpha)
			req := k.askRequest(2)
			req.Action = agent.Action{Kind: u.given, Target: u.given, Attrs: map[string]any{
				u.given:  u.given,
				"nested": map[string]any{u.given: []any{u.given, 7, nil}},
				// The six characters of JSON's escape for a NUL, which are
				// not a NUL.
				"written": `\u0000`,
			}}
			want := agent.Action{Kind: u.kept, Target: u.kept, Attrs: map[string]any{
				u.kept:    u.kept,
				"nested":  map[string]any{u.kept: []any{u.kept, float64(7), nil}},
				"written": `\u0000`,
			}}

			got, err := k.store.RequestApproval(k.ctx, lease, req)

			require.NoError(k.t, err, u.name)
			assert.Equal(k.t, want, got.Action, "%s, as returned", u.name)
			assert.Equal(k.t, want, k.approval(req.ID).Action, "%s, as read", u.name)
		})},
		{"keeps who answered an approval, and why", each(func(k *kit, u unkeepable) {
			_, pending := k.parked(agentAlpha, nil)

			got, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{
				ID: pending.ID, Approved: true, By: u.given, Reason: u.given, Now: k.tick(),
			})

			require.NoError(k.t, err, u.name)
			assert.Equal(k.t, agent.ApprovalApproved, got.Status, u.name)
			assert.Equal(k.t, u.kept, got.DecidedBy, "%s, as returned", u.name)
			assert.Equal(k.t, u.kept, got.Reason, "%s, as returned", u.name)
			read := k.approval(pending.ID)
			assert.Equal(k.t, u.kept, read.DecidedBy, "%s, as read", u.name)
			assert.Equal(k.t, u.kept, read.Reason, "%s, as read", u.name)
		})},
		{"refuses a run whose agent, start key or owner cannot be kept", each(func(k *kit, u unkeepable) {
			edits := []struct {
				name string
				edit func(run *agent.Run, bad string)
			}{
				{"its agent", func(run *agent.Run, bad string) { run.Agent = bad }},
				{"its start key", func(run *agent.Run, bad string) { run.Key = bad }},
				{"the owner it is stored with", func(run *agent.Run, bad string) { run.LeaseOwner = bad }},
			}
			for _, e := range edits {
				run := k.newRun(agentAlpha)
				e.edit(&run, u.given)

				_, created, err := k.store.CreateRun(k.ctx, run)

				require.Error(k.t, err, "%s, given %s", e.name, u.name)
				assert.False(k.t, created, "%s, given %s", e.name, u.name)
				_, err = k.store.GetRun(k.ctx, run.ID)
				assert.ErrorIs(k.t, err, agent.ErrNotFound, "%s, given %s: nothing was stored", e.name, u.name)
			}
		})},
		{"refuses a claim under an owner that cannot be kept", func(k *kit) {
			run := k.create(agentAlpha)
			before := k.snapshot(run.ID)

			for _, u := range unkeepables {
				got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: u.given, Agents: suiteAgents, Now: k.tick(), TTL: suiteTTL})
				require.Error(k.t, err, u.name)
				assert.Nil(k.t, got)

				got, err = k.store.Claim(k.ctx, agent.ClaimRequest{
					Owner: u.given, Agents: suiteAgents, RunID: run.ID, Now: k.tick(), TTL: suiteTTL,
				})
				require.Error(k.t, err, "%s, by id", u.name)
				assert.NotErrorIs(k.t, err, agent.ErrNotClaimable, "the claim is refused, not the run")
				assert.Nil(k.t, got)
			}
			k.unchanged(before)
		}},
		{"a lease under an owner that cannot be kept is no run's", func(k *kit) {
			run, lease := k.held(agentAlpha)
			before := k.snapshot(run.ID)

			for _, u := range unkeepables {
				err := k.store.BeginModel(k.ctx, agent.Lease{RunID: run.ID, Owner: u.given, Epoch: lease.Epoch}, 1, k.tick())
				require.ErrorIs(k.t, err, agent.ErrLeaseLost, u.name)
			}
			k.unchanged(before)
		}},
		{"an agent whose name cannot be kept has no runs to claim or to list", func(k *kit) {
			run := k.create(agentAlpha)
			before := k.snapshot(run.ID)
			names := make([]string, len(unkeepables))
			for i, u := range unkeepables {
				names[i] = u.given
			}

			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: names, Now: k.tick(), TTL: suiteTTL})
			require.NoError(k.t, err)
			assert.Nil(k.t, got)
			got, err = k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: names, RunID: run.ID, Now: k.tick(), TTL: suiteTTL,
			})
			require.ErrorIs(k.t, err, agent.ErrNotClaimable)
			assert.Nil(k.t, got)
			k.unchanged(before)

			for _, bad := range names {
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
				Owner: workerA, Agents: append([]string{agentAlpha}, names...), Now: k.tick(), TTL: suiteTTL,
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
				"no UUID at all":               malformedID,
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

// A store keeps a time to the microsecond, which is what a database column
// holds: the microsecond below it, for every time but one. A lease's expiry
// is kept to the microsecond above, so that a store never counts a lease
// lapsed before its holder does.
func timesCases() []storeCase {
	const (
		// under is less than a microsecond.
		under = 999 * time.Nanosecond
		micro = time.Microsecond
	)
	return []storeCase{
		{"keeps every time to the microsecond below it", func(k *kit) {
			base := k.now().Add(time.Hour)
			at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }

			run := k.newRun(agentAlpha)
			run.CreatedAt, run.UpdatedAt = at(1).Add(under), at(1).Add(1500*time.Nanosecond)
			expires, retry := at(2).Add(under), at(2).Add(under)
			run.LeaseExpiresAt, run.NextAttemptAt = &expires, &retry
			stored := k.insert(run)
			for name, got := range map[string]agent.Run{"as returned": stored, "as read": k.run(run.ID)} {
				assert.True(k.t, at(1).Equal(got.CreatedAt), "CreatedAt %s: %s", name, got.CreatedAt)
				assert.True(k.t, at(1).Add(micro).Equal(got.UpdatedAt), "UpdatedAt %s: %s", name, got.UpdatedAt)
				k.timeIs(at(2), got.LeaseExpiresAt, "a lease's expiry a run is stored with, "+name)
				k.timeIs(at(2), got.NextAttemptAt, "NextAttemptAt "+name)
			}

			held, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: suiteAgents, RunID: run.ID, Now: at(3).Add(under), TTL: suiteTTL,
			})
			require.NoError(k.t, err)
			assert.True(k.t, at(3).Equal(held.UpdatedAt), "UpdatedAt after a claim: %s", held.UpdatedAt)
			lease := held.Lease()

			// A reply that took a second and a half, between two times as
			// they are kept: a nanosecond short of that as they were given.
			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, at(4).Add(under)))
			require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: Use(Call("call-1", toolSend, sendInput)).Message, Stop: agent.StopToolUse,
				Model: suiteModel, Now: at(1504).Add(500 * time.Nanosecond),
			}))
			reply := k.step(run.ID, 1)
			assert.True(k.t, at(4).Equal(reply.CreatedAt), "a step's CreatedAt: %s", reply.CreatedAt)
			k.timeIs(at(4), reply.StartedAt, "a step's StartedAt")
			k.timeIs(at(1504), reply.FinishedAt, "a step's FinishedAt")
			assert.Equal(k.t, int64(1500), k.run(run.ID).ActiveMillis, "the time working is between the times as they are kept")
			assert.True(k.t, at(1504).Equal(k.step(run.ID, 2).CreatedAt), "a proposed step's CreatedAt")

			req := k.askRequest(2)
			due := at(5000).Add(under)
			req.Now, req.ExpiresAt = at(1505).Add(under), &due
			asked, err := k.store.RequestApproval(k.ctx, lease, req)
			require.NoError(k.t, err)
			assert.True(k.t, at(1505).Equal(asked.RequestedAt), "RequestedAt: %s", asked.RequestedAt)
			k.timeIs(at(5000), asked.ExpiresAt, "an approval's ExpiresAt")
			decided, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{
				ID: req.ID, Approved: true, By: personA, Now: at(1506).Add(under),
			})
			require.NoError(k.t, err)
			k.timeIs(at(1506), decided.DecidedAt, "DecidedAt")

			again := at(1600).Add(under)
			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{NextAttemptAt: &again, Now: at(1507).Add(under)}))
			yielded := k.run(run.ID)
			k.timeIs(at(1600), yielded.NextAttemptAt, "NextAttemptAt")
			assert.True(k.t, at(1507).Equal(yielded.UpdatedAt), "UpdatedAt after a yield: %s", yielded.UpdatedAt)

			// It is claimable at the microsecond its NextAttemptAt was kept
			// as, though that is before the time it was given.
			held, err = k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: suiteAgents, RunID: run.ID, Now: at(1600), TTL: suiteTTL,
			})
			require.NoError(k.t, err)
			require.NoError(k.t, k.store.Finish(k.ctx, held.Lease(), agent.FinishRequest{
				Status: agent.StatusCompleted, Now: at(1601).Add(under),
			}))
			k.timeIs(at(1601), k.run(run.ID).FinishedAt, "FinishedAt")
		}},
		{"keeps a lease's expiry to the microsecond above it, so that it is never taken before its holder counts it lapsed", func(k *kit) {
			run := k.create(agentAlpha)
			now := k.tick()
			// The lease lapses half a microsecond past a whole one.
			ttl := suiteTTL + 500*time.Nanosecond
			lapses := now.Add(ttl)

			held, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: suiteAgents, RunID: run.ID, Now: now, TTL: ttl})
			require.NoError(k.t, err)
			k.timeIs(now.Add(suiteTTL+micro), held.LeaseExpiresAt, "the expiry as returned")
			k.timeIs(now.Add(suiteTTL+micro), k.run(run.ID).LeaseExpiresAt, "the expiry as read")
			before := k.snapshot(run.ID)

			claim := func(at time.Time) (*agent.Run, error) {
				return k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerB, Agents: suiteAgents, Now: at, TTL: suiteTTL})
			}
			// A nanosecond before the holder counts its lease lapsed, and
			// at every instant up to the end of that microsecond.
			for _, early := range []time.Time{lapses.Add(-time.Nanosecond), now.Add(suiteTTL), lapses, now.Add(suiteTTL + under)} {
				got, err := claim(early)
				require.NoError(k.t, err)
				require.Nil(k.t, got, "the lease was taken at %s, and its holder counts it good until %s", early, lapses)
			}
			k.unchanged(before)

			got, err := claim(now.Add(suiteTTL + micro))
			require.NoError(k.t, err)
			require.NotNil(k.t, got, "at the microsecond the expiry was kept as")
			assert.Equal(k.t, workerB, got.LeaseOwner)
		}},
		{"a heartbeat extends the lease to the microsecond above, and a lease of whole microseconds is kept as it is", func(k *kit) {
			run, lease := k.held(agentAlpha)
			now := k.tick()

			_, err := k.store.Heartbeat(k.ctx, lease, now.Add(time.Nanosecond), suiteTTL)
			require.NoError(k.t, err)
			k.timeIs(now.Add(suiteTTL+micro), k.run(run.ID).LeaseExpiresAt, "a nanosecond past a whole microsecond")

			_, err = k.store.Heartbeat(k.ctx, lease, now, suiteTTL+3*micro)
			require.NoError(k.t, err)
			k.timeIs(now.Add(suiteTTL+3*micro), k.run(run.ID).LeaseExpiresAt, "a whole number of microseconds")
		}},
	}
}
