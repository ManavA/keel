package anthropic_test

import (
	"context"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
)

// errorBody is the error shape the errors reference documents.
func errorBody(typ, message string) string {
	return `{"type":"error","error":{"type":"` + typ + `","message":"` + message + `"},"request_id":"` + requestID + `"}`
}

// spendCapBody is the 429 the rate-limits reference shows for an organization
// that has reached its monthly spend cap. It comes with no retry-after.
const spendCapBody = `{
  "type": "error",
  "error": {
    "type": "rate_limit_error",
    "message": "You have reached your API usage limits: your organization has crossed its monthly API usage threshold, set based on your organization's API tier. You will regain access on 2026-09-01 at 00:00 UTC.",
    "details": { "error_code": "enforced_spend_limit_reached" }
  },
  "request_id": "req_018EeWyXxfu5pfWkrYcMdjWG"
}`

func TestClient_ErrorStatuses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header map[string]string
		body   string

		wantType       string
		wantMessage    string
		wantRetryable  bool
		wantRetryAfter time.Duration
	}{
		{
			name: "400 invalid_request_error", status: 400,
			body:     errorBody("invalid_request_error", "tool_choice: type \\\"tool\\\" and \\\"any\\\" are not supported for this model."),
			wantType: "invalid_request_error", wantMessage: `tool_choice: type "tool" and "any" are not supported for this model.`,
		},
		{
			name: "401 authentication_error", status: 401,
			body:     errorBody("authentication_error", "invalid x-api-key"),
			wantType: "authentication_error", wantMessage: "invalid x-api-key",
		},
		{
			name: "402 billing_error", status: 402,
			body:     errorBody("billing_error", "There is an issue with your billing information."),
			wantType: "billing_error", wantMessage: "There is an issue with your billing information.",
		},
		{
			name: "403 permission_error", status: 403,
			body:     errorBody("permission_error", "Your API key does not have permission to use the specified resource."),
			wantType: "permission_error", wantMessage: "Your API key does not have permission to use the specified resource.",
		},
		{
			name: "404 not_found_error", status: 404,
			body:     errorBody("not_found_error", "The requested resource could not be found."),
			wantType: "not_found_error", wantMessage: "The requested resource could not be found.",
		},
		{
			name: "408 is retryable", status: 408,
			body:     errorBody("timeout_error", "The request timed out."),
			wantType: "timeout_error", wantMessage: "The request timed out.", wantRetryable: true,
		},
		{
			name: "409 conflict_error is retryable", status: 409,
			body:     errorBody("conflict_error", "The request conflicts with the current state of a resource."),
			wantType: "conflict_error", wantMessage: "The request conflicts with the current state of a resource.", wantRetryable: true,
		},
		{
			name: "413 request_too_large", status: 413,
			body:     errorBody("request_too_large", "Request exceeds the maximum allowed number of bytes."),
			wantType: "request_too_large", wantMessage: "Request exceeds the maximum allowed number of bytes.",
		},
		{
			name: "429 with retry-after is a rate limit, and retryable", status: 429,
			header:   map[string]string{"retry-after": "17"},
			body:     errorBody("rate_limit_error", "This request would exceed your organization's rate limit."),
			wantType: "rate_limit_error", wantMessage: "This request would exceed your organization's rate limit.",
			wantRetryable: true, wantRetryAfter: 17 * time.Second,
		},
		{
			name: "429 with a retry-after of zero is still a rate limit", status: 429,
			header:   map[string]string{"retry-after": "0"},
			body:     errorBody("rate_limit_error", "slow down"),
			wantType: "rate_limit_error", wantMessage: "slow down", wantRetryable: true,
		},
		{
			name: "429 without retry-after is a spend cap, and keeps failing", status: 429,
			body:     spendCapBody,
			wantType: "rate_limit_error",
			wantMessage: "You have reached your API usage limits: your organization has crossed its monthly API usage threshold, " +
				"set based on your organization's API tier. You will regain access on 2026-09-01 at 00:00 UTC.",
		},
		{
			name: "500 api_error is retryable", status: 500,
			body:     errorBody("api_error", "Internal server error"),
			wantType: "api_error", wantMessage: "Internal server error", wantRetryable: true,
		},
		{
			name: "504 timeout_error is retryable", status: 504,
			body:     errorBody("timeout_error", "The request timed out while processing."),
			wantType: "timeout_error", wantMessage: "The request timed out while processing.", wantRetryable: true,
		},
		{
			name: "529 overloaded_error is retryable", status: 529,
			body:     errorBody("overloaded_error", "Overloaded"),
			wantType: "overloaded_error", wantMessage: "Overloaded", wantRetryable: true,
		},
		{
			name: "retry-after is read on any status", status: 529,
			header:   map[string]string{"retry-after": "3"},
			body:     errorBody("overloaded_error", "Overloaded"),
			wantType: "overloaded_error", wantMessage: "Overloaded", wantRetryable: true, wantRetryAfter: 3 * time.Second,
		},
		{
			name: "a fractional retry-after", status: 429,
			header:   map[string]string{"retry-after": "1.5"},
			body:     errorBody("rate_limit_error", "slow down"),
			wantType: "rate_limit_error", wantMessage: "slow down", wantRetryable: true, wantRetryAfter: 1500 * time.Millisecond,
		},
		{
			name: "a retry-after that is not a number is present and waits nothing", status: 429,
			header:   map[string]string{"retry-after": "soon"},
			body:     errorBody("rate_limit_error", "slow down"),
			wantType: "rate_limit_error", wantMessage: "slow down", wantRetryable: true,
		},
		{
			name: "a retry-after too long to hold is as long as can be held", status: 429,
			header:   map[string]string{"retry-after": "1e30"},
			body:     errorBody("rate_limit_error", "slow down"),
			wantType: "rate_limit_error", wantMessage: "slow down", wantRetryable: true, wantRetryAfter: math.MaxInt64,
		},
		{
			name: "a negative retry-after waits nothing", status: 429,
			header:   map[string]string{"retry-after": "-5"},
			body:     errorBody("rate_limit_error", "slow down"),
			wantType: "rate_limit_error", wantMessage: "slow down", wantRetryable: true,
		},
		{
			name: "a body that is not JSON", status: 502,
			body:        "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n</html>\r\n",
			wantMessage: "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n</html>", wantRetryable: true,
		},
		{
			name: "a body that is JSON and not an error", status: 400,
			body:        `{"message":"no"}`,
			wantMessage: `{"message":"no"}`,
		},
		{
			name: "no body at all", status: 503,
			wantMessage: "Service Unavailable", wantRetryable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.Header().Set("request-id", requestID)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			c := api.client(t, anthropic.Options{})

			check := func(t *testing.T, resp *llm.Response, err error) {
				t.Helper()
				require.Error(t, err)
				assert.Nil(t, resp)
				var e *llm.Error
				require.ErrorAs(t, err, &e)
				assert.Equal(t, &llm.Error{
					Provider:   "anthropic",
					Status:     tt.status,
					Type:       tt.wantType,
					Message:    tt.wantMessage,
					RequestID:  requestID,
					RetryAfter: tt.wantRetryAfter,
					Retryable:  tt.wantRetryable,
				}, e)
				assert.Equal(t, tt.wantRetryable, llm.Retryable(err))
			}

			t.Run("Generate", func(t *testing.T) {
				resp, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
				check(t, resp, err)
			})
			t.Run("Stream", func(t *testing.T) {
				resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, func(llm.Delta) error {
					t.Error("fn was called for a request that failed")
					return nil
				})
				check(t, resp, err)
			})
		})
	}
}

