package sse_test

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm/internal/sse"
)

// event is an sse.Event with its data as a string, so a failed comparison
// prints text rather than bytes.
type event struct {
	Name string
	Data string
	ID   string
}

// readAll reads r to the end of its stream.
func readAll(t *testing.T, r *sse.Reader) []event {
	t.Helper()

	var got []event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err)
		got = append(got, event{Name: ev.Name, Data: string(ev.Data), ID: ev.ID})
	}
}

func TestReader_Next(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []event
	}{
		{name: "an empty stream", stream: "", want: nil},
		{
			name:   "one event",
			stream: "event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
			want:   []event{{Name: "message_start", Data: `{"type":"message_start"}`}},
		},
		{
			name:   "two events",
			stream: "event: a\ndata: one\n\nevent: b\ndata: two\n\n",
			want:   []event{{Name: "a", Data: "one"}, {Name: "b", Data: "two"}},
		},
		{
			name:   "data over several lines is joined with a line feed",
			stream: "data: first\ndata: second\ndata: third\n\n",
			want:   []event{{Data: "first\nsecond\nthird"}},
		},
		{
			name:   "a comment line is skipped",
			stream: ": keep-alive\n\nevent: a\n: inside an event\ndata: one\n\n",
			want:   []event{{Name: "a", Data: "one"}},
		},
		{
			name:   "an event with no event field has no name",
			stream: "data: {\"id\":\"chunk\"}\n\ndata: [DONE]\n\n",
			want:   []event{{Data: `{"id":"chunk"}`}, {Data: "[DONE]"}},
		},
		{
			name:   "an event with an id",
			stream: "id: 42\nevent: a\ndata: one\n\n",
			want:   []event{{Name: "a", Data: "one", ID: "42"}},
		},
		{
			name:   "an id belongs to the event that carries it",
			stream: "id: 42\ndata: one\n\ndata: two\n\n",
			want:   []event{{Data: "one", ID: "42"}, {Data: "two"}},
		},
		{
			name:   "a stream that ends without the final blank line still gives its last event",
			stream: "data: one\n\nevent: b\ndata: two\n",
			want:   []event{{Data: "one"}, {Name: "b", Data: "two"}},
		},
		{
			name:   "a stream that ends in the middle of a line still gives its last event",
			stream: "data: one\n\ndata: two",
			want:   []event{{Data: "one"}, {Data: "two"}},
		},
		{
			name:   "an event with no data is skipped, and its name does not reach the next",
			stream: "event: ping\n\ndata: one\n\n",
			want:   []event{{Data: "one"}},
		},
		{
			name:   "an empty data field is data",
			stream: "event: a\ndata:\n\n",
			want:   []event{{Name: "a", Data: ""}},
		},
		{
			name:   "only one space after the colon is dropped",
			stream: "data:no space\n\ndata:  two spaces\n\n",
			want:   []event{{Data: "no space"}, {Data: " two spaces"}},
		},
		{
			name:   "a colon inside the value is kept",
			stream: "data: {\"a\":\"b: c\"}\n\n",
			want:   []event{{Data: `{"a":"b: c"}`}},
		},
		{
			name:   "lines may end in a carriage return and a line feed",
			stream: "event: a\r\ndata: one\r\ndata: two\r\n\r\n",
			want:   []event{{Name: "a", Data: "one\ntwo"}},
		},
		{
			name:   "a field this reader does not know is ignored",
			stream: "retry: 3000\nfuture: thing\ndata: one\n\n",
			want:   []event{{Data: "one"}},
		},
		{
			name:   "a line with no colon is a field with no value",
			stream: "data\ndata: one\n\n",
			want:   []event{{Data: "\none"}},
		},
		{
			name:   "blank lines between events are not events",
			stream: "\n\ndata: one\n\n\n\ndata: two\n\n",
			want:   []event{{Data: "one"}, {Data: "two"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, readAll(t, sse.NewReader(strings.NewReader(tt.stream))))
		})
	}
}

// chunks is a reader that returns its input one piece per Read, the way a
// network connection delivers a stream cut at arbitrary points.
type chunks struct {
	pieces []string
}

func (c *chunks) Read(p []byte) (int, error) {
	if len(c.pieces) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.pieces[0])
	if n < len(c.pieces[0]) {
		c.pieces[0] = c.pieces[0][n:]
		return n, nil
	}
	c.pieces = c.pieces[1:]
	return n, nil
}

func TestReader_InputCutAcrossReads(t *testing.T) {
	const stream = "event: a\ndata: first\ndata: second\n\nid: 7\ndata: two\n\n"
	want := []event{{Name: "a", Data: "first\nsecond"}, {Data: "two", ID: "7"}}

	tests := []struct {
		name   string
		reader io.Reader
	}{
		{
			name: "in the middle of a field name, a value and a blank line",
			reader: &chunks{pieces: []string{
				"eve", "nt: a\nda", "ta: fir", "st\ndata: second\n", "\nid: 7\ndata: tw", "o\n", "\n",
			}},
		},
		{name: "one byte at a time", reader: iotest.OneByteReader(strings.NewReader(stream))},
		{name: "half of each read", reader: iotest.HalfReader(strings.NewReader(stream))},
		{name: "the last bytes arriving with the end of the stream", reader: iotest.DataErrReader(strings.NewReader(stream))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, want, readAll(t, sse.NewReader(tt.reader)))
		})
	}
}

