package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
)

// generate asks a fake that answers body for one reply.
func generate(t *testing.T, body string) *llm.Response {
	t.Helper()
	api := newFakeAPI(t, replyJSON(body))
	resp, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})
	require.NoError(t, err)
	require.NotNil(t, resp)
	return resp
}

func TestClient_Generate_Text(t *testing.T) {
	// message.json is the reference's own example reply, which carries a
	// citation, a container and every usage field.
	body := fixture(t, "message.json")
	resp := generate(t, body)

	assert.Equal(t, "msg_013Zva2CMHLNnXjNJJKqJ2EF", resp.ID)
	assert.Equal(t, "claude-opus-5-5", resp.Model)
	assert.Equal(t, llm.RoleAssistant, resp.Message.Role)
	assert.Equal(t, "Hi! My name is Claude.", resp.Message.Text)
	assert.Empty(t, resp.Message.ToolCalls)
	assert.Empty(t, resp.Message.ToolResults)
	assert.Equal(t, llm.StopEnd, resp.Stop)
	// The example carries stop_details beside end_turn. Only a refusal has
	// a Refusal.
	assert.Nil(t, resp.Refusal)
	assert.Empty(t, resp.Attempts)

	require.NotNil(t, resp.Message.Opaque)
	assert.Equal(t, "anthropic", resp.Message.Opaque.Provider)
	assert.Equal(t, string(contentOf(t, body)), string(resp.Message.Opaque.Data),
		"Opaque is the content array as the bytes received")
}

func TestClient_Generate_ToolCall(t *testing.T) {
	body := fixture(t, "tool_use.json")
	resp := generate(t, body)

	assert.Equal(t, "I'll check the current weather in San Francisco for you.", resp.Message.Text)
	assert.Equal(t, llm.StopToolUse, resp.Stop)
	require.Len(t, resp.Message.ToolCalls, 1)
	call := resp.Message.ToolCalls[0]
	assert.Equal(t, "toolu_01A09q90qw90lq917835lq9", call.ID)
	assert.Equal(t, "get_weather", call.Name)
	assert.False(t, call.Malformed)
	// The arguments are the bytes received: "unit" stays before "location",
	// and the spacing is the server's.
	assert.Equal(t, `{ "unit": "celsius", "location": "San Francisco, CA" }`, string(call.Input))
	assert.Equal(t, string(contentOf(t, body)), string(resp.Message.Opaque.Data))
}

func TestClient_Generate_ToolCallWithNoArguments(t *testing.T) {
	tests := []struct {
		name  string
		block string
	}{
		{name: "an empty object", block: `{"type":"tool_use","id":"toolu_1","name":"now","input":{}}`},
		{name: "null", block: `{"type":"tool_use","id":"toolu_1","name":"now","input":null}`},
		{name: "no input at all", block: `{"type":"tool_use","id":"toolu_1","name":"now"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := generate(t, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5",`+
				`"content":[`+tt.block+`],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)

			require.Len(t, resp.Message.ToolCalls, 1)
			assert.Equal(t, `{}`, string(resp.Message.ToolCalls[0].Input))
			_, err := json.Marshal(resp.Message)
			require.NoError(t, err, "the message must survive a journal's json.Marshal")
		})
	}
}

func TestClient_Generate_ThinkingBlockIsKept(t *testing.T) {
	// A thinking block whose text is empty still carries a signature the next
	// request must send back. It shows in Opaque and nowhere else.
	body := fixture(t, "thinking.json")
	resp := generate(t, body)

	assert.Equal(t, "I'll check the current weather in San Francisco for you.", resp.Message.Text)
	require.Len(t, resp.Message.ToolCalls, 1)
	require.NotNil(t, resp.Message.Opaque)
	assert.Equal(t, string(contentOf(t, body)), string(resp.Message.Opaque.Data))

	var blocks []struct {
		Type      string `json:"type"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(resp.Message.Opaque.Data, &blocks))
	require.Len(t, blocks, 4)
	assert.Equal(t, "thinking", blocks[0].Type)
	assert.Equal(t, "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds", blocks[0].Signature)
	assert.Equal(t, "redacted_thinking", blocks[1].Type)
	assert.NotEmpty(t, blocks[1].Data)
}

func TestClient_Generate_UnknownBlockTypeIsKept(t *testing.T) {
	const content = `[{"type":"text","text":"One."},{"type":"a_block_from_the_future","payload":{"k":[1,2,3]}},{"type":"text","text":" Two."}]`
	resp := generate(t, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5",`+
		`"content":`+content+`,"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)

	assert.Equal(t, "One. Two.", resp.Message.Text, "text blocks are concatenated in order")
	assert.Equal(t, content, string(resp.Message.Opaque.Data))
}

func TestClient_Generate_StopReasons(t *testing.T) {
	tests := []struct {
		wire string
		want llm.StopReason
	}{
		{wire: "end_turn", want: llm.StopEnd},
		{wire: "tool_use", want: llm.StopToolUse},
		{wire: "max_tokens", want: llm.StopMaxTokens},
		{wire: "stop_sequence", want: llm.StopSequence},
		{wire: "pause_turn", want: llm.StopPause},
		{wire: "refusal", want: llm.StopRefusal},
		{wire: "model_context_window_exceeded", want: llm.StopContextWindow},
		// A reason added after this package was written is passed through, so
		// that it is never mistaken for a reply that ended normally.
		{wire: "a_reason_from_the_future", want: llm.StopReason("a_reason_from_the_future")},
	}
	for _, tt := range tests {
		t.Run(tt.wire, func(t *testing.T) {
			resp := generate(t, fmt.Sprintf(`{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5",`+
				`"content":[{"type":"text","text":"ok"}],"stop_reason":%q,"stop_sequence":null,"stop_details":null,`+
				`"usage":{"input_tokens":1,"output_tokens":1}}`, tt.wire))
			assert.Equal(t, tt.want, resp.Stop)
			if tt.want == llm.StopRefusal {
				assert.NotNil(t, resp.Refusal, "Refusal is set whenever Stop is StopRefusal")
			} else {
				assert.Nil(t, resp.Refusal)
			}
		})
	}
}

