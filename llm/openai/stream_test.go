package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

// chunk is a chat.completion.chunk with one choice. delta is a JSON object
// and finish a bare reason, or "" for null.
func chunk(delta, finish string) string {
	reason := "null"
	if finish != "" {
		reason = strconv.Quote(finish)
	}
	return fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m",`+
		`"choices":[{"index":0,"delta":%s,"logprobs":null,"finish_reason":%s}],"usage":null}`, delta, reason)
}

// usageChunk is the final chunk include_usage asks for: no choices.
func usageChunk(usage string) string {
	return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":` + usage + `}`
}

// callPiece is one entry of a delta's tool_calls. An empty id or name is left
// out, as a server does after the first chunk of a call.
func callPiece(index int, id, name, args string) string {
	fn := `"arguments":` + strconv.Quote(args)
	if name != "" {
		fn = `"name":` + strconv.Quote(name) + "," + fn
	}
	head := fmt.Sprintf(`"index":%d`, index)
	if id != "" {
		head += `,"id":` + strconv.Quote(id) + `,"type":"function"`
	}
	return `{` + head + `,"function":{` + fn + `}}`
}

func callsDelta(pieces ...string) string {
	return `{"tool_calls":[` + strings.Join(pieces, ",") + `]}`
}

func textDelta(text string) string {
	b, _ := json.Marshal(text)
	return `{"content":` + string(b) + `}`
}

// deltaLog collects what a stream's callback receives.
type deltaLog struct{ got []llm.Delta }

func (l *deltaLog) fn(d llm.Delta) error {
	l.got = append(l.got, d)
	return nil
}

func stream(t *testing.T, handler http.HandlerFunc) (*llm.Response, []llm.Delta, error) {
	t.Helper()
	srv, _ := newServer(t, handler)
	var log deltaLog
	resp, err := newClient(t, srv).Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, log.fn)
	return resp, log.got, err
}

const finalUsage = `{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11}`

