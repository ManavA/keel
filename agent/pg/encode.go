package pg

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/ManavA/keel/agent"
)

// The JSON columns hold values that carry JSON somebody else wrote: a call's
// arguments, a turn in its provider's own form, a tool's schema. encoding/json
// rewrites a json.RawMessage as it marshals one: it drops the space between
// tokens and escapes <, > and &. A json column keeps the text it is given, so
// the encoders below write the object around each raw value themselves and
// put the raw value in as the bytes it came as. What reads a column back is
// encoding/json, which hands a raw value over as the bytes in the text.
//
// Each encoder writes the fields its type's JSON tags name, and leaves out
// the ones those tags leave out, so that json.Unmarshal reads back what
// json.Marshal would have written. A test holds each to that, with every
// field of the type set.

// jsonObject builds one JSON object, field by field.
type jsonObject struct {
	buf []byte
	err error
}

func (o *jsonObject) key(name string) {
	if len(o.buf) == 0 {
		o.buf = append(o.buf, '{')
	} else {
		o.buf = append(o.buf, ',')
	}
	o.buf = append(o.buf, '"')
	o.buf = append(o.buf, name...)
	o.buf = append(o.buf, '"', ':')
}

// str writes a string field. A NUL goes in as the escape JSON has for it,
// and a byte that is not UTF-8 as the replacement character.
func (o *jsonObject) str(name, value string) {
	o.key(name)
	o.buf = appendJSONString(o.buf, value)
}

func (o *jsonObject) num(name string, value int64) {
	o.key(name)
	o.buf = strconv.AppendInt(o.buf, value, 10)
}

func (o *jsonObject) flag(name string) {
	o.key(name)
	o.buf = append(o.buf, "true"...)
}

// raw writes a field whose value is JSON already, as the bytes given. None
// is written as null.
func (o *jsonObject) raw(name string, value json.RawMessage) {
	o.key(name)
	if len(value) == 0 {
		o.buf = append(o.buf, "null"...)
		return
	}
	if err := validRaw(value); err != nil && o.err == nil {
		o.err = fmt.Errorf("%s: %w", name, err)
	}
	o.buf = append(o.buf, value...)
}

// list writes a field whose value is a list of objects already built.
func (o *jsonObject) list(name string, items []string) {
	o.key(name)
	o.buf = append(o.buf, '[')
	for i, item := range items {
		if i > 0 {
			o.buf = append(o.buf, ',')
		}
		o.buf = append(o.buf, item...)
	}
	o.buf = append(o.buf, ']')
}

func (o *jsonObject) object(name, value string) {
	o.key(name)
	o.buf = append(o.buf, value...)
}

func (o *jsonObject) done() (string, error) {
	if o.err != nil {
		return "", o.err
	}
	if len(o.buf) == 0 {
		return "{}", nil
	}
	return string(append(o.buf, '}')), nil
}

func appendJSONString(buf []byte, s string) []byte {
	// A string always marshals.
	quoted, _ := json.Marshal(s)
	return append(buf, quoted...)
}

// validRaw reports why a json column could not keep value. Postgres wants
// JSON, and wants it in UTF-8, which encoding/json does not check.
func validRaw(value json.RawMessage) error {
	if !json.Valid(value) {
		return errors.New("not valid JSON")
	}
	if !utf8.Valid(value) {
		return errors.New("not UTF-8")
	}
	return nil
}

// encodeCall is c as agent_steps.call holds it.
func encodeCall(c agent.Call) (string, error) {
	var o jsonObject
	o.str("id", c.ID)
	o.str("name", c.Name)
	o.raw("input", c.Input)
	if c.Malformed {
		o.flag("malformed")
	}
	return o.done()
}

// encodeMessage is m as agent_steps.message holds it.
func encodeMessage(m agent.Message) (string, error) {
	var o jsonObject
	o.str("role", string(m.Role))
	if m.Text != "" {
		o.str("text", m.Text)
	}
	if len(m.Calls) > 0 {
		calls := make([]string, len(m.Calls))
		for i, c := range m.Calls {
			call, err := encodeCall(c)
			if err != nil {
				return "", fmt.Errorf("call %d: %w", i+1, err)
			}
			calls[i] = call
		}
		o.list("calls", calls)
	}
	if len(m.Results) > 0 {
		results := make([]string, len(m.Results))
		for i, r := range m.Results {
			var result jsonObject
			result.str("call_id", r.CallID)
			result.str("content", r.Content)
			if r.IsError {
				result.flag("is_error")
			}
			results[i], _ = result.done()
		}
		o.list("results", results)
	}
	if m.Opaque != nil {
		var opaque jsonObject
		opaque.str("provider", m.Opaque.Provider)
		opaque.raw("data", m.Opaque.Data)
		text, err := opaque.done()
		if err != nil {
			return "", fmt.Errorf("opaque: %w", err)
		}
		o.object("opaque", text)
	}
	return o.done()
}

// encodeSnapshot is s as agent_runs.definition holds it.
func encodeSnapshot(s agent.Snapshot) (string, error) {
	var o jsonObject
	o.str("system", s.System)
	if s.Model != "" {
		o.str("model", s.Model)
	}
	if len(s.Tools) > 0 {
		tools := make([]string, len(s.Tools))
		for i, t := range s.Tools {
			var tool jsonObject
			tool.str("name", t.Name)
			if t.Description != "" {
				tool.str("description", t.Description)
			}
			if len(t.Schema) > 0 {
				tool.raw("schema", t.Schema)
			}
			text, err := tool.done()
			if err != nil {
				return "", fmt.Errorf("tool %q: %w", t.Name, err)
			}
			tools[i] = text
		}
		o.list("tools", tools)
	}
	if len(s.Output) > 0 {
		o.raw("output", s.Output)
	}
	if s.MaxTokens != 0 {
		o.num("max_tokens", int64(s.MaxTokens))
	}
	var limits jsonObject
	limits.num("max_duration_ns", int64(s.Limits.MaxDuration))
	limits.num("max_cost_micros", s.Limits.MaxCostMicros)
	limits.num("max_tokens", s.Limits.MaxTokens)
	limits.num("max_model_calls", int64(s.Limits.MaxModelCalls))
	text, _ := limits.done()
	o.object("limits", text)
	return o.done()
}

// encodeMetadata is a run's metadata as agent_runs.metadata holds it: an
// object, and an empty one for none.
func encodeMetadata(metadata map[string]string) (string, error) {
	if len(metadata) == 0 {
		return "{}", nil
	}
	text, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	if text, err = keptJSON(text); err != nil {
		return "", err
	}
	return string(text), nil
}

// encodeAction is a as agent_approvals.action holds it. It fails for
// attributes JSON cannot hold.
func encodeAction(a agent.Action) (string, error) {
	text, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("action attributes: %w", err)
	}
	if text, err = keptJSON(text); err != nil {
		return "", fmt.Errorf("action attributes: %w", err)
	}
	return string(text), nil
}
