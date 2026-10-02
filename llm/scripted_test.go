package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// conversationOf returns a request holding that many finished assistant turns,
// each followed by the tool message that answers it, after one user message.
func conversationOf(assistantTurns int) llm.Request {
	msgs := []llm.Message{{Role: llm.RoleUser, Text: "start"}}
	for i := range assistantTurns {
		id := fmt.Sprintf("call_%d_0", i)
		msgs = append(msgs,
			llm.Message{
				Role:      llm.RoleAssistant,
				Text:      "working",
				ToolCalls: []llm.ToolCall{{ID: id, Name: "read", Input: json.RawMessage(`{"id":1}`)}},
			},
			llm.Message{Role: llm.RoleTool, ToolResults: []llm.ToolResult{{CallID: id, Content: "the text"}}},
		)
	}
	return llm.Request{System: "be brief", Messages: msgs}
}

func TestScripted_AnswersFromTheRequestAlone(t *testing.T) {
	script := llm.Replies(
		llm.Reply{Text: "first", ToolCalls: []llm.ToolCall{{Name: "read", Input: json.RawMessage(`{"id":1}`)}}},
		llm.Reply{Text: "second"},
	)
	// The first turn has a call with no id and a default usage, the two things
	// a counter or a clock could leak into.
	reqs := []llm.Request{conversationOf(0), conversationOf(1)}

	t.Run("two instances built from one script", func(t *testing.T) {
		a := llm.NewScripted(script, llm.ScriptedOptions{})
		b := llm.NewScripted(script, llm.ScriptedOptions{})
		for _, req := range reqs {
			fromA, err := a.Generate(t.Context(), req)
			require.NoError(t, err)
			fromB, err := b.Generate(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, fromA, fromB)
		}
	})

	t.Run("one instance asked the same request twice", func(t *testing.T) {
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		for _, req := range reqs {
			first, err := s.Generate(t.Context(), req)
			require.NoError(t, err)
			second, err := s.Generate(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, first, second)
		}
	})

	t.Run("a stream and a generate of one request", func(t *testing.T) {
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		for _, req := range reqs {
			generated, err := s.Generate(t.Context(), req)
			require.NoError(t, err)
			streamed, err := s.Stream(t.Context(), req, func(llm.Delta) error { return nil })
			require.NoError(t, err)
			assert.Equal(t, generated, streamed)
		}
	})
}

// The control for the test above: a script that counts its calls instead of
// reading the request does answer the same request differently each time, and
// the comparison in that test sees it.
func TestScripted_ACallCountingScriptAnswersTheSameRequestDifferently(t *testing.T) {
	calls := 0
	s := llm.NewScripted(func(llm.Request, int) (llm.Reply, error) {
		calls++
		return llm.Reply{Text: fmt.Sprintf("call %d", calls)}, nil
	}, llm.ScriptedOptions{})

	first, err := s.Generate(t.Context(), llm.Request{})
	require.NoError(t, err)
	second, err := s.Generate(t.Context(), llm.Request{})
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

func TestScripted_ChoosesTheReplyByAssistantTurns(t *testing.T) {
	script := llm.Replies(llm.Reply{Text: "zero"}, llm.Reply{Text: "one"}, llm.Reply{Text: "two"})
	tests := []struct {
		name string
		req  llm.Request
		want string
	}{
		{name: "no assistant turn", req: conversationOf(0), want: "zero"},
		{name: "one assistant turn", req: conversationOf(1), want: "one"},
		{name: "two assistant turns", req: conversationOf(2), want: "two"},
		{name: "an empty request", req: llm.Request{}, want: "zero"},
		{
			name: "user and tool messages are not turns",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleUser, Text: "a"},
				{Role: llm.RoleUser, Text: "b"},
				{Role: llm.RoleTool, ToolResults: []llm.ToolResult{{CallID: "x", Content: "c"}}},
				{Role: llm.RoleUser, Text: "d"},
			}},
			want: "zero",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := llm.NewScripted(script, llm.ScriptedOptions{})
			resp, err := s.Generate(t.Context(), tt.req)
			require.NoError(t, err)
			assert.Equal(t, tt.want, resp.Message.Text)
			assert.Equal(t, llm.RoleAssistant, resp.Message.Role)
		})
	}
}

