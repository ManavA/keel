package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/internal/sse"
)

// The events of a stream that this package acts on. Any other is ignored:
// the reference says new ones may be added.
const (
	eventMessageStart      = "message_start"
	eventContentBlockStart = "content_block_start"
	eventContentBlockDelta = "content_block_delta"
	eventContentBlockStop  = "content_block_stop"
	eventMessageDelta      = "message_delta"
	eventMessageStop       = "message_stop"
	eventError             = "error"
)

// streamEvent is the fields of every event this package acts on, in one
// struct. Each event fills in the ones its type has.
type streamEvent struct {
	Message      *wireMessage    `json:"message"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Usage        *wireUsage      `json:"usage"`
	Error        wireError       `json:"error"`
}

// streamed is a reply being put together from a stream's events.
type streamed struct {
	// msg holds the id, model, stop reason and usage as the events have
	// given them so far.
	msg     wireMessage
	started bool
	blocks  map[int]*streamedBlock
	// calls counts the client tool calls opened so far.
	calls int
	// lost says why the content array cannot be rebuilt as the server holds
	// it, and is "" while it can.
	lost string
}

// streamedBlock is one content block being put together from its deltas.
type streamedBlock struct {
	// fields is the block as content_block_start gave it. The deltas are
	// folded into it when the stream ends.
	fields map[string]json.RawMessage
	typ    string

	// text, thinking and input are what the deltas have added up to. Each
	// is nil until its first delta, and a field no delta touched stays as
	// content_block_start gave it.
	text, thinking, input []byte

	// call is this block's place among the reply's tool calls, for a client
	// tool call, and announced whether fn has been given its id and name.
	call      int
	announced bool
}

// readStream reads the events of resp into a Response, calling fn for each
// increment. body is resp's body under the size bound.
func (c *Client) readStream(ctx context.Context, resp *http.Response, body io.Reader, fn func(llm.Delta) error) (*llm.Response, error) {
	events := sse.NewReader(body)
	s := &streamed{blocks: make(map[int]*streamedBlock)}
	for {
		ev, err := events.Next()
		if err != nil {
			// The stream is over and message_stop never came. Whether the
			// connection dropped or the server stopped early, the reply is
			// not whole and the call has failed.
			if errors.Is(err, io.EOF) {
				err = fmt.Errorf("the stream ended before message_stop: %w", io.ErrUnexpectedEOF)
			}
			return nil, readError(ctx, resp, err)
		}

		// Every event repeats its name as the type in its data, so a stream
		// whose event lines were lost on the way still reads.
		kind := ev.Name
		if kind == "" {
			var typed struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(ev.Data, &typed)
			kind = typed.Type
		}
		switch kind {
		case eventMessageStart, eventContentBlockStart, eventContentBlockDelta,
			eventContentBlockStop, eventMessageDelta, eventMessageStop, eventError:
		default:
			continue
		}

		var e streamEvent
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return nil, replyError(resp, "the stream's %s event is not valid: %v", kind, err)
		}
		switch kind {
		case eventMessageStart:
			if e.Message != nil {
				s.started = true
				s.msg.ID, s.msg.Model, s.msg.Usage = e.Message.ID, e.Message.Model, e.Message.Usage
			}
		case eventContentBlockStart:
			if err := s.open(e); err != nil {
				return nil, replyError(resp, "the stream's %s event is not valid: %v", kind, err)
			}
		case eventContentBlockDelta:
			b, ok := s.blocks[e.Index]
			if !ok {
				return nil, replyError(resp, "the stream sent a delta for content block %d, which it never started", e.Index)
			}
			if err := s.apply(b, e.Delta, fn); err != nil {
				return nil, err
			}
		case eventContentBlockStop:
			// A call that streamed no arguments has not been announced yet.
			if b, ok := s.blocks[e.Index]; ok && b.typ == blockToolUse && !b.announced {
				if err := fn(llm.Delta{ToolCall: b.announce("")}); err != nil {
					return nil, err
				}
			}
		case eventMessageDelta:
			s.finish(e)
		case eventMessageStop:
			if !s.started {
				return nil, replyError(resp, "the stream ended without a message_start event")
			}
			// The reply is complete. Whatever the connection does after
			// this, there is nothing left to read.
			return s.response(ctx, c), nil
		case eventError:
			return nil, streamError(resp, e.Error)
		}
	}
}

// open starts the block a content_block_start event describes.
func (s *streamed) open(e streamEvent) error {
	if _, ok := s.blocks[e.Index]; ok {
		return fmt.Errorf("content block %d started twice", e.Index)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(e.ContentBlock, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("content block %d is not an object", e.Index)
	}

	b := &streamedBlock{fields: fields, typ: str(fields["type"])}
	switch b.typ {
	case blockToolUse:
		b.call = s.calls
		s.calls++
	case blockFallback:
		// message_start named the model that then declined. The one that
		// carries on is the one this block hands over to.
		if to := readBlock(nil, fields).toModel; to != "" {
			s.msg.Model = to
		}
	}
	s.blocks[e.Index] = b
	return nil
}

// apply folds one delta into its block and tells fn about it.
func (s *streamed) apply(b *streamedBlock, raw json.RawMessage, fn func(llm.Delta) error) error {
	// A delta is read field by field, so that one of a type added later,
	// whatever its fields hold, cannot fail to decode.
	var delta map[string]json.RawMessage
	_ = json.Unmarshal(raw, &delta)

	switch kind := str(delta["type"]); kind {
	case "text_delta":
		text := str(delta["text"])
		b.text = extend(b.text, b.fields["text"], text)
		if text != "" {
			return fn(llm.Delta{Text: text})
		}
	case "input_json_delta":
		part := str(delta["partial_json"])
		b.input = extend(b.input, nil, part)
		if b.typ == blockToolUse {
			return fn(llm.Delta{ToolCall: b.announce(part)})
		}
	case "thinking_delta":
		thinking := str(delta["thinking"])
		b.thinking = extend(b.thinking, b.fields["thinking"], thinking)
		if thinking != "" {
			return fn(llm.Delta{Reasoning: thinking})
		}
	case "signature_delta":
		b.fields["signature"] = quote(str(delta["signature"]))
	default:
		if s.lost == "" {
			s.lost = fmt.Sprintf("the stream sent a delta of type %q, which this package does not know", kind)
		}
	}
	return nil
}

// extend adds part to a field the deltas build up. The field starts from
// the string content_block_start gave it, and is not nil from its first
// delta on, even an empty one.
func extend(field []byte, start json.RawMessage, part string) []byte {
	if field == nil {
		field = append(make([]byte, 0, len(part)), str(start)...)
	}
	return append(field, part...)
}

// announce builds the delta for a piece of a client tool call's arguments.
// The first one for a call carries its id and name.
func (b *streamedBlock) announce(part string) *llm.ToolCallDelta {
	d := &llm.ToolCallDelta{Index: b.call, InputJSON: part}
	if !b.announced {
		b.announced = true
		d.ID, d.Name = str(b.fields["id"]), str(b.fields["name"])
	}
	return d
}

// finish takes what a message_delta event says about how the reply ended.
func (s *streamed) finish(e streamEvent) {
	var delta struct {
		StopReason  *string          `json:"stop_reason"`
		StopDetails *wireStopDetails `json:"stop_details"`
	}
	_ = json.Unmarshal(e.Delta, &delta)
	if delta.StopReason != nil {
		s.msg.StopReason = *delta.StopReason
	}
	if delta.StopDetails != nil {
		s.msg.StopDetails = delta.StopDetails
	}
	if e.Usage != nil {
		s.msg.Usage.merge(*e.Usage)
	}
}

// response builds the reply once message_stop has arrived.
func (s *streamed) response(ctx context.Context, c *Client) *llm.Response {
	blocks := make([]block, 0, len(s.blocks))
	for _, index := range slices.Sorted(maps.Keys(s.blocks)) {
		b, malformed := s.blocks[index].block()
		if malformed && s.lost == "" {
			s.lost = fmt.Sprintf("the arguments of tool call %q are not valid JSON", b.id)
		}
		blocks = append(blocks, b)
	}

	resp := s.msg.response(blocks, nil)
	if s.lost != "" {
		// The content array would not be the one the server holds, and
		// sending back a different one is worse than sending none: the turn
		// is replayed from its text and tool calls instead.
		resp.Message.Opaque = nil
		c.logger.WarnContext(ctx, "anthropic: a streamed reply cannot be rebuilt as it was sent, so it has no provider form to replay",
			"reason", s.lost, "message_id", resp.ID, "model", resp.Model)
	}
	return resp
}

// block folds the deltas into the block content_block_start gave and returns
// it as a content block. malformed reports a tool call, the client's or the
// server's, whose arguments did not add up to a JSON object.
func (b *streamedBlock) block() (out block, malformed bool) {
	if b.text != nil {
		b.fields["text"] = quote(string(b.text))
	}
	if b.thinking != nil {
		b.fields["thinking"] = quote(string(b.thinking))
	}
	if b.input != nil {
		input := bytes.TrimSpace(b.input)
		switch {
		case len(input) == 0:
			b.fields["input"] = emptyObject
		case input[0] == '{' && json.Valid(input):
			b.fields["input"] = b.input
		default:
			// Cut short, by max_tokens for one. A string keeps what arrived
			// and keeps the message something json.Marshal accepts.
			b.fields["input"] = quote(string(b.input))
			malformed = true
		}
	}

	names := slices.Sorted(maps.Keys(b.fields))
	fields := make([]field, len(names))
	for i, name := range names {
		fields[i] = field{name: name, value: b.fields[name]}
	}
	out = readBlock(object(fields...), b.fields)
	out.malformed = malformed
	return out, malformed
}
