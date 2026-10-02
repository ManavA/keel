package llm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/llm"
)

func TestEstimateInputTokens(t *testing.T) {
	tests := []struct {
		name string
		req  llm.Request
		want int64
	}{
		{name: "an empty request", req: llm.Request{}, want: 0},
		{name: "three bytes of system prompt are one token", req: llm.Request{System: "abc"}, want: 1},
		{name: "a fourth byte rounds up to a second", req: llm.Request{System: "abcd"}, want: 2},
		{
			name: "a message costs eight before its text",
			req:  llm.Request{Messages: []llm.Message{{Role: llm.RoleUser}}},
			want: 8,
		},
		{
			name: "message text",
			req:  llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: strings.Repeat("a", 30)}}},
			want: 8 + 10,
		},
		{
			name: "tool arguments",
			req: llm.Request{Messages: []llm.Message{{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read", Input: json.RawMessage(`{"id":"ab"}`)}},
			}}},
			want: 8 + 4,
		},
		{
			name: "tool results",
			req: llm.Request{Messages: []llm.Message{{
				Role:        llm.RoleTool,
				ToolResults: []llm.ToolResult{{CallID: "call_1", Content: strings.Repeat("r", 9)}, {CallID: "call_2", Content: "abc"}},
			}}},
			want: 8 + 4,
		},
		{
			name: "a tool definition: its name, description and schema",
			req: llm.Request{Tools: []llm.Tool{{
				Name: "read", Description: "Reads.", Schema: json.RawMessage(`{"type":"object"}`),
			}}},
			want: (4 + 6 + 17 + 2) / 3,
		},
		{
			name: "an output schema: its name, description and schema",
			req: llm.Request{Output: &llm.Schema{
				Name: "answer", Description: "It.", JSON: json.RawMessage(`{"type":"object"}`),
			}},
			want: (6 + 3 + 17 + 2) / 3,
		},
		{
			// 2 + 4 + 2 + 4 bytes round up once, over the sum, to 4 tokens:
			// rounding each part would give 6.
			name: "the bytes are summed before they are rounded",
			req: llm.Request{
				System: "ab",
				Messages: []llm.Message{
					{Role: llm.RoleUser, Text: "abcd"},
					{Role: llm.RoleAssistant, Text: "ab"},
				},
				Tools: []llm.Tool{{Name: "abcd"}},
			},
			want: 16 + 4,
		},
		{
			// A provider that replays its own form sends thinking blocks and
			// signatures the text and tool calls do not show.
			name: "a provider's form larger than the message's fields is what is counted",
			req: llm.Request{Messages: []llm.Message{{
				Role:   llm.RoleAssistant,
				Text:   "abc",
				Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(strings.Repeat("o", 30))},
			}}},
			want: 8 + 10,
		},
		{
			// Another provider builds the turn from the fields instead.
			name: "a provider's form smaller than the message's fields is not",
			req: llm.Request{Messages: []llm.Message{{
				Role:      llm.RoleAssistant,
				Text:      strings.Repeat("a", 18),
				ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read", Input: json.RawMessage(`{"id":"abcd"}`)}},
				Opaque:    &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(`[]`)},
			}}},
			want: 8 + (18+13+2)/3,
		},
		{
			name: "the two forms of one turn are never both counted",
			req: llm.Request{Messages: []llm.Message{{
				Role:   llm.RoleAssistant,
				Text:   strings.Repeat("a", 30),
				Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(strings.Repeat("o", 30))},
			}}},
			want: 8 + 10,
		},
		{
			name: "the larger form is chosen for each message on its own",
			req: llm.Request{Messages: []llm.Message{
				{
					Role:   llm.RoleAssistant,
					Text:   "abc",
					Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(strings.Repeat("o", 30))},
				},
				{
					Role:   llm.RoleAssistant,
					Text:   strings.Repeat("a", 30),
					Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(`[]`)},
				},
			}},
			want: 16 + 20,
		},
		{
			name: "a provider's form with no data counts as the fields",
			req: llm.Request{Messages: []llm.Message{{
				Role:   llm.RoleAssistant,
				Text:   strings.Repeat("a", 30),
				Opaque: &llm.Opaque{Provider: "anthropic"},
			}}},
			want: 8 + 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, llm.EstimateInputTokens(tt.req))
		})
	}
}