func TestScripted_HandsTheScriptTheRequestAndItsTurn(t *testing.T) {
	var gotReq llm.Request
	gotTurn := -1
	s := llm.NewScripted(func(req llm.Request, turn int) (llm.Reply, error) {
		gotReq, gotTurn = req, turn
		return llm.Reply{}, nil
	}, llm.ScriptedOptions{})

	req := conversationOf(2)
	req.Model = "some-model"
	_, err := s.Generate(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, req, gotReq)
	assert.Equal(t, 2, gotTurn)
}

func TestScripted_ATurnPastTheEndIsExhausted(t *testing.T) {
	tests := []struct {
		name   string
		script llm.Script
		req    llm.Request
	}{
		{name: "after the last reply", script: llm.Replies(llm.Reply{Text: "a"}, llm.Reply{Text: "b"}), req: conversationOf(2)},
		{name: "far past it", script: llm.Replies(llm.Reply{Text: "a"}), req: conversationOf(5)},
		{name: "a script with no replies", script: llm.Replies(), req: conversationOf(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := llm.NewScripted(tt.script, llm.ScriptedOptions{})

			resp, err := s.Generate(t.Context(), tt.req)
			assert.ErrorIs(t, err, llm.ErrScriptExhausted)
			assert.Nil(t, resp)

			deltas := 0
			resp, err = s.Stream(t.Context(), tt.req, func(llm.Delta) error { deltas++; return nil })
			assert.ErrorIs(t, err, llm.ErrScriptExhausted)
			assert.Nil(t, resp)
			assert.Zero(t, deltas)
		})
	}

	t.Run("the error says which turn", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: "a"}), llm.ScriptedOptions{})
		_, err := s.Generate(t.Context(), conversationOf(1))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "turn 1")
	})
}

func TestScripted_ReturnsTheErrorItWasGiven(t *testing.T) {
	boom := errors.New("boom")
	overloaded := &llm.Error{Provider: "anthropic", Status: 529, Type: "overloaded_error", Retryable: true}

	tests := []struct {
		name   string
		script llm.Script
		want   error
	}{
		{name: "Reply.Err", script: llm.Replies(llm.Reply{Text: "ignored", Err: boom}), want: boom},
		{name: "Reply.Err that is a provider error", script: llm.Replies(llm.Reply{Err: overloaded}), want: overloaded},
		{
			name: "an error from the script itself",
			script: func(llm.Request, int) (llm.Reply, error) {
				return llm.Reply{Text: "ignored"}, boom
			},
			want: boom,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := llm.NewScripted(tt.script, llm.ScriptedOptions{})

			resp, err := s.Generate(t.Context(), llm.Request{})
			assert.Same(t, tt.want, err)
			assert.Nil(t, resp)

			deltas := 0
			resp, err = s.Stream(t.Context(), llm.Request{}, func(llm.Delta) error { deltas++; return nil })
			assert.Same(t, tt.want, err)
			assert.Nil(t, resp)
			assert.Zero(t, deltas, "a failed call delivers nothing")
		})
	}

	t.Run("a scripted provider error keeps its retryability", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Err: overloaded}), llm.ScriptedOptions{})
		_, err := s.Generate(t.Context(), llm.Request{})
		assert.True(t, llm.Retryable(err))
	})
}

func TestRoute(t *testing.T) {
	byModel := map[string]llm.Script{
		"big":   llm.Replies(llm.Reply{Text: "from big"}),
		"small": llm.Replies(llm.Reply{Text: "from small"}),
		"":      llm.Replies(llm.Reply{Text: "from the default"}),
	}
	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "a model the map names", model: "small", want: "from small"},
		{name: "another model the map names", model: "big", want: "from big"},
		{name: "a model it does not name falls to the empty entry", model: "other", want: "from the default"},
		{name: "a request that names none uses the empty entry", model: "", want: "from the default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := llm.NewScripted(llm.Route(byModel), llm.ScriptedOptions{})
			resp, err := s.Generate(t.Context(), llm.Request{Model: tt.model})
			require.NoError(t, err)
			assert.Equal(t, tt.want, resp.Message.Text)
		})
	}

	t.Run("the chosen script is given the same turn", func(t *testing.T) {
		route := llm.Route(map[string]llm.Script{"big": llm.Replies(llm.Reply{Text: "a"}, llm.Reply{Text: "b"})})
		s := llm.NewScripted(route, llm.ScriptedOptions{})
		req := conversationOf(1)
		req.Model = "big"
		resp, err := s.Generate(t.Context(), req)
		require.NoError(t, err)
		assert.Equal(t, "b", resp.Message.Text)
	})

	t.Run("a model with no entry and no empty entry is exhausted", func(t *testing.T) {
		route := llm.Route(map[string]llm.Script{"big": llm.Replies(llm.Reply{Text: "a"})})
		s := llm.NewScripted(route, llm.ScriptedOptions{})
		_, err := s.Generate(t.Context(), llm.Request{Model: "other"})
		require.ErrorIs(t, err, llm.ErrScriptExhausted)
		assert.Contains(t, err.Error(), `"other"`)
	})

	t.Run("a nil map has no scripts", func(t *testing.T) {
		s := llm.NewScripted(llm.Route(nil), llm.ScriptedOptions{})
		_, err := s.Generate(t.Context(), llm.Request{})
		assert.ErrorIs(t, err, llm.ErrScriptExhausted)
	})
}

