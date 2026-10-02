package pg_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// What each column does with a NUL character, which a model, a tool or a
// person can put in any string they write, and with a byte that is not
// UTF-8.
//
// The JSON columns keep a NUL: inside a string it is the escape \u0000, which
// JSON text may hold. A TEXT column holds neither character, and a JSONB
// column no NUL, and Postgres would refuse the statement. The store never
// lets it: what it only records, it keeps with each such character as the
// replacement character, and what it compares, a name or a key, it refuses
// before it opens a transaction. This file records which is which, column by
// column.

const nul = "a\x00b"

// nulWorld is a store with a run held by workerA whose journal is a reply
// and the one call it made, and a second call asked about and parked on in
// another run.
type nulWorld struct {
	k      *kit
	run    agent.Run
	lease  agent.Lease
	parked agent.Run
	asked  agent.Approval
}

func newNULWorld(t *testing.T) nulWorld {
	k, _ := newKit(t)
	parked, asked := k.parked(nil)
	run, lease := k.proposed()
	return nulWorld{k: k, run: run, lease: lease, parked: parked, asked: asked}
}

func TestNUL_ColumnsThatKeepOne(t *testing.T) {
	tests := []struct {
		column string
		// write stores a value holding a NUL and returns what it reads back
		// as.
		write func(t *testing.T, w nulWorld) (wrote, read string)
	}{
		{"agent_runs.definition, in the system prompt", func(t *testing.T, w nulWorld) (string, string) {
			run := w.k.newRun(agentAlpha)
			run.Definition.System = nul
			w.k.insert(run)
			return nul, w.k.run(run.ID).Definition.System
		}},
		{"agent_runs.definition, in a tool's schema", func(t *testing.T, w nulWorld) (string, string) {
			schema := `{"default":"a\u0000b"}`
			run := w.k.newRun(agentAlpha)
			run.Definition.Tools = []agent.ToolSpec{{Name: toolSend, Description: nul, Schema: raw(schema)}}
			w.k.insert(run)
			tool := w.k.run(run.ID).Definition.Tools[0]
			return schema + nul, string(tool.Schema) + tool.Description
		}},
		{"agent_steps.message, in the reply's text", func(t *testing.T, w nulWorld) (string, string) {
			require.NoError(t, w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()))
			require.NoError(t, w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq: 3, Message: agent.Message{Role: agent.RoleAssistant, Text: nul}, Stop: agent.StopEnd, Now: w.k.tick(),
			}))
			return nul, w.k.steps(w.run.ID)[2].Message.Text
		}},
		{"agent_steps.message, in a tool result and the provider's form", func(t *testing.T, w nulWorld) (string, string) {
			data := `{"sig":"a\u0000b"}`
			require.NoError(t, w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()))
			require.NoError(t, w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq: 3,
				Message: agent.Message{
					Role:    agent.RoleAssistant,
					Results: []agent.Result{{CallID: nul, Content: nul}},
					Opaque:  &agent.Opaque{Provider: nul, Data: raw(data)},
				},
				Stop: agent.StopEnd, Now: w.k.tick(),
			}))
			m := w.k.steps(w.run.ID)[2].Message
			return nul + nul + nul + data, m.Results[0].CallID + m.Results[0].Content + m.Opaque.Provider + string(m.Opaque.Data)
		}},
		{"agent_steps.call, in a call's id and its arguments", func(t *testing.T, w nulWorld) (string, string) {
			input := `{"to":"a\u0000b"}`
			require.NoError(t, w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()))
			require.NoError(t, w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq:     3,
				Message: agent.Message{Role: agent.RoleAssistant, Calls: []agent.Call{{ID: nul, Name: toolSend, Input: raw(input)}}},
				Stop:    agent.StopToolUse, Now: w.k.tick(),
			}))
			call := w.k.steps(w.run.ID)[3].Call
			return nul + input, call.ID + string(call.Input)
		}},
		{"agent_approvals.input", func(t *testing.T, w nulWorld) (string, string) {
			input := `{"to":"a\u0000b"}`
			require.NoError(t, w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()))
			require.NoError(t, w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq:     3,
				Message: agenttest.Use(agenttest.Call("call-2", toolSend, input)).Message,
				Stop:    agent.StopToolUse, Now: w.k.tick(),
			}))
			approval := w.k.ask(w.lease, 4, nil)
			return input, string(w.k.approval(approval.ID).Input)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.column, func(t *testing.T) {
			wrote, read := tt.write(t, newNULWorld(t))
			assert.Equal(t, wrote, read)
		})
	}
}

// lost is what a NUL and a byte that is not UTF-8 are each kept as.
const lost = "a�b�"

