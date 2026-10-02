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
	// A call counts its overhead, its id and its name as well as its arguments,
	// so the bounds below leave room for those and pin the arguments exactly.
	oneCall := callOverhead + len("c1") + len("f")
	twoCalls := 2*callOverhead + len("c1") + len("c2") + 2*len("f")

	tests := []struct {
		name    string
		limit   int
		events  []string
		wantErr bool
	}{
		{name: "text up to the bound", limit: 100, events: []string{content(strings.Repeat("a", 60)), content(strings.Repeat("a", 40))}},
		{name: "text past the bound", limit: 100, events: []string{content(strings.Repeat("a", 60)), content(strings.Repeat("a", 41))}, wantErr: true},
		{name: "arguments up to the bound", limit: oneCall + 100, events: []string{arguments(0, "c1", strings.Repeat("a", 60)), arguments(0, "", strings.Repeat("a", 40))}},
		{name: "arguments past the bound", limit: oneCall + 100, events: []string{arguments(0, "c1", strings.Repeat("a", 60)), arguments(0, "", strings.Repeat("a", 41))}, wantErr: true},
		{name: "arguments of two calls add up", limit: twoCalls + 100, events: []string{arguments(0, "c1", strings.Repeat("a", 60)), arguments(1, "c2", strings.Repeat("a", 41))}, wantErr: true},
		{name: "refusal text counts too", limit: 100, events: []string{refusal(strings.Repeat("a", 60)), refusal(strings.Repeat("a", 41))}, wantErr: true},
		{name: "text and arguments add up", limit: oneCall + 100, events: []string{content(strings.Repeat("a", 50)), arguments(0, "c1", strings.Repeat("a", 51))}, wantErr: true},
		{name: "text and arguments up to the bound", limit: oneCall + 100, events: []string{content(strings.Repeat("a", 50)), arguments(0, "c1", strings.Repeat("a", 50))}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := sseEvents(append(tt.events, finish, "[DONE]")...)
			c := internalClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			}, func(c *Client) { c.maxReplyBytes = tt.limit })

			_, err := c.Stream(t.Context(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}}}, nil)

			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var apiErr *llm.Error
			assert.False(t, errors.As(err, &apiErr), "a plain error: a retry meets the same size")
			assert.False(t, llm.Retryable(err))
			assert.Contains(t, err.Error(), fmt.Sprintf("larger than %d bytes", tt.limit))
		})
	}
}

