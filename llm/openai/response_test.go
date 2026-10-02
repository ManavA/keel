package openai_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

// completion builds a chat.completion body around one choice. message is a
// JSON object, finish a JSON literal ("null" for none) and usage a JSON
// object, or empty to leave the field out.
func completion(message, finish, usage string) string {
	body := fmt.Sprintf(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m",`+
		`"choices":[{"index":0,"message":%s,"logprobs":null,"finish_reason":%s}]`, message, finish)
	if usage != "" {
		body += `,"usage":` + usage
	}
	return body + "}"
}

func generate(t *testing.T, body string, mods ...func(*openai.Options)) (*llm.Response, error) {
	t.Helper()
	srv, _ := newServer(t, serveJSON(body))
	return newClient(t, srv, mods...).Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
}

func TestGenerate_DocumentedExamples(t *testing.T) {
	tests := []struct {
		name string
		file string
		want *llm.Response
	}{
		{
			name: "text reply",
			file: "chat_completion.json",
			want: &llm.Response{
				ID:      "chatcmpl-B9MBs8CjcvOU2jLn4n570S5qMJKcT",
				Model:   "gpt-6-astra",
				Message: llm.Message{Role: llm.RoleAssistant, Text: "Hello! How can I assist you today?"},
				Stop:    llm.StopEnd,
				Usage:   llm.Usage{InputTokens: 19, OutputTokens: 10},
			},
		},
		{
			name: "tool call, with the arguments laid out as the reference writes them",
			file: "chat_completion_tool_calls.json",
			want: &llm.Response{
				ID:    "chatcmpl-abc123",
				Model: "gpt-6-astra",
				Message: llm.Message{
					Role: llm.RoleAssistant,
					ToolCalls: []llm.ToolCall{{
						ID:    "call_abc123",
						Name:  "get_current_weather",
						Input: json.RawMessage("{\n\"location\": \"Boston, MA\"\n}"),
					}},
				},
				Stop:  llm.StopToolUse,
				Usage: llm.Usage{InputTokens: 82, OutputTokens: 17},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := generate(t, testdata(t, tt.file))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGenerate_Text(t *testing.T) {
	tests := []struct {
		name string
		body string
		req  llm.Request
		want *llm.Response
	}{
		{
			name: "content null is empty text",
			body: completion(`{"role":"assistant","content":null}`, `"stop"`, ""),
			want: &llm.Response{ID: "chatcmpl-1", Model: "m", Message: llm.Message{Role: llm.RoleAssistant}, Stop: llm.StopEnd},
		},
		{
			name: "id and model missing from the body, the model asked for is reported",
			body: `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			want: &llm.Response{Model: "test-model", Message: llm.Message{Role: llm.RoleAssistant, Text: "hi"}, Stop: llm.StopEnd},
		},
		{
			name: "a model the request names is the one reported when the body names none",
			body: `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			req:  llm.Request{Model: "asked-for"},
			want: &llm.Response{Model: "asked-for", Message: llm.Message{Role: llm.RoleAssistant, Text: "hi"}, Stop: llm.StopEnd},
		},
		{
			name: "fields this package does not read are ignored",
			body: `{"id":"x","object":"chat.completion","created":1,"model":"m","service_tier":"default","system_fingerprint":"fp",` +
				`"choices":[{"index":0,"logprobs":{"content":[]},"finish_reason":"stop","message":{"role":"assistant",` +
				`"content":"hi","annotations":[{"type":"url_citation"}],"audio":null,"unknown":{"a":1}}}],` +
				`"moderation":{"input":null,"output":null}}`,
			want: &llm.Response{ID: "x", Model: "m", Message: llm.Message{Role: llm.RoleAssistant, Text: "hi"}, Stop: llm.StopEnd},
		},
		{
			name: "only the first choice is read",
			body: `{"id":"x","model":"m","choices":[` +
				`{"index":0,"message":{"role":"assistant","content":"first"},"finish_reason":"stop"},` +
				`{"index":1,"message":{"role":"assistant","content":"second"},"finish_reason":"stop"}]}`,
			want: &llm.Response{ID: "x", Model: "m", Message: llm.Message{Role: llm.RoleAssistant, Text: "first"}, Stop: llm.StopEnd},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newServer(t, serveJSON(tt.body))
			req := tt.req
			req.Messages = userMsg("hi")
			got, err := newClient(t, srv).Generate(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Nil(t, got.Message.Opaque, "this protocol has no provider form of a turn")
		})
	}
}

func TestGenerate_ToolCalls(t *testing.T) {
	fn := func(id, name, args string) string {
		quoted, _ := json.Marshal(args)
		return fmt.Sprintf(`{"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}`, id, name, quoted)
	}
	reply := func(calls ...string) string {
		return completion(`{"role":"assistant","content":null,"tool_calls":[`+strings.Join(calls, ",")+`]}`, `"tool_calls"`, "")
	}

	tests := []struct {
		name string
		body string
		want []llm.ToolCall
	}{
		{
			name: "one call",
			body: reply(fn("call_1", "read_document", `{"id":7}`)),
			want: []llm.ToolCall{{ID: "call_1", Name: "read_document", Input: json.RawMessage(`{"id":7}`)}},
		},
		{
			name: "two calls keep their order",
			body: reply(fn("call_b", "second", `{}`), fn("call_a", "first", `{}`)),
			want: []llm.ToolCall{
				{ID: "call_b", Name: "second", Input: json.RawMessage(`{}`)},
				{ID: "call_a", Name: "first", Input: json.RawMessage(`{}`)},
			},
		},
		{
			name: "arguments are the bytes received, in the order and spacing received",
			body: reply(fn("c", "f", `{ "zeta": 1,  "alpha": [2, 3] }`)),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`{ "zeta": 1,  "alpha": [2, 3] }`)}},
		},
		{
			name: "arguments that are not valid JSON are one JSON string and the call is malformed",
			body: reply(fn("c", "f", `{"id":`)),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`"{\"id\":"`), Malformed: true}},
		},
		{
			name: "prose where arguments belong is malformed too",
			body: reply(fn("c", "f", "I would call it with the document id")),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`"I would call it with the document id"`), Malformed: true}},
		},
		{
			name: "empty arguments are an empty object, not malformed",
			body: reply(fn("c", "f", ``)),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`{}`)}},
		},
		{
			name: "blank arguments are an empty object, not malformed",
			body: reply(fn("c", "f", " \n")),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`{}`)}},
		},
		{
			name: "a call with no type field is still a function call",
			body: completion(`{"role":"assistant","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]}`, `"tool_calls"`, ""),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`{}`)}},
		},
		{
			name: "a custom tool call, which this package never asks for, is not turned into a function call",
			body: completion(`{"role":"assistant","tool_calls":[`+
				`{"id":"x","type":"custom","custom":{"name":"n","input":"i"}},`+fn("c", "f", `{}`)+`]}`, `"tool_calls"`, ""),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`{}`)}},
		},
		{
			name: "text beside a call is kept",
			body: completion(`{"role":"assistant","content":"on it","tool_calls":[`+fn("c", "f", `{}`)+`]}`, `"tool_calls"`, ""),
			want: []llm.ToolCall{{ID: "c", Name: "f", Input: json.RawMessage(`{}`)}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := generate(t, tt.body)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Message.ToolCalls)
			assert.Equal(t, llm.RoleAssistant, got.Message.Role)
			for _, call := range got.Message.ToolCalls {
				assert.True(t, json.Valid(call.Input), "Input is always valid JSON: %q", call.Input)
			}
		})
	}
}

