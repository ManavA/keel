package httpx_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/log"
	"github.com/ManavA/keel/metrics"
	"github.com/ManavA/keel/perf"
)

// plainWriter is a ResponseWriter with no Flush and no Unwrap: nothing a
// stream could use. It remembers whether anything reached it.
type plainWriter struct {
	header      http.Header
	wroteHeader bool
	body        bytes.Buffer
}

func newPlainWriter() *plainWriter { return &plainWriter{header: http.Header{}} }

func (w *plainWriter) Header() http.Header { return w.header }
func (w *plainWriter) WriteHeader(int)     { w.wroteHeader = true }
func (w *plainWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.body.Write(b)
}

// unwrapper is what a middleware's wrapper is when it passes nothing through
// but Unwrap: no Flush of its own.
type unwrapper struct{ http.ResponseWriter }

func (u unwrapper) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// flushErrorWriter can flush only through FlushError, the form
// http.ResponseController looks for first.
type flushErrorWriter struct {
	http.ResponseWriter
	flushes int
}

func (w *flushErrorWriter) FlushError() error {
	w.flushes++
	return nil
}

// spyWriter records what a stream does to its response: the headers as they
// stood when the status was written, and how much body each flush pushed out.
type spyWriter struct {
	*httptest.ResponseRecorder
	statuses       []int
	headerAtStatus http.Header
	flushedAt      []int
}

func newSpyWriter() *spyWriter { return &spyWriter{ResponseRecorder: httptest.NewRecorder()} }

func (w *spyWriter) WriteHeader(code int) {
	if w.headerAtStatus == nil {
		w.headerAtStatus = w.Header().Clone()
	}
	w.statuses = append(w.statuses, code)
	w.ResponseRecorder.WriteHeader(code)
}

func (w *spyWriter) Flush() {
	w.flushedAt = append(w.flushedAt, w.Body.Len())
	w.ResponseRecorder.Flush()
}

// deadlineWriter records the write deadlines asked of it.
type deadlineWriter struct {
	*spyWriter
	deadlines []time.Time
	errs      []error // the result of the nth call; nil once they run out
}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	n := len(w.deadlines)
	w.deadlines = append(w.deadlines, t)
	if n < len(w.errs) {
		return w.errs[n]
	}
	return nil
}

// breakableWriter fails its writes or its flushes on demand, and counts the
// writes it was asked for.
type breakableWriter struct {
	*spyWriter
	writeErr, flushErr error
	writes             int
}

func (w *breakableWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.spyWriter.Write(b)
}

func (w *breakableWriter) FlushError() error {
	if w.flushErr != nil {
		return w.flushErr
	}
	w.Flush()
	return nil
}

// syncBuffer is a buffer the server's goroutines and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func getRequest() *http.Request { return httptest.NewRequest(http.MethodGet, "/stream", nil) }

func newStream(t *testing.T, w http.ResponseWriter) *httpx.EventStream {
	t.Helper()
	s, err := httpx.NewEventStream(w, getRequest(), httpx.EventStreamOptions{})
	require.NoError(t, err)
	return s
}

