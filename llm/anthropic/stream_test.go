package anthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
)

// stream asks a fake that answers events for one streamed reply and returns
// it with every delta fn was given.
func stream(t *testing.T, opts anthropic.Options, events string) (*llm.Response, []llm.Delta, error) {
	t.Helper()
	api := newFakeAPI(t, replyStream(events))
	var deltas []llm.Delta
	resp, err := api.client(t, opts).Stream(context.Background(), llm.Request{Messages: hello()}, func(d llm.Delta) error {
		deltas = append(deltas, d)
		return nil
	})
	return resp, deltas, err
}

// recode writes a JSON value in encoding/json's own form, so that two values
// that differ in spacing or key order become the same bytes.
func recode(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(raw, &v), "not JSON: %s", raw)
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return out
}

// plain returns resp with its JSON fields recoded. A streamed reply is
// rebuilt from deltas and an unstreamed one is the bytes of one body, so the
// two agree as values and not as bytes.
func plain(t *testing.T, resp *llm.Response) llm.Response {
	t.Helper()
	require.NotNil(t, resp)
	out := *resp
	out.Message.ToolCalls = append([]llm.ToolCall(nil), resp.Message.ToolCalls...)
	for i := range out.Message.ToolCalls {
		out.Message.ToolCalls[i].Input = recode(t, out.Message.ToolCalls[i].Input)
	}
	if resp.Message.Opaque != nil {
		o := *resp.Message.Opaque
		o.Data = recode(t, o.Data)
		out.Message.Opaque = &o
	}
	return out
}

// textDeltas joins the text of every delta.
func textDeltas(deltas []llm.Delta) string {
	var b strings.Builder
	for _, d := range deltas {
		b.WriteString(d.Text)
	}
	return b.String()
}

// The streamed reply is the unstreamed one: for each recorded event sequence
// Stream returns what Generate builds from the equivalent body.
func TestClient_Stream_EqualsGenerate(t *testing.T) {
	tests := []struct {
		name string
		// events and body are fixtures holding the same reply.
		events string
		body   string
		opts   anthropic.Options
		// refused marks a reply that has no provider form to replay.
		refused bool
	}{
		{name: "text only, with a ping", events: "stream_text.sse", body: "stream_text.json"},
		{name: "text then a tool call split across deltas", events: "stream_tool_use.sse", body: "stream_tool_use.json"},
		{name: "a thinking block with no text and a signature", events: "stream_thinking.sse", body: "stream_thinking.json"},
		{name: "a refusal before any output", events: "stream_refusal.sse", body: "refusal.json", refused: true},
		{
			name:   "a fallback part way through the reply",
			events: "stream_fallback.sse", body: "stream_fallback.json",
			opts: anthropic.Options{RefusalFallback: "default"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, replyEither(fixture(t, tt.body), fixture(t, tt.events)))
			c := api.client(t, tt.opts)
			req := llm.Request{Messages: hello()}

			want, err := c.Generate(context.Background(), req)
			require.NoError(t, err)
			got, err := c.Stream(context.Background(), req, func(llm.Delta) error { return nil })
			require.NoError(t, err)

			assert.Equal(t, plain(t, want), plain(t, got))
			// A reply with nothing in it would also be equal to itself.
			assert.NotEmpty(t, got.ID)
			assert.NotEmpty(t, got.Model)
			assert.NotEmpty(t, got.Stop)
			if tt.refused {
				assert.Nil(t, got.Message.Opaque)
				return
			}
			require.NotNil(t, got.Message.Opaque)
			assert.Equal(t, anthropic.Name, got.Message.Opaque.Provider)
		})
	}
}

func TestClient_Stream_Text(t *testing.T) {
	resp, deltas, err := stream(t, anthropic.Options{}, fixture(t, "stream_text.sse"))
	require.NoError(t, err)

	assert.Equal(t, []llm.Delta{{Text: "Hello"}, {Text: "!"}}, deltas, "a ping is not a delta")
	assert.Equal(t, "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY", resp.ID)
	assert.Equal(t, "claude-opus-5-5", resp.Model)
	assert.Equal(t, llm.RoleAssistant, resp.Message.Role)
	assert.Equal(t, "Hello!", resp.Message.Text)
	assert.Equal(t, llm.StopEnd, resp.Stop)
	// input_tokens comes from message_start and output_tokens from
	// message_delta, which does not repeat the input count.
	assert.Equal(t, llm.Usage{InputTokens: 25, OutputTokens: 15}, resp.Usage)
	assert.JSONEq(t, `[{"type":"text","text":"Hello!"}]`, string(resp.Message.Opaque.Data))
}