func TestReader_ALineLongerThanTheReadBuffer(t *testing.T) {
	long := strings.Repeat("x", 100_000)

	got := readAll(t, sse.NewReader(strings.NewReader("data: "+long+"\ndata: tail\n\n: "+long+"\ndata: after\n\n")))

	assert.Equal(t, []event{{Data: long + "\ntail"}, {Data: "after"}}, got)
}

func TestReader_EndOfStreamIsSticky(t *testing.T) {
	r := sse.NewReader(strings.NewReader("data: one\n"))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, "one", string(ev.Data))

	for range 3 {
		_, err = r.Next()
		assert.ErrorIs(t, err, io.EOF)
	}
}

func TestReader_EventDataIsNotReused(t *testing.T) {
	r := sse.NewReader(strings.NewReader("data: first\n\ndata: second\n\n"))

	first, err := r.Next()
	require.NoError(t, err)
	_, err = r.Next()
	require.NoError(t, err)

	assert.Equal(t, "first", string(first.Data), "reading the next event must not overwrite the one before it")
}

func TestReader_ReadErrorIsReturned(t *testing.T) {
	errBroken := errors.New("connection reset")
	r := sse.NewReader(io.MultiReader(
		strings.NewReader("data: one\n\ndata: half an ev"),
		iotest.ErrReader(errBroken),
	))

	ev, err := r.Next()
	require.NoError(t, err)
	assert.Equal(t, "one", string(ev.Data))

	// A stream that broke is not a stream that ended: the event it was in
	// the middle of is not handed out as though it were whole.
	_, err = r.Next()
	require.ErrorIs(t, err, errBroken)
	assert.NotErrorIs(t, err, io.EOF)

	_, err = r.Next()
	assert.ErrorIs(t, err, errBroken, "the failure is returned again, not replaced by io.EOF")
}

// endless repeats a pattern for ever, so a reader that did not bound an event
// would never return.
type endless struct {
	pattern string
	off     int
}

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = e.pattern[e.off]
		e.off = (e.off + 1) % len(e.pattern)
	}
	return len(p), nil
}

func TestReader_EventOverTheBound(t *testing.T) {
	const field = "data: "

	t.Run("an event of exactly MaxEventBytes is read", func(t *testing.T) {
		payload := strings.Repeat("x", sse.MaxEventBytes-len(field))

		ev, err := sse.NewReader(strings.NewReader(field + payload + "\n\n")).Next()

		require.NoError(t, err)
		assert.Len(t, ev.Data, len(payload))
	})

	t.Run("one byte more is an error", func(t *testing.T) {
		payload := strings.Repeat("x", sse.MaxEventBytes-len(field)+1)
		r := sse.NewReader(strings.NewReader(field + payload + "\n\ndata: next\n\n"))

		_, err := r.Next()
		require.Error(t, err)
		assert.NotErrorIs(t, err, io.EOF)
		assert.Contains(t, err.Error(), "16777216")

		_, again := r.Next()
		assert.Equal(t, err, again, "the reader does not resume part way through an event it refused")
	})

	t.Run("a line that never ends is refused, not read for ever", func(t *testing.T) {
		_, err := sse.NewReader(&endless{pattern: "x"}).Next()

		require.Error(t, err)
		assert.NotErrorIs(t, err, io.EOF)
	})

	t.Run("an event of many short lines that never ends is refused", func(t *testing.T) {
		_, err := sse.NewReader(&endless{pattern: "data: xxxxxxxxxx\n"}).Next()

		require.Error(t, err)
		assert.NotErrorIs(t, err, io.EOF)
	})

	t.Run("comments are not part of any event and may run past the bound", func(t *testing.T) {
		const comment = ": keep-alive\n"
		stream := io.MultiReader(
			io.LimitReader(&endless{pattern: comment}, int64(len(comment)*(sse.MaxEventBytes/len(comment)+2))),
			strings.NewReader("data: one\n\n"),
		)

		ev, err := sse.NewReader(stream).Next()

		require.NoError(t, err)
		assert.Equal(t, "one", string(ev.Data))
	})

	t.Run("a comment inside an event already at the bound does not push it over", func(t *testing.T) {
		payload := strings.Repeat("x", sse.MaxEventBytes-len(field))

		ev, err := sse.NewReader(strings.NewReader(field + payload + "\n: keep-alive\n\n")).Next()

		require.NoError(t, err)
		assert.Len(t, ev.Data, len(payload))
	})

	t.Run("one comment line longer than the bound is skipped without being held", func(t *testing.T) {
		stream := io.MultiReader(
			strings.NewReader(":"),
			io.LimitReader(&endless{pattern: "x"}, sse.MaxEventBytes+1),
			strings.NewReader("\ndata: one\n\n"),
		)

		ev, err := sse.NewReader(stream).Next()

		require.NoError(t, err)
		assert.Equal(t, "one", string(ev.Data))
	})
}
