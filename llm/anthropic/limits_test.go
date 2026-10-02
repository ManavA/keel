package anthropic_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
)

// ping is the keep-alive event the streaming reference documents.
const ping = "event: ping\ndata: {\"type\": \"ping\"}\n\n"

// textStreamInTwo is the reference's basic stream cut where a long reply
// would still be under way: after its last text delta, before the block
// closes.
func textStreamInTwo(t *testing.T) (head, tail string) {
	t.Helper()
	head, tail, ok := strings.Cut(fixture(t, "stream_text.sse"), "event: content_block_stop")
	require.True(t, ok)
	return head, "event: content_block_stop" + tail
}

// replyPaced answers with an event stream written one part at a time, each
// flushed and followed by gap, until the client goes away.
func replyPaced(gap time.Duration, parts ...string) reply {
	return func(w http.ResponseWriter, req received) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", requestID)
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		for i, part := range parts {
			if _, err := io.WriteString(w, part); err != nil {
				return
			}
			flusher.Flush()
			if i == len(parts)-1 {
				return
			}
			select {
			case <-time.After(gap):
			case <-req.Gone:
				return
			}
		}
	}
}

// pings returns n keep-alive events, each its own part.
func pings(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = ping
	}
	return out
}

func noDeltas(llm.Delta) error { return nil }

// A stream is as long as the reply is. The time an unstreamed call is given
// is not a bound on it.
func TestClient_Stream_IsNotCutByTheRequestTimeout(t *testing.T) {
	const requestTimeout = 250 * time.Millisecond
	head, tail := textStreamInTwo(t)

	// Sixty keep-alives ten milliseconds apart: more than twice the timeout.
	parts := append(append([]string{head}, pings(60)...), tail)
	api := newFakeAPI(t, replyPaced(10*time.Millisecond, parts...))
	started := time.Now()
	resp, err := api.clientWithin(t, anthropic.Options{}, requestTimeout).
		Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
	lasted := time.Since(started)

	require.NoError(t, err)
	assert.Equal(t, "Hello!", resp.Message.Text)
	require.Greater(t, lasted, requestTimeout, "the stream must outlast the timeout it is being tested against")

	// The fixture can fail: the same timeout does end a call that is not
	// streamed, and that is the client's own timeout, so worth another try.
	slow := newFakeAPI(t, func(_ http.ResponseWriter, req received) { <-req.Gone })
	_, err = slow.clientWithin(t, anthropic.Options{}, requestTimeout).
		Generate(context.Background(), llm.Request{Messages: hello()})
	require.Error(t, err)
	var e *llm.Error
	require.ErrorAs(t, err, &e)
	assert.Zero(t, e.Status)
	require.Error(t, e.Err)
	assert.True(t, e.Retryable, "the caller's context is live: the timeout was the client's own")
}

// A caller who hands in a client with a Timeout has chosen a bound on every
// call, a stream included.
func TestClient_Stream_CallersClientTimeoutCutsIt(t *testing.T) {
	head, tail := textStreamInTwo(t)
	parts := append(append([]string{head}, pings(60)...), tail)
	api := newFakeAPI(t, replyPaced(10*time.Millisecond, parts...))
	c := api.client(t, anthropic.Options{HTTPClient: &http.Client{Timeout: 250 * time.Millisecond}})

	resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
	require.Error(t, err)
	assert.Nil(t, resp)
	var e *llm.Error
	require.ErrorAs(t, err, &e)
	assert.True(t, e.Retryable)
}