func TestEventStreamWritesTheFramesOfEachEvent(t *testing.T) {
	send := func(ev httpx.ServerEvent) func(*httpx.EventStream) error {
		return func(s *httpx.EventStream) error { return s.Send(ev) }
	}
	tests := []struct {
		name  string
		retry time.Duration
		send  func(*httpx.EventStream) error
		want  string
	}{
		{
			name: "data only",
			send: send(httpx.ServerEvent{Data: []byte("hello")}),
			want: "data: hello\n\n",
		},
		{
			name: "with a type",
			send: send(httpx.ServerEvent{Type: "tick", Data: []byte("hello")}),
			want: "event: tick\ndata: hello\n\n",
		},
		{
			name: "with an id",
			send: send(httpx.ServerEvent{ID: "7", Data: []byte("hello")}),
			want: "id: 7\ndata: hello\n\n",
		},
		{
			name: "with an id and a type",
			send: send(httpx.ServerEvent{ID: "7", Type: "tick", Data: []byte("hello")}),
			want: "id: 7\nevent: tick\ndata: hello\n\n",
		},
		{
			name: "data of three lines",
			send: send(httpx.ServerEvent{Data: []byte("a\nb\nc")}),
			want: "data: a\ndata: b\ndata: c\n\n",
		},
		{
			name: "data whose lines end in CRLF or a bare CR",
			send: send(httpx.ServerEvent{Data: []byte("a\r\nb\rc")}),
			want: "data: a\ndata: b\ndata: c\n\n",
		},
		{
			name: "data ending in a line break",
			send: send(httpx.ServerEvent{Data: []byte("a\n")}),
			want: "data: a\ndata:\n\n",
		},
		{
			name: "data that reads like a field stays data",
			send: send(httpx.ServerEvent{Data: []byte("x\nevent: boom\r\nid: 9")}),
			want: "data: x\ndata: event: boom\ndata: id: 9\n\n",
		},
		{
			name: "empty data, which a client still dispatches",
			send: send(httpx.ServerEvent{}),
			want: "data:\n\n",
		},
		{
			name: "empty data with a type",
			send: send(httpx.ServerEvent{Type: "end"}),
			want: "event: end\ndata:\n\n",
		},
		{
			name: "SendJSON",
			send: func(s *httpx.EventStream) error { return s.SendJSON("3", "update", map[string]int{"n": 1}) },
			want: "id: 3\nevent: update\ndata: {\"n\":1}\n\n",
		},
		{
			name: "SendJSON with no id and no type",
			send: func(s *httpx.EventStream) error { return s.SendJSON("", "", []string{"a", "b"}) },
			want: "data: [\"a\",\"b\"]\n\n",
		},
		{
			name: "SendJSON of a string holding a line break stays on one data line",
			send: func(s *httpx.EventStream) error { return s.SendJSON("", "", "a\nb") },
			want: "data: \"a\\nb\"\n\n",
		},
		{
			name: "a comment",
			send: func(s *httpx.EventStream) error { return s.Comment("keep-alive") },
			want: ": keep-alive\n\n",
		},
		{
			name: "an empty comment",
			send: func(s *httpx.EventStream) error { return s.Comment("") },
			want: ":\n\n",
		},
		{
			name:  "retry set",
			retry: 3 * time.Second,
			send:  send(httpx.ServerEvent{Data: []byte("hello")}),
			want:  "retry: 3000\n\ndata: hello\n\n",
		},
		{
			name:  "retry under a millisecond is not sent as an immediate reconnect",
			retry: 500 * time.Microsecond,
			send:  send(httpx.ServerEvent{Data: []byte("hello")}),
			want:  "retry: 1\n\ndata: hello\n\n",
		},
		{
			name:  "a negative retry sends none",
			retry: -time.Second,
			send:  send(httpx.ServerEvent{Data: []byte("hello")}),
			want:  "data: hello\n\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s, err := httpx.NewEventStream(rec, getRequest(), httpx.EventStreamOptions{Retry: tc.retry})
			require.NoError(t, err)

			require.NoError(t, tc.send(s))

			assert.Equal(t, tc.want, rec.Body.String())
		})
	}
}

func TestEventStreamsAreFramedAsOneWritePerEvent(t *testing.T) {
	// A write that failed halfway through an event would leave the client with
	// the start of one, so an event goes out in a single Write.
	counter := &writeCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	s := newStream(t, counter)

	require.NoError(t, s.Send(httpx.ServerEvent{ID: "1", Type: "t", Data: []byte("a\nb\nc")}))
	require.NoError(t, s.Comment("x"))

	assert.Equal(t, 2, counter.writes)
}

type writeCountingRecorder struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *writeCountingRecorder) Write(b []byte) (int, error) {
	w.writes++
	return w.ResponseRecorder.Write(b)
}

func TestNewEventStreamSetsTheStreamHeadersAndStatus(t *testing.T) {
	spy := newSpyWriter()

	_, err := httpx.NewEventStream(spy, getRequest(), httpx.EventStreamOptions{})
	require.NoError(t, err)

	assert.Equal(t, []int{http.StatusOK}, spy.statuses)
	assert.Equal(t, http.StatusOK, spy.Code)
	// Read from the copy taken as the status was written: a header set after
	// that point never reaches the client.
	assert.Equal(t, "text/event-stream", spy.headerAtStatus.Get("Content-Type"))
	assert.Equal(t, "no-cache", spy.headerAtStatus.Get("Cache-Control"))
	assert.Equal(t, "no", spy.headerAtStatus.Get("X-Accel-Buffering"))
	assert.Empty(t, spy.Body.String())
}

func TestNewEventStreamFlushesBeforeTheFirstEvent(t *testing.T) {
	tests := []struct {
		name          string
		retry         time.Duration
		wantFlushedAt []int
	}{
		{name: "nothing to write", wantFlushedAt: []int{0}},
		{name: "after the retry line", retry: 2 * time.Second, wantFlushedAt: []int{len("retry: 2000\n\n")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := newSpyWriter()

			_, err := httpx.NewEventStream(spy, getRequest(), httpx.EventStreamOptions{Retry: tc.retry})
			require.NoError(t, err)

			assert.Equal(t, tc.wantFlushedAt, spy.flushedAt)
		})
	}
}

