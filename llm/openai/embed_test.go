package openai_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

const oneVector = `{"object":"list","data":[{"object":"embedding","embedding":[1],"index":0}],"model":"e","usage":{"prompt_tokens":1,"total_tokens":1}}`

// echoVectors answers an embeddings request with one vector per input, [i]
// for the i-th, so a request of any size gets a reply that fits it.
func echoVectors(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Input []string `json:"input"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	data := make([]string, len(req.Input))
	for i := range req.Input {
		data[i] = fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":[%d]}`, i, i)
	}
	serveJSON(`{"object":"list","model":"e","data":[`+strings.Join(data, ",")+`],"usage":{"prompt_tokens":1,"total_tokens":1}}`)(w, r)
}

func TestEmbed_RequestBody(t *testing.T) {
	tests := []struct {
		name string
		opts func(*openai.Options)
		req  llm.EmbedRequest
		want string
	}{
		{
			name: "one input, the model from the request",
			req:  llm.EmbedRequest{Model: "request-embedder", Input: []string{"hello"}},
			want: `{"model":"request-embedder","input":["hello"],"encoding_format":"float"}`,
		},
		{
			name: "the model from the options when the request names none",
			opts: func(o *openai.Options) { o.EmbeddingModel = "option-embedder" },
			req:  llm.EmbedRequest{Input: []string{"hello"}},
			want: `{"model":"option-embedder","input":["hello"],"encoding_format":"float"}`,
		},
		{
			name: "the request's model wins over the options'",
			opts: func(o *openai.Options) { o.EmbeddingModel = "option-embedder" },
			req:  llm.EmbedRequest{Model: "request-embedder", Input: []string{"hello"}},
			want: `{"model":"request-embedder","input":["hello"],"encoding_format":"float"}`,
		},
		{
			name: "the chat model is never used as the embedding model",
			opts: func(o *openai.Options) { o.Model = "chat-model"; o.EmbeddingModel = "option-embedder" },
			req:  llm.EmbedRequest{Input: []string{"hello"}},
			want: `{"model":"option-embedder","input":["hello"],"encoding_format":"float"}`,
		},
		{
			name: "several inputs stay in order, as an array",
			req:  llm.EmbedRequest{Model: "e", Input: []string{"zeta", "alpha", "mid"}},
			want: `{"model":"e","input":["zeta","alpha","mid"],"encoding_format":"float"}`,
		},
		{
			name: "dimensions when set",
			req:  llm.EmbedRequest{Model: "e", Input: []string{"hello"}, Dimensions: 256},
			want: `{"model":"e","input":["hello"],"encoding_format":"float","dimensions":256}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := newServer(t, echoVectors)
			c := newClient(t, srv, func(o *openai.Options) {
				if tt.opts != nil {
					tt.opts(o)
				}
			})

			_, err := c.Embed(t.Context(), tt.req)
			require.NoError(t, err)

			got := rec.only(t)
			assert.Equal(t, http.MethodPost, got.Method)
			assert.Equal(t, "/v1/embeddings", got.Path)
			assert.JSONEq(t, tt.want, string(got.Body))
		})
	}
}

func TestEmbed_DocumentedExample(t *testing.T) {
	srv, _ := newServer(t, serveJSON(testdata(t, "embeddings.json")))

	got, err := newClient(t, srv).Embed(t.Context(), llm.EmbedRequest{Model: "text-embedding-3-small", Input: []string{"Hello world"}, Dimensions: 3})
	require.NoError(t, err)

	assert.Equal(t, &llm.EmbedResponse{
		Model:   "text-embedding-3-small",
		Vectors: [][]float32{{0.26726124, 0.53452248, 0.80178373}},
		Usage:   llm.Usage{InputTokens: 2},
	}, got)
}

func TestEmbed_Response(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		body  string
		want  *llm.EmbedResponse
	}{
		{
			name:  "vectors come back in input order whatever order the server sent them in",
			input: []string{"a", "b", "c"},
			body: `{"object":"list","model":"e","usage":{"prompt_tokens":6,"total_tokens":6},"data":[` +
				`{"object":"embedding","index":2,"embedding":[3,3]},` +
				`{"object":"embedding","index":0,"embedding":[1,1]},` +
				`{"object":"embedding","index":1,"embedding":[2,2]}]}`,
			want: &llm.EmbedResponse{Model: "e", Vectors: [][]float32{{1, 1}, {2, 2}, {3, 3}}, Usage: llm.Usage{InputTokens: 6}},
		},
		{
			name:  "a server that sends no index is read in the order it sent",
			input: []string{"a", "b"},
			body:  `{"model":"e","data":[{"embedding":[1]},{"embedding":[2]}]}`,
			want:  &llm.EmbedResponse{Model: "e", Vectors: [][]float32{{1}, {2}}},
		},
		{
			name:  "no model in the reply, the model asked for is reported",
			input: []string{"a"},
			body:  `{"data":[{"index":0,"embedding":[1]}]}`,
			want:  &llm.EmbedResponse{Model: "request-embedder", Vectors: [][]float32{{1}}},
		},
		{
			name:  "no usage in the reply",
			input: []string{"a"},
			body:  `{"model":"e","data":[{"index":0,"embedding":[1]}]}`,
			want:  &llm.EmbedResponse{Model: "e", Vectors: [][]float32{{1}}},
		},
		{
			name:  "usage counts the prompt only",
			input: []string{"a"},
			body:  `{"model":"e","data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":5,"total_tokens":99}}`,
			want:  &llm.EmbedResponse{Model: "e", Vectors: [][]float32{{1}}, Usage: llm.Usage{InputTokens: 5}},
		},
		{
			name:  "fields this package does not read are ignored",
			input: []string{"a"},
			body:  `{"object":"list","model":"e","extra":{"a":1},"data":[{"object":"embedding","index":0,"embedding":[0.5],"extra":true}]}`,
			want:  &llm.EmbedResponse{Model: "e", Vectors: [][]float32{{0.5}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newServer(t, serveJSON(tt.body))
			got, err := newClient(t, srv).Embed(t.Context(), llm.EmbedRequest{Model: "request-embedder", Input: tt.input})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEmbed_NoInputSendsNothing(t *testing.T) {
	srv, rec := newServer(t, serveJSON(oneVector))

	for _, input := range [][]string{nil, {}} {
		got, err := newClient(t, srv).Embed(t.Context(), llm.EmbedRequest{Model: "e", Input: input})
		require.NoError(t, err)
		assert.Equal(t, &llm.EmbedResponse{Model: "e", Vectors: [][]float32{}}, got)
	}
	assert.Zero(t, rec.count(), "one vector per input, and no input needs no call")
}

func TestEmbed_RefusesWhatItCannotSend(t *testing.T) {
	srv, rec := newServer(t, serveJSON(oneVector))
	c := newClient(t, srv)

	_, err := c.Embed(t.Context(), llm.EmbedRequest{Input: []string{"hello"}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedding model")
	assert.False(t, llm.Retryable(err))
	assert.Zero(t, rec.count(), "the chat model is no stand-in, and nothing is sent")
}

func TestEmbed_RefusesAReplyItCannotRead(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		body    string
		wantErr string
	}{
		{name: "fewer vectors than inputs", input: []string{"a", "b"}, body: `{"data":[{"index":0,"embedding":[1]}]}`, wantErr: "1 vector"},
		{name: "more vectors than inputs", input: []string{"a"}, body: `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2]}]}`, wantErr: "2 vector"},
		{name: "an index past the inputs", input: []string{"a", "b"}, body: `{"data":[{"index":0,"embedding":[1]},{"index":2,"embedding":[2]}]}`, wantErr: "index 2"},
		{name: "a negative index", input: []string{"a"}, body: `{"data":[{"index":-1,"embedding":[1]}]}`, wantErr: "index -1"},
		{name: "the same index twice", input: []string{"a", "b"}, body: `{"data":[{"index":1,"embedding":[1]},{"index":1,"embedding":[2]}]}`, wantErr: "index 1 twice"},
		{name: "a vector sent as a string, which base64 would be", input: []string{"a"}, body: `{"data":[{"index":0,"embedding":"AAAAAA=="}]}`, wantErr: "decode"},
		{name: "not JSON", input: []string{"a"}, body: `<html></html>`, wantErr: "decode"},
		{name: "no data", input: []string{"a"}, body: `{"model":"e"}`, wantErr: "0 vector"},
		{name: "no data and an error that says nothing", input: []string{"a"}, body: `{"model":"e","error":{},"data":[]}`, wantErr: "0 vector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newServer(t, serveJSON(tt.body))
			_, err := newClient(t, srv).Embed(t.Context(), llm.EmbedRequest{Model: "e", Input: tt.input})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			notAnLLMError(t, err)
		})
	}
}

// An error object in a 200 reply is an error with the server's own words, as
// it is for a chat call, and not a count of vectors that are not there.
func TestEmbed_AnErrorInsideASuccessfulResponse(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantType  string
		wantMsg   string
		wantRetry bool
	}{
		{name: "final", body: apiErr("invalid_request_error", "", "input too long"), wantType: "invalid_request_error", wantMsg: "input too long"},
		{name: "overloaded", body: apiErr("service_unavailable_error", "server_is_overloaded", "busy"), wantType: "service_unavailable_error", wantMsg: "busy", wantRetry: true},
		{name: "a rate limit", body: apiErr("rate_limit_error", "", "slow"), wantType: "rate_limit_error", wantMsg: "slow", wantRetry: true},
		{name: "out of credit", body: apiErr("insufficient_quota", "credit_balance_exhausted", "no credits"), wantType: "insufficient_quota", wantMsg: "no credits"},
		{name: "an error beside an empty list", body: `{"object":"list","data":[],"error":{"message":"nothing embedded","type":"server_error"}}`, wantType: "server_error", wantMsg: "nothing embedded", wantRetry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newServer(t, failWith(http.StatusOK, http.Header{"X-Request-Id": {"req_9"}}, tt.body))

			_, err := newClient(t, srv).Embed(t.Context(), llm.EmbedRequest{Model: "e", Input: []string{"a"}})

			got := asLLMError(t, err)
			assert.Equal(t, "openai", got.Provider)
			assert.Equal(t, http.StatusOK, got.Status)
			assert.Equal(t, tt.wantType, got.Type)
			assert.Equal(t, tt.wantMsg, got.Message)
			assert.Equal(t, tt.wantRetry, got.Retryable)
			assert.Equal(t, "req_9", got.RequestID)
			assert.NoError(t, got.Err)
		})
	}

	t.Run("an error beside all the vectors asked for does not take them away", func(t *testing.T) {
		srv, _ := newServer(t, serveJSON(`{"model":"e","error":{"message":"deprecated model","type":"warning"},"data":[{"index":0,"embedding":[1]}]}`))
		got, err := newClient(t, srv).Embed(t.Context(), llm.EmbedRequest{Model: "e", Input: []string{"a"}})
		require.NoError(t, err)
		assert.Equal(t, [][]float32{{1}}, got.Vectors)
	})

	t.Run("an empty error beside the vectors is no error", func(t *testing.T) {
		srv, _ := newServer(t, serveJSON(`{"model":"e","error":{},"data":[{"index":0,"embedding":[1]}]}`))
		got, err := newClient(t, srv).Embed(t.Context(), llm.EmbedRequest{Model: "e", Input: []string{"a"}})
		require.NoError(t, err)
		assert.Equal(t, [][]float32{{1}}, got.Vectors)
	})
}