func TestClient_Stream_IdleLimit(t *testing.T) {
	head, tail := textStreamInTwo(t)
	const limit = 100 * time.Millisecond

	silent := []struct {
		name   string
		answer reply
		wantID string
	}{
		{
			name: "silent after the reply has begun",
			answer: func(w http.ResponseWriter, req received) {
				replyPaced(0, head)(w, req)
				<-req.Gone
			},
			wantID: requestID,
		},
		{
			name: "silent after the headers",
			answer: func(w http.ResponseWriter, req received) {
				replyPaced(0)(w, req)
				<-req.Gone
			},
			wantID: requestID,
		},
		{
			name:   "silent before the response begins",
			answer: func(_ http.ResponseWriter, req received) { <-req.Gone },
		},
		{
			name:   "a gap between events longer than the limit",
			answer: replyPaced(5*limit, head, tail),
			wantID: requestID,
		},
	}
	for _, tt := range silent {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, tt.answer)
			c := api.client(t, anthropic.Options{IdleTimeout: limit})

			started := time.Now()
			resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
			waited := time.Since(started)

			require.Error(t, err)
			assert.Nil(t, resp)
			assert.GreaterOrEqual(t, waited, limit)
			assert.Less(t, waited, 10*time.Second, "the stream was given up, not waited out")

			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, anthropic.Name, e.Provider)
			assert.Zero(t, e.Status)
			assert.Equal(t, tt.wantID, e.RequestID)
			require.Error(t, e.Err)
			assert.Contains(t, e.Err.Error(), "sent nothing for")
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			assert.True(t, e.Retryable)
			assert.True(t, llm.Retryable(err), "a stalled connection is worth another try")
			assert.NotErrorIs(t, err, context.Canceled, "the caller did not cancel anything")
		})
	}

	t.Run("a negative limit is no limit", func(t *testing.T) {
		api := newFakeAPI(t, replyPaced(5*limit, head, tail))
		c := api.client(t, anthropic.Options{IdleTimeout: -1})

		resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		assert.Equal(t, "Hello!", resp.Message.Text)
	})

	t.Run("keep-alive pings inside a long gap keep the stream alive", func(t *testing.T) {
		// Five times the limit passes between the two halves of the reply,
		// and never the limit between two events.
		parts := append(append([]string{head}, pings(50)...), tail)
		api := newFakeAPI(t, replyPaced(limit/10, parts...))
		c := api.client(t, anthropic.Options{IdleTimeout: limit})

		started := time.Now()
		resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		assert.Equal(t, "Hello!", resp.Message.Text)
		require.Greater(t, time.Since(started), limit, "the stream must outlast the limit it is being tested against")
	})

	t.Run("comment lines are bytes from the server too", func(t *testing.T) {
		// No event arrives for five times the limit. The server is plainly
		// alive all the while, and that is what the limit is about.
		comments := make([]string, 50)
		for i := range comments {
			comments[i] = ": keep-alive\n\n"
		}
		parts := append(append([]string{head}, comments...), tail)
		api := newFakeAPI(t, replyPaced(limit/10, parts...))
		c := api.client(t, anthropic.Options{IdleTimeout: limit})

		started := time.Now()
		resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		assert.Equal(t, "Hello!", resp.Message.Text)
		require.Greater(t, time.Since(started), limit, "the stream must outlast the limit it is being tested against")
	})

	t.Run("a callback slower than the limit is not the server's silence", func(t *testing.T) {
		// Events arrive steadily. It is fn that takes twice the limit over
		// each delta, and backpressure from the caller is not a stall.
		parts := strings.SplitAfter(fixture(t, "stream_tool_use.sse"), "\n\n")
		api := newFakeAPI(t, replyPaced(limit/20, parts...))
		c := api.client(t, anthropic.Options{IdleTimeout: limit})

		slow := 0
		resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, func(llm.Delta) error {
			if slow < 3 {
				slow++
				time.Sleep(2 * limit)
			}
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, 3, slow)
		assert.Equal(t, llm.StopToolUse, resp.Stop)
		assert.Equal(t, "Okay, let's check the weather for San Francisco, CA:", resp.Message.Text)
	})

	t.Run("the caller's cancellation is the caller's, whatever the limit", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, req received) {
			replyPaced(0, head)(w, req)
			<-req.Gone
		})
		c := api.client(t, anthropic.Options{IdleTimeout: time.Minute})
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		defer cancel()

		_, err := c.Stream(ctx, llm.Request{Messages: hello()}, noDeltas)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.False(t, llm.Retryable(err))
		var e *llm.Error
		assert.NotErrorAs(t, err, &e)
	})

	t.Run("zero is two minutes", func(t *testing.T) {
		for _, tt := range []struct {
			set  time.Duration
			want time.Duration
		}{
			{set: 0, want: 2 * time.Minute},
			{set: 45 * time.Second, want: 45 * time.Second},
			{set: -1, want: 0},
		} {
			c, err := anthropic.New(anthropic.Options{APIKey: testKey, IdleTimeout: tt.set})
			require.NoError(t, err)
			assert.Equal(t, tt.want, c.StreamIdleLimit(), "IdleTimeout %v", tt.set)
		}
	})
}

// megabyteDelta is a text delta one megabyte long.
func megabyteDelta() string {
	return "event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` +
		strings.Repeat("a", 1<<20) + `"}}` + "\n\n"
}

