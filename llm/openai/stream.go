package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/internal/sse"
)

// doneMarker is the data of the event that ends a stream.
const doneMarker = "[DONE]"

// callOverhead is what each tool call counts against the bound of a reply
// besides its id, name and arguments: what it costs apart from its text, so
// that a server that opens calls without end meets the bound.
const callOverhead = 256

// errQuiet is the cause a stream's context is cancelled with when the server
// has sent nothing for too long.
var errQuiet = errors.New("stream idle")

// wireChunk is a chat.completion.chunk.
type wireChunk struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Choices []wireChunkChoice `json:"choices"`
	Usage   *wireUsage        `json:"usage"`
	Error   json.RawMessage   `json:"error"`
}

type wireChunkChoice struct {
	Delta        wireDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type wireDelta struct {
	Content   string         `json:"content"`
	Refusal   string         `json:"refusal"`
	ToolCalls []wireToolCall `json:"tool_calls"`
}

// Stream implements llm.Model.
func (c *Client) Stream(ctx context.Context, req llm.Request, fn func(llm.Delta) error) (*llm.Response, error) {
	if fn == nil {
		fn = func(llm.Delta) error { return nil }
	}
	body, err := c.chatBody(req, true)
	if err != nil {
		return nil, err
	}

	// A stream is bounded by the caller's context and by how long it goes
	// quiet, and not by a timeout on the whole request, which would cut a long
	// reply that is going well. The watchdog cancels sctx when it goes quiet.
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	wd := newWatchdog(c.idleTimeout, cancel)
	defer wd.stop()

	httpReq, err := c.newRequest(sctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, requestFailed(ctx, resp, c.ifQuiet(ctx, sctx, err))
	}
	defer func() { _ = resp.Body.Close() }()
	live := &watchedBody{r: resp.Body, wd: wd}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := readBounded(live)
		return nil, httpError(resp, raw)
	}
	if !isEventStream(resp) {
		drain(wd, resp.Body)
		return nil, notAStream(resp)
	}

	st := newStreamState(c.maxReplyBytes)
	model := c.modelFor(req)
	events := sse.NewReader(live)
	// The callback's time is the caller's and not the server's silence, so
	// the idle clock waits while it runs.
	deliver := func(d llm.Delta) error {
		wd.pause()
		defer wd.resume()
		return fn(d)
	}
	for {
		ev, err := events.Next()
		if err != nil {
			return c.endOfStream(ctx, st, model, c.ifQuiet(ctx, sctx, err))
		}
		data := bytes.TrimSpace(ev.Data)
		switch {
		case len(data) == 0:
			continue
		case string(data) == doneMarker:
			out, err := c.endOfStream(ctx, st, model, nil)
			if err == nil {
				drain(wd, resp.Body)
			}
			return out, err
		}

		var ch wireChunk
		if err := json.Unmarshal(data, &ch); err != nil {
			return nil, fmt.Errorf("openai: decode stream chunk: %w", err)
		}
		if e, ok := errorFields(ch.Error); ok && e.present() {
			return nil, embeddedError(resp.StatusCode, resp.Header.Get("X-Request-Id"), e)
		}
		if err := st.apply(ch, deliver); err != nil {
			return nil, err
		}
	}
}

// ifQuiet is err, or when the stream's context was cancelled because the
// server went quiet, an error that says so. The caller's own context ending is
// not that, and wins.
func (c *Client) ifQuiet(ctx, sctx context.Context, err error) error {
	if ctx.Err() == nil && errors.Is(context.Cause(sctx), errQuiet) {
		return fmt.Errorf("no data from the server for %s", c.idleTimeout)
	}
	return err
}

