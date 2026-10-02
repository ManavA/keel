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

// blockOverhead is what each content block of a stream counts against the
// bound of a reply besides its bytes: the struct it is gathered in, its entry
// in the stream's map of blocks, and the second struct and slice entry it
// becomes when the reply is built. That is about twice what a tool call costs
// the OpenAI provider, which counts 256. Without it a server that opened
// blocks of two bytes without end would be counted by the two bytes.
const blockOverhead = 512

// streamed is a reply being put together from a stream's events.
type streamed struct {
	// msg holds the id, model, stop reason and usage as the events have
	// given them so far.
	msg     wireMessage
	started bool
	blocks  map[int]*streamedBlock
	// calls counts the client tool calls opened so far.
	calls int
	// size is what the reply costs to hold so far, and limit the most it may
	// reach. Everything that is kept is counted, in the form it is kept in:
	// the message_start and message_delta events whole, each block as it
	// started and blockOverhead for it, and every delta as it is written
	// into the block, escapes and all. The count never falls: a later
	// message_delta is added to the earlier ones, since what an earlier one
	// said stays unless the later one says it again.
	size, limit int64
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
	s.size += int64(n)
	if s.size > s.limit {
		return sizeError("the streamed reply", s.limit)
	}
	return nil
}

// streamedBlock is one content block being put together from its deltas.
type streamedBlock struct {
	// start is the block as content_block_start gave it. The deltas are
	// folded into it when the stream ends.
	start json.RawMessage
	typ   string
	// id and name are a tool call's, for the delta that announces it.
	id, name string

	// text and thinking are what the deltas have added up to, held as they
	// will be written: the inside of a JSON string, escapes and all. input
	// is a tool call's arguments as their own JSON text. Each is nil until
	// its first delta, and a field no delta touched stays as
	// content_block_start gave it.
	text, thinking, input []byte
	// signature is the signature a delta gave, as a JSON string, or nil.
	signature json.RawMessage

	// call is this block's place among the reply's tool calls, for a client
	// tool call, and announced whether fn has been given its id and name.
	call      int
	announced bool
	// closed says content_block_stop has come.
	closed bool
}

// readStream reads the events of resp into a Response, calling fn for each
// increment. watch bounds each wait for the server.
func (c *Client) readStream(ctx context.Context, resp *http.Response, watch *idleWatch, fn func(llm.Delta) error) (*llm.Response, error) {
	events := sse.NewReader(streamBody{r: resp.Body, watch: watch})
	s := &streamed{blocks: make(map[int]*streamedBlock), limit: c.bound}
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
			if err := s.grow(blockOverhead + len(e.ContentBlock)); err != nil {
				return nil, err
			}
			if err := s.open(e); err != nil {
				return nil, replyError(resp, "the stream's %s event is not valid: %v", kind, err)
			}
		case eventContentBlockDelta:
			b, err := s.openBlock(e.Index, "delta")
			if err != nil {
				return nil, replyError(resp, "%v", err)
			}
			if err := s.apply(b, e.Delta, fn); err != nil {
				return nil, err
			}
		case eventContentBlockStop:
			b, err := s.openBlock(e.Index, "stop")
			if err != nil {
				return nil, replyError(resp, "%v", err)
			}
			b.closed = true
			// A call that streamed no arguments has not been announced yet.
			if b.typ == blockToolUse && !b.announced {
				if err := fn(llm.Delta{ToolCall: b.announce("")}); err != nil {
					return nil, err
				}
			}
		case eventMessageDelta:
			// How the reply ended is kept too: the explanation of a refusal,
			// the models that worked on it.
			if err := s.grow(len(ev.Data)); err != nil {
				return nil, err
			}
			s.finish(e)
		case eventMessageStop:
			if !s.started || s.msg.StopReason == "" {
				// The reference says the stop reason is not null once a
				// stream has ended. A stream that stops without one, or with
				// no message at all, lost its ending on the way, as one cut
				// short did, and the same request may fare better.
				return nil, callError(ctx, requestID(resp),
					fmt.Errorf("the stream reached message_stop with no stop reason before it: %w", io.ErrUnexpectedEOF))
			}
			// The reply is complete. Whatever the connection does after
			// this cannot fail the call.
			watch.drain(resp.Body)
			return s.response(ctx, c, resp)
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
	fields, err := fieldsOf(e.ContentBlock)
	if err != nil {
		return err
	}

	b := &streamedBlock{start: e.ContentBlock, typ: str(fields.Type)}
	switch b.typ {
	case blockToolUse:
		b.id, b.name = str(fields.ID), str(fields.Name)
		b.call = s.calls
		s.calls++
	case blockFallback:
		// message_start named the model that then declined. The one that
		// carries on is the one this block hands over to.
		if read, err := readBlock(e.ContentBlock); err == nil && read.toModel != "" {
			s.msg.Model = read.toModel
		}
	}
	s.blocks[e.Index] = b
	return nil
}