// Each case is a recorded event sequence and the unstreamed body that says
// the same thing. The streamed Response must equal what Generate builds from
// that body, and the callback must see the increments in the order they came.
func TestStream_BuildsWhatGenerateBuilds(t *testing.T) {
	tests := []struct {
		name       string
		events     []string
		equivalent string
		wantDeltas []llm.Delta
	}{
		{
			name: "text",
			events: []string{
				chunk(`{"role":"assistant","content":""}`, ""),
				chunk(textDelta("Hel"), ""),
				chunk(textDelta("lo"), ""),
				chunk(`{}`, "stop"),
				usageChunk(finalUsage),
			},
			equivalent: completion(`{"role":"assistant","content":"Hello"}`, `"stop"`, finalUsage),
			wantDeltas: []llm.Delta{{Text: "Hel"}, {Text: "lo"}},
		},
		{
			name: "a tool call whose id and name arrive first and whose arguments arrive in pieces",
			events: []string{
				chunk(`{"role":"assistant","content":null,"tool_calls":[`+callPiece(0, "call_abc123", "get_current_weather", "")+`]}`, ""),
				chunk(callsDelta(callPiece(0, "", "", `{"loca`)), ""),
				chunk(callsDelta(callPiece(0, "", "", `tion": "Bos`)), ""),
				chunk(callsDelta(callPiece(0, "", "", `ton, MA"}`)), ""),
				chunk(`{}`, "tool_calls"),
				usageChunk(finalUsage),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"call_abc123","type":"function","function":{"name":"get_current_weather","arguments":"{\"location\": \"Boston, MA\"}"}}]}`,
				`"tool_calls"`, finalUsage),
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_abc123", Name: "get_current_weather"}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `{"loca`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `tion": "Bos`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `ton, MA"}`}},
			},
		},
		{
			name: "the first chunk of a call may already carry arguments",
			events: []string{
				chunk(callsDelta(callPiece(0, "c1", "f", `{"a":`)), ""),
				chunk(callsDelta(callPiece(0, "", "", `1}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`, `"tool_calls"`, ""),
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "f", InputJSON: `{"a":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `1}`}},
			},
		},
		{
			name: "two tool calls, their pieces interleaved, told apart by index",
			events: []string{
				chunk(callsDelta(callPiece(0, "call_a", "first", `{"x":`)), ""),
				chunk(callsDelta(callPiece(1, "call_b", "second", `{"y":`)), ""),
				chunk(callsDelta(callPiece(0, "", "", `1}`)), ""),
				chunk(callsDelta(callPiece(1, "", "", `2}`)), ""),
				chunk(`{}`, "tool_calls"),
				usageChunk(finalUsage),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"call_a","type":"function","function":{"name":"first","arguments":"{\"x\":1}"}},`+
				`{"id":"call_b","type":"function","function":{"name":"second","arguments":"{\"y\":2}"}}]}`, `"tool_calls"`, finalUsage),
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_a", Name: "first", InputJSON: `{"x":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 1, ID: "call_b", Name: "second", InputJSON: `{"y":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `1}`}},
				{ToolCall: &llm.ToolCallDelta{Index: 1, InputJSON: `2}`}},
			},
		},
		{
			name: "two tool calls in one chunk",
			events: []string{
				chunk(callsDelta(callPiece(0, "call_a", "first", `{}`), callPiece(1, "call_b", "second", `{}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"call_a","type":"function","function":{"name":"first","arguments":"{}"}},`+
				`{"id":"call_b","type":"function","function":{"name":"second","arguments":"{}"}}]}`, `"tool_calls"`, ""),
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_a", Name: "first", InputJSON: `{}`}},
				{ToolCall: &llm.ToolCallDelta{Index: 1, ID: "call_b", Name: "second", InputJSON: `{}`}},
			},
		},
		{
			name: "a server that repeats the id and name on every chunk of a call",
			events: []string{
				chunk(callsDelta(callPiece(0, "c1", "f", `{"a":`)), ""),
				chunk(callsDelta(callPiece(0, "c1", "f", `1}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`, `"tool_calls"`, ""),
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "f", InputJSON: `{"a":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `1}`}},
			},
		},
		{
			name: "a call that takes no arguments at all is an empty object",
			events: []string{
				chunk(callsDelta(callPiece(0, "c1", "ping", "")), ""),
				chunk(`{}`, "tool_calls"),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"ping","arguments":""}}]}`, `"tool_calls"`, ""),
			wantDeltas: []llm.Delta{{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "ping"}}},
		},
		{
			name: "arguments that never become valid JSON are malformed once assembled",
			events: []string{
				chunk(callsDelta(callPiece(0, "c1", "f", `{"id":`)), ""),
				chunk(callsDelta(callPiece(0, "", "", ` tru`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"id\": tru"}}]}`, `"tool_calls"`, ""),
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "f", InputJSON: `{"id":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: ` tru`}},
			},
		},
		{
			name: "text and then a call",
			events: []string{
				chunk(textDelta("on it"), ""),
				chunk(callsDelta(callPiece(0, "c1", "f", `{}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			equivalent: completion(`{"role":"assistant","content":"on it","tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}`, `"tool_calls"`, ""),
			wantDeltas: []llm.Delta{
				{Text: "on it"},
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "f", InputJSON: `{}`}},
			},
		},
		{
			name: "usage with its details objects",
			events: []string{
				chunk(textDelta("hi"), ""),
				chunk(`{}`, "stop"),
				usageChunk(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,` +
					`"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}`),
			},
			equivalent: completion(`{"role":"assistant","content":"hi"}`, `"stop"`,
				`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,`+
					`"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":5}}`),
			wantDeltas: []llm.Delta{{Text: "hi"}},
		},
		{
			name: "no usage chunk at all leaves usage zero",
			events: []string{
				chunk(textDelta("hi"), ""),
				chunk(`{}`, "stop"),
			},
			equivalent: completion(`{"role":"assistant","content":"hi"}`, `"stop"`, ""),
			wantDeltas: []llm.Delta{{Text: "hi"}},
		},
		{
			name: "usage on the chunk that carries the finish reason",
			events: []string{
				chunk(textDelta("hi"), ""),
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m",` +
					`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + finalUsage + `}`,
			},
			equivalent: completion(`{"role":"assistant","content":"hi"}`, `"stop"`, finalUsage),
			wantDeltas: []llm.Delta{{Text: "hi"}},
		},
		{
			name: "length",
			events: []string{
				chunk(textDelta("cut"), ""),
				chunk(`{}`, "length"),
				usageChunk(finalUsage),
			},
			equivalent: completion(`{"role":"assistant","content":"cut"}`, `"length"`, finalUsage),
			wantDeltas: []llm.Delta{{Text: "cut"}},
		},
		{
			name: "a refusal arrives in pieces and is not text",
			events: []string{
				chunk(`{"role":"assistant","content":null,"refusal":""}`, ""),
				chunk(`{"refusal":"I can't "}`, ""),
				chunk(`{"refusal":"help."}`, ""),
				chunk(`{}`, "stop"),
				usageChunk(finalUsage),
			},
			equivalent: completion(`{"role":"assistant","content":null,"refusal":"I can't help."}`, `"stop"`, finalUsage),
		},
		{
			name: "the content filter",
			events: []string{
				chunk(textDelta("partial"), ""),
				chunk(`{}`, "content_filter"),
			},
			equivalent: completion(`{"role":"assistant","content":"partial"}`, `"content_filter"`, ""),
			wantDeltas: []llm.Delta{{Text: "partial"}},
		},
		{
			name: "fields this package does not read, and comments and empty events between chunks",
			events: []string{
				`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"m","system_fingerprint":"fp","service_tier":"default",` +
					`"obfuscation":"xxxx","choices":[{"index":0,"delta":{"role":"assistant","content":"hi","function_call":null,"extra":1},` +
					`"logprobs":{"content":[]},"finish_reason":null}],"usage":null}`,
				``,
				chunk(`{}`, "stop"),
			},
			equivalent: completion(`{"role":"assistant","content":"hi"}`, `"stop"`, ""),
			wantDeltas: []llm.Delta{{Text: "hi"}},
		},
		{
			name: "an error that is empty, null or an empty object on a chunk with real choices is no error",
			events: []string{
				`{"id":"chatcmpl-1","model":"m","error":"","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","model":"m","error":{},"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
				`{"id":"chatcmpl-1","model":"m","error":null,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			equivalent: completion(`{"role":"assistant","content":"Hello"}`, `"stop"`, ""),
			wantDeltas: []llm.Delta{{Text: "Hel"}, {Text: "lo"}},
		},
		{
			name: "no id or model on any chunk",
			events: []string{
				`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
				`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			},
			equivalent: `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			wantDeltas: []llm.Delta{{Text: "hi"}},
		},
		{
			name: "no finish reason, ended by DONE: a reply of text is an end",
			events: []string{
				chunk(textDelta("Hel"), ""),
				chunk(textDelta("lo"), ""),
				usageChunk(finalUsage),
			},
			equivalent: completion(`{"role":"assistant","content":"Hello"}`, `null`, finalUsage),
			wantDeltas: []llm.Delta{{Text: "Hel"}, {Text: "lo"}},
		},
		{
			name: "no finish reason, ended by DONE: a reply that made calls is a tool use",
			events: []string{
				chunk(callsDelta(callPiece(0, "c1", "f", `{"a":`)), ""),
				chunk(callsDelta(callPiece(0, "", "", `1}`)), ""),
			},
			equivalent: completion(`{"role":"assistant","content":null,"tool_calls":[`+
				`{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]}`, `null`, ""),
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "f", InputJSON: `{"a":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `1}`}},
			},
		},
		{
			name: "no finish reason, ended by DONE: a refusal is a refusal",
			events: []string{
				chunk(`{"refusal":"I can't help."}`, ""),
			},
			equivalent: completion(`{"role":"assistant","content":null,"refusal":"I can't help."}`, `null`, ""),
		},
		{
			name: "a finish reason this package does not know, with text",
			events: []string{
				chunk(textDelta("hi"), ""),
				chunk(`{}`, "eos"),
			},
			equivalent: completion(`{"role":"assistant","content":"hi"}`, `"eos"`, ""),
			wantDeltas: []llm.Delta{{Text: "hi"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body strings.Builder
			for _, e := range tt.events {
				if e == "" {
					body.WriteString(": keep-alive\n\ndata:\n\n")
					continue
				}
				fmt.Fprintf(&body, "data: %s\n\n", e)
			}
			body.WriteString("data: [DONE]\n\n")

			streamed, deltas, err := stream(t, serveStream(body.String()))
			require.NoError(t, err)

			generated, err := generate(t, tt.equivalent)
			require.NoError(t, err)

			assert.Equal(t, generated, streamed, "the streamed reply is the unstreamed one")
			assert.Equal(t, tt.wantDeltas, deltas)
		})
	}
}

func TestStream_DocumentedExample(t *testing.T) {
	var got []llm.Delta
	srv, _ := newServer(t, serveStream(testdata(t, "chat_completion_stream.sse")))
	resp, err := newClient(t, srv).Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(d llm.Delta) error {
		got = append(got, d)
		return nil
	})
	require.NoError(t, err)

	assert.Equal(t, &llm.Response{
		ID:      "chatcmpl-123",
		Model:   "gpt-6-astra",
		Message: llm.Message{Role: llm.RoleAssistant, Text: "Hello! How can I assist you today?"},
		Stop:    llm.StopEnd,
		Usage:   llm.Usage{InputTokens: 19, OutputTokens: 10},
	}, resp)
	assert.Equal(t, []llm.Delta{{Text: "Hello"}, {Text: "! How can I assist you today?"}}, got)
}

func TestStream_MalformedArgumentsAreAJSONString(t *testing.T) {
	body := sseBody(
		chunk(callsDelta(callPiece(0, "c1", "f", `{"id":`)), ""),
		chunk(`{}`, "tool_calls"),
	)
	resp, _, err := stream(t, serveStream(body))
	require.NoError(t, err)
	assert.Equal(t, []llm.ToolCall{{ID: "c1", Name: "f", Input: json.RawMessage(`"{\"id\":"`), Malformed: true}}, resp.Message.ToolCalls)
}

func TestStream_ToolCallsAreOrderedByIndexNotByArrival(t *testing.T) {
	var events []string
	for i := 7; i >= 0; i-- {
		events = append(events, chunk(callsDelta(callPiece(i, fmt.Sprintf("call_%d", i), "f", `{}`)), ""))
	}
	events = append(events, chunk(`{}`, "tool_calls"))

	resp, _, err := stream(t, serveStream(sseBody(events...)))
	require.NoError(t, err)

	var ids []string
	for _, call := range resp.Message.ToolCalls {
		ids = append(ids, call.ID)
	}
	assert.Equal(t, []string{"call_0", "call_1", "call_2", "call_3", "call_4", "call_5", "call_6", "call_7"}, ids)
}

func TestStream_APieceThatAddsNothingIsNotDelivered(t *testing.T) {
	body := sseBody(
		chunk(callsDelta(callPiece(0, "c1", "f", `{"a":`)), ""),
		chunk(callsDelta(callPiece(0, "", "", ``)), ""),
		chunk(callsDelta(callPiece(0, "", "", `1}`)), ""),
		chunk(`{}`, "tool_calls"),
	)
	resp, deltas, err := stream(t, serveStream(body))
	require.NoError(t, err)
	assert.Equal(t, []llm.Delta{
		{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "f", InputJSON: `{"a":`}},
		{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `1}`}},
	}, deltas)
	assert.JSONEq(t, `{"a":1}`, string(resp.Message.ToolCalls[0].Input))
}

func TestStream_ToolCallsSentToTheCallbackKeepTheServersIndex(t *testing.T) {
	body := sseBody(
		chunk(callsDelta(callPiece(2, "call_c", "third", `{}`)), ""),
		chunk(`{}`, "tool_calls"),
	)
	_, deltas, err := stream(t, serveStream(body))
	require.NoError(t, err)
	assert.Equal(t, []llm.Delta{{ToolCall: &llm.ToolCallDelta{Index: 2, ID: "call_c", Name: "third", InputJSON: `{}`}}}, deltas)
}

func TestStream_CallbackErrorStopsTheStream(t *testing.T) {
	errStop := errors.New("caller has had enough")
	closed := make(chan struct{})
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, word := range []string{"one", "two", "three"} {
			fmt.Fprintf(w, "data: %s\n\n", chunk(textDelta(word), ""))
			flusher.Flush()
		}
		// Hold the stream open: only the client hanging up ends it.
		<-r.Context().Done()
		close(closed)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var calls int
	resp, err := newClient(t, srv).Stream(ctx, llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error {
		calls++
		if calls == 2 {
			return errStop
		}
		return nil
	})

	require.ErrorIs(t, err, errStop)
	assert.Nil(t, resp)
	assert.Equal(t, 2, calls, "no increment is delivered after the callback has failed")
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection was left open after the callback failed")
	}
}

func TestStream_ACancelledContextIsItsOwnError(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", chunk(textDelta("one"), ""))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := newClient(t, srv).Stream(ctx, llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error {
		cancel()
		return nil
	})

	require.ErrorIs(t, err, context.Canceled)
	notAnLLMError(t, err)
}

func TestStream_ErrorChunk(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantType  string
		wantMsg   string
		wantRetry bool
	}{
		{
			name:     "a request the server rejected after it started to answer",
			payload:  `{"error":{"message":"maximum context length exceeded","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			wantType: "invalid_request_error",
			wantMsg:  "maximum context length exceeded",
		},
		{
			name:      "a model that became overloaded",
			payload:   `{"error":{"message":"overloaded","type":"service_unavailable_error","param":null,"code":"server_is_overloaded"}}`,
			wantType:  "service_unavailable_error",
			wantMsg:   "overloaded",
			wantRetry: true,
		},
		{
			name:      "overloaded, by the code alone",
			payload:   `{"error":{"message":"slow down","type":"error","param":null,"code":"slow_down"}}`,
			wantType:  "error",
			wantMsg:   "slow down",
			wantRetry: true,
		},
		{
			name:      "a rate limit, by its type",
			payload:   `{"error":{"message":"slow","type":"rate_limit_error","param":null,"code":null}}`,
			wantType:  "rate_limit_error",
			wantMsg:   "slow",
			wantRetry: true,
		},
		{
			name:      "a rate limit, by its code",
			payload:   `{"error":{"message":"slow","type":"error","param":null,"code":"rate_limit_exceeded"}}`,
			wantType:  "error",
			wantMsg:   "slow",
			wantRetry: true,
		},
		{
			name:      "a server error, by its type",
			payload:   `{"error":{"message":"oops","type":"server_error","param":null,"code":null}}`,
			wantType:  "server_error",
			wantMsg:   "oops",
			wantRetry: true,
		},
		{
			name:      "a server error, by its code",
			payload:   `{"error":{"message":"oops","type":"error","param":null,"code":"server_error"}}`,
			wantType:  "error",
			wantMsg:   "oops",
			wantRetry: true,
		},
		{
			name:     "a rate limit that is a spend limit is final",
			payload:  `{"error":{"message":"limit","type":"rate_limit_error","param":null,"code":"project_spend_limit_exceeded"}}`,
			wantType: "rate_limit_error",
			wantMsg:  "limit",
		},
		{
			name:     "a code that is a number",
			payload:  `{"error":{"object":"error","message":"bad input","type":"BadRequestError","param":null,"code":400}}`,
			wantType: "BadRequestError",
			wantMsg:  "bad input",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := sseBody(chunk(textDelta("one"), ""), tt.payload)
			_, deltas, err := stream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-Id", "req_9")
				serveStream(body)(w, r)
			})

			var apiErr *llm.Error
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, "openai", apiErr.Provider)
			assert.Equal(t, tt.wantType, apiErr.Type)
			assert.Equal(t, tt.wantMsg, apiErr.Message)
			assert.Equal(t, tt.wantRetry, apiErr.Retryable)
			assert.Equal(t, tt.wantRetry, llm.Retryable(err))
			assert.Equal(t, "req_9", apiErr.RequestID)
			assert.Equal(t, http.StatusOK, apiErr.Status)
			assert.NoError(t, apiErr.Err, "the server answered, so this is not a transport failure")
			assert.Equal(t, []llm.Delta{{Text: "one"}}, deltas, "what arrived before the error was delivered")
		})
	}
}

