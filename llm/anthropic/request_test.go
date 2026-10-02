package anthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
)

// helloContent is what one user turn saying "Hello" is sent as.
const helloContent = `{"role":"user","content":[{"type":"text","text":"Hello"}]}`

// weatherSchema is the tool schema the tool-use reference uses.
const weatherSchema = `{"type":"object","properties":{"location":{"type":"string","description":"The city and state, e.g. San Francisco, CA"}},"required":["location"]}`

// thinkingTurn is an assistant turn as the API returns one: a thinking block
// with no text, text, and a tool call whose arguments are not in
// alphabetical order. The spacing is uneven and the text holds characters
// encoding/json would rewrite, so a turn that went through a decoder and
// back does not match it byte for byte.
const thinkingTurn = `[ {"type":"thinking","thinking":"","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"} ,` +
	` {"type":"text","text":"Let me check <that> & more: é"},` +
	`{"type":"tool_use","id":"toolu_01A09q90qw90lq917835lq9","name":"get_weather","input":{"unit": "celsius","location":"San Francisco, CA"}} ]`

func ptr[T any](v T) *T { return &v }

func TestClient_RequestBody(t *testing.T) {
	tests := []struct {
		name string
		opts anthropic.Options
		req  llm.Request
		// stream sends the request through Stream.
		stream bool
		// want is the whole body, compared as a JSON value.
		want string
		// wantBeta is the anthropic-beta header, "" for none.
		wantBeta string
	}{
		{
			name: "the model the request names wins",
			opts: anthropic.Options{Model: "claude-sonnet-5-5"},
			req:  llm.Request{Model: "claude-haiku-4-5-20251001", Messages: hello()},
			want: `{"model":"claude-haiku-4-5-20251001","max_tokens":16000,"messages":[` + helloContent + `]}`,
		},
		{
			name: "the model from Options when the request names none",
			opts: anthropic.Options{Model: "claude-sonnet-5-5"},
			req:  llm.Request{Messages: hello()},
			want: `{"model":"claude-sonnet-5-5","max_tokens":16000,"messages":[` + helloContent + `]}`,
		},
		{
			name: "the default model when neither names one",
			req:  llm.Request{Messages: hello()},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `]}`,
		},
		{
			name: "max_tokens from the request",
			opts: anthropic.Options{MaxTokens: 2048},
			req:  llm.Request{Messages: hello(), MaxTokens: 1024},
			want: `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[` + helloContent + `]}`,
		},
		{
			name: "max_tokens from Options when the request sets none",
			opts: anthropic.Options{MaxTokens: 2048},
			req:  llm.Request{Messages: hello()},
			want: `{"model":"claude-opus-5-5","max_tokens":2048,"messages":[` + helloContent + `]}`,
		},
		{
			name:   "max_tokens from the request, streaming",
			opts:   anthropic.Options{MaxTokens: 2048},
			req:    llm.Request{Messages: hello(), MaxTokens: 1024},
			stream: true,
			want:   `{"model":"claude-opus-5-5","max_tokens":1024,"messages":[` + helloContent + `],"stream":true}`,
		},
		{
			name:   "max_tokens from Options, streaming",
			opts:   anthropic.Options{MaxTokens: 2048},
			req:    llm.Request{Messages: hello()},
			stream: true,
			want:   `{"model":"claude-opus-5-5","max_tokens":2048,"messages":[` + helloContent + `],"stream":true}`,
		},
		{
			name:   "max_tokens defaults to 64000 when streaming",
			req:    llm.Request{Messages: hello()},
			stream: true,
			want:   `{"model":"claude-opus-5-5","max_tokens":64000,"messages":[` + helloContent + `],"stream":true}`,
		},
		{
			name: "system is a string",
			req:  llm.Request{System: "You are terse.", Messages: hello()},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"system":"You are terse.","messages":[` + helloContent + `]}`,
		},
		{
			name: "an assistant turn from this provider goes back as it came",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{
					Role: llm.RoleAssistant,
					// The fields disagree with Data on purpose: Data is what
					// is sent.
					Text:      "not this",
					ToolCalls: []llm.ToolCall{{ID: "toolu_other", Name: "other", Input: json.RawMessage(`{}`)}},
					Opaque:    &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(thinkingTurn)},
				},
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":` + thinkingTurn + `}]}`,
		},
		{
			name: "an assistant turn from another provider is rebuilt from text and tool calls",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{
					Role: llm.RoleAssistant,
					Text: "I'll check.",
					ToolCalls: []llm.ToolCall{
						{ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{"unit":"celsius","location":"San Francisco, CA"}`)},
					},
					Opaque: &llm.Opaque{Provider: "openai", Data: json.RawMessage(`{"anything":true}`)},
				},
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":[{"type":"text","text":"I'll check."},` +
				`{"type":"tool_use","id":"call_1","name":"get_weather","input":{"unit":"celsius","location":"San Francisco, CA"}}]}]}`,
		},
		{
			name: "an assistant turn with no provider form: tool calls only, no text block",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "call_1", Name: "first", Input: json.RawMessage(`{"a":1}`)},
						{ID: "call_2", Name: "second", Input: json.RawMessage(`{"b":2}`)},
					},
				},
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":[` +
				`{"type":"tool_use","id":"call_1","name":"first","input":{"a":1}},` +
				`{"type":"tool_use","id":"call_2","name":"second","input":{"b":2}}]}]}`,
		},
		{
			name: "an assistant turn with text only",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant, Text: "Hi."},
				userTurn("Again"),
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":[{"type":"text","text":"Hi."}]}` +
				`,{"role":"user","content":[{"type":"text","text":"Again"}]}]}`,
		},
		{
			name: "a malformed call is sent with empty arguments",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "call_1", Name: "get_weather", Input: json.RawMessage(`"{\"location\": "`), Malformed: true},
					},
				},
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"get_weather","input":{}}]}]}`,
		},
		{
			name: "a call with no arguments is sent with empty arguments",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "call_1", Name: "now"},
						{ID: "call_2", Name: "today", Input: json.RawMessage(`null`)},
					},
				},
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"now","input":{}},` +
				`{"type":"tool_use","id":"call_2","name":"today","input":{}}]}]}`,
		},
		{
			name: "a provider form with nothing in it is rebuilt",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant, Text: "Hi.", Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(`null`)}},
				{Role: llm.RoleAssistant, Text: "Still here.", Opaque: &llm.Opaque{Provider: "anthropic"}},
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":[{"type":"text","text":"Hi."}]}` +
				`,{"role":"assistant","content":[{"type":"text","text":"Still here."}]}]}`,
		},
		{
			// A refused reply, or one the model ended with nothing in it,
			// comes back as an assistant turn with no content. The API
			// refuses an empty content array, so the turn is not sent.
			name: "an assistant turn with nothing in it is left out",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant},
				userTurn("Again"),
				{Role: llm.RoleAssistant, Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(` [ ] `)}},
				{Role: llm.RoleAssistant, Opaque: &llm.Opaque{Provider: "openai", Data: json.RawMessage(`{"anything":true}`)}},
				userTurn("And again"),
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"user","content":[{"type":"text","text":"Again"}]}` +
				`,{"role":"user","content":[{"type":"text","text":"And again"}]}]}`,
		},
		{
			name: "tool results share one user message with nothing else in it",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{
						{ID: "toolu_1", Name: "first", Input: json.RawMessage(`{}`)},
						{ID: "toolu_2", Name: "second", Input: json.RawMessage(`{}`)},
					},
				},
				{
					Role: llm.RoleTool,
					// Text on a tool message is not sent: the reference wants
					// results first and this package sends only results.
					Text: "not sent",
					ToolResults: []llm.ToolResult{
						{CallID: "toolu_1", Content: "15 degrees"},
						{CallID: "toolu_2", Content: "ConnectionError: the service is not available (HTTP 500)", IsError: true},
					},
				},
			}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent +
				`,{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"first","input":{}},` +
				`{"type":"tool_use","id":"toolu_2","name":"second","input":{}}]}` +
				`,{"role":"user","content":[` +
				`{"type":"tool_result","tool_use_id":"toolu_1","content":"15 degrees"},` +
				`{"type":"tool_result","tool_use_id":"toolu_2","content":"ConnectionError: the service is not available (HTTP 500)","is_error":true}]}]}`,
		},
		{
			name: "tools, in the order given, with and without a schema and strict",
			req: llm.Request{
				Messages: hello(),
				Tools: []llm.Tool{
					{Name: "get_weather", Description: "Get the current weather in a given location", Schema: json.RawMessage(weatherSchema), Strict: true},
					{Name: "now"},
					{Name: "get_time", Description: "Get the time", Schema: json.RawMessage(`{"type":"object","properties":{}}`)},
				},
			},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],"tools":[` +
				`{"name":"get_weather","description":"Get the current weather in a given location","input_schema":` + weatherSchema + `,"strict":true},` +
				`{"name":"now","input_schema":{"type":"object"}},` +
				`{"name":"get_time","description":"Get the time","input_schema":{"type":"object","properties":{}}}]}`,
		},
		{
			name: "tool choice none keeps the tools and says not to use them",
			req: llm.Request{
				Messages:   hello(),
				Tools:      []llm.Tool{{Name: "now"}},
				ToolChoice: llm.ToolChoiceNone,
			},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],` +
				`"tools":[{"name":"now","input_schema":{"type":"object"}}],"tool_choice":{"type":"none"}}`,
		},
		{
			name: "tool choice auto sends no tool_choice",
			req: llm.Request{
				Messages:   hello(),
				Tools:      []llm.Tool{{Name: "now"}},
				ToolChoice: llm.ToolChoiceAuto,
			},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],` +
				`"tools":[{"name":"now","input_schema":{"type":"object"}}]}`,
		},
		{
			name: "tool choice none with no tools sends no tool_choice",
			req:  llm.Request{Messages: hello(), ToolChoice: llm.ToolChoiceNone},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `]}`,
		},
		{
			name: "an output schema goes in output_config.format",
			req: llm.Request{
				Messages: hello(),
				Output: &llm.Schema{
					// The Messages API has no place for a name or a description.
					Name:        "contact",
					Description: "A contact",
					JSON:        json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`),
				},
			},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],` +
				`"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}}}}`,
		},
		{
			name: "effort goes in output_config.effort",
			req:  llm.Request{Messages: hello(), Effort: llm.EffortXHigh},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],"output_config":{"effort":"xhigh"}}`,
		},
		{
			name: "effort and an output schema share output_config",
			req: llm.Request{
				Messages: hello(),
				Effort:   llm.EffortLow,
				Output:   &llm.Schema{JSON: json.RawMessage(`{"type":"object"}`)},
			},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],` +
				`"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}}}`,
		},
		{
			name: "temperature is sent when set",
			req:  llm.Request{Messages: hello(), Temperature: ptr(1.0)},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],"temperature":1}`,
		},
		{
			name: "a temperature of zero is still sent",
			req:  llm.Request{Model: "claude-haiku-4-5-20251001", Messages: hello(), Temperature: ptr(0.0)},
			want: `{"model":"claude-haiku-4-5-20251001","max_tokens":16000,"messages":[` + helloContent + `],"temperature":0}`,
		},
		{
			name: "stop sequences",
			req:  llm.Request{Messages: hello(), Stop: []string{"END", "\n\n"}},
			want: `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],"stop_sequences":["END","\n\n"]}`,
		},
		{
			name:     "the refusal fallback sends fallbacks and its beta",
			opts:     anthropic.Options{RefusalFallback: "default"},
			req:      llm.Request{Messages: hello()},
			want:     `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],"fallbacks":"default"}`,
			wantBeta: "server-side-fallback-2026-07-01",
		},
		{
			name: "betas are joined, the caller's first",
			opts: anthropic.Options{
				RefusalFallback: "default",
				Betas:           []string{"thinking-binding-controls-2026-08-01", "context-management-2025-06-27"},
			},
			req:      llm.Request{Messages: hello()},
			want:     `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],"fallbacks":"default"}`,
			wantBeta: "thinking-binding-controls-2026-08-01,context-management-2025-06-27,server-side-fallback-2026-07-01",
		},
		{
			name: "a beta the caller already names is not sent twice",
			opts: anthropic.Options{
				RefusalFallback: "default",
				Betas:           []string{"server-side-fallback-2026-07-01", "context-management-2025-06-27"},
			},
			req:      llm.Request{Messages: hello()},
			want:     `{"model":"claude-opus-5-5","max_tokens":16000,"messages":[` + helloContent + `],"fallbacks":"default"}`,
			wantBeta: "server-side-fallback-2026-07-01,context-management-2025-06-27",
		},
		{
			name: "Extra is merged in last and replaces a field this package set",
			opts: anthropic.Options{Extra: map[string]any{
				"thinking":   map[string]any{"type": "adaptive", "display": "summarized"},
				"metadata":   map[string]any{"user_id": "u_1"},
				"max_tokens": 512,
			}},
			req: llm.Request{Messages: hello(), MaxTokens: 1024},
			want: `{"model":"claude-opus-5-5","max_tokens":512,"messages":[` + helloContent + `],` +
				`"metadata":{"user_id":"u_1"},"thinking":{"type":"adaptive","display":"summarized"}}`,
		},
		{
			name:   "Extra is merged into a streamed request too",
			opts:   anthropic.Options{Extra: map[string]any{"metadata": map[string]any{"user_id": "u_1"}}},
			req:    llm.Request{Messages: hello()},
			stream: true,
			want: `{"model":"claude-opus-5-5","max_tokens":64000,"messages":[` + helloContent + `],` +
				`"stream":true,"metadata":{"user_id":"u_1"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, replyEither(okBody, okStream))
			c := api.client(t, tt.opts)

			var err error
			if tt.stream {
				_, err = c.Stream(context.Background(), tt.req, func(llm.Delta) error { return nil })
			} else {
				_, err = c.Generate(context.Background(), tt.req)
			}
			require.NoError(t, err)

			got := api.last(t)
			assert.JSONEq(t, tt.want, string(got.Body))
			assert.Equal(t, tt.wantBeta, got.Header.Get("anthropic-beta"))
			// JSONEq reads a key written twice as its last value, and what
			// the API makes of one is not documented.
			assertNoKeyTwice(t, got.Body)
		})
	}
}