// endOfStream settles a stream that has stopped. cause is why the reader
// stopped, or nil when the stream ended with [DONE].
//
// A reply is complete once a chunk has carried a finish reason, and what stops
// after that, a dropped connection or a missing [DONE], matters only for a usage
// chunk that may not have arrived. Some servers never send a finish reason, so a
// clean [DONE] also completes a reply, with its stop reason read from what it
// holds, unless nothing at all came before it. Any other stop is a failure of
// the connection, which the same request may get past; except an event that is
// too large, which a retry would meet again.
func (c *Client) endOfStream(ctx context.Context, st *streamState, requested string, cause error) (*llm.Response, error) {
	switch {
	case st.finished:
		if cause != nil && !errors.Is(cause, io.EOF) {
			c.log.Warn("openai: stream cut after its finish reason; its usage may be missing", "error", cause)
		}
		return c.response(st.assembled(), requested), nil
	case cause == nil && st.seen:
		c.log.Debug("openai: stream ended with [DONE] and no finish_reason; the stop reason is read from the reply")
		return c.response(st.assembled(), requested), nil
	case cause == nil:
		cause = fmt.Errorf("stream ended with [DONE] and no reply before it: %w", io.ErrUnexpectedEOF)
	case errors.Is(cause, sse.ErrEventTooLarge):
		return nil, fmt.Errorf("openai: a stream event is larger than %d bytes", sse.MaxEventBytes)
	case errors.Is(cause, io.EOF):
		cause = fmt.Errorf("stream ended before a finish reason or [DONE]: %w", io.ErrUnexpectedEOF)
	case errors.Is(cause, io.ErrUnexpectedEOF):
		cause = fmt.Errorf("stream ended before a finish reason or [DONE]: %w", cause)
	}
	return nil, failed(ctx, cause)
}

// streamState accumulates the chunks of one stream.
type streamState struct {
	id, model string
	text      strings.Builder
	refusal   strings.Builder
	calls     map[int]*streamCall
	// byKey is the most recent slot opened for each index the server has
	// sent, and latestKey the index of the last piece, which a piece that
	// sends none continues.
	byKey     map[int]int
	latestKey int
	// size is what the reply adds up to so far, bounded by limit.
	size, limit int
	finish      string
	finished    bool
	// seen is whether any chunk had an id or a choice: whether the server
	// began a reply.
	seen  bool
	usage *wireUsage
}

// streamCall is one tool call being assembled, found by its slot.
type streamCall struct {
	id, name  string
	arguments strings.Builder
}

func newStreamState(limit int) *streamState {
	return &streamState{calls: map[int]*streamCall{}, byKey: map[int]int{}, limit: limit}
}

// apply folds one chunk in and passes what it adds to fn.
func (s *streamState) apply(ch wireChunk, fn func(llm.Delta) error) error {
	if s.id == "" {
		s.id = ch.ID
	}
	if ch.Model != "" {
		s.model = ch.Model
	}
	if ch.Usage != nil {
		s.usage = ch.Usage
	}
	if ch.ID != "" || len(ch.Choices) > 0 {
		s.seen = true
	}
	if len(ch.Choices) == 0 {
		return nil
	}

	choice := ch.Choices[0]
	if text := choice.Delta.Content; text != "" {
		if err := s.grow(len(text)); err != nil {
			return err
		}
		s.text.WriteString(text)
		if err := fn(llm.Delta{Text: text}); err != nil {
			return err
		}
	}
	if err := s.grow(len(choice.Delta.Refusal)); err != nil {
		return err
	}
	s.refusal.WriteString(choice.Delta.Refusal)
	for _, piece := range choice.Delta.ToolCalls {
		if piece.Function == nil {
			continue
		}
		if err := s.applyCall(piece, fn); err != nil {
			return err
		}
	}
	if choice.FinishReason != "" {
		s.finish = choice.FinishReason
		s.finished = true
	}
	return nil
}

// grow counts n more bytes of the reply against its bound.
func (s *streamState) grow(n int) error {
	s.size += n
	if s.size > s.limit {
		return fmt.Errorf("openai: the reply is larger than %d bytes", s.limit)
	}
	return nil
}

// slot is the slot a piece of a tool call belongs to. A piece belongs to the
// most recent slot opened for its index, or for a server that sends none, for
// the index of the last piece. An id that is not the id that slot already has
// is a different call, so it opens a new slot, and calls are not merged into one
// malformed call. A slot is the index the server gave unless that slot is taken.
func (s *streamState) slot(piece wireToolCall) int {
	key := s.latestKey
	if piece.Index != nil {
		key = *piece.Index
	}
	slot, ok := s.byKey[key]
	switch call := s.calls[slot]; {
	case !ok:
		slot = key
		if _, taken := s.calls[slot]; taken {
			slot = s.nextSlot()
		}
	case call != nil && piece.ID != "" && call.id != "" && piece.ID != call.id:
		slot = s.nextSlot()
	}
	s.byKey[key] = slot
	s.latestKey = key
	return slot
}

// nextSlot is the first slot after every one in use.
func (s *streamState) nextSlot() int {
	next := 0
	for i := range s.calls {
		next = max(next, i+1)
	}
	return next
}