func TestGenerate_NoToolCallsIsANilSlice(t *testing.T) {
	got, err := generate(t, completion(`{"role":"assistant","content":"hi","tool_calls":[]}`, `"stop"`, ""))
	require.NoError(t, err)
	assert.Nil(t, got.Message.ToolCalls)
}

func TestGenerate_Refusal(t *testing.T) {
	tests := []struct {
		name        string
		message     string
		finish      string
		wantStop    llm.StopReason
		wantRefusal *llm.Refusal
		wantText    string
	}{
		{
			name:        "a refusal string ends as a refusal whatever the finish reason says",
			message:     `{"role":"assistant","content":null,"refusal":"I can't help with that."}`,
			finish:      `"stop"`,
			wantStop:    llm.StopRefusal,
			wantRefusal: &llm.Refusal{Explanation: "I can't help with that."},
		},
		{
			name:     "a refusal null is no refusal",
			message:  `{"role":"assistant","content":"fine","refusal":null}`,
			finish:   `"stop"`,
			wantStop: llm.StopEnd,
			wantText: "fine",
		},
		{
			name:     "an empty refusal string is no refusal",
			message:  `{"role":"assistant","content":"fine","refusal":""}`,
			finish:   `"stop"`,
			wantStop: llm.StopEnd,
			wantText: "fine",
		},
		{
			name:        "the content filter is a refusal with a category and no explanation",
			message:     `{"role":"assistant","content":"cut off"}`,
			finish:      `"content_filter"`,
			wantStop:    llm.StopRefusal,
			wantRefusal: &llm.Refusal{Category: "content_filter"},
			wantText:    "cut off",
		},
		{
			name:        "the content filter and a refusal string together",
			message:     `{"role":"assistant","content":null,"refusal":"no"}`,
			finish:      `"content_filter"`,
			wantStop:    llm.StopRefusal,
			wantRefusal: &llm.Refusal{Category: "content_filter", Explanation: "no"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := generate(t, completion(tt.message, tt.finish, ""))
			require.NoError(t, err, "a refusal is a reply, not an error")
			assert.Equal(t, tt.wantStop, got.Stop)
			assert.Equal(t, tt.wantRefusal, got.Refusal)
			assert.Equal(t, tt.wantText, got.Message.Text)
		})
	}
}

