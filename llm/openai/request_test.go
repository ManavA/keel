package openai_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

func ptr[T any](v T) *T { return &v }

// Each case compares the whole body the server received, so a field sent
// that should not be, as well as one missing, fails.
func TestGenerate_RequestBody(t *testing.T) {
	readDoc := llm.Tool{
		Name:        "read_document",
		Description: "Read one document",
		Schema:      json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`),
		Strict:      true,
	}
	call := func(id, name, input string) llm.ToolCall {
		return llm.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
	}

	tests := []struct {
		name string
		opts func(*openai.Options)
		req  llm.Request
		want string
	}{
		{
			name: "model from the request",
			req:  llm.Request{Model: "request-model", Messages: userMsg("hi")},
			want: `{"model":"request-model","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "model from the options when the request names none",
			req:  llm.Request{Messages: userMsg("hi")},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "system prompt is the first message under the system role",
			req:  llm.Request{System: "be brief", Messages: userMsg("hi")},
			want: `{"model":"test-model","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "system prompt under the configured role",
			opts: func(o *openai.Options) { o.SystemRole = "developer" },
			req:  llm.Request{System: "be brief", Messages: userMsg("hi")},
			want: `{"model":"test-model","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "no system message when the prompt is empty",
			opts: func(o *openai.Options) { o.SystemRole = "developer" },
			req:  llm.Request{Messages: userMsg("hi")},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "assistant turn with text only",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleUser, Text: "hi"},
				{Role: llm.RoleAssistant, Text: "hello"},
				{Role: llm.RoleUser, Text: "again"},
			}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":"hello"},{"role":"user","content":"again"}]}`,
		},
		{
			name: "assistant turn that only calls a tool has null content",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("call_1", "read_document", `{"id":7}`)}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"read_document","arguments":"{\"id\":7}"}}]}]}`,
		},
		{
			name: "assistant turn with text and a call",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, Text: "let me look", ToolCalls: []llm.ToolCall{call("call_1", "read_document", `{"id":7}`)}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":"let me look","tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"read_document","arguments":"{\"id\":7}"}}]}]}`,
		},
		{
			name: "two calls stay in order",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
					call("call_b", "second", `{}`), call("call_a", "first", `{}`),
				}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"call_b","type":"function","function":{"name":"second","arguments":"{}"}},` +
				`{"id":"call_a","type":"function","function":{"name":"first","arguments":"{}"}}]}]}`,
		},
		{
			name: "assistant turn with neither text nor calls still has a content string",
			req:  llm.Request{Messages: []llm.Message{{Role: llm.RoleAssistant}}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":""}]}`,
		},
		{
			name: "arguments are the input bytes as written, not re-encoded",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("c", "f", `{ "zeta" : 1,  "alpha" : 2 }`)}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"c","type":"function","function":{"name":"f","arguments":"{ \"zeta\" : 1,  \"alpha\" : 2 }"}}]}]}`,
		},
		{
			name: "a malformed call is sent back as the text the model wrote",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c", Name: "f", Input: json.RawMessage(`"{\"id\":"`), Malformed: true,
				}}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"c","type":"function","function":{"name":"f","arguments":"{\"id\":"}}]}]}`,
		},
		{
			name: "a malformed call whose input is not a string is sent as an empty object",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
					ID: "c", Name: "f", Input: json.RawMessage(`{"id":1}`), Malformed: true,
				}}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		},
		{
			name: "a call with no input is sent with an empty object",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "c", Name: "f"}, {ID: "d", Name: "g", Input: json.RawMessage(`null`)}}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}},` +
				`{"id":"d","type":"function","function":{"name":"g","arguments":"{}"}}]}]}`,
		},
		{
			name: "one tool message per result, in order",
			req: llm.Request{Messages: []llm.Message{{
				Role: llm.RoleTool,
				ToolResults: []llm.ToolResult{
					{CallID: "call_b", Content: "second"},
					{CallID: "call_a", Content: "first"},
				},
			}}},
			want: `{"model":"test-model","messages":[` +
				`{"role":"tool","tool_call_id":"call_b","content":"second"},` +
				`{"role":"tool","tool_call_id":"call_a","content":"first"}]}`,
		},
		{
			name: "an error result is prefixed because the protocol has no error flag",
			req: llm.Request{Messages: []llm.Message{{
				Role: llm.RoleTool,
				ToolResults: []llm.ToolResult{
					{CallID: "call_1", Content: "the text"},
					{CallID: "call_2", Content: "no such document", IsError: true},
				},
			}}},
			want: `{"model":"test-model","messages":[` +
				`{"role":"tool","tool_call_id":"call_1","content":"the text"},` +
				`{"role":"tool","tool_call_id":"call_2","content":"ERROR: no such document"}]}`,
		},
		{
			name: "opaque is never sent, whoever produced it",
			req: llm.Request{Messages: []llm.Message{
				{Role: llm.RoleAssistant, Text: "from openai", Opaque: &llm.Opaque{Provider: "openai", Data: json.RawMessage(`{"x":1}`)}},
				{Role: llm.RoleAssistant, Text: "from anthropic", ToolCalls: []llm.ToolCall{call("c", "f", `{}`)},
					Opaque: &llm.Opaque{Provider: "anthropic", Data: json.RawMessage(`[{"type":"thinking"}]`)}},
			}},
			want: `{"model":"test-model","messages":[{"role":"assistant","content":"from openai"},` +
				`{"role":"assistant","content":"from anthropic","tool_calls":[` +
				`{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		},
		{
			name: "tool with a schema, a description and strict",
			req:  llm.Request{Messages: userMsg("hi"), Tools: []llm.Tool{readDoc}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{` +
				`"name":"read_document","description":"Read one document",` +
				`"parameters":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]},"strict":true}}]}`,
		},
		{
			name: "tool with only a name sends no description, parameters or strict",
			req:  llm.Request{Messages: userMsg("hi"), Tools: []llm.Tool{{Name: "ping"}}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"ping"}}]}`,
		},
		{
			name: "a null schema is no schema",
			req:  llm.Request{Messages: userMsg("hi"), Tools: []llm.Tool{{Name: "ping", Schema: json.RawMessage(`null`)}}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"ping"}}]}`,
		},
		{
			name: "tools keep the order given",
			req:  llm.Request{Messages: userMsg("hi"), Tools: []llm.Tool{{Name: "zeta"}, {Name: "alpha"}}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"tools":[` +
				`{"type":"function","function":{"name":"zeta"}},{"type":"function","function":{"name":"alpha"}}]}`,
		},
		{
			name: "tool choice none",
			req:  llm.Request{Messages: userMsg("hi"), Tools: []llm.Tool{{Name: "ping"}}, ToolChoice: llm.ToolChoiceNone},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"ping"}}],"tool_choice":"none"}`,
		},
		{
			name: "tool choice auto is left out",
			req:  llm.Request{Messages: userMsg("hi"), Tools: []llm.Tool{{Name: "ping"}}, ToolChoice: llm.ToolChoiceAuto},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"ping"}}]}`,
		},
		{
			name: "tool choice none with no tools is left out, since the model has none to call",
			req:  llm.Request{Messages: userMsg("hi"), ToolChoice: llm.ToolChoiceNone},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "output schema under the default name",
			req: llm.Request{Messages: userMsg("hi"), Output: &llm.Schema{
				JSON: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`),
			}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema",` +
				`"json_schema":{"name":"output","schema":{"type":"object","properties":{"n":{"type":"integer"}}},"strict":true}}}`,
		},
		{
			name: "output schema with its own name and description",
			req: llm.Request{Messages: userMsg("hi"), Output: &llm.Schema{
				Name: "verdict", Description: "The verdict", JSON: json.RawMessage(`{"type":"object"}`),
			}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema",` +
				`"json_schema":{"name":"verdict","description":"The verdict","schema":{"type":"object"},"strict":true}}}`,
		},
		{
			name: "max tokens from the request",
			req:  llm.Request{Messages: userMsg("hi"), MaxTokens: 100},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":100}`,
		},
		{
			name: "max tokens from the options when the request sets none",
			opts: func(o *openai.Options) { o.MaxTokens = 300 },
			req:  llm.Request{Messages: userMsg("hi")},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":300}`,
		},
		{
			name: "the request's bound wins over the options'",
			opts: func(o *openai.Options) { o.MaxTokens = 300 },
			req:  llm.Request{Messages: userMsg("hi"), MaxTokens: 100},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":100}`,
		},
		{
			name: "the legacy name for a server that only knows it",
			opts: func(o *openai.Options) { o.LegacyMaxTokens = true },
			req:  llm.Request{Messages: userMsg("hi"), MaxTokens: 100},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`,
		},
		{
			name: "the legacy name also carries the options' bound",
			opts: func(o *openai.Options) { o.LegacyMaxTokens = true; o.MaxTokens = 300 },
			req:  llm.Request{Messages: userMsg("hi")},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":300}`,
		},
		{
			name: "no bound at all when both are zero",
			opts: func(o *openai.Options) { o.LegacyMaxTokens = true },
			req:  llm.Request{Messages: userMsg("hi")},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "temperature when set",
			req:  llm.Request{Messages: userMsg("hi"), Temperature: ptr(0.2)},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"temperature":0.2}`,
		},
		{
			name: "a temperature of zero is still sent",
			req:  llm.Request{Messages: userMsg("hi"), Temperature: ptr(0.0)},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"temperature":0}`,
		},
		{
			name: "reasoning effort",
			req:  llm.Request{Messages: userMsg("hi"), Effort: llm.EffortXHigh},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`,
		},
		{
			name: "stop sequences",
			req:  llm.Request{Messages: userMsg("hi"), Stop: []string{"END", "STOP"}},
			want: `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stop":["END","STOP"]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := newServer(t, serveJSON(simpleReply))
			c := newClient(t, srv, func(o *openai.Options) {
				if tt.opts != nil {
					tt.opts(o)
				}
			})

			_, err := c.Generate(t.Context(), tt.req)
			require.NoError(t, err)

			got := rec.only(t)
			assert.Equal(t, http.MethodPost, got.Method)
			assert.Equal(t, "/v1/chat/completions", got.Path)
			assert.JSONEq(t, tt.want, string(got.Body))
		})
	}
}

