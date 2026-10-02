package openai_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

// testModel is the model every test client is configured with, so a request
// that names none shows up in the body as this.
const testModel = "test-model"

// simpleReply is the smallest completion a client can read: used where the
// test is about the request the server received.
const simpleReply = `{"id":"chatcmpl-1","model":"test-model","choices":[` +
	`{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

// seen is one request a test server received.
type seen struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// JSON decodes the request body, failing the test when it is not an object.
func (s seen) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(s.Body, &m), "body: %s", s.Body)
	return m
}

// recorder keeps every request a test server received, in order.
type recorder struct {
	mu   sync.Mutex
	reqs []seen
}

// add records req and leaves its body readable for the handler.
func (r *recorder) add(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, seen{Method: req.Method, Path: req.URL.Path, Header: req.Header.Clone(), Body: body})
}

// only returns the one request the server received.
func (r *recorder) only(t *testing.T) seen {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Len(t, r.reqs, 1, "requests received")
	return r.reqs[0]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *recorder) all() []seen {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seen(nil), r.reqs...)
}

// streamRequested reports whether the request body asks for a stream.
func streamRequested(r *http.Request) bool {
	var body struct {
		Stream bool `json:"stream"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	return body.Stream
}

// newServer starts a server that records each request and then lets h
// answer it.
func newServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// answer replies with a fixed status and JSON body.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// serveJSON replies 200 with a JSON body.
func serveJSON(body string) http.HandlerFunc { return answer(http.StatusOK, body) }

// serveStream replies 200 with a server-sent event stream.
func serveStream(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

// testdata reads a canned body.
func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

// newClient builds a client for srv. The base URL carries a /v1 prefix, as a
// real one does, so a path built wrongly shows.
func newClient(t *testing.T, srv *httptest.Server, mods ...func(*openai.Options)) *openai.Client {
	t.Helper()
	opts := openai.Options{BaseURL: srv.URL + "/v1", Model: testModel, Logger: slog.New(slog.DiscardHandler)}
	for _, mod := range mods {
		mod(&opts)
	}
	c, err := openai.New(opts)
	require.NoError(t, err)
	return c
}

func userMsg(text string) []llm.Message {
	return []llm.Message{{Role: llm.RoleUser, Text: text}}
}

// sseBody frames each data payload as one event and ends with [DONE].
func sseBody(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		fmt.Fprintf(&b, "data: %s\n\n", p)
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		opts    openai.Options
		wantErr string
	}{
		{name: "refuses an empty model", opts: openai.Options{}, wantErr: "model"},
		{name: "refuses an empty model even with a key and a base url", opts: openai.Options{APIKey: "k", BaseURL: "http://localhost:11434/v1"}, wantErr: "model"},
		{name: "a model alone is enough", opts: openai.Options{Model: "llama"}},
		{name: "every option set", opts: openai.Options{
			APIKey: "k", BaseURL: "http://localhost:1/v1", Model: "m", EmbeddingModel: "e",
			MaxTokens: 10, LegacyMaxTokens: true, SystemRole: "developer",
			Header: http.Header{"X-A": {"b"}}, HTTPClient: &http.Client{}, Logger: nil,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := openai.New(tt.opts)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, c)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, c)
		})
	}
}

func TestConstants(t *testing.T) {
	assert.Equal(t, "openai", openai.Name)
	assert.Equal(t, "https://api.openai.com/v1", openai.DefaultBaseURL)
}

func TestNew_DoesNotShareTheCallersHeader(t *testing.T) {
	srv, rec := newServer(t, serveJSON(simpleReply))
	hdr := http.Header{"X-Gateway": {"one"}}
	c := newClient(t, srv, func(o *openai.Options) { o.Header = hdr })
	hdr.Set("X-Gateway", "changed after New")

	_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
	require.NoError(t, err)
	assert.Equal(t, []string{"one"}, rec.only(t).Header.Values("X-Gateway"))
}

// logged returns a logger for Options and a function that says what it has
// been asked to write.
func logged() (*slog.Logger, func() string) {
	var buf bytes.Buffer
	var mu sync.Mutex
	h := slog.NewTextHandler(&lockedWriter{mu: &mu, w: &buf}, &slog.HandlerOptions{Level: slog.LevelWarn})
	return slog.New(h), func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type countingTransport struct {
	next  http.RoundTripper
	calls atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.next.RoundTrip(r)
}

func TestNew_UsesTheHTTPClientItIsGiven(t *testing.T) {
	srv, _ := newServer(t, serveJSON(simpleReply))
	transport := &countingTransport{next: http.DefaultTransport}
	c := newClient(t, srv, func(o *openai.Options) { o.HTTPClient = &http.Client{Transport: transport} })

	_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})

	require.NoError(t, err)
	assert.EqualValues(t, 1, transport.calls.Load())
}

// A Client holds no state beyond its options, so concurrent calls, of every
// kind, share nothing. Run under -race.
func TestClient_ConcurrentUse(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/embeddings":
			echoVectors(w, r)
		case streamRequested(r):
			serveStream(sseBody(chunk(textDelta("ok"), ""), chunk(`{}`, "stop")))(w, r)
		default:
			serveJSON(simpleReply)(w, r)
		}
	})
	c := newClient(t, srv, func(o *openai.Options) { o.EmbeddingModel = "e" })

	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for range 20 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, err := c.Generate(t.Context(), llm.Request{Messages: userMsg("hi")})
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := c.Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := c.Embed(t.Context(), llm.EmbedRequest{Input: []string{"a", "b"}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}
}

func TestStream_ANilCallbackIsNoCallback(t *testing.T) {
	srv, _ := newServer(t, serveStream(sseBody(chunk(textDelta("ok"), ""), chunk(`{}`, "stop"))))

	resp, err := newClient(t, srv).Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, nil)

	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Message.Text)
}