// replyEvents answers with head, then count copies of event, then tail.
func replyEvents(head, event string, count int, tail string) reply {
	return func(w http.ResponseWriter, _ received) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", requestID)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, head)
		for range count {
			if _, err := io.WriteString(w, event); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, tail)
	}
}

func TestClient_Stream_SizeBounds(t *testing.T) {
	head, tail := textStreamInTwo(t)

	// Passing a size bound is a plain error: the same request would pass it
	// again, so there is nothing to retry and nothing of the API's to report.
	refused := func(t *testing.T, resp *llm.Response, err error) {
		t.Helper()
		require.Error(t, err)
		assert.Nil(t, resp)
		assert.Contains(t, err.Error(), "larger than")
		assert.False(t, llm.Retryable(err))
		var e *llm.Error
		assert.NotErrorAs(t, err, &e)
	}

	t.Run("one event over the event bound is refused", func(t *testing.T) {
		huge := "event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` +
			strings.Repeat("a", 17<<20) + `"}}` + "\n\n"
		api := newFakeAPI(t, replyEvents(head, huge, 1, tail))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		refused(t, resp, err)
	})

	t.Run("a reply that outgrows the body bound is refused", func(t *testing.T) {
		// Each event is well under the event bound. It is the reply they
		// add up to that passes 32 MiB.
		api := newFakeAPI(t, replyEvents(head, megabyteDelta(), 33, tail))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		refused(t, resp, err)
	})

	t.Run("content that arrives in the start of a block counts too", func(t *testing.T) {
		// Thirty-three text blocks, each a megabyte long as it starts and
		// with no delta after.
		megabyte := strings.Repeat("a", 1<<20)
		var blocks strings.Builder
		for i := range 33 {
			blocks.WriteString("event: content_block_start\n" +
				`data: {"type":"content_block_start","index":` + strconv.Itoa(i+1) + `,"content_block":{"type":"text","text":"` + megabyte + `"}}` + "\n\n")
		}
		api := newFakeAPI(t, replyStream(head+blocks.String()+tail))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		refused(t, resp, err)
	})

	t.Run("tool-call names and ids count as much as text does", func(t *testing.T) {
		// Forty tool calls with no arguments at all, each with a name a
		// megabyte long. A bound that counted only text and arguments would
		// let all forty megabytes through.
		megabyte := strings.Repeat("a", 1<<20)
		var blocks strings.Builder
		for i := range 40 {
			index := strconv.Itoa(i + 1)
			blocks.WriteString("event: content_block_start\n" +
				`data: {"type":"content_block_start","index":` + index + `,"content_block":{"type":"tool_use","id":"toolu_` + index + `","name":"` + megabyte + `","input":{}}}` + "\n\n" +
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":` + index + `}` + "\n\n")
		}
		api := newFakeAPI(t, replyStream(head+blocks.String()+tail))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		refused(t, resp, err)
	})

	t.Run("what the message itself carries counts too", func(t *testing.T) {
		// An id fifteen megabytes long and eighteen megabytes of text: each
		// under the bound, and the reply that keeps both over it.
		start, rest, ok := strings.Cut(head, `"id": "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY"`)
		require.True(t, ok, "the fixture's message_start no longer has the id this test replaces")
		long := start + `"id": "` + strings.Repeat("m", 15<<20) + `"` + rest
		api := newFakeAPI(t, replyEvents(long, megabyteDelta(), 18, tail))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		refused(t, resp, err)
	})

	t.Run("how the reply ended counts too, and only as it last stood", func(t *testing.T) {
		// A refusal whose explanation is twelve megabytes long.
		ending := func(stop string) string {
			return "event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"` + stop + `","stop_sequence":null,"stop_details":{"type":"refusal","category":"bio","explanation":"` +
				strings.Repeat("e", 12<<20) + `"}},"usage":{"output_tokens":15}}` + "\n\n"
		}
		stop := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		closeBlock := "event: content_block_stop\ndata: {\"type\": \"content_block_stop\", \"index\": 0}\n\n"

		// Twenty-one megabytes of text and twelve of explanation is too much.
		api := newFakeAPI(t, replyEvents(head, megabyteDelta(), 21, closeBlock+ending("refusal")+stop))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		refused(t, resp, err)

		// Ten of text and two endings of twelve is not: the second ending
		// replaces the first, and the reply keeps twenty-two.
		api = newFakeAPI(t, replyEvents(head, megabyteDelta(), 10, closeBlock+ending("end_turn")+ending("refusal")+stop))
		resp, err = api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		assert.Equal(t, llm.StopRefusal, resp.Stop)
		assert.Len(t, resp.Refusal.Explanation, 12<<20)
	})

	t.Run("an error status with a body past the body bound is still that status", func(t *testing.T) {
		megabyte := strings.Repeat("a", 1<<20)
		api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
			w.Header().Set("request-id", requestID)
			w.WriteHeader(http.StatusBadGateway)
			for range 33 {
				if _, err := io.WriteString(w, megabyte); err != nil {
					return
				}
			}
		})
		c := api.client(t, anthropic.Options{})
		check := func(t *testing.T, resp *llm.Response, err error) {
			t.Helper()
			assert.Nil(t, resp)
			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, http.StatusBadGateway, e.Status, "the status is the answer, whatever became of the body")
			assert.Equal(t, requestID, e.RequestID)
			assert.True(t, e.Retryable)
			assert.LessOrEqual(t, len(e.Message), 2048)
		}
		resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		check(t, resp, err)
		resp, err = c.Generate(context.Background(), llm.Request{Messages: hello()})
		check(t, resp, err)
	})

	t.Run("the bound is on the reply and not on the stream", func(t *testing.T) {
		// More than 32 MiB passes over the wire in events that add nothing
		// to the reply, as a long stream's keep-alives do.
		filler := "event: note\n" + `data: {"type":"note","text":"` + strings.Repeat("a", 1<<20) + `"}` + "\n\n"
		api := newFakeAPI(t, replyEvents(head, filler, 33, tail))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		assert.Equal(t, "Hello!", resp.Message.Text)
	})

	t.Run("a reply just under the body bound is read", func(t *testing.T) {
		api := newFakeAPI(t, replyEvents(head, megabyteDelta(), 31, tail))
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		assert.Len(t, resp.Message.Text, len("Hello!")+31<<20)
	})
}