// Each case is a stream stopped at one point. A reply is complete once a chunk
// has carried a finish reason, whatever follows, and also when the stream ends
// with a clean DONE even though no chunk did, since some servers never send
// one. A stream that stops with neither is a failure of the connection.
func TestStream_CutShort(t *testing.T) {
	head := "data: " + chunk(`{"role":"assistant","content":""}`, "") + "\n\n" +
		"data: " + chunk(textDelta("Hel"), "") + "\n\n"
	finish := "data: " + chunk(`{}`, "stop") + "\n\n"
	usage := "data: " + usageChunk(finalUsage) + "\n\n"

	partial := &llm.Response{
		ID: "chatcmpl-1", Model: "m",
		Message: llm.Message{Role: llm.RoleAssistant, Text: "Hel"},
		Stop:    llm.StopEnd,
	}
	withUsage := *partial
	withUsage.Usage = llm.Usage{InputTokens: 9, OutputTokens: 2}

	tests := []struct {
		name string
		body string
		// want is the reply when the stream had already finished; nil when
		// the cut is a failure.
		want *llm.Response
	}{
		{name: "before the finish chunk, between events, no DONE", body: head},
		{name: "before the finish chunk, in the middle of a data line", body: head + `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"lo`},
		{name: "in the middle of the finish chunk itself, before its blank line", body: head + "data: " + chunk(`{}`, "stop") + "\n"},
		{name: "in the middle of the finish chunk's name", body: head + "dat"},
		{name: "in the middle of DONE, with no finish chunk", body: head + "data: [DO"},
		{name: "DONE with no blank line, so the event is never delivered, and no finish chunk", body: head + "data: [DONE]\n"},
		{name: "after the usage chunk, with neither a finish chunk nor DONE", body: head + usage},
		{name: "nothing but DONE", body: "data: [DONE]\n\n"},
		{name: "an empty stream", body: ""},
		{name: "only comments", body: ": keep-alive\n\n"},

		{name: "after the finish chunk, between events, no usage, no DONE", body: head + finish, want: partial},
		{name: "no finish chunk, but a clean DONE: the server sends none", body: head + "data: [DONE]\n\n", want: partial},
		{name: "no finish chunk, a usage chunk and then a clean DONE", body: head + usage + "data: [DONE]\n\n", want: &withUsage},
		{name: "after the finish chunk, in the middle of the usage chunk", body: head + finish + `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tok`, want: partial},
		{name: "after the finish chunk, in the middle of DONE", body: head + finish + "data: [DO", want: partial},
		{name: "after the finish chunk, DONE arrived cleanly, no usage", body: head + finish + "data: [DONE]\n\n", want: partial},
		{name: "after the finish chunk and the usage chunk, no DONE", body: head + finish + usage, want: &withUsage},
		{name: "after the finish chunk and the usage chunk, DONE with no blank line", body: head + finish + usage + "data: [DONE]\n", want: &withUsage},
		{name: "after the finish chunk and the usage chunk, DONE complete", body: head + finish + usage + "data: [DONE]\n\n", want: &withUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _, err := stream(t, serveStream(tt.body))

			if tt.want != nil {
				require.NoError(t, err)
				assert.Equal(t, tt.want, resp)
				return
			}
			assertTransportFailure(t, err)
			assert.Nil(t, resp)
		})
	}
}

