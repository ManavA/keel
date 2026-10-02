package agent_test

import (
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// A run keeps the prompt and the tools it started with. A deploy in the
// middle of it registers the agent again with another prompt, a tool removed
// and a tool added, and the run goes on as it began: the model is sent what
// it was sent before, since a conversation continued under another prompt or
// other tools is one no model was ever asked.
func TestSnapshot_ARunGoesOnWithThePromptAndToolsItStartedWith(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		guard := &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}}
		schema := func(field string) json.RawMessage {
			return json.RawMessage(`{"type":"object","required":["` + field + `"]}`)
		}
		described := func(name, description, field string) agent.Tool {
			tool := calls.tool(name, nil)
			tool.Description, tool.Schema = description, schema(field)
			return tool
		}
		lookup := described("lookup", "look an order up", "order")
		refund := described("refund", "refund an order", "order")

		// The old build. The run makes its first call, which a person is
		// asked about, and parks.
		before := agent.Definition{
			Name: "clerk", System: "be exact", Model: "model-a", MaxTokens: 512,
			Tools: []agent.Tool{refund, lookup},
		}
		old := newExecFixture(t, execConfig{
			defs:  []agent.Definition{before},
			guard: guard,
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "lookup", `{"order":7}`)),
				agenttest.Use(agenttest.Call("call-2", "refund", `{"order":7}`)),
				agenttest.Use(agenttest.Call("call-3", "refund", `{"order":8}`)),
				agenttest.Say("order 7 was found; no refund could be made"),
			),
		})
		started := old.start("clerk", "refund order 7")
		parked := old.execute(started.ID)
		require.Equal(t, agent.StatusWaiting, parked.Status)
		require.Equal(t, []string{"1 model completed", "2 lookup completed", "3 model completed", "4 refund waiting"},
			old.journal(started.ID))
		_, err := old.engine.Approve(t.Context(), old.approvals(started.ID)[0].ID, "ops@example.test", "")
		require.NoError(t, err)

		// The new build: another prompt and model, refund removed, audit
		// added, and lookup described differently.
		after := agent.Definition{
			Name: "clerk", System: "be brief", Model: "model-b", MaxTokens: 64,
			Tools: []agent.Tool{
				described("audit", "write to the audit log", "entry"),
				described("lookup", "look anything up", "id"),
			},
		}
		deployed := old.rival(execConfig{defs: []agent.Definition{after}, guard: guard})

		got := deployed.execute(started.ID)

		require.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "order 7 was found; no refund could be made", got.Output)

		// Every request of the run, from either build, is under the prompt,
		// the model, the bound and the tools the run started with.
		wantTools := []agent.ToolSpec{
			{Name: "lookup", Description: "look an order up", Schema: schema("order")},
			{Name: "refund", Description: "refund an order", Schema: schema("order")},
		}
		requests := old.model.Requests()
		require.Len(t, requests, 4, "two from the old build and two from the new")
		for i, req := range requests {
			assert.Equal(t, "be exact", req.System, "request %d", i+1)
			assert.Equal(t, "model-a", req.Model, "request %d", i+1)
			assert.Equal(t, 512, req.MaxTokens, "request %d", i+1)
			assert.Equal(t, wantTools, req.Tools, "request %d: the removed tool is still offered, and the added one is not", i+1)
			if i > 0 {
				earlier := requests[i-1].Messages
				require.GreaterOrEqual(t, len(req.Messages), len(earlier))
				assert.Equal(t, earlier, req.Messages[:len(earlier)], "request %d starts with the one before it", i+1)
			}
		}

		// A call to the removed tool is answered and not run: the one a
		// person approved before the deploy, and the one the model made after
		// it. The guard is not asked about a tool this build cannot run.
		steps := old.steps(started.ID)
		require.Equal(t, []string{
			"1 model completed", "2 lookup completed", "3 model completed", "4 refund completed",
			"5 model completed", "6 refund completed", "7 model completed",
		}, execJournal(steps))
		for _, seq := range []int{4, 6} {
			st := steps[seq-1]
			assert.Equal(t, "tool is not available", st.Result, "step %d", seq)
			assert.True(t, st.IsError, "step %d", seq)
		}
		assert.Equal(t, 1, steps[3].Attempts, "the approved call was started, and then answered")
		assert.Zero(t, steps[5].Attempts, "the call made after the deploy never started")
		assert.Empty(t, calls.of("refund"))
		assert.Len(t, calls.of("lookup"), 1)
		assert.Empty(t, calls.of("audit"))
		asked := guard.questions()
		require.Len(t, asked, 2, "lookup, and the first refund, by the old build; nothing by the new")
		assert.Equal(t, []agent.Result{{CallID: "call-3", Content: "tool is not available", IsError: true}},
			execResults(requests[3]))

		// The snapshot on the run is what it was. A run started now takes the
		// new one.
		assert.Equal(t, started.Definition, got.Definition)
		assert.Equal(t, "be exact", got.Definition.System)
		fresh, err := deployed.engine.Start(t.Context(), agent.StartRequest{Agent: "clerk", Input: "hello"})
		require.NoError(t, err)
		assert.Equal(t, "be brief", fresh.Definition.System)
		assert.Equal(t, []string{"audit", "lookup"}, []string{fresh.Definition.Tools[0].Name, fresh.Definition.Tools[1].Name})
	})
}