func TestEventStreamFlushesAfterWritingEachEvent(t *testing.T) {
	spy := newSpyWriter()
	s := newStream(t, spy)

	require.NoError(t, s.Send(httpx.ServerEvent{Data: []byte("a")}))
	require.NoError(t, s.SendJSON("", "", 1))
	require.NoError(t, s.Comment("c"))

	first := len("data: a\n\n")
	second := first + len("data: 1\n\n")
	third := second + len(": c\n\n")
	assert.Equal(t, []int{0, first, second, third}, spy.flushedAt,
		"each flush must come after the bytes it is meant to push out")
}

func TestEventStreamRefusesAnEventItCannotFrameAndWritesNothing(t *testing.T) {
	tests := []struct {
		name string
		send func(*httpx.EventStream) error
	}{
		{"an id with a line feed", func(s *httpx.EventStream) error {
			return s.Send(httpx.ServerEvent{ID: "1\nevent: x", Data: []byte("a")})
		}},
		{"an id with a carriage return", func(s *httpx.EventStream) error {
			return s.Send(httpx.ServerEvent{ID: "1\rx", Data: []byte("a")})
		}},
		{"an id with a NUL, which a browser would drop along with the resume point", func(s *httpx.EventStream) error {
			return s.Send(httpx.ServerEvent{ID: "1\x002", Data: []byte("a")})
		}},
		{"a type with a line feed", func(s *httpx.EventStream) error {
			return s.Send(httpx.ServerEvent{Type: "a\nb", Data: []byte("a")})
		}},
		{"a type with CRLF", func(s *httpx.EventStream) error {
			return s.Send(httpx.ServerEvent{Type: "a\r\nb", Data: []byte("a")})
		}},
		{"SendJSON with a line feed in the id", func(s *httpx.EventStream) error {
			return s.SendJSON("1\n2", "t", 1)
		}},
		{"SendJSON with a NUL in the id", func(s *httpx.EventStream) error {
			return s.SendJSON("1\x002", "t", 1)
		}},
		{"SendJSON with a line feed in the type", func(s *httpx.EventStream) error {
			return s.SendJSON("1", "t\nx", 1)
		}},
		{"a comment with a line feed", func(s *httpx.EventStream) error {
			return s.Comment("a\nb")
		}},
		{"SendJSON of a value that cannot be encoded", func(s *httpx.EventStream) error {
			return s.SendJSON("1", "t", make(chan int))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := newSpyWriter()
			s := newStream(t, spy)

			require.Error(t, tc.send(s))

			assert.Empty(t, spy.Body.String(), "a refused event must write nothing")
			assert.Equal(t, []int{0}, spy.flushedAt, "nor flush anything but the opening")

			// Nothing was written, so the stream is still in step.
			require.NoError(t, s.Send(httpx.ServerEvent{Data: []byte("ok")}))
			assert.Equal(t, "data: ok\n\n", spy.Body.String())
		})
	}
}

func TestNewEventStreamRefusesAWriterThatCannotFlush(t *testing.T) {
	tests := []struct {
		name string
		wrap func(*plainWriter) http.ResponseWriter
	}{
		{"no Flush and no Unwrap", func(w *plainWriter) http.ResponseWriter { return w }},
		{"a wrapper over a writer that cannot flush", func(w *plainWriter) http.ResponseWriter {
			return unwrapper{w}
		}},
		{"two wrappers over a writer that cannot flush", func(w *plainWriter) http.ResponseWriter {
			return unwrapper{unwrapper{w}}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := newPlainWriter()

			s, err := httpx.NewEventStream(tc.wrap(base), getRequest(), httpx.EventStreamOptions{Retry: time.Second})

			require.ErrorIs(t, err, httpx.ErrStreamUnsupported)
			assert.Nil(t, s)
			assert.False(t, base.wroteHeader, "no status may be written, so the handler can still answer")
			assert.Empty(t, base.header, "nor a header set")
			assert.Zero(t, base.body.Len())
		})
	}
}

func TestNewEventStreamFindsAFlusherThroughWrappers(t *testing.T) {
	spy := newSpyWriter()
	errWriter := &flushErrorWriter{ResponseWriter: newPlainWriter()}
	tests := []struct {
		name    string
		w       http.ResponseWriter
		flushes func() int
	}{
		{"the writer itself", spy, func() int { return len(spy.flushedAt) }},
		{"a wrapper over a flusher", unwrapper{spy}, func() int { return len(spy.flushedAt) }},
		{"two wrappers over a flusher", unwrapper{unwrapper{spy}}, func() int { return len(spy.flushedAt) }},
		{"a writer that flushes through FlushError", errWriter, func() int { return errWriter.flushes }},
		{"a wrapper over FlushError", unwrapper{errWriter}, func() int { return errWriter.flushes }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy.flushedAt, errWriter.flushes = nil, 0

			s, err := httpx.NewEventStream(tc.w, getRequest(), httpx.EventStreamOptions{})
			require.NoError(t, err)
			require.NoError(t, s.Send(httpx.ServerEvent{Data: []byte("a")}))

			assert.Equal(t, 2, tc.flushes(), "the opening and the event each flush")
		})
	}
}

