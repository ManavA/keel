package openai_test

import (
	"context"
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

// apiErr is the documented error body: error.message, type, param and code,
// the last two null when there is nothing to say.
func apiErr(typ, code, msg string) string {
	quote := func(s string) string {
		if s == "" {
			return "null"
		}
		return strconv.Quote(s)
	}
	return fmt.Sprintf(`{"error":{"message":%s,"type":%s,"param":null,"code":%s}}`, quote(msg), quote(typ), quote(code))
}

// failWith answers every request with a status, headers and body.
func failWith(status int, header http.Header, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// failure makes one Generate call against a server that answers like that
// and returns the *llm.Error it produced.
func failure(t *testing.T, status int, header http.Header, body string) *llm.Error {
	t.Helper()
	srv, _ := newServer(t, failWith(status, header, body))
	_, err := newClient(t, srv).Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
	var got *llm.Error
	require.ErrorAs(t, err, &got)
	return got
}

func TestGenerate_ErrorStatuses(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		header    http.Header
		body      string
		wantType  string
		wantMsg   string
		wantRetry bool
	}{
		{name: "400 bad request", status: 400, body: apiErr("invalid_request_error", "", "bad field"), wantType: "invalid_request_error", wantMsg: "bad field"},
		{
			name: "400 for a service tier the project may not use", status: 400,
			body:     `{"error":{"message":"Invalid service_tier argument: The requested service tier is not allowed for this project.","type":"invalid_request_error","param":"service_tier","code":null}}`,
			wantType: "invalid_request_error", wantMsg: "Invalid service_tier argument: The requested service tier is not allowed for this project.",
		},
		{name: "401 invalid authentication", status: 401, body: apiErr("invalid_request_error", "invalid_api_key", "Incorrect API key provided"), wantType: "invalid_request_error", wantMsg: "Incorrect API key provided"},
		{name: "401 ip not authorized", status: 401, body: apiErr("invalid_request_error", "", "IP not authorized"), wantType: "invalid_request_error", wantMsg: "IP not authorized"},
		{name: "403 country not supported", status: 403, body: apiErr("invalid_request_error", "unsupported_country_region_territory", "Country, region, or territory not supported"), wantType: "invalid_request_error", wantMsg: "Country, region, or territory not supported"},
		{name: "404 no such model", status: 404, body: apiErr("invalid_request_error", "model_not_found", "The model does not exist"), wantType: "invalid_request_error", wantMsg: "The model does not exist"},
		{name: "408 request timeout", status: 408, body: apiErr("timeout", "", "Request timed out"), wantType: "timeout", wantMsg: "Request timed out", wantRetry: true},
		{name: "409 conflict", status: 409, body: apiErr("conflict", "", "conflict"), wantType: "conflict", wantMsg: "conflict"},
		{name: "422 unprocessable", status: 422, body: apiErr("invalid_request_error", "", "cannot process"), wantType: "invalid_request_error", wantMsg: "cannot process"},

		{name: "429 rate limit reached", status: 429, body: apiErr("requests", "rate_limit_exceeded", "Rate limit reached for requests"), wantType: "requests", wantMsg: "Rate limit reached for requests", wantRetry: true},
		{name: "429 rate limit with no code at all", status: 429, body: apiErr("rate_limit_error", "", "slow"), wantType: "rate_limit_error", wantMsg: "slow", wantRetry: true},
		{name: "429 slow down", status: 429, header: http.Header{"Retry-After": {"3"}}, body: apiErr("rate_limit_error", "slow_down", "Your request rate increased too quickly"), wantType: "rate_limit_error", wantMsg: "Your request rate increased too quickly", wantRetry: true},

		{name: "429 credit balance exhausted", status: 429, body: apiErr("billing", "credit_balance_exhausted", "no credits"), wantType: "billing", wantMsg: "no credits"},
		{name: "429 insufficient quota as the code", status: 429, body: apiErr("billing", "insufficient_quota", "quota"), wantType: "billing", wantMsg: "quota"},
		{name: "429 insufficient quota as the type, which the error guide says it can be", status: 429, body: apiErr("insufficient_quota", "", "You exceeded your current quota"), wantType: "insufficient_quota", wantMsg: "You exceeded your current quota"},
		{name: "429 organization spend limit", status: 429, body: apiErr("billing", "organization_spend_limit_exceeded", "org limit"), wantType: "billing", wantMsg: "org limit"},
		{name: "429 project spend limit", status: 429, body: apiErr("billing", "project_spend_limit_exceeded", "project limit"), wantType: "billing", wantMsg: "project limit"},
		{name: "429 organization usage limit", status: 429, body: apiErr("billing", "organization_usage_limit_exceeded", "usage limit"), wantType: "billing", wantMsg: "usage limit"},
		{
			name: "429 spend limit stays final even with a Retry-After", status: 429, header: http.Header{"Retry-After": {"30"}},
			body: apiErr("billing", "project_spend_limit_exceeded", "project limit"), wantType: "billing", wantMsg: "project limit",
		},

		{name: "500 server error", status: 500, body: apiErr("server_error", "", "The server had an error"), wantType: "server_error", wantMsg: "The server had an error", wantRetry: true},
		{name: "502 bad gateway", status: 502, body: apiErr("", "", "bad gateway"), wantType: "", wantMsg: "bad gateway", wantRetry: true},
		{name: "503 model overloaded", status: 503, header: http.Header{"Retry-After": {"2"}}, body: apiErr("service_unavailable_error", "server_is_overloaded", "overloaded"), wantType: "service_unavailable_error", wantMsg: "overloaded", wantRetry: true},
		{name: "504 gateway timeout", status: 504, body: apiErr("", "", "timeout"), wantType: "", wantMsg: "timeout", wantRetry: true},
		{name: "529 as an overloaded gateway sends it", status: 529, body: apiErr("overloaded", "", "busy"), wantType: "overloaded", wantMsg: "busy", wantRetry: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failure(t, tt.status, tt.header, tt.body)

			assert.Equal(t, "openai", got.Provider)
			assert.Equal(t, tt.status, got.Status)
			assert.Equal(t, tt.wantType, got.Type)
			assert.Equal(t, tt.wantMsg, got.Message)
			assert.Equal(t, tt.wantRetry, got.Retryable)
			assert.NoError(t, got.Err, "the server answered, so there is no transport error")
			assert.Equal(t, tt.wantRetry, llm.Retryable(got))
		})
	}
}

