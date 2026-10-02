package anthropic_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
)

// testKey is not a credential: no test here reaches a real endpoint.
const testKey = "test-key"

// okBody is the smallest reply the Messages API documents, for tests that
// only look at what was sent.
const okBody = `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5",` +
	`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,` +
	`"usage":{"input_tokens":1,"output_tokens":1}}`

// okStream is okBody as the event sequence the streaming reference documents.
const okStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// received is one request the fake API was sent.
type received struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
	// Gone is closed when the client has gone away.
	Gone <-chan struct{}
}

// reply answers one request the fake API received.
type reply func(w http.ResponseWriter, req received)

// fakeAPI is an httptest server standing in for the Messages API. It records
// every request and answers each with a reply.
type fakeAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []received
}

func newFakeAPI(t *testing.T, answer reply) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		req := received{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body, Gone: r.Context().Done()}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.mu.Unlock()
		answer(w, req)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// client builds a Client pointed at the fake. opts needs no key or address.
func (f *fakeAPI) client(t *testing.T, opts anthropic.Options) *anthropic.Client {
	t.Helper()
	if opts.APIKey == "" {
		opts.APIKey = testKey
	}
	opts.BaseURL = f.srv.URL
	c, err := anthropic.New(opts)
	require.NoError(t, err)
	return c
}

// clientWithin is client with the time an unstreamed call is given set to
// timeout, which is otherwise ten minutes.
func (f *fakeAPI) clientWithin(t *testing.T, opts anthropic.Options, timeout time.Duration) *anthropic.Client {
	t.Helper()
	if opts.APIKey == "" {
		opts.APIKey = testKey
	}
	opts.BaseURL = f.srv.URL
	c, err := anthropic.NewWithRequestTimeout(opts, timeout)
	require.NoError(t, err)
	return c
}

// smallBound is the bound the size tests hold a reply to in place of 32 MiB,
// so that a test of the bound need not move 33 MiB to pass it.
const smallBound = 1 << 20

// clientSmall is a client that holds a reply to smallBound.
func (f *fakeAPI) clientSmall(t *testing.T) *anthropic.Client {
	t.Helper()
	c, err := anthropic.NewWithBound(anthropic.Options{APIKey: testKey, BaseURL: f.srv.URL}, smallBound)
	require.NoError(t, err)
	return c
}

// last returns the most recent request.
func (f *fakeAPI) last(t *testing.T) received {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.requests, "the fake API received no request")
	return f.requests[len(f.requests)-1]
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// requestID is the request-id header every reply of the fake carries, in the
// form the errors reference shows.
const requestID = "req_018EeWyXxfu5pfWkrYcMdjWG"

// replyJSON answers 200 with a JSON body.
func replyJSON(body string) reply {
	return func(w http.ResponseWriter, _ received) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", requestID)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

// replyStream answers with a server-sent event stream.
func replyStream(events string) reply {
	return func(w http.ResponseWriter, _ received) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", requestID)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, events)
	}
}

// replyEither answers a request that asked for a stream with events and any
// other with body, so one fake serves Generate and Stream alike.
func replyEither(body, events string) reply {
	return func(w http.ResponseWriter, req received) {
		var asked struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(req.Body, &asked)
		if asked.Stream {
			replyStream(events)(w, req)
			return
		}
		replyJSON(body)(w, req)
	}
}

// fixture reads a canned body or event sequence from testdata.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	if strings.HasSuffix(name, ".sse") {
		// An editor that trims trailing blank lines would turn the last
		// event into one cut short.
		require.True(t, strings.HasSuffix(string(b), "\n\n"),
			"%s must end with the blank line that ends its last event", name)
	}
	return string(b)
}

// contentOf returns the content array of a canned Messages API body, as the
// bytes the file holds.
func contentOf(t *testing.T, body string) json.RawMessage {
	t.Helper()
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &m))
	require.NotEmpty(t, m.Content)
	return m.Content
}

func userTurn(text string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Text: text}
}

func hello() []llm.Message {
	return []llm.Message{userTurn("Hello")}
}