// After message_stop the little that is left of the body is read, so the
// connection goes back to the pool. A server that flushes each event sends
// the end of the body after the last event, and a client that stopped
// reading at message_stop would throw the connection away.
func TestClient_Stream_ReusesTheConnection(t *testing.T) {
	events := strings.SplitAfter(fixture(t, "stream_tool_use.sse"), "\n\n")
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range events {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
		}
		// The end of the body is its own write, a moment after the last
		// event. Without the pause the two can reach the client together,
		// and a client that never drains would pass by luck.
		time.Sleep(5 * time.Millisecond)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	c, err := anthropic.New(anthropic.Options{APIKey: testKey, BaseURL: srv.URL})
	require.NoError(t, err)
	const calls = 5
	for range calls {
		resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		require.Equal(t, llm.StopToolUse, resp.Stop)
	}
	assert.Equal(t, int64(1), opened.Load(), "%d streams, one after another, share one connection", calls)
}

// The drain has bounds of its own. A server that sends the whole reply and
// then holds the stream open costs a tenth of a second, not a hang.
func TestClient_Stream_ServerThatNeverEndsTheBody(t *testing.T) {
	whole := fixture(t, "stream_text.sse")
	api := newFakeAPI(t, func(w http.ResponseWriter, req received) {
		replyPaced(0, whole)(w, req)
		<-req.Gone
	})
	// No idle limit, so nothing but the drain's own bound can end the wait.
	c := api.client(t, anthropic.Options{IdleTimeout: -1})

	started := time.Now()
	resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
	require.NoError(t, err, "message_stop was read: the reply is whole")
	assert.Equal(t, "Hello!", resp.Message.Text)
	assert.Less(t, time.Since(started), 10*time.Second)
}

// What follows message_stop is read only a little way. A server that goes on
// sending is not listened to for as long as it cares to send.
func TestClient_Stream_ServerThatKeepsSendingAfterTheReply(t *testing.T) {
	whole := fixture(t, "stream_text.sse")
	megabyte := megabyteDelta()
	var sent atomic.Int64
	done := make(chan struct{})
	api := newFakeAPI(t, func(w http.ResponseWriter, req received) {
		defer close(done)
		replyPaced(0, whole)(w, req)
		for {
			select {
			case <-req.Gone:
				return
			default:
			}
			n, err := io.WriteString(w, megabyte)
			sent.Add(int64(n))
			if err != nil {
				return
			}
		}
	})

	resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
	require.NoError(t, err)
	assert.Equal(t, "Hello!", resp.Message.Text)

	<-done
	// What the server got out before the connection closed is what the
	// kernel would buffer, not what a client reading for the whole of the
	// drain's tenth of a second would take.
	assert.Less(t, sent.Load(), int64(64<<20))
}

