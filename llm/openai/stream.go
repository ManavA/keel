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

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/internal/sse"
)

// doneMarker is the data of the event that ends a stream.
const doneMarker = "[DONE]"

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
	resp, err := c.post(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	st := newStreamState()
	events := sse.NewReader(resp.Body)
	for {
		ev, err := events.Next()
		if err != nil {
			return c.endOfStream(ctx, st, c.modelFor(req), err)
		}
		data := bytes.TrimSpace(ev.Data)
		switch {
		case len(data) == 0:
			continue
		case string(data) == doneMarker:
			return c.endOfStream(ctx, st, c.modelFor(req), io.EOF)
		}

		var ch wireChunk
		if err := json.Unmarshal(data, &ch); err != nil {
			return nil, fmt.Errorf("openai: decode stream chunk: %w", err)
		}
		if e, ok := errorFields(ch.Error); ok {
			return nil, embeddedError(resp, e)
		}
		if err := st.apply(ch, fn); err != nil {
			return nil, err
		}
	}
}

// endOfStream settles a stream that has stopped, cleanly or not. A reply is
// complete once a chunk has carried a finish reason, and what stops after
// that, a dropped connection or a missing [DONE], is of no consequence beyond
// a usage chunk that may not have arrived. Any earlier stop is a failure of
// the connection, which the same request may get past.
func (c *Client) endOfStream(ctx context.Context, st *streamState, requested string, cause error) (*llm.Response, error) {
	if st.finished {
		if !errors.Is(cause, io.EOF) {
			c.log.Warn("openai: stream cut after its finish reason; its usage may be missing", "error", cause)
		}
		return c.response(st.reply(), requested), nil
	}
	if errors.Is(cause, io.EOF) {
		cause = io.ErrUnexpectedEOF
	}
	if errors.Is(cause, io.ErrUnexpectedEOF) {
		cause = fmt.Errorf("stream ended before a finish reason: %w", cause)
	}
	return nil, transportError(ctx, cause)
}

// streamState accumulates the chunks of one stream.
type streamState struct {
	id, model string
	text      strings.Builder
	refusal   strings.Builder
	calls     map[int]*streamCall
	finish    string
	finished  bool
	usage     *wireUsage
}

// streamCall is one tool call being assembled, found by its index.
type streamCall struct {
	id, name  string
	arguments strings.Builder
}

func newStreamState() *streamState {
	return &streamState{calls: map[int]*streamCall{}}
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
	if len(ch.Choices) == 0 {
		return nil
	}

	choice := ch.Choices[0]
	if text := choice.Delta.Content; text != "" {
		s.text.WriteString(text)
		if err := fn(llm.Delta{Text: text}); err != nil {
			return err
		}
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

// applyCall adds one piece of a tool call. The callback gets the id and the
// name once, with the first piece that carries each, and the arguments as
// they come.
func (s *streamState) applyCall(piece wireToolCall, fn func(llm.Delta) error) error {
	call, ok := s.calls[piece.Index]
	if !ok {
		call = &streamCall{}
		s.calls[piece.Index] = call
	}
	d := llm.ToolCallDelta{Index: piece.Index, InputJSON: piece.Function.Arguments}
	if call.id == "" && piece.ID != "" {
		call.id, d.ID = piece.ID, piece.ID
	}
	if call.name == "" && piece.Function.Name != "" {
		call.name, d.Name = piece.Function.Name, piece.Function.Name
	}
	call.arguments.WriteString(piece.Function.Arguments)

	if d.ID != "" || d.Name != "" || d.InputJSON != "" {
		return fn(llm.Delta{ToolCall: &d})
	}
	return nil
}

// reply is what the stream has said so far, calls in index order.
func (s *streamState) reply() reply {
	r := reply{
		id:      s.id,
		model:   s.model,
		text:    s.text.String(),
		refusal: s.refusal.String(),
		finish:  s.finish,
		usage:   s.usage,
	}
	indexes := make([]int, 0, len(s.calls))
	for i := range s.calls {
		indexes = append(indexes, i)
	}
	slices.Sort(indexes)
	for _, i := range indexes {
		call := s.calls[i]
		r.calls = append(r.calls, replyCall{id: call.id, name: call.name, arguments: call.arguments.String()})
	}
	return r
}
