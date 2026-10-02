package openai

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// The defaults the options document: nothing else shows them, since a test
// that overrode each would not see what the zero value does.
func TestNew_Defaults(t *testing.T) {
	c, err := New(Options{Model: "m"})
	require.NoError(t, err)

	assert.Equal(t, DefaultBaseURL, c.baseURL)
	assert.Equal(t, "system", c.systemRole)
	assert.Same(t, slog.Default(), c.log)
	assert.Zero(t, c.maxTokens, "no bound unless one is set")
	assert.Empty(t, c.apiKey, "no key, no bearer token")
	assert.Equal(t, 2*time.Minute, c.idleTimeout)
	assert.Equal(t, maxBodyBytes, c.maxReplyBytes)

	assert.Zero(t, c.http.Timeout, "a client timeout would cut a stream, which has no whole-request bound")
	assert.Equal(t, 10*time.Minute, c.callTimeout, "the bound on a call that is not a stream")
}

func TestNew_IdleTimeout(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "zero is two minutes", in: 0, want: 2 * time.Minute},
		{name: "a limit is kept", in: 45 * time.Second, want: 45 * time.Second},
		{name: "negative is none", in: -1, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(Options{Model: "m", IdleTimeout: tt.in})
			require.NoError(t, err)
			assert.Equal(t, tt.want, c.idleTimeout)
		})
	}
}

// A client the caller supplies has the caller's timeouts and no others.
func TestNew_SuppliedClientHasNoCallTimeout(t *testing.T) {
	c, err := New(Options{Model: "m", HTTPClient: &http.Client{Timeout: time.Second}})
	require.NoError(t, err)
	assert.Zero(t, c.callTimeout)
	assert.Equal(t, time.Second, c.http.Timeout, "the caller's own timeout is kept")
}

func internalClient(t *testing.T, h http.HandlerFunc, mod func(*Client)) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL, Model: "m", Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	if mod != nil {
		mod(c)
	}
	return c
}

func sseEvents(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "data: %s\n\n", e)
	}
	return b.String()
}

func content(text string) string {
	return fmt.Sprintf(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text)
}

func arguments(index int, id, args string) string {
	head := fmt.Sprintf(`"index":%d`, index)
	if id != "" {
		head += fmt.Sprintf(`,"id":%q,"type":"function"`, id)
	}
	return fmt.Sprintf(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{%s,"function":{"name":"f","arguments":%q}}]},"finish_reason":null}]}`, head, args)
}

func refusal(text string) string {
	return fmt.Sprintf(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"refusal":%q},"finish_reason":null}]}`, text)
}

const finish = `{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

// The default client's whole-call timeout is for a call that has an answer to
// wait for. It does not apply to a stream, which may run as long as it sends.
func TestDefaultClient_CallTimeoutDoesNotCutAStream(t *testing.T) {
	c := internalClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 10 {
			_, _ = io.WriteString(w, sseEvents(content("w")))
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
		_, _ = io.WriteString(w, sseEvents(finish, "[DONE]"))
	}, func(c *Client) { c.callTimeout = 50 * time.Millisecond })

	start := time.Now()
	resp, err := c.Stream(t.Context(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}}}, nil)

	require.NoError(t, err)
	assert.Equal(t, "wwwwwwwwww", resp.Message.Text)
	assert.Greater(t, time.Since(start), 3*c.callTimeout, "the stream outlived the call timeout, which is the point")
}

func TestDefaultClient_CallTimeoutBoundsACallThatIsNotAStream(t *testing.T) {
	c := internalClient(t, func(_ http.ResponseWriter, r *http.Request) {
		// The body is read so that the server can see the client hang up.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}, func(c *Client) { c.callTimeout = 60 * time.Millisecond })
	ctx := t.Context()

	for name, call := range map[string]func() error{
		"Generate": func() error {
			_, err := c.Generate(ctx, llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}}})
			return err
		},
		"Embed": func() error {
			_, err := c.Embed(ctx, llm.EmbedRequest{Model: "e", Input: []string{"a"}})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()

			var got *llm.Error
			require.ErrorAs(t, err, &got)
			assert.True(t, got.Retryable, "the caller's context is live, so this is the client's own timeout")
			assert.NoError(t, ctx.Err())
		})
	}
}

// The bound on the reply is the package's body bound, applied to what the
// reply adds up to, text and tool-call arguments together.
func TestStream_ReplyBoundCountsTextAndArguments(t *testing.T) {
	tests := []struct {
		name    string
		events  []string
		wantErr bool
	}{
		{name: "text up to the bound", events: []string{content(strings.Repeat("a", 60)), content(strings.Repeat("a", 40))}},
		{name: "text past the bound", events: []string{content(strings.Repeat("a", 60)), content(strings.Repeat("a", 41))}, wantErr: true},
		{name: "arguments up to the bound", events: []string{arguments(0, "c1", strings.Repeat("a", 60)), arguments(0, "", strings.Repeat("a", 40))}},
		{name: "arguments past the bound", events: []string{arguments(0, "c1", strings.Repeat("a", 60)), arguments(0, "", strings.Repeat("a", 41))}, wantErr: true},
		{name: "arguments of two calls add up", events: []string{arguments(0, "c1", strings.Repeat("a", 60)), arguments(1, "c2", strings.Repeat("a", 41))}, wantErr: true},
		{name: "refusal text counts too", events: []string{refusal(strings.Repeat("a", 60)), refusal(strings.Repeat("a", 41))}, wantErr: true},
		{name: "text and arguments add up", events: []string{content(strings.Repeat("a", 50)), arguments(0, "c1", strings.Repeat("a", 51))}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := sseEvents(append(tt.events, finish, "[DONE]")...)
			c := internalClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			}, func(c *Client) { c.maxReplyBytes = 100 })

			_, err := c.Stream(t.Context(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}}}, nil)

			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var apiErr *llm.Error
			assert.False(t, errors.As(err, &apiErr), "a plain error: a retry meets the same size")
			assert.False(t, llm.Retryable(err))
			assert.Contains(t, err.Error(), "larger than 100 bytes")
		})
	}
}
