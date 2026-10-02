// Package sse reads a server-sent event stream.
//
// It reads what the two providers send and no more of the standard than
// that. Lines end in a line feed, with or without a carriage return before
// it; a bare carriage return is not a line ending, and a leading byte order
// mark is not removed. The retry field is ignored, and an id belongs to the
// event that carries it instead of persisting onto the events after it.
//
// A stream is only over when it ends between events. One that stops before
// the blank line that would end an event is reported as io.ErrUnexpectedEOF,
// the same error a transport gives for a body cut short, so a provider can
// treat both as the connection failing.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// MaxEventBytes bounds one event. A longer one is an error rather than an
// allocation without limit.
const MaxEventBytes = 16 << 20

// Event is one event from a stream.
type Event struct {
	// Name is the event field, or "" when the stream sent none.
	Name string
	// Data is the data lines joined with "\n".
	Data []byte
	ID   string
}

var (
	errTooLarge = fmt.Errorf("sse: event is larger than %d bytes", MaxEventBytes)
	errCutShort = fmt.Errorf("sse: stream ended in the middle of an event: %w", io.ErrUnexpectedEOF)
)

// Reader reads events from a stream.
type Reader struct {
	br *bufio.Reader
	// line holds the line being read. It is reused from one line to the
	// next, so nothing that outlives a line may alias it.
	line []byte
	// err is what Next returns once the stream has ended or failed.
	err error
}

// NewReader reads events from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReader(r)}
}

// Next returns the next event. It skips comment lines and events with no
// data, and returns io.EOF when the stream ends after a complete event. A
// stream that ends in the middle of an event, with no blank line after it,
// is a connection that dropped: the event is not returned, and the error
// wraps io.ErrUnexpectedEOF.
func (r *Reader) Next() (Event, error) {
	if r.err != nil {
		return Event{}, r.err
	}

	var (
		ev      Event
		hasData bool
		// size is the length of the event's lines so far, without their
		// line endings. Comments are not counted: they belong to no event.
		size int
	)
	for {
		line, comment, err := r.readLine(MaxEventBytes - size)
		if errors.Is(err, io.EOF) {
			// line is what was left after the last line ending. With that,
			// or with fields read and no blank line to end them, the stream
			// stopped part way through an event.
			r.err = io.EOF
			if size > 0 || len(line) > 0 {
				r.err = errCutShort
			}
			return Event{}, r.err
		}
		if err != nil {
			r.err = err
			return Event{}, err
		}

		switch {
		case comment:
		case len(line) > 0:
			size += len(line)
			name, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(name) {
			case "event":
				ev.Name = string(value)
			case "data":
				if hasData {
					ev.Data = append(ev.Data, '\n')
				}
				ev.Data = append(ev.Data, value...)
				hasData = true
			case "id":
				ev.ID = string(value)
			}
		case hasData:
			return ev, nil
		default:
			// A blank line with no data before it: the fields read so far
			// belong to an event that is skipped.
			ev, size = Event{}, 0
		}
	}
}

// readLine reads one line and returns it without its line ending, or
// reports that it was a comment, which is consumed without being kept. The
// error is io.EOF when the stream ended with this line, which then holds
// whatever followed the last line ending.
//
// room is how many bytes the line may add to its event. The check is made as
// the line arrives, so a line that never ends is refused once it has outgrown
// room and is not buffered whole first.
func (r *Reader) readLine(room int) (line []byte, comment bool, err error) {
	r.line = r.line[:0]
	first := true
	for {
		chunk, readErr := r.br.ReadSlice('\n')
		if first && len(chunk) > 0 {
			comment = chunk[0] == ':'
			first = false
		}
		if !comment {
			// Two bytes of grace for a line ending not yet trimmed.
			if len(r.line)+len(chunk) > room+2 {
				return nil, false, errTooLarge
			}
			r.line = append(r.line, chunk...)
		}

		switch {
		case readErr == nil, errors.Is(readErr, io.EOF):
			line = bytes.TrimSuffix(r.line, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			if len(line) > room {
				return nil, false, errTooLarge
			}
			return line, comment, readErr
		case errors.Is(readErr, bufio.ErrBufferFull):
			// The line is longer than the buffer; the rest follows.
		default:
			return nil, false, fmt.Errorf("sse: read: %w", readErr)
		}
	}
}