func TestScripts_AreNotChangedByTheirCallersAfterwards(t *testing.T) {
	t.Run("Route and its map", func(t *testing.T) {
		byModel := map[string]llm.Script{"big": llm.Replies(llm.Reply{Text: "from big"})}
		route := llm.Route(byModel)
		byModel["big"] = llm.Replies(llm.Reply{Text: "replaced"})
		byModel["added"] = llm.Replies(llm.Reply{Text: "added"})

		s := llm.NewScripted(route, llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{Model: "big"})
		require.NoError(t, err)
		assert.Equal(t, "from big", resp.Message.Text)
		_, err = s.Generate(t.Context(), llm.Request{Model: "added"})
		assert.ErrorIs(t, err, llm.ErrScriptExhausted)
	})

	t.Run("Replies and its slice", func(t *testing.T) {
		replies := []llm.Reply{{Text: "first"}}
		script := llm.Replies(replies...)
		replies[0] = llm.Reply{Text: "replaced"}

		s := llm.NewScripted(script, llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)
		assert.Equal(t, "first", resp.Message.Text)
	})
}

func TestScripted_ToolCalls(t *testing.T) {
	t.Run("a call with no id is named by its turn and index", func(t *testing.T) {
		script := llm.Replies(
			llm.Reply{},
			llm.Reply{},
			llm.Reply{ToolCalls: []llm.ToolCall{
				{Name: "read", Input: json.RawMessage(`{"id":1}`)},
				{ID: "mine", Name: "read", Input: json.RawMessage(`{"id":2}`)},
				{Name: "save", Input: json.RawMessage(`{"id":3}`)},
			}},
		)
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), conversationOf(2))
		require.NoError(t, err)

		require.Len(t, resp.Message.ToolCalls, 3)
		assert.Equal(t, "call_2_0", resp.Message.ToolCalls[0].ID)
		assert.Equal(t, "mine", resp.Message.ToolCalls[1].ID, "an id the script set is kept")
		assert.Equal(t, "call_2_2", resp.Message.ToolCalls[2].ID)
	})

	t.Run("arguments and the malformed flag come through as written", func(t *testing.T) {
		malformed := json.RawMessage(`"{\"id\":"`)
		script := llm.Replies(llm.Reply{ToolCalls: []llm.ToolCall{
			{Name: "read", Input: json.RawMessage(`{"b":2,"a":1}`)},
			{Name: "read", Input: malformed, Malformed: true},
		}})
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)

		calls := resp.Message.ToolCalls
		assert.Equal(t, `{"b":2,"a":1}`, string(calls[0].Input), "key order is the script's, not sorted")
		assert.False(t, calls[0].Malformed)
		assert.Equal(t, malformed, calls[1].Input)
		assert.True(t, calls[1].Malformed)
	})

	t.Run("a call with no arguments gets an empty object", func(t *testing.T) {
		script := llm.Replies(llm.Reply{ToolCalls: []llm.ToolCall{
			{Name: "list_documents"},
			{Name: "list_documents", Input: json.RawMessage{}},
		}})
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)

		for _, c := range resp.Message.ToolCalls {
			assert.Equal(t, `{}`, string(c.Input))
		}
		_, err = json.Marshal(resp.Message)
		assert.NoError(t, err, "the reply can be journaled as it is")
	})

	t.Run("a reply with no calls has none, not an empty list", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: "done"}), llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)
		assert.Nil(t, resp.Message.ToolCalls)
	})

	t.Run("changing a response does not change the script", func(t *testing.T) {
		script := llm.Replies(llm.Reply{ToolCalls: []llm.ToolCall{{Name: "read", Input: json.RawMessage(`{"id":1}`)}}})
		s := llm.NewScripted(script, llm.ScriptedOptions{})

		first, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)
		first.Message.ToolCalls[0].Input[2] = 'X'
		first.Message.ToolCalls[0].Name = "other"

		second, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)
		assert.Equal(t, "read", second.Message.ToolCalls[0].Name)
		assert.Equal(t, `{"id":1}`, string(second.Message.ToolCalls[0].Input))
	})
}