// assertNoKeyTwice fails when the top-level object of body names a key more
// than once.
func assertNoKeyTwice(t *testing.T, body []byte) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		require.NoError(t, err)
		key, ok := tok.(string)
		require.True(t, ok, "a key is a string")
		assert.False(t, seen[key], "the body names %q twice: %s", key, body)
		seen[key] = true
		var value json.RawMessage
		require.NoError(t, dec.Decode(&value))
	}
}

// The whole point of Opaque is that the provider's own form of a turn comes
// back with its bytes untouched: not decoded, not re-ordered, not re-spaced,
// not re-escaped.
func TestClient_OpaqueIsSentByteForByte(t *testing.T) {
	api := newFakeAPI(t, replyJSON(okBody))
	c := api.client(t, anthropic.Options{})

	_, err := c.Generate(context.Background(), llm.Request{Messages: []llm.Message{
		userTurn("What is the weather?"),
		{Role: llm.RoleAssistant, Opaque: &llm.Opaque{Provider: anthropic.Name, Data: json.RawMessage(thinkingTurn)}},
		{Role: llm.RoleTool, ToolResults: []llm.ToolResult{{CallID: "toolu_01A09q90qw90lq917835lq9", Content: "15 degrees"}}},
	}})
	require.NoError(t, err)

	body := api.last(t).Body
	require.True(t, json.Valid(body), "the body must still be JSON: %s", body)
	want := `{"role":"assistant","content":` + thinkingTurn + `}`
	assert.True(t, bytes.Contains(body, []byte(want)),
		"the assistant turn was not sent as it came\nwant, somewhere in the body:\n%s\nbody:\n%s", want, body)
}

