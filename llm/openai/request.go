package openai

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/ManavA/keel/llm"
)

// defaultSchemaName labels an output schema that has no name of its own; the
// reference requires one.
const defaultSchemaName = "output"

// errorPrefix marks a tool result that failed, since the protocol has no flag
// for it.
const errorPrefix = "ERROR: "

// chatRequest is the body of POST /chat/completions. A field that has
// nothing to say is left out.
type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          string          `json:"tool_choice,omitempty"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
}

// chatMessage is one message. Content is a pointer so that an assistant turn
// that only calls tools can send null, as the reference's own example shows.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      bool            `json:"strict"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatBody maps req to the JSON body of a chat completions request.
func (c *Client) chatBody(req llm.Request, stream bool) ([]byte, error) {
	body := chatRequest{
		Model:           c.modelFor(req),
		ReasoningEffort: string(req.Effort),
		Stop:            req.Stop,
		Temperature:     req.Temperature,
	}

	msgs, err := c.chatMessages(req)
	if err != nil {
		return nil, err
	}
	body.Messages = msgs

	for _, t := range req.Tools {
		body.Tools = append(body.Tools, chatTool{Type: "function", Function: chatFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schemaOrNil(t.Schema),
			Strict:      t.Strict,
		}})
	}
	switch req.ToolChoice {
	case llm.ToolChoiceAuto:
	case llm.ToolChoiceNone:
		// Without tools there is nothing to refuse, and the reference gives
		// "none" as the default for that case.
		if len(req.Tools) > 0 {
			body.ToolChoice = "none"
		}
	default:
		return nil, fmt.Errorf("openai: unknown tool choice %q", req.ToolChoice)
	}

	if req.Output != nil {
		if len(bytes.TrimSpace(req.Output.JSON)) == 0 {
			return nil, fmt.Errorf("openai: output schema %q has no schema", req.Output.Name)
		}
		name := req.Output.Name
		if name == "" {
			name = defaultSchemaName
		}
		body.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: jsonSchema{
			Name:        name,
			Description: req.Output.Description,
			Schema:      req.Output.JSON,
			Strict:      true,
		}}
	}

	limit := req.MaxTokens
	if limit <= 0 {
		limit = c.maxTokens
	}
	if limit > 0 {
		if c.legacyMaxTokens {
			body.MaxTokens = limit
		} else {
			body.MaxCompletionTokens = limit
		}
	}

	if stream {
		body.Stream = true
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	return encode(body)
}

// chatMessages maps the system prompt and the conversation.
func (c *Client) chatMessages(req llm.Request) ([]chatMessage, error) {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: c.systemRole, Content: &req.System})
	}
	for i, m := range req.Messages {
		switch m.Role {
		case llm.RoleUser:
			msgs = append(msgs, chatMessage{Role: "user", Content: &m.Text})
		case llm.RoleAssistant:
			msgs = append(msgs, assistantMessage(m))
		case llm.RoleTool:
			for _, r := range m.ToolResults {
				content := r.Content
				if r.IsError {
					content = errorPrefix + content
				}
				msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: r.CallID, Content: &content})
			}
		default:
			return nil, fmt.Errorf("openai: message %d: unknown role %q", i, m.Role)
		}
	}
	return msgs, nil
}

// assistantMessage rebuilds a turn from its text and calls. Opaque is not
// read: this protocol has no richer form of a turn.
func assistantMessage(m llm.Message) chatMessage {
	out := chatMessage{Role: "assistant"}
	// The reference requires content unless there are tool calls.
	if m.Text != "" || len(m.ToolCalls) == 0 {
		out.Content = &m.Text
	}
	for _, call := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, chatToolCall{
			ID:       call.ID,
			Type:     "function",
			Function: chatFunctionCall{Name: call.Name, Arguments: callArguments(call)},
		})
	}
	return out
}

// callArguments is what goes in a call's arguments string: the input as it
// was written, or for a malformed call the text the model wrote.
func callArguments(call llm.ToolCall) string {
	in := bytes.TrimSpace(call.Input)
	if call.Malformed {
		var text string
		if json.Unmarshal(in, &text) == nil && text != "" {
			return text
		}
		return "{}"
	}
	if len(in) == 0 || bytes.Equal(in, []byte("null")) {
		return "{}"
	}
	return string(call.Input)
}

// schemaOrNil is a tool's schema, or nil when it has none, which the
// reference reads as a function with no parameters.
func schemaOrNil(schema json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(schema)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return schema
}

// encode marshals v without escaping <, > and &, which would change the
// bytes of a schema or a message for no reason.
func encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