func TestGenerate_FinishReasons(t *testing.T) {
	const withCall = `{"role":"assistant","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}`
	const textOnly = `{"role":"assistant","content":"hi"}`

	tests := []struct {
		name    string
		message string
		finish  string // a JSON literal; "" leaves the key out
		want    llm.StopReason
	}{
		{name: "stop", message: textOnly, finish: `"stop"`, want: llm.StopEnd},
		{name: "length", message: textOnly, finish: `"length"`, want: llm.StopMaxTokens},
		{name: "tool_calls", message: withCall, finish: `"tool_calls"`, want: llm.StopToolUse},
		{name: "function_call, the deprecated name for tool_calls", message: withCall, finish: `"function_call"`, want: llm.StopToolUse},
		{name: "tool_calls is a tool use whatever the message holds", message: textOnly, finish: `"tool_calls"`, want: llm.StopToolUse},
		{name: "function_call is a tool use whatever the message holds", message: textOnly, finish: `"function_call"`, want: llm.StopToolUse},
		{name: "content_filter", message: textOnly, finish: `"content_filter"`, want: llm.StopRefusal},
		{name: "null with calls was a tool use", message: withCall, finish: `null`, want: llm.StopToolUse},
		{name: "null with text was the end", message: textOnly, finish: `null`, want: llm.StopEnd},
		{name: "missing with calls was a tool use", message: withCall, finish: ``, want: llm.StopToolUse},
		{name: "missing with text was the end", message: textOnly, finish: ``, want: llm.StopEnd},
		{name: "a reason this package does not know, with calls", message: withCall, finish: `"eos"`, want: llm.StopToolUse},
		{name: "a reason this package does not know, with text", message: textOnly, finish: `"eos"`, want: llm.StopEnd},
		{name: "stop beside calls stays the end, as the table says", message: withCall, finish: `"stop"`, want: llm.StopEnd},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := completion(tt.message, `null`, "")
			if tt.finish == "" {
				body = strings.Replace(body, `,"finish_reason":null`, "", 1)
			} else {
				body = strings.Replace(body, `"finish_reason":null`, `"finish_reason":`+tt.finish, 1)
			}
			got, err := generate(t, body)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Stop)
		})
	}
}