func TestClient_Stream_ToolCallSplitAcrossDeltas(t *testing.T) {
	resp, deltas, err := stream(t, anthropic.Options{}, fixture(t, "stream_tool_use.sse"))
	require.NoError(t, err)

	assert.Equal(t, "Okay, let's check the weather for San Francisco, CA:", resp.Message.Text)
	assert.Equal(t, resp.Message.Text, textDeltas(deltas))
	assert.Equal(t, llm.StopToolUse, resp.Stop)
	assert.Equal(t, llm.Usage{InputTokens: 472, OutputTokens: 89}, resp.Usage)
	require.Len(t, resp.Message.ToolCalls, 1)
	assert.Equal(t, llm.ToolCall{
		ID:   "toolu_01T1x1fJ34qAmk2tNTrN7Up6",
		Name: "get_weather",
		// The pieces joined, as the bytes received.
		Input: json.RawMessage(`{"location": "San Francisco, CA"}`),
	}, resp.Message.ToolCalls[0])

	var calls []llm.ToolCallDelta
	for _, d := range deltas {
		if d.ToolCall != nil {
			assert.Empty(t, d.Text)
			calls = append(calls, *d.ToolCall)
		}
	}
	assert.Equal(t, []llm.ToolCallDelta{
		{Index: 0, ID: "toolu_01T1x1fJ34qAmk2tNTrN7Up6", Name: "get_weather"},
		{Index: 0, InputJSON: `{"location":`},
		{Index: 0, InputJSON: ` "San`},
		{Index: 0, InputJSON: ` Francisc`},
		{Index: 0, InputJSON: `o,`},
		{Index: 0, InputJSON: ` CA"}`},
	}, calls, "the id and name arrive once, on the first delta of the call")
}

func TestClient_Stream_NilFn(t *testing.T) {
	api := newFakeAPI(t, replyStream(fixture(t, "stream_text.sse")))
	resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello!", resp.Message.Text)
}

// The text of a delta is kept as it will be written, escaped, and comes out
// as the text it was.
func TestClient_Stream_TextThatNeedsEscaping(t *testing.T) {
	const text = "quote \" backslash \\ newline \n tab \t nul \x00 line separator \u2028 <b>&amp;</b> é 🐟"
	part, err := json.Marshal(text)
	require.NoError(t, err)
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Start: \"x\" "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(part) + `}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(part) + `}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":` + string(part) + `}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`
	resp, _, err := stream(t, anthropic.Options{}, events)
	require.NoError(t, err)

	want := `Start: "x" ` + text + text
	assert.Equal(t, want, resp.Message.Text)
	require.True(t, json.Valid(resp.Message.Opaque.Data), "%s", resp.Message.Opaque.Data)
	var blocks []struct {
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	require.NoError(t, json.Unmarshal(resp.Message.Opaque.Data, &blocks))
	require.Len(t, blocks, 2)
	assert.Equal(t, want, blocks[0].Text)
	assert.Equal(t, text, blocks[1].Thinking)
}

func TestClient_Stream_TwoToolCalls(t *testing.T) {
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"first","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"z\": 1, \"a\": 2}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"second","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_3","name":"third","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":40}}

event: message_stop
data: {"type":"message_stop"}

`
	resp, deltas, err := stream(t, anthropic.Options{}, events)
	require.NoError(t, err)

	assert.Equal(t, []llm.ToolCall{
		{ID: "toolu_1", Name: "first", Input: json.RawMessage(`{"z": 1, "a": 2}`)},
		// No delta at all, and only an empty one: both are a call with no
		// arguments, which is an empty object.
		{ID: "toolu_2", Name: "second", Input: json.RawMessage(`{}`)},
		{ID: "toolu_3", Name: "third", Input: json.RawMessage(`{}`)},
	}, resp.Message.ToolCalls)

	var calls []llm.ToolCallDelta
	for _, d := range deltas {
		require.NotNil(t, d.ToolCall)
		calls = append(calls, *d.ToolCall)
	}
	assert.Equal(t, []llm.ToolCallDelta{
		{Index: 0, ID: "toolu_1", Name: "first", InputJSON: `{"z": 1, "a": 2}`},
		// A call that streamed no arguments is still announced.
		{Index: 1, ID: "toolu_2", Name: "second"},
		{Index: 2, ID: "toolu_3", Name: "third"},
	}, calls)

	_, err = json.Marshal(resp.Message)
	require.NoError(t, err)
}

func TestClient_Stream_Thinking(t *testing.T) {
	t.Run("a block with no text opens, takes its signature and closes", func(t *testing.T) {
		resp, deltas, err := stream(t, anthropic.Options{}, fixture(t, "stream_thinking.sse"))
		require.NoError(t, err)

		for _, d := range deltas {
			assert.Empty(t, d.Reasoning, "an empty thinking delta is not delivered")
			assert.NotNil(t, d.ToolCall)
		}
		assert.Equal(t, llm.Usage{InputTokens: 472, OutputTokens: 121, ReasoningTokens: 32}, resp.Usage)

		var blocks []map[string]any
		require.NoError(t, json.Unmarshal(resp.Message.Opaque.Data, &blocks))
		require.Len(t, blocks, 2)
		assert.Equal(t, map[string]any{
			"type":      "thinking",
			"thinking":  "",
			"signature": "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds",
		}, blocks[0], "the block stays, with the signature the next request needs")
		// A field of the block this package does not read is kept too.
		assert.Equal(t, map[string]any{"type": "direct"}, blocks[1]["caller"])
	})

	t.Run("thinking text is delivered as Reasoning", func(t *testing.T) {
		// The reference's own example of a stream with thinking. It carries
		// no usage at all.
		events := `event: message_start
data: {"type": "message_start", "message": {"id": "msg_01", "type": "message", "role": "assistant", "content": [], "model": "claude-opus-5-5", "stop_reason": null, "stop_sequence": null}}

event: content_block_start
data: {"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": "", "signature": ""}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "I need to find the GCD of 1071 and 462 using the Euclidean algorithm.\n\n1071 = 2 × 462 + 147"}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "\n462 = 3 × 147 + 21"}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": "EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"}}