func TestClient_Stream_ReplyThatIsNotAnEventStream(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		// want is what the error names.
		want string
	}{
		{name: "a JSON message", contentType: "application/json", body: okBody, want: `"application/json"`},
		{name: "a page", contentType: "text/html; charset=utf-8", body: "<html>sign in</html>", want: `"text/html; charset=utf-8"`},
		{name: "no content type at all", contentType: "", body: okStream, want: "no content type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
				// Setting the key to nil stops net/http from guessing a type.
				w.Header()["Content-Type"] = nil
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				w.Header().Set("request-id", requestID)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tt.body)
			})
			resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()},
				func(llm.Delta) error {
					t.Error("fn was called for a reply that is not a stream")
					return nil
				})

			require.Error(t, err)
			assert.Nil(t, resp)
			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, http.StatusOK, e.Status)
			assert.Equal(t, requestID, e.RequestID)
			assert.Contains(t, e.Message, tt.want)
			assert.False(t, e.Retryable)
			assert.False(t, llm.Retryable(err))
		})
	}

	t.Run("an event stream with parameters is one", func(t *testing.T) {
		api := newFakeAPI(t, func(w http.ResponseWriter, _ received) {
			w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, okStream)
		})
		resp, err := api.client(t, anthropic.Options{}).Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)
		assert.Equal(t, "ok", resp.Message.Text)
	})
}

