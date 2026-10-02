package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// The delta types this package folds into a block. A block that receives any
// other cannot be rebuilt as the server holds it; citations_delta is one
// such, since this package does not map citations.
const (
	deltaText      = "text_delta"
	deltaInputJSON = "input_json_delta"
	deltaThinking  = "thinking_delta"
	deltaSignature = "signature_delta"
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
	// size is the bytes gathered so far of everything the reply keeps: the
	// message's id and model, each block as it started (a tool call's id
	// and name with it), every delta folded into a block, and how the reply
	// ended.
	size int
	// ending is how many of those bytes the latest message_delta accounts
	// for.
	ending int
	// lost says why the content array cannot be rebuilt as the server holds
	// it, and is "" while it can.
	lost string
}

// grow counts n more bytes that the reply keeps, and fails once it has
// outgrown the bound an unstreamed body is held to. Events that add nothing
// to the reply, keep-alives above all, are not counted: a stream may run as
// long as it likes, and it is what the reply holds in memory that is
// bounded.
func (s *streamed) grow(n int) error {
	s.size += n
	if s.size > maxBodyBytes {
		return sizeError(fmt.Errorf("the streamed reply is larger than %d bytes", maxBodyBytes))
	}
	return nil
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
// increment. watch bounds each wait for the server.
func (c *Client) readStream(ctx context.Context, resp *http.Response, watch *idleWatch, fn func(llm.Delta) error) (*llm.Response, error) {
	events := sse.NewReader(streamBody{r: resp.Body, watch: watch})
	s := &streamed{blocks: make(map[int]*streamedBlock)}
	for {
		// The wait starts afresh here, after fn has returned, so the time
		// the caller took over the last delta is not the server's silence.
		watch.begin(c.idle)
		ev, err := events.Next()
		stalled := watch.end()
		if err != nil || stalled {
			return nil, c.eventFailure(ctx, requestID(resp), err, stalled)
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
				// The message's id and model are kept.
				if err := s.grow(len(ev.Data)); err != nil {
					return nil, err
				}
				s.started = true
				s.msg.ID, s.msg.Model, s.msg.Usage = e.Message.ID, e.Message.Model, e.Message.Usage
			}
		case eventContentBlockStart:
			if err := s.grow(len(e.ContentBlock)); err != nil {
				return nil, err
			}
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
			// How the reply ended is kept too: the explanation of a refusal,
			// the models that worked on it. A later message_delta replaces
			// what an earlier one said, so only the latest is counted.
			if err := s.grow(len(ev.Data) - s.ending); err != nil {
				return nil, err
			}
			s.ending = len(ev.Data)
			s.finish(e)
		case eventMessageStop:
			if !s.started {
				return nil, replyError(resp, "the stream ended without a message_start event")
			}
			// The reply is complete. Whatever the connection does after
			// this cannot fail the call.
			watch.drain(resp.Body)
			return s.response(ctx, c), nil
		case eventError:
			return nil, sentError(resp, e.Error)
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

	kind := str(delta["type"])
	var part string
	switch kind {
	case deltaText:
		part = str(delta["text"])
	case deltaInputJSON:
		part = str(delta["partial_json"])
	case deltaThinking:
		part = str(delta["thinking"])
	case deltaSignature:
		part = str(delta["signature"])
	default:
		if s.lost == "" {
			s.lost = fmt.Sprintf("the stream sent a delta of type %q, which this package does not know", kind)
		}
		return nil
	}
	if err := s.grow(len(part)); err != nil {
		return err
	}

	switch kind {
	case deltaText:
		b.text = extend(b.text, b.fields["text"], part)
		if part != "" {
			return fn(llm.Delta{Text: part})
		}
	case deltaInputJSON:
		b.input = extend(b.input, nil, part)
		if b.typ == blockToolUse {
			return fn(llm.Delta{ToolCall: b.announce(part)})
		}
	case deltaThinking:
		b.thinking = extend(b.thinking, b.fields["thinking"], part)
		if part != "" {
			return fn(llm.Delta{Reasoning: part})
		}
	case deltaSignature:
		b.fields["signature"] = quote(part)
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
	// A refused reply has no provider form whatever the stream held, so
	// there is nothing lost to warn about.
	if s.lost != "" && resp.Stop != llm.StopRefusal {
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