event: content_block_stop
data: {"type": "content_block_stop", "index": 0}

event: content_block_start
data: {"type": "content_block_start", "index": 1, "content_block": {"type": "text", "text": ""}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": "The greatest common divisor of 1071 and 462 is **21**."}}

event: content_block_stop
data: {"type": "content_block_stop", "index": 1}

event: message_delta
data: {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": null}}

event: message_stop
data: {"type": "message_stop"}

`
		resp, deltas, err := stream(t, anthropic.Options{}, events)
		require.NoError(t, err)

		assert.Equal(t, []llm.Delta{
			{Reasoning: "I need to find the GCD of 1071 and 462 using the Euclidean algorithm.\n\n1071 = 2 × 462 + 147"},
			{Reasoning: "\n462 = 3 × 147 + 21"},
			{Text: "The greatest common divisor of 1071 and 462 is **21**."},
		}, deltas)
		assert.Equal(t, "The greatest common divisor of 1071 and 462 is **21**.", resp.Message.Text,
			"reasoning is not part of the reply's text")
		assert.Equal(t, llm.Usage{}, resp.Usage)
		assert.JSONEq(t, `[
			{"type":"thinking","thinking":"I need to find the GCD of 1071 and 462 using the Euclidean algorithm.\n\n1071 = 2 × 462 + 147\n462 = 3 × 147 + 21","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"},
			{"type":"text","text":"The greatest common divisor of 1071 and 462 is **21**."}
		]`, string(resp.Message.Opaque.Data))
	})
}

func TestClient_Stream_BlockKeepsWhatItStartedWith(t *testing.T) {
	// Every documented block starts empty and is filled by deltas. Nothing
	// says one must, so what content_block_start gives is the starting point
	// and not thrown away.
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"Already ","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":" there."}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	resp, deltas, err := stream(t, anthropic.Options{}, events)
	require.NoError(t, err)

	assert.Equal(t, "Hello there.", resp.Message.Text)
	assert.Equal(t, []llm.Delta{{Reasoning: "thought."}, {Text: "lo"}}, deltas, "an empty text delta is not delivered")
	assert.JSONEq(t, `[
		{"type":"thinking","thinking":"Already thought.","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"},
		{"type":"text","text":"Hello"},
		{"type":"text","text":" there."}
	]`, string(resp.Message.Opaque.Data))
}

func TestClient_Stream_UnknownEventIsIgnored(t *testing.T) {
	want, _, err := stream(t, anthropic.Options{}, fixture(t, "stream_text.sse"))
	require.NoError(t, err)

	// The same stream with events this package has never heard of, one of
	// them not even JSON, before, between and inside the block.
	events := "event: session_note\ndata: {\"type\":\"session_note\",\"note\":\"hello\"}\n\n" +
		strings.Replace(fixture(t, "stream_text.sse"),
			"event: ping\ndata: {\"type\": \"ping\"}\n\n",
			"event: content_block_progress\ndata: {\"type\":\"content_block_progress\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"NOT TEXT\"}}\n\n"+
				"event: heartbeat\ndata: plain words\n\n", 1)
	require.Contains(t, events, "content_block_progress", "the fixture no longer has the ping this test replaces")

	got, deltas, err := stream(t, anthropic.Options{}, events)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, []llm.Delta{{Text: "Hello"}, {Text: "!"}}, deltas)
}

func TestClient_Stream_EventsWithoutNames(t *testing.T) {
	// Every event repeats its type in its data, so a stream that reaches the
	// client with its event lines gone still reads the same.
	whole := fixture(t, "stream_tool_use.sse")
	want, wantDeltas, err := stream(t, anthropic.Options{}, whole)
	require.NoError(t, err)

	var bare []string
	for _, line := range strings.Split(whole, "\n") {
		if !strings.HasPrefix(line, "event:") {
			bare = append(bare, line)
		}
	}
	got, gotDeltas, err := stream(t, anthropic.Options{}, strings.Join(bare, "\n"))
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, wantDeltas, gotDeltas)
}

// captureLog collects what a Client logs.
func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestClient_Stream_UnknownDeltaLeavesOpaqueNil(t *testing.T) {
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The grass is green."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"delta_from_the_future","payload":{"k":1}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"now","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"zone\": \"UTC\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`
	logger, logged := captureLog()
	resp, deltas, err := stream(t, anthropic.Options{Logger: logger}, events)
	require.NoError(t, err)

	// The content cannot be rebuilt as the server holds it, so no provider
	// form is offered and the turn is replayed from Text and ToolCalls.
	assert.Nil(t, resp.Message.Opaque)
	assert.Equal(t, "The grass is green.", resp.Message.Text)
	assert.Equal(t, []llm.ToolCall{{ID: "toolu_1", Name: "now", Input: json.RawMessage(`{"zone": "UTC"}`)}}, resp.Message.ToolCalls)
	assert.Equal(t, llm.StopToolUse, resp.Stop)
	assert.Equal(t, llm.Usage{InputTokens: 10, OutputTokens: 30}, resp.Usage)
	assert.Equal(t, "msg_01", resp.ID)
	assert.Len(t, deltas, 2, "the delta this package does not know is not delivered")

	assert.Contains(t, logged.String(), "level=WARN")
	assert.Contains(t, logged.String(), "delta_from_the_future")
	assert.Equal(t, 1, strings.Count(logged.String(), "level=WARN"), "one line for the reply, not one per delta")
}

func TestClient_Stream_KnownDeltasLogNothing(t *testing.T) {
	logger, logged := captureLog()
	_, _, err := stream(t, anthropic.Options{Logger: logger}, fixture(t, "stream_fallback.sse"))
	require.NoError(t, err)
	assert.Empty(t, logged.String())
}

func TestClient_Stream_ErrorEvent(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantType  string
		wantMsg   string
		retryable bool
	}{
		{
			name:     "overloaded_error is retryable",
			data:     `{"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}`,
			wantType: "overloaded_error", wantMsg: "Overloaded", retryable: true,
		},
		{
			name:     "api_error is retryable",
			data:     `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			wantType: "api_error", wantMsg: "Internal server error", retryable: true,
		},
		{
			name:     "invalid_request_error is not",
			data:     `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`,
			wantType: "invalid_request_error", wantMsg: "bad",
		},
		{
			name:     "rate_limit_error mid-stream carries no retry-after, so it is not",
			data:     `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			wantType: "rate_limit_error", wantMsg: "slow down",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The reply is under way when the error arrives.
			events := strings.Replace(fixture(t, "stream_text.sse"),
				"event: content_block_stop",
				"event: error\ndata: "+tt.data+"\n\nevent: content_block_stop", 1)
			require.Contains(t, events, "event: error")

			resp, deltas, err := stream(t, anthropic.Options{}, events)
			require.Error(t, err)
			assert.Nil(t, resp, "a reply that failed part way is not returned")
			assert.Equal(t, "Hello!", textDeltas(deltas), "what arrived before the error was delivered")

			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, anthropic.Name, e.Provider)
			assert.Equal(t, tt.wantType, e.Type)
			assert.Equal(t, tt.wantMsg, e.Message)
			assert.Equal(t, requestID, e.RequestID)
			assert.Equal(t, tt.retryable, e.Retryable)
			assert.Equal(t, tt.retryable, llm.Retryable(err))
			assert.Equal(t, http.StatusOK, e.Status, "the status the stream opened with")
			assert.NoError(t, e.Err, "the API answered: this is not a transport failure")
		})
	}
}

