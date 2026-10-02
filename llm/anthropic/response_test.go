package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
				assert.Equal(t, llm.Message{Role: llm.RoleAssistant}, resp.Message, "a refused reply carries no output")
			} else {
				assert.Nil(t, resp.Refusal)
				assert.Equal(t, "ok", resp.Message.Text)
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
		assert.Equal(t, llm.Message{Role: llm.RoleAssistant}, resp.Message)
		assert.Equal(t, "msg_01XFUDYJgAACzvnptvVoYEL", resp.ID)
		assert.Equal(t, "claude-opus-5-5", resp.Model)
	})

	t.Run("without a category", func(t *testing.T) {
		resp := generate(t, fixture(t, "refusal_no_category.json"))

		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.Equal(t, &llm.Refusal{}, resp.Refusal)
		assert.Equal(t, llm.Message{Role: llm.RoleAssistant}, resp.Message)
	})

	t.Run("partial output is not handed on", func(t *testing.T) {
		// The reference: "treat any partial output as incomplete and discard
		// it". A tool call from a turn the model refused must never reach
		// whatever runs tools, and the turn has no form to replay.
		resp := generate(t, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[`+
			`{"type":"thinking","thinking":"","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"},`+
			`{"type":"text","text":"Let me look at"},`+
			`{"type":"tool_use","id":"toolu_01A09q90qw90lq917835lq9","name":"get_weather","input":{"location":"San Francisco, CA"}}],`+
			`"stop_reason":"refusal","stop_sequence":null,`+
			`"stop_details":{"type":"refusal","category":"cyber","explanation":"This request was declined because it could enable cyber harm."},`+
			`"usage":{"input_tokens":412,"output_tokens":37}}`)

		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.Equal(t, "cyber", resp.Refusal.Category)
		assert.Equal(t, llm.Message{Role: llm.RoleAssistant}, resp.Message)
		assert.Empty(t, resp.Message.Text)
		assert.Empty(t, resp.Message.ToolCalls)
		assert.Nil(t, resp.Message.Opaque)
	})
}

// billedPrices prices every token at one millionth of a dollar, so the cost
// of a reply is the number of tokens it was billed for.
func billedPrices() llm.Prices {
	one := llm.Price{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000}
	return llm.Prices{"claude-opus-5-5": one, "claude-sonnet-5-5": one, "claude-haiku-4-5-20251001": one}
}

// What a reply reports as usage is what the reference says is billed. An
// attempt that declined before producing any output is billed only in the
// three categories the refusals reference marks, and is listed with no usage
// otherwise, so the record still shows it ran.
func TestClient_Generate_Billing(t *testing.T) {
	const (
		// One reply, refused before any output.
		refusedHead = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":"refusal","stop_sequence":null,`
		// A fallback block as the beta Messages reference defines it, with
		// the category that made the first model hand over.
		toSonnet = `{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-sonnet-5-5"},"trigger":{"type":"refusal","category":%s}}`
		toHaiku  = `{"type":"fallback","from":{"model":"claude-sonnet-5-5"},"to":{"model":"claude-haiku-4-5-20251001"},"trigger":{"type":"refusal","category":%s}}`
		answer   = `{"type":"text","text":"Hi! How can I help you today?"}`

		opusDeclined   = `{"type":"message","model":"claude-opus-5-5","input_tokens":535,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
		sonnetDeclined = `{"type":"message","model":"claude-sonnet-5-5","input_tokens":500,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`
		sonnetServed   = `{"type":"fallback_message","model":"claude-sonnet-5-5","input_tokens":412,"output_tokens":264,"cache_read_input_tokens":96,"cache_creation_input_tokens":31}`
		haikuServed    = `{"type":"fallback_message","model":"claude-haiku-4-5-20251001","input_tokens":412,"output_tokens":264,"cache_read_input_tokens":96,"cache_creation_input_tokens":31}`
		servedUsage    = `"input_tokens":412,"output_tokens":264,"cache_read_input_tokens":96,"cache_creation_input_tokens":31`
	)
	served := llm.Usage{InputTokens: 412, OutputTokens: 264, CacheReadTokens: 96, CacheWriteTokens: 31}
	fallback := func(model string, content []string, iterations ...string) string {
		return `{"id":"msg_01","type":"message","role":"assistant","model":"` + model + `","content":` + jsonArray(content) +
			`,"stop_reason":"end_turn","stop_details":null,"usage":{` + servedUsage + `,"iterations":` + jsonArray(iterations) + `}}`
	}

	tests := []struct {
		name string
		body string

		wantUsage    llm.Usage
		wantAttempts []llm.Attempt
		// wantBilled is every token the reference says the call is billed
		// for, over all the models that worked on it.
		wantBilled int64
	}{
		{
			name: "a refusal before any output, in a category that is not billed",
			body: refusedHead + `"stop_details":{"type":"refusal","category":"cyber","explanation":"x"},"usage":{"input_tokens":412,"output_tokens":0}}`,
		},
		{
			name: "a refusal before any output, in the other category that is not billed",
			body: refusedHead + `"stop_details":{"type":"refusal","category":"general_harms","explanation":"x"},"usage":{"input_tokens":412,"output_tokens":0}}`,
		},
		{
			name: "a refusal before any output, with no category",
			body: refusedHead + `"stop_details":{"type":"refusal","category":null,"explanation":null},"usage":{"input_tokens":412,"output_tokens":0}}`,
		},
		{
			name:      "a refusal before any output, category bio, is billed",
			body:      refusedHead + `"stop_details":{"type":"refusal","category":"bio","explanation":"x"},"usage":{"input_tokens":412,"output_tokens":0}}`,
			wantUsage: llm.Usage{InputTokens: 412}, wantBilled: 412,
		},
		{
			name:      "a refusal before any output, category frontier_llm, is billed",
			body:      refusedHead + `"stop_details":{"type":"refusal","category":"frontier_llm","explanation":"x"},"usage":{"input_tokens":412,"output_tokens":0}}`,
			wantUsage: llm.Usage{InputTokens: 412}, wantBilled: 412,
		},
		{
			name:      "a refusal before any output, category reasoning_extraction, is billed",
			body:      refusedHead + `"stop_details":{"type":"refusal","category":"reasoning_extraction","explanation":"x"},"usage":{"input_tokens":412,"output_tokens":0}}`,
			wantUsage: llm.Usage{InputTokens: 412}, wantBilled: 412,
		},
		{
			name:      "a refusal after some output is billed whatever its category",
			body:      refusedHead + `"stop_details":{"type":"refusal","category":"cyber","explanation":"x"},"usage":{"input_tokens":412,"output_tokens":37}}`,
			wantUsage: llm.Usage{InputTokens: 412, OutputTokens: 37}, wantBilled: 449,
		},
		{
			name:      "a refusal that does not say why is taken as billed",
			body:      refusedHead + `"stop_details":null,"usage":{"input_tokens":412,"output_tokens":0}}`,
			wantUsage: llm.Usage{InputTokens: 412}, wantBilled: 412,
		},
		{
			// The reference's own example reply carries stop_details beside
			// end_turn. Only a refusal is judged by them.
			name: "stop_details beside another stop reason change nothing",
			body: `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":"end_turn","stop_sequence":null,` +
				`"stop_details":{"type":"refusal","category":"cyber","explanation":"x"},"usage":{"input_tokens":412,"output_tokens":0}}`,
			wantUsage: llm.Usage{InputTokens: 412}, wantBilled: 412,
		},
		{
			name:      "the first model declined before any output, in a category that is not billed",
			body:      fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"cyber"`), answer}, opusDeclined, sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5"},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 803,
		},
		{
			name:      "the first model declined before any output, with no category",
			body:      fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `null`), answer}, opusDeclined, sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5"},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 803,
		},
		{
			name:      "the first model declined before any output, in a billed category",
			body:      fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"bio"`), answer}, opusDeclined, sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 535 + 803,
		},
		{
			name: "the first model declined after some output, which is billed whatever the category",
			body: fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"cyber"`), answer},
				`{"type":"message","model":"claude-opus-5-5","input_tokens":535,"output_tokens":148,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535, OutputTokens: 148}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 683 + 803,
		},
		{
			name: "a declined model that ran a tool loop first produced output, so all of it is billed",
			body: fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"cyber"`), answer},
				`{"type":"message","model":"claude-opus-5-5","input_tokens":300,"output_tokens":40,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				`{"type":"message","model":"claude-opus-5-5","input_tokens":380,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 300, OutputTokens: 40}},
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 380}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 340 + 380 + 803,
		},
		{
			name: "two models declined, each judged by its own category",
			body: fallback("claude-haiku-4-5-20251001",
				[]string{fmt.Sprintf(toSonnet, `"cyber"`), fmt.Sprintf(toHaiku, `"frontier_llm"`), answer},
				opusDeclined, sonnetDeclined, haikuServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5"},
				{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 500}},
				{Model: "claude-haiku-4-5-20251001", Usage: served},
			},
			wantBilled: 500 + 803,
		},
		{
			// The refusals guide prints its fallback block with no trigger.
			// With nothing to say which category it was, the attempt is
			// taken as billed: a cost that may be too high, never too low.
			name: "a fallback block that does not say why is taken as billed",
			body: fallback("claude-sonnet-5-5",
				[]string{`{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-sonnet-5-5"}}`, answer},
				opusDeclined, sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 535 + 803,
		},
		{
			name: "a fallback block whose trigger is null does not say why either",
			body: fallback("claude-sonnet-5-5",
				[]string{`{"type":"fallback","from":{"model":"claude-opus-5-5"},"to":{"model":"claude-sonnet-5-5"},"trigger":null}`, answer},
				opusDeclined, sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 535 + 803,
		},
		{
			// The model that answered sampled twice, in a server-side tool
			// loop. Its entries are one attempt, and the model before it is
			// still the one the fallback block speaks of.
			name: "the answering model's own tool loop is one attempt",
			body: fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"cyber"`), answer},
				opusDeclined,
				`{"type":"message","model":"claude-sonnet-5-5","input_tokens":200,"output_tokens":30,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5"},
				{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 200, OutputTokens: 30}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 230 + 803,
		},
		{
			// A compaction between the two is nobody's turn at the reply: it
			// neither joins the declined attempt nor makes a third.
			name: "a compaction entry beside a fallback is billed and is not an attempt at the reply",
			body: fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"cyber"`), answer},
				opusDeclined, `{"type":"compaction","input_tokens":40,"output_tokens":9}`, sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5"},
				{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 40, OutputTokens: 9}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 49 + 803,
		},
		{
			name: "a compaction entry ahead of the declined attempt changes nothing either",
			body: fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"cyber"`), answer},
				`{"type":"compaction","input_tokens":40,"output_tokens":9}`, opusDeclined, sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 40, OutputTokens: 9}},
				{Model: "claude-opus-5-5"},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 49 + 803,
		},
		{
			// The reference types a message entry's model "Model or null".
			// One with none is read as work of the model that answered, and
			// never as a model that declined for free.
			name: "an entry that names no model is not taken for a model that declined",
			body: fallback("claude-sonnet-5-5", []string{fmt.Sprintf(toSonnet, `"cyber"`), answer},
				`{"type":"message","model":null,"input_tokens":535,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`,
				sonnetServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 535}},
				{Model: "claude-sonnet-5-5", Usage: served},
			},
			wantBilled: 535 + 803,
		},
		{
			// Sticky routing: a later turn goes straight to the fallback
			// model, with no fallback block and nothing declined.
			name:         "a turn routed straight to the fallback model",
			body:         fallback("claude-sonnet-5-5", []string{answer}, sonnetServed),
			wantUsage:    served,
			wantAttempts: []llm.Attempt{{Model: "claude-sonnet-5-5", Usage: served}},
			wantBilled:   803,
		},
		{
			name: "models and fallback blocks that do not line up: nothing is taken as free",
			body: fallback("claude-haiku-4-5-20251001", []string{fmt.Sprintf(toHaiku, `"cyber"`), answer},
				opusDeclined, sonnetDeclined, haikuServed),
			wantUsage: served,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535}},
				{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 500}},
				{Model: "claude-haiku-4-5-20251001", Usage: served},
			},
			wantBilled: 535 + 500 + 803,
		},
		{
			name: "every model declined: the last refusal is the reply, each attempt judged by its own category",
			body: `{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[` + fmt.Sprintf(toSonnet, `"bio"`) + `],` +
				`"stop_reason":"refusal","stop_details":{"type":"refusal","category":"cyber","explanation":"x"},` +
				`"usage":{"input_tokens":412,"output_tokens":0,"iterations":[` + opusDeclined + `,` +
				`{"type":"fallback_message","model":"claude-sonnet-5-5","input_tokens":412,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}]}}`,
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535}},
				{Model: "claude-sonnet-5-5"},
			},
			wantBilled: 535,
		},
		{
			// A compaction entry names no model and is billed besides the
			// counts at the top level, which the beta reference says do not
			// include it.
			name: "a compaction entry is billed to the model that answered",
			body: `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"ok"}],` +
				`"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3,"iterations":[` +
				`{"type":"compaction","input_tokens":40,"output_tokens":9},` +
				`{"type":"message","model":"claude-opus-5-5","input_tokens":7,"output_tokens":3}]}}`,
			wantUsage: llm.Usage{InputTokens: 7, OutputTokens: 3},
			wantAttempts: []llm.Attempt{
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 40, OutputTokens: 9}},
				{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}},
			},
			wantBilled: 59,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := generate(t, tt.body)

			assert.Equal(t, tt.wantUsage, resp.Usage)
			assert.Equal(t, tt.wantAttempts, resp.Attempts)
			cost, err := billedPrices().CostOf(resp)
			require.NoError(t, err)
			assert.Equal(t, tt.wantBilled, cost, "the cost of the reply is what the reference says is billed")
		})
	}
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
	// The first model declined before any output, in a category the
	// reference does not bill. It is listed, with nothing against it.
	assert.Equal(t, []llm.Attempt{
		{Model: "claude-opus-5-5"},
		{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 412, OutputTokens: 264, CacheReadTokens: 96, CacheWriteTokens: 31}},
	}, resp.Attempts)
	cost, err := billedPrices().CostOf(resp)
	require.NoError(t, err)
	assert.Equal(t, int64(412+264+96+31), cost)

	// Nothing comes before the fallback block here, so the echo rule keeps
	// every block.
	assert.JSONEq(t, string(contentOf(t, body)), string(resp.Message.Opaque.Data))
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