func TestClient_Redirects(t *testing.T) {
	// redirectTo answers every request with a 307 to location, which is the
	// redirect that asks for the same method and body again.
	redirectTo := func(location func() string) reply {
		return func(w http.ResponseWriter, _ received) {
			w.Header().Set("Location", location())
			w.Header().Set("request-id", requestID)
			w.WriteHeader(http.StatusTemporaryRedirect)
		}
	}
	opts := func(hc *http.Client) anthropic.Options {
		return anthropic.Options{HTTPClient: hc, Betas: []string{"context-management-2025-06-27"}}
	}

	t.Run("the default client does not follow one", func(t *testing.T) {
		elsewhere := newFakeAPI(t, replyEither(okBody, okStream))
		api := newFakeAPI(t, redirectTo(func() string { return elsewhere.srv.URL + "/v1/messages" }))
		c := api.client(t, opts(nil))

		check := func(t *testing.T, resp *llm.Response, err error) {
			t.Helper()
			require.Error(t, err)
			assert.Nil(t, resp)
			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, http.StatusTemporaryRedirect, e.Status)
			assert.Equal(t, requestID, e.RequestID)
			assert.False(t, e.Retryable)
		}
		resp, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
		check(t, resp, err)
		resp, err = c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		check(t, resp, err)
		assert.Zero(t, elsewhere.count(), "nothing was sent to the host the redirect named")
	})

	t.Run("a caller's client that follows one takes no key and no beta to another host", func(t *testing.T) {
		elsewhere := newFakeAPI(t, replyEither(okBody, okStream))
		api := newFakeAPI(t, redirectTo(func() string { return elsewhere.srv.URL + "/v1/messages" }))
		c := api.client(t, opts(&http.Client{}))

		_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
		require.NoError(t, err, "the caller's client follows redirects, and that is the caller's policy")
		_, err = c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		require.NoError(t, err)

		assert.Equal(t, testKey, api.last(t).Header.Get("x-api-key"), "the configured host is sent the key")
		require.Equal(t, 2, elsewhere.count())
		elsewhere.mu.Lock()
		defer elsewhere.mu.Unlock()
		for _, got := range elsewhere.requests {
			for _, name := range []string{"x-api-key", "anthropic-beta", "anthropic-version", "authorization"} {
				assert.Empty(t, got.Header.Values(name), "%s went to another host", name)
			}
		}
	})

	t.Run("a redirect within the configured host keeps them", func(t *testing.T) {
		var api *fakeAPI
		api = newFakeAPI(t, func(w http.ResponseWriter, req received) {
			if req.Path == "/v1/messages" {
				redirectTo(func() string { return api.srv.URL + "/moved/v1/messages" })(w, req)
				return
			}
			replyJSON(okBody)(w, req)
		})
		c := api.client(t, opts(&http.Client{}))

		_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
		require.NoError(t, err)
		got := api.last(t)
		assert.Equal(t, "/moved/v1/messages", got.Path)
		assert.Equal(t, testKey, got.Header.Get("x-api-key"))
		assert.Equal(t, "context-management-2025-06-27", got.Header.Get("anthropic-beta"))
		assert.Equal(t, "2023-06-01", got.Header.Get("anthropic-version"))
	})

	t.Run("the same host over another scheme is another place", func(t *testing.T) {
		// The configured address is https. A redirect to http on the same
		// host would put the key on the wire in the clear.
		var plain []http.Header
		transport := roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Scheme == "https" {
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     http.Header{"Location": {"http://api.keel.test/v1/messages"}},
					Body:       http.NoBody,
					Request:    r,
				}, nil
			}
			plain = append(plain, r.Header.Clone())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(okBody)),
				Request:    r,
			}, nil
		})
		c, err := anthropic.New(anthropic.Options{
			APIKey:     testKey,
			BaseURL:    "https://api.keel.test",
			HTTPClient: &http.Client{Transport: transport},
		})
		require.NoError(t, err)

		_, err = c.Generate(context.Background(), llm.Request{Messages: hello()})
		require.NoError(t, err)
		require.Len(t, plain, 1)
		assert.Empty(t, plain[0].Values("x-api-key"))
	})

	t.Run("another origin is a change of scheme, host or port, default ports counted", func(t *testing.T) {
		tests := []struct {
			name     string
			base     string
			location string
			// same says the redirect stays on the configured origin.
			same bool
		}{
			{name: "the default port written out", base: "https://api.keel.test", location: "https://api.keel.test:443/v1/messages", same: true},
			{name: "the default port left out", base: "https://api.keel.test:443", location: "https://api.keel.test/v1/messages", same: true},
			{name: "the host in another case", base: "https://api.keel.test", location: "https://API.keel.test/v1/messages", same: true},
			{name: "another port", base: "https://api.keel.test", location: "https://api.keel.test:8443/v1/messages"},
			{name: "another port, on a base with one", base: "http://api.keel.test:8081", location: "http://api.keel.test:8082/v1/messages"},
			{name: "another host", base: "https://api.keel.test", location: "https://other.keel.test/v1/messages"},
			{name: "another scheme on its own default port", base: "https://api.keel.test", location: "http://api.keel.test/v1/messages"},
			{name: "another scheme on the same port", base: "https://api.keel.test", location: "http://api.keel.test:443/v1/messages"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var followed []http.Header
				first := true
				transport := roundTrip(func(r *http.Request) (*http.Response, error) {
					if first {
						first = false
						return &http.Response{
							StatusCode: http.StatusTemporaryRedirect,
							Header:     http.Header{"Location": {tt.location}},
							Body:       http.NoBody,
							Request:    r,
						}, nil
					}
					followed = append(followed, r.Header.Clone())
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(okBody)),
						Request:    r,
					}, nil
				})
				c, err := anthropic.New(anthropic.Options{
					APIKey:     testKey,
					BaseURL:    tt.base,
					Betas:      []string{"context-management-2025-06-27"},
					HTTPClient: &http.Client{Transport: transport},
				})
				require.NoError(t, err)

				_, err = c.Generate(context.Background(), llm.Request{Messages: hello()})
				require.NoError(t, err)
				require.Len(t, followed, 1)
				if tt.same {
					assert.Equal(t, testKey, followed[0].Get("x-api-key"))
					assert.Equal(t, "context-management-2025-06-27", followed[0].Get("anthropic-beta"))
					return
				}
				assert.Empty(t, followed[0].Values("x-api-key"))
				assert.Empty(t, followed[0].Values("anthropic-beta"))
				assert.Empty(t, followed[0].Values("anthropic-version"))
			})
		}
	})

	t.Run("the origin that counts is the configured one, not the hop before", func(t *testing.T) {
		// The first redirect leaves the configured host and the second stays
		// on the host it left for. net/http copies the first request's
		// headers onto every hop, so a guard that compared each hop with the
		// one before would hand the key over on the second.
		var elsewhere []http.Header
		transport := roundTrip(func(r *http.Request) (*http.Response, error) {
			redirect := func(location string) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTemporaryRedirect,
					Header:     http.Header{"Location": {location}},
					Body:       http.NoBody,
					Request:    r,
				}, nil
			}
			switch {
			case r.URL.Host == "api.keel.test":
				return redirect("https://other.keel.test/first")
			case r.URL.Path == "/first":
				elsewhere = append(elsewhere, r.Header.Clone())
				return redirect("https://other.keel.test/second")
			}
			elsewhere = append(elsewhere, r.Header.Clone())
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(okBody)),
				Request:    r,
			}, nil
		})
		c, err := anthropic.New(anthropic.Options{
			APIKey:     testKey,
			BaseURL:    "https://api.keel.test",
			HTTPClient: &http.Client{Transport: transport},
		})
		require.NoError(t, err)

		_, err = c.Generate(context.Background(), llm.Request{Messages: hello()})
		require.NoError(t, err)
		require.Len(t, elsewhere, 2)
		for i, header := range elsewhere {
			assert.Empty(t, header.Values("x-api-key"), "hop %d on the other host was sent the key", i+1)
		}
	})

	t.Run("a redirect the caller's policy refuses is not retryable", func(t *testing.T) {
		elsewhere := newFakeAPI(t, replyJSON(okBody))
		api := newFakeAPI(t, redirectTo(func() string { return elsewhere.srv.URL + "/v1/messages" }))
		refusal := errors.New("this service follows no redirect")
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return refusal }}
		c := api.client(t, opts(hc))

		check := func(t *testing.T, err error) {
			t.Helper()
			require.ErrorIs(t, err, refusal)
			var e *llm.Error
			require.ErrorAs(t, err, &e)
			assert.False(t, e.Retryable, "the policy would refuse it again")
			assert.False(t, llm.Retryable(err))
			assert.Equal(t, http.StatusTemporaryRedirect, e.Status, "the 3xx is the server's answer")
			assert.Equal(t, requestID, e.RequestID)
			assert.Contains(t, e.Message, "redirect")
		}
		_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
		check(t, err)
		_, err = c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		check(t, err)
		assert.Zero(t, elsewhere.count())
	})

	t.Run("the caller's own redirect policy is asked", func(t *testing.T) {
		elsewhere := newFakeAPI(t, replyJSON(okBody))
		api := newFakeAPI(t, redirectTo(func() string { return elsewhere.srv.URL + "/v1/messages" }))
		asked := 0
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			asked++
			return http.ErrUseLastResponse
		}}
		c := api.client(t, opts(hc))

		_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
		var e *llm.Error
		require.ErrorAs(t, err, &e)
		assert.Equal(t, http.StatusTemporaryRedirect, e.Status)
		assert.Equal(t, 1, asked)
		assert.Zero(t, elsewhere.count())
	})

	t.Run("the caller's client is left as it was", func(t *testing.T) {
		hc := &http.Client{}
		_, err := anthropic.New(anthropic.Options{APIKey: testKey, HTTPClient: hc})
		require.NoError(t, err)
		assert.Nil(t, hc.CheckRedirect)
	})

	t.Run("a caller's client with no policy still stops after ten", func(t *testing.T) {
		var api *fakeAPI
		api = newFakeAPI(t, redirectTo(func() string { return api.srv.URL + "/v1/messages" }))
		c := api.client(t, opts(&http.Client{}))

		_, err := c.Generate(context.Background(), llm.Request{Messages: hello()})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "10 redirects")
		assert.Equal(t, 10, api.count())
		var e *llm.Error
		require.ErrorAs(t, err, &e)
		assert.False(t, e.Retryable, "the eleventh would be refused again")
		assert.Equal(t, http.StatusTemporaryRedirect, e.Status)
	})
}