// applyCall adds one piece of a tool call. The callback gets the id and the
// name once, with the first piece that carries each, and the arguments as
// they come. Every byte the reply keeps counts against its bound: each call,
// its id and name when they are first stored, and its arguments.
func (s *streamState) applyCall(piece wireToolCall, fn func(llm.Delta) error) error {
	idx := s.slot(piece)
	call, ok := s.calls[idx]
	if !ok {
		if err := s.grow(callOverhead); err != nil {
			return err
		}
		call = &streamCall{}
		s.calls[idx] = call
	}
	d := llm.ToolCallDelta{Index: idx, InputJSON: piece.Function.Arguments}
	if call.id == "" && piece.ID != "" {
		if err := s.grow(len(piece.ID)); err != nil {
			return err
		}
		call.id, d.ID = piece.ID, piece.ID
	}
	if call.name == "" && piece.Function.Name != "" {
		if err := s.grow(len(piece.Function.Name)); err != nil {
			return err
		}
		call.name, d.Name = piece.Function.Name, piece.Function.Name
	}
	if err := s.grow(len(piece.Function.Arguments)); err != nil {
		return err
	}
	call.arguments.WriteString(piece.Function.Arguments)

	if d.ID != "" || d.Name != "" || d.InputJSON != "" {
		return fn(llm.Delta{ToolCall: &d})
	}
	return nil
}

// assembled is what the stream has said so far, calls in slot order.
func (s *streamState) assembled() reply {
	r := reply{
		id:      s.id,
		model:   s.model,
		text:    s.text.String(),
		refusal: s.refusal.String(),
		finish:  s.finish,
		usage:   s.usage,
	}
	slots := make([]int, 0, len(s.calls))
	for i := range s.calls {
		slots = append(slots, i)
	}
	slices.Sort(slots)
	for _, i := range slots {
		call := s.calls[i]
		r.calls = append(r.calls, replyCall{id: call.id, name: call.name, arguments: call.arguments.String()})
	}
	return r
}

// watchdog cancels a stream's context when nothing has arrived for too long.
type watchdog struct {
	idle   time.Duration
	timer  *time.Timer
	cancel context.CancelCauseFunc

	// mu guards paused, which the timer's function reads from its own goroutine.
	mu     sync.Mutex
	paused bool
}

// newWatchdog starts the clock when idle is positive; zero is no limit.
func newWatchdog(idle time.Duration, cancel context.CancelCauseFunc) *watchdog {
	w := &watchdog{idle: idle, cancel: cancel}
	if idle > 0 {
		w.timer = time.AfterFunc(idle, w.fire)
	}
	return w
}

// fire is the timer's function: the stream has been quiet, unless the clock was
// stopped for the callback in the moment since it went off.
func (w *watchdog) fire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.paused {
		w.cancel(errQuiet)
	}
}

// touch records that bytes arrived.
func (w *watchdog) touch() {
	if w.idle > 0 {
		w.timer.Reset(w.idle)
	}
}

// pause stops the clock while the caller's callback runs, and resume starts it
// again from nothing when the callback returns.
func (w *watchdog) pause() {
	if w.idle <= 0 {
		return
	}
	w.mu.Lock()
	w.paused = true
	w.timer.Stop()
	w.mu.Unlock()
}

func (w *watchdog) resume() {
	if w.idle <= 0 {
		return
	}
	w.mu.Lock()
	w.paused = false
	w.timer.Reset(w.idle)
	w.mu.Unlock()
}

// arm sets the clock to d, whatever the limit was.
func (w *watchdog) arm(d time.Duration) {
	if w.timer == nil {
		w.timer = time.AfterFunc(d, w.fire)
		return
	}
	w.timer.Reset(d)
}

func (w *watchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}

// watchedBody tells a watchdog each time bytes arrive.
type watchedBody struct {
	r  io.Reader
	wd *watchdog
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.wd.touch()
	}
	return n, err
}

// drain reads what is left of a body that has ended cleanly, a little and for a
// moment, so that net/http can use the connection again: it does so only for a
// body read to its end, and DONE is read before the end of the chunked body
// that carries it. A server that holds the stream open, or goes on sending, is
// given up on, and its connection is not reused.
func drain(wd *watchdog, body io.Reader) {
	wd.arm(drainTimeout)
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
}