func TestClient_RequestIsTheSameEveryTime(t *testing.T) {
	// A replayed conversation is checked against what was sent before, and
	// the process replaying it is rarely the one that sent it. So the same
	// request must give the same bytes from any Client built the same way,
	// the order of a map included.
	api := newFakeAPI(t, replyJSON(okBody))
	opts := anthropic.Options{Extra: map[string]any{
		"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8,
	}}
	req := llm.Request{System: "You are terse.", Messages: hello(), Tools: []llm.Tool{{Name: "now"}}}

	_, err := api.client(t, opts).Generate(context.Background(), req)
	require.NoError(t, err)
	first := api.last(t).Body
	for range 20 {
		_, err := api.client(t, opts).Generate(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, string(first), string(api.last(t).Body))
	}
}

func TestClient_RequestThatCannotBeSent(t *testing.T) {
	tests := []struct {
		name string
		req  llm.Request
		want string
	}{
		{
			name: "a user turn with no text",
			req:  llm.Request{Messages: []llm.Message{userTurn("Hello"), {Role: llm.RoleAssistant, Text: "Hi."}, userTurn("")}},
			want: "message 2",
		},
		{
			name: "a tool turn with no results",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "now", Input: json.RawMessage(`{}`)}}},
				{Role: llm.RoleTool},
			}},
			want: "message 2",
		},
		{
			name: "a provider form that is JSON and not an array",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant, Text: "Hi.", Opaque: &llm.Opaque{Provider: anthropic.Name, Data: json.RawMessage(`{"type":"text","text":"Hi."}`)}},
			}},
			want: "message 1",
		},
		{
			name: "a provider form that is a JSON string",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant, Opaque: &llm.Opaque{Provider: anthropic.Name, Data: json.RawMessage(`"Hi."`)}},
			}},
			want: "message 1",
		},
		{
			name: "a provider form that is not JSON",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant, Opaque: &llm.Opaque{Provider: anthropic.Name, Data: json.RawMessage(`[{"type":"text"`)}},
			}},
			want: "message 1",
		},
		{
			name: "a role this package does not know",
			req:  llm.Request{Messages: []llm.Message{{Role: "system", Text: "Hello"}}},
			want: `role "system"`,
		},
		{
			name: "a tool choice this package does not know",
			req:  llm.Request{Messages: hello(), Tools: []llm.Tool{{Name: "now"}}, ToolChoice: "any"},
			want: `tool choice "any"`,
		},
		{
			name: "tool arguments that are not JSON",
			req: llm.Request{Messages: []llm.Message{
				userTurn("Hello"),
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "now", Input: json.RawMessage(`{"a":`)}}},
			}},
			want: "message 1",
		},
		{
			name: "an output schema that is not JSON",
			req:  llm.Request{Messages: hello(), Output: &llm.Schema{JSON: json.RawMessage(`{"type":`)}},
			want: "output schema",
		},
		{
			name: "an output schema with nothing in it",
			req:  llm.Request{Messages: hello(), Output: &llm.Schema{Name: "contact"}},
			want: "output schema",
		},
		{
			name: "a temperature that is not a number",
			req:  llm.Request{Messages: hello(), Temperature: ptr(math.NaN())},
			want: "temperature",
		},
		{
			name: "a tool schema that is not JSON",
			req:  llm.Request{Messages: hello(), Tools: []llm.Tool{{Name: "now", Schema: json.RawMessage(`{`)}}},
			want: "tools",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, replyEither(okBody, okStream))
			c := api.client(t, anthropic.Options{})

			// A request this package will not send is an *llm.Error that
			// says what is wrong with it, and is not worth another try.
			check := func(t *testing.T, err error) {
				t.Helper()
				require.Error(t, err)
				var e *llm.Error
				require.ErrorAs(t, err, &e)
				assert.Equal(t, anthropic.Name, e.Provider)
				assert.Zero(t, e.Status)
				assert.Contains(t, e.Message, tt.want)
				assert.False(t, e.Retryable)
				assert.False(t, llm.Retryable(err))
			}
			_, err := c.Generate(context.Background(), tt.req)
			check(t, err)
			_, err = c.Stream(context.Background(), tt.req, func(llm.Delta) error { return nil })
			check(t, err)

			assert.Zero(t, api.count(), "a request that cannot be built is not sent")
		})
	}
}