func TestClient_Generate_Refusal(t *testing.T) {
	t.Run("with a category", func(t *testing.T) {
		resp := generate(t, fixture(t, "refusal.json"))

		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.Equal(t, &llm.Refusal{
			Category:    "cyber",
			Explanation: "This request was declined because it could enable cyber harm.",
		}, resp.Refusal)
		assert.Empty(t, resp.Message.Text)
		assert.Empty(t, resp.Message.ToolCalls)
		assert.Equal(t, llm.Usage{InputTokens: 412}, resp.Usage)
	})

	t.Run("without a category", func(t *testing.T) {
		resp := generate(t, fixture(t, "refusal_no_category.json"))

		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.Equal(t, &llm.Refusal{}, resp.Refusal)
	})
}

func TestClient_Generate_Usage(t *testing.T) {
	// usage.json gives every count a different value, so two fields read
	// from each other's place cannot agree by accident.
	resp := generate(t, fixture(t, "usage.json"))

	assert.Equal(t, llm.Usage{
		InputTokens:      2095,
		OutputTokens:     503,
		CacheReadTokens:  1803,
		CacheWriteTokens: 248,
		ReasoningTokens:  87,
	}, resp.Usage)
	assert.Empty(t, resp.Attempts)
}

func TestClient_Generate_UsageWithNullCounts(t *testing.T) {
	// The reference types the two cache counts "number or null".
	resp := generate(t, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5",`+
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
		`"usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":null,"cache_read_input_tokens":null,"output_tokens_details":null}}`)
	assert.Equal(t, llm.Usage{InputTokens: 7, OutputTokens: 3}, resp.Usage)
}

func TestClient_Generate_Fallback(t *testing.T) {
	// fallback.json is the reference's example of a refusal before any
	// output that a second model then answered.
	body := fixture(t, "fallback.json")
	resp := generate(t, body)

	assert.Equal(t, "claude-sonnet-5-5", resp.Model, "the model that answered")
	assert.Equal(t, "Hi! How can I help you today?", resp.Message.Text)
	assert.Equal(t, llm.StopEnd, resp.Stop)
	assert.Nil(t, resp.Refusal)
	assert.Equal(t, llm.Usage{InputTokens: 412, OutputTokens: 264, CacheReadTokens: 96, CacheWriteTokens: 31}, resp.Usage,
		"Usage is what the answering model was billed")
	assert.Equal(t, []llm.Attempt{
		{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535}},
		{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 412, OutputTokens: 264, CacheReadTokens: 96, CacheWriteTokens: 31}},
	}, resp.Attempts)

	// Nothing comes before the fallback block here, so the echo rule keeps
	// every block.
	assert.JSONEq(t, string(contentOf(t, body)), string(resp.Message.Opaque.Data))
}

