package agenttest_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// conversation builds the request a run sends once it holds assistantTurns
// replies: the input, then each reply followed by its results.
func conversation(agentName string, assistantTurns int) agent.Request {
	req := agent.Request{
		RunID:    "run-1",
		Agent:    agentName,
		Messages: []agent.Message{{Role: agent.RoleUser, Text: "begin"}},
	}
	for range assistantTurns {
		req.Messages = append(req.Messages,
			agent.Message{Role: agent.RoleAssistant, Calls: []agent.Call{agenttest.Call("call-1", "lookup", `{"id":1}`)}},
			agent.Message{Role: agent.RoleTool, Results: []agent.Result{{CallID: "call-1", Content: "found"}}},
		)
	}
	return req
}

func TestSay(t *testing.T) {
	assert.Equal(t,
		agent.Response{
			Message: agent.Message{Role: agent.RoleAssistant, Text: "all done"},
			Stop:    agent.StopEnd,
		},
		agenttest.Say("all done"))
}

func TestUse(t *testing.T) {
	first := agenttest.Call("call-1", "lookup", `{"id":1}`)
	second := agenttest.Call("call-2", "send", `{"to":"a"}`)

	assert.Equal(t,
		agent.Response{
			Message: agent.Message{Role: agent.RoleAssistant, Calls: []agent.Call{first, second}},
			Stop:    agent.StopToolUse,
		},
		agenttest.Use(first, second))
}

func TestCall(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  agent.Call
	}{
		{
			name:  "arguments are kept as written",
			input: `{"id": 7, "tags": ["a"]}`,
			want:  agent.Call{ID: "call-1", Name: "lookup", Input: json.RawMessage(`{"id": 7, "tags": ["a"]}`)},
		},
		{
			name:  "no arguments are an empty object, which a journal can store",
			input: "",
			want:  agent.Call{ID: "call-1", Name: "lookup", Input: json.RawMessage(`{}`)},
		},
		{
			name:  "arguments that are not JSON make a malformed call holding them as one string",
			input: `{"id":`,
			want:  agent.Call{ID: "call-1", Name: "lookup", Input: json.RawMessage(`"{\"id\":"`), Malformed: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agenttest.Call("call-1", "lookup", tt.input)

			assert.Equal(t, tt.want, got)
			assert.True(t, json.Valid(got.Input), "the input is always something a journal can store")
		})
	}
}

func TestReplies(t *testing.T) {
	script := agenttest.Replies(
		agenttest.Use(agenttest.Call("call-1", "lookup", `{"id":1}`)),
		agenttest.Say("all done"),
	)

	t.Run("plays one reply per turn, in order", func(t *testing.T) {
		first, err := script(agent.Request{}, 0)
		require.NoError(t, err)
		assert.Equal(t, agenttest.Use(agenttest.Call("call-1", "lookup", `{"id":1}`)), first)

		second, err := script(agent.Request{}, 1)
		require.NoError(t, err)
		assert.Equal(t, agenttest.Say("all done"), second)
	})

	t.Run("a turn past the end is an error", func(t *testing.T) {
		_, err := script(agent.Request{}, 2)
		require.Error(t, err)
		assert.NotErrorIs(t, err, agent.ErrPermanent,
			"an exhausted script is retried like any other model failure")
	})

	t.Run("a negative turn is an error", func(t *testing.T) {
		_, err := script(agent.Request{}, -1)
		assert.Error(t, err)
	})

	t.Run("a reply is the caller's to change", func(t *testing.T) {
		first, err := script(agent.Request{}, 0)
		require.NoError(t, err)
		first.Message.Calls[0].Name = "changed"
		first.Message.Calls[0].Input[2] = 'X'

		again, err := script(agent.Request{}, 0)
		require.NoError(t, err)
		assert.Equal(t, agenttest.Use(agenttest.Call("call-1", "lookup", `{"id":1}`)), again)
	})
}

func TestByAgent(t *testing.T) {
	script := agenttest.ByAgent(map[string]agenttest.Script{
		"alpha": agenttest.Replies(agenttest.Say("from alpha")),
		"beta":  agenttest.Replies(agenttest.Say("from beta")),
	})

	tests := []struct {
		name    string
		agent   string
		want    string
		wantErr bool
	}{
		{name: "the first agent's script", agent: "alpha", want: "from alpha"},
		{name: "the second agent's script", agent: "beta", want: "from beta"},
		{name: "an agent with no script is an error", agent: "gamma", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := script(agent.Request{Agent: tt.agent}, 0)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.agent)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Message.Text)
		})
	}
}

