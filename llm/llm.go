package llm

import (
	"context"
	"encoding/json"
)

// Role says who a Message is from.
type Role string

// The roles a Message can carry. The system prompt is Request.System, not a
// message: both providers treat it as a property of the request.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn of a conversation.
type Message struct {
	Role Role `json:"role"`
	// Text is the turn's text. For an assistant turn that only calls tools it
	// may be empty.
	Text string `json:"text,omitempty"`
	// ToolCalls are the calls an assistant turn makes.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolResults answers the calls of the assistant turn before it. One
	// RoleTool message carries every result for that turn.
	ToolResults []ToolResult `json:"tool_results,omitempty"`
	// Opaque is the provider's own form of an assistant turn. See Opaque.
	Opaque *Opaque `json:"opaque,omitempty"`
}

// ToolCall is one call the model asks for.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input is the arguments as the model wrote them, a JSON object. It is
	// never decoded and re-encoded on the way through this package.
	Input json.RawMessage `json:"input"`
	// Malformed reports that the provider returned arguments that are not
	// valid JSON. Input then holds them as one JSON string.
	Malformed bool `json:"malformed,omitempty"`
}

// ToolResult is the outcome of one ToolCall, sent back to the model.
type ToolResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Opaque is an assistant turn exactly as one provider returned it, kept so
// the next request can send it back unchanged. A provider replays Data only
// when Provider is its own name, and otherwise builds the turn from Text and
// ToolCalls.
type Opaque struct {
	Provider string          `json:"provider"`
	Data     json.RawMessage `json:"data"`
}

// Tool describes one tool the model may call.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	// Strict asks the provider to guarantee Input matches Schema.
	Strict bool `json:"strict,omitempty"`
}

// Schema is a JSON Schema the reply text must satisfy.
type Schema struct {
	// Name labels the format for providers that require one.
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	JSON        json.RawMessage `json:"schema"`
}

// ToolChoice says whether the model may call tools.
type ToolChoice string

// The tool choices. There is no choice that forces a call: current Claude
// models reject one, and a schema on the reply does the same job.
const (
	ToolChoiceAuto ToolChoice = ""
	ToolChoiceNone ToolChoice = "none"
)

// Effort is how hard the model should work on a reply.
type Effort string

// The effort levels both providers accept. Empty leaves the provider's
// default in place.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// Request is one chat turn to generate.
type Request struct {
	// Model names the model. Empty uses the provider's configured default.
	Model    string
	System   string
	Messages []Message
	Tools    []Tool
	// ToolChoice defaults to ToolChoiceAuto.
	ToolChoice ToolChoice
	// Output, when set, constrains the reply text to a JSON Schema.
	Output *Schema
	// MaxTokens bounds the reply. Zero uses the provider's default.
	MaxTokens int
	// Temperature is sent only when set. Current Claude models reject any
	// value but the default, so leave it nil for them.
	Temperature *float64
	Effort      Effort
	Stop        []string
}

// StopReason says why a reply ended.
type StopReason string

// The stop reasons.
const (
	StopEnd           StopReason = "end"
	StopToolUse       StopReason = "tool_use"
	StopMaxTokens     StopReason = "max_tokens"
	StopSequence      StopReason = "stop_sequence"
	StopRefusal       StopReason = "refusal"
	StopPause         StopReason = "pause"
	StopContextWindow StopReason = "context_window"
)

// Usage counts the tokens one call was billed for.
type Usage struct {
	// InputTokens excludes tokens read from or written to a prompt cache.
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// ReasoningTokens is the part of OutputTokens spent on reasoning. It is
	// already counted in OutputTokens.
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
}

// Total is every token billed: input, cache reads and writes, and output.
func (u Usage) Total() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens
}

// Add returns the sum of u and o.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		ReasoningTokens:  u.ReasoningTokens + o.ReasoningTokens,
	}
}

// Refusal explains a reply that ended with StopRefusal.
type Refusal struct {
	Category    string `json:"category,omitempty"`
	Explanation string `json:"explanation,omitempty"`
}

// Attempt is one model's billed share of a reply that more than one model
// worked on.
type Attempt struct {
	Model string `json:"model"`
	Usage Usage  `json:"usage"`
}

// Response is one generated turn.
type Response struct {
	// ID is the provider's id for the reply.
	ID string
	// Model is the model that produced Message, which after a fallback is not
	// the one the request named.
	Model   string
	Message Message
	Stop    StopReason
	// Usage is what Model was billed.
	Usage Usage
	// Refusal is set when Stop is StopRefusal.
	Refusal *Refusal
	// Attempts lists every billed attempt when more than one model ran.
	// Empty means one attempt, by Model, costing Usage.
	Attempts []Attempt
}

// Delta is one increment of a streamed reply.
type Delta struct {
	Text      string
	Reasoning string
	ToolCall  *ToolCallDelta
}

// ToolCallDelta is one increment of a streamed tool call. ID and Name arrive
// once, on the first delta for Index.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	InputJSON string
}

// Model generates chat turns.
type Model interface {
	// Generate returns one complete reply.
	Generate(ctx context.Context, req Request) (*Response, error)
	// Stream calls fn for each increment as it arrives and returns the same
	// Response Generate would have. An error from fn stops the stream and is
	// returned.
	Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error)
}

// EmbedRequest asks for one vector per input.
type EmbedRequest struct {
	Model string
	Input []string
	// Dimensions asks for vectors of this length. Zero uses the model's own.
	Dimensions int
}

// EmbedResponse holds one vector per input, in input order.
type EmbedResponse struct {
	Model   string
	Vectors [][]float32
	Usage   Usage
}

// Embedder turns text into vectors.
type Embedder interface {
	Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error)
}
