package pg_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// What each column does with a NUL character, which a model, a tool or a
// person can put in any string they write.
//
// The JSON columns keep one: inside a string it is the escape \u0000, which
// JSON text may hold. The TEXT columns and the two JSONB columns cannot hold
// one at all, and Postgres refuses the statement. So does a byte that is not
// UTF-8 in a TEXT column. This file records which is which, column by
// column, so that the answer is on the page and a change to a column's type
// shows here.

// The SQLSTATEs Postgres refuses a string with.
const (
	// refusedText is character_not_in_repertoire: a TEXT value holds a NUL
	// or a byte sequence that is not UTF-8.
	refusedText = "22021"
	// refusedJSONB is untranslatable_character: a JSONB string holds \u0000.
	refusedJSONB = "22P05"
)

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

func TestNUL_ColumnsThatRefuseOne(t *testing.T) {
	tests := []struct {
		column string
		code   string
		// write makes a store call that would put value in the column, and
		// is otherwise one the store accepts.
		write func(w nulWorld, value string) error
	}{
		{"agent_runs.agent", refusedText, func(w nulWorld, v string) error {
			_, _, err := w.k.store.CreateRun(w.k.ctx, w.k.newRun(agentAlpha+v))
			return err
		}},
		{"agent_runs.input", refusedText, func(w nulWorld, v string) error {
			run := w.k.newRun(agentAlpha)
			run.Input = "input" + v
			_, _, err := w.k.store.CreateRun(w.k.ctx, run)
			return err
		}},
		{"agent_runs.start_key", refusedText, func(w nulWorld, v string) error {
			run := w.k.newRun(agentAlpha)
			run.Key = "start-1" + v
			_, _, err := w.k.store.CreateRun(w.k.ctx, run)
			return err
		}},
		{"agent_runs.metadata", refusedJSONB, func(w nulWorld, v string) error {
			run := w.k.newRun(agentAlpha)
			run.Metadata = map[string]string{"note": "n" + v}
			_, _, err := w.k.store.CreateRun(w.k.ctx, run)
			return err
		}},
		{"agent_runs.lease_owner", refusedText, func(w nulWorld, v string) error {
			_, err := w.k.store.Claim(w.k.ctx, agent.ClaimRequest{
				Owner: workerB + v, Agents: testAgents, RunID: w.k.create().ID, Now: w.k.tick(), TTL: testTTL,
			})
			return err
		}},
		{"agent_runs.error, from Yield", refusedText, func(w nulWorld, v string) error {
			return w.k.store.Yield(w.k.ctx, w.lease, agent.YieldRequest{Failed: true, Error: "boom" + v, Now: w.k.tick()})
		}},
		{"agent_runs.reason, from Park", refusedText, func(w nulWorld, v string) error {
			w.k.ask(w.lease, 2, nil)
			_, err := w.k.store.Park(w.k.ctx, w.lease, agent.ParkRequest{Reason: agent.ReasonApproval + v, Now: w.k.tick()})
			return err
		}},
		{"agent_runs.reason, from Finish", refusedText, func(w nulWorld, v string) error {
			return w.k.store.Finish(w.k.ctx, w.lease, agent.FinishRequest{
				Status: agent.StatusFailed, Reason: agent.ReasonError + v, Now: w.k.tick(),
			})
		}},
		{"agent_runs.output", refusedText, func(w nulWorld, v string) error {
			return w.k.store.Finish(w.k.ctx, w.lease, agent.FinishRequest{
				Status: agent.StatusCompleted, Output: "the answer" + v, Now: w.k.tick(),
			})
		}},
		{"agent_runs.error, from Finish", refusedText, func(w nulWorld, v string) error {
			return w.k.store.Finish(w.k.ctx, w.lease, agent.FinishRequest{
				Status: agent.StatusFailed, Error: "gave up" + v, Now: w.k.tick(),
			})
		}},
		{"agent_runs.cancel_by", refusedText, func(w nulWorld, v string) error {
			return w.k.store.RequestCancel(w.k.ctx, agent.CancelRequest{RunID: w.run.ID, By: person + v, Now: w.k.tick()})
		}},
		{"agent_runs.cancel_reason", refusedText, func(w nulWorld, v string) error {
			return w.k.store.RequestCancel(w.k.ctx, agent.CancelRequest{
				RunID: w.run.ID, By: person, Reason: "wrong batch" + v, Now: w.k.tick(),
			})
		}},
		{"agent_steps.name, of a model step", refusedText, func(w nulWorld, v string) error {
			if err := w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()); err != nil {
				return err
			}
			return w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq: 3, Message: agenttest.Say("done").Message, Stop: agent.StopEnd, Model: "model-a" + v, Now: w.k.tick(),
			})
		}},
		{"agent_steps.stop", refusedText, func(w nulWorld, v string) error {
			if err := w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()); err != nil {
				return err
			}
			return w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq: 3, Message: agenttest.Say("done").Message, Stop: agent.StopEnd + agent.Stop(v), Now: w.k.tick(),
			})
		}},
		{"agent_steps.name, of a tool step: the name the model called", refusedText, func(w nulWorld, v string) error {
			if err := w.k.store.BeginModel(w.k.ctx, w.lease, 3, w.k.tick()); err != nil {
				return err
			}
			return w.k.store.CompleteModel(w.k.ctx, w.lease, agent.CompleteModelRequest{
				Seq:     3,
				Message: agenttest.Use(agenttest.Call("call-2", toolSend+v, sendInput)).Message,
				Stop:    agent.StopToolUse, Now: w.k.tick(),
			})
		}},
		{"agent_steps.decision", refusedText, func(w nulWorld, v string) error {
			return w.k.store.UpdateStep(w.k.ctx, w.lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepStarted, Decision: agent.Allow + agent.Effect(v), Now: w.k.tick(),
			})
		}},
		{"agent_steps.rule", refusedText, func(w nulWorld, v string) error {
			return w.k.store.UpdateStep(w.k.ctx, w.lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepStarted, Decision: agent.Allow, Rule: "a rule" + v, Now: w.k.tick(),
			})
		}},
		{"agent_steps.result: what a tool returned", refusedText, func(w nulWorld, v string) error {
			text := "sent" + v
			return w.k.store.UpdateStep(w.k.ctx, w.lease, agent.StepUpdate{
				Seq: 2, From: agent.StepProposed, To: agent.StepCompleted, Result: &text, Now: w.k.tick(),
			})
		}},
		{"agent_approvals.action, in an attribute", refusedJSONB, func(w nulWorld, v string) error {
			req := w.k.askRequest(2)
			req.Action.Attrs = map[string]any{"to": "a" + v}
			_, err := w.k.store.RequestApproval(w.k.ctx, w.lease, req)
			return err
		}},
		{"agent_approvals.action, in the target", refusedJSONB, func(w nulWorld, v string) error {
			req := w.k.askRequest(2)
			req.Action.Target = toolSend + v
			_, err := w.k.store.RequestApproval(w.k.ctx, w.lease, req)
			return err
		}},
		{"agent_approvals.rule", refusedText, func(w nulWorld, v string) error {
			req := w.k.askRequest(2)
			req.Rule += v
			_, err := w.k.store.RequestApproval(w.k.ctx, w.lease, req)
			return err
		}},
		{"agent_approvals.decided_by", refusedText, func(w nulWorld, v string) error {
			_, err := w.k.store.DecideApproval(w.k.ctx, agent.DecideRequest{ID: w.asked.ID, Approved: true, By: person + v, Now: w.k.tick()})
			return err
		}},
		{"agent_approvals.reason", refusedText, func(w nulWorld, v string) error {
			_, err := w.k.store.DecideApproval(w.k.ctx, agent.DecideRequest{
				ID: w.asked.ID, Approved: true, By: person, Reason: "checked" + v, Now: w.k.tick(),
			})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.column, func(t *testing.T) {
			w := newNULWorld(t)

			err := tt.write(w, nul)

			var refusal *pgconn.PgError
			require.ErrorAs(t, err, &refusal, "the store kept a NUL in %s", tt.column)
			assert.Equal(t, tt.code, refusal.Code)
			for _, sentinel := range []error{agent.ErrNotFound, agent.ErrLeaseLost, agent.ErrConflict, agent.ErrAlreadyDecided} {
				assert.NotErrorIs(t, err, sentinel)
			}

			// The statement that was refused took its transaction with it,
			// so nothing was left half-written: the same call without the
			// NUL goes through, in the same store and under the same lease.
			require.NoError(t, tt.write(w, ""))
		})
	}
}

// A TEXT column refuses a byte that is not UTF-8 as it refuses a NUL. The
// result of a tool call is the one that matters: a tool that read a file in
// another encoding hands such a string back.
func TestNUL_ATextColumnRefusesAByteThatIsNotUTF8(t *testing.T) {
	w := newNULWorld(t)
	text := "caf\xff"

	err := w.k.store.UpdateStep(w.k.ctx, w.lease, agent.StepUpdate{
		Seq: 2, From: agent.StepProposed, To: agent.StepCompleted, Result: &text, Now: w.k.tick(),
	})

	var refusal *pgconn.PgError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, refusedText, refusal.Code)
	assert.Equal(t, agent.StepProposed, w.k.steps(w.run.ID)[1].Status)
}

func TestNUL_OnceRefusesOneInAKey(t *testing.T) {
	tx := begin(t, newDatabase(t).pool(t))

	_, err := agentpg.Once(t.Context(), tx, "run-1:2"+nul)

	var refusal *pgconn.PgError
	require.True(t, errors.As(err, &refusal))
	assert.Equal(t, refusedText, refusal.Code)
}