func TestNewEventStreamLiftsTheWriteDeadline(t *testing.T) {
	boom := errors.New("connection is gone")
	tests := []struct {
		name         string
		err          error
		wrap         bool
		wantErr      error
		wantStarted  bool
		wantDeadline bool
	}{
		{name: "supported", wantStarted: true, wantDeadline: true},
		{name: "supported through a wrapper", wrap: true, wantStarted: true, wantDeadline: true},
		{name: "not supported by the writer", err: http.ErrNotSupported, wantStarted: true, wantDeadline: true},
		{
			name:        "not supported, wrapped",
			err:         fmt.Errorf("server: %w", http.ErrNotSupported),
			wantStarted: true, wantDeadline: true,
		},
		{name: "any other failure is returned, before a header is written", err: boom, wantErr: boom, wantDeadline: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dw := &deadlineWriter{spyWriter: newSpyWriter(), errs: []error{tc.err}}
			var w http.ResponseWriter = dw
			if tc.wrap {
				w = unwrapper{dw}
			}

			s, err := httpx.NewEventStream(w, getRequest(), httpx.EventStreamOptions{})

			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantStarted, s != nil)
			if tc.wantDeadline {
				require.Len(t, dw.deadlines, 1)
				assert.True(t, dw.deadlines[0].IsZero(), "the deadline is lifted, not moved")
			}
			if !tc.wantStarted {
				assert.Empty(t, dw.statuses, "a refused stream writes no status")
				assert.Empty(t, dw.Header(), "and sets no header")
			}
		})
	}
}

func TestEventStreamBoundsEachSend(t *testing.T) {
	tests := []struct {
		name      string
		limit     time.Duration
		errs      []error
		wantLimit time.Duration // zero: no deadline is set around a send
	}{
		{name: "the default is thirty seconds", wantLimit: 30 * time.Second},
		{name: "a limit set", limit: 5 * time.Second, wantLimit: 5 * time.Second},
		{name: "a negative limit sets none", limit: -time.Second},
		{name: "a writer that does not support deadlines is not asked again", errs: []error{http.ErrNotSupported}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dw := &deadlineWriter{spyWriter: newSpyWriter(), errs: tc.errs}
			s, err := httpx.NewEventStream(dw, getRequest(), httpx.EventStreamOptions{SendTimeout: tc.limit})
			require.NoError(t, err)

			before := time.Now()
			require.NoError(t, s.Send(httpx.ServerEvent{Data: []byte("a")}))
			require.NoError(t, s.Comment("c"))
			after := time.Now()

			if tc.wantLimit == 0 {
				assert.Len(t, dw.deadlines, 1, "only the deadline lifted at the start")
				return
			}
			// Lifted at the start, then for each send: set, and lifted again so
			// that the time between sends does not count against the next one.
			require.Len(t, dw.deadlines, 5)
			for _, i := range []int{0, 2, 4} {
				assert.True(t, dw.deadlines[i].IsZero(), "deadline %d should be lifted", i)
			}
			for _, i := range []int{1, 3} {
				assert.WithinRange(t, dw.deadlines[i], before.Add(tc.wantLimit), after.Add(tc.wantLimit))
			}
		})
	}
}

func TestASendWhoseDeadlineCannotBeSetFailsAndEndsTheStream(t *testing.T) {
	boom := errors.New("connection is gone")
	dw := &deadlineWriter{spyWriter: newSpyWriter(), errs: []error{nil, boom}}
	s := newStream(t, dw)

	require.ErrorIs(t, s.Send(httpx.ServerEvent{Data: []byte("a")}), boom)
	assert.Empty(t, dw.Body.String(), "nothing is written under a deadline that was not set")
	asked := len(dw.deadlines)

	require.ErrorIs(t, s.Comment("c"), boom)
	assert.Len(t, dw.deadlines, asked, "the stream is over, so nothing asks again")
}

func TestEventStreamIsUnusableAfterAFailedSend(t *testing.T) {
	// A write that failed partway leaves half an event on the wire, so no later
	// event could be framed after it.
	boom := errors.New("broken pipe")
	tests := []struct {
		name string
		fail func(*breakableWriter)
	}{
		{"a write that fails", func(w *breakableWriter) { w.writeErr = boom }},
		{"a flush that fails", func(w *breakableWriter) { w.flushErr = boom }},
	}
	later := map[string]func(*httpx.EventStream) error{
		"Send":     func(s *httpx.EventStream) error { return s.Send(httpx.ServerEvent{Data: []byte("b")}) },
		"SendJSON": func(s *httpx.EventStream) error { return s.SendJSON("", "", 2) },
		"Comment":  func(s *httpx.EventStream) error { return s.Comment("c") },
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bw := &breakableWriter{spyWriter: newSpyWriter()}
			s := newStream(t, bw)
			tc.fail(bw)

			require.ErrorIs(t, s.Send(httpx.ServerEvent{Data: []byte("a")}), boom)
			writes, written := bw.writes, bw.Body.Len()

			for name, send := range later {
				assert.ErrorIs(t, send(s), boom, name)
			}
			assert.Equal(t, writes, bw.writes, "a broken stream is not written to again")
			assert.Equal(t, written, bw.Body.Len())
		})
	}
}