func TestModel_TheReplyDependsOnTheRequestAlone(t *testing.T) {
	script := func() agenttest.Script {
		return agenttest.Replies(
			agenttest.Use(agenttest.Call("call-1", "lookup", `{"id":1}`)),
			agenttest.Say("all done"),
		)
	}
	ctx := t.Context()

	t.Run("the turn is the number of assistant messages in the request", func(t *testing.T) {
		model := agenttest.NewModel(script())

		// Asked for the second turn first: a model that counted its own calls
		// would play the first reply here.
		second, err := model.Generate(ctx, conversation("alpha", 1))
		require.NoError(t, err)
		assert.Equal(t, "all done", second.Message.Text)

		first, err := model.Generate(ctx, conversation("alpha", 0))
		require.NoError(t, err)
		assert.Equal(t, agent.StopToolUse, first.Stop)
	})

	t.Run("only assistant messages count towards the turn", func(t *testing.T) {
		var turns []int
		model := agenttest.NewModel(func(_ agent.Request, turn int) (agent.Response, error) {
			turns = append(turns, turn)
			return agenttest.Say("ok"), nil
		})
		// Two replies that paused and made no calls, so no tool message
		// follows either: counting anything but assistant turns gives 0.
		paused := agent.Request{Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "begin"},
			{Role: agent.RoleAssistant, Text: "thinking"},
			{Role: agent.RoleAssistant, Text: "still thinking"},
		}}

		_, err := model.Generate(ctx, paused)
		require.NoError(t, err)
		_, err = model.Generate(ctx, agent.Request{})
		require.NoError(t, err)
		assert.Equal(t, []int{2, 0}, turns)
	})

	t.Run("a request asked twice gets the same reply twice", func(t *testing.T) {
		model := agenttest.NewModel(script())

		once, err := model.Generate(ctx, conversation("alpha", 0))
		require.NoError(t, err)
		twice, err := model.Generate(ctx, conversation("alpha", 0))
		require.NoError(t, err)
		assert.Equal(t, once, twice)
	})

	t.Run("a model built afresh answers as the first one did", func(t *testing.T) {
		before, err := agenttest.NewModel(script()).Generate(ctx, conversation("alpha", 1))
		require.NoError(t, err)
		after, err := agenttest.NewModel(script()).Generate(ctx, conversation("alpha", 1))
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})

	t.Run("the script is handed the request and its turn", func(t *testing.T) {
		var gotReq agent.Request
		var gotTurn int
		model := agenttest.NewModel(func(req agent.Request, turn int) (agent.Response, error) {
			gotReq, gotTurn = req, turn
			return agenttest.Say("ok"), nil
		})
		req := conversation("alpha", 3)

		_, err := model.Generate(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, req, gotReq)
		assert.Equal(t, 3, gotTurn)
	})
}

func TestModel_NamesTheModelThatAnswered(t *testing.T) {
	ctx := t.Context()
	named := agenttest.Say("ok")
	named.Model = "model-from-the-script"

	tests := []struct {
		name    string
		reply   agent.Response
		request string
		want    string
	}{
		{name: "the script's own name is kept", reply: named, request: "model-a", want: "model-from-the-script"},
		{name: "otherwise it is the model the request asked for", reply: agenttest.Say("ok"), request: "model-a", want: "model-a"},
		{name: "and scripted when the request names none", reply: agenttest.Say("ok"), request: "", want: "scripted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := agenttest.NewModel(agenttest.Replies(tt.reply))

			got, err := model.Generate(ctx, agent.Request{Model: tt.request})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Model)
		})
	}
}

func TestModel_ReturnsTheScriptsError(t *testing.T) {
	boom := errors.New("model unavailable")
	model := agenttest.NewModel(func(agent.Request, int) (agent.Response, error) {
		return agent.Response{}, boom
	})

	_, err := model.Generate(t.Context(), conversation("alpha", 0))
	require.ErrorIs(t, err, boom)
	assert.Len(t, model.Requests(), 1, "a request that failed was still received")
}

func TestModel_ACancelledContextIsNotAnswered(t *testing.T) {
	model := agenttest.NewModel(agenttest.Replies(agenttest.Say("ok")))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := model.Generate(ctx, conversation("alpha", 0))
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, model.Requests())
}