func TestGenerate_Usage(t *testing.T) {
	tests := []struct {
		name  string
		usage string // a JSON object, or "" to leave the field out
		want  llm.Usage
	}{
		{name: "no usage object", usage: "", want: llm.Usage{}},
		{name: "plain counts", usage: `{"prompt_tokens":19,"completion_tokens":10,"total_tokens":29}`, want: llm.Usage{InputTokens: 19, OutputTokens: 10}},
		{
			name:  "details objects present and empty",
			usage: `{"prompt_tokens":19,"completion_tokens":10,"prompt_tokens_details":{},"completion_tokens_details":{}}`,
			want:  llm.Usage{InputTokens: 19, OutputTokens: 10},
		},
		{
			name:  "details objects null",
			usage: `{"prompt_tokens":19,"completion_tokens":10,"prompt_tokens_details":null,"completion_tokens_details":null}`,
			want:  llm.Usage{InputTokens: 19, OutputTokens: 10},
		},
		{
			name:  "details the reference's own example shows, all zero",
			usage: `{"prompt_tokens":19,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`,
			want:  llm.Usage{InputTokens: 19, OutputTokens: 10},
		},
		{
			name:  "cached tokens are read from the cache, not billed as input",
			usage: `{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}`,
			want:  llm.Usage{InputTokens: 70, OutputTokens: 20, CacheReadTokens: 30},
		},
		{
			name:  "cache writes are not billed as input either",
			usage: `{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cache_write_tokens":20}}`,
			want:  llm.Usage{InputTokens: 80, OutputTokens: 20, CacheWriteTokens: 20},
		},
		{
			name:  "cached and written together",
			usage: `{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":20}}`,
			want:  llm.Usage{InputTokens: 50, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 20},
		},
		{
			name:  "cached tokens never take input below zero",
			usage: `{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":30}}`,
			want:  llm.Usage{InputTokens: 0, OutputTokens: 2, CacheReadTokens: 30},
		},
		{
			name:  "cached and written beyond the prompt never take input below zero",
			usage: `{"prompt_tokens":40,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":30,"cache_write_tokens":30}}`,
			want:  llm.Usage{InputTokens: 0, OutputTokens: 2, CacheReadTokens: 30, CacheWriteTokens: 30},
		},
		{
			name:  "reasoning tokens are part of the output, counted again on their own",
			usage: `{"prompt_tokens":10,"completion_tokens":50,"completion_tokens_details":{"reasoning_tokens":20}}`,
			want:  llm.Usage{InputTokens: 10, OutputTokens: 50, ReasoningTokens: 20},
		},
		{
			name:  "total_tokens is not read",
			usage: `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":9999}`,
			want:  llm.Usage{InputTokens: 10, OutputTokens: 5},
		},
		{
			name:  "a server that sends only some of the counts",
			usage: `{"completion_tokens":5}`,
			want:  llm.Usage{OutputTokens: 5},
		},
		{name: "usage null", usage: "null", want: llm.Usage{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := generate(t, completion(`{"role":"assistant","content":"hi"}`, `"stop"`, tt.usage))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Usage)
			assert.Empty(t, got.Attempts, "one model ran")
		})
	}
}

func TestGenerate_RefusesAReplyItCannotRead(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "no choices", body: `{"id":"x","model":"m","choices":[]}`, wantErr: "no choices"},
		{name: "no choices key", body: `{"id":"x","model":"m"}`, wantErr: "no choices"},
		{name: "not JSON", body: `<html>welcome to the gateway</html>`, wantErr: "decode"},
		{name: "a JSON array", body: `[1,2]`, wantErr: "decode"},
		{name: "empty", body: ``, wantErr: "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := generate(t, tt.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.False(t, llm.Retryable(err), "the same request will get the same unreadable answer")
		})
	}
}

func TestGenerate_ResponseBodyIsBounded(t *testing.T) {
	const limit = 32 << 20
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"`)
		chunk := strings.Repeat("a", 1<<20)
		for range limit/len(chunk) + 1 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	})

	_, err := newClient(t, srv).Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than")
	assert.False(t, llm.Retryable(err))
}