// An error object in a body that came with a 200 is still the API's error.
func TestClient_Generate_ErrorObjectUnderA200(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantType  string
		wantMsg   string
		retryable bool
	}{
		{
			name:     "overloaded_error is retryable",
			body:     `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_011CSHoEeqs5C35K2UUqR7Fy"}`,
			wantType: "overloaded_error", wantMsg: "Overloaded", retryable: true,
		},
		{
			name:     "api_error is retryable",
			body:     `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			wantType: "api_error", wantMsg: "Internal server error", retryable: true,
		},
		{
			name:     "invalid_request_error is not",
			body:     `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`,
			wantType: "invalid_request_error", wantMsg: "bad",
		},
		{
			name:    "an error that names no type is still an error",
			body:    `{"type":"error","error":{"message":"something went wrong"}}`,
			wantMsg: "something went wrong",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := func(t *testing.T, resp *llm.Response, err error) {
				t.Helper()
				require.Error(t, err)
				assert.Nil(t, resp)
				var e *llm.Error
				require.ErrorAs(t, err, &e)
				assert.Equal(t, &llm.Error{
					Provider:  anthropic.Name,
					Status:    http.StatusOK,
					Type:      tt.wantType,
					Message:   tt.wantMsg,
					RequestID: requestID,
					Retryable: tt.retryable,
				}, e)
			}
			api := newFakeAPI(t, replyJSON(tt.body))
			c := api.client(t, anthropic.Options{})

			resp, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
			check(t, resp, err)
			// The same body in answer to a request for a stream.
			resp, err = c.Stream(context.Background(), llm.Request{Messages: hello()}, func(llm.Delta) error { return nil })
			check(t, resp, err)
		})
	}

	t.Run("the request id is the body's when no header carries one", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_011CSHoEeqs5C35K2UUqR7Fy"}`)
		})
		_, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})
		var e *llm.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "req_011CSHoEeqs5C35K2UUqR7Fy", e.RequestID)
	})
}

func TestClient_Generate_EmptyReply(t *testing.T) {
	// A reply the server ended properly with nothing in it is a reply.
	resp := generate(t, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],`+
		`"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":1}}`)

	assert.Equal(t, llm.StopEnd, resp.Stop)
	assert.Empty(t, resp.Message.Text)
	assert.Empty(t, resp.Message.ToolCalls)
	assert.Equal(t, `[]`, string(resp.Message.Opaque.Data))
	assert.Equal(t, llm.Usage{InputTokens: 9, OutputTokens: 1}, resp.Usage)
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