func TestClient_Generate_AttemptWithNoModel(t *testing.T) {
	// Every usage.iterations entry the reference shows names its model. An
	// entry that did not is priced as the model that answered, since an
	// attempt with no model cannot be priced at all.
	resp := generate(t, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5",`+
		`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3,`+
		`"iterations":[{"type":"other","input_tokens":40,"output_tokens":9},{"type":"message","model":"claude-opus-5-5","input_tokens":7,"output_tokens":3}]}}`)

	assert.Equal(t, []llm.Attempt{
		{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 40, OutputTokens: 9}},
		{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}},
	}, resp.Attempts)
	cost, err := llm.Prices{"claude-opus-5-5": {Input: 4_000_000, Output: 20_000_000}}.CostOf(resp)
	require.NoError(t, err)
	assert.Equal(t, int64(40*4+9*20+7*4+3*20), cost)
}

func TestClient_Generate_FallbackEchoRule(t *testing.T) {
	const (
		thinkingA   = `{"type":"thinking","thinking":"","signature":"sigA"}`
		redacted    = `{"type":"redacted_thinking","data":"cipher"}`
		connector   = `{"type":"connector_text","text":"Looking."}`
		textA       = `{"type":"text","text":"Before."}`
		toolA       = `{"type":"tool_use","id":"toolu_A","name":"first","input":{"a":1}}`
		serverDone  = `{"type":"server_tool_use","id":"srvtoolu_1","name":"code_execution","input":{"code":"print(1)"}}`
		serverOut   = `{"type":"code_execution_tool_result","tool_use_id":"srvtoolu_1","content":{"type":"code_execution_result","content":[],"return_code":0,"stderr":"","stdout":"1\n"}}`
		serverOpen  = `{"type":"server_tool_use","id":"srvtoolu_2","name":"code_execution","input":{"code":"print(2)"}}`
		unknown     = `{"type":"a_block_from_the_future","payload":1}`
		fallbackAB  = `{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-sonnet-5-5"}}`
		fallbackBC  = `{"type":"fallback","from":{"model":"claude-sonnet-5-5"},"to":{"model":"claude-haiku-4-5-20251001"}}`
		thinkingB   = `{"type":"thinking","thinking":"","signature":"sigB"}`
		textB       = `{"type":"text","text":" Between."}`
		toolB       = `{"type":"tool_use","id":"toolu_B","name":"second","input":{"b":2}}`
		thinkingC   = `{"type":"thinking","thinking":"","signature":"sigC"}`
		textC       = `{"type":"text","text":" After."}`
		toolC       = `{"type":"tool_use","id":"toolu_C","name":"third","input":{"c":3}}`
		serverAfter = `{"type":"server_tool_use","id":"srvtoolu_3","name":"code_execution","input":{"code":"print(3)"}}`
	)
	tests := []struct {
		name string
		// content is the blocks the API returned, in order.
		content []string
		// want is the blocks kept for the next request, in order.
		want      []string
		wantText  string
		wantCalls []string
	}{
		{
			name:     "no fallback block: nothing is dropped",
			content:  []string{thinkingA, redacted, connector, textA, toolA, serverOpen},
			want:     []string{thinkingA, redacted, connector, textA, toolA, serverOpen},
			wantText: "Before.", wantCalls: []string{"toolu_A"},
		},
		{
			name:     "thinking, redacted thinking and connector text before the fallback are dropped",
			content:  []string{thinkingA, redacted, connector, textA, fallbackAB, textC},
			want:     []string{textA, fallbackAB, textC},
			wantText: "Before. After.",
		},
		{
			name:     "a client tool call before the fallback is dropped, and is not a call to run",
			content:  []string{textA, toolA, fallbackAB, thinkingC, toolC},
			want:     []string{textA, fallbackAB, thinkingC, toolC},
			wantText: "Before.", wantCalls: []string{"toolu_C"},
		},
		{
			name:     "a server tool call before the fallback is kept with its result and dropped without one",
			content:  []string{serverDone, serverOut, serverOpen, fallbackAB, textC},
			want:     []string{serverDone, serverOut, fallbackAB, textC},
			wantText: " After.",
		},
		{
			name:     "everything after the fallback is kept, whatever it is",
			content:  []string{fallbackAB, thinkingC, redacted, connector, textC, toolC, serverAfter, unknown},
			want:     []string{fallbackAB, thinkingC, redacted, connector, textC, toolC, serverAfter, unknown},
			wantText: " After.", wantCalls: []string{"toolu_C"},
		},
		{
			name:     "a block type this package does not know is kept where it is",
			content:  []string{unknown, thinkingA, fallbackAB, textC},
			want:     []string{unknown, fallbackAB, textC},
			wantText: " After.",
		},
		{
			name:     "with two fallbacks the rule runs up to the last, and the first stays where it was",
			content:  []string{thinkingA, textA, toolA, fallbackAB, thinkingB, textB, toolB, fallbackBC, thinkingC, textC, toolC},
			want:     []string{textA, fallbackAB, textB, fallbackBC, thinkingC, textC, toolC},
			wantText: "Before. Between. After.", wantCalls: []string{"toolu_C"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := generate(t, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5-5",`+
				`"content":`+jsonArray(tt.content)+`,"stop_reason":"end_turn","stop_details":null,`+
				`"usage":{"input_tokens":1,"output_tokens":1}}`)

			require.NotNil(t, resp.Message.Opaque)
			assert.JSONEq(t, jsonArray(tt.want), string(resp.Message.Opaque.Data))
			assert.Equal(t, tt.wantText, resp.Message.Text)
			var calls []string
			for _, c := range resp.Message.ToolCalls {
				calls = append(calls, c.ID)
			}
			assert.Equal(t, tt.wantCalls, calls)
		})
	}
}