func TestClient_Stream_FnErrorStopsTheStream(t *testing.T) {
	api := newFakeAPI(t, replyStream(fixture(t, "stream_tool_use.sse")))
	stop := errors.New("the consumer has gone")

	calls := 0
	resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()},
		func(llm.Delta) error {
			calls++
			if calls == 3 {
				return stop
			}
			return nil
		})

	require.ErrorIs(t, err, stop)
	assert.Nil(t, resp)
	assert.Equal(t, 3, calls, "fn is not called again after it returns an error")
	assert.False(t, llm.Retryable(err))
}

func TestClient_Stream_FnErrorOnACallWithNoArguments(t *testing.T) {
	// A call that streamed no arguments is announced when its block closes,
	// and an error from fn there stops the stream like any other.
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"now","input":{}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}

`
	api := newFakeAPI(t, replyStream(events))
	stop := errors.New("the consumer has gone")
	resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()},
		func(d llm.Delta) error {
			require.NotNil(t, d.ToolCall)
			return stop
		})
	require.ErrorIs(t, err, stop)
	assert.Nil(t, resp)
}

func TestClient_Stream_UsageIsCumulative(t *testing.T) {
	tests := []struct {
		name   string
		start  string
		deltas []string
		want   llm.Usage
	}{
		{
			name:   "output tokens replace and are not added",
			start:  `{"input_tokens":25,"output_tokens":1}`,
			deltas: []string{`{"output_tokens":5}`, `{"output_tokens":15}`},
			want:   llm.Usage{InputTokens: 25, OutputTokens: 15},
		},
		{
			name:  "a count message_delta repeats replaces the one message_start gave",
			start: `{"input_tokens":2679,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}`,
			deltas: []string{
				`{"input_tokens":10682,"cache_creation_input_tokens":40,"cache_read_input_tokens":2000,"output_tokens":510,"output_tokens_details":{"thinking_tokens":77},"server_tool_use":{"web_search_requests":1}}`,
			},
			want: llm.Usage{InputTokens: 10682, OutputTokens: 510, CacheWriteTokens: 40, CacheReadTokens: 2000, ReasoningTokens: 77},
		},
		{
			name:   "a count message_delta sends as null keeps the one message_start gave",
			start:  `{"input_tokens":25,"cache_creation_input_tokens":7,"cache_read_input_tokens":9,"output_tokens":1}`,
			deltas: []string{`{"input_tokens":null,"cache_creation_input_tokens":null,"cache_read_input_tokens":null,"output_tokens":15,"output_tokens_details":null}`},
			want:   llm.Usage{InputTokens: 25, OutputTokens: 15, CacheWriteTokens: 7, CacheReadTokens: 9},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString(`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":` + tt.start + `}}` + "\n\n" +
				`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
				`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
				`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n")
			for _, u := range tt.deltas {
				b.WriteString(`event: message_delta` + "\n" +
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":` + u + `}` + "\n\n")
			}
			b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

			resp, _, err := stream(t, anthropic.Options{}, b.String())
			require.NoError(t, err)
			assert.Equal(t, tt.want, resp.Usage)
			assert.Equal(t, llm.StopEnd, resp.Stop)
		})
	}
}