func TestClient_ErrorRequestID(t *testing.T) {
	t.Run("from the body when the header is missing", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"gone"},"request_id":"req_011CSHoEeqs5C35K2UUqR7Fy"}`)
		})
		_, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})

		var e *llm.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, "req_011CSHoEeqs5C35K2UUqR7Fy", e.RequestID)
	})

	t.Run("the header wins", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
			w.Header().Set("request-id", requestID)
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","message":"gone"},"request_id":"req_other"}`)
		})
		_, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})

		var e *llm.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, requestID, e.RequestID)
	})
}

func TestClient_ErrorMessageIsBounded(t *testing.T) {
	// A proxy's error page is not worth carrying whole into a log line.
	api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", 100_000))
	})
	_, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})

	var e *llm.Error
	require.ErrorAs(t, err, &e)
	assert.LessOrEqual(t, len(e.Message), 2048)
	assert.NotEmpty(t, e.Message)
}

func TestClient_TransportFailure(t *testing.T) {
	check := func(t *testing.T, resp *llm.Response, err error) {
		t.Helper()
		require.Error(t, err)
		assert.Nil(t, resp)
		var e *llm.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, anthropic.Name, e.Provider)
		assert.Zero(t, e.Status, "no response arrived")
		require.Error(t, e.Err)
		assert.True(t, e.Retryable)
		assert.True(t, llm.Retryable(err))
	}

	t.Run("nothing is listening", func(t *testing.T) {
		api := newFakeAPI(t, replyJSON(okBody))
		c := api.client(t, anthropic.Options{})
		api.srv.Close()

		resp, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
		check(t, resp, err)
		resp, err = c.Stream(context.Background(), llm.Request{Messages: hello()}, func(llm.Delta) error { return nil })
		check(t, resp, err)
	})

	t.Run("the connection drops in the middle of a reply", func(t *testing.T) {
		api := newFakeAPI(t, replyCut(t, okBody[:40]))
		resp, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})
		check(t, resp, err)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})
}

