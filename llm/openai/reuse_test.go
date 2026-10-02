package openai_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/openai"
)

// newCountingServer starts a server that counts the connections it accepts,
// so a test can tell a connection that was reused from one that was not.
func newCountingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// flushing answers with a stream that is flushed after every event, as a
// server that streams really does: the body is chunked and its end is a chunk
// of its own, written when the handler returns. That arrives a moment after
// the last event, so a client that stops at DONE has not yet read it, and has
// to read it to be done with the connection.
func flushing(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			flusher.Flush()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func event(payload string) string { return "data: " + payload + "\n\n" }

const reuseCalls = 5

// Every call a client makes, in sequence, goes over one connection when it
// ends well, whatever the shape of the call. The control rows are the ones
// that must not be reused, so the hook is shown to see the difference.
func TestClient_ConnectionIsReused(t *testing.T) {
	text := event(chunk(textDelta("hi"), ""))
	stop := event(chunk(`{}`, "stop"))
	usage := event(usageChunk(finalUsage))
	done := event("[DONE]")

	generate := func(ctx context.Context, c *openai.Client) error {
		_, err := c.Generate(ctx, llm.Request{Messages: userMsg("hi")})
		return err
	}
	streamed := func(ctx context.Context, c *openai.Client) error {
		_, err := c.Stream(ctx, llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
		return err
	}
	embed := func(ctx context.Context, c *openai.Client) error {
		_, err := c.Embed(ctx, llm.EmbedRequest{Model: "e", Input: []string{"a"}})
		return err
	}
	stopped := func(ctx context.Context, c *openai.Client) error {
		_, err := c.Stream(ctx, llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return errors.New("stop") })
		return err
	}

	tests := []struct {
		name      string
		handler   http.HandlerFunc
		call      func(ctx context.Context, c *openai.Client) error
		wantErr   bool
		wantConns int32
	}{
		{name: "a stream that ends with DONE", handler: flushing(text, stop, usage, done), call: streamed, wantConns: 1},
		{name: "a stream that ends with DONE and no finish reason", handler: flushing(text, done), call: streamed, wantConns: 1},
		{name: "a stream that ends after its usage, with no DONE", handler: flushing(text, stop, usage), call: streamed, wantConns: 1},
		{name: "a stream answered with an error status", handler: failWith(429, nil, apiErr("requests", "", "slow")), call: streamed, wantErr: true, wantConns: 1},
		{name: "a stream answered 200 with a page that is not a stream", handler: answer(200, "<html>proxy</html>"), call: streamed, wantErr: true, wantConns: 1},
		{name: "a completion", handler: serveJSON(simpleReply), call: generate, wantConns: 1},
		{name: "a completion answered with an error status", handler: failWith(500, nil, apiErr("server_error", "", "oops")), call: generate, wantErr: true, wantConns: 1},
		{name: "a completion that cannot be read", handler: serveJSON(`<html></html>`), call: generate, wantErr: true, wantConns: 1},
		{name: "embeddings", handler: serveJSON(oneVector), call: embed, wantConns: 1},
		{name: "embeddings answered with an error status", handler: failWith(503, nil, apiErr("service_unavailable_error", "", "busy")), call: embed, wantErr: true, wantConns: 1},
		{name: "a redirect that is not followed", handler: failWith(307, http.Header{"Location": {"/elsewhere"}}, ""), call: generate, wantErr: true, wantConns: 1},

		{
			name:      "control: a callback that stops the stream leaves unread data, so the connection goes",
			handler:   flushing(text, text, text),
			call:      stopped,
			wantErr:   true,
			wantConns: reuseCalls,
		},
		{
			name: "control: a stream cut before its finish reason is a failed connection",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(text)+500))
				_, _ = io.WriteString(w, text)
			},
			call:      streamed,
			wantErr:   true,
			wantConns: reuseCalls,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, conns := newCountingServer(t, tt.handler)
			c := newClient(t, srv, func(o *openai.Options) { o.EmbeddingModel = "e" })

			for range reuseCalls {
				err := tt.call(t.Context(), c)
				if tt.wantErr {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
			assert.Equal(t, tt.wantConns, conns.Load(), "connections opened for %d calls in sequence", reuseCalls)
		})
	}
}

// A server that holds the stream open after DONE, or follows it with more than
// a connection is worth, does not hold the call: the drain is bounded in time
// and in bytes, and the connection is simply not reused.
func TestStream_DrainIsBounded(t *testing.T) {
	text := event(chunk(textDelta("hi"), ""))
	stop := event(chunk(`{}`, "stop"))
	done := event("[DONE]")

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "the server holds the stream open",
			handler: func(w http.ResponseWriter, r *http.Request) {
				flushing(text, stop, done)(w, r)
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
			},
		},
		{
			name: "the server follows DONE with more than a connection is worth, and ends",
			handler: func(w http.ResponseWriter, r *http.Request) {
				flushing(text, stop, done)(w, r)
				_, _ = io.WriteString(w, ": "+strings.Repeat("x", 1<<20)+"\n")
			},
		},
		{
			name: "the server never stops sending",
			handler: func(w http.ResponseWriter, r *http.Request) {
				flushing(text, stop, done)(w, r)
				junk := ": " + strings.Repeat("x", 1024) + "\n"
				for r.Context().Err() == nil {
					if _, err := io.WriteString(w, junk); err != nil {
						return
					}
					w.(http.Flusher).Flush()
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, conns := newCountingServer(t, tt.handler)
			c := newClient(t, srv)

			start := time.Now()
			for range 2 {
				resp, err := c.Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
				require.NoError(t, err)
				assert.Equal(t, "hi", resp.Message.Text)
			}

			assert.Less(t, time.Since(start), 5*time.Second, "the call waited for the server to finish")
			assert.EqualValues(t, 2, conns.Load(), "a connection that could not be drained is not reused")
		})
	}
}
