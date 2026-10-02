package agent

import (
	"context"
	"encoding/json"
	"time"
)

// Model generates the next turn of a run's conversation. An adapter over
// any chat model satisfies it; this package imports none.
type Model interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

// Guard decides whether an action may happen. An adapter over a rule engine
// satisfies it; this package imports none.
type Guard interface {
	Decide(ctx context.Context, a Action) (Decision, error)
}

// Clock tells the time. Every timestamp in the journal, every lease and
// every budget is measured on it.
type Clock interface {
	Now() time.Time
}

// Publisher receives a notification after each change to a run. It has the
// shape of events.Publisher, so a bus from that package can be passed as is.
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Role says who a Message is from.
type Role string

// The roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Call is one tool call the model asked for.
type Call struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input is the arguments exactly as the model wrote them.
	Input json.RawMessage `json:"input"`
	// Malformed reports arguments that were not valid JSON. Input then holds
	// them as one JSON string, and the call is answered with an error
	// without running.
	Malformed bool `json:"malformed,omitempty"`
}

// Result is what a call returned to the model.
type Result struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
}

// Opaque is an assistant turn in its provider's own form, journaled so it
// can be sent back unchanged.
type Opaque struct {
	Provider string          `json:"provider"`
	Data     json.RawMessage `json:"data"`
}

// Message is one turn of a run's conversation.
type Message struct {
	Role    Role     `json:"role"`
	Text    string   `json:"text,omitempty"`
	Calls   []Call   `json:"calls,omitempty"`
	Results []Result `json:"results,omitempty"`
	Opaque  *Opaque  `json:"opaque,omitempty"`
}

// ToolSpec is what the model is told about a tool.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

// Request is what a Model is asked to answer.
type Request struct {
	RunID string
	Agent string
	// Model is the run's Definition.Model, for an adapter that serves
	// several models.
	Model    string
	System   string
	Messages []Message
	Tools    []ToolSpec
	// Output, when set, is a JSON Schema the final answer must satisfy.
	Output    json.RawMessage
	MaxTokens int
}

// Stop says why a Model's reply ended.
type Stop string

// The stop reasons.
const (
	StopEnd           Stop = "end"
	StopToolUse       Stop = "tool_use"
	StopMaxTokens     Stop = "max_tokens"
	StopRefusal       Stop = "refusal"
	StopPause         Stop = "pause"
	StopContextWindow Stop = "context_window"
)

// Usage is what work cost.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	// CostMicros is in millionths of a US dollar. The Model reports it; this
	// package knows no prices.
	CostMicros int64 `json:"cost_micros"`
}

// Add returns the sum of u and o.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:  u.InputTokens + o.InputTokens,
		OutputTokens: u.OutputTokens + o.OutputTokens,
		CostMicros:   u.CostMicros + o.CostMicros,
	}
}

// Response is a Model's reply.
type Response struct {
	Message Message
	Stop    Stop
	Usage   Usage
	// Model is the model that answered.
	Model string
}

// Action describes a tool call to the Guard.
type Action struct {
	Kind   string         `json:"kind"`
	Target string         `json:"target,omitempty"`
	Attrs  map[string]any `json:"attrs,omitempty"`
}

// Attribute names the engine sets on every Action before the Guard sees it,
// unless the tool's own Action already set them.
const (
	AttrAgent = "agent"
	AttrTool  = "tool"
	AttrRun   = "run"
	AttrSeq   = "seq"
)

// Effect is what a Guard says about an Action.
type Effect string

// The effects, from least strict to most. An Effect that is none of these
// is treated as Block.
const (
	Allow Effect = "allow"
	Ask   Effect = "ask"
	Block Effect = "block"
)

// Decision is a Guard's answer and the rule that gave it.
type Decision struct {
	Effect Effect `json:"decision"`
	Rule   string `json:"rule"`
}

// Rule names recorded for a decision no Guard rule made.
const (
	// RuleNoGuard is recorded when the engine has no Guard.
	RuleNoGuard = "no guard configured"
	// RuleToolApproval is recorded when the Guard allowed a call and the
	// tool itself is marked as needing approval.
	RuleToolApproval = "tool requires approval"
	// RuleInterrupted is recorded when an at-most-once call was interrupted
	// and a person is asked whether to run it again.
	RuleInterrupted = "call was interrupted; outcome unknown"
)

// Invocation is one execution of a tool call.
type Invocation struct {
	RunID string
	Agent string
	// Seq is the call's step in the journal.
	Seq int
	// Attempt is 1 the first time the call executes and greater after an
	// interruption.
	Attempt int
	// Key is the call's idempotency key. It is the same on every attempt.
	Key  string
	Call Call
}

// ToolFunc runs a tool. The string is returned to the model. An error is
// returned to the model as a failed result, unless it wraps ErrTransient, in
// which case the call is tried again later with the same Key.
type ToolFunc func(ctx context.Context, in Invocation) (string, error)

// Tool is something an agent can do.
type Tool struct {
	// Name must match ^[a-zA-Z0-9_-]{1,64}$ and be unique in the Definition.
	Name        string
	Description string
	// Schema is the JSON Schema of the arguments. Nil is an object with no
	// declared properties.
	Schema json.RawMessage
	// Run executes the tool. Exactly one of Run and Delegate is set.
	Run ToolFunc
	// Delegate names a registered agent. A call starts a child run of it
	// with the call's arguments as input, and the child's output is the
	// result.
	Delegate string
	// Action describes a call to the Guard. Nil describes it as
	// Action{Kind: "run", Target: Name}.
	Action func(in Invocation) Action
	// Approval parks every call for a person, whatever the Guard says.
	Approval bool
	// AtMostOnce stops an interrupted call from being executed again unless
	// a person approves it.
	AtMostOnce bool
	// Timeout bounds one execution. Default 2 minutes.
	Timeout time.Duration
}

// Limits bound a run. A zero field takes its default; a negative field is
// no limit.
type Limits struct {
	// MaxDuration bounds the time spent in model and tool calls. Time parked
	// for a person or a child run does not count. Default 15 minutes.
	MaxDuration time.Duration `json:"max_duration_ns"`
	// MaxCostMicros bounds cost, in millionths of a US dollar. Default none.
	MaxCostMicros int64 `json:"max_cost_micros"`
	// MaxTokens bounds input plus output tokens. Default none.
	MaxTokens int64 `json:"max_tokens"`
	// MaxModelCalls bounds model calls. Default 50.
	MaxModelCalls int `json:"max_model_calls"`
}

// Definition is an agent: its instructions, its tools and its limits.
type Definition struct {
	// Name identifies the agent. Every process that may resume its runs
	// must register a Definition with this name.
	Name   string
	System string
	// Model is passed to the Model in Request.Model.
	Model string
	Tools []Tool
	// Output, when set, is a JSON Schema for the final answer.
	Output    json.RawMessage
	MaxTokens int
	Limits    Limits
}

// Snapshot is the part of a Definition the model sees, fixed on a run when
// it starts so that a deploy cannot change a conversation already under way.
type Snapshot struct {
	System    string          `json:"system"`
	Model     string          `json:"model,omitempty"`
	Tools     []ToolSpec      `json:"tools,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	MaxTokens int             `json:"max_tokens,omitempty"`
	Limits    Limits          `json:"limits"`
}