func TestScripted_StopReason(t *testing.T) {
	call := []llm.ToolCall{{Name: "read", Input: json.RawMessage(`{}`)}}
	tests := []struct {
		name  string
		reply llm.Reply
		want  llm.StopReason
	}{
		{name: "text only defaults to end", reply: llm.Reply{Text: "done"}, want: llm.StopEnd},
		{name: "nothing at all defaults to end", reply: llm.Reply{}, want: llm.StopEnd},
		{name: "tool calls default to tool use", reply: llm.Reply{ToolCalls: call}, want: llm.StopToolUse},
		{name: "text and tool calls default to tool use", reply: llm.Reply{Text: "x", ToolCalls: call}, want: llm.StopToolUse},
		{name: "a stop set on a text reply is kept", reply: llm.Reply{Text: "cut", Stop: llm.StopMaxTokens}, want: llm.StopMaxTokens},
		{name: "a stop set on a call is kept", reply: llm.Reply{ToolCalls: call, Stop: llm.StopPause}, want: llm.StopPause},
		{name: "a context window stop", reply: llm.Reply{Stop: llm.StopContextWindow}, want: llm.StopContextWindow},
		{name: "a stop sequence", reply: llm.Reply{Text: "x", Stop: llm.StopSequence}, want: llm.StopSequence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := llm.NewScripted(llm.Replies(tt.reply), llm.ScriptedOptions{})
			resp, err := s.Generate(t.Context(), llm.Request{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, resp.Stop)
			assert.Nil(t, resp.Refusal)
		})
	}

	t.Run("a refusal carries a Refusal, as Response says it does", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Stop: llm.StopRefusal}), llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err, "a refusal is a reply, not an error")
		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.NotNil(t, resp.Refusal)
	})
}

func TestScripted_DefaultUsage(t *testing.T) {
	tests := []struct {
		name  string
		req   llm.Request
		reply llm.Reply
		// wantOutput is one token per four bytes of text and arguments, plus one.
		wantOutput int64
	}{
		{name: "an empty reply costs the one", reply: llm.Reply{}, wantOutput: 1},
		{name: "three bytes of text", reply: llm.Reply{Text: "abc"}, wantOutput: 1},
		{name: "four bytes of text", reply: llm.Reply{Text: "abcd"}, wantOutput: 2},
		{name: "ten bytes of text", reply: llm.Reply{Text: "abcdefghij"}, wantOutput: 3},
		{
			name: "tool arguments count",
			reply: llm.Reply{ToolCalls: []llm.ToolCall{
				{Name: "read", Input: json.RawMessage(`{"id":"abcdef"}`)}, // 15 bytes
			}},
			wantOutput: 4,
		},
		{
			name:       "text and arguments are added",
			reply:      llm.Reply{Text: "abcd", ToolCalls: []llm.ToolCall{{Name: "read", Input: json.RawMessage(`{"id":1}`)}}}, // 4 + 8
			wantOutput: 4,
		},
		{
			name:       "a call with no arguments counts as the empty object it is sent as",
			reply:      llm.Reply{Text: "abcd", ToolCalls: []llm.ToolCall{{Name: "read"}}}, // 4 + 2
			wantOutput: 2,
		},
		{name: "a long request", req: conversationOf(4), reply: llm.Reply{Text: "ok"}, wantOutput: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := llm.NewScripted(func(llm.Request, int) (llm.Reply, error) { return tt.reply, nil }, llm.ScriptedOptions{})
			resp, err := s.Generate(t.Context(), tt.req)
			require.NoError(t, err)

			assert.Equal(t, llm.Usage{InputTokens: llm.EstimateInputTokens(tt.req), OutputTokens: tt.wantOutput}, resp.Usage)

			again, err := s.Generate(t.Context(), tt.req)
			require.NoError(t, err)
			assert.Equal(t, resp.Usage, again.Usage, "the same request and reply cost the same every time")
		})
	}

	t.Run("input is the estimate for the request", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{}), llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{System: "abc"})
		require.NoError(t, err)
		assert.Equal(t, int64(1), resp.Usage.InputTokens)
	})

	t.Run("a longer request costs more to send", func(t *testing.T) {
		s := llm.NewScripted(func(llm.Request, int) (llm.Reply, error) { return llm.Reply{}, nil }, llm.ScriptedOptions{})
		short, err := s.Generate(t.Context(), conversationOf(1))
		require.NoError(t, err)
		long, err := s.Generate(t.Context(), conversationOf(3))
		require.NoError(t, err)
		assert.Greater(t, long.Usage.InputTokens, short.Usage.InputTokens)
	})

	t.Run("a usage the script sets is used as it is", func(t *testing.T) {
		set := llm.Usage{OutputTokens: 7, CacheReadTokens: 3}
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: "abcdefgh", Usage: set}), llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{System: "abc"})
		require.NoError(t, err)
		assert.Equal(t, set, resp.Usage, "no input estimate is added to a usage that was given")
	})
}