func TestNUL_ColumnsThatKeepWhatTheyCannotHoldAsTheReplacementCharacter(t *testing.T) {
	const bad = nul + "\xff"
	tests := []struct {
		column string
		// write makes a store call that puts bad in the column, and
		// returns what the column then reads back as.
		write func(t *testing.T, w nulWorld) string
	}{
		{"agent_runs.input", func(t *testing.T, w nulWorld) string {
			run := w.k.newRun(agentAlpha)
			run.Input = bad
			return w.k.insert(run).Input
		}},
		{"agent_runs.metadata, in a key and in a value", func(t *testing.T, w nulWorld) string {
			run := w.k.newRun(agentAlpha)
			run.Metadata = map[string]string{bad: bad, "plain": "kept"}
			w.k.insert(run)
			metadata := w.k.run(run.ID).Metadata
			require.Equal(t, map[string]string{lost: lost, "plain": "kept"}, metadata)
			return metadata[lost]
		}},
		{"agent_runs.error, from Yield", func(t *testing.T, w nulWorld) string {
			require.NoError(t, w.k.store.Yield(w.k.ctx, w.lease, agent.YieldRequest{Failed: true, Error: bad, Now: w.k.tick()}))
			return w.k.run(w.run.ID).Error
		}},
		{"agent_runs.reason, from Park", func(t *testing.T, w nulWorld) string {
			w.k.ask(w.lease, 2, nil)
			parked, err := w.k.store.Park(w.k.ctx, w.lease, agent.ParkRequest{Reason: bad, Now: w.k.tick()})
			require.NoError(t, err)
			require.True(t, parked)
			return w.k.run(w.run.ID).Reason
		}},
		{"agent_runs.reason, output and error, from Finish", func(t *testing.T, w nulWorld) string {
			require.NoError(t, w.k.store.Finish(w.k.ctx, w.lease, agent.FinishRequest{
				Status: agent.StatusFailed, Reason: bad, Output: bad, Error: bad, Now: w.k.tick(),
			}))
			run := w.k.run(w.run.ID)
			require.Equal(t, lost, run.Reason)
			require.Equal(t, lost, run.Error)
			return run.Output
		}},
		{"agent_runs.cancel_by and cancel_reason", func(t *testing.T, w nulWorld) string {
			require.NoError(t, w.k.store.RequestCancel(w.k.ctx, agent.CancelRequest{RunID: w.run.ID, By: bad, Reason: bad, Now: w.k.tick()}))
			run := w.k.run(w.run.ID)
			require.Equal(t, lost, run.CancelBy)
			return run.CancelReason
		}},
		{"agent_steps.name and stop, of a model step", func(t *testing.T, w nulWorld) string {
			require.NoError(t, w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()))
			require.NoError(t, w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq: 3, Message: agenttest.Say("done").Message, Stop: agent.Stop(bad), Model: bad, Now: w.k.tick(),
			}))
			step := w.k.steps(w.run.ID)[2]
			require.Equal(t, agent.Stop(lost), step.Stop)
			return step.Name
		}},
		{"agent_steps.name, of a tool step: the name the model called, and agent_approvals.tool", func(t *testing.T, w nulWorld) string {
			require.NoError(t, w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()))
			require.NoError(t, w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq:     3,
				Message: agent.Message{Role: agent.RoleAssistant, Calls: []agent.Call{{ID: "call-2", Name: bad, Input: raw(sendInput)}}},
				Stop:    agent.StopToolUse, Now: w.k.tick(),
			}))
			step := w.k.steps(w.run.ID)[3]
			require.Equal(t, nul+"�", step.Call.Name, "the call keeps its NUL: it is in a json column")
			require.Equal(t, lost, w.k.ask(w.lease, 4, nil).Tool)
			return step.Name
		}},
		{"agent_steps.decision, rule and result: what a tool returned", func(t *testing.T, w nulWorld) string {
			text := bad
			require.NoError(t, w.k.store.UpdateStep(w.k.ctx, w.lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepCompleted,
				Decision: agent.Effect(bad), Rule: bad, Result: &text, Now: w.k.tick(),
			}))
			step := w.k.steps(w.run.ID)[1]
			require.Equal(t, agent.Effect(lost), step.Decision)
			require.Equal(t, lost, step.Rule)
			return step.Result
		}},
		{"agent_approvals.action, in its kind, its target, and the keys and values of its attributes", func(t *testing.T, w nulWorld) string {
			req := w.k.askRequest(2)
			req.Action = agent.Action{Kind: bad, Target: bad, Attrs: map[string]any{
				bad:      bad,
				"nested": map[string]any{bad: []any{bad, 7, nil}},
				// The six characters of the escape, which are not a NUL.
				"written": `\u0000`,
				"exact":   uint64(12345678901234567890),
			}}
			_, err := w.k.store.RequestApproval(w.k.ctx, w.lease, req)
			require.NoError(t, err)
			action := w.k.approval(req.ID).Action
			require.Equal(t, lost, action.Kind)
			require.Equal(t, map[string]any{
				lost:      lost,
				"nested":  map[string]any{lost: []any{lost, float64(7), nil}},
				"written": `\u0000`,
				"exact":   float64(12345678901234567890),
			}, action.Attrs)
			return action.Target
		}},
		{"agent_approvals.rule, and the step's decision and rule", func(t *testing.T, w nulWorld) string {
			req := w.k.askRequest(2)
			req.Decision, req.Rule = agent.Effect(bad), bad
			approval, err := w.k.store.RequestApproval(w.k.ctx, w.lease, req)
			require.NoError(t, err)
			step := w.k.steps(w.run.ID)[1]
			require.Equal(t, agent.Effect(lost), step.Decision)
			require.Equal(t, lost, step.Rule)
			return approval.Rule
		}},
		{"agent_approvals.decided_by and reason", func(t *testing.T, w nulWorld) string {
			approval, err := w.k.store.DecideApproval(w.k.ctx, agent.DecideRequest{ID: w.asked.ID, Approved: true, By: bad, Reason: bad, Now: w.k.tick()})
			require.NoError(t, err)
			require.Equal(t, lost, approval.DecidedBy)
			return w.k.approval(w.asked.ID).Reason
		}},
	}
	for _, tt := range tests {
		t.Run(tt.column, func(t *testing.T) {
			assert.Equal(t, lost, tt.write(t, newNULWorld(t)))
		})
	}
}

