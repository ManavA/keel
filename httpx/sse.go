package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrStreamUnsupported is returned by NewEventStream when the response
// cannot be flushed, so events would sit in a buffer.
var ErrStreamUnsupported = errors.New("httpx: response cannot be streamed")

// ServerEvent is one server-sent event.
type ServerEvent struct {
	// ID is sent as the event's id, which a reconnecting client returns in
	// Last-Event-ID. Empty sends none.
	ID string
	// Type is the event name. Empty sends none, which a client reads as
	// "message".
	Type string
	Data []byte
}

// EventStreamOptions configures NewEventStream. The zero value works.
type EventStreamOptions struct {
	// Retry is sent once as the delay a client should wait before
	// reconnecting. Zero sends none.
	Retry time.Duration
}

// EventStream writes server-sent events to one response. It is not safe
// for concurrent use.
//
// It does not outlive the request: once the request's context ends, Send,
// SendJSON and Comment write nothing and return an error wrapping the
// context's, and the handler should return. A write to a connection the client
// has closed often succeeds, and the router's middleware discards flush
// errors, so the context is the one dependable sign that the client is gone.
type EventStream struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	ctx context.Context // the request's, checked before every write
	buf []byte
}

// NewEventStream starts an event stream on w: it sets the headers, lifts
// the server's write deadline for this response, and flushes.
//
// The router's timeout is not lifted: it ends the request's context, and the
// stream with it. A service that wants one long connection builds its router
// with RouterOptions.Timeout set to -1.
//
// It returns ErrStreamUnsupported, before writing anything, when no writer
// in the Unwrap chain can flush, so the handler can still answer with an
// error. Any other error means the client has gone.
func NewEventStream(w http.ResponseWriter, r *http.Request, opts EventStreamOptions) (*EventStream, error) {
	if !canFlush(w) {
		return nil, ErrStreamUnsupported
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, fmt.Errorf("httpx: lift write deadline: %w", err)
	}

	// Set before the status is written: a compressing middleware reads the
	// content type there, and a header set later never reaches the client.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s := &EventStream{w: w, rc: rc, ctx: r.Context()}
	if opts.Retry > 0 {
		// Under a millisecond it would read as zero, a reconnect with no wait.
		s.buf = fmt.Appendf(nil, "retry: %d\n\n", max(opts.Retry.Milliseconds(), 1))
		if _, err := w.Write(s.buf); err != nil {
			return nil, fmt.Errorf("httpx: write retry: %w", err)
		}
	}
	if err := rc.Flush(); err != nil {
		return nil, fmt.Errorf("httpx: start event stream: %w", err)
	}
	return s, nil
}

// canFlush reports whether a flush on w would reach a writer that does it,
// walking the same chain http.ResponseController walks, so that the answer is
// known before anything is written.
func canFlush(w http.ResponseWriter) bool {
	for {
		switch t := w.(type) {
		case interface{ FlushError() error }, http.Flusher:
			return true
		case interface{ Unwrap() http.ResponseWriter }:
			w = t.Unwrap()
		default:
			return false
		}
	}
}

// Send writes one event and flushes it.
func (s *EventStream) Send(ev ServerEvent) error {
	if strings.ContainsAny(ev.ID, "\r\n") {
		return errors.New("httpx: event id contains a line break")
	}
	if strings.ContainsAny(ev.Type, "\r\n") {
		return errors.New("httpx: event type contains a line break")
	}
	if err := s.ctx.Err(); err != nil {
		return fmt.Errorf("httpx: send event: %w", err)
	}

	buf := s.buf[:0]
	if ev.ID != "" {
		buf = appendField(buf, "id", ev.ID)
	}
	if ev.Type != "" {
		buf = appendField(buf, "event", ev.Type)
	}
	buf = appendData(buf, ev.Data)
	buf = append(buf, '\n')
	s.buf = buf
	return s.write()
}

// SendJSON writes v as the data of one event.
func (s *EventStream) SendJSON(id, typ string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("httpx: encode event: %w", err)
	}
	return s.Send(ServerEvent{ID: id, Type: typ, Data: data})
}

// Comment writes a comment line, which clients ignore. It is how an idle
// stream keeps a proxy from closing it. The text may not contain a line break.
func (s *EventStream) Comment(text string) error {
	if strings.ContainsAny(text, "\r\n") {
		return errors.New("httpx: comment contains a line break")
	}
	if err := s.ctx.Err(); err != nil {
		return fmt.Errorf("httpx: send comment: %w", err)
	}
	s.buf = append(appendField(s.buf[:0], "", text), '\n')
	return s.write()
}

// write sends s.buf as one Write, so a failure never leaves half an event on
// the wire, and flushes it.
func (s *EventStream) write() error {
	if _, err := s.w.Write(s.buf); err != nil {
		return fmt.Errorf("httpx: write event: %w", err)
	}
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("httpx: flush event: %w", err)
	}
	return nil
}

// LastEventID is the id of the last event a reconnecting client received,
// or "".
func LastEventID(r *http.Request) string {
	return r.Header.Get("Last-Event-ID")
}

// appendField appends one "name: value" line. An empty name makes a comment
// line, and an empty value drops the space after the colon.
func appendField[V string | []byte](buf []byte, name string, value V) []byte {
	buf = append(buf, name...)
	buf = append(buf, ':')
	if len(value) > 0 {
		buf = append(buf, ' ')
		buf = append(buf, value...)
	}
	return append(buf, '\n')
}

// appendData appends one data line for each line of data, with at least one
// for empty data, which a client still dispatches. A line ends at LF, CR or
// CRLF, the three a client accepts, so no byte of data can begin a field of
// its own.
func appendData(buf, data []byte) []byte {
	for {
		end := bytes.IndexAny(data, "\r\n")
		if end < 0 {
			return appendField(buf, "data", data)
		}
		buf = appendField(buf, "data", data[:end])
		next := end + 1
		if data[end] == '\r' && next < len(data) && data[next] == '\n' {
			next++
		}
		data = data[next:]
	}
}