func TestScripted_ResponseModel(t *testing.T) {
	tests := []struct {
		name  string
		opts  llm.ScriptedOptions
		model string
		want  string
	}{
		{name: "no name, none asked for", want: "scripted"},
		{name: "the name option when the request names none", opts: llm.ScriptedOptions{Name: "house"}, want: "house"},
		{name: "the model the request names", model: "claude-haiku-4-5-20251001", want: "claude-haiku-4-5-20251001"},
		{
			name: "the request's model wins over the name option",
			opts: llm.ScriptedOptions{Name: "house"}, model: "claude-haiku-4-5-20251001", want: "claude-haiku-4-5-20251001",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := llm.NewScripted(llm.Replies(llm.Reply{Text: "x"}), tt.opts)
			resp, err := s.Generate(t.Context(), llm.Request{Model: tt.model})
			require.NoError(t, err)
			assert.Equal(t, tt.want, resp.Model)
			assert.Empty(t, resp.Attempts)
			assert.Nil(t, resp.Message.Opaque, "a scripted turn has no provider form")
		})
	}
}

func TestScripted_Stream(t *testing.T) {
	reply := llm.Reply{
		Text: "One two  three\nfour",
		ToolCalls: []llm.ToolCall{
			{Name: "read", Input: json.RawMessage(`{"id":1}`)},
			{ID: "mine", Name: "save", Input: json.RawMessage(`{"id":2}`)},
		},
	}

	collect := func(t *testing.T, s *llm.Scripted, req llm.Request) ([]llm.Delta, *llm.Response) {
		t.Helper()
		var got []llm.Delta
		resp, err := s.Stream(t.Context(), req, func(d llm.Delta) error {
			got = append(got, d)
			return nil
		})
		require.NoError(t, err)
		return got, resp
	}

	t.Run("text a word at a time, then each call whole", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(reply), llm.ScriptedOptions{})
		got, _ := collect(t, s, llm.Request{})

		want := []llm.Delta{
			{Text: "One "},
			{Text: "two  "},
			{Text: "three\n"},
			{Text: "four"},
			{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_0_0", Name: "read", InputJSON: `{"id":1}`}},
			{ToolCall: &llm.ToolCallDelta{Index: 1, ID: "mine", Name: "save", InputJSON: `{"id":2}`}},
		}
		assert.Equal(t, want, got)
	})

	t.Run("the deltas add up to the reply", func(t *testing.T) {
		text := "  leading space, a\ttab,\r\na newline, and a trailing one \n"
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: text}), llm.ScriptedOptions{})
		got, resp := collect(t, s, llm.Request{})

		var b strings.Builder
		for _, d := range got {
			assert.NotEmpty(t, d.Text, "no empty delta")
			b.WriteString(d.Text)
		}
		assert.Equal(t, text, b.String())
		assert.Equal(t, text, resp.Message.Text)
		assert.Greater(t, len(got), 1)
	})

	t.Run("returns what Generate returns", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(reply), llm.ScriptedOptions{})
		_, streamed := collect(t, s, llm.Request{Model: "m"})
		generated, err := s.Generate(t.Context(), llm.Request{Model: "m"})
		require.NoError(t, err)
		assert.Equal(t, generated, streamed)
	})

	t.Run("a reply of tool calls only has no text delta", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{ToolCalls: reply.ToolCalls[:1]}), llm.ScriptedOptions{})
		got, _ := collect(t, s, llm.Request{})
		require.Len(t, got, 1)
		assert.NotNil(t, got[0].ToolCall)
	})

	t.Run("a reply of one word is one delta", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: "done"}), llm.ScriptedOptions{})
		got, _ := collect(t, s, llm.Request{})
		assert.Equal(t, []llm.Delta{{Text: "done"}}, got)
	})

	t.Run("a reply with nothing in it has no delta", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{}), llm.ScriptedOptions{})
		got, resp := collect(t, s, llm.Request{})
		assert.Empty(t, got)
		assert.Equal(t, llm.StopEnd, resp.Stop)
	})

	t.Run("a call with no arguments is streamed as an empty object", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{ToolCalls: []llm.ToolCall{{Name: "list_documents"}}}), llm.ScriptedOptions{})
		got, _ := collect(t, s, llm.Request{})
		require.Len(t, got, 1)
		assert.Equal(t, `{}`, got[0].ToolCall.InputJSON)
	})

	t.Run("a malformed call is streamed as the text the model wrote", func(t *testing.T) {
		malformed := llm.ToolCall{Name: "read", Input: json.RawMessage(`"{\"id\":"`), Malformed: true}
		s := llm.NewScripted(llm.Replies(llm.Reply{ToolCalls: []llm.ToolCall{malformed}}), llm.ScriptedOptions{})
		got, resp := collect(t, s, llm.Request{})

		require.Len(t, got, 1)
		assert.Equal(t, `{"id":`, got[0].ToolCall.InputJSON)
		assert.True(t, resp.Message.ToolCalls[0].Malformed)
		assert.Equal(t, malformed.Input, resp.Message.ToolCalls[0].Input)
	})

	t.Run("an error from fn stops the stream and is returned", func(t *testing.T) {
		stop := errors.New("client went away")
		s := llm.NewScripted(llm.Replies(reply), llm.ScriptedOptions{})
		calls := 0
		resp, err := s.Stream(t.Context(), llm.Request{}, func(llm.Delta) error {
			calls++
			if calls == 2 {
				return stop
			}
			return nil
		})
		assert.Same(t, stop, err)
		assert.Nil(t, resp)
		assert.Equal(t, 2, calls, "nothing is delivered after the error")
	})

	t.Run("an error from fn on a tool call delta stops there", func(t *testing.T) {
		stop := errors.New("client went away")
		s := llm.NewScripted(llm.Replies(reply), llm.ScriptedOptions{})
		var got []llm.Delta
		_, err := s.Stream(t.Context(), llm.Request{}, func(d llm.Delta) error {
			got = append(got, d)
			if d.ToolCall != nil {
				return stop
			}
			return nil
		})
		assert.Same(t, stop, err)
		require.Len(t, got, 5)
		assert.Equal(t, 0, got[4].ToolCall.Index)
	})

	t.Run("a context cancelled between deltas ends the stream", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s := llm.NewScripted(llm.Replies(reply), llm.ScriptedOptions{})
		calls := 0
		resp, err := s.Stream(ctx, llm.Request{}, func(llm.Delta) error {
			calls++
			cancel()
			return nil
		})
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, resp)
		assert.Equal(t, 1, calls)
	})
}