// A name is compared, so it cannot be kept as something else: one that
// holds a NUL or a byte that is not UTF-8 is refused, for what it is and
// before a transaction is opened.
func TestNUL_NamesThatCannotBeKeptAreRefused(t *testing.T) {
	store := agentpg.New(neverBegun{t})
	ctx := t.Context()
	run := func(edit func(*agent.Run)) agent.Run {
		r := agent.Run{ID: newID(), Agent: agentAlpha, Status: agent.StatusRunnable, CreatedAt: testStart, UpdatedAt: testStart}
		edit(&r)
		return r
	}

	for _, bad := range []string{nul, "caf\xff"} {
		refusals := map[string]func() error{
			"agent_runs.agent": func() error {
				_, _, err := store.CreateRun(ctx, run(func(r *agent.Run) { r.Agent = bad }))
				return err
			},
			"agent_runs.start_key": func() error {
				_, _, err := store.CreateRun(ctx, run(func(r *agent.Run) { r.Key = bad }))
				return err
			},
			"agent_runs.lease_owner, on a run stored with one": func() error {
				_, _, err := store.CreateRun(ctx, run(func(r *agent.Run) { r.LeaseOwner = bad }))
				return err
			},
			"agent_runs.lease_owner, from Claim": func() error {
				_, err := store.Claim(ctx, agent.ClaimRequest{Owner: bad, Agents: testAgents, Now: testStart, TTL: testTTL})
				return err
			},
		}
		for column, refuse := range refusals {
			err := refuse()
			require.Error(t, err, "%s, given %q", column, bad)
			var fromDatabase *pgconn.PgError
			assert.NotErrorAs(t, err, &fromDatabase, column)
		}
	}
}

// An agent's name that no column keeps is no run's agent. A claim that
// lists one claims for the others.
func TestNUL_AClaimPassesOverAnAgentNoRunCouldHave(t *testing.T) {
	k, _ := newKit(t)
	run := k.create()

	got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: []string{nul, "caf\xff"}, Now: k.tick(), TTL: testTTL})
	require.NoError(t, err)
	assert.Nil(t, got)

	_, err = k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: []string{nul}, RunID: run.ID, Now: k.tick(), TTL: testTTL})
	require.ErrorIs(t, err, agent.ErrNotClaimable)

	got, err = k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: []string{nul, agentAlpha}, Now: k.tick(), TTL: testTTL})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, run.ID, got.ID)
}

func TestNUL_OnceRefusesAKeyThatCannotBeKept(t *testing.T) {
	tx := begin(t, newDatabase(t).pool(t))

	for _, key := range []string{"run-1:2" + nul, "run-1:2\xff"} {
		_, err := agentpg.Once(t.Context(), tx, key)
		require.Error(t, err)
		var fromDatabase *pgconn.PgError
		assert.NotErrorAs(t, err, &fromDatabase, "the key never reached the database")
	}

	// The transaction is still good: Postgres was not asked.
	first, err := agentpg.Once(t.Context(), tx, "run-1:2")
	require.NoError(t, err)
	assert.True(t, first)
}
