package openai

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ManavA/keel/llm"
)

// The finish reasons the reference lists.
const (
	finishStop          = "stop"
	finishLength        = "length"
	finishToolCalls     = "tool_calls"
	finishFunctionCall  = "function_call"
	finishContentFilter = "content_filter"
)

// wireCompletion is a chat.completion. Every field is optional: a server
// that speaks the protocol does not always send all of them.
type wireCompletion struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []wireChoice    `json:"choices"`
	Usage   *wireUsage      `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type wireChoice struct {
	Message      wireMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type wireMessage struct {
	Content   string         `json:"content"`
	Refusal   string         `json:"refusal"`
	ToolCalls []wireToolCall `json:"tool_calls"`
}

// wireToolCall is a tool call, whole in a completion and in pieces in a
// stream. Function is nil for a call of a kind this package never asks for.
type wireToolCall struct {
	Index    int           `json:"index"`
	ID       string        `json:"id"`
	Function *wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	PromptDetails    *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// usage maps the counts. Cached and cache-written tokens are counted in
// prompt_tokens, so they come out of InputTokens, which is never below zero.
func (u *wireUsage) usage() llm.Usage {
	if u == nil {
		return llm.Usage{}
	}
	out := llm.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if d := u.PromptDetails; d != nil {
		out.CacheReadTokens = d.CachedTokens
		out.CacheWriteTokens = d.CacheWriteTokens
		out.InputTokens = max(0, out.InputTokens-d.CachedTokens-d.CacheWriteTokens)
	}
	if d := u.CompletionDetails; d != nil {
		out.ReasoningTokens = d.ReasoningTokens
	}
	return out
}

// reply is what a completion or a finished stream says, before it is
// mapped. Both paths build their Response from it, so a streamed reply is the
// one the same body would have given.
type reply struct {
	id, model string
	text      string
	refusal   string
	calls     []replyCall
	finish    string
	usage     *wireUsage
}

type replyCall struct {
	id, name, arguments string
}

// completionResponse maps a decoded completion. requested is the model the
// request named, reported when the reply names none.
func (c *Client) completionResponse(resp *http.Response, comp wireCompletion, requested string) (*llm.Response, error) {
	if len(comp.Choices) == 0 {
		if e, ok := errorFields(comp.Error); ok {
			return nil, embeddedError(resp, e)
		}
		return nil, errors.New("openai: response has no choices")
	}
	choice := comp.Choices[0]
	r := reply{
		id:      comp.ID,
		model:   comp.Model,
		text:    choice.Message.Content,
		refusal: choice.Message.Refusal,
		finish:  choice.FinishReason,
		usage:   comp.Usage,
	}
	for _, tc := range choice.Message.ToolCalls {
		if tc.Function == nil {
			continue
		}
		r.calls = append(r.calls, replyCall{id: tc.ID, name: tc.Function.Name, arguments: tc.Function.Arguments})
	}
	return c.response(r, requested), nil
}

// response maps a reply to the llm form.
func (c *Client) response(r reply, requested string) *llm.Response {
	msg := llm.Message{Role: llm.RoleAssistant, Text: r.text}
	for _, call := range r.calls {
		input, malformed := toolInput(call.arguments)
		msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
			ID: call.id, Name: call.name, Input: input, Malformed: malformed,
		})
	}

	model := r.model
	if model == "" {
		model = requested
	}
	stop, refusal := c.stopReason(r.finish, r.refusal, len(r.calls) > 0)
	return &llm.Response{
		ID:      r.id,
		Model:   model,
		Message: msg,
		Stop:    stop,
		Usage:   r.usage.usage(),
		Refusal: refusal,
	}
}

// toolInput is a call's arguments as llm.ToolCall.Input. The reference warns
// they are not always valid JSON: then the text is kept as one JSON string and
// the call is malformed. Nothing at all is an empty object, which is what a
// call with no arguments is.
func toolInput(arguments string) (input json.RawMessage, malformed bool) {
	if strings.TrimSpace(arguments) == "" {
		return json.RawMessage("{}"), false
	}
	if json.Valid([]byte(arguments)) {
		return json.RawMessage(arguments), false
	}
	text, _ := json.Marshal(arguments)
	return text, true
}

// stopReason maps a finish reason, and the refusal a reply may hold. A
// refusal string is a refusal whatever the finish reason says; the content
// filter is one with a category and no explanation. A finish reason that is
// missing, or that this package does not know, is read from what the reply
// holds: a call to answer is a tool use, anything else an end.
func (c *Client) stopReason(finish, refusalText string, hasCalls bool) (llm.StopReason, *llm.Refusal) {
	var refusal *llm.Refusal
	if refusalText != "" {
		refusal = &llm.Refusal{Explanation: refusalText}
	}
	if finish == finishContentFilter {
		if refusal == nil {
			refusal = &llm.Refusal{}
		}
		refusal.Category = finishContentFilter
	}
	if refusal != nil {
		return llm.StopRefusal, refusal
	}

	switch finish {
	case finishStop:
		return llm.StopEnd, nil
	case finishLength:
		return llm.StopMaxTokens, nil
	case finishToolCalls, finishFunctionCall:
		return llm.StopToolUse, nil
	case "":
	default:
		c.log.Warn("openai: unknown finish_reason", "finish_reason", finish)
	}
	if hasCalls {
		return llm.StopToolUse, nil
	}
	return llm.StopEnd, nil
}
