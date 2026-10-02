package openai_test

import (
	"context"
	"errors"
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

// holdOpen keeps a handler from returning until the client hangs up.
func holdOpen(r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

const shortIdle = 150 * time.Millisecond

func withIdle(d time.Duration) func(*openai.Options) {
	return func(o *openai.Options) { o.IdleTimeout = d }
}

// asLLMError finds the *llm.Error in err, or fails.
func asLLMError(t *testing.T, err error) *llm.Error {
	t.Helper()
	var got *llm.Error
	require.ErrorAs(t, err, &got)
	return got
}

// notAnLLMError reports that err is a plain error: nothing in its chain is
// an *llm.Error, which is what a caller's retry logic would act on.
func notAnLLMError(t *testing.T, err error) {
	t.Helper()
	var got *llm.Error
	assert.False(t, errors.As(err, &got), "want a plain error, got %v", err)
	assert.False(t, llm.Retryable(err))
}

// The idle limit is the wait for a stream's next bytes. It is not a limit on
// the stream, which may run as long as it keeps sending.
func TestStream_IdleLimit(t *testing.T) {
	text := event(chunk(textDelta("hi"), ""))
	stop := event(chunk(`{}`, "stop"))
	done := event("[DONE]")

	t.Run("the server goes quiet before it answers", func(t *testing.T) {
		srv, _ := newServer(t, func(_ http.ResponseWriter, r *http.Request) { holdOpen(r) })

		_, err := newClient(t, srv, withIdle(shortIdle)).
			Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

		got := asLLMError(t, err)
		assert.True(t, got.Retryable, "the same request may find the server awake")
		assert.Zero(t, got.Status)
		assert.Contains(t, got.Error(), "no data from the server")
		assert.NotErrorIs(t, err, context.Canceled, "the caller did not cancel anything")
	})

	t.Run("the server goes quiet in the middle of the reply", func(t *testing.T) {
		srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			flushing(text)(w, r)
			holdOpen(r)
		})
		var log deltaLog

		_, err := newClient(t, srv, withIdle(shortIdle)).
			Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, log.fn)

		got := asLLMError(t, err)
		assert.True(t, got.Retryable)
		assert.NotErrorIs(t, err, context.Canceled)
		assert.Equal(t, []llm.Delta{{Text: "hi"}}, log.got, "what arrived was delivered")
	})

	t.Run("the server goes quiet after the finish reason, and the reply is complete", func(t *testing.T) {
		srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			flushing(text, stop)(w, r)
			holdOpen(r)
		})

		resp, err := newClient(t, srv, withIdle(shortIdle)).
			Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

		require.NoError(t, err)
		assert.Equal(t, "hi", resp.Message.Text)
		assert.Equal(t, llm.StopEnd, resp.Stop)
	})

	t.Run("a stream that keeps sending is not idle, even if it only sends comments", func(t *testing.T) {
		srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for range 10 {
				_, _ = io.WriteString(w, ": keep-alive\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(40 * time.Millisecond)
			}
			flushing(text, stop, done)(w, r)
		})

		start := time.Now()
		resp, err := newClient(t, srv, withIdle(shortIdle)).
			Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

		require.NoError(t, err)
		assert.Equal(t, "hi", resp.Message.Text)
		assert.Greater(t, time.Since(start), 2*shortIdle, "the stream outlived the idle limit, which is the point")
	})

	t.Run("a negative limit is no limit", func(t *testing.T) {
		srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			flushing(text)(w, r)
			time.Sleep(4 * shortIdle)
			flushing(stop, done)(w, r)
		})

		resp, err := newClient(t, srv, withIdle(-1)).
			Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

		require.NoError(t, err)
		assert.Equal(t, "hi", resp.Message.Text)
	})

	t.Run("the caller's context ends first, and its error is the one returned", func(t *testing.T) {
		srv, _ := newServer(t, func(_ http.ResponseWriter, r *http.Request) { holdOpen(r) })
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		_, err := newClient(t, srv, withIdle(time.Minute)).
			Stream(ctx, llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

		require.ErrorIs(t, err, context.DeadlineExceeded)
		notAnLLMError(t, err)
	})
}

// Nothing about the size of a stream is bounded by how long it takes: a long
// stream is fine. What is bounded is one event, and what the reply adds up to.
func TestStream_EventSizeIsBounded(t *testing.T) {
	const eventBytes = 17 << 20 // past the reader's bound of 16 MiB
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"x","choices":[{"index":0,"delta":{"content":"`)
		chunk := strings.Repeat("a", 1<<20)
		for range eventBytes >> 20 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, `"}}]}`+"\n\n")
	})

	_, err := newClient(t, srv).
		Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

	require.Error(t, err)
	notAnLLMError(t, err)
	assert.Contains(t, err.Error(), "larger than", "a retry would meet the same size")
}

func TestStream_ReplySizeIsBounded(t *testing.T) {
	const limit = 32 << 20
	piece := strings.Repeat("a", 1<<20)
	var events []string
	for range limit>>20 + 2 {
		events = append(events, event(chunk(textDelta(piece), "")))
	}
	srv, _ := newServer(t, flushing(append(events, event(chunk(`{}`, "stop")), event("[DONE]"))...))
	var seen int

	_, err := newClient(t, srv).
		Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { seen++; return nil })

	require.Error(t, err)
	notAnLLMError(t, err)
	assert.Contains(t, err.Error(), "larger than")
	assert.LessOrEqual(t, seen, limit>>20+1, "the stream stopped when the reply outgrew the bound")
}

