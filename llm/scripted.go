package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"unicode"
)

// defaultScriptedName is what a Scripted reports as Response.Model when
// neither the request nor its options name a model.
const defaultScriptedName = "scripted"

// Reply is one scripted turn.
type Reply struct {
	Text      string
	ToolCalls []ToolCall
	// Stop defaults to StopToolUse when ToolCalls is set and StopEnd otherwise.
	Stop StopReason
	// Usage defaults to a count derived from the request and reply lengths,
	// so the same script always costs the same.
	Usage Usage
	// Err, when set, is returned instead of a reply.
	Err error
}

// Script chooses the reply for a request. turn is how many assistant turns
// the request already holds, so the choice depends on the request alone and
// a fresh Scripted in a restarted process gives the same answers.
type Script func(req Request, turn int) (Reply, error)

// Replies is a Script that plays replies in order, one per turn.
func Replies(replies ...Reply) Script {
	replies = slices.Clone(replies)
	return func(_ Request, turn int) (Reply, error) {
		if turn < 0 || turn >= len(replies) {
			return Reply{}, fmt.Errorf("%w (turn %d, script has %d)", ErrScriptExhausted, turn, len(replies))
		}
		return replies[turn], nil
	}
}

// Route is a Script that picks another by Request.Model. The "" entry is
// used for a model the map does not name.
func Route(byModel map[string]Script) Script {
	byModel = maps.Clone(byModel)
	return func(req Request, turn int) (Reply, error) {
		script, ok := byModel[req.Model]
		if !ok {
			script, ok = byModel[""]
		}
		if !ok {
			return Reply{}, fmt.Errorf("%w (no script for model %q)", ErrScriptExhausted, req.Model)
		}
		return script(req, turn)
	}
}

// ScriptedOptions configures a Scripted. The zero value works.
type ScriptedOptions struct {
	// Name is reported as Response.Model when the request names none.
	// Default "scripted".
	Name string
}

// Scripted is the in-process Model: it answers from a Script, with no network.
//
// It is safe for concurrent use, provided its Script is.
type Scripted struct {
	script Script
	name   string

	mu       sync.Mutex
	requests []Request
}

var _ Model = (*Scripted)(nil)

// NewScripted builds a Scripted over script.
func NewScripted(script Script, opts ScriptedOptions) *Scripted {
	name := opts.Name
	if name == "" {
		name = defaultScriptedName
	}
	return &Scripted{script: script, name: name}
}

// Generate implements Model.
func (s *Scripted) Generate(ctx context.Context, req Request) (*Response, error) {
	return s.respond(ctx, req)
}

// Stream implements Model. It sends the text a word at a time, each word with
// the white space after it, and then each tool call as one delta.
func (s *Scripted) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error) {
	resp, err := s.respond(ctx, req)
	if err != nil {
		return nil, err
	}
	send := func(d Delta) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(d)
	}
	for _, word := range words(resp.Message.Text) {
		if err := send(Delta{Text: word}); err != nil {
			return nil, err
		}
	}
	for i, c := range resp.Message.ToolCalls {
		d := Delta{ToolCall: &ToolCallDelta{Index: i, ID: c.ID, Name: c.Name, InputJSON: argumentText(c)}}
		if err := send(d); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// Requests returns every request received, in order, including those whose
// call failed. A call made with a context that had already ended never
// started, so it is not among them. The requests are copies.
func (s *Scripted) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	for i, r := range s.requests {
		out[i] = cloneRequest(r)
	}
	return out
}

// respond is the one call Generate and Stream share.
func (s *Scripted) respond(ctx context.Context, req Request) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.requests = append(s.requests, cloneRequest(req))
	s.mu.Unlock()

	turn := assistantTurns(req)
	reply, err := s.script(req, turn)
	if err != nil {
		return nil, err
	}
	if reply.Err != nil {
		return nil, reply.Err
	}

	var calls []ToolCall
	if len(reply.ToolCalls) > 0 {
		calls = make([]ToolCall, len(reply.ToolCalls))
	}
	for i, c := range reply.ToolCalls {
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d_%d", turn, i)
		}
		if len(c.Input) == 0 {
			c.Input = json.RawMessage(`{}`)
		} else {
			c.Input = bytes.Clone(c.Input)
		}
		calls[i] = c
	}

	stop := reply.Stop
	if stop == "" {
		stop = StopEnd
		if len(calls) > 0 {
			stop = StopToolUse
		}
	}
	usage := reply.Usage
	if usage == (Usage{}) {
		usage = defaultUsage(req, reply.Text, calls)
	}
	model := req.Model
	if model == "" {
		model = s.name
	}

	resp := &Response{
		Model:   model,
		Message: Message{Role: RoleAssistant, Text: reply.Text, ToolCalls: calls},
		Stop:    stop,
		Usage:   usage,
	}
	if stop == StopRefusal {
		resp.Refusal = &Refusal{}
	}
	return resp, nil
}

// assistantTurns is how many assistant messages req already holds.
func assistantTurns(req Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role == RoleAssistant {
			n++
		}
	}
	return n
}

// defaultUsage is the count a reply costs when its script gave none: the
// estimate for the request in, and one token per four bytes of text and tool
// arguments, plus one, out.
func defaultUsage(req Request, text string, calls []ToolCall) Usage {
	n := len(text)
	for _, c := range calls {
		n += len(c.Input)
	}
	return Usage{InputTokens: EstimateInputTokens(req), OutputTokens: int64(n/4 + 1)}
}

// words splits s into pieces that add up to s, each a run of text and the
// white space after it. Any white space at the start is a piece of its own.
func words(s string) []string {
	var out []string
	start := 0
	inSpace := false
	for i, r := range s {
		space := unicode.IsSpace(r)
		if inSpace && !space {
			out = append(out, s[start:i])
			start = i
		}
		inSpace = space
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// argumentText is a call's arguments as a stream would carry them: the text
// the model wrote, which for a malformed call is the string Input holds.
func argumentText(c ToolCall) string {
	if c.Malformed {
		var written string
		if json.Unmarshal(c.Input, &written) == nil {
			return written
		}
	}
	return string(c.Input)
}

// cloneRequest copies r deeply enough that neither side can change the other.
func cloneRequest(r Request) Request {
	r.Messages = slices.Clone(r.Messages)
	for i, m := range r.Messages {
		m.ToolCalls = slices.Clone(m.ToolCalls)
		for j, c := range m.ToolCalls {
			m.ToolCalls[j].Input = bytes.Clone(c.Input)
		}
		m.ToolResults = slices.Clone(m.ToolResults)
		if m.Opaque != nil {
			o := *m.Opaque
			o.Data = bytes.Clone(o.Data)
			m.Opaque = &o
		}
		r.Messages[i] = m
	}
	r.Tools = slices.Clone(r.Tools)
	for i, t := range r.Tools {
		r.Tools[i].Schema = bytes.Clone(t.Schema)
	}
	if r.Output != nil {
		o := *r.Output
		o.JSON = bytes.Clone(o.JSON)
		r.Output = &o
	}
	if r.Temperature != nil {
		t := *r.Temperature
		r.Temperature = &t
	}
	r.Stop = slices.Clone(r.Stop)
	return r
}
