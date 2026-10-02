package anthropic

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/ManavA/keel/llm"
)

// The roles and block types of the Messages API that this package writes or
// reads.
const (
	roleUser      = "user"
	roleAssistant = "assistant"

	blockText             = "text"
	blockToolUse          = "tool_use"
	blockToolResult       = "tool_result"
	blockThinking         = "thinking"
	blockRedactedThinking = "redacted_thinking"
	blockConnectorText    = "connector_text"
	blockServerToolUse    = "server_tool_use"
	blockFallback         = "fallback"
)

// emptyObject is the arguments of a tool call that has none. Each call gets
// its own: the bytes are handed to callers, and one who edited a shared slice
// in place would change every other call's arguments.
func emptyObject() json.RawMessage { return json.RawMessage(`{}`) }

// unsendable reports a request this package will not send, because the API
// would refuse it or it cannot be written. No call was made, and the same
// request would be refused again.
func unsendable(format string, args ...any) *llm.Error {
	return &llm.Error{Provider: Name, Message: fmt.Sprintf(format, args...)}
}

// field is one member of a JSON object written by object.
type field struct {
	name  string
	value json.RawMessage
}

// object writes fields as one JSON object, each value as the bytes given.
//
// The request body is put together with this and not with json.Marshal. An
// assistant turn's Opaque.Data must arrive as the bytes it was stored as,
// and json.Marshal compacts a RawMessage and rewrites <, > and & inside it.
// Every other piece of JSON a caller supplies is written the same way, after
// json.Valid has passed it.
func object(fields ...field) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(quote(f.name))
		b.WriteByte(':')
		b.Write(f.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// array writes items as one JSON array, each as the bytes given.
func array(items []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(item)
	}
	b.WriteByte(']')
	return b.Bytes()
}

// encode is json.Marshal without its escaping of <, > and &, which the API
// has no use for and which makes a body harder to read in a log.
func encode(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

// quote writes s as a JSON string.
func quote(s string) json.RawMessage {
	// Encoding a string cannot fail: bytes that are not UTF-8 are replaced.
	out, _ := encode(s)
	return out
}

// body builds the request body for req. The fields are written in a fixed
// order, so the same request is the same bytes every time.
func (c *Client) body(req llm.Request, stream bool) ([]byte, error) {
	maxTokens := cmp.Or(req.MaxTokens, c.maxTokens, defaultMaxTokens)
	if stream {
		maxTokens = cmp.Or(req.MaxTokens, c.maxTokens, defaultStreamMaxTokens)
	}
	fields := []field{
		{"model", quote(cmp.Or(req.Model, c.model))},
		{"max_tokens", json.RawMessage(strconv.Itoa(maxTokens))},
	}
	if req.System != "" {
		fields = append(fields, field{"system", quote(req.System)})
	}

	msgs, err := messages(req.Messages)
	if err != nil {
		return nil, unsendable("%v", err)
	}
	fields = append(fields, field{"messages", msgs})

	if len(req.Tools) > 0 {
		defs, err := tools(req.Tools)
		if err != nil {
			return nil, unsendable("%v", err)
		}
		fields = append(fields, field{"tools", defs})
	}
	switch req.ToolChoice {
	case llm.ToolChoiceAuto:
	case llm.ToolChoiceNone:
		// With no tools there is nothing to choose among, and the field has
		// no meaning without them.
		if len(req.Tools) > 0 {
			fields = append(fields, field{"tool_choice", object(field{"type", quote("none")})})
		}
	default:
		return nil, unsendable("unknown tool choice %q", req.ToolChoice)
	}

	var config []field
	if req.Effort != "" {
		config = append(config, field{"effort", quote(string(req.Effort))})
	}
	if req.Output != nil {
		if !json.Valid(req.Output.JSON) {
			return nil, unsendable("the output schema is not valid JSON")
		}
		config = append(config, field{"format", object(
			field{"type", quote("json_schema")},
			field{"schema", req.Output.JSON},
		)})
	}
	if len(config) > 0 {
		fields = append(fields, field{"output_config", object(config...)})
	}

	if req.Temperature != nil {
		value, err := encode(*req.Temperature)
		if err != nil {
			return nil, unsendable("temperature: %v", err)
		}
		fields = append(fields, field{"temperature", value})
	}
	if len(req.Stop) > 0 {
		stops := make([]json.RawMessage, len(req.Stop))
		for i, s := range req.Stop {
			stops[i] = quote(s)
		}
		fields = append(fields, field{"stop_sequences", array(stops)})
	}
	if c.fallbacks {
		fields = append(fields, field{"fallbacks", quote(refusalFallbackDefault)})
	}
	if stream {
		fields = append(fields, field{"stream", json.RawMessage(`true`)})
	}

	for _, extra := range c.extra {
		fields = set(fields, extra)
	}
	return object(fields...), nil
}

// set replaces the field named as f, or adds f when there is none.
func set(fields []field, f field) []field {
	for i := range fields {
		if fields[i].name == f.name {
			fields[i].value = f.value
			return fields
		}
	}
	return append(fields, f)
}

// messages writes the conversation as the messages array.
//
// A turn the API would refuse is not sent. An assistant turn with nothing in
// it is left out: a refused reply is one, and so is a reply the model ended
// with no content, and an empty content array is an error to the API. The
// user turns on either side of it then follow each other, which the API
// reads as one turn. A user turn with no text and a tool turn with no
// results cannot be left out without changing what the conversation says, so
// they are errors here, before any call is made.
func messages(msgs []llm.Message) (json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(msgs))
	for i, m := range msgs {
		role, content, err := message(m)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		if content == nil {
			continue
		}
		out = append(out, object(field{"role", quote(role)}, field{"content", content}))
	}
	return array(out), nil
}