// A body the transport itself reports as truncated is the same case as one
// the reader reports: the server promised more bytes than it sent.
func TestStream_BodyTruncatedByTheTransport(t *testing.T) {
	head := "data: " + chunk(textDelta("Hel"), "") + "\n\n"
	finish := "data: " + chunk(`{}`, "stop") + "\n\n"
	usage := "data: " + usageChunk(finalUsage) + "\n\n"

	tests := []struct {
		name     string
		body     string
		wantText string // empty when the cut is a failure
	}{
		{name: "before the finish chunk", body: head},
		{name: "in the middle of a data line", body: head + `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"lo`},
		{name: "after the finish chunk", body: head + finish, wantText: "Hel"},
		{name: "after the finish chunk, in the middle of the usage chunk", body: head + finish + `data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_`, wantText: "Hel"},
		{name: "after the usage chunk", body: head + finish + usage, wantText: "Hel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, _, err := stream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(tt.body)+64))
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tt.body)
			})

			if tt.wantText == "" {
				assertTransportFailure(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantText, resp.Message.Text)
			assert.Equal(t, llm.StopEnd, resp.Stop)
		})
	}
}

func TestStream_ACutAfterSomeDeltasStillDeliveredThem(t *testing.T) {
	body := "data: " + chunk(textDelta("Hel"), "") + "\n\n"
	_, deltas, err := stream(t, serveStream(body))
	assertTransportFailure(t, err)
	assert.Equal(t, []llm.Delta{{Text: "Hel"}}, deltas, "what arrived before the cut was delivered, so a caller must not replay it into the same callback blindly")
}