func TestClient_Stream_Refusal(t *testing.T) {
	t.Run("before any output", func(t *testing.T) {
		resp, deltas, err := stream(t, anthropic.Options{}, fixture(t, "stream_refusal.sse"))
		require.NoError(t, err, "a refusal is a reply")

		assert.Empty(t, deltas)
		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.Equal(t, &llm.Refusal{
			Category:    "cyber",
			Explanation: "This request was declined because it could enable cyber harm.",
		}, resp.Refusal)
		assert.Equal(t, llm.Message{Role: llm.RoleAssistant}, resp.Message)
		assert.Equal(t, llm.Usage{}, resp.Usage, "declined before any output, in a category that is not billed")
	})

	t.Run("after partial output", func(t *testing.T) {
		// Text and a whole tool call arrive, and then the reply is refused.
		// What was streamed has been seen and cannot be taken back; the
		// Response carries none of it.
		events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":412,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me look at"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"delta_from_the_future","payload":{"k":1}}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01A09q90qw90lq917835lq9","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"location\": \"San Francisco, CA\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"This request was declined because it could enable cyber harm."}},"usage":{"output_tokens":37}}

event: message_stop
data: {"type":"message_stop"}

`
		logger, logged := captureLog()
		resp, deltas, err := stream(t, anthropic.Options{Logger: logger}, events)
		require.NoError(t, err)

		require.Len(t, deltas, 2, "the deltas were delivered as they came")
		assert.Equal(t, "Let me look at", deltas[0].Text)
		assert.Equal(t, "get_weather", deltas[1].ToolCall.Name)

		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.Equal(t, "cyber", resp.Refusal.Category)
		assert.Equal(t, llm.Message{Role: llm.RoleAssistant}, resp.Message, "a refused reply hands on no text, no tool call and nothing to replay")
		assert.Equal(t, llm.Usage{InputTokens: 412, OutputTokens: 37}, resp.Usage, "a refusal after output is billed")
		// The stream held a delta this package does not know, which costs any
		// other reply its provider form and earns a warning. A refused reply
		// had none to lose.
		assert.Empty(t, logged.String(), "there is nothing to warn about: a refused turn is never replayed")
	})
}

func TestClient_Stream_EmptyReply(t *testing.T) {
	// The server opened the message, said how it ended and closed it, with
	// no content between. That is a reply, and not a stream that failed.
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
	resp, deltas, err := stream(t, anthropic.Options{}, events)
	require.NoError(t, err)

	assert.Empty(t, deltas)
	assert.Equal(t, "msg_01", resp.ID)
	assert.Equal(t, llm.StopEnd, resp.Stop)
	assert.Empty(t, resp.Message.Text)
	assert.Equal(t, `[]`, string(resp.Message.Opaque.Data))
	assert.Equal(t, llm.Usage{InputTokens: 9, OutputTokens: 1}, resp.Usage)
}

func TestClient_Stream_Fallback(t *testing.T) {
	resp, deltas, err := stream(t, anthropic.Options{RefusalFallback: "default"}, fixture(t, "stream_fallback.sse"))
	require.NoError(t, err)

	// message_start named the model that then declined. The model that
	// answered is the one the fallback block hands over to.
	assert.Equal(t, "claude-sonnet-5-5", resp.Model)
	assert.Equal(t, "Let me look at that. Here is what I found.", resp.Message.Text)
	assert.Equal(t, resp.Message.Text, textDeltas(deltas))
	assert.Equal(t, llm.StopToolUse, resp.Stop)
	assert.Equal(t, llm.Usage{InputTokens: 412, OutputTokens: 264, CacheReadTokens: 96, CacheWriteTokens: 31}, resp.Usage)
	assert.Equal(t, []llm.Attempt{
		{Model: "claude-opus-5-5", Usage: llm.Usage{InputTokens: 535, OutputTokens: 148}},
		{Model: "claude-sonnet-5-5", Usage: llm.Usage{InputTokens: 412, OutputTokens: 264, CacheReadTokens: 96, CacheWriteTokens: 31}},
	}, resp.Attempts)

	// Deltas number the client's tool calls as they stream, the declining
	// model's included. A server tool's arguments are not a call to announce.
	var announced []llm.ToolCallDelta
	for _, d := range deltas {
		if d.ToolCall != nil && d.ToolCall.ID != "" {
			announced = append(announced, llm.ToolCallDelta{Index: d.ToolCall.Index, ID: d.ToolCall.ID, Name: d.ToolCall.Name})
		}
	}
	assert.Equal(t, []llm.ToolCallDelta{
		{Index: 0, ID: "toolu_01T1x1fJ34qAmk2tNTrN7Up6", Name: "get_weather"},
		{Index: 1, ID: "toolu_01A09q90qw90lq917835lq9", Name: "get_weather"},
	}, announced)

	// The call the declining model made is not one to run: the echo rule
	// drops it from the turn, so a result for it would answer nothing.
	require.Len(t, resp.Message.ToolCalls, 1)
	assert.Equal(t, "toolu_01A09q90qw90lq917835lq9", resp.Message.ToolCalls[0].ID)

	var kept []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(resp.Message.Opaque.Data, &kept))
	var types []string
	for _, b := range kept {
		types = append(types, b.Type)
	}
	assert.Equal(t, []string{
		"text", "server_tool_use", "code_execution_tool_result", "fallback", "thinking", "text", "tool_use",
	}, types)
	assert.Equal(t, "srvtoolu_014hJH82Qum7Td6UV8gDXThB", kept[1].ID, "the server tool call that has its result")
}

// A streamed reply holding a thinking block goes back as the next request's
// assistant turn with the values the server sent.
func TestClient_Stream_AssistantTurnIsReplayedAsReceived(t *testing.T) {
	api := newFakeAPI(t, replyEither(okBody, fixture(t, "stream_thinking.sse")))
	c := api.client(t, anthropic.Options{})
	ctx := context.Background()

	messages := []llm.Message{userTurn("What is the weather in San Francisco?")}
	first, err := c.Stream(ctx, llm.Request{Messages: messages}, func(llm.Delta) error { return nil })
	require.NoError(t, err)
	require.Len(t, first.Message.ToolCalls, 1)

	messages = append(messages, first.Message, llm.Message{
		Role:        llm.RoleTool,
		ToolResults: []llm.ToolResult{{CallID: first.Message.ToolCalls[0].ID, Content: "15 degrees"}},
	})
	_, err = c.Generate(ctx, llm.Request{Messages: messages})
	require.NoError(t, err)

	var got struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(api.last(t).Body, &got))
	require.Len(t, got.Messages, 3)
	assert.JSONEq(t, string(contentOf(t, fixture(t, "stream_thinking.json"))), string(got.Messages[1].Content))
}

func TestClient_Stream_ToolArgumentsThatAreNotJSON(t *testing.T) {
	// A reply stopped by max_tokens in the middle of a tool call.
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"location\": \"San Fra"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":64000}}

event: message_stop
data: {"type":"message_stop"}

`
	logger, logged := captureLog()
	resp, _, err := stream(t, anthropic.Options{Logger: logger}, events)
	require.NoError(t, err)

	assert.Equal(t, llm.StopMaxTokens, resp.Stop)
	require.Len(t, resp.Message.ToolCalls, 1)
	call := resp.Message.ToolCalls[0]
	assert.True(t, call.Malformed)
	var text string
	require.NoError(t, json.Unmarshal(call.Input, &text), "Input holds the arguments as one JSON string")
	assert.Equal(t, `{"location": "San Fra`, text)
	assert.Nil(t, resp.Message.Opaque, "a turn that cannot be rebuilt offers no provider form")
	assert.Contains(t, logged.String(), "level=WARN")

	_, err = json.Marshal(resp.Message)
	require.NoError(t, err, "the message must survive a journal's json.Marshal")
}

func TestClient_Stream_EventsOutOfPlace(t *testing.T) {
	start := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

`
	tests := []struct {
		name   string
		events string
	}{
		{
			name: "a delta for a block that never started",
			events: start + `event: content_block_delta
data: {"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"lost"}}

`,
		},
		{
			name: "an event whose data is not JSON",
			events: start + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":

`,
		},
		{
			name: "a block started twice",
			events: start + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
		},
		{
			name: "a stop for a block that never started",
			events: start + `event: content_block_stop
data: {"type":"content_block_stop","index":3}

`,
		},
		{
			name: "a delta for a block after its stop",
			events: start + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"late"}}

`,
		},
		{
			name: "a block stopped twice",
			events: start + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`,
		},
		{
			name: "a block that is null",
			events: start + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":null}

`,
		},
		{
			name: "a block that is not an object",
			events: start + `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":"text"}

`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _, err := stream(t, anthropic.Options{}, tt.events+
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			require.Error(t, err)
			assert.Nil(t, resp)

			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, anthropic.Name, e.Provider)
			assert.Equal(t, requestID, e.RequestID)
			assert.False(t, e.Retryable, "the same request would be answered the same way")
		})
	}
}

// The reference says stop_reason is non-null by the time a stream ends. A
// message_stop with no reason before it, or with nothing before it at all,
// is a stream that lost its ending: a failed call that may well succeed
// asked again, and not a reply with an empty stop.
func TestClient_Stream_StoppedWithoutAnEnding(t *testing.T) {
	start := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

`
	const stop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	tests := []struct {
		name   string
		events string
	}{
		{name: "message_stop and nothing before it", events: stop},
		{name: "a reply with no message_delta", events: start + stop},
		{
			name: "a message_delta whose stop_reason is null",
			events: start + "event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":3}}` + "\n\n" + stop,
		},
		{
			name: "a message_delta with no stop_reason",
			events: start + "event: message_delta\n" +
				`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":3}}` + "\n\n" + stop,
		},
		{
			name: "a stop_reason that is empty",
			events: start + "event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":""},"usage":{"output_tokens":3}}` + "\n\n" + stop,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _, err := stream(t, anthropic.Options{}, tt.events)
			require.Error(t, err)
			assert.Nil(t, resp)
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)

			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, anthropic.Name, e.Provider)
			assert.Zero(t, e.Status)
			assert.Equal(t, requestID, e.RequestID)
			assert.True(t, e.Retryable)
			assert.True(t, llm.Retryable(err))
		})
	}

	t.Run("a stop reason this package does not know is still a reason", func(t *testing.T) {
		resp, _, err := stream(t, anthropic.Options{}, start+"event: message_delta\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"a_reason_from_the_future"},"usage":{"output_tokens":3}}`+"\n\n"+stop)
		require.NoError(t, err)
		assert.Equal(t, llm.StopReason("a_reason_from_the_future"), resp.Stop)
		assert.Equal(t, "ok", resp.Message.Text)
	})
}