func TestScripted_ACancelledContextEndsTheCallAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	asked := 0
	s := llm.NewScripted(func(llm.Request, int) (llm.Reply, error) {
		asked++
		return llm.Reply{Text: "x"}, nil
	}, llm.ScriptedOptions{})

	resp, err := s.Generate(ctx, llm.Request{})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, resp)

	deltas := 0
	resp, err = s.Stream(ctx, llm.Request{}, func(llm.Delta) error { deltas++; return nil })
	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, resp)

	assert.False(t, llm.Retryable(err), "a cancelled call is not worth retrying")
	assert.Zero(t, asked, "the script is not asked")
	assert.Zero(t, deltas)
	assert.Empty(t, s.Requests(), "a call that never started is not a request received")
}

func TestScripted_Requests(t *testing.T) {
	build := func() llm.Request {
		temperature := 0.5
		return llm.Request{
			Model:  "m",
			System: "be brief",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Text: "start"},
				{
					Role:      llm.RoleAssistant,
					Text:      "working",
					ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read", Input: json.RawMessage(`{"id":1}`)}},
					Opaque:    &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(`{"content":[]}`)},
				},
				{Role: llm.RoleTool, ToolResults: []llm.ToolResult{{CallID: "c1", Content: "text"}}},
			},
			Tools:       []llm.Tool{{Name: "read", Schema: json.RawMessage(`{"type":"object"}`)}},
			Output:      &llm.Schema{Name: "answer", JSON: json.RawMessage(`{"type":"object"}`)},
			Temperature: &temperature,
			Stop:        []string{"END"},
		}
	}
	script := func(llm.Request, int) (llm.Reply, error) { return llm.Reply{Text: "ok"}, nil }

	t.Run("none before the first call", func(t *testing.T) {
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		assert.Empty(t, s.Requests())
	})

	t.Run("every request, in order, whether the call worked or not", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: "a"}), llm.ScriptedOptions{})
		first := llm.Request{System: "one"}
		second := conversationOf(1) // past the end of the script, so the call fails
		third := llm.Request{System: "three"}

		_, err := s.Generate(t.Context(), first)
		require.NoError(t, err)
		_, err = s.Generate(t.Context(), second)
		require.ErrorIs(t, err, llm.ErrScriptExhausted)
		_, err = s.Stream(t.Context(), third, func(llm.Delta) error { return nil })
		require.NoError(t, err)

		assert.Equal(t, []llm.Request{first, second, third}, s.Requests(), "a stream is one request")
	})

	t.Run("a request holds everything the caller sent", func(t *testing.T) {
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		_, err := s.Generate(t.Context(), build())
		require.NoError(t, err)
		assert.Equal(t, []llm.Request{build()}, s.Requests())
	})

	t.Run("what the caller changes afterwards does not change the record", func(t *testing.T) {
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		req := build()
		_, err := s.Generate(t.Context(), req)
		require.NoError(t, err)

		req.Messages[0].Text = "changed"
		req.Messages[1].ToolCalls[0].Input[2] = 'X'
		req.Messages[1].Opaque.Data[2] = 'X'
		req.Messages[2].ToolResults[0].Content = "changed"
		req.Tools[0].Schema[2] = 'X'
		req.Output.JSON[2] = 'X'
		req.Output.Name = "changed"
		req.Stop[0] = "changed"
		*req.Temperature = 9

		assert.Equal(t, []llm.Request{build()}, s.Requests())
	})

	t.Run("what the caller does to the slice it was given does not change the record", func(t *testing.T) {
		s := llm.NewScripted(script, llm.ScriptedOptions{})
		_, err := s.Generate(t.Context(), build())
		require.NoError(t, err)

		got := s.Requests()
		got[0].System = "changed"
		got[0].Messages[0].Text = "changed"
		got[0].Messages[1].ToolCalls[0].Input[2] = 'X'
		got[0].Messages[1].Opaque.Data[2] = 'X'
		got[0].Tools[0].Schema[2] = 'X'
		got[0].Output.JSON[2] = 'X'
		*got[0].Temperature = 9
		got[0].Stop[0] = "changed"

		assert.Equal(t, []llm.Request{build()}, s.Requests())
	})
}

func TestScripted_IsSafeForConcurrentUse(t *testing.T) {
	const callers = 40
	s := llm.NewScripted(func(req llm.Request, _ int) (llm.Reply, error) {
		return llm.Reply{Text: "answer to " + req.System}, nil
	}, llm.ScriptedOptions{})

	var wg sync.WaitGroup
	for i := range callers {
		system := fmt.Sprintf("caller %d", i)
		wg.Go(func() {
			if i%2 == 0 {
				_, err := s.Generate(t.Context(), llm.Request{System: system})
				assert.NoError(t, err)
				return
			}
			_, err := s.Stream(t.Context(), llm.Request{System: system}, func(llm.Delta) error { return nil })
			assert.NoError(t, err)
		})
		wg.Go(func() {
			// Reading the record while calls are being added to it.
			for _, r := range s.Requests() {
				assert.NotEmpty(t, r.System)
			}
		})
	}
	wg.Wait()

	seen := map[string]int{}
	for _, r := range s.Requests() {
		seen[r.System]++
	}
	assert.Len(t, seen, callers, "every caller's request was recorded")
	for system, n := range seen {
		assert.Equal(t, 1, n, system)
	}
}