// assertTransportFailure checks the error a stream cut before its finish
// reason must be: a failure of the connection, which the same request may
// get past.
func assertTransportFailure(t *testing.T, err error) {
	t.Helper()
	var apiErr *llm.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "openai", apiErr.Provider)
	require.Error(t, apiErr.Err, "a transport failure carries its cause")
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.True(t, apiErr.Retryable)
	assert.True(t, llm.Retryable(err))
}

func TestStream_ACutAfterTheFinishReasonIsLogged(t *testing.T) {
	logger, written := logged()
	body := "data: " + chunk(textDelta("Hel"), "") + "\n\n" + "data: " + chunk(`{}`, "stop") + "\n\n" + "data: [DO"
	srv, _ := newServer(t, serveStream(body))

	resp, err := newClient(t, srv, func(o *openai.Options) { o.Logger = logger }).
		Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

	require.NoError(t, err)
	assert.Equal(t, "Hel", resp.Message.Text)
	assert.Contains(t, written(), "stream cut after its finish reason")
}

func TestStream_ACleanEndIsNotLogged(t *testing.T) {
	logger, written := logged()
	srv, _ := newServer(t, serveStream(sseBody(chunk(textDelta("Hel"), ""), chunk(`{}`, "stop"), usageChunk(finalUsage))))

	_, err := newClient(t, srv, func(o *openai.Options) { o.Logger = logger }).
		Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

	require.NoError(t, err)
	assert.Empty(t, written())
}