// The case that fails if the mapping is wrong: a reply is sent back as the
// next request's assistant turn, and the server must see the arguments it
// wrote, byte for byte, including the ones that were not JSON.
func TestGenerate_AnAssistantTurnGoesBackAsItCame(t *testing.T) {
	const wellFormed = "{ \"zeta\": 1,\n \"alpha\": 2 }"
	const notJSON = `{"id":`
	first := completion(`{"role":"assistant","content":"looking","tool_calls":[`+
		`{"id":"call_1","type":"function","function":{"name":"read_document","arguments":"{ \"zeta\": 1,\n \"alpha\": 2 }"}},`+
		`{"id":"call_2","type":"function","function":{"name":"read_document","arguments":"{\"id\":"}}]}`, `"tool_calls"`, "")

	var n atomic.Int32
	srv, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			serveJSON(first)(w, r)
			return
		}
		serveJSON(simpleReply)(w, r)
	})
	c := newClient(t, srv)

	got, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("read both")})
	require.NoError(t, err)
	require.Len(t, got.Message.ToolCalls, 2)
	assert.False(t, got.Message.ToolCalls[0].Malformed)
	assert.True(t, got.Message.ToolCalls[1].Malformed)

	next := append(userMsg("read both"), got.Message, llm.Message{
		Role: llm.RoleTool,
		ToolResults: []llm.ToolResult{
			{CallID: "call_1", Content: "the text"},
			{CallID: "call_2", Content: "arguments were not valid JSON", IsError: true},
		},
	})
	_, err = c.Generate(t.Context(), llm.Request{Messages: next})
	require.NoError(t, err)

	body := rec.all()[1].JSON(t)
	messages, ok := body["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 4)

	assistant, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "looking", assistant["content"])
	calls, ok := assistant["tool_calls"].([]any)
	require.True(t, ok)
	require.Len(t, calls, 2)
	args := func(i int) any {
		call, _ := calls[i].(map[string]any)
		fn, _ := call["function"].(map[string]any)
		return fn["arguments"]
	}
	assert.Equal(t, wellFormed, args(0))
	assert.Equal(t, notJSON, args(1))

	assert.JSONEq(t, `{"role":"tool","tool_call_id":"call_1","content":"the text"}`, mustJSON(t, messages[2]))
	assert.JSONEq(t, `{"role":"tool","tool_call_id":"call_2","content":"ERROR: arguments were not valid JSON"}`, mustJSON(t, messages[3]))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestGenerate_AnUnknownFinishReasonIsLoggedAndNotAnError(t *testing.T) {
	logger, written := logged()

	got, err := generate(t, completion(`{"role":"assistant","content":"hi"}`, `"eos"`, ""), func(o *openai.Options) { o.Logger = logger })

	require.NoError(t, err)
	assert.Equal(t, llm.StopEnd, got.Stop)
	assert.Contains(t, written(), `finish_reason=eos`)
}

func TestGenerate_AMissingFinishReasonIsNotLogged(t *testing.T) {
	logger, written := logged()

	_, err := generate(t, `{"choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`, func(o *openai.Options) { o.Logger = logger })

	require.NoError(t, err)
	assert.Empty(t, written())
}

// An error object in a 200 body is an error, not an unreadable reply. It is
// retryable only when it says it passes.
func TestGenerate_AnErrorInsideASuccessfulResponse(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantType  string
		wantMsg   string
		wantRetry bool
	}{
		{
			name:     "final",
			body:     apiErr("invalid_request_error", "context_length_exceeded", "too long"),
			wantType: "invalid_request_error", wantMsg: "too long",
		},
		{
			name:     "overloaded, by its type",
			body:     apiErr("service_unavailable_error", "", "busy"),
			wantType: "service_unavailable_error", wantMsg: "busy", wantRetry: true,
		},
		{
			name:     "overloaded, by its code",
			body:     apiErr("error", "server_is_overloaded", "busy"),
			wantType: "error", wantMsg: "busy", wantRetry: true,
		},
		{
			name:     "an error with no message",
			body:     `{"error":{"type":"error"}}`,
			wantType: "error", wantMsg: "the server reported an error in a successful response",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newServer(t, failWith(http.StatusOK, http.Header{"X-Request-Id": {"req_9"}}, tt.body))
			_, err := newClient(t, srv).Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

			var got *llm.Error
			require.ErrorAs(t, err, &got)
			assert.Equal(t, "openai", got.Provider)
			assert.Equal(t, "req_9", got.RequestID)
			assert.Equal(t, http.StatusOK, got.Status)
			assert.Equal(t, tt.wantType, got.Type)
			assert.Equal(t, tt.wantMsg, got.Message)
			assert.Equal(t, tt.wantRetry, got.Retryable)
			assert.NoError(t, got.Err)
		})
	}
}