// A thinking block that closes with no signature is left out of the
// provider's form of the turn, with the thinking after it, so that the turn
// can be replayed.
func TestClient_Stream_ThinkingWithNoSignatureIsNotKept(t *testing.T) {
	events := `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"This block never gets its signature."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":"ErUBCkYIBxgCIkDx8Yk3gYq0oSmJ7oQp1nZk2bWc9hV"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_start
data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"now","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"zone\": \"UTC\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":3}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`
	logger, logged := captureLog()
	api := newFakeAPI(t, replyEither(okBody, events))
	c := api.client(t, anthropic.Options{Logger: logger})
	ctx := context.Background()

	resp, err := c.Stream(ctx, llm.Request{Messages: hello()}, func(llm.Delta) error { return nil })
	require.NoError(t, err)

	require.NotNil(t, resp.Message.Opaque, "the turn can still be replayed in its own form")
	assert.JSONEq(t, `[
		{"type":"thinking","thinking":"","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"},
		{"type":"tool_use","id":"toolu_1","name":"now","input":{"zone": "UTC"}}
	]`, string(resp.Message.Opaque.Data))
	require.Len(t, resp.Message.ToolCalls, 1)
	assert.Equal(t, 1, strings.Count(logged.String(), "level=WARN"), logged.String())
	assert.Contains(t, logged.String(), "signature")

	// What goes back holds no block with an empty signature.
	_, err = c.Generate(ctx, llm.Request{Messages: []llm.Message{
		userTurn("What time is it?"),
		resp.Message,
		{Role: llm.RoleTool, ToolResults: []llm.ToolResult{{CallID: "toolu_1", Content: "12:00"}}},
	}})
	require.NoError(t, err)
	sent := string(api.last(t).Body)
	assert.NotContains(t, sent, `"signature":""`)
	assert.NotContains(t, sent, "never gets its signature")
}

