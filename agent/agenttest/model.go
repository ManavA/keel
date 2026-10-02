package agenttest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/ManavA/keel/agent"
)

// scriptedModel is what a Model reports as the model that answered when
// neither the script nor the request names one.
const scriptedModel = "scripted"

// Script chooses the reply to a request. turn is how many assistant turns
// the request already holds.
type Script func(req agent.Request, turn int) (agent.Response, error)

// Replies is a Script that plays replies in order, one per turn. A turn past
// the last reply is an error, and not one that wraps agent.ErrPermanent: an
// engine treats it as it would any model failure.
func Replies(replies ...agent.Response) Script {
	return func(_ agent.Request, turn int) (agent.Response, error) {
		if turn < 0 || turn >= len(replies) {
			return agent.Response{}, fmt.Errorf("agenttest: script has no reply for turn %d", turn)
		}
		return cloneResponse(replies[turn]), nil
	}
}

// ByAgent is a Script that picks another by Request.Agent. An agent the map
// does not name is an error.
func ByAgent(scripts map[string]Script) Script {
	return func(req agent.Request, turn int) (agent.Response, error) {
		script, ok := scripts[req.Agent]
		if !ok {
			return agent.Response{}, fmt.Errorf("agenttest: no script for agent %q", req.Agent)
		}
		return script(req, turn)
	}
}

// Say is a final answer.
func Say(text string) agent.Response {
	return agent.Response{
		Message: agent.Message{Role: agent.RoleAssistant, Text: text},
		Stop:    agent.StopEnd,
	}
}

// Use is a turn that calls tools.
func Use(calls ...agent.Call) agent.Response {
	return agent.Response{
		Message: agent.Message{Role: agent.RoleAssistant, Calls: calls},
		Stop:    agent.StopToolUse,
	}
}

// Call builds a tool call from its id, tool name and JSON arguments. No
// arguments are the empty object. Arguments that are not valid JSON make a
// malformed call, which holds them as one JSON string, the way a provider
// adapter reports arguments it could not parse.
func Call(id, name, input string) agent.Call {
	call := agent.Call{ID: id, Name: name}
	switch {
	case input == "":
		call.Input = json.RawMessage(`{}`)
	case json.Valid([]byte(input)):
		call.Input = json.RawMessage(input)
	default:
		// A string always marshals.
		call.Input, _ = json.Marshal(input)
		call.Malformed = true
	}
	return call
}

// Model is a scripted agent.Model that records what it was asked. It is safe
// for concurrent use.
type Model struct {
	script Script

	mu       sync.Mutex
	requests []agent.Request
}

// NewModel builds a Model over script.
func NewModel(script Script) *Model {
	return &Model{script: script}
}

// Generate implements agent.Model. The reply is the script's for the request
// and the number of assistant turns in it; when the script names no model,
// Response.Model is Request.Model, or "scripted" when that is empty too. A
// context already done is not answered and not recorded.
func (m *Model) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	if err := ctx.Err(); err != nil {
		return agent.Response{}, err
	}

	m.mu.Lock()
	m.requests = append(m.requests, cloneRequest(req))
	m.mu.Unlock()

	turn := 0
	for _, msg := range req.Messages {
		if msg.Role == agent.RoleAssistant {
			turn++
		}
	}
	resp, err := m.script(req, turn)
	if err != nil {
		return agent.Response{}, err
	}
	if resp.Model == "" {
		resp.Model = req.Model
	}
	if resp.Model == "" {
		resp.Model = scriptedModel
	}
	return resp, nil
}

// Requests returns every request received, in order. Each is as it was when
// it arrived, whatever its sender has done to it since.
func (m *Model) Requests() []agent.Request {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]agent.Request, len(m.requests))
	for i, req := range m.requests {
		out[i] = cloneRequest(req)
	}
	return out
}

// GuardFunc adapts a function to agent.Guard.
type GuardFunc func(ctx context.Context, a agent.Action) (agent.Decision, error)

// Decide implements agent.Guard.
func (f GuardFunc) Decide(ctx context.Context, a agent.Action) (agent.Decision, error) {
	return f(ctx, a)
}

var (
	_ agent.Model = (*Model)(nil)
	_ agent.Guard = GuardFunc(nil)
)

func cloneRequest(req agent.Request) agent.Request {
	if req.Messages != nil {
		messages := make([]agent.Message, len(req.Messages))
		for i, msg := range req.Messages {
			messages[i] = cloneMessage(msg)
		}
		req.Messages = messages
	}
	if req.Tools != nil {
		tools := make([]agent.ToolSpec, len(req.Tools))
		for i, tool := range req.Tools {
			tool.Schema = slices.Clone(tool.Schema)
			tools[i] = tool
		}
		req.Tools = tools
	}
	req.Output = slices.Clone(req.Output)
	return req
}

func cloneResponse(resp agent.Response) agent.Response {
	resp.Message = cloneMessage(resp.Message)
	return resp
}

func cloneMessage(msg agent.Message) agent.Message {
	if msg.Calls != nil {
		calls := make([]agent.Call, len(msg.Calls))
		for i, call := range msg.Calls {
			call.Input = slices.Clone(call.Input)
			calls[i] = call
		}
		msg.Calls = calls
	}
	msg.Results = slices.Clone(msg.Results)
	if msg.Opaque != nil {
		opaque := *msg.Opaque
		opaque.Data = slices.Clone(opaque.Data)
		msg.Opaque = &opaque
	}
	return msg
}