// message gives one message's role and content array. The content is nil for
// an assistant turn with nothing in it, which is not sent.
func message(m llm.Message) (role string, content json.RawMessage, err error) {
	switch m.Role {
	case llm.RoleUser:
		if m.Text == "" {
			return "", nil, errors.New("a user turn has no text")
		}
		return roleUser, array([]json.RawMessage{textBlock(m.Text)}), nil

	case llm.RoleAssistant:
		if data, ok := replayable(m.Opaque); ok {
			empty, err := contentArray(data)
			switch {
			case err != nil:
				return "", nil, err
			case empty:
				return roleAssistant, nil, nil
			}
			return roleAssistant, data, nil
		}
		if m.Text == "" && len(m.ToolCalls) == 0 {
			return roleAssistant, nil, nil
		}
		content, err := rebuilt(m)
		return roleAssistant, content, err

	case llm.RoleTool:
		if len(m.ToolResults) == 0 {
			return "", nil, errors.New("a tool turn has no results")
		}
		// The API takes tool results as a user message and wants them ahead
		// of anything else in it. This one holds nothing else.
		results := make([]json.RawMessage, len(m.ToolResults))
		for i, r := range m.ToolResults {
			result := []field{
				{"type", quote(blockToolResult)},
				{"tool_use_id", quote(r.CallID)},
				{"content", quote(r.Content)},
			}
			if r.IsError {
				result = append(result, field{"is_error", json.RawMessage(`true`)})
			}
			results[i] = object(result...)
		}
		return roleUser, array(results), nil

	default:
		return "", nil, fmt.Errorf("unknown role %q", m.Role)
	}
}

// contentArray checks that the provider's form of a turn is what the API
// takes as content, a JSON array, and reports whether it is an empty one.
func contentArray(data json.RawMessage) (empty bool, err error) {
	data = bytes.TrimSpace(data)
	switch {
	case !json.Valid(data):
		return false, errors.New("the provider's form of the turn is not valid JSON")
	case data[0] != '[':
		return false, errors.New("the provider's form of the turn is not a JSON array")
	}
	return len(bytes.TrimSpace(data[1:len(data)-1])) == 0, nil
}

func textBlock(text string) json.RawMessage {
	return object(field{"type", quote(blockText)}, field{"text", quote(text)})
}

// replayable returns the content array to send back unchanged, when o is
// this provider's own form of a turn and holds one.
func replayable(o *llm.Opaque) (json.RawMessage, bool) {
	if o == nil || o.Provider != Name || absent(o.Data) {
		return nil, false
	}
	return o.Data, true
}

// absent reports whether raw holds no JSON value, or null. A nil RawMessage
// that has been through a journal comes back as null.
func absent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

// rebuilt builds an assistant turn's content from its text and tool calls,
// for a turn that has no form of this provider's to replay.
func rebuilt(m llm.Message) (json.RawMessage, error) {
	blocks := make([]json.RawMessage, 0, 1+len(m.ToolCalls))
	if m.Text != "" {
		blocks = append(blocks, textBlock(m.Text))
	}
	for _, call := range m.ToolCalls {
		input := call.Input
		// The API takes an object here. Arguments that never parsed have
		// none to offer, and a call with no arguments is an empty one.
		if call.Malformed || absent(input) {
			input = emptyObject()
		}
		if !json.Valid(input) {
			return nil, fmt.Errorf("the arguments of tool call %q are not valid JSON", call.ID)
		}
		blocks = append(blocks, object(
			field{"type", quote(blockToolUse)},
			field{"id", quote(call.ID)},
			field{"name", quote(call.Name)},
			field{"input", input},
		))
	}
	return array(blocks), nil
}

// tools writes the tool definitions, in the order given.
func tools(defs []llm.Tool) (json.RawMessage, error) {
	out := make([]json.RawMessage, len(defs))
	for i, t := range defs {
		def := []field{{"name", quote(t.Name)}}
		if t.Description != "" {
			def = append(def, field{"description", quote(t.Description)})
		}
		schema := t.Schema
		if absent(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("tools: the schema of %q is not valid JSON", t.Name)
		}
		def = append(def, field{"input_schema", schema})
		if t.Strict {
			def = append(def, field{"strict", json.RawMessage(`true`)})
		}
		out[i] = object(def...)
	}
	return array(out), nil
}