// jsonArray joins JSON values into one array.
func jsonArray(values []string) string {
	out := "["
	for i, v := range values {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out + "]"
}

func TestClient_Generate_ReplyThatIsNotAMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "not JSON", body: `<html>captive portal</html>`},
		{name: "an empty body", body: ``},
		{name: "content that is not an array", body: `{"id":"msg_01","content":"text","stop_reason":"end_turn"}`},
		{name: "content that is null", body: `{"id":"msg_01","content":null,"stop_reason":"end_turn"}`},
		{name: "no content", body: `{"id":"msg_01","stop_reason":"end_turn"}`},
		{name: "a block that is not an object", body: `{"id":"msg_01","content":["text"],"stop_reason":"end_turn"}`},
		{name: "an error body under a 200", body: `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, replyJSON(tt.body))
			resp, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})
			require.Error(t, err)
			assert.Nil(t, resp)

			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, anthropic.Name, e.Provider)
			assert.Equal(t, http.StatusOK, e.Status)
			assert.Equal(t, requestID, e.RequestID)
			assert.False(t, e.Retryable)
		})
	}
}

// The case that must fail when the mapping is wrong. A reply holding a
// thinking block is sent back as the next request's assistant turn, and the
// server must receive the content array it sent. A provider that rebuilt the
// turn from Text and ToolCalls would lose the thinking blocks and fail here.
func TestClient_AssistantTurnIsReplayedAsReceived(t *testing.T) {
	body := fixture(t, "thinking.json")
	sent := contentOf(t, body)
	api := newFakeAPI(t, replyJSON(body))
	c := api.client(t, anthropic.Options{})
	ctx := context.Background()

	messages := []llm.Message{userTurn("What is the weather in San Francisco?")}
	first, err := c.Generate(ctx, llm.Request{Messages: messages})
	require.NoError(t, err)
	require.Len(t, first.Message.ToolCalls, 1)

	// The journal stores the message as JSON and reads it back.
	stored, err := json.Marshal(first.Message)
	require.NoError(t, err)
	var restored llm.Message
	require.NoError(t, json.Unmarshal(stored, &restored))

	messages = append(messages, restored, llm.Message{
		Role:        llm.RoleTool,
		ToolResults: []llm.ToolResult{{CallID: first.Message.ToolCalls[0].ID, Content: "15 degrees"}},
	})
	_, err = c.Generate(ctx, llm.Request{Messages: messages})
	require.NoError(t, err)

	var got struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(api.last(t).Body, &got))
	require.Len(t, got.Messages, 3)
	assert.Equal(t, "assistant", got.Messages[1].Role)
	assert.JSONEq(t, string(sent), string(got.Messages[1].Content),
		"the server must receive the content array it sent, thinking blocks included")
	assert.JSONEq(t,
		`[{"type":"tool_result","tool_use_id":"toolu_01A09q90qw90lq917835lq9","content":"15 degrees"}]`,
		string(got.Messages[2].Content))
}

// Without a journal in between, the bytes themselves come back.
func TestClient_AssistantTurnIsReplayedByteForByte(t *testing.T) {
	body := fixture(t, "thinking.json")
	sent := contentOf(t, body)
	api := newFakeAPI(t, replyJSON(body))
	c := api.client(t, anthropic.Options{})
	ctx := context.Background()

	messages := []llm.Message{userTurn("What is the weather in San Francisco?")}
	first, err := c.Generate(ctx, llm.Request{Messages: messages})
	require.NoError(t, err)

	messages = append(messages, first.Message, llm.Message{
		Role:        llm.RoleTool,
		ToolResults: []llm.ToolResult{{CallID: first.Message.ToolCalls[0].ID, Content: "15 degrees"}},
	})
	_, err = c.Generate(ctx, llm.Request{Messages: messages})
	require.NoError(t, err)

	assert.Contains(t, string(api.last(t).Body), `{"role":"assistant","content":`+string(sent)+`}`)
}