func TestGenerate_ErrorRetryAfter(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{name: "absent", header: "", want: 0},
		{name: "whole seconds", header: "7", want: 7 * time.Second},
		{name: "zero", header: "0", want: 0},
		{name: "fractional seconds", header: "1.5", want: 1500 * time.Millisecond},
		{name: "padded", header: " 12 ", want: 12 * time.Second},
		{name: "negative is no wait", header: "-3", want: 0},
		{name: "not a number or a date", header: "soon", want: 0},
		{name: "an absurd number does not overflow", header: "1e30", want: time.Duration(1<<63 - 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := http.Header{}
			if tt.header != "" {
				hdr.Set("Retry-After", tt.header)
			}
			got := failure(t, 429, hdr, apiErr("requests", "", "slow"))
			assert.Equal(t, tt.want, got.RetryAfter)
			assert.True(t, got.Retryable, "a 429 is retryable, with or without a wait")
		})
	}

	t.Run("an HTTP date is the wait until then", func(t *testing.T) {
		hdr := http.Header{}
		hdr.Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
		got := failure(t, 503, hdr, apiErr("service_unavailable_error", "server_is_overloaded", "overloaded"))
		assert.Greater(t, got.RetryAfter, 60*time.Second)
		assert.LessOrEqual(t, got.RetryAfter, 90*time.Second)
	})
	t.Run("an HTTP date in the past is no wait", func(t *testing.T) {
		hdr := http.Header{}
		hdr.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
		got := failure(t, 503, hdr, apiErr("", "", "overloaded"))
		assert.Zero(t, got.RetryAfter)
	})
	t.Run("a Retry-After on a status that is not retryable is still reported", func(t *testing.T) {
		hdr := http.Header{"Retry-After": {"5"}}
		got := failure(t, 400, hdr, apiErr("invalid_request_error", "", "bad"))
		assert.Equal(t, 5*time.Second, got.RetryAfter)
		assert.False(t, got.Retryable)
	})
}

func TestGenerate_ErrorRequestID(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{name: "from x-request-id", header: http.Header{"X-Request-Id": {"req_abc123"}}, want: "req_abc123"},
		{name: "absent", header: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failure(t, 500, tt.header, apiErr("server_error", "", "oops"))
			assert.Equal(t, tt.want, got.RequestID)
		})
	}
}

func TestGenerate_ErrorBodies(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantType string
		wantMsg  string
	}{
		{name: "the documented shape", status: 400, body: apiErr("invalid_request_error", "bad", "bad field"), wantType: "invalid_request_error", wantMsg: "bad field"},
		{name: "no type, so the code stands in for it", status: 400, body: `{"error":{"message":"bad field","code":"bad_field"}}`, wantType: "bad_field", wantMsg: "bad field"},
		{name: "a code that is a number", status: 400, body: `{"error":{"message":"bad field","type":"","code":400}}`, wantType: "400", wantMsg: "bad field"},
		{name: "error is a plain string", status: 400, body: `{"error":"model not loaded"}`, wantMsg: "model not loaded"},
		{name: "a message at the top level", status: 400, body: `{"message":"no such route"}`, wantMsg: "no such route"},
		{name: "error null", status: 500, body: `{"error":null}`, wantMsg: `{"error":null}`},
		{name: "html from a gateway", status: 502, body: "<html>\n<body>Bad Gateway</body>\n</html>\n", wantMsg: "<html>\n<body>Bad Gateway</body>\n</html>"},
		{name: "plain text", status: 503, body: "upstream connect error", wantMsg: "upstream connect error"},
		{name: "an empty body falls back to the status text", status: 503, body: "", wantMsg: "Service Unavailable"},
		{name: "an empty message falls back to the status text", status: 400, body: `{"error":{"message":"","type":"invalid_request_error"}}`, wantType: "invalid_request_error", wantMsg: "Bad Request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failure(t, tt.status, nil, tt.body)
			assert.Equal(t, tt.status, got.Status)
			assert.Equal(t, tt.wantType, got.Type)
			assert.Equal(t, tt.wantMsg, got.Message)
		})
	}

	t.Run("a long body is cut", func(t *testing.T) {
		got := failure(t, 502, nil, strings.Repeat("x", 5000))
		assert.Less(t, len(got.Message), 600)
		assert.True(t, strings.HasPrefix(got.Message, "xxxx"))
	})
	t.Run("a body that is not UTF-8 does not break the message", func(t *testing.T) {
		got := failure(t, 502, nil, "bad \xff\xfe gateway")
		assert.Contains(t, got.Message, "gateway")
	})
}