func TestGenerate_Headers(t *testing.T) {
	tests := []struct {
		name string
		opts func(*openai.Options)
		want func(t *testing.T, h http.Header)
	}{
		{
			name: "a key is sent as a bearer token",
			opts: func(o *openai.Options) { o.APIKey = "sk-test-123" },
			want: func(t *testing.T, h http.Header) {
				assert.Equal(t, "Bearer sk-test-123", h.Get("Authorization"))
				assert.Equal(t, "application/json", h.Get("Content-Type"))
			},
		},
		{
			name: "no key sends no authorization header at all",
			want: func(t *testing.T, h http.Header) {
				_, present := h["Authorization"]
				assert.False(t, present, "Authorization header: %v", h["Authorization"])
				assert.Equal(t, "application/json", h.Get("Content-Type"))
			},
		},
		{
			name: "header is added to the request",
			opts: func(o *openai.Options) {
				o.Header = http.Header{"X-Gateway-Key": {"abc"}, "X-Team": {"a", "b"}}
			},
			want: func(t *testing.T, h http.Header) {
				assert.Equal(t, []string{"abc"}, h.Values("X-Gateway-Key"))
				assert.Equal(t, []string{"a", "b"}, h.Values("X-Team"))
			},
		},
		{
			name: "header serves a gateway that wants its own authorization",
			opts: func(o *openai.Options) { o.Header = http.Header{"Authorization": {"Basic Zm9vOmJhcg=="}} },
			want: func(t *testing.T, h http.Header) {
				assert.Equal(t, []string{"Basic Zm9vOmJhcg=="}, h.Values("Authorization"))
			},
		},
		{
			name: "header cannot replace the key's authorization or the content type",
			opts: func(o *openai.Options) {
				o.APIKey = "sk-test-123"
				o.Header = http.Header{"Authorization": {"Bearer other"}, "Content-Type": {"text/plain"}}
			},
			want: func(t *testing.T, h http.Header) {
				assert.Equal(t, []string{"Bearer sk-test-123"}, h.Values("Authorization"))
				assert.Equal(t, []string{"application/json"}, h.Values("Content-Type"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := newServer(t, serveJSON(simpleReply))
			c := newClient(t, srv, func(o *openai.Options) {
				if tt.opts != nil {
					tt.opts(o)
				}
			})
			_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
			require.NoError(t, err)
			tt.want(t, rec.only(t).Header)
		})
	}
}

func TestClient_EveryCallSendsTheSameHeaders(t *testing.T) {
	srv, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/embeddings":
			serveJSON(`{"data":[{"index":0,"embedding":[1]}],"model":"e","usage":{"prompt_tokens":1}}`)(w, r)
		case streamRequested(r):
			serveStream(sseBody(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`))(w, r)
		default:
			serveJSON(simpleReply)(w, r)
		}
	})
	c := newClient(t, srv, func(o *openai.Options) {
		o.APIKey = "sk-test-123"
		o.EmbeddingModel = "embed-model"
		o.Header = http.Header{"X-Gateway": {"yes"}}
	})

	_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
	require.NoError(t, err)
	_, err = c.Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
	require.NoError(t, err)
	_, err = c.Embed(t.Context(), llm.EmbedRequest{Input: []string{"a"}})
	require.NoError(t, err)

	got := rec.all()
	require.Len(t, got, 3)
	for _, req := range got {
		assert.Equal(t, "Bearer sk-test-123", req.Header.Get("Authorization"), req.Path)
		assert.Equal(t, "yes", req.Header.Get("X-Gateway"), req.Path)
		assert.Equal(t, "application/json", req.Header.Get("Content-Type"), req.Path)
	}
}

func TestGenerate_BaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL func(srvURL string) string
		want    string
	}{
		{name: "with a path prefix", baseURL: func(u string) string { return u + "/v1" }, want: "/v1/chat/completions"},
		{name: "a trailing slash does not double", baseURL: func(u string) string { return u + "/v1/" }, want: "/v1/chat/completions"},
		{name: "no prefix at all", baseURL: func(u string) string { return u }, want: "/chat/completions"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := newServer(t, serveJSON(simpleReply))
			c, err := openai.New(openai.Options{BaseURL: tt.baseURL(srv.URL), Model: testModel})
			require.NoError(t, err)
			_, err = c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
			require.NoError(t, err)
			assert.Equal(t, tt.want, rec.only(t).Path)
		})
	}
}

// A request this package cannot express is refused before anything is sent,
// since the server's answer to it would be a 400 at best.
func TestGenerate_RefusesARequestItCannotSend(t *testing.T) {
	tests := []struct {
		name    string
		req     llm.Request
		wantErr string
	}{
		{
			name:    "a message with no known role",
			req:     llm.Request{Messages: []llm.Message{{Role: "system", Text: "x"}}},
			wantErr: `message 0: unknown role "system"`,
		},
		{
			name:    "an output schema with no schema",
			req:     llm.Request{Messages: userMsg("hi"), Output: &llm.Schema{Name: "empty"}},
			wantErr: "output schema",
		},
		{
			name: "a tool whose schema is not JSON",
			req: llm.Request{Messages: userMsg("hi"), Tools: []llm.Tool{
				{Name: "broken", Schema: json.RawMessage(`{"type":`)},
			}},
			wantErr: "encode request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := newServer(t, serveJSON(simpleReply))
			c := newClient(t, srv)

			_, err := c.Generate(t.Context(), tt.req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.False(t, llm.Retryable(err), "a request that cannot be built is not worth retrying")
			assert.Zero(t, rec.count(), "nothing is sent")

			_, err = c.Stream(t.Context(), tt.req, func(llm.Delta) error { return nil })
			require.Error(t, err)
			assert.Zero(t, rec.count(), "nothing is sent for a stream either")
		})
	}
}

func TestStream_RequestBodyAsksForUsage(t *testing.T) {
	srv, rec := newServer(t, serveStream(sseBody(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)))
	c := newClient(t, srv)

	_, err := c.Stream(t.Context(), llm.Request{System: "be brief", Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
	require.NoError(t, err)

	got := rec.only(t)
	assert.Equal(t, "/v1/chat/completions", got.Path)
	assert.JSONEq(t, `{"model":"test-model","stream":true,"stream_options":{"include_usage":true},`+
		`"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`, string(got.Body))
}

func TestGenerate_RequestBodyHasNoStreamFields(t *testing.T) {
	srv, rec := newServer(t, serveJSON(simpleReply))
	c := newClient(t, srv)

	_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
	require.NoError(t, err)

	body := rec.only(t).JSON(t)
	assert.NotContains(t, body, "stream")
	assert.NotContains(t, body, "stream_options")
}

// A schema or a message is sent with the bytes it has: <, > and & are not
// turned into escapes that a reader would have to undo.
func TestGenerate_RequestBodyDoesNotEscapeHTML(t *testing.T) {
	srv, rec := newServer(t, serveJSON(simpleReply))
	c := newClient(t, srv)

	_, err := c.Generate(t.Context(), llm.Request{
		System:   "a < b & c > d",
		Messages: userMsg("x<y"),
		Tools:    []llm.Tool{{Name: "cmp", Schema: json.RawMessage(`{"description":"a<b&c>d"}`)}},
	})
	require.NoError(t, err)

	body := string(rec.only(t).Body)
	assert.Contains(t, body, `a < b & c > d`)
	assert.Contains(t, body, `x<y`)
	assert.Contains(t, body, `{"description":"a<b&c>d"}`)
	assert.NotContains(t, body, `\u003c`)
}