func TestStream_NoFinishReasonButACleanDone(t *testing.T) {
	t.Run("a reply of text ends as an end", func(t *testing.T) {
		resp, _, err := stream(t, serveStream(sseBody(chunk(textDelta("hi"), ""))))
		require.NoError(t, err)
		assert.Equal(t, llm.StopEnd, resp.Stop)
		assert.Equal(t, "hi", resp.Message.Text)
	})
	t.Run("a reply that made calls ends as a tool use, from the calls assembled", func(t *testing.T) {
		resp, _, err := stream(t, serveStream(sseBody(chunk(callsDelta(callPiece(0, "c1", "f", `{}`)), ""))))
		require.NoError(t, err)
		assert.Equal(t, llm.StopToolUse, resp.Stop)
		require.Len(t, resp.Message.ToolCalls, 1)
	})
}

// A stream that says nothing before DONE has not answered. A stream whose
// server said it finished, with a finish reason, and said no more than that, is
// an empty reply.
func TestStream_ADoneThatFollowsNothing(t *testing.T) {
	failures := []struct {
		name string
		body string
	}{
		{name: "nothing but DONE", body: "data: [DONE]\n\n"},
		{name: "comments and empty events, then DONE", body: ": keep-alive\n\ndata:\n\ndata: [DONE]\n\n"},
		{name: "a usage chunk with no id and no choice, then DONE", body: sseBody(`{"object":"chat.completion.chunk","choices":[],"usage":` + finalUsage + `}`)},
	}
	for _, tt := range failures {
		t.Run(tt.name, func(t *testing.T) {
			resp, deltas, err := stream(t, serveStream(tt.body))
			assertTransportFailure(t, err)
			assert.Nil(t, resp)
			assert.Empty(t, deltas)
		})
	}

	replies := []struct {
		name string
		body string
		want *llm.Response
	}{
		{
			name: "a finish reason and no content is an empty reply",
			body: sseBody(chunk(`{}`, "stop")),
			want: &llm.Response{ID: "chatcmpl-1", Model: "m", Message: llm.Message{Role: llm.RoleAssistant}, Stop: llm.StopEnd},
		},
		{
			name: "a finish reason of length and no content is an empty reply, cut at the bound",
			body: sseBody(chunk(`{}`, "length")),
			want: &llm.Response{ID: "chatcmpl-1", Model: "m", Message: llm.Message{Role: llm.RoleAssistant}, Stop: llm.StopMaxTokens},
		},
		{
			name: "a chunk with an id and an empty delta, then DONE, is the server's empty reply",
			body: sseBody(chunk(`{"role":"assistant","content":""}`, "")),
			want: &llm.Response{ID: "chatcmpl-1", Model: "m", Message: llm.Message{Role: llm.RoleAssistant}, Stop: llm.StopEnd},
		},
		{
			name: "a usage chunk that has an id, then DONE",
			body: sseBody(usageChunk(finalUsage)),
			want: &llm.Response{ID: "chatcmpl-1", Model: "m", Message: llm.Message{Role: llm.RoleAssistant}, Stop: llm.StopEnd, Usage: llm.Usage{InputTokens: 9, OutputTokens: 2}},
		},
	}
	for _, tt := range replies {
		t.Run(tt.name, func(t *testing.T) {
			resp, deltas, err := stream(t, serveStream(tt.body))
			require.NoError(t, err)
			assert.Equal(t, tt.want, resp)
			assert.Empty(t, deltas)
		})
	}
}