func TestEventStreamStopsOnceTheRequestIsDone(t *testing.T) {
	tests := []struct {
		name string
		send func(*httpx.EventStream) error
	}{
		{"Send", func(s *httpx.EventStream) error { return s.Send(httpx.ServerEvent{Data: []byte("a")}) }},
		{"SendJSON", func(s *httpx.EventStream) error { return s.SendJSON("", "", 1) }},
		{"Comment", func(s *httpx.EventStream) error { return s.Comment("a") }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			spy := newSpyWriter()
			s, err := httpx.NewEventStream(spy, getRequest().WithContext(ctx), httpx.EventStreamOptions{})
			require.NoError(t, err)
			require.NoError(t, tc.send(s), "the stream works while its request does")
			written, flushed := spy.Body.Len(), len(spy.flushedAt)

			cancel()

			require.ErrorIs(t, tc.send(s), context.Canceled)
			assert.Equal(t, written, spy.Body.Len())
			assert.Len(t, spy.flushedAt, flushed)
		})
	}
}

func TestLastEventID(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   string
	}{
		{name: "sent by a reconnecting client", header: http.Header{"Last-Event-Id": {"42"}}, want: "42"},
		{name: "an id that is not a number", header: http.Header{"Last-Event-Id": {"run-9/step-2"}}, want: "run-9/step-2"},
		{name: "no header", want: ""},
		{name: "an empty header", header: http.Header{"Last-Event-Id": {""}}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := getRequest()
			for k, v := range tc.header {
				req.Header[k] = v
			}

			assert.Equal(t, tc.want, httpx.LastEventID(req))
		})
	}
}

// What follows runs a stream against a real httpx.Server through httpx.NewRouter.
// The deadlines and the connection are the real ones, which a recorder cannot
// stand in for.

// serve starts handler on an httpx.Server whose write timeout is writeTimeout
// (zero takes the default) and returns its base URL. The server stops with the
// test, and a handler still running then fails it.
func serve(t *testing.T, writeTimeout time.Duration, handler http.Handler) string {
	t.Helper()
	return serveLogging(t, writeTimeout, handler, log.New(log.Options{Output: io.Discard}))
}

// serveLogging is serve with the server's logger given, which is also where
// net/http reports what it dislikes: through ErrorLog, at warning level.
func serveLogging(t *testing.T, writeTimeout time.Duration, handler http.Handler, logger *slog.Logger) string {
	t.Helper()
	srv := httpx.NewServer(httpx.ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         handler,
		WriteTimeout:    writeTimeout,
		ShutdownTimeout: 2 * time.Second,
		Logger:          logger,
	})
	require.NoError(t, srv.Listen())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(t.Context()) }()
	t.Cleanup(func() {
		assert.NoError(t, <-done, "a handler was still running when the test ended")
	})
	return "http://" + srv.Addr()
}

// streamContext bounds a client, so a stream that never delivers fails a test
// instead of hanging it.
func streamContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dial sends a GET and returns the response once its headers arrive.
func dial(ctx context.Context, t *testing.T, url string, header http.Header) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header[k] = v
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, nil
}

func openStream(ctx context.Context, t *testing.T, url string, header http.Header) (*http.Response, *bufio.Reader) {
	t.Helper()
	resp, err := dial(ctx, t, url, header)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp, bufio.NewReader(resp.Body)
}