// The status is the answer. An error whose body was cut short is still the
// error the status says it is, with whatever of the body arrived.
func TestClient_ErrorStatusWithABodyCutShort(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
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
		body := errorBody("overloaded_error", "Overloaded")
		_, _ = buf.WriteString("HTTP/1.1 529 Overloaded\r\nContent-Type: application/json\r\nrequest-id: " + requestID +
			"\r\nContent-Length: " + strconv.Itoa(len(body)+100) + "\r\n\r\n" + body)
		_ = buf.Flush()
	})
	c := api.client(t, anthropic.Options{})

	check := func(t *testing.T, err error) {
		t.Helper()
		var e *llm.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, 529, e.Status)
		assert.Equal(t, "overloaded_error", e.Type)
		assert.Equal(t, requestID, e.RequestID)
		assert.True(t, e.Retryable)
	}
	_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
	check(t, err)
	_, err = c.Stream(context.Background(), llm.Request{Messages: hello()}, func(llm.Delta) error { return nil })
	check(t, err)
}

func TestClient_CancelledContextIsNotRetryable(t *testing.T) {
	api := newFakeAPI(t, replyEither(okBody, okStream))
	c := api.client(t, anthropic.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	check := func(t *testing.T, resp *llm.Response, err error) {
		t.Helper()
		require.Error(t, err)
		assert.Nil(t, resp)
		require.ErrorIs(t, err, context.Canceled)
		assert.False(t, llm.Retryable(err))
		var e *llm.Error
		assert.NotErrorAs(t, err, &e, "the caller gave up: that is the context's error and not the provider's")
	}

	resp, err := c.Generate(ctx, llm.Request{Messages: hello()})
	check(t, resp, err)
	resp, err = c.Stream(ctx, llm.Request{Messages: hello()}, func(llm.Delta) error { return nil })
	check(t, resp, err)
}

func TestClient_DeadlineIsNotRetryable(t *testing.T) {
	released := make(chan struct{})
	api := newFakeAPI(t, func(http.ResponseWriter, received) { <-released })
	t.Cleanup(func() { close(released) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := api.client(t, anthropic.Options{}).Generate(ctx, llm.Request{Messages: hello()})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, llm.Retryable(err))
	var e *llm.Error
	assert.NotErrorAs(t, err, &e, "the caller's deadline is the context's error and not the provider's")
}

// A reply past the bound is refused instead of read whole. Passing a size
// bound is a plain error: the same request would pass it again.
func TestClient_Generate_ReplyOverTheBodyBound(t *testing.T) {
	const bound = 32 << 20
	chunk := strings.Repeat("a", 1<<20)

	api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", requestID)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"`)
		for range bound/len(chunk) + 1 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	resp, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "larger than")
	assert.False(t, llm.Retryable(err))
	var e *llm.Error
	assert.NotErrorAs(t, err, &e)
}