func TestNew(t *testing.T) {
	t.Run("refuses an empty APIKey", func(t *testing.T) {
		c, err := anthropic.New(anthropic.Options{})
		require.Error(t, err)
		assert.Nil(t, c)
		assert.Contains(t, err.Error(), "APIKey")
	})

	t.Run("needs nothing but a key", func(t *testing.T) {
		c, err := anthropic.New(anthropic.Options{APIKey: testKey})
		require.NoError(t, err)
		assert.NotNil(t, c)
	})

	t.Run("refuses a refusal fallback it does not know", func(t *testing.T) {
		_, err := anthropic.New(anthropic.Options{APIKey: testKey, RefusalFallback: "Default"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "RefusalFallback")
	})

	t.Run("refuses an Extra value that cannot be sent", func(t *testing.T) {
		_, err := anthropic.New(anthropic.Options{
			APIKey: testKey,
			Extra:  map[string]any{"metadata": func() {}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Extra")
	})
}

func TestConstants(t *testing.T) {
	assert.Equal(t, "anthropic", anthropic.Name)
	assert.Equal(t, "claude-opus-5-5", anthropic.DefaultModel)
	assert.Equal(t, "https://api.anthropic.com", anthropic.DefaultBaseURL)
	assert.Equal(t, "2023-06-01", anthropic.Version)
}

func TestClient_RequestLine(t *testing.T) {
	api := newFakeAPI(t, replyJSON(okBody))
	c := api.client(t, anthropic.Options{})

	_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
	require.NoError(t, err)

	got := api.last(t)
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/v1/messages", got.Path)
	assert.Equal(t, testKey, got.Header.Get("x-api-key"))
	assert.Equal(t, "2023-06-01", got.Header.Get("anthropic-version"))
	assert.Equal(t, "application/json", got.Header.Get("content-type"))
	_, sent := got.Header["Anthropic-Beta"]
	assert.False(t, sent, "anthropic-beta is sent only when there is a beta to name")
}

func TestClient_BaseURLThatIsNotAnAddress(t *testing.T) {
	for _, base := range []string{"http://[::1", "api.keel.test", "/v1"} {
		t.Run(base, func(t *testing.T) {
			c, err := anthropic.New(anthropic.Options{APIKey: testKey, BaseURL: base})
			require.Error(t, err)
			assert.Nil(t, c)
			assert.Contains(t, err.Error(), "BaseURL")
		})
	}
}

func TestClient_BaseURLWithATrailingSlash(t *testing.T) {
	api := newFakeAPI(t, replyJSON(okBody))
	c, err := anthropic.New(anthropic.Options{APIKey: testKey, BaseURL: api.srv.URL + "/"})
	require.NoError(t, err)

	_, err = c.Generate(context.Background(), llm.Request{Messages: hello()})
	require.NoError(t, err)
	assert.Equal(t, "/v1/messages", api.last(t).Path)
}

// countingTransport counts the requests that pass through it.
type countingTransport struct {
	next  http.RoundTripper
	calls atomic.Int64
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.next.RoundTrip(r)
}

func TestClient_UsesTheHTTPClientGiven(t *testing.T) {
	api := newFakeAPI(t, replyJSON(okBody))
	transport := &countingTransport{next: http.DefaultTransport}
	c := api.client(t, anthropic.Options{HTTPClient: &http.Client{Transport: transport}})

	_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
	require.NoError(t, err)
	assert.Equal(t, int64(1), transport.calls.Load())
	assert.Equal(t, 1, api.count(), "one call is one request: a Client does not retry")
}

func TestClient_OneRequestPerFailedCall(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	_, err := api.client(t, anthropic.Options{}).Generate(context.Background(), llm.Request{Messages: hello()})
	require.Error(t, err)
	assert.True(t, llm.Retryable(err))
	assert.Equal(t, 1, api.count(), "retrying is llm.Retrying's job")
}

func TestClient_ConcurrentUse(t *testing.T) {
	// Run under -race. A Client holds nothing a call changes.
	api := newFakeAPI(t, replyEither(fixture(t, "stream_tool_use.json"), fixture(t, "stream_tool_use.sse")))
	c := api.client(t, anthropic.Options{
		RefusalFallback: "default",
		Extra:           map[string]any{"metadata": map[string]any{"user_id": "u_1"}, "max_tokens": 512},
	})
	req := llm.Request{System: "You are terse.", Messages: hello(), Tools: []llm.Tool{{Name: "get_weather"}}}

	want, err := c.Generate(context.Background(), req)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got *llm.Response
			var err error
			if i%2 == 0 {
				got, err = c.Generate(context.Background(), req)
			} else {
				got, err = c.Stream(context.Background(), req, func(llm.Delta) error { return nil })
			}
			if assert.NoError(t, err) {
				assert.Equal(t, want.Message.Text, got.Message.Text)
				assert.Equal(t, want.Usage, got.Usage)
			}
		}()
	}
	wg.Wait()
}