// Generate, Stream and Embed read one error the same way.
func TestErrors_AreTheSameForEveryCall(t *testing.T) {
	calls := map[string]func(ctx context.Context, c *openai.Client) error{
		"Generate": func(ctx context.Context, c *openai.Client) error {
			_, err := c.Generate(ctx, llm.Request{Messages: userMsg("hi")})
			return err
		},
		"Stream": func(ctx context.Context, c *openai.Client) error {
			_, err := c.Stream(ctx, llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
			return err
		},
		"Embed": func(ctx context.Context, c *openai.Client) error {
			_, err := c.Embed(ctx, llm.EmbedRequest{Model: "e", Input: []string{"a"}})
			return err
		},
	}
	tests := []struct {
		name      string
		status    int
		header    http.Header
		body      string
		wantRetry bool
		wantWait  time.Duration
	}{
		{name: "overloaded", status: 503, header: http.Header{"Retry-After": {"4"}, "X-Request-Id": {"req_1"}}, body: apiErr("service_unavailable_error", "server_is_overloaded", "overloaded"), wantRetry: true, wantWait: 4 * time.Second},
		{name: "out of credit", status: 429, header: http.Header{"X-Request-Id": {"req_1"}}, body: apiErr("insufficient_quota", "credit_balance_exhausted", "no credits")},
	}
	for callName, call := range calls {
		for _, tt := range tests {
			t.Run(callName+" "+tt.name, func(t *testing.T) {
				srv, _ := newServer(t, failWith(tt.status, tt.header, tt.body))
				err := call(t.Context(), newClient(t, srv))

				var got *llm.Error
				require.ErrorAs(t, err, &got)
				assert.Equal(t, tt.status, got.Status)
				assert.Equal(t, tt.wantRetry, got.Retryable)
				assert.Equal(t, tt.wantWait, got.RetryAfter)
				assert.Equal(t, "req_1", got.RequestID)
			})
		}
	}
}

func TestErrors_TransportFailures(t *testing.T) {
	t.Run("a server that is not there", func(t *testing.T) {
		srv, _ := newServer(t, serveJSON(simpleReply))
		c := newClient(t, srv)
		srv.Close()

		_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		var got *llm.Error
		require.ErrorAs(t, err, &got)
		assert.Equal(t, "openai", got.Provider)
		assert.Zero(t, got.Status, "no response arrived")
		require.Error(t, got.Err)
		assert.True(t, got.Retryable)
		assert.True(t, llm.Retryable(err))
	})

	t.Run("a connection closed before any answer", func(t *testing.T) {
		srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})

		_, err := newClient(t, srv).Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		var got *llm.Error
		require.ErrorAs(t, err, &got)
		assert.Zero(t, got.Status)
		assert.True(t, got.Retryable)
		assert.True(t, llm.Retryable(err))
	})

	t.Run("a reply cut short is a transport failure, not an unreadable answer", func(t *testing.T) {
		body := `{"id":"chatcmpl-1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"par`
		srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)+200))
			_, _ = io.WriteString(w, body)
		})

		_, err := newClient(t, srv).Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

		var got *llm.Error
		require.ErrorAs(t, err, &got)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		assert.True(t, got.Retryable)
		assert.True(t, llm.Retryable(err))
	})

	t.Run("a context cancelled before the call", func(t *testing.T) {
		srv, rec := newServer(t, serveJSON(simpleReply))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := newClient(t, srv).Generate(ctx, llm.Request{Messages: userMsg("hi")})

		var got *llm.Error
		require.ErrorAs(t, err, &got)
		assert.ErrorIs(t, err, context.Canceled)
		assert.False(t, got.Retryable, "the call ended because its context did")
		assert.False(t, llm.Retryable(err))
		assert.Zero(t, rec.count())
	})

	t.Run("a deadline that passes while waiting for the answer", func(t *testing.T) {
		release := make(chan struct{})
		srv, _ := newServer(t, func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		_, err := newClient(t, srv).Generate(ctx, llm.Request{Messages: userMsg("hi")})

		var got *llm.Error
		require.ErrorAs(t, err, &got)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, got.Retryable)
		assert.False(t, llm.Retryable(err))
	})
}