// replyCut writes head as the body of a chunked reply and then drops the
// connection without ending the body, which the client's transport reports
// as an unexpected end of file.
func replyCut(t *testing.T, head string) reply {
	return func(w http.ResponseWriter, _ received) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test server cannot hijack its connection")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nrequest-id: " + requestID +
			"\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = buf.WriteString(strconv.FormatInt(int64(len(head)), 16) + "\r\n" + head + "\r\n")
		_ = buf.Flush()
	}
}

func TestClient_Stream_CutBeforeTheTerminalEvent(t *testing.T) {
	whole := fixture(t, "stream_text.sse")
	stop := strings.Index(whole, "event: message_stop")
	require.Positive(t, stop)
	lastDelta := strings.Index(whole, "event: message_delta")
	require.Positive(t, lastDelta)

	tests := []struct {
		name string
		// answer serves the cut stream.
		answer func(t *testing.T) reply
	}{
		{
			name:   "the server ends the body between events, before message_stop",
			answer: func(*testing.T) reply { return replyStream(whole[:stop]) },
		},
		{
			name:   "the server ends the body in the middle of an event",
			answer: func(*testing.T) reply { return replyStream(whole[:lastDelta+30]) },
		},
		{
			name:   "the server ends the body in the middle of message_stop itself",
			answer: func(*testing.T) reply { return replyStream(strings.TrimRight(whole, "\n")) },
		},
		{
			name:   "the connection drops between events",
			answer: func(t *testing.T) reply { return replyCut(t, whole[:stop]) },
		},
		{
			name:   "the connection drops in the middle of an event",
			answer: func(t *testing.T) reply { return replyCut(t, whole[:lastDelta+30]) },
		},
		{
			name:   "the stream ends before it says anything",
			answer: func(*testing.T) reply { return replyStream("") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, tt.answer(t))
			resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()},
				func(llm.Delta) error { return nil })

			require.Error(t, err)
			assert.Nil(t, resp, "a reply cut short is not a reply")
			require.ErrorIs(t, err, io.ErrUnexpectedEOF)

			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, anthropic.Name, e.Provider)
			assert.Error(t, e.Err, "a transport failure carries its cause")
			assert.Zero(t, e.Status)
			assert.Equal(t, requestID, e.RequestID, "the response had begun, so its id is known")
			assert.True(t, e.Retryable)
			assert.True(t, llm.Retryable(err), "the same request may well succeed")
		})
	}
}