// nextEvent reads one event, up to and including its blank line. It returns
// io.EOF when the stream ended between events and io.ErrUnexpectedEOF when it
// ended inside one.
func nextEvent(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		sb.WriteString(line)
		if err != nil {
			if errors.Is(err, io.EOF) && sb.Len() > 0 {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
		if line == "\n" {
			return sb.String(), nil
		}
	}
}

func readEvent(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	ev, err := nextEvent(br)
	require.NoError(t, err, "the stream did not deliver the event")
	return ev
}

// collectEvents reads a stream to its end. The error is nil for a stream that
// ended cleanly and says how it was cut otherwise.
func collectEvents(ctx context.Context, t *testing.T, url string) ([]string, error) {
	t.Helper()
	resp, err := dial(ctx, t, url, nil)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(resp.Body)
	var events []string
	for {
		ev, err := nextEvent(br)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

func TestEventStreamDeliversEachEventAsItIsSentThroughTheRouter(t *testing.T) {
	const count = 3
	tests := []struct {
		name     string
		opts     httpx.RouterOptions
		mount    func(*chi.Mux)
		wantGzip bool
	}{
		{name: "the default middleware"},
		{
			name: "every optional middleware the router has",
			opts: httpx.RouterOptions{
				CORS:          &middleware.CORSOptions{AllowedOrigins: []string{"*"}},
				RateLimit:     &middleware.RateLimitOptions{Requests: 1000, Window: time.Minute},
				Metrics:       metrics.New(metrics.Instruments{}),
				CompressLevel: 5,
			},
		},
		{
			// chi's compressor leaves event streams alone unless told otherwise;
			// told to compress them, its gzip writer is a buffer a flush must
			// empty.
			name:     "a compressor that does compress event streams",
			mount:    func(r *chi.Mux) { r.Use(chimw.Compress(5, "text/event-stream")) },
			wantGzip: true,
		},
		{
			// perf is a level above httpx, so only this test reaches up to it:
			// its Gzip is the wrapper a service mounts over a stream.
			name:  "perf.Gzip",
			mount: func(r *chi.Mux) { r.Use(perf.Gzip) },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The handler sends nothing until the client says it holds what
			// came before, so a stream that keeps events back stalls here.
			ack := make(chan struct{}, count+1)
			router := newTestRouter(t, tc.opts)
			if tc.mount != nil {
				tc.mount(router)
			}
			router.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
				s, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{})
				if !assert.NoError(t, err) {
					return
				}
				for i := 1; i <= count; i++ {
					select {
					case <-ack:
					case <-r.Context().Done():
						return
					}
					assert.NoError(t, s.SendJSON(strconv.Itoa(i), "tick", i))
				}
			})
			base := serve(t, 0, router)

			resp, br := openStream(streamContext(t), t, base+"/stream", nil)

			// Reaching here means the headers were flushed with no event sent.
			assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
			assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
			assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"))
			assert.Equal(t, tc.wantGzip, resp.Uncompressed, "the case must exercise the writer it names")
			for i := 1; i <= count; i++ {
				ack <- struct{}{}
				want := fmt.Sprintf("id: %d\nevent: tick\ndata: %d\n\n", i, i)
				assert.Equal(t, want, readEvent(t, br))
			}
			_, err := nextEvent(br)
			assert.ErrorIs(t, err, io.EOF, "the stream ends when the handler returns")
		})
	}
}

const (
	tickCount         = 10
	tickEvery         = 50 * time.Millisecond
	shortWriteTimeout = 100 * time.Millisecond
)

// tickingHandler sends tickCount events, one every tickEvery, through
// whatever open returns. The two streams below differ only in open.
func tickingHandler(open func(http.ResponseWriter, *http.Request) (func(id int) error, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		send, err := open(w, r)
		if err != nil {
			httpx.InternalError(w, r, err)
			return
		}
		ticker := time.NewTicker(tickEvery)
		defer ticker.Stop()
		for i := 1; i <= tickCount; i++ {
			select {
			case <-ticker.C:
			case <-r.Context().Done():
				return
			}
			if send(i) != nil {
				return
			}
		}
	}
}

func viaEventStream(opts httpx.EventStreamOptions) func(http.ResponseWriter, *http.Request) (func(int) error, error) {
	return func(w http.ResponseWriter, r *http.Request) (func(int) error, error) {
		s, err := httpx.NewEventStream(w, r, opts)
		if err != nil {
			return nil, err
		}
		return func(id int) error {
			return s.Send(httpx.ServerEvent{ID: strconv.Itoa(id), Data: []byte("tick")})
		}, nil
	}
}

func viaPlainWrites(w http.ResponseWriter, _ *http.Request) (func(int) error, error) {
	w.Header().Set("Content-Type", "text/event-stream")
	rc := http.NewResponseController(w)
	return func(id int) error {
		if _, err := fmt.Fprintf(w, "id: %d\ndata: tick\n\n", id); err != nil {
			return err
		}
		return rc.Flush()
	}, nil
}

func TestEventStreamOutlivesTheServersWriteTimeout(t *testing.T) {
	// The router's own timeout is off, as the design says a long stream's must
	// be; only the server's write timeout is left, and a stream five times as
	// long as it must still arrive whole. Each send has its own limit, so a
	// limit shorter than the whole stream must not end it either.
	tests := []struct {
		name string
		opts httpx.EventStreamOptions
	}{
		{"with the default send limit", httpx.EventStreamOptions{}},
		{"with a send limit shorter than the stream", httpx.EventStreamOptions{SendTimeout: 3 * tickEvery}},
		{"with no send limit", httpx.EventStreamOptions{SendTimeout: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(t, httpx.RouterOptions{Timeout: -1})
			router.Get("/stream", tickingHandler(viaEventStream(tc.opts)))
			base := serve(t, shortWriteTimeout, router)

			start := time.Now()
			events, err := collectEvents(streamContext(t), t, base+"/stream")
			elapsed := time.Since(start)

			require.NoError(t, err)
			assert.Len(t, events, tickCount)
			assert.Equal(t, "id: 1\ndata: tick\n\n", events[0])
			assert.Equal(t, "id: 10\ndata: tick\n\n", events[tickCount-1])
			assert.Greater(t, elapsed, 3*shortWriteTimeout, "the stream must have run past the write timeout")
			if tc.opts.SendTimeout > 0 {
				assert.Greater(t, elapsed, tc.opts.SendTimeout, "and past the limit on a send")
			}
		})
	}
}

