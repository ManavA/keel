package agent_test

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// A store refuses raw JSON that is not JSON or not UTF-8, and would refuse
// it again on every retry. So a reply that carries such a thing is made one
// a store can keep before it is journaled: the tests below are of what the
// journal then holds and what the model is told.

func TestExecute_AReplyWhoseArgumentsNoStoreCanKeepBecomesAMalformedCall(t *testing.T) {
	tests := []struct {
		name  string
		input string
		// held is the text the call's Input holds, as one JSON string.
		held string
	}{
		{"not JSON", `{"order":`, `{"order":`},
		{"JSON, and not UTF-8", "{\"note\":\"\xff\"}", "{\"note\":\"�\"}"},
		{"neither JSON nor UTF-8", "\xff\xfe{", "��{"},
		{"a NUL character outside a string", "{\x00}", "{\x00}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				guard := &execGuard{}
				turn := 0
				var requests []agent.Request
				f := newExecFixture(t, execConfig{
					defs:  []agent.Definition{execClerk(calls.tool("lookup", nil), calls.tool("refund", nil))},
					guard: guard,
					model: execModelFunc(func(_ context.Context, req agent.Request) (agent.Response, error) {
						requests = append(requests, req)
						turn++
						if turn > 1 {
							return agenttest.Say("the refund could not be read"), nil
						}
						// As an adapter that passes on whatever its provider
						// sent: the arguments are not checked, and not marked.
						return agent.Response{
							Message: agent.Message{Role: agent.RoleAssistant, Calls: []agent.Call{
								{ID: "call-1", Name: "refund", Input: json.RawMessage(tt.input)},
								{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{"order":7}`)},
							}},
							Stop: agent.StopToolUse,
						}, nil
					}),
				})
				started := f.start("clerk", "refund order 7")

				got := f.execute(started.ID)

				require.Equal(t, agent.StatusCompleted, got.Status, "the reply was kept, and the run went on: %s", got.Error)
				assert.Zero(t, got.Failures)
				steps := f.steps(started.ID)
				require.Equal(t, []string{"1 model completed", "2 refund completed", "3 lookup completed", "4 model completed"},
					execJournal(steps))

				bad := steps[1]
				require.NotNil(t, bad.Call)
				assert.True(t, bad.Call.Malformed)
				var held string
				require.NoError(t, json.Unmarshal(bad.Call.Input, &held), "the arguments are held as one JSON string")
				assert.Equal(t, tt.held, held)
				assert.Equal(t, "arguments were not valid JSON", bad.Result)
				assert.True(t, bad.IsError)
				assert.Zero(t, bad.Attempts, "it never started")
				assert.Empty(t, bad.Decision, "and was put to no guard")
				assert.Empty(t, calls.of("refund"), "a malformed call is never run")

				// The call beside it is untouched, and ran.
				assert.False(t, steps[2].Call.Malformed)
				assert.JSONEq(t, `{"order":7}`, string(steps[2].Call.Input))
				assert.Len(t, calls.of("lookup"), 1)
				require.Len(t, guard.questions(), 1)
				assert.Equal(t, "lookup", guard.questions()[0].Target)

				// The model is sent its own turn back as the journal holds
				// it, and told what became of each call.
				require.Len(t, requests, 2)
				require.Len(t, requests[1].Messages, 3)
				assert.Equal(t, *steps[0].Message, requests[1].Messages[1])
				assert.True(t, requests[1].Messages[1].Calls[0].Malformed)
				assert.Equal(t, []agent.Result{
					{CallID: "call-1", Content: "arguments were not valid JSON", IsError: true},
					{CallID: "call-2", Content: "lookup ok"},
				}, execResults(requests[1]))
			})
		})
	}
}

func TestExecute_AReplysProviderFormIsKeptOnlyWhenAStoreCanKeepIt(t *testing.T) {
	tests := []struct {
		name string
		data string
		kept bool
	}{
		{"valid JSON is kept as it came", `{"blocks":[{"type":"thinking","signature":"abc"}]}`, true},
		{"not JSON is dropped", `{"blocks":`, false},
		{"JSON that is not UTF-8 is dropped", "{\"signature\":\"\xff\"}", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				turn := 0
				var requests []agent.Request
				f := newExecFixture(t, execConfig{
					defs: []agent.Definition{execClerk(calls.tool("lookup", nil))},
					model: execModelFunc(func(_ context.Context, req agent.Request) (agent.Response, error) {
						requests = append(requests, req)
						turn++
						if turn > 1 {
							return agenttest.Say("order 7 is paid"), nil
						}
						resp := agenttest.Use(agenttest.Call("call-1", "lookup", `{"order":7}`))
						resp.Message.Text = "looking it up"
						resp.Message.Opaque = &agent.Opaque{Provider: "vendor", Data: json.RawMessage(tt.data)}
						return resp, nil
					}),
				})
				started := f.start("clerk", "is order 7 paid?")

				got := f.execute(started.ID)

				require.Equal(t, agent.StatusCompleted, got.Status, "the reply was kept, and the run went on: %s", got.Error)
				assert.Zero(t, got.Failures)
				reply := f.steps(started.ID)[0].Message
				require.NotNil(t, reply)
				if tt.kept {
					require.NotNil(t, reply.Opaque)
					assert.Equal(t, "vendor", reply.Opaque.Provider)
					assert.Equal(t, tt.data, string(reply.Opaque.Data), "byte for byte")
				} else {
					assert.Nil(t, reply.Opaque, "a form no store can keep is dropped, not repaired")
				}
				// With or without it, the turn is whole: its text and its
				// call, which ran, and the model is sent it back.
				assert.Equal(t, "looking it up", reply.Text)
				require.Len(t, reply.Calls, 1)
				assert.False(t, reply.Calls[0].Malformed)
				assert.Len(t, calls.of("lookup"), 1)
				require.Len(t, requests, 2)
				assert.Equal(t, *reply, requests[1].Messages[1])
			})
		})
	}
}