func TestClient_Stream_CutAfterTheTerminalEvent(t *testing.T) {
	whole := fixture(t, "stream_text.sse")
	want, _, err := stream(t, anthropic.Options{}, whole)
	require.NoError(t, err)

	tests := []struct {
		name   string
		answer func(t *testing.T) reply
	}{
		{
			name:   "the server ends the body in the middle of a later event",
			answer: func(*testing.T) reply { return replyStream(whole + "event: ping\ndata: {\"type\": \"pi") },
		},
		{
			name:   "the connection drops straight after message_stop",
			answer: func(t *testing.T) reply { return replyCut(t, whole) },
		},
		{
			name:   "the connection drops in the middle of a later event",
			answer: func(t *testing.T) reply { return replyCut(t, whole+"event: ping\ndata: {\"type\": \"pi") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, tt.answer(t))
			got, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()},
				func(llm.Delta) error { return nil })

			require.NoError(t, err, "message_stop was read: the reply is complete, whatever the connection did next")
			assert.Equal(t, want, got)
		})
	}
}

func TestClient_Stream_CancelledContextIsNotRetryable(t *testing.T) {
	whole := fixture(t, "stream_text.sse")
	firstDelta := strings.Index(whole, "event: content_block_delta")
	secondDelta := strings.LastIndex(whole, "event: content_block_delta")
	require.Positive(t, firstDelta)
	require.Greater(t, secondDelta, firstDelta)

	released := make(chan struct{})
	api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, whole[:secondDelta])
		w.(http.Flusher).Flush()
		// Hold the stream open until the client has gone.
		<-released
	})
	t.Cleanup(func() { close(released) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp, err := api.client(t, anthropic.Options{}).Stream(ctx, llm.Request{Messages: hello()},
		func(llm.Delta) error {
			cancel()
			return nil
		})

	require.Error(t, err)
	assert.Nil(t, resp)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, llm.Retryable(err))
	var e *llm.Error
	assert.NotErrorAs(t, err, &e, "the caller gave up: that is the context's error and not the provider's")
}