func TestStream_ALongHealthyStreamIsNotCut(t *testing.T) {
	var events []string
	for range 300 {
		events = append(events, event(chunk(textDelta("word "), "")))
	}
	srv, _ := newServer(t, flushing(append(events, event(chunk(`{}`, "stop")), event("[DONE]"))...))

	resp, err := newClient(t, srv).
		Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })

	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("word ", 300), resp.Message.Text)
}

// A 200 that is not an event stream is not a cut stream: asking again gets
// the same page, so it is an error that says what came back, and a retry
// would only repeat it.
func TestStream_AResponseThatIsNotAnEventStream(t *testing.T) {
	tests := []struct {
		name        string
		contentType string // "-" sends none
		body        string
		wantInError string
	}{
		{name: "an html page from a proxy", contentType: "text/html; charset=utf-8", body: "<html>sign in</html>", wantInError: "text/html"},
		{name: "a server that ignored stream and sent the whole completion", contentType: "application/json", body: simpleReply, wantInError: "application/json"},
		{name: "plain text", contentType: "text/plain", body: "ok", wantInError: "text/plain"},
		{name: "no content type", contentType: "-", body: simpleReply, wantInError: "no content type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentType == "-" {
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", tt.contentType)
				}
				_, _ = io.WriteString(w, tt.body)
			})
			var log deltaLog

			resp, err := newClient(t, srv).Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, log.fn)

			require.Error(t, err)
			assert.Nil(t, resp)
			got := asLLMError(t, err)
			assert.Equal(t, "openai", got.Provider)
			assert.Equal(t, http.StatusOK, got.Status)
			assert.False(t, got.Retryable)
			assert.False(t, llm.Retryable(err))
			assert.Contains(t, got.Message, tt.wantInError)
			assert.Empty(t, log.got)
		})
	}

	for _, ct := range []string{"text/event-stream", "text/event-stream; charset=utf-8", "Text/Event-Stream"} {
		t.Run("accepted: "+ct, func(t *testing.T) {
			srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				_, _ = io.WriteString(w, sseBody(chunk(textDelta("hi"), ""), chunk(`{}`, "stop")))
			})
			resp, err := newClient(t, srv).Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { return nil })
			require.NoError(t, err)
			assert.Equal(t, "hi", resp.Message.Text)
		})
	}
}

// What a reply keeps is its text, its refusal, and the ids, names and
// arguments of its tool calls: all of it counts against the bound. A server
// that opens call after call with a huge name and no arguments, or with ids
// and nothing else, meets it as surely as one that streams text.
func TestStream_ToolCallNamesAndIdsAreBounded(t *testing.T) {
	big := strings.Repeat("a", 1<<20)

	tests := []struct {
		name  string
		piece func(i int) string
	}{
		{name: "a megabyte name on each of forty calls, with no arguments", piece: func(i int) string { return callPiece(i, "c"+strconv.Itoa(i), big, "") }},
		{name: "a megabyte id on each of forty calls", piece: func(i int) string { return callPiece(i, big+strconv.Itoa(i), "f", "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			for i := range 40 {
				events = append(events, event(chunk(callsDelta(tt.piece(i)), "")))
			}
			srv, _ := newServer(t, flushing(append(events, event(chunk(`{}`, "tool_calls")), event("[DONE]"))...))
			var seen int

			_, err := newClient(t, srv).
				Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error { seen++; return nil })

			require.Error(t, err, "a 40 MiB reply is not a reply within a 32 MiB bound")
			notAnLLMError(t, err)
			assert.Contains(t, err.Error(), "larger than")
			assert.Less(t, seen, 40, "the stream stopped when the reply outgrew the bound")
		})
	}
}

// A callback that takes longer than the idle limit is the caller's time, not
// the server's silence: the stream goes on when it returns.
func TestStream_ASlowCallbackIsNotTheServersSilence(t *testing.T) {
	const slow = 250 * time.Millisecond
	var events []string
	for range 4 {
		events = append(events, event(chunk(textDelta("w"), "")))
	}
	events = append(events, event(chunk(`{}`, "stop")), event("[DONE]"))

	t.Run("events arriving steadily, a callback slower than the limit", func(t *testing.T) {
		srv, _ := newServer(t, flushing(events...))

		resp, err := newClient(t, srv, withIdle(100*time.Millisecond)).
			Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error {
				time.Sleep(slow)
				return nil
			})

		require.NoError(t, err, "a callback that blocks is backpressure, and not a reason to retry")
		assert.Equal(t, "wwww", resp.Message.Text)
	})

	t.Run("the clock starts again when the callback returns", func(t *testing.T) {
		srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			flushing(events[0])(w, r)
			holdOpen(r)
		})
		returned := make(chan time.Time, 1)

		start := time.Now()
		_, err := newClient(t, srv, withIdle(100*time.Millisecond)).
			Stream(t.Context(), llm.Request{Messages: userMsg("hi")}, func(llm.Delta) error {
				time.Sleep(slow)
				returned <- time.Now()
				return nil
			})
		end := time.Now()

		got := asLLMError(t, err)
		assert.True(t, got.Retryable)
		assert.Contains(t, got.Error(), "no data from the server")
		back := <-returned
		assert.Greater(t, back.Sub(start), slow)
		assert.Less(t, end.Sub(back), 2*time.Second, "the server's real silence was still caught")
	})
}