// Every byte the reply keeps counts, ids and names as much as text, and each
// call counts a fixed amount besides, for what it costs apart from its text, so
// a server that opens calls without end meets the bound.
func TestStream_ToolCallIdsNamesAndNumberAreBounded(t *testing.T) {
	call := func(index int, id, name string) string {
		return fmt.Sprintf(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`, index, id, name)
	}
	small := func(n int) []string {
		var events []string
		for i := range n {
			events = append(events, call(i, fmt.Sprintf("c%d", i), "f"))
		}
		return events
	}
	perCall := callOverhead + len("c0") + len("f")

	tests := []struct {
		name    string
		limit   int
		events  []string
		wantErr bool
	}{
		{name: "an id and a name up to the bound", limit: callOverhead + 100, events: []string{call(0, strings.Repeat("a", 60), strings.Repeat("a", 40))}},
		{name: "a name past the bound", limit: callOverhead + 100, events: []string{call(0, strings.Repeat("a", 60), strings.Repeat("a", 41))}, wantErr: true},
		{name: "an id past the bound", limit: callOverhead + 100, events: []string{call(0, strings.Repeat("a", 61), strings.Repeat("a", 40))}, wantErr: true},
		{name: "many small calls up to the bound", limit: 3 * perCall, events: small(3)},
		{name: "many small calls past the bound", limit: 3 * perCall, events: small(4), wantErr: true},
		{name: "a call is counted once however many pieces repeat its id and name", limit: perCall, events: []string{call(0, "c0", "f"), call(0, "c0", "f"), call(0, "c0", "f")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := sseEvents(append(tt.events, finish, "[DONE]")...)
			c := internalClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			}, func(c *Client) { c.maxReplyBytes = tt.limit })

			_, err := c.Stream(t.Context(), llm.Request{Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}}}, nil)

			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			var apiErr *llm.Error
			assert.False(t, errors.As(err, &apiErr), "a plain error: a retry meets the same size")
			assert.Contains(t, err.Error(), fmt.Sprintf("larger than %d bytes", tt.limit))
		})
	}
}

// The key and the caller's headers go only to the origin that was configured:
// the same scheme, host and port, with a default port the same as none. A hop
// to another origin loses them whatever the hops before it were.
func TestGuardedClient_WithholdsFromAnotherOrigin(t *testing.T) {
	newGuarded := func(t *testing.T, baseURL string) *Client {
		t.Helper()
		c, err := New(Options{
			Model: "m", APIKey: "sk-test", BaseURL: baseURL,
			Header:     http.Header{"X-Gateway": {"secret"}},
			HTTPClient: &http.Client{},
		})
		require.NoError(t, err)
		return c
	}
	request := func(t *testing.T, url string) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, url, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Gateway", "secret")
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	tests := []struct {
		name      string
		baseURL   string
		first     string // the request that began the chain
		redirect  string
		wantKeeps bool
	}{
		{name: "the same origin", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "https://api.test/v1/moved", wantKeeps: true},
		{name: "the default port written out", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "https://api.test:443/moved", wantKeeps: true},
		{name: "the default port written out in the base url", baseURL: "https://api.test:443/v1", first: "https://api.test:443/v1/chat", redirect: "https://api.test/moved", wantKeeps: true},
		{name: "the default http port written out", baseURL: "http://api.test/v1", first: "http://api.test/v1/chat", redirect: "http://api.test:80/moved", wantKeeps: true},
		{name: "the scheme in another case", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "HTTPS://api.test/moved", wantKeeps: true},
		{name: "the host in another case", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "https://API.TEST/moved", wantKeeps: true},
		{name: "http for https on the same host name", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "http://api.test/moved"},
		{name: "https for http on the same host name", baseURL: "http://api.test/v1", first: "http://api.test/v1/chat", redirect: "https://api.test/moved"},
		{name: "another scheme on the same explicit port", baseURL: "https://api.test:8443/v1", first: "https://api.test:8443/v1/chat", redirect: "http://api.test:8443/moved"},
		{name: "another port", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "https://api.test:8443/moved"},
		{name: "another port on a local runtime", baseURL: "http://localhost:11434/v1", first: "http://localhost:11434/v1/chat", redirect: "http://localhost:11435/moved"},
		{name: "the same local port", baseURL: "http://localhost:11434/v1", first: "http://localhost:11434/v1/chat", redirect: "http://localhost:11434/moved", wantKeeps: true},
		{name: "another host", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "https://elsewhere.test/moved"},
		{name: "a host that only starts the same", baseURL: "https://api.test/v1", first: "https://api.test/v1/chat", redirect: "https://api.test.example/moved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newGuarded(t, tt.baseURL)
			next := request(t, tt.redirect)

			require.NoError(t, c.http.CheckRedirect(next, []*http.Request{request(t, tt.first)}))

			if tt.wantKeeps {
				assert.Equal(t, "Bearer sk-test", next.Header.Get("Authorization"))
				assert.Equal(t, "secret", next.Header.Get("X-Gateway"))
			} else {
				assert.Empty(t, next.Header.Values("Authorization"))
				assert.Empty(t, next.Header.Values("X-Gateway"))
			}
			assert.Equal(t, "application/json", next.Header.Get("Content-Type"), "what is not ours is left alone")
		})
	}

	t.Run("it is measured against the configured origin, not the hop before", func(t *testing.T) {
		c := newGuarded(t, "https://api.test/v1")

		// A second hop within the other origin is still the other origin.
		next := request(t, "https://elsewhere.test/b")
		require.NoError(t, c.http.CheckRedirect(next, []*http.Request{request(t, "https://api.test/v1/chat"), request(t, "https://elsewhere.test/a")}))
		assert.Empty(t, next.Header.Values("Authorization"))
		assert.Empty(t, next.Header.Values("X-Gateway"))

		// A hop back to the configured origin is the configured origin.
		back := request(t, "https://api.test/v1/again")
		require.NoError(t, c.http.CheckRedirect(back, []*http.Request{request(t, "https://elsewhere.test/a")}))
		assert.Equal(t, "Bearer sk-test", back.Header.Get("Authorization"))
		assert.Equal(t, "secret", back.Header.Get("X-Gateway"))
	})
}