func TestModel_Requests(t *testing.T) {
	ctx := t.Context()

	t.Run("none before any call", func(t *testing.T) {
		model := agenttest.NewModel(agenttest.Replies(agenttest.Say("ok")))
		assert.Empty(t, model.Requests())
	})

	t.Run("every request, in the order received", func(t *testing.T) {
		model := agenttest.NewModel(func(agent.Request, int) (agent.Response, error) {
			return agenttest.Say("ok"), nil
		})
		sent := []agent.Request{conversation("alpha", 0), conversation("beta", 2), conversation("alpha", 1)}
		for _, req := range sent {
			_, err := model.Generate(ctx, req)
			require.NoError(t, err)
		}
		assert.Equal(t, sent, model.Requests())
	})

	t.Run("a recorded request is not changed by what the caller does next", func(t *testing.T) {
		model := agenttest.NewModel(agenttest.Replies(agenttest.Say("first"), agenttest.Say("ok")))
		req := conversation("alpha", 1)
		req.Tools = []agent.ToolSpec{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
		req.Output = json.RawMessage(`{"type":"string"}`)
		req.Messages[1].Opaque = &agent.Opaque{Provider: "p", Data: json.RawMessage(`{"z":1}`)}
		want := conversation("alpha", 1)
		want.Tools = []agent.ToolSpec{{Name: "lookup", Schema: json.RawMessage(`{"type":"object"}`)}}
		want.Output = json.RawMessage(`{"type":"string"}`)
		want.Messages[1].Opaque = &agent.Opaque{Provider: "p", Data: json.RawMessage(`{"z":1}`)}

		_, err := model.Generate(ctx, req)
		require.NoError(t, err)

		req.Messages[0].Text = "changed"
		req.Messages[1].Calls[0].Name = "changed"
		req.Messages[1].Calls[0].Input[2] = 'X'
		req.Messages[1].Opaque.Data[2] = 'X'
		req.Messages[2].Results[0].Content = "changed"
		req.Tools[0].Name = "changed"
		req.Tools[0].Schema[2] = 'X'
		req.Output[2] = 'X'

		assert.Equal(t, []agent.Request{want}, model.Requests())
	})

	t.Run("the list returned is the caller's to change", func(t *testing.T) {
		model := agenttest.NewModel(agenttest.Replies(agenttest.Say("ok")))
		_, err := model.Generate(ctx, conversation("alpha", 0))
		require.NoError(t, err)

		got := model.Requests()
		got[0].Agent = "changed"
		got[0].Messages[0].Text = "changed"

		assert.Equal(t, []agent.Request{conversation("alpha", 0)}, model.Requests())
	})
}

// Run under -race: two executions of one engine ask the model at once.
func TestModel_IsSafeForConcurrentUse(t *testing.T) {
	const goroutines = 8
	model := agenttest.NewModel(agenttest.ByAgent(map[string]agenttest.Script{
		"alpha": agenttest.Replies(agenttest.Say("from alpha")),
		"beta":  agenttest.Replies(agenttest.Say("from beta")),
	}))
	ctx := t.Context()

	var wg sync.WaitGroup
	replies := make([]agent.Response, goroutines)
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Go(func() {
			name := "alpha"
			if i%2 == 1 {
				name = "beta"
			}
			replies[i], errs[i] = model.Generate(ctx, conversation(name, 0))
			_ = model.Requests()
		})
	}
	wg.Wait()

	for i := range goroutines {
		require.NoError(t, errs[i])
		want := "from alpha"
		if i%2 == 1 {
			want = "from beta"
		}
		assert.Equal(t, want, replies[i].Message.Text)
	}
	assert.Len(t, model.Requests(), goroutines)
}

func TestGuardFunc(t *testing.T) {
	var seen agent.Action
	guard := agenttest.GuardFunc(func(_ context.Context, a agent.Action) (agent.Decision, error) {
		seen = a
		return agent.Decision{Effect: agent.Ask, Rule: "sends need a person"}, nil
	})
	action := agent.Action{Kind: "run", Target: "send"}

	got, err := guard.Decide(t.Context(), action)
	require.NoError(t, err)
	assert.Equal(t, agent.Decision{Effect: agent.Ask, Rule: "sends need a person"}, got)
	assert.Equal(t, action, seen)
}
