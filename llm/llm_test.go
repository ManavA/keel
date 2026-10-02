package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// These literals are the wire form a journal stores and an adapter copies
// field for field, so they are compared as bytes: a renamed tag, a reordered
// field or a dropped omitempty fails here.
func TestMessage_JSON(t *testing.T) {
	tests := []struct {
		name string
		msg  llm.Message
		want string
	}{
		{
			name: "user text",
			msg:  llm.Message{Role: llm.RoleUser, Text: "summarise the batch"},
			want: `{"role":"user","text":"summarise the batch"}`,
		},
		{
			name: "assistant turn that only calls a tool",
			msg: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "read_document", Input: json.RawMessage(`{"id":7}`)}},
			},
			want: `{"role":"assistant","tool_calls":[{"id":"call_1","name":"read_document","input":{"id":7}}]}`,
		},
		{
			name: "malformed call carries its arguments as one string",
			msg: llm.Message{
				Role: llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{
					ID: "call_1", Name: "read_document", Input: json.RawMessage(`"{\"id\":"`), Malformed: true,
				}},
			},
			want: `{"role":"assistant","tool_calls":[{"id":"call_1","name":"read_document","input":"{\"id\":","malformed":true}]}`,
		},
		{
			name: "tool results, one failed",
			msg: llm.Message{
				Role: llm.RoleTool,
				ToolResults: []llm.ToolResult{
					{CallID: "call_1", Content: "the text"},
					{CallID: "call_2", Content: "no such document", IsError: true},
				},
			},
			want: `{"role":"tool","tool_results":[{"call_id":"call_1","content":"the text"},` +
				`{"call_id":"call_2","content":"no such document","is_error":true}]}`,
		},
		{
			name: "every field at once",
			msg: llm.Message{
				Role:        llm.RoleAssistant,
				Text:        "reading it now",
				ToolCalls:   []llm.ToolCall{{ID: "call_1", Name: "read_document", Input: json.RawMessage(`{"id":7}`)}},
				ToolResults: []llm.ToolResult{{CallID: "call_0", Content: "ok"}},
				Opaque: &llm.Opaque{
					Provider: "anthropic",
					Data:     json.RawMessage(`[{"type":"thinking","thinking":"","signature":"sig"}]`),
				},
			},
			want: `{"role":"assistant","text":"reading it now",` +
				`"tool_calls":[{"id":"call_1","name":"read_document","input":{"id":7}}],` +
				`"tool_results":[{"call_id":"call_0","content":"ok"}],` +
				`"opaque":{"provider":"anthropic","data":[{"type":"thinking","thinking":"","signature":"sig"}]}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.msg)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))

			var back llm.Message
			require.NoError(t, json.Unmarshal(got, &back))
			assert.Equal(t, tt.msg, back, "the wire form must decode to the message it came from")
		})
	}
}

// A provider compares the values of a replayed turn, and a journal that
// reordered them would still be a different record from the one received.
func TestMessage_JSONKeepsRawKeyOrder(t *testing.T) {
	msg := llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "save", Input: json.RawMessage(`{"z":1,"a":2}`)}},
		Opaque:    &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(`[{"z":1,"a":2}]`)},
	}

	encoded, err := json.Marshal(msg)
	require.NoError(t, err)

	var back llm.Message
	require.NoError(t, json.Unmarshal(encoded, &back))
	assert.Equal(t, `{"z":1,"a":2}`, string(back.ToolCalls[0].Input))
	assert.Equal(t, `[{"z":1,"a":2}]`, string(back.Opaque.Data))
}

func TestWireForms_JSON(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "usage with every count",
			value: llm.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, ReasoningTokens: 5},
			want:  `{"input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_write_tokens":40,"reasoning_tokens":5}`,
		},
		{
			name:  "zero usage keeps the two counts every call has",
			value: llm.Usage{},
			want:  `{"input_tokens":0,"output_tokens":0}`,
		},
		{
			name: "tool",
			value: llm.Tool{
				Name: "read_document", Description: "Read one document.",
				Schema: json.RawMessage(`{"type":"object"}`), Strict: true,
			},
			want: `{"name":"read_document","description":"Read one document.","schema":{"type":"object"},"strict":true}`,
		},
		{
			name:  "tool with only a name",
			value: llm.Tool{Name: "list_documents"},
			want:  `{"name":"list_documents"}`,
		},
		{
			name:  "schema",
			value: llm.Schema{Name: "answer", Description: "The digest.", JSON: json.RawMessage(`{"type":"object"}`)},
			want:  `{"name":"answer","description":"The digest.","schema":{"type":"object"}}`,
		},
		{
			name:  "refusal",
			value: llm.Refusal{Category: "cyber", Explanation: "declined"},
			want:  `{"category":"cyber","explanation":"declined"}`,
		},
		{
			name:  "attempt",
			value: llm.Attempt{Model: "model-a", Usage: llm.Usage{InputTokens: 1, OutputTokens: 2}},
			want:  `{"model":"model-a","usage":{"input_tokens":1,"output_tokens":2}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.value)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

// The values of these constants are written into journals and compared by
// adapters, so each is pinned to the word the design gives it.
func TestConstants_WireValues(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "RoleUser", got: string(llm.RoleUser), want: "user"},
		{name: "RoleAssistant", got: string(llm.RoleAssistant), want: "assistant"},
		{name: "RoleTool", got: string(llm.RoleTool), want: "tool"},
		{name: "ToolChoiceAuto", got: string(llm.ToolChoiceAuto), want: ""},
		{name: "ToolChoiceNone", got: string(llm.ToolChoiceNone), want: "none"},
		{name: "EffortLow", got: string(llm.EffortLow), want: "low"},
		{name: "EffortMedium", got: string(llm.EffortMedium), want: "medium"},
		{name: "EffortHigh", got: string(llm.EffortHigh), want: "high"},
		{name: "EffortXHigh", got: string(llm.EffortXHigh), want: "xhigh"},
		{name: "EffortMax", got: string(llm.EffortMax), want: "max"},
		{name: "StopEnd", got: string(llm.StopEnd), want: "end"},
		{name: "StopToolUse", got: string(llm.StopToolUse), want: "tool_use"},
		{name: "StopMaxTokens", got: string(llm.StopMaxTokens), want: "max_tokens"},
		{name: "StopSequence", got: string(llm.StopSequence), want: "stop_sequence"},
		{name: "StopRefusal", got: string(llm.StopRefusal), want: "refusal"},
		{name: "StopPause", got: string(llm.StopPause), want: "pause"},
		{name: "StopContextWindow", got: string(llm.StopContextWindow), want: "context_window"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

func TestUsage_Total(t *testing.T) {
	tests := []struct {
		name  string
		usage llm.Usage
		want  int64
	}{
		{name: "zero", usage: llm.Usage{}, want: 0},
		{name: "input and output", usage: llm.Usage{InputTokens: 100, OutputTokens: 40}, want: 140},
		{
			name:  "cache reads and writes are billed too",
			usage: llm.Usage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 1000, CacheWriteTokens: 200},
			want:  1340,
		},
		{
			name:  "reasoning is already inside output and is not counted twice",
			usage: llm.Usage{InputTokens: 100, OutputTokens: 40, ReasoningTokens: 30},
			want:  140,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.usage.Total())
		})
	}
}

func TestUsage_Add(t *testing.T) {
	a := llm.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5}
	b := llm.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, ReasoningTokens: 50}

	assert.Equal(t,
		llm.Usage{InputTokens: 11, OutputTokens: 22, CacheReadTokens: 33, CacheWriteTokens: 44, ReasoningTokens: 55},
		a.Add(b))
	assert.Equal(t, a, a.Add(llm.Usage{}), "adding nothing changes nothing")
	assert.Equal(t, llm.Usage{InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5}, a,
		"Add returns the sum and leaves its receiver alone")
}