// A budget refuses a call by this estimate, so growing a request must never
// make it look cheaper.
func TestEstimateInputTokens_NeverFallsAsARequestGrows(t *testing.T) {
	bases := []struct {
		name string
		req  llm.Request
	}{
		{name: "an empty request", req: llm.Request{}},
		{name: "a request one byte short of a token boundary", req: llm.Request{System: "ab"}},
		{name: "a request on a token boundary", req: llm.Request{System: "abc"}},
		{
			name: "a conversation with tools and a schema",
			req: llm.Request{
				System: "You review documents.",
				Messages: []llm.Message{
					{Role: llm.RoleUser, Text: "Review document 7."},
					{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read", Input: json.RawMessage(`{"id":7}`)}}},
					{Role: llm.RoleTool, ToolResults: []llm.ToolResult{{CallID: "c1", Content: "the text"}}},
				},
				Tools:  []llm.Tool{{Name: "read", Description: "Reads.", Schema: json.RawMessage(`{"type":"object"}`)}},
				Output: &llm.Schema{JSON: json.RawMessage(`{"type":"object"}`)},
			},
		},
	}

	// mustRise marks a growth of three bytes or more, or of a whole message,
	// which adds at least one token wherever the request stood. Without it
	// an estimate that never moved would pass.
	growths := []struct {
		name     string
		grow     func(llm.Request) llm.Request
		mustRise bool
	}{
		{
			name: "a message",
			grow: func(r llm.Request) llm.Request {
				r.Messages = append(append([]llm.Message(nil), r.Messages...), llm.Message{Role: llm.RoleUser, Text: "x"})
				return r
			},
			mustRise: true,
		},
		{
			name: "an empty message",
			grow: func(r llm.Request) llm.Request {
				r.Messages = append(append([]llm.Message(nil), r.Messages...), llm.Message{Role: llm.RoleUser})
				return r
			},
			mustRise: true,
		},
		{
			// Thinking blocks and their signatures: what a replayed turn
			// costs beyond its text. This is the case a budget's worst case
			// must not come out low for.
			name: "an assistant turn whose provider's form is larger than its text",
			grow: func(r llm.Request) llm.Request {
				r.Messages = append(append([]llm.Message(nil), r.Messages...), llm.Message{
					Role:   llm.RoleAssistant,
					Text:   "ok",
					Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(strings.Repeat("o", 3000))},
				})
				return r
			},
			mustRise: true,
		},
		{
			name: "a provider's form put on every message already there",
			grow: func(r llm.Request) llm.Request {
				messages := append([]llm.Message(nil), r.Messages...)
				for i := range messages {
					messages[i].Opaque = &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(`[{"type":"text"}]`)}
				}
				r.Messages = messages
				return r
			},
		},
		{
			name: "a tool with a one-byte name",
			grow: func(r llm.Request) llm.Request {
				r.Tools = append(append([]llm.Tool(nil), r.Tools...), llm.Tool{Name: "t"})
				return r
			},
		},
		{
			name: "a tool with a schema",
			grow: func(r llm.Request) llm.Request {
				r.Tools = append(append([]llm.Tool(nil), r.Tools...),
					llm.Tool{Name: "save", Description: "Saves.", Schema: json.RawMessage(`{"type":"object"}`)})
				return r
			},
			mustRise: true,
		},
		{
			name: "an output schema in place of none or of a smaller one",
			grow: func(r llm.Request) llm.Request {
				r.Output = &llm.Schema{Name: "answer", JSON: json.RawMessage(`{"type":"object","properties":{}}`)}
				return r
			},
			mustRise: true,
		},
	}

	for _, base := range bases {
		for _, growth := range growths {
			t.Run(base.name+" plus "+growth.name, func(t *testing.T) {
				before := llm.EstimateInputTokens(base.req)
				after := llm.EstimateInputTokens(growth.grow(base.req))

				assert.GreaterOrEqual(t, after, before)
				if growth.mustRise {
					assert.Greater(t, after, before)
				}
			})
		}
	}
}