func TestPlainWritesAreCutOffByTheWriteTimeoutWhichTheFixtureMustShow(t *testing.T) {
	// The case that must fail: the same handler body without NewEventStream.
	// With the server's default write timeout it delivers everything, so a
	// cut at 100 milliseconds is the timeout's doing and not the fixture's.
	tests := []struct {
		name         string
		writeTimeout time.Duration
		wantCut      bool
	}{
		{name: "with a 100 millisecond write timeout", writeTimeout: shortWriteTimeout, wantCut: true},
		{name: "with the default write timeout", writeTimeout: 0, wantCut: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := newTestRouter(t, httpx.RouterOptions{Timeout: -1})
			router.Get("/stream", tickingHandler(viaPlainWrites))
			base := serve(t, tc.writeTimeout, router)

			events, err := collectEvents(streamContext(t), t, base+"/stream")

			if tc.wantCut {
				require.Error(t, err, "the connection should have been closed under the stream")
				assert.Less(t, len(events), tickCount)
				return
			}
			require.NoError(t, err)
			assert.Len(t, events, tickCount)
		})
	}
}

func TestEventStreamEndsWithTheRoutersTimeout(t *testing.T) {
	// The helper does not outlive the request context, and the router's
	// timeout ends that context: a deployment that wants one long connection
	// builds its router with Timeout -1. Ending a stream this way is ordinary,
	// since the client reconnects, so it must not be logged as a problem: the
	// logger here keeps only warnings and above, which is also where net/http
	// reports a second status written to a response already sent.
	type ending struct {
		err  error
		sent int
	}
	problems := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(problems, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ended := make(chan ending, 1)
	router := newTestRouter(t, httpx.RouterOptions{Timeout: 150 * time.Millisecond, Logger: logger})
	router.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
		s, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{})
		if !assert.NoError(t, err) {
			return
		}
		sent := 0
		send := func() error {
			err := s.Send(httpx.ServerEvent{Data: []byte("tick")})
			if err == nil {
				sent++
			}
			return err
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		err = send()
		for err == nil {
			<-ticker.C
			err = send()
		}
		ended <- ending{err, sent}
	})
	base := serveLogging(t, 0, router, logger)

	events, err := collectEvents(streamContext(t), t, base+"/stream")

	require.NoError(t, err, "the stream ends cleanly, which is what lets EventSource reconnect")
	select {
	case end := <-ended:
		assert.ErrorIs(t, end.err, context.DeadlineExceeded)
		assert.Len(t, events, end.sent, "the client received every event the handler sent")
	case <-time.After(2 * time.Second):
		t.Fatal("the handler was still sending after the router's timeout")
	}
	assert.Empty(t, problems.String(), "a stream ended by the router's timeout logged a warning")
}

func TestASendToAClientThatStopsReadingIsBounded(t *testing.T) {
	// The write deadline is lifted for the stream, so without a limit on each
	// send a client that stays connected and stops reading holds the handler in
	// its write once the socket buffers are full, whatever the context says.
	const limit = 200 * time.Millisecond
	type result struct {
		err     error
		elapsed time.Duration
		again   error
	}
	tests := []struct {
		name string
		size int
	}{
		{"large events block a write", 256 << 10},
		{"small events block a flush", 1500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ended := make(chan result, 1)
			router := newTestRouter(t, httpx.RouterOptions{})
			router.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
				s, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{SendTimeout: limit})
				if !assert.NoError(t, err) {
					return
				}
				event := httpx.ServerEvent{Data: bytes.Repeat([]byte("x"), tc.size)}
				for {
					start := time.Now()
					if err := s.Send(event); err != nil {
						ended <- result{err, time.Since(start), s.Comment("anyone there")}
						return
					}
				}
			})
			base := serve(t, 0, router)
			// The client reads the headers and then nothing, so its socket
			// buffers fill and stay full.
			openStream(streamContext(t), t, base+"/stream", nil)

			select {
			case res := <-ended:
				require.ErrorIs(t, res.err, os.ErrDeadlineExceeded)
				// The send that fails is the one that was held up, not a later one
				// that found the connection already broken: a flush error lost on
				// the way up would show as a failure that came at once.
				assert.GreaterOrEqual(t, res.elapsed, limit, "the send that failed was not the one held up")
				assert.Less(t, res.elapsed, 2*time.Second, "the send that hit the limit took far longer than it")
				assert.ErrorIs(t, res.again, res.err, "the stream is unusable afterwards")
			case <-time.After(4 * time.Second):
				t.Fatal("a send to a client that stopped reading was still blocked after four seconds")
			}
		})
	}
}