// roundTrip is a transport that answers from a function, with no network.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// settledGoroutines returns the number of goroutines once it has stopped
// falling, since a connection's goroutines take a moment to end after it is
// closed.
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for range 200 {
		time.Sleep(5 * time.Millisecond)
		next := runtime.NumGoroutine()
		if next >= n {
			return n
		}
		n = next
	}
	return n
}

// Stream starts nothing that is still running when it returns, however it
// returns: no watch on the stream, no timer, no reader.
func TestClient_Stream_LeavesNothingRunning(t *testing.T) {
	whole := fixture(t, "stream_tool_use.sse")
	head, tail := textStreamInTwo(t)
	fnErr := errors.New("the consumer has gone")

	tests := []struct {
		name   string
		answer reply
		opts   anthropic.Options
		// run makes the call and says whether it ended as the case means it to.
		run func(t *testing.T, c *anthropic.Client)
	}{
		{
			name:   "a clean end",
			answer: replyStream(whole),
			run: func(t *testing.T, c *anthropic.Client) {
				_, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
				require.NoError(t, err)
			},
		},
		{
			name:   "an error event",
			answer: replyStream(head + "event: error\ndata: {\"type\": \"error\", \"error\": {\"type\": \"overloaded_error\", \"message\": \"Overloaded\"}}\n\n"),
			run: func(t *testing.T, c *anthropic.Client) {
				_, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
				var e *llm.Error
				require.ErrorAs(t, err, &e)
				require.Equal(t, "overloaded_error", e.Type)
			},
		},
		{
			name:   "a status that is not 2xx",
			answer: func(w http.ResponseWriter, _ received) { w.WriteHeader(http.StatusServiceUnavailable) },
			run: func(t *testing.T, c *anthropic.Client) {
				_, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
				var e *llm.Error
				require.ErrorAs(t, err, &e)
				require.Equal(t, http.StatusServiceUnavailable, e.Status)
			},
		},
		{
			name:   "an error from fn",
			answer: replyStream(whole),
			run: func(t *testing.T, c *anthropic.Client) {
				_, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, func(llm.Delta) error { return fnErr })
				require.ErrorIs(t, err, fnErr)
			},
		},
		{
			name: "the caller's cancellation",
			answer: func(w http.ResponseWriter, req received) {
				replyPaced(0, head)(w, req)
				<-req.Gone
			},
			run: func(t *testing.T, c *anthropic.Client) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				_, err := c.Stream(ctx, llm.Request{Messages: hello()}, func(llm.Delta) error {
					cancel()
					return nil
				})
				require.ErrorIs(t, err, context.Canceled)
			},
		},
		{
			name: "the idle limit",
			answer: func(w http.ResponseWriter, req received) {
				replyPaced(0, head)(w, req)
				<-req.Gone
			},
			opts: anthropic.Options{IdleTimeout: 50 * time.Millisecond},
			run: func(t *testing.T, c *anthropic.Client) {
				_, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
				var e *llm.Error
				require.ErrorAs(t, err, &e)
				require.True(t, e.Retryable)
			},
		},
		{
			name: "a server that never ends the body",
			answer: func(w http.ResponseWriter, req received) {
				replyPaced(0, head+tail)(w, req)
				<-req.Gone
			},
			run: func(t *testing.T, c *anthropic.Client) {
				_, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
				require.NoError(t, err)
			},
		},
		{
			name: "a stream cut short",
			answer: func(w http.ResponseWriter, req received) {
				replyPaced(0, head)(w, req)
			},
			run: func(t *testing.T, c *anthropic.Client) {
				_, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t, tt.answer)
			// A transport of its own, with no connection kept, so that what
			// is counted is this package's and not net/http's idle pool.
			transport := &http.Transport{DisableKeepAlives: true}
			tt.opts.HTTPClient = &http.Client{Transport: transport}
			c := api.client(t, tt.opts)

			before := settledGoroutines()
			for range 5 {
				tt.run(t, c)
			}
			transport.CloseIdleConnections()
			after := settledGoroutines()
			assert.LessOrEqual(t, after, before, "goroutines left running after Stream returned")
		})
	}
}