// openBlock returns the block an event of the given kind is for. The block
// must have started and not yet stopped: a stream that says otherwise is not
// one this package can put a reply together from.
func (s *streamed) openBlock(index int, kind string) (*streamedBlock, error) {
	b, ok := s.blocks[index]
	switch {
	case !ok:
		return nil, fmt.Errorf("the stream sent a %s for content block %d, which it never started", kind, index)
	case b.closed:
		return nil, fmt.Errorf("the stream sent a %s for content block %d after stopping it", kind, index)
	}
	return b, nil
}

// apply folds one delta into its block and tells fn about it.
func (s *streamed) apply(b *streamedBlock, raw json.RawMessage, fn func(llm.Delta) error) error {
	// A delta is read field by field, so that one of a type added later,
	// whatever its fields hold, cannot fail to decode.
	var delta struct {
		Type        json.RawMessage `json:"type"`
		Text        json.RawMessage `json:"text"`
		PartialJSON json.RawMessage `json:"partial_json"`
		Thinking    json.RawMessage `json:"thinking"`
		Signature   json.RawMessage `json:"signature"`
	}
	_ = json.Unmarshal(raw, &delta)

	kind := str(delta.Type)
	var part string
	switch kind {
	case deltaText:
		part = str(delta.Text)
	case deltaInputJSON:
		part = str(delta.PartialJSON)
	case deltaThinking:
		part = str(delta.Thinking)
	case deltaSignature:
		part = str(delta.Signature)
	default:
		if s.lost == "" {
			s.lost = fmt.Sprintf("the stream sent a delta of type %q, which this package does not know", kind)
		}
		return nil
	}

	// What is counted is what is kept: the delta as it is written into a
	// JSON string, where a character can take six bytes, and not the bytes
	// it decoded to. Tool arguments are kept as their own JSON text when
	// they turn out to be JSON and as a string when they do not, and the
	// string is the longer.
	written := quote(part)
	inside := written[1 : len(written)-1]
	if err := s.grow(len(inside)); err != nil {
		return err
	}

	var err error
	switch kind {
	case deltaText:
		if b.text == nil {
			if b.text, err = s.begun(b, func(f blockFields) json.RawMessage { return f.Text }); err != nil {
				return err
			}
		}
		b.text = append(b.text, inside...)
		if part != "" {
			return fn(llm.Delta{Text: part})
		}
	case deltaInputJSON:
		if b.input == nil {
			b.input = []byte{}
		}
		b.input = append(b.input, part...)
		if b.typ == blockToolUse {
			return fn(llm.Delta{ToolCall: b.announce(part)})
		}
	case deltaThinking:
		if b.thinking == nil {
			if b.thinking, err = s.begun(b, func(f blockFields) json.RawMessage { return f.Thinking }); err != nil {
				return err
			}
		}
		b.thinking = append(b.thinking, inside...)
		if part != "" {
			return fn(llm.Delta{Reasoning: part})
		}
	case deltaSignature:
		b.signature = written
	}
	return nil
}

// begun returns what a field the deltas build up starts from: the string
// content_block_start gave it, as the inside of a JSON string, and never
// nil. It is counted, since it is written again when the block is rebuilt.
func (s *streamed) begun(b *streamedBlock, pick func(blockFields) json.RawMessage) ([]byte, error) {
	fields, _ := fieldsOf(b.start)
	written := quote(str(pick(fields)))
	inside := written[1 : len(written)-1]
	if err := s.grow(len(inside)); err != nil {
		return nil, err
	}
	return append([]byte{}, inside...), nil
}