func TestAnIdleStreamStillEndsCleanlyAfterItsSendLimit(t *testing.T) {
	// The deadline around a send is lifted again once the event is out. Left in
	// place it would run out while the stream sat idle, and the write that ends
	// the response would be the one to fail: the client would see a cut stream.
	const limit = 50 * time.Millisecond
	router := newTestRouter(t, httpx.RouterOptions{Timeout: -1})
	router.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
		s, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{SendTimeout: limit})
		if !assert.NoError(t, err) {
			return
		}
		for _, data := range []string{"first", "second"} {
			assert.NoError(t, s.Send(httpx.ServerEvent{Data: []byte(data)}))
			select {
			case <-time.After(4 * limit):
			case <-r.Context().Done():
				return
			}
		}
	})
	base := serve(t, 0, router)

	events, err := collectEvents(streamContext(t), t, base+"/stream")

	require.NoError(t, err, "the stream was cut instead of ending")
	assert.Equal(t, []string{"data: first\n\n", "data: second\n\n"}, events)
}

func TestAClientThatGoesAwayEndsTheHandlerThroughTheRequestContext(t *testing.T) {
	ended := make(chan error, 1)
	router := newTestRouter(t, httpx.RouterOptions{})
	router.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
		s, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{})
		if !assert.NoError(t, err) {
			return
		}
		assert.NoError(t, s.Send(httpx.ServerEvent{Data: []byte("hello")}))
		// Idle, as a stream waiting for the next change is.
		<-r.Context().Done()
		ended <- r.Context().Err()
	})
	base := serve(t, 0, router)
	ctx, goAway := context.WithCancel(streamContext(t))
	_, br := openStream(ctx, t, base+"/stream", nil)
	assert.Equal(t, "data: hello\n\n", readEvent(t, br))

	goAway()

	select {
	case err := <-ended:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("the handler was still running two seconds after its client went away")
	}
}

func TestAReconnectingClientIsServedFromItsLastEventID(t *testing.T) {
	router := newTestRouter(t, httpx.RouterOptions{})
	router.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
		next := 1
		if last := httpx.LastEventID(r); last != "" {
			n, err := strconv.Atoi(last)
			if err != nil {
				httpx.BadRequest(w, r, err)
				return
			}
			next = n + 1
		}
		s, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{})
		if !assert.NoError(t, err) {
			return
		}
		for id := next; id < next+2; id++ {
			assert.NoError(t, s.Send(httpx.ServerEvent{ID: strconv.Itoa(id), Data: []byte("event " + strconv.Itoa(id))}))
		}
	})
	base := serve(t, 0, router)

	first, err := collectEvents(streamContext(t), t, base+"/stream")
	require.NoError(t, err)
	require.Equal(t, []string{"id: 1\ndata: event 1\n\n", "id: 2\ndata: event 2\n\n"}, first)

	// The connection dropped; EventSource reconnects with the last id it saw.
	last, _ := strings.CutPrefix(strings.SplitN(first[1], "\n", 2)[0], "id: ")
	resp, err := dial(streamContext(t), t, base+"/stream", http.Header{"Last-Event-Id": {last}})
	require.NoError(t, err)
	br := bufio.NewReader(resp.Body)
	assert.Equal(t, "id: 3\ndata: event 3\n\n", readEvent(t, br))
	assert.Equal(t, "id: 4\ndata: event 4\n\n", readEvent(t, br))
}

func TestAHandlerCanStillAnswerWhenTheWriterCannotFlush(t *testing.T) {
	// Some middleware wraps the writer and passes neither Flush nor Unwrap.
	hideFlush := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(struct{ http.ResponseWriter }{w}, r)
		})
	}
	router := newTestRouter(t, httpx.RouterOptions{})
	router.With(hideFlush).Get("/stream", func(w http.ResponseWriter, r *http.Request) {
		_, err := httpx.NewEventStream(w, r, httpx.EventStreamOptions{})
		if errors.Is(err, httpx.ErrStreamUnsupported) {
			httpx.Error(w, r, http.StatusNotImplemented, err)
			return
		}
		assert.Fail(t, "the stream should have been refused", "err: %v", err)
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, getRequest())

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Header().Get("Cache-Control"))
	assert.Empty(t, rec.Header().Get("X-Accel-Buffering"))
}