// The last event and the idle limit arrive together. Whichever wins, the
// call ends one of two ways and never a third: the whole reply, or a stall
// that is retryable and blames no context.
func TestClient_Stream_IdleLimitRacesTheLastEvent(t *testing.T) {
	head, tail := textStreamInTwo(t)
	const limit = 20 * time.Millisecond

	var whole, stalled int
	for i := range 60 {
		// The gap before the last events sweeps across the limit.
		gap := limit - 3*time.Millisecond + time.Duration(i%7)*time.Millisecond
		api := newFakeAPI(t, replyPaced(gap, head, tail))
		c := api.client(t, anthropic.Options{IdleTimeout: limit})

		resp, err := c.Stream(context.Background(), llm.Request{Messages: hello()}, noDeltas)
		if err == nil {
			whole++
			require.NotNil(t, resp)
			require.Equal(t, "Hello!", resp.Message.Text)
			require.Equal(t, llm.StopEnd, resp.Stop)
			continue
		}
		stalled++
		require.Nil(t, resp)
		var e *llm.Error
		require.ErrorAs(t, err, &e, "run %d: %v", i, err)
		require.True(t, e.Retryable, "run %d: %v", i, err)
		require.True(t, llm.Retryable(err), "run %d: %v", i, err)
		require.NotErrorIs(t, err, context.Canceled, "run %d", i)
		require.NotErrorIs(t, err, context.DeadlineExceeded, "run %d", i)
		require.Contains(t, e.Err.Error(), "sent nothing for", "run %d", i)
	}
	t.Logf("%d whole, %d stalled", whole, stalled)
}