// The reply carries no field for it, so the missing finish reason is a debug
// line, and a finish reason that did arrive logs nothing.
func TestStream_NoFinishReasonIsLoggedAtDebug(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantLog string
	}{
		{name: "none sent", body: sseBody(chunk(textDelta("hi"), "")), wantLog: "no finish_reason"},
		{name: "one sent", body: sseBody(chunk(textDelta("hi"), ""), chunk(`{}`, "stop"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, written := logged()
			srv, _ := newServer(t, serveStream(tt.body))

			_, err := newClient(t, srv, func(o *openai.Options) { o.Logger = logger }).
				Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

			require.NoError(t, err)
			if tt.wantLog == "" {
				assert.Empty(t, written())
				return
			}
			assert.Contains(t, written(), tt.wantLog)
			assert.Contains(t, written(), "level=DEBUG")
		})
	}
}

// rawPiece is a piece of a tool call from a server that sends no index.
func rawPiece(id, name, args string) string {
	fn := `"arguments":` + strconv.Quote(args)
	if name != "" {
		fn = `"name":` + strconv.Quote(name) + "," + fn
	}
	head := ""
	if id != "" {
		head = `"id":` + strconv.Quote(id) + `,"type":"function",`
	}
	return `{` + head + `"function":{` + fn + `}}`
}