// announce builds the delta for a piece of a client tool call's arguments.
// The first one for a call carries its id and name.
func (b *streamedBlock) announce(part string) *llm.ToolCallDelta {
	d := &llm.ToolCallDelta{Index: b.call, InputJSON: part}
	if !b.announced {
		b.announced = true
		d.ID, d.Name = b.id, b.name
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
func (s *streamed) response(ctx context.Context, c *Client, httpResp *http.Response) (*llm.Response, error) {
	blocks := make([]block, 0, len(s.blocks))
	for _, index := range slices.Sorted(maps.Keys(s.blocks)) {
		b, malformed, err := s.blocks[index].block()
		if err != nil {
			return nil, replyError(httpResp, "content block %d of the stream cannot be put together: %v", index, err)
		}
		if malformed && s.lost == "" {
			s.lost = fmt.Sprintf("the arguments of tool call %q are not valid JSON", b.id)
		}
		blocks = append(blocks, b)
	}

	resp, unsigned := s.msg.response(blocks, nil)
	// A refused reply has no provider form whatever the stream held, so
	// there is nothing lost to warn about.
	switch {
	case resp.Stop == llm.StopRefusal:
	case s.lost != "":
		// The content array would not be the one the server holds, and
		// sending back a different one is worse than sending none: the turn
		// is replayed from its text and tool calls instead.
		resp.Message.Opaque = nil
		c.logger.WarnContext(ctx, "anthropic: a streamed reply cannot be rebuilt as it was sent, so it has no provider form to replay",
			"reason", s.lost, "message_id", resp.ID, "model", resp.Model)
	default:
		c.warnUnsigned(ctx, resp, unsigned)
	}
	return resp, nil
}

// block folds the deltas into the block content_block_start gave and returns
// it as a content block. malformed reports a tool call, the client's or the
// server's, whose arguments did not add up to a JSON object.
func (b *streamedBlock) block() (out block, malformed bool, err error) {
	set := make(map[string]json.RawMessage, 4)
	if b.text != nil {
		set["text"] = quoted(b.text)
	}
	if b.thinking != nil {
		set["thinking"] = quoted(b.thinking)
	}
	if b.signature != nil {
		set["signature"] = b.signature
	}
	if b.input != nil {
		input := bytes.TrimSpace(b.input)
		switch {
		case len(input) == 0:
			set["input"] = emptyObject()
		case input[0] == '{' && json.Valid(input):
			set["input"] = b.input
		default:
			// Cut short, by max_tokens for one. A string keeps what arrived
			// and keeps the message something json.Marshal accepts.
			set["input"] = quote(string(b.input))
			malformed = true
		}
	}

	raw, err := rewrite(b.start, set)
	if err != nil {
		return block{}, false, err
	}
	if out, err = readBlock(raw); err != nil {
		return block{}, false, err
	}
	out.malformed = malformed
	return out, malformed, nil
}

// quoted puts quotes round the inside of a JSON string.
func quoted(inside []byte) json.RawMessage {
	out := make([]byte, 0, len(inside)+2)
	out = append(out, '"')
	out = append(out, inside...)
	return append(out, '"')
}

// rewrite returns the JSON object raw with each field named in set given the
// value there, and a field of set that raw lacks added at the end. Every
// other field is written as it was received, in the order it came, so a
// field this package does not know of survives a stream. The fields are
// copied across one at a time and none is held, so a block of many fields
// costs its bytes and no more.
func rewrite(raw json.RawMessage, set map[string]json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("it is not a JSON object")
	}
	var out bytes.Buffer
	out.Grow(len(raw))
	out.WriteByte('{')
	first := true
	write := func(name string, value []byte) {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(quote(name))
		out.WriteByte(':')
		out.Write(value)
	}

	written := make(map[string]bool, len(set))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string)
		// The value is what lies between the end of the name and the end of
		// the value's last token, less the colon and the space before it.
		from := dec.InputOffset()
		if err := skipValue(dec); err != nil {
			return nil, err
		}
		value := bytes.TrimLeft(raw[from:dec.InputOffset()], ": \t\r\n")
		if replacement, ok := set[name]; ok {
			if written[name] {
				continue
			}
			written[name] = true
			value = replacement
		}
		write(name, value)
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		if !written[name] {
			write(name, set[name])
		}
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// skipValue reads one JSON value from dec and keeps none of it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}