// With no index to tell calls apart, a new id is a new call and a piece with
// none belongs to the latest, so distinct calls are not merged into one
// malformed call.
func TestStream_ToolCallPiecesWithNoIndex(t *testing.T) {
	tests := []struct {
		name       string
		events     []string
		wantCalls  []llm.ToolCall
		wantDeltas []llm.Delta
	}{
		{
			name: "two calls, each in pieces",
			events: []string{
				chunk(callsDelta(rawPiece("call_a", "first", `{"x":`)), ""),
				chunk(callsDelta(rawPiece("", "", `1}`)), ""),
				chunk(callsDelta(rawPiece("call_b", "second", `{"y":`)), ""),
				chunk(callsDelta(rawPiece("", "", `2}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			wantCalls: []llm.ToolCall{
				{ID: "call_a", Name: "first", Input: json.RawMessage(`{"x":1}`)},
				{ID: "call_b", Name: "second", Input: json.RawMessage(`{"y":2}`)},
			},
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_a", Name: "first", InputJSON: `{"x":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `1}`}},
				{ToolCall: &llm.ToolCallDelta{Index: 1, ID: "call_b", Name: "second", InputJSON: `{"y":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 1, InputJSON: `2}`}},
			},
		},
		{
			name: "two whole calls, one per chunk",
			events: []string{
				chunk(callsDelta(rawPiece("call_a", "first", `{}`)), ""),
				chunk(callsDelta(rawPiece("call_b", "second", `{}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			wantCalls: []llm.ToolCall{
				{ID: "call_a", Name: "first", Input: json.RawMessage(`{}`)},
				{ID: "call_b", Name: "second", Input: json.RawMessage(`{}`)},
			},
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_a", Name: "first", InputJSON: `{}`}},
				{ToolCall: &llm.ToolCallDelta{Index: 1, ID: "call_b", Name: "second", InputJSON: `{}`}},
			},
		},
		{
			name: "one call that repeats its id on every piece stays one call",
			events: []string{
				chunk(callsDelta(rawPiece("call_a", "first", `{"x":`)), ""),
				chunk(callsDelta(rawPiece("call_a", "", `1}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			wantCalls: []llm.ToolCall{{ID: "call_a", Name: "first", Input: json.RawMessage(`{"x":1}`)}},
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_a", Name: "first", InputJSON: `{"x":`}},
				{ToolCall: &llm.ToolCallDelta{Index: 0, InputJSON: `1}`}},
			},
		},
		{
			name: "an index that is given but reused by a call with another id is a new call",
			events: []string{
				chunk(callsDelta(callPiece(0, "call_a", "first", `{}`)), ""),
				chunk(callsDelta(callPiece(0, "call_b", "second", `{}`)), ""),
				chunk(`{}`, "tool_calls"),
			},
			wantCalls: []llm.ToolCall{
				{ID: "call_a", Name: "first", Input: json.RawMessage(`{}`)},
				{ID: "call_b", Name: "second", Input: json.RawMessage(`{}`)},
			},
			wantDeltas: []llm.Delta{
				{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "call_a", Name: "first", InputJSON: `{}`}},
				{ToolCall: &llm.ToolCallDelta{Index: 1, ID: "call_b", Name: "second", InputJSON: `{}`}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, deltas, err := stream(t, serveStream(sseBody(tt.events...)))
			require.NoError(t, err)
			assert.Equal(t, tt.wantCalls, resp.Message.ToolCalls)
			assert.Equal(t, tt.wantDeltas, deltas)
		})
	}
}

// A chunk with a choice began a reply, whether or not the server gave it an id.
func TestStream_AChoiceWithNoIdThenDone(t *testing.T) {
	body := sseBody(`{"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`)

	resp, deltas, err := stream(t, serveStream(body))

	require.NoError(t, err)
	assert.Equal(t, &llm.Response{Model: "test-model", Message: llm.Message{Role: llm.RoleAssistant, Text: "hi"}, Stop: llm.StopEnd}, resp)
	assert.Equal(t, []llm.Delta{{Text: "hi"}}, deltas)
}
