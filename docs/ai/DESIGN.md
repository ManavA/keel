# Agents on Keel: design of the first milestone

Status: design for review. Nothing here is built yet. `PLAN.md` beside this
file splits the first milestone into tasks.

Written against `main` at `e20a12b` on 2026-10-02.

## Contents

1. [Positioning](#1-positioning)
2. [What the first milestone shows](#2-what-the-first-milestone-shows)
3. [Sources read](#3-sources-read)
4. [`llm`](#4-llm)
5. [`policy`](#5-policy)
6. [`agent`](#6-agent)
7. [`httpx`: server-sent events](#7-httpx-server-sent-events)
8. [How the packages meet](#8-how-the-packages-meet)
9. [`examples/agent`](#9-examplesagent)
10. [Layering](#10-layering)
11. [Later milestones](#11-later-milestones)
12. [Decisions the conventions did not settle](#12-decisions-the-conventions-did-not-settle)

## 1. Positioning

Keel becomes the set of Go packages for running AI agents in production on
Postgres alone. An agent run is a journal in Postgres: every model call and
every tool call is written down before it starts and after it finishes, so a
process that dies mid-run is replaced by any other process, which carries on
from the step the journal stops at. What an agent may do is decided outside
the model, by rules that are data: a tool call is allowed, held for a person,
or blocked, and the rule that decided is on the record. The model is reached
through one small interface, with Anthropic's API and the OpenAI-compatible
protocol as providers and a scripted model as the in-process default, so the
whole thing runs under `go test` with no network. Keel's existing rule holds:
a project running only Postgres gets all of it, with no queue, no workflow
server and no vendor SDK.

## 2. What the first milestone shows

`examples/agent` is the demonstration, and the packages exist to make each
line of it true:

1. An agent works through a batch of documents, delegating each one to a
   specialist run.
2. The process is killed mid-run. Restarted, it resumes at the step the
   journal stops at, and no tool call that completed is made again.
3. The run stops and waits for a person before the one action that sends
   something. It stays waiting across a restart. Approved, it carries on.
4. A rule blocks an action the agent may not take, and the timeline names the
   rule.
5. A test that scores the run's outcome passes under `go test`, with a
   scripted model and no network.

Three new packages carry this, plus one new file in `httpx` and one in `app`:

| Package | What it is | In-process default | External option |
|---|---|---|---|
| `llm` | One interface for a chat model, and wrappers for retry, fallback, budget and accounting | `Scripted` | `llm/anthropic`, `llm/openai` |
| `policy` | Rules for what an action may do; every decision recorded with its rule | `MemoryRecorder` | `policy/pg` |
| `agent` | Durable runs: journal, lease, budgets, approvals, child runs | `MemoryStore` | `agent/pg`; `agent/httpapi` for the HTTP surface |

## 3. Sources read

The request and response shapes in section 4 come from the vendors' own
references, read on 2026-10-02. Nothing about a wire format in this document
is from memory.

Anthropic:

| Address | Used for |
|---|---|
| `https://platform.claude.com/docs/en/api/messages` | Request body, response, `usage`, `stop_reason`, tool definitions, `tool_result` |
| `https://platform.claude.com/docs/en/build-with-claude/streaming` | Event flow, delta types, error events |
| `https://platform.claude.com/docs/en/build-with-claude/structured-outputs` | `output_config.format`, schema limits |
| `https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls` | Where `tool_result` blocks must go |
| `https://platform.claude.com/docs/en/api/errors` | Status codes, error body, `request-id`, the validation errors below |
| `https://platform.claude.com/docs/en/api/rate-limits` | `retry-after` |
| `https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback` | `stop_reason: "refusal"`, `stop_details`, server-side fallback |
| `https://platform.claude.com/docs/en/build-with-claude/preserved-thinking` | What a resumed conversation must send back unchanged |
| `https://platform.claude.com/docs/en/about-claude/pricing` | Prices in the example's table |

OpenAI (the reference has moved from `platform.openai.com/docs/api-reference`
to `developers.openai.com`; the old addresses redirect):

| Address | Used for |
|---|---|
| `https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create` | Request body, response, `usage`, `finish_reason`, tools, `response_format` |
| `https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events` | `chat.completion.chunk` |
| `https://developers.openai.com/api/reference/resources/embeddings/methods/create` | Embeddings |
| `https://developers.openai.com/api/docs/guides/error-codes` | Status codes, error codes, `Retry-After` |
| `https://developers.openai.com/api/reference/overview` | The `x-request-id` response header |
| `https://raw.githubusercontent.com/openai/openai-openapi/manual_spec/openapi.yaml` | The error body (`ErrorResponse`, `Error`) |

Four facts from these pages shape the design, and each differs from what
older code assumes:

- **A tool call cannot be forced.** `claude-opus-5-5` and `claude-sonnet-5-5`
  answer `tool_choice` of type `any` or `tool` with a 400. So `llm` has no
  forced tool choice, and structured output goes through
  `output_config.format`, never through a forced call.
- **Sampling parameters are refused.** Models after Claude Opus 4.6 reject
  any `temperature` but the default. So `Request.Temperature` is a pointer
  and is sent only when set.
- **An assistant turn must go back as it came.** A reply carries `thinking`
  blocks whose signature is valid only while the `system` prompt, the
  `tools` and every earlier message have the same content as when the block
  was produced. Accounts created on or after 2026-08-31 get a 400 otherwise.
  The reference's own answer for resuming a saved session is to persist
  what was sent and received and replay that: the rendered system prompt,
  the tool definitions, and each assistant turn as returned. JSON formatting
  and key order do not matter; the values do. A durable journal is that
  record by construction, and section 6 is built so it stays one: the
  system prompt and tools are fixed on the run when it starts, the
  provider's own form of each assistant turn is journaled, and the
  conversation is only ever appended to.
- **A refusal is a reply, not an error.** It is HTTP 200 with
  `stop_reason: "refusal"`, to be read before the content.

Model names in examples are current ones: `claude-opus-5-5`,
`claude-sonnet-5-5`, `claude-haiku-4-5-20251001`.

## 4. `llm`

### 4.1 Purpose

`llm` is how a service asks a language model for the next turn of a
conversation: text, tool calls, a reply held to a JSON Schema, a streamed
reply, and separately embeddings. It declares the interface, the types that
cross it, a scripted model that needs no network, and four wrappers that
compose around any model. It knows no provider. The two providers are
subpackages over `net/http` and `encoding/json`, with no vendor SDK, so
importing `llm` pulls in nothing beyond Keel's `retry` and the standard
library, and importing a provider adds nothing to that.

It does not run tools and does not loop. One call in, one turn out. `agent`
is the loop.

### 4.2 Exported API

The contract files, in full. `PLAN.md` task T0 lands these before anything
else is built against them.

*package llm: llm.go, errors.go, price.go*

```go
package llm

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

// ErrBudgetExceeded is returned by Budgeted for a call it refused.
var ErrBudgetExceeded = errors.New("llm: budget exceeded")

// ErrNoPrice is returned where a cost is needed for a model the price table
// does not list.
var ErrNoPrice = errors.New("llm: no price for model")

// ErrScriptExhausted is returned by Scripted when asked for a turn its script
// does not have.
var ErrScriptExhausted = errors.New("llm: script has no reply for this turn")

// Error is a failed call to a provider.
type Error struct {
	// Provider is the provider's name, "anthropic" or "openai".
	Provider string
	// Status is the HTTP status, or zero when no response arrived.
	Status int
	// Type is the provider's own error type, such as "rate_limit_error".
	Type      string
	Message   string
	RequestID string
	// RetryAfter is the wait the provider asked for, or zero.
	RetryAfter time.Duration
	// Retryable reports whether the same request may succeed later.
	Retryable bool
	// Err is the transport error when no response arrived.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("llm: %s: %v", e.Provider, e.Err)
	}
	return fmt.Sprintf("llm: %s: status %d %s: %s", e.Provider, e.Status, e.Type, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether err is a provider failure worth trying again.
//
// If err holds an *Error, its Retryable field decides, whatever the error it
// wraps says. A client's own timeout wraps context.DeadlineExceeded while the
// caller's context is still live, and is worth another try; a provider whose
// caller's context is done returns that context's error and no *Error. Any
// other error is not retryable: a bare context cancellation or deadline,
// ErrBudgetExceeded, and anything that is not from a provider.
func Retryable(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Retryable
}

// Price is what one model costs, in millionths of a US dollar per million
// tokens: $4 per million tokens is 4_000_000.
type Price struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Prices is a price table by model name.
type Prices map[string]Price

// Cost is what u costs on model, in millionths of a US dollar, rounded up.
// It returns ErrNoPrice for a model the table does not list.
func (p Prices) Cost(model string, u Usage) (int64, error) {
	price, ok := p[model]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrNoPrice, model)
	}
	total := u.InputTokens*price.Input + u.OutputTokens*price.Output +
		u.CacheReadTokens*price.CacheRead + u.CacheWriteTokens*price.CacheWrite
	return (total + 999_999) / 1_000_000, nil
}

// CostOf is the cost of a whole Response: each of its Attempts, or its one
// Model and Usage when it has none.
func (p Prices) CostOf(resp *Response) (int64, error) {
	if len(resp.Attempts) == 0 {
		return p.Cost(resp.Model, resp.Usage)
	}
	var total int64
	for _, a := range resp.Attempts {
		c, err := p.Cost(a.Model, a.Usage)
		if err != nil {
			return 0, err
		}
		total += c
	}
	return total, nil
}

// CostFor is the cost of a whole Response as Budgeted and Metered price it:
// each attempt at the model it names or, when the table does not list that
// one, at asked, the model the request asked for. A provider commonly answers
// an alias with a dated id the table does not carry, which CostOf would
// refuse. It returns an error wrapping ErrNoPrice when some attempt can be
// priced at neither name, and never the sum of the attempts that could be.
func (p Prices) CostFor(resp *Response, asked string) (int64, error)
```

The rest of the package, as signatures:

*package llm: estimate.go, scripted.go, embed.go, decode.go, retrying.go, fallback.go, budget.go, metered.go*

```go
package llm

// ---- scripted.go ----

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
func Replies(replies ...Reply) Script

// Route is a Script that picks another by Request.Model. The "" entry is
// used for a model the map does not name.
func Route(byModel map[string]Script) Script

// ScriptedOptions configures a Scripted. The zero value works.
type ScriptedOptions struct {
	// Name is reported as Response.Model when the request names none.
	// Default "scripted".
	Name string
}

// Scripted is the in-process Model: it answers from a Script, with no network.
type Scripted struct{ /* unexported fields */ }

// NewScripted builds a Scripted over script.
func NewScripted(script Script, opts ScriptedOptions) *Scripted

// Generate implements Model.
func (s *Scripted) Generate(ctx context.Context, req Request) (*Response, error)

// Stream implements Model.
func (s *Scripted) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error)

// Requests returns every request received, in order.
func (s *Scripted) Requests() []Request

// ---- embed.go ----

// HashEmbedder is the in-process Embedder.
type HashEmbedder struct{ /* unexported fields */ }

// NewHashEmbedder builds a HashEmbedder producing vectors of dims
// dimensions, 256 when dims is not positive.
func NewHashEmbedder(dims int) *HashEmbedder

// Embed implements Embedder.
func (e *HashEmbedder) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error)

// ---- estimate.go ----

// EstimateInputTokens is an upper estimate of what req costs to send.
func EstimateInputTokens(req Request) int64

// ---- retrying.go ----

// RetryOptions configures a Retrying. The zero value retries a retryable
// failure up to three attempts.
type RetryOptions struct {
	// Retry is passed to retry.Do. Its Retryable is replaced by Retryable
	// from this package, and MaxAttempts defaults to 3.
	Retry retry.Options
	// MaxRetryAfter caps how long a provider's Retry-After is honoured.
	// Default 60 seconds.
	MaxRetryAfter time.Duration
}

// Retrying retries a Model's retryable failures with backoff.
type Retrying struct{ /* unexported fields */ }

// NewRetrying wraps m.
func NewRetrying(m Model, opts RetryOptions) *Retrying

// Generate implements Model.
func (r *Retrying) Generate(ctx context.Context, req Request) (*Response, error)

// Stream implements Model.
func (r *Retrying) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error)

// ---- fallback.go ----

// FallbackOptions configures a Fallback. The zero value moves on after any
// error but a cancelled context or a refused budget.
type FallbackOptions struct {
	// ShouldFallback decides whether err moves on to the next model. Nil
	// uses the default above.
	ShouldFallback func(err error) bool
	// OnRefusal also moves on when a model answers with StopRefusal. A
	// refusal is billed, so if no later model answers, the last refusal is the
	// reply, with a nil error and every refused attempt in Attempts, and the
	// failures that followed are logged.
	OnRefusal bool
	Logger    *slog.Logger
}

// Fallback tries each Model in order until one answers.
type Fallback struct{ /* unexported fields */ }

// NewFallback builds a chain over models, which must not be empty.
func NewFallback(opts FallbackOptions, models ...Model) (*Fallback, error)

// Generate implements Model.
func (f *Fallback) Generate(ctx context.Context, req Request) (*Response, error)

// Stream implements Model.
func (f *Fallback) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error)

// ---- budget.go ----

// BudgetOptions configures a Budgeted. A limit of zero is no limit.
type BudgetOptions struct {
	// MaxTokens bounds Usage.Total summed over every call.
	MaxTokens int64
	// MaxCostMicros bounds the cost summed over every call, in millionths of
	// a US dollar. It needs Prices.
	MaxCostMicros int64
	Prices        Prices
	// Model is the model a request that names none is priced as.
	Model string
	// DefaultMaxTokens is the reply bound assumed for a request that sets
	// none. Default 16000.
	DefaultMaxTokens int
	// Logger receives a warning when a call settles for more than was held
	// for it. Nil uses slog.Default().
	Logger *slog.Logger
}

// Spend is what a Budgeted has used.
type Spend struct {
	Calls      int64
	Tokens     int64
	CostMicros int64
}

// Budgeted refuses a call that could take a Model past a budget.
type Budgeted struct{ /* unexported fields */ }

// NewBudgeted wraps m. It returns an error when MaxCostMicros is set
// without Prices.
func NewBudgeted(m Model, opts BudgetOptions) (*Budgeted, error)

// Generate implements Model.
func (b *Budgeted) Generate(ctx context.Context, req Request) (*Response, error)

// Stream implements Model.
func (b *Budgeted) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error)

// Spent reports what has been used so far.
func (b *Budgeted) Spent() Spend

// ---- metered.go ----

// CallRecord is the account of one call through a Metered.
type CallRecord struct {
	At       time.Time
	Duration time.Duration
	// Model is the model that answered, or the one asked for when the call
	// failed.
	Model string
	Usage Usage
	// CostMicros is the call's cost, and Priced whether every attempt was
	// priced: by its own model's name in the table or, failing that, at the
	// model the request asked for. A model that a fallback moves to should be
	// listed in the table, since otherwise it is priced at the first model's
	// rate.
	CostMicros int64
	Priced     bool
	Stop       StopReason
	Err        error
}

// MeterOptions configures a Metered. The zero value counts tokens and
// prices nothing.
type MeterOptions struct {
	Prices Prices
	// Record receives each CallRecord. Nil keeps only the totals.
	Record func(ctx context.Context, c CallRecord)
	// Now defaults to time.Now.
	Now func() time.Time
}

// Metered accounts for every call through a Model.
type Metered struct{ /* unexported fields */ }

// NewMetered wraps m.
func NewMetered(m Model, opts MeterOptions) *Metered

// Generate implements Model.
func (m *Metered) Generate(ctx context.Context, req Request) (*Response, error)

// Stream implements Model.
func (m *Metered) Stream(ctx context.Context, req Request, fn func(Delta) error) (*Response, error)

// Totals reports what every call so far used.
func (m *Metered) Totals() Spend

// ---- decode.go ----

// Decode reads a structured reply into T. It returns an error when the reply
// did not end with StopEnd, since a refused or truncated reply need not
// match the schema.
func Decode[T any](resp *Response) (T, error)

var (
	_ Model    = (*Scripted)(nil)
	_ Model    = (*Retrying)(nil)
	_ Model    = (*Fallback)(nil)
	_ Model    = (*Budgeted)(nil)
	_ Model    = (*Metered)(nil)
	_ Embedder = (*HashEmbedder)(nil)
)
```

Both providers read server-sent events through one internal reader:

*package sse: llm/internal/sse/sse.go*

```go
// Package sse reads a server-sent event stream.
package sse

// MaxEventBytes bounds one event. A longer one is an error rather than an
// allocation without limit.
const MaxEventBytes = 16 << 20

// Event is one event from a stream.
type Event struct {
	// Name is the event field, or "" when the stream sent none.
	Name string
	// Data is the data lines joined with "\n".
	Data []byte
	ID   string
}

// Reader reads events from a stream.
type Reader struct{ /* unexported fields */ }

// NewReader reads events from r.
func NewReader(r io.Reader) *Reader

// Next returns the next event. It skips comment lines and events with no
// data, and returns io.EOF when the stream ends after a complete event. A
// stream that ends in the middle of an event, with no blank line after it,
// is a connection that dropped: the event is not returned, and the error
// wraps io.ErrUnexpectedEOF.
func (r *Reader) Next() (Event, error)
```

How a stream ends matters to a provider, so the reader tells the two ways
apart. A stream that ends between events, after the blank line that closes
the last one, returns `io.EOF`. A stream that ends after any field line, or
part of one, with no blank line to close the event, did not end: the
connection dropped. `Next` then returns an error wrapping
`io.ErrUnexpectedEOF` and never the partial event, which is what the standard
says to do with it. A transport that notices the truncation itself gives the
same error, so both providers test for one thing and report it as a
transport failure, an `*llm.Error` with `Err` set, retryable unless the
context ended. A failed read is returned wrapped, so `errors.Is` still finds
its cause, and once `Next` has returned an error it returns that error
again. Decisions 36 to 39 in section 12 record the rest of what the reader
does and does not read.

### 4.3 What each piece does

**`Scripted`.** The reply to a request is `script(req, turn)`, where `turn`
is the number of assistant messages already in `req.Messages`. Nothing else
decides the reply, so a `Scripted` built afresh in a restarted process gives
the same answers the first one would have, and a call asked twice gets the
same reply twice. This is what makes the kill-and-resume tests deterministic.
A `ToolCall` with no ID is given `call_<turn>_<index>`. When `Reply.Usage` is
zero, `InputTokens` is `EstimateInputTokens(req)` and `OutputTokens` is one
per four bytes of reply text and tool arguments, plus one, so a script always
costs the same. `Response.Model` is `Request.Model`, or `ScriptedOptions.Name`
when the request names none. `Stream` sends the text a word at a time and
each tool call as one delta, then returns what `Generate` would have. It is
safe for concurrent use.

**`EstimateInputTokens`.** One token per three bytes of the system prompt,
the messages, the tool definitions and the output schema, rounded up once
over their sum, plus eight per message. A message is counted in whichever of
its two forms is larger: its text, tool arguments and tool results, or the
bytes of its `Opaque.Data`. A provider is sent one form or the other, and its
own form of a turn also carries what the fields do not show, such as
thinking blocks and their signatures. A tool definition is its name,
description and schema, and the output schema its name, description and
JSON. It is an upper estimate for English text and not a tokenizer; it
exists so a budget can refuse a call before making it.

This rule changed while the contracts task was built. It first counted
message text, tool arguments and tool results and left `Opaque` out, which
made the estimate low for a conversation that replays thinking blocks, and
so made `Budgeted`'s worst case low. Decisions 33 to 35 in section 12 record
the change and what else the count includes.

**`HashEmbedder`.** Each input is lowercased and split on anything that is
not a letter or a digit; each token is hashed with FNV-1a to a dimension and
a sign; the vector is L2-normalised. Two texts sharing words land near each
other, the same text always gives the same vector in every process, and an
empty text gives the zero vector. It is for tests and for the vector-search
milestone's default, not for retrieval quality.

**`Retrying`.** `Generate` runs through `retry.Do` with
`Retryable: llm.Retryable`, three attempts by default, a 500 millisecond base
delay and a 30 second cap. When a failed attempt carries `RetryAfter`, the
wrapper waits that long, capped at `MaxRetryAfter`, before returning the
error to `retry.Do`, whose jitter is then added on top; it does not wait
after the last attempt. `Stream` is retried only while `fn` has not been
called: a stream that has delivered a delta is never replayed into the same
callback, so a failure after one is not retried, and an error returned by `fn`
itself comes back as `fn` returned it. The caller's own context ending stops
the loop at once, in a wait or between attempts, and the error returned wraps
the context's. Any other failure is returned as `retry.Do`'s `*retry.Error`,
through which `errors.As` still finds the `*llm.Error`. A model that returns
neither a reply nor an error is a bug, not a transient failure: it is reported
and not retried.

**`Fallback`.** Each model is tried in order. By default the chain moves on
after any error except `ErrBudgetExceeded`; `ShouldFallback` replaces that
test. The caller's context ending stops the chain whatever `ShouldFallback`
says, and it is read from the context, not from the error: an HTTP client's
own timeout wraps `context.DeadlineExceeded` while the caller's context is
live, and is a failed model like any other. `Request.Model` goes to the first
model only: a model name means something to one provider, so each later model
uses its own configured default. A model that did not produce an assistant
turn's `Opaque` ignores it and builds the turn from `Text` and `ToolCalls`,
which is how a conversation crosses providers. When every model fails, or the
chain stops early, the error is `llm: fallback:` wrapping `errors.Join` of
`model i of n: <error>` for each model tried; a nil model is refused by
`NewFallback`. A model that returns neither a reply nor an error has failed.

`Stream` moves on only before the first delta. Once the caller's callback has
been called the chain ends with what that model returns, and an error returned
by the callback itself comes back as the callback returned it.

With `OnRefusal`, a reply with `StopRefusal` also moves on. A refusal is
billed, so it is not dropped: it is listed in the `Attempts` of the reply that
follows, so that a price table sees every billed attempt, and if no later
model answers, the last refusal is itself the reply, with a nil error and every
refused attempt in its `Attempts`. The failures that followed it are logged
at warning level. This holds when the chain is ended by the caller's context,
by a refused budget, or by a `ShouldFallback` that says no. For a stream the
returned `Response` is authoritative, not the deltas: a later model may have
delivered deltas and then failed, and the reply is still the earlier refusal.
A refusal that itself delivered a delta stops the chain and is the reply, so a
refused attempt's own deltas are never left unaccounted. The one exception is
the callback's own error, which is returned with no reply, so a refusal moved
past before it is in no account; that is accepted, since the caller stopped
the stream, and its usage is logged at warning level.

**`Budgeted`.** Before a call it computes the worst case: estimated input
tokens plus the request's `MaxTokens` (or `DefaultMaxTokens`), and that
usage priced for the model the request names, or `BudgetOptions.Model` when
it names none. If what is spent, plus what calls in flight hold, plus this
worst case would pass a limit, it returns an error wrapping
`ErrBudgetExceeded` and makes no call. Otherwise it holds the worst case,
makes the call, gives the hold back, also if the model panics, and adds the
real usage, summed over the reply's `Attempts`, and its cost. Each attempt is
priced at the model it names or, when the table does not list that one, at the
model the request asked for: a provider commonly answers an alias with a dated
id. Only a reply that can be priced at neither is charged the cost that was
held. With `MaxCostMicros` set, a request for a model the table does not list
is refused with `ErrNoPrice`, because an unpriced call cannot be shown to fit.
A call that fails adds nothing, the call itself included: whether the provider
billed it cannot be known. A negative limit refuses every call. A model that
returns neither a reply nor an error has failed.

The guarantee is therefore: no call starts once the budget is spent, and none
starts whose worst case, priced as the model asked for, does not fit. It does
not promise that real spend stays under the limit, because the hold is an
upper bound only as far as its parts are. The estimate of the input can
undercount a prompt; the reply can come from a dearer model than the one the
hold was priced at; a chain behind the budget can bill several attempts for
one call. Real spend is added as it is, never clamped to what was held, so the
next call is refused for it, and a call that settles for more than its hold, in
a dimension that has a limit, is logged at warning level with the model asked
for, the model that answered, the hold and the real figures.

**`Metered`.** Every call, failed or not, produces one `CallRecord` and adds
to `Totals`. A failed call has no usage and costs nothing: its record carries
the error and the model asked for, `Priced` is false, and `Totals.Calls` still
counts it. `Priced` is true when every attempt behind the reply was priced, by
its own model's name or, failing that, at the model the request asked for;
otherwise, and with no table, it is false and the tokens are still counted. A
fallback to a different model should therefore list that model in the table,
since otherwise it is priced at the first model's rate. The record is made
when the call, or the stream, ends, and `Record` runs after the totals are
updated and with no lock held.

**`Decode`.** Unmarshals `resp.Message.Text` into `T`. It refuses a reply
whose `Stop` is not `StopEnd`, since the reference says a refused or
truncated reply need not match the schema.

### 4.4 `llm/anthropic`

*package anthropic: llm/anthropic/anthropic.go*

```go
// Package anthropic is the llm.Model for Anthropic's Messages API.
package anthropic

// Name is what this provider writes in llm.Opaque.Provider and llm.Error.Provider.
const Name = "anthropic"

// DefaultModel is used when neither the request nor Options names a model.
const DefaultModel = "claude-opus-5-5"

// DefaultBaseURL is Anthropic's own endpoint.
const DefaultBaseURL = "https://api.anthropic.com"

// Version is the anthropic-version header this package is written against.
const Version = "2023-06-01"

// Options configures a Client. Only APIKey is required.
type Options struct {
	// APIKey is sent as x-api-key. Required.
	APIKey string
	// BaseURL defaults to DefaultBaseURL. Tests point it at httptest.
	BaseURL string
	// Model is used for a request that names none. Default DefaultModel.
	Model string
	// MaxTokens is used for a request that sets none. Default 16000 for
	// Generate and 64000 for Stream.
	MaxTokens int
	// HTTPClient defaults to a client with a 10 minute timeout.
	HTTPClient *http.Client
	// Betas are extra anthropic-beta values to send.
	Betas []string
	// RefusalFallback, when "default", asks the API to retry a refused
	// request on the model it recommends. Empty leaves a refusal as the
	// reply.
	RefusalFallback string
	// Extra is merged into every request body, for a field this package
	// does not model.
	Extra map[string]any
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Client calls the Messages API over net/http.
type Client struct{ /* unexported fields */ }

// New builds a Client. It returns an error when APIKey is empty.
func New(opts Options) (*Client, error)

// Generate implements llm.Model.
func (c *Client) Generate(ctx context.Context, req llm.Request) (*llm.Response, error)

// Stream implements llm.Model.
func (c *Client) Stream(ctx context.Context, req llm.Request, fn func(llm.Delta) error) (*llm.Response, error)

var _ llm.Model = (*Client)(nil)
```

`POST {BaseURL}/v1/messages` with `x-api-key`, `anthropic-version:
2023-06-01`, `content-type: application/json`, and `anthropic-beta` holding
`Options.Betas` and any value below, comma-separated.

Request:

| `llm` | Messages API |
|---|---|
| `Request.Model`, else `Options.Model`, else `DefaultModel` | `model` |
| `Request.MaxTokens`, else `Options.MaxTokens`, else 16000 (`Generate`) or 64000 (`Stream`) | `max_tokens` |
| `Request.System` | `system`, a string; omitted when empty |
| `RoleUser` message | `{"role":"user","content":[{"type":"text","text":…}]}` |
| `RoleAssistant` message whose `Opaque.Provider` is `"anthropic"` | `{"role":"assistant","content":` `Opaque.Data` `}`, unchanged |
| Any other `RoleAssistant` message | A `text` block when `Text` is not empty, then one `{"type":"tool_use","id","name","input"}` block per `ToolCall`; a `Malformed` call is sent with `input` `{}` |
| `RoleTool` message | One `user` message holding one `{"type":"tool_result","tool_use_id","content","is_error"}` block per `ToolResult`, in order, and nothing else; `is_error` is omitted when false |
| `Tools` | `tools`: `{"name","description","input_schema","strict"}`; `input_schema` is `{"type":"object"}` when `Schema` is nil; `strict` is omitted when false |
| `ToolChoiceNone` | `tool_choice: {"type":"none"}`; `ToolChoiceAuto` omits the field |
| `Output` | `output_config.format: {"type":"json_schema","schema":` `Output.JSON` `}` |
| `Effort` | `output_config.effort` |
| `Temperature` | `temperature`, only when not nil |
| `Stop` | `stop_sequences` |
| `Options.RefusalFallback == "default"` | `fallbacks: "default"` and beta `server-side-fallback-2026-07-01` |
| `Options.Extra` | Merged into the body last |

`tools` is always sent in the order given. The provider never removes
`tools` to turn tool use off: that would change the checked prefix.

Response:

| Messages API | `llm` |
|---|---|
| `id`, `model` | `Response.ID`, `Response.Model` |
| `content`: `text` blocks | `Message.Text`, concatenated in order |
| `content`: `tool_use` blocks | `Message.ToolCalls`: `id`, `name`, and `input` as the bytes received |
| `content`, the whole array | `Message.Opaque{Provider: "anthropic", Data: …}`, the bytes received |
| `stop_reason` | `end_turn` → `StopEnd`; `tool_use` → `StopToolUse`; `max_tokens` → `StopMaxTokens`; `stop_sequence` → `StopSequence`; `pause_turn` → `StopPause`; `refusal` → `StopRefusal`; `model_context_window_exceeded` → `StopContextWindow` |
| `stop_details.category`, `.explanation` | `Response.Refusal` |
| `usage.input_tokens`, `.output_tokens`, `.cache_read_input_tokens`, `.cache_creation_input_tokens`, `.output_tokens_details.thinking_tokens` | `Usage.InputTokens`, `.OutputTokens`, `.CacheReadTokens`, `.CacheWriteTokens`, `.ReasoningTokens` |
| `usage.iterations`, when present | `Response.Attempts`, one per entry: `model` and its token counts |

Every block type is kept in `Opaque`, including `thinking` blocks with empty
text, `redacted_thinking`, and types this package does not know. One rule
applies before `Opaque` is stored: when the content holds a `fallback` block,
the reference's table for echoing a fallback turn is applied (drop
`thinking`, `redacted_thinking`, `connector_text` and client `tool_use`
blocks that come before the final `fallback` block; keep everything else
where it is).

Streaming. The body adds `"stream": true`. Events, read with
`llm/internal/sse`:

| Event | Handling |
|---|---|
| `message_start` | Take `message.id`, `message.model` and the first `usage` |
| `content_block_start` | Open the block at `index` with `content_block` as received |
| `content_block_delta` `text_delta` | Append to the block's text; `fn(Delta{Text})` |
| `content_block_delta` `input_json_delta` | Append `partial_json`; `fn(Delta{ToolCall})`, with `ID` and `Name` on the first delta of the block |
| `content_block_delta` `thinking_delta` | Append; `fn(Delta{Reasoning})` when not empty |
| `content_block_delta` `signature_delta` | Set the block's `signature` |
| `content_block_stop` | Close the block. A `tool_use` block's accumulated JSON becomes its `input`; empty accumulates to `{}` |
| `message_delta` | Take `delta.stop_reason`, `delta.stop_details`; `usage` here is cumulative and replaces what was held |
| `message_stop` | End |
| `ping`, and any event not listed | Ignored |
| `error` | Return an `*llm.Error` of that type; `overloaded_error` and `api_error` are retryable |

A block stays in the rebuilt content even when no delta carried text: a
`thinking` block opens, takes its signature and closes. `Opaque` is the
rebuilt content array. If a block receives a delta type this package does
not know, the content cannot be rebuilt faithfully, so `Opaque` is left nil
for that reply and a line is logged at Warn; the turn is then replayed from
`Text` and `ToolCalls`, which the reference allows.

Errors. A response that is not 2xx becomes `*llm.Error` with `Status`, the
body's `error.type` and `error.message`, `RequestID` from the `request-id`
header, and `RetryAfter` from `retry-after` in seconds. `Retryable` is true
for 408, 409 and every 5xx, which includes 529 `overloaded_error`, and for
429 only when `retry-after` is present: the reference says a 429 without it
is a spend cap that keeps failing. A transport failure is retryable unless
the context ended.

Not implemented by this provider: `llm.Embedder`. Anthropic has no embeddings
endpoint.

### 4.5 `llm/openai`

*package openai: llm/openai/openai.go*

```go
// Package openai is the llm.Model and llm.Embedder for the OpenAI chat
// completions protocol, which local runtimes also speak.
package openai

// Name is what this provider writes in llm.Error.Provider.
const Name = "openai"

// DefaultBaseURL is OpenAI's own endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

// Options configures a Client. Only Model is required.
type Options struct {
	// APIKey is sent as a bearer token. Empty sends none, which is what a
	// local runtime expects.
	APIKey string
	// BaseURL defaults to DefaultBaseURL. A local runtime is usually
	// http://localhost:11434/v1 or similar.
	BaseURL string
	// Model is used for a request that names none. Required: this protocol
	// has no model that is right for every server.
	Model string
	// EmbeddingModel is used for an EmbedRequest that names none.
	EmbeddingModel string
	// MaxTokens is used for a request that sets none. Zero sends no bound.
	MaxTokens int
	// LegacyMaxTokens sends the bound as max_tokens rather than
	// max_completion_tokens, for servers that only know the older name.
	LegacyMaxTokens bool
	// SystemRole is the role the system prompt is sent under. Default
	// "system"; OpenAI's newer models also accept "developer".
	SystemRole string
	// Header is added to every request, for a gateway that needs one. It
	// cannot replace Content-Type, nor the Authorization header an APIKey
	// sets.
	Header http.Header
	// HTTPClient defaults to a client that follows no redirects and has no
	// timeout of its own: a call that is not a stream is bounded by 10
	// minutes, and a stream by IdleTimeout. A client given here keeps its own
	// timeout, which then bounds a stream too, and its own policy on
	// redirects. The key and Header are withheld from a redirect to another
	// origin than BaseURL's, another scheme, host or port. The request is not:
	// a client that follows a 307 or 308 re-sends its body, which holds the
	// prompt, to wherever the redirect points, and that is for the client given
	// here to decide.
	HTTPClient *http.Client
	// IdleTimeout is how long a stream may go without receiving a byte from
	// the server, from the request being sent to the end of the stream. A
	// keep-alive comment is bytes and counts, so a server that sends nothing
	// else is bounded by the context and not by this, and the time a callback
	// takes is not counted. Zero is two minutes; negative is no limit.
	IdleTimeout time.Duration
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Client calls a chat completions endpoint over net/http.
type Client struct{ /* unexported fields */ }

// New builds a Client. It returns an error when Model is empty.
func New(opts Options) (*Client, error)

// Generate implements llm.Model.
func (c *Client) Generate(ctx context.Context, req llm.Request) (*llm.Response, error)

// Stream implements llm.Model.
func (c *Client) Stream(ctx context.Context, req llm.Request, fn func(llm.Delta) error) (*llm.Response, error)

// Embed implements llm.Embedder.
func (c *Client) Embed(ctx context.Context, req llm.EmbedRequest) (*llm.EmbedResponse, error)

var (
	_ llm.Model    = (*Client)(nil)
	_ llm.Embedder = (*Client)(nil)
)
```

`POST {BaseURL}/chat/completions`, with `Authorization: Bearer` when
`APIKey` is set, and `Options.Header`.

Request:

| `llm` | Chat completions |
|---|---|
| `Request.Model`, else `Options.Model` | `model` |
| `Request.System` | First message, `{"role": SystemRole, "content": …}`; omitted when empty |
| `RoleUser` message | `{"role":"user","content": Text}` |
| `RoleAssistant` message | `{"role":"assistant","content": Text or null,"tool_calls":[{"id","type":"function","function":{"name","arguments"}}]}`; `arguments` is `Input` as a string, or for a `Malformed` call the string `Input` holds |
| `RoleTool` message | One `{"role":"tool","tool_call_id","content"}` message per `ToolResult`, in order. The protocol has no error flag, so a result with `IsError` is sent with `content` prefixed `ERROR: ` |
| `Tools` | `tools`: `{"type":"function","function":{"name","description","parameters","strict"}}` |
| `ToolChoiceNone` | `tool_choice: "none"`; `ToolChoiceAuto` omits the field |
| `Output` | `response_format: {"type":"json_schema","json_schema":{"name","description","schema","strict":true}}`; `name` is `"output"` when `Schema.Name` is empty |
| `Request.MaxTokens`, else `Options.MaxTokens` | `max_completion_tokens`, or `max_tokens` with `LegacyMaxTokens`; omitted when zero |
| `Temperature` | `temperature`, only when not nil |
| `Effort` | `reasoning_effort` |
| `Stop` | `stop` |

`Opaque` is never sent and never set: an assistant turn in this protocol is
fully described by its text and tool calls.

Response, from `choices[0]`:

| Chat completions | `llm` |
|---|---|
| `id`, `model` | `Response.ID`, `Response.Model` |
| `message.content` | `Message.Text` |
| `message.tool_calls[]` | `Message.ToolCalls`: `id`, `function.name`, and `function.arguments` as `Input`. The reference warns the arguments are not always valid JSON: when `json.Valid` fails, `Input` is the text as one JSON string and `Malformed` is true |
| `message.refusal`, when not null | `StopRefusal`, `Refusal.Explanation` |
| `finish_reason` | `stop` → `StopEnd`; `length` → `StopMaxTokens`; `tool_calls` and `function_call` → `StopToolUse`; `content_filter` → `StopRefusal` |
| `usage.prompt_tokens`, `.completion_tokens`, `.prompt_tokens_details.cached_tokens`, `.prompt_tokens_details.cache_write_tokens`, `.completion_tokens_details.reasoning_tokens` | `InputTokens` is `prompt_tokens` less cached and cache-write tokens, never below zero; `OutputTokens`; `CacheReadTokens`; `CacheWriteTokens`; `ReasoningTokens` |

Streaming. The body adds `"stream": true` and
`"stream_options": {"include_usage": true}`. Each `data:` line is a
`chat.completion.chunk`; `data: [DONE]` ends the stream. From
`choices[0].delta`: `content` appends to the text and calls
`fn(Delta{Text})`; each `tool_calls[]` entry accumulates by its `index`,
with `id` and `function.name` arriving on its first chunk and
`function.arguments` in pieces. `finish_reason` arrives on the last chunk
with choices, and `usage` on a final chunk whose `choices` is empty. A
server that sends no usage chunk leaves `Usage` zero.

A stream ends in one of three ways. A chunk carried a `finish_reason`: the
reply is complete, whatever follows, and a connection cut after it, with or
without `data: [DONE]`, costs at most a usage chunk. No chunk did, but the
stream ended cleanly with `data: [DONE]`: some servers never send a finish
reason, so the reply is complete and its stop reason is read from what was
assembled (`StopToolUse` for tool calls, `StopRefusal` for a refusal,
`StopEnd` otherwise), and the missing reason is logged at debug level, since
`Response` has no field for it. Neither, which includes a `[DONE]` that
follows no chunk with an id or a choice, a stream that ends between events and
one cut in the middle of an event: a failure of the connection, an `*llm.Error`
with `Err` set and `Retryable` true. A server that said it finished, with a
finish reason, and said no more, has given an empty reply, which is legitimate.

A response to a streaming request whose content type is not
`text/event-stream` is an `*llm.Error` that names the content type and is not
retryable: it is a page from a proxy, or a server that ignored `stream`, and
asking again gets the same. An `{"error": …}` chunk in a stream, which the
reference does not document and servers send, is an `*llm.Error` too. A piece
of a tool call belongs to the most recent call opened for its `index`, or from
a server that sends none, for the index of the last piece, and an id that is
not the one that call already has opens a new one.

A stream is bounded by the caller's context; by `IdleTimeout` without a byte
from the server, where a keep-alive comment is a byte and the time a callback
takes is not counted; by 16 MiB in one event; and by 32 MiB in all that the
reply keeps: its text, its refusal and the id, name and arguments of each tool
call, with a fixed 256 bytes counted for each call besides, so that a server
that opens calls without end meets the bound. It is not bounded by a timeout on
the whole request, which would cut a long reply that is going well. A reply that
outgrows a size bound is a plain error, not retryable, since a retry meets the
same size. After a stream ends cleanly the body is read on, for at most 64 KiB
and 100 milliseconds, so that the connection is used again; after a failure or
an error from `fn` it is not, and the connection is closed.

Embeddings. `POST {BaseURL}/embeddings` with `model`, `input` as an array,
`encoding_format: "float"`, and `dimensions` when set. `data[]` is ordered
by its `index` into `Vectors`; `usage.prompt_tokens` is `InputTokens`.

Errors. The body is `{"error":{"message","type","param","code"}}`; a server
may send the message, type and code at the top level, or `error` as a string,
or a `code` that is a number. `Retryable` is true for 408 and every 5xx, and
for 429 unless `code` or `type` is one of `insufficient_quota`,
`credit_balance_exhausted`, `organization_spend_limit_exceeded`,
`project_spend_limit_exceeded` or `organization_usage_limit_exceeded`, which no
wait fixes. The reference's error guide gives `insufficient_quota` as the
`type` that can accompany those codes, so both fields are read. `RetryAfter` is
read from `Retry-After`, as seconds or as an HTTP date; `RequestID` from
`x-request-id`.

An error object inside a 200 response, in a completion, an embeddings reply
or a stream, is an `*llm.Error` with `Status` 200. It counts as an error only
when it carries a message or a type: an empty string or an empty object, which
some servers send beside a good reply, is not one. It is retryable when its
`type` or `code` is one the reference gives as passing (`rate_limit_error`,
`rate_limit_exceeded`, `service_unavailable_error`, `server_is_overloaded`,
`slow_down`, `server_error`) and not one of the final codes above.

A 3xx is an `*llm.Error`, not retryable, because the default client does not
follow redirects: one would send the caller's headers, and for a 307 or 308
the whole prompt, to whatever host the answer named. A client supplied in the
options keeps its own redirect policy, and the package withholds the key and
`Header` from any origin other than the configured one's (another scheme, host
or port, with a default port the same as none). The request is not withheld: a
client that follows a 307 or 308 re-sends its body, which holds the prompt, to
wherever the redirect points, and that is for the supplied client's policy to
set. A redirect that a supplied client refuses, by its own check or by the
ten hops `net/http` allows, is an `*llm.Error` that is not retryable.

When a call fails and the caller's context is done, the error is the context's
and is not retryable. When the context is live, a transport failure or the
client's own timeout is an `*llm.Error` with `Err` set and `Retryable` true.

### 4.6 Interfaces consumed

`llm` consumes `retry.Options` and `retry.Do`, and `*slog.Logger`. Each
provider consumes an `*http.Client`. Nothing else.

### 4.7 Failure and concurrency

Every type here is safe for concurrent use. `Scripted`, `Budgeted` and
`Metered` guard their state with a mutex; the providers hold no state beyond
their options. No call is retried unless it is wrapped in `Retrying`: a
provider makes one HTTP request per call.

The caller's own context ending, cancelled or past its deadline, ends a call
at once: it is never retried and never moves a `Fallback` on. That is read
from the context, not from an error that merely wraps a deadline. An HTTP
client's own timeout wraps `context.DeadlineExceeded` while the caller's
context is live; it is a failure of the provider, which marks it retryable,
and both `Retryable` and `Fallback` treat it as one. A provider whose caller's
context is done returns the context's error and no `*Error`. If the server had
already answered with an error status, that status is what is returned, even
when the cancellation cut its body short.

A provider reads at most 32 MiB of response body, and the event reader at
most 16 MiB per event; past either it returns an error instead of
allocating without limit, and a plain one that is not retryable, since a
retry meets the same size. A stream is not a body in this sense: it is bounded
by its context, by an idle limit on the bytes received from the server
(default two minutes, from the options; negative means none; comments count and
the time a callback takes does not), by the event bound, and by the same 32 MiB
applied to what the reply keeps. No timeout on the whole request applies
to a stream. After a stream ends cleanly a provider reads the rest of the body,
a little and for a moment, so that the connection is reused. The default
`http.Client` a provider builds follows no redirects.

The order to compose the wrappers, outermost first, is `Budgeted`, `Metered`,
`Fallback`, then one `Retrying` per provider: each provider retries its own
transient failures before the chain moves on, the budget refuses a call
before anything else sees it, and the budget and the meter each see one call
however many attempts it took. The meter records only what the budget let
through.

Two things hold for every wrapper. A stream callback's own error is the
caller's signal to stop, not a failure of the model, so each wrapper returns it
as the callback returned it, never wrapped, joined or retried. And a model that
returns neither a reply nor an error is a bug in the model: each wrapper
reports it as an error with one message, and none counts it as a reply.

### 4.8 In-process default

`Scripted` for chat and `HashEmbedder` for embeddings. Neither needs a
network, a key or a file.

### 4.9 Tests

All without a database and without a network.

- Contract files: JSON shapes of `Message`, `ToolCall`, `Usage` pinned
  against literals; `Prices.Cost` rounding, an unlisted model, `CostOf` with
  and without `Attempts`; `Retryable` over a table of errors, including a
  wrapped `*Error` and a cancelled context.
- `internal/sse`: multi-line data, comments, events with no name, an event
  split across reads, a missing final blank line, an event over the bound.
- `Scripted`: the reply depends on the request alone (two instances, same
  answers; the same request twice, same answer); a turn past the script is
  `ErrScriptExhausted`; `Route` by model; default usage is stable; `Stream`
  returns what `Generate` does.
- Wrappers, each against a fake model declared in the test file: retry
  counts by error class, `Retry-After` honoured and capped, no wait after
  the last attempt, a stream not retried after its first delta; fallback
  order, the model name cleared after the first, refusal with and without
  `OnRefusal`, all failing; budget refusing by tokens and by cost, an
  unpriced model, concurrent calls never passing the limit together (run
  under `-race`); meter totals with an unpriced reply.
- Providers, against `httptest.Server`: for each row of the tables above a
  case that asserts the request body the server received and the `Response`
  built from a canned reply. Canned bodies are files under `testdata/`
  copied from the references' own examples. Streaming cases replay a
  recorded event sequence, including a tool call split across deltas, a
  thinking block with no text, a `ping`, an unknown event type, an unknown
  delta type (asserting `Opaque` is nil and the reply still usable), and a
  mid-stream `error` event. Error cases cover each status in 4.4 and 4.5.
  One case per provider must fail when the mapping is wrong: the assistant
  turn replay test sends back a reply holding a `thinking` block and
  asserts the server receives the content array with the same values.
- One live test per provider behind the `live` build tag, as
  `geocode/mapbox_live_test.go` does, skipping without its key:
  `go test -tags=live ./llm/anthropic/ -run Live`. It never runs in CI.

### 4.10 Left out

- Images, documents and audio in messages. `Message` carries text; a later
  field adds parts without breaking it.
- Forced tool choice, for the reason in section 3.
- A price table. Prices change and a library's copy goes stale; the caller
  supplies `Prices`, and the example carries one with the date it was read.
- Deriving a JSON Schema from a Go type. Schemas are written as JSON.
- Prompt-cache markers, the Batches API, token counting over the network,
  server-side tools, and Anthropic's `input_transformations`. `Betas` and
  `Extra` let a caller reach a field this package does not model.
- Eager tool-input streaming. The API validates buffered tool input and
  does not validate eager input; a journal wants validated input.
- Metrics instruments. `metrics.Instruments` is a fixed struct, so this
  comes with the `trace` milestone.

## 5. `policy`

### 5.1 Purpose

`policy` decides what an action may do. An action has a kind, a target and
attributes. A rule gives the actions it matches one of three effects: allow,
ask a person, or block. When several rules match, the strictest wins, and
when two are equally strict the earlier one is reported. Every decision is
recorded with the rule that made it. Rules are data: a Go value, and a JSON
form that round-trips.

It sits beside `textpolicy` and imports nothing from it. `textpolicy`
answers "may this text be stored or shown", by matching patterns against
normalised text, and reports a rule name and never the text. `policy`
answers "may this action happen", by matching facts about the action.
Neither grows out of the other: their inputs, their matching and their
outcomes differ, and `textpolicy`'s two-outcome answer has no place for
"ask". They compose through a fact: a caller that checks outgoing text with
`textpolicy` puts the name of the rule that matched into the action's
attributes, and a `policy` rule blocks on that attribute. Section 9 does
this.

### 5.2 Semantics, and the reference they agree with

The reference is `web/src/demos/permissions/logic.ts` in the hanaML
repository, with its tests in `web/test/demos/permissions.test.ts`.

1. A rule whose `When` holds for the action is a match, and one whose
   `When` cannot be told is a match unless its effect is allow (the three
   values are set out below).
2. The effects are ordered allow, ask, block.
3. The decision is the first match of the greatest strictness: walking the
   rules in order, a match replaces the best so far only when it is strictly
   stricter. This is the reference's `reduce`, and gives "on a tie the
   earlier rule is reported".
4. When nothing matches, the effect is `Policy.Default`, or `Block` when
   that is empty, and the rule reported is `RuleDefault`.

A `Match` holds when every part that is set holds:

- `Kinds`: the action's kind is one of them. Empty is every kind.
- `Target`: `path.Match(pattern, action.Target)` is true. Empty is every
  target. The target is matched as written, with no normalisation: no case
  folding, no trimming, no cleaning of dots or slashes. A block rule by
  target is therefore passed by any other spelling of the same thing, and
  whatever builds an `Action` (the `app` adapter, the example's tools) puts
  targets in one canonical spelling before asking for a decision. `*` matches
  any run of characters but a slash, `?` one character but a slash,
  `[a-z]` and `[^a-z]` a class, and `\` makes the next character literal.
  A pattern that does not compile is a validation error, and so is a bare
  `*`, which matches no target that holds a slash: an empty target is every
  target.
- `Attrs`: every `Cond` holds. A condition on an attribute the action does
  not carry does not hold, for every operator, `OpNe` included, except
  `OpExists` with value `false`; an author who means "block unless the
  attribute is present and equal" adds an `OpExists` condition, as the
  package comment shows. Numbers compare as numbers, exactly, whatever
  holds them (an integer type, a float type, `json.Number`); `OpEq` and
  `OpNe` also compare strings and booleans; `OpGt`, `OpGte`, `OpLt`, `OpLte`
  hold only between numbers; `OpIn` holds when the attribute equals any
  element of the list in `Value`; `OpExists` compares presence with the
  boolean in `Value`, and an attribute that is present and `null` is
  present. Attribute names and string values are matched exactly as written,
  as targets and kinds are: `Amount` is not `amount`, and `"prod "` is not
  `"prod"`.

**Three values.** A condition holds, does not hold, or cannot be told. It
cannot be told when the attribute is present but its type is one the
operator cannot compare: a string where a number is wanted, a boolean, a
list, a pointer, `null`, NaN, a number written too long to compare, a float
whose two readings disagree about the condition (below), or a
type different from the one the value is (`OpEq` of the number 5 against the
string `"5"`). `OpIn` is `OpEq` on each element joined by "or", and in three
values: it holds if any element equals the attribute; it does not hold only
if every element could be compared with the attribute and none equalled it;
otherwise it cannot be told. A block rule on `in [22, "ssh"]` therefore
blocks the attribute `"22"`, as `in [22]` and `eq 22` do. It also cannot be told when the condition itself is
malformed, which only a `Policy` that skipped `Validate` can have: an
operator that is not listed, or a value its operator cannot use. A target
pattern that does not compile, on such a `Policy`, cannot be told either. A
rule matches for certain when every condition holds, and does not match when
any condition does not hold, whatever else cannot be told. When no condition
fails but at least one cannot be told, a rule whose effect is ask or block
matches and a rule whose effect is allow does not. What cannot be evaluated
therefore counts toward the stricter outcome, and a model that writes an
amount as the string `"1250"` cannot walk round a limit by it. Strings are
never read as numbers.

A decision reached that way says so. `Decision.Uncertain` names each
attribute whose condition could not be told, in the order of the conditions
(`"target pattern"` and `"kinds list"` for those parts of a malformed rule),
so the record explains why a rule whose condition looks unmet decided. It is
empty when the deciding rule matched for certain, and it belongs to the rule
that decided, not to others that also matched.

**Numbers.** A number is compared exactly. An integer is itself. A float
without an integer value is the shortest decimal that gives it back, so a
`float64` 0.1 is the number 0.1 and meets a threshold written 0.1; comparing
the binary value instead would leave 0.3 below a threshold of 0.3. A float
with an integer value has two readings, that integer and its shortest
decimal. Below 2^53 (below 2^24 for a `float32`) they are one number. Above
they are two: a `float64` 2^70 is 1180591620717411303424 and also
1180591620717411300000, and a `float64` 1e23 is 99999999999999991611392 and
also 1e23. A condition is told under both readings. Where they give the same
outcome that is the outcome: a `float64` 1e23 is above 1e22 and is not above
1e24 or 1e23 whichever it is. Where they differ the condition cannot be told,
and the attribute is named in `Uncertain` like any other: a `float64` 1e23
against `gte 1e23`, or 2^70 against `gt 1180591620717411303000`. When both
sides are such floats their readings are paired, exact with exact and
shortest with shortest, so a float equals itself. So a float cannot be told
against a threshold that lies within its rounding, and a number that is
meant to be exact should reach the package as a `json.Number`, not as a
`float64` that has already rounded it: tool input is decoded with
`json.Decoder.UseNumber`, and the `json.Number` passed on. A `json.Number` is the decimal it
spells, of any size or precision up to 4096 bytes of text and an exponent of
4096, which bounds what a hostile value can cost; text past either bound, or
that is not a JSON number (no plus sign, no leading zero, no `Infinity`), is
not a number, so cannot be told. `Parse` keeps every number in a policy as a
`json.Number`, so a threshold is never passed through a `float64`.

The reference's policy is a fixed shape; in Go it is this rule list, in this
order, and the port of the reference's tests builds it with a test helper:

| Reference | Rule |
|---|---|
| `kinds[k]`, for each kind | `{Name: "Default for <k> actions", Effect: kinds[k], When: {Kinds: [k]}}` |
| `payLimit` | `{Name: "Payment above the $<limit> limit", Effect: Ask, When: {Kinds: ["pay"], Attrs: [{amount gt limit}]}}` |
| `external` | `{Name: "Reaches outside the company", Effect: external, When: {Attrs: [{external eq true}]}}` |
| `sensitive` | `{Name: "Touches sensitive data", Effect: sensitive, When: {Attrs: [{sensitive eq true}]}}` |

Under that list every case in the reference's tests gives the same decision
and the same rule name, including a payment with no amount (the condition
does not hold, as `undefined ?? 0` is not above the limit) and
`external: false` (not a match). A grid of 21,870 policies and actions, with
the reference's answer to each stored in `policy/testdata`, holds the Go
code to it as a standing test.

Go differs from the reference in two places, on purpose. The reference
reads a missing amount as 0, where Go reads the attribute as absent; the two
agree for every limit from 0 up, which is the demonstration's range, and
differ below it. And the reference compares an amount that is not a number
as JavaScript does, where Go cannot tell: `"1250"` asks in both, but `"50"`,
`"abc"`, `null` and NaN are allowed by the reference's coercion and asked
about by Go.

Two names differ, and one function is not carried over:

- The reference calls the middle effect `approve`. Go calls it `Ask`, with
  the wire value `"ask"`, because `agent` has an `Approve` that means a
  person said yes, and a timeline reading "decision: approve" for a call
  still waiting would mislead. `Effect.UnmarshalText` reads `"approve"` as
  `Ask`, so a reference fixture decodes unchanged. A `Decision` marshals as
  `{"decision": …, "rule": …, …}`, the reference's `Verdict` and more.
- `sortActions` is `Policy.Group`.
- `clampLimit` clamps a number typed into the demonstration's form. It is
  not policy and is not ported.

### 5.3 Exported API

*package policy: policy.go, decide.go, parse.go, recorder.go, decider.go*

```go
package policy

// Effect is what a rule says about an action.
type Effect string

// The effects, from least strict to most.
const (
	Allow Effect = "allow"
	Ask   Effect = "ask"
	Block Effect = "block"
)

// Valid reports whether e is one of the three effects.
func (e Effect) Valid() bool

// UnmarshalText reads an effect. It also reads "approve" as Ask, which is
// what the TypeScript reference calls it.
func (e *Effect) UnmarshalText(text []byte) error

// Strictest returns the strictest of effects, or "" for none.
func Strictest(effects ...Effect) Effect

// Action is something an agent is about to do.
type Action struct {
	// Kind is the sort of action: "read", "write", "send", "pay", "delete",
	// "run", or any word a service's rules use.
	Kind string `json:"kind"`
	// Target is what it is done to, such as "email:ap@example.com".
	Target string `json:"target,omitempty"`
	// Attrs are the facts rules decide on: an amount, whether the action
	// reaches outside, whether it touches sensitive data.
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Op is a comparison a Cond makes.
type Op string

// The comparisons.
const (
	OpEq     Op = "eq"
	OpNe     Op = "ne"
	OpGt     Op = "gt"
	OpGte    Op = "gte"
	OpLt     Op = "lt"
	OpLte    Op = "lte"
	OpIn     Op = "in"
	OpExists Op = "exists"
)

// Cond is one condition on an attribute.
type Cond struct {
	Attr  string `json:"attr"`
	Op    Op     `json:"op"`
	Value any    `json:"value,omitempty"`
}

// Match says which actions a rule applies to. Every part that is set must
// hold; the zero Match applies to every action.
type Match struct {
	// Kinds lists the kinds the rule applies to. Empty is every kind.
	Kinds []string `json:"kinds,omitempty"`
	// Target is a path.Match pattern on Action.Target. Empty is every target.
	Target string `json:"target,omitempty"`
	// Attrs must all hold.
	Attrs []Cond `json:"attrs,omitempty"`
}

// Rule gives an effect to the actions it matches.
type Rule struct {
	// Name is recorded as the reason for a decision, so it must say what the
	// rule is in words a person reviewing the decision can read.
	Name   string `json:"name"`
	Effect Effect `json:"effect"`
	When   Match  `json:"when"`
}

// Policy is an ordered list of rules.
type Policy struct {
	// Version labels the rule set in every recorded decision.
	Version string `json:"version,omitempty"`
	// Default is the effect when no rule matches. Empty is Block.
	Default Effect `json:"default,omitempty"`
	Rules   []Rule `json:"rules"`
}

// RuleDefault is the rule name recorded when no rule matched.
const RuleDefault = "no rule matched"

// Decision is the outcome for one action.
type Decision struct {
	Effect Effect `json:"decision"`
	// Rule names the rule that decided, or RuleDefault.
	Rule string `json:"rule"`
	// Index is that rule's position in Policy.Rules, or -1.
	Index int `json:"index"`
	// Matched names every rule that matched, in order.
	Matched []string `json:"matched,omitempty"`
	// Uncertain is set when Rule decided without every one of its conditions
	// holding: a condition could not be told, and an ask or block rule counts
	// that against the action. It names each attribute whose value a condition
	// could not compare, and "target pattern" or "kinds list" for a part of
	// the rule that could not be evaluated. It is empty when Rule matched for
	// certain.
	Uncertain []string `json:"uncertain,omitempty"`
}

// Validate reports the first thing wrong with p.
func (p Policy) Validate() error

// Decide gives the decision for a. It is pure: it reads nothing but p and a,
// and records nothing. A condition that cannot be told counts toward the
// stricter outcome: see 5.2.
func (p Policy) Decide(a Action) Decision

// Judged is an action with its decision.
type Judged struct {
	Action   Action   `json:"action"`
	Decision Decision `json:"decision"`
}

// Grouped is a batch of actions sorted by effect, each group in input order.
type Grouped struct {
	Allow []Judged `json:"allow"`
	Ask   []Judged `json:"ask"`
	Block []Judged `json:"block"`
}

// Group decides every action and sorts them by effect.
func (p Policy) Group(actions []Action) Grouped

// Parse reads a policy from its JSON form and validates it. The document must
// be a JSON object. An unknown field is an error, and so is a key repeated in
// any object, compared without regard to case. Numbers are kept as written,
// as json.Number.
func Parse(data []byte) (Policy, error)

// ErrUnrecordable is what a Recorder wraps in the error it returns for a record
// it can never store, however often it is tried again: a value its storage
// cannot represent, or more of them than it will take. A Recorder that cannot
// write because of how things are at the moment (a database that is down, a
// context that is done) does not wrap it. Whoever retries a decision that failed
// to record retries on the second kind and stops at the first, since a retry of
// a record that can never be stored never ends. The error that wraps it is still
// the Recorder's own, which errors.Is and errors.As can find.
var ErrUnrecordable = errors.New("policy: the record cannot be stored")

// Record is one decision as it is logged.
type Record struct {
	// ID is the store's own number for a record it lists, which with At is the
	// record's place in the log: a Recorder that keeps an ID sets it on the
	// records it returns and ignores it on the ones it is handed. It is zero for
	// a record that no store has numbered, and the in-memory recorder has none.
	ID       int64     `json:"id,omitempty"`
	At       time.Time `json:"at"`
	Action   Action    `json:"action"`
	Decision Decision  `json:"decision"`
	// Version is the Policy.Version that decided.
	Version string `json:"version,omitempty"`
}

// Recorder keeps the decision log. Record returns nil only when the record is
// kept; a Decider returns no decision for one that is not. An error for a record
// that can never be stored, whatever is retried, wraps ErrUnrecordable. Any
// other error is one a later call may not meet.
type Recorder interface {
	Record(ctx context.Context, rec Record) error
}

// MemoryRecorder is the in-process Recorder. It keeps the most recent 1000
// decisions, and what it keeps and returns are deep copies. Its zero value is
// ready to use.
type MemoryRecorder struct{ /* unexported fields */ }

// NewMemoryRecorder builds an empty MemoryRecorder.
func NewMemoryRecorder() *MemoryRecorder

// Record implements Recorder. It fails only when rec holds more than 10000
// values to copy, and the error wraps ErrUnrecordable.
func (m *MemoryRecorder) Record(ctx context.Context, rec Record) error

// Records returns a copy of the log, oldest first.
func (m *MemoryRecorder) Records() []Record

// Options configures a Decider. The zero value records in memory.
type Options struct {
	// Recorder defaults to NewMemoryRecorder.
	Recorder Recorder
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default.
	Logger *slog.Logger
	// MemoryRecords is how many of the most recent decisions the default
	// in-memory recorder keeps. Zero or less keeps 1000. It has no effect when
	// Recorder is set.
	MemoryRecords int
}

// Decider decides actions under one Policy and records every decision.
type Decider struct{ /* unexported fields */ }

// NewDecider builds a Decider. It returns an error when p does not validate.
// The Decider takes a deep copy of the rules, every list and every value in
// them; a policy whose conditions hold more than 10000 values is refused.
func NewDecider(p Policy, opts Options) (*Decider, error)

// Decide decides a and records the decision. When the record cannot be
// written it returns the error and a zero Decision, whose empty Effect no
// caller may read as Allow. An action whose attributes hold more than 10000
// values cannot be copied for the record, and the error wraps ErrUnrecordable,
// as does any a Recorder returns for a record it can never store.
func (d *Decider) Decide(ctx context.Context, a Action) (Decision, error)

// Policy returns the rules this Decider decides under, as a deep copy.
func (d *Decider) Policy() Policy

var _ Recorder = (*MemoryRecorder)(nil)
```

The JSON form of a policy:

```json
{
  "version": "2026-10-02",
  "default": "block",
  "rules": [
    {"name": "Reading is allowed", "effect": "allow", "when": {"kinds": ["read"]}},
    {"name": "Sending needs a person", "effect": "ask", "when": {"kinds": ["send"]}},
    {"name": "Payment above the $200 limit", "effect": "ask",
     "when": {"kinds": ["pay"], "attrs": [{"attr": "amount", "op": "gt", "value": 200}]}},
    {"name": "Deleting documents is never allowed", "effect": "block", "when": {"kinds": ["delete"]}}
  ]
}
```

`Validate` refuses: a rule with no name, two rules with one name (the name
is the record, so it must identify one rule), an effect that is not one of
the three, a `Default` that is neither empty nor one of the three, a target
pattern that does not compile, an operator that is not listed, an `OpIn`
whose value is not a list, an `OpExists` whose value is not a boolean, and a
numeric operator whose value is not a number.

It also refuses each form that can never hold, or holds for every action by
accident, because on a block rule either one fails open without a word, and
it names the rule and the condition as it does the other faults: a `Cond`
with an empty `Attr`; an `OpEq` or `OpNe` whose value is not a number, a
string or a boolean, so no value at all, and no list or object either, since
such a value is equal to nothing and would leave `OpEq` never holding and
`OpNe` always holding; a number that is not finite (NaN, or an infinity,
which nothing is above and every number is below) as the value of any
operator or as an element of an `OpIn` list; an `OpIn` whose list is empty or
has an element that is not a number, a string or a boolean; and a kind in
`Match.Kinds` that is the empty string, which names no action. The empty
`Kinds`, `Target` and `Attrs` are not refused: the field comments above say
each means every kind, every target and no condition. Two conditions that
contradict one another are not detected, with one exception: `OpExists` with
value `false`, which says the attribute is absent, is refused together with
any other condition on the same attribute, since an absent attribute meets
none, and written twice it is refused as a repeat. A bare `*` target is refused, with a message that says to leave the
target empty for every target. A rule named `RuleDefault` is refused, since
the record would then not say whether a rule matched. A decision record
holds the name of the rule that decided and of each that matched, the
policy's version, and the names of the attributes a condition could not tell;
a name and the version go to text columns, and a name is indexed. So a rule's
name, an attribute name and the version are refused when they hold a NUL
character or are not valid UTF-8, and a rule's name and the version when they
are longer than 256 bytes, each with a message that names the rule: a policy
with one would load and then fail to record every decision made under it, or
record something other than what decided. A kind and a target pattern are
never in a record, and are refused for a NUL because one that holds it can
only match an action that could never be recorded.

`Parse` first reads the document once to refuse what the decoder would take
without a word: a document that is not an object (`null` is not an empty
policy), a key repeated in any object at any depth (compared without regard
to case, since the decoder matches keys that way, and the later of
`"effect":"block","effect":"allow"` would otherwise win), and anything after
the document. The error names the key and where it is. It then decodes with
unknown fields disallowed, keeping every number as a `json.Number`, and
validates, so a serialised policy with any of these cannot be loaded, and a
`Parse(Marshal(p))` is `p` when `p`'s numbers are `json.Number` values, as
`Parse` returns them.

*package pg: policy/pg/store.go, migrations.go*

```go
// Package pg is the Postgres-backed policy.Recorder.
package pg

// Table is the name of the table policy/pg/migrations creates.
const Table = "policy_decisions"

// MigrationsFS embeds this package's migrations.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

type conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store is a policy.Recorder backed by Postgres.
type Store struct{ /* unexported fields */ }

// New builds a Store over db.
func New(db conn) *Store

// Record implements policy.Recorder. An error for a record that can never be
// stored wraps policy.ErrUnrecordable; one that is the database's does not.
// Over a pgx.Tx the insert runs in a savepoint, rolled back when it fails, so
// that a refusal never leaves the caller's transaction aborted.
func (s *Store) Record(ctx context.Context, rec policy.Record) error

// Cursor is a position in the log, as agent.Cursor is in a run listing: the
// time a decision was decided and its ID.
type Cursor struct {
	At time.Time
	ID int64
}

// Filter narrows List. The zero Filter lists the newest decisions. Effect,
// Rule and Kind match exactly, and so case-sensitively.
type Filter struct {
	Effect policy.Effect
	Rule   string
	Kind   string
	Since  time.Time
	// Before returns decisions older than this position. A cursor whose ID is
	// below 1, or whose time is outside the years 1 to 9999, is an error.
	Before *Cursor
	// Limit defaults to 100 and is at most 1000: a larger number is 1000.
	Limit int
}

// List returns a page of decisions, newest first by the time decided and then
// by ID, each with its ID. To read the next page, set Before to the At and ID
// of the last record of this one.
func (s *Store) List(ctx context.Context, f Filter) ([]policy.Record, error)

var _ policy.Recorder = (*Store)(nil)
```

### 5.4 Table

`policy/pg/migrations/001_policy_decisions.up.sql`:

```sql
-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

-- The decision log. Append-only: there is no update and no delete, so it is
-- a record of what was decided and why.
CREATE TABLE IF NOT EXISTS policy_decisions (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    decided_at     TIMESTAMPTZ NOT NULL,
    kind           TEXT NOT NULL,
    target         TEXT NOT NULL DEFAULT '',
    attrs          JSONB NOT NULL DEFAULT '{}'::jsonb,
    effect         TEXT NOT NULL CHECK (effect IN ('allow', 'ask', 'block')),
    rule           TEXT NOT NULL,
    rule_index     INTEGER NOT NULL,
    matched        JSONB NOT NULL DEFAULT '[]'::jsonb,
    uncertain      JSONB NOT NULL DEFAULT '[]'::jsonb,
    policy_version TEXT NOT NULL DEFAULT ''
);

-- List reads newest first, optionally narrowed by effect or rule. The rule index
-- is in that order within a rule, so a filter on one reads only its rows.
CREATE INDEX IF NOT EXISTS policy_decisions_decided_idx ON policy_decisions (decided_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS policy_decisions_rule_idx ON policy_decisions (rule, decided_at DESC, id DESC);
```

`attrs`, `matched` and `uncertain` are written as `{}`, `[]` and `[]` when
the record's own are nil, never as JSON `null`, and `decided_at` is stored in
UTC. `uncertain` is what `Decision.Uncertain` carries: the attributes a
decision could not tell, so a reviewer reading the table sees why an ask or
block rule decided on a condition that looks unmet.

The listing orders by `(decided_at, id)`, which is the order of
`policy_decisions_decided_idx`, and pages with a row comparison on it,
`(decided_at, id) < ($time, $id)`, so a page is a short index scan however
far back it is, and records that share a time are each on one page and no
more. A filter on a rule reads `policy_decisions_rule_idx`, which is in the
same order within a rule, `(rule, decided_at DESC, id DESC)`, and so reads
only that rule's rows and does not sort them; with the index on the rule and
the id alone it read them all and sorted. `id` is what the log gives a
record when it is listed, as `Record.ID`. `jsonb` keeps a number as a
`numeric`, with every digit and the scale it was written with, up to 131072
digits before the point and 16383 after it, an exponent counting as it
expands; it refuses `\u0000`, a number past either limit, and a value nested
more deeply than the server's stack, and each of those is an error that no
retry changes. A time is held to years 1 to 9999.

The down file drops the two indexes and the table.

### 5.5 Interfaces consumed

`Recorder`, one method, declared here. `*slog.Logger`. `policy/pg` consumes
the two methods of `*pgxpool.Pool` it uses, `Exec` and `Query`, through an
unexported interface, as `jobs/pg` does.

### 5.6 Failure and concurrency

`Policy.Decide` is pure and total: it cannot fail, reads nothing but its
arguments, and may be called from any number of goroutines. A `Decider` is
immutable after `NewDecider`: it holds a deep copy of the policy, every
list and nested value, and `Policy()` hands out another, so nothing a caller
does afterwards, from any goroutine, changes the rules it decides under; to
change the rules, build another. A `Recorder` is handed a deep copy of the
record, so neither side can change what the other holds. What Go cannot
copy (a function, a channel, the unexported fields of a struct) is shared.
A copy is bounded in work: a policy whose conditions hold more than 10000
values between them is refused by `NewDecider`, and an action whose
attributes hold more is a record that cannot be written, so `Decide`
returns an error and a zero `Decision`. `MemoryRecorder.Record` refuses such
a record too, and its zero value is ready to use. The errors for a record
that is too large wrap `ErrUnrecordable`, as the next paragraphs say.

`Decider.Decide` computes the decision and then records it. If the record
cannot be written, it returns the error and a zero `Decision`. The zero
`Effect` is not `Allow`, and every caller in this design treats an effect
that is not one of the three as block, so a decision that could not be
recorded is never acted on as an allow. A `Recorder` that panics takes
`Decide` with it, and no decision is returned. The decision log is
append-only: `policy/pg` has an insert and a read, and no update or delete.

A record that can never be stored is told apart from a store that is down. A
`Recorder` wraps `ErrUnrecordable` in the error it returns for a record that
no retry will make storable, and does not for a failure of the moment, so a
caller that retries a step whose decision failed to record retries the second
and stops at the first. `Decider.Decide` wraps it for an action with more than
10000 values, `MemoryRecorder.Record` for a record with more, and
`policy/pg` for what it refuses before it sends anything (a NUL character, a
decision whose effect is none of the three, a kind, target, rule or version
that is not valid UTF-8, a time outside the years 1 to 9999, an index outside
an integer column, attributes JSON cannot hold, an empty `json.Number`) and
for the errors the server gives of the record's own content or size: SQLSTATE
class 22, a data exception, and class 54, a program limit. A refused connection, a cancelled context, an aborted transaction and
a server shutting down are the store's, and do not wrap it. The `Decider`
returns a zero `Decision` either way. A NUL, bytes that are not UTF-8, or too
much length in a policy's names or version is refused by `Validate`, since
otherwise every decision made under that rule would fail to record, for ever.

A store built over a `pgx.Tx` shares the caller's transaction, and a statement
the server refuses aborts a transaction, so that every statement after it
fails until the rollback. `policy/pg` therefore refuses in Go what it can,
and runs the insert in a savepoint when its connection is a transaction,
rolled back to when it fails: whatever it refuses, the caller's transaction
goes on. Over a pool the insert is one statement. A page is read from the
records already committed: a record that commits late with a time older than
the cursor of a pass that is already beyond it is not in that pass, and a
fresh pass from the newest finds it.

An invalid policy cannot reach `Decide` through a `Decider`, since
`NewDecider` validates. Called on a `Policy` value directly, `Decide` treats
a rule with an unknown effect as block, and a rule whose match cannot be
evaluated (a pattern that does not compile, a condition with an operator
that is not listed or a value its operator cannot use, a list of kinds with
an empty entry) as cannot be told: it matches unless its effect is allow,
and says so in `Decision.Uncertain`. "Does not match" would be the safe
reading only for an allow rule. The other forms `Validate` refuses are
decided as written.

### 5.7 In-process default

`MemoryRecorder`, which is what `Options.Recorder` is when nil. It keeps the
most recent 1000 decisions, or `Options.MemoryRecords`, and drops the
oldest: with the zero `Options` it cannot be read, so it must not grow with
every decision.

### 5.8 Tests

Without a database:

- The reference's `decide` and `sortActions` cases, ported one for one with
  the same policy, the same actions and the same expected rule names. The
  port lives in `policy/reference_test.go` and names the file it mirrors.
- A table over `Match`: each operator, each Go number type against each,
  an absent attribute under each operator, kinds, target patterns.
- The three values: each operator against each type it cannot compare;
  conditions combined; a malformed rule under an allow, an ask and a block
  effect; numbers exactly, including text of any size within the bounds and
  past them. The reviewer's case: an allow by kind and an ask above a limit,
  with the amount as a string, a pointer, a list, NaN and a number too large
  for a `float64`.
- The grid comparison with the reference, as a standing test against
  `policy/testdata/reference_grid.json`.
- No rule matched: `Block` by default, `Policy.Default` when set,
  `RuleDefault` reported, `Index` -1.
- `Validate`, one case per refusal above. `Parse` round-trip:
  `Parse(Marshal(p))` equals `p`; an unknown field is an error; a repeated
  key is an error at each level, in any case; a document that is not an
  object is an error; `"approve"` decodes as `Ask`; thresholds stay exact.
- `Decider`: each decision is recorded once with the policy's version; a
  recorder that fails yields an error and a zero decision. The case that
  must fail: a test asserting the zero decision is not `Allow`, so that a
  change making the failure path return the computed decision goes red.
  A cancelled context; a recorder that panics; a policy with conditions,
  including an `in` list, changed after `NewDecider` and after `Policy()`,
  and under `-race`; a recorder handed a copy; the bounded memory log; the
  zero value of `MemoryRecorder`, alone and under a `Decider`; a value that
  shares a child at every level.

With `pg/testdb`: `policy/pg` records and lists; filters by effect, rule,
kind and time, case-sensitively; attributes survive the round trip, numbers
exactly; concurrent records all land. Paging: twelve records recorded in a
scrambled order, several sharing a time, read back by cursor in pages of every
size from 1 to 13 and compared with an order worked out by hand, each record
once; each filter, alone and combined, paged and compared with the same
filter in one piece; a cursor past the end, before everything, in another
zone and finer than a microsecond; a malformed cursor refused before a query;
a limit of a million giving pages of 1000 and the cursor reaching the rest.
The sentinel: every refusal of the store, and each server error class, wraps
`policy.ErrUnrecordable` and a refused connection, a closed pool, a
cancelled context and an aborted transaction do not; through `New(tx)`, after
each refusal the next statement in the caller's transaction succeeds, and the
statements sent over a pool and over a transaction are as the package says. The
server is asked for the plan of the statement `List` runs for a rule filter
with a cursor, which must read the rule index and not sort. A NUL, a number
past `numeric`'s range, a value nested too deeply, a lone surrogate and an
empty `json.Number` are each refused. Its migrations, with the index order,
are found by the repository-wide replay check without wiring.

### 5.9 Left out

- Storing rule sets in Postgres, versioning them, and reloading them while
  running. A policy is loaded from JSON at startup.
- Rules that read anything but the action: time of day, rate, history.
- An expression language. Conditions are a list that must all hold; a rule
  that needs "or" is two rules.
- Who is acting, as a field. The actor is an attribute like any other, and
  `agent` sets it.

## 6. `agent`

### 6.1 Purpose

`agent` runs an agent to completion across process deaths. A run is a
journal in Postgres. The package owns the loop (ask the model, judge each
tool call, execute it, feed the results back), the journal that makes the
loop resumable, the lease that lets any process pick a run up, the budgets
that stop it, the approvals that park it for a person, and child runs.

It imports no model and no rule engine. It declares the four small
interfaces it consumes, `Model`, `Guard`, `Clock` and `Publisher`, and
section 8 shows the adapters.

| Package | Holds |
|---|---|
| `agent` | Types, the `Store` contract, `Engine`, `MemoryStore`. Imports the standard library and `github.com/google/uuid` |
| `agent/pg` | The Postgres `Store`, its migration, `Once` |
| `agent/httpapi` | The HTTP surface, on `httpx` |
| `agent/agenttest` | A clock, a scripted model, a fault-injecting store, and the `Store` contract suite |

### 6.2 Exported API

The contract files, in full. Task T0 lands these.

*package agent: agent.go, journal.go, events.go, errors.go*

```go
package agent

// ---- what the package consumes ----

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

// ---- conversation ----

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

// ---- actions and decisions ----

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

// ---- definitions ----

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

// ---- the journal ----

// Status is where a run stands.
type Status string

// The run statuses. A runnable run with a live lease is being executed.
const (
	StatusRunnable  Status = "runnable"
	StatusWaiting   Status = "waiting"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Reasons recorded on a run: why it waits, or why it ended as it did.
const (
	ReasonApproval      = "approval"
	ReasonChildren      = "children"
	ReasonTimeBudget    = "time_budget"
	ReasonCostBudget    = "cost_budget"
	ReasonTokenBudget   = "token_budget"
	ReasonModelCalls    = "model_calls"
	ReasonRefusal       = "refusal"
	ReasonTruncated     = "truncated"
	ReasonContextWindow = "context_window"
	ReasonError         = "error"
	ReasonAbandoned     = "abandoned"
	ReasonCancelled     = "cancelled"
)

// Lease identifies one hold on a run. Epoch rises by one each time the run
// is claimed, and every journal write names the Epoch it was made under.
type Lease struct {
	RunID string
	Owner string
	Epoch int64
}

// Run is one execution of an agent.
type Run struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Status Status `json:"status"`
	Reason string `json:"reason,omitempty"`
	Input  string `json:"input"`
	Output string `json:"output,omitempty"`
	// Error is the last failure. A failed execution records it, and it stays
	// through later executions that go well, until the run ends and records
	// its own, which is none for a run that ended well.
	Error string `json:"error,omitempty"`

	// ParentID and ParentSeq name the tool step of the run that started
	// this one. Depth is 0 for a run nobody delegated.
	ParentID  string `json:"parent_id,omitempty"`
	ParentSeq int    `json:"parent_seq,omitempty"`
	Depth     int    `json:"depth,omitempty"`

	// Key is the caller's idempotency key for starting the run.
	Key        string   `json:"key,omitempty"`
	Definition Snapshot `json:"definition"`
	// Metadata is the caller's own. It reads back as JSON gives it, and as
	// an empty map when the run was started with none.
	Metadata map[string]string `json:"metadata,omitempty"`

	Usage        Usage `json:"usage"`
	ModelCalls   int   `json:"model_calls"`
	ActiveMillis int64 `json:"active_ms"`

	// Rev rises by one with every change to the run, its steps or its
	// approvals.
	Rev int64 `json:"rev"`

	LeaseOwner     string     `json:"lease_owner,omitempty"`
	LeaseEpoch     int64      `json:"lease_epoch"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	// Failures counts executions in a row that ended in an error or a lapsed
	// lease. Any completed step resets it.
	Failures      int        `json:"failures,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	CancelRequested bool   `json:"cancel_requested,omitempty"`
	CancelBy        string `json:"cancel_by,omitempty"`
	CancelReason    string `json:"cancel_reason,omitempty"`

	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Lease returns the hold the run records.
func (r Run) Lease() Lease { return Lease{RunID: r.ID, Owner: r.LeaseOwner, Epoch: r.LeaseEpoch} }

// Terminal reports whether the run has ended.
func (r Run) Terminal() bool {
	return r.Status == StatusCompleted || r.Status == StatusFailed || r.Status == StatusCancelled
}

// Running reports whether the run is being executed at now.
func (r Run) Running(now time.Time) bool {
	return r.Status == StatusRunnable && r.LeaseExpiresAt != nil && r.LeaseExpiresAt.After(now)
}

// StepKind is what a step is.
type StepKind string

// The step kinds.
const (
	StepModel StepKind = "model"
	StepTool  StepKind = "tool"
)

// StepStatus is where a step stands.
type StepStatus string

// The step statuses. A model step is started, then completed. A tool step
// is proposed when the model asks for it, and from there blocked, or
// waiting and then declined, or started and then completed.
const (
	StepProposed  StepStatus = "proposed"
	StepWaiting   StepStatus = "waiting"
	StepStarted   StepStatus = "started"
	StepCompleted StepStatus = "completed"
	StepBlocked   StepStatus = "blocked"
	StepDeclined  StepStatus = "declined"
)

// Done reports whether the step has its final result.
func (s StepStatus) Done() bool {
	return s == StepCompleted || s == StepBlocked || s == StepDeclined
}

// Step is one entry in a run's journal: a model call or a tool call.
type Step struct {
	RunID  string     `json:"run_id"`
	Seq    int        `json:"seq"`
	Kind   StepKind   `json:"kind"`
	Status StepStatus `json:"status"`
	// Name is the tool's name, or the model that answered.
	Name string `json:"name,omitempty"`

	// Message and Stop are a completed model step's reply.
	Message *Message `json:"message,omitempty"`
	Stop    Stop     `json:"stop,omitempty"`

	// Turn is the model step that proposed a tool step. Call is the call as
	// the model wrote it, and Key its idempotency key.
	Turn int    `json:"turn,omitempty"`
	Call *Call  `json:"call,omitempty"`
	Key  string `json:"key,omitempty"`
	// Decision and Rule are the Guard's answer for a tool step.
	Decision Effect `json:"decision,omitempty"`
	Rule     string `json:"rule,omitempty"`
	// Result and IsError are what the tool step returned to the model.
	Result  string `json:"result,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	// ChildRunID is the run a delegating tool step started.
	ChildRunID string `json:"child_run_id,omitempty"`

	// Attempts counts how many times the step has started.
	Attempts int   `json:"attempts"`
	Usage    Usage `json:"usage"`
	Rev      int64 `json:"rev"`

	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// StepKey is the idempotency key of the tool step at seq in run runID.
func StepKey(runID string, seq int) string {
	return runID + ":" + strconv.Itoa(seq)
}

// ApprovalStatus is where an approval stands.
type ApprovalStatus string

// The approval statuses.
const (
	ApprovalPending   ApprovalStatus = "pending"
	ApprovalApproved  ApprovalStatus = "approved"
	ApprovalDeclined  ApprovalStatus = "declined"
	ApprovalExpired   ApprovalStatus = "expired"
	ApprovalCancelled ApprovalStatus = "cancelled"
)

// ApprovalCause is why a person is being asked.
type ApprovalCause string

// The causes.
const (
	CauseGuard       ApprovalCause = "guard"
	CauseTool        ApprovalCause = "tool"
	CauseInterrupted ApprovalCause = "interrupted"
)

// Approval is a question put to a person about one tool step.
type Approval struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	Seq   int    `json:"seq"`
	// Attempt is the step's Attempts when the question was asked, so an
	// interrupted step can be asked about again.
	Attempt int           `json:"attempt"`
	Cause   ApprovalCause `json:"cause"`
	Tool    string        `json:"tool"`
	// Input is the call's arguments: exactly what runs if approved.
	Input json.RawMessage `json:"input"`
	// Action is what the Guard was asked about. Its Attrs read back as JSON
	// gives them, and as an empty map when there were none. A number comes
	// back as a float64, so an integer above 2^53 is no longer exact: put
	// one that must be in a string.
	Action Action `json:"action"`
	Rule   string `json:"rule"`

	Status    ApprovalStatus `json:"status"`
	DecidedBy string         `json:"decided_by,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	Rev       int64          `json:"rev"`

	RequestedAt time.Time `json:"requested_at"`
	// DecidedAt is when the approval stopped being pending, whether a person
	// answered it, it lapsed, or its run ended. It is nil exactly while the
	// approval is pending.
	DecidedAt *time.Time `json:"decided_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Changes is what happened to a run after a revision.
type Changes struct {
	Run       Run        `json:"run"`
	Steps     []Step     `json:"steps"`
	Approvals []Approval `json:"approvals"`
}

// Cursor is a position in a run listing.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// RunFilter narrows ListRuns. The zero value lists every run, newest first.
type RunFilter struct {
	Status   Status
	Agent    string
	ParentID string
	// Before returns runs older than this position.
	Before *Cursor
	// Limit defaults to 50 and is capped at 200.
	Limit int
}

// ApprovalFilter narrows ListApprovals. The zero value lists every
// approval, oldest first.
type ApprovalFilter struct {
	Status ApprovalStatus
	RunID  string
	// Limit defaults to 50 and is capped at 200.
	Limit int
}

// ---- the store ----

// ClaimRequest asks for a run to execute.
type ClaimRequest struct {
	// Owner names who will hold the run. It must not be empty.
	Owner string
	// Agents limits the claim to runs of these agents.
	Agents []string
	// RunID, when set, claims that run or fails: with ErrNotFound when there
	// is no such run, and with ErrNotClaimable when it cannot be taken. A
	// claim by RunID waits for a write to the run that is in progress and
	// then decides. A claim without one passes over a run being written to.
	RunID string
	Now   time.Time
	// TTL is how long the lease lasts without a heartbeat. It must be more
	// than zero.
	TTL time.Duration
}

// YieldRequest gives a run back without finishing it.
type YieldRequest struct {
	// Failed marks the execution as failed: Failures rises by one and Error
	// is recorded.
	Failed bool
	Error  string
	// NextAttemptAt, when set, hides the run from Claim until then.
	NextAttemptAt *time.Time
	Now           time.Time
}

// ParkRequest asks to set a run waiting.
type ParkRequest struct {
	// Reason is ReasonApproval or ReasonChildren.
	Reason string
	Now    time.Time
}

// FinishRequest ends a run.
type FinishRequest struct {
	// Status is StatusCompleted, StatusFailed or StatusCancelled.
	Status Status
	Reason string
	Output string
	Error  string
	Now    time.Time
}

// CompleteModelRequest records a model's reply.
type CompleteModelRequest struct {
	Seq     int
	Message Message
	Stop    Stop
	Model   string
	Usage   Usage
	Now     time.Time
}

// StepUpdate moves a tool step from one status to another.
type StepUpdate struct {
	Seq int
	// From is the status the step must be in.
	From StepStatus
	To   StepStatus
	// Decision and Rule are recorded when Decision is not empty.
	Decision Effect
	Rule     string
	// Result and IsError are recorded when Result is not nil.
	Result  *string
	IsError bool
	// ChildRunID is recorded when not empty.
	ChildRunID string
	// Usage is added to the step and to the run's totals.
	Usage Usage
	Now   time.Time
}

// ApprovalRequest parks a tool step for a person.
type ApprovalRequest struct {
	// ID is the new approval's id.
	ID  string
	Seq int
	// From is the status the step must be in: StepProposed, or StepStarted
	// for an interrupted call.
	From     StepStatus
	Cause    ApprovalCause
	Action   Action
	Decision Effect
	Rule     string
	// ExpiresAt, when set, is when an unanswered approval lapses.
	ExpiresAt *time.Time
	Now       time.Time
}

// DecideRequest answers an approval.
type DecideRequest struct {
	ID       string
	Approved bool
	By       string
	Reason   string
	Now      time.Time
}

// CancelRequest asks for a run to be cancelled.
type CancelRequest struct {
	RunID  string
	By     string
	Reason string
	Now    time.Time
}

// Store keeps runs and their journals. Every method is one transaction.
//
// The journal methods take the caller's Lease and fail with ErrLeaseLost
// when the run has been claimed again since, so a process that lost a run
// cannot write to it. agent/pg is the Postgres implementation and
// MemoryStore the in-process one; agenttest.RunStoreSuite is the contract
// both pass.
type Store interface {
	// CreateRun inserts run as runnable. When run.Key is set and a run of
	// the same agent already has it, that run is returned and created is
	// false.
	CreateRun(ctx context.Context, run Run) (stored Run, created bool, err error)
	GetRun(ctx context.Context, id string) (Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)

	// Claim takes the oldest runnable run whose lease is free or lapsed and
	// whose NextAttemptAt has passed, raising its Epoch. It returns nil
	// when there is none. Taking over a lapsed lease counts as a failure.
	Claim(ctx context.Context, req ClaimRequest) (*Run, error)
	// Heartbeat extends the lease to now plus ttl and reports whether
	// cancellation has been requested.
	Heartbeat(ctx context.Context, lease Lease, now time.Time, ttl time.Duration) (cancelRequested bool, err error)
	// Yield releases the lease and leaves the run runnable.
	Yield(ctx context.Context, lease Lease, req YieldRequest) error
	// Park sets the run waiting and releases the lease, but only while it
	// has something to wait for: a pending approval, or a child run that
	// has not ended. Otherwise, or when cancellation has been requested,
	// it changes nothing and reports false.
	Park(ctx context.Context, lease Lease, req ParkRequest) (parked bool, err error)
	// Finish ends the run, releases the lease, cancels its pending
	// approvals, and makes a waiting parent runnable.
	Finish(ctx context.Context, lease Lease, req FinishRequest) error

	// Steps returns the journal in order.
	Steps(ctx context.Context, runID string) ([]Step, error)
	// BeginModel records that the model call at seq has started. Called
	// again for a step still started, it counts another attempt.
	BeginModel(ctx context.Context, lease Lease, seq int, now time.Time) error
	// CompleteModel records the reply at seq, adds its usage to the run,
	// and appends one proposed tool step per call in the reply, at the
	// following sequence numbers.
	CompleteModel(ctx context.Context, lease Lease, req CompleteModelRequest) error
	// UpdateStep moves a tool step. It fails with ErrConflict when the step
	// is not in req.From.
	UpdateStep(ctx context.Context, lease Lease, req StepUpdate) error
	// RequestApproval sets a tool step waiting and records the question.
	// Asked again for the same step and attempt, it returns the approval
	// already recorded.
	RequestApproval(ctx context.Context, lease Lease, req ApprovalRequest) (Approval, error)

	GetApproval(ctx context.Context, id string) (Approval, error)
	ListApprovals(ctx context.Context, f ApprovalFilter) ([]Approval, error)
	// DecideApproval answers a pending approval and makes its run runnable.
	// For one already answered it returns that answer with
	// ErrAlreadyDecided.
	DecideApproval(ctx context.Context, req DecideRequest) (Approval, error)
	// ExpireApprovals lapses every pending approval past its ExpiresAt,
	// makes their runs runnable, and reports how many lapsed.
	ExpireApprovals(ctx context.Context, now time.Time) (int, error)
	// RequestCancel marks the run for cancellation and makes it runnable if
	// it was waiting. It fails with ErrFinished for a run that has ended.
	RequestCancel(ctx context.Context, req CancelRequest) error

	// Changes returns the run, and the steps and approvals whose Rev is
	// greater than since.
	Changes(ctx context.Context, runID string, since int64) (Changes, error)
}

// ---- notifications ----

// TopicRuns is the topic every Event is published to.
const TopicRuns = "agent.runs"

// Event types.
const (
	EventRunStarted        = "run.started"
	EventRunWaiting        = "run.waiting"
	EventRunCompleted      = "run.completed"
	EventRunFailed         = "run.failed"
	EventRunCancelled      = "run.cancelled"
	// EventRunCancelRequested says a run was asked to stop and is still to
	// end; EventRunCancelled says it has ended, cancelled.
	EventRunCancelRequested = "run.cancel_requested"
	EventStepStarted       = "step.started"
	EventStepCompleted     = "step.completed"
	EventStepBlocked       = "step.blocked"
	EventApprovalRequested = "approval.requested"
	EventApprovalDecided   = "approval.decided"
)

// Event says that a run changed. It carries nothing from the journal: a
// subscriber that wants the change reads it with Engine.Changes.
type Event struct {
	Type  string    `json:"type"`
	RunID string    `json:"run_id"`
	Agent string    `json:"agent"`
	Seq   int       `json:"seq,omitempty"`
	At    time.Time `json:"at"`
}

// ---- errors ----

// Errors a caller has a decision to make about.
var (
	ErrNotFound       = errors.New("agent: not found")
	ErrUnknownAgent   = errors.New("agent: no such agent registered")
	ErrLeaseLost      = errors.New("agent: lease lost")
	ErrNotClaimable   = errors.New("agent: run cannot be claimed")
	ErrConflict       = errors.New("agent: step is not in the expected status")
	ErrAlreadyDecided = errors.New("agent: approval already decided")
	ErrFinished       = errors.New("agent: run has ended")

	// ErrTransient marks a tool error as worth trying again: the call stays
	// started and is executed again later with the same Key.
	ErrTransient = errors.New("agent: transient failure")
	// ErrPermanent marks a Model error as one no retry will fix: the run
	// fails at once.
	ErrPermanent = errors.New("agent: permanent failure")
)
```

The engine:

*package agent: engine.go, memory.go*

```go
package agent

// Options configures an Engine. Only Model is required.
type Options struct {
	// Model answers every model step. Required.
	Model Model
	// Store defaults to NewMemoryStore, which keeps runs for the life of the
	// process. Use agent/pg for runs that survive it.
	Store Store
	// Guard judges every tool call. Nil allows every call and records
	// RuleNoGuard as the reason.
	Guard Guard
	// Clock defaults to the system clock.
	Clock Clock
	// Events receives an Event after each change. Nil publishes nothing.
	Events Publisher
	// Logger defaults to slog.Default.
	Logger *slog.Logger
	// NewID makes ids for runs and approvals. Default a random UUID. A store
	// keeps a run or an approval only under a UUID, written as uuid.NewString
	// writes one, so a NewID of one's own must return those.
	NewID func() string

	// WorkerID names this process in leases. Default host name, process id
	// and a random suffix.
	WorkerID string
	// LeaseTTL is how long a claim lasts without a heartbeat, and so how
	// long a run whose process died waits to be taken over. Default 30
	// seconds.
	LeaseTTL time.Duration
	// HeartbeatInterval defaults to a third of LeaseTTL. It must be below
	// LeaseTTL, or New refuses the Options: a lease that is not extended
	// before it lapses is taken over while its holder still works.
	HeartbeatInterval time.Duration
	// PollInterval is how often Work looks for a run when it found none.
	// Default 1 second.
	PollInterval time.Duration
	// Concurrency is how many runs Work executes at once. Default 4.
	Concurrency int
	// MaxFailures is how many executions in a row may fail before the run
	// does. Default 5.
	MaxFailures int
	// RetryBase and RetryMax bound the wait after a failed execution, which
	// doubles from RetryBase. Defaults 1 second and 1 minute.
	RetryBase time.Duration
	RetryMax  time.Duration
	// DrainTimeout is how long Work lets a step in flight finish after its
	// context is cancelled. Default 10 seconds.
	DrainTimeout time.Duration
	// ApprovalTTL is how long an approval may stay unanswered before it
	// lapses, which declines the call. Zero never lapses.
	ApprovalTTL time.Duration
	// MaxDepth bounds delegation. Default 3.
	MaxDepth int
}

// StartRequest starts a run.
type StartRequest struct {
	// Agent names a registered Definition.
	Agent string
	Input string
	// Key makes the start idempotent: a second Start with the same Agent
	// and Key returns the first run.
	Key string
	// Limits, when set, replaces the Definition's limits for this run.
	Limits   *Limits
	Metadata map[string]string
}

// Report counts what one Tick did.
type Report struct {
	Claimed   int
	Completed int
	Failed    int
	Cancelled int
	Parked    int
	Yielded   int
}

// Engine starts runs, executes them, and answers for them.
type Engine struct{ /* unexported fields */ }

// New builds an Engine. It returns an error when Model is nil, and when
// HeartbeatInterval is not below LeaseTTL once both have their defaults.
func New(opts Options) (*Engine, error)

// Register adds an agent. It returns an error for a Definition that does
// not validate or a name already registered.
func (e *Engine) Register(def Definition) error

// Start records a new runnable run and returns it. It executes nothing.
func (e *Engine) Start(ctx context.Context, req StartRequest) (Run, error)

// Execute claims run runID and executes it until it ends or parks, and
// returns it as it then stands. It returns ErrNotClaimable when another
// process holds the run or it is not runnable.
func (e *Engine) Execute(ctx context.Context, runID string) (Run, error)

// Tick is one pass: it lapses overdue approvals, claims runnable runs, up
// to Concurrency of them, executes each until it ends or parks, and
// returns when all have.
func (e *Engine) Tick(ctx context.Context) (Report, error)

// Work keeps up to Concurrency executions going until ctx is cancelled:
// it claims a run whenever a slot is free and looks again after
// PollInterval when there is none. Then it waits up to DrainTimeout for
// steps in flight and returns ctx.Err().
func (e *Engine) Work(ctx context.Context) error

// GetRun returns one run.
func (e *Engine) GetRun(ctx context.Context, id string) (Run, error)

// ListRuns lists runs, newest first.
func (e *Engine) ListRuns(ctx context.Context, f RunFilter) ([]Run, error)

// Timeline returns a run's journal in order.
func (e *Engine) Timeline(ctx context.Context, runID string) ([]Step, error)

// Changes returns what happened to a run after revision since.
func (e *Engine) Changes(ctx context.Context, runID string, since int64) (Changes, error)

// ListApprovals lists approvals, oldest first.
func (e *Engine) ListApprovals(ctx context.Context, f ApprovalFilter) ([]Approval, error)

// Approve answers an approval yes, on behalf of by.
func (e *Engine) Approve(ctx context.Context, approvalID, by, reason string) (Approval, error)

// Decline answers an approval no, on behalf of by.
func (e *Engine) Decline(ctx context.Context, approvalID, by, reason string) (Approval, error)

// Cancel asks for a run and its child runs to be cancelled.
func (e *Engine) Cancel(ctx context.Context, runID, by, reason string) error

// MemoryStore is the in-process Store.
type MemoryStore struct{ /* unexported fields */ }

// NewMemoryStore builds an empty MemoryStore.
func NewMemoryStore() *MemoryStore
```

*package pg: agent/pg/store.go, once.go, migrations.go*

```go
// Package pg is the Postgres-backed agent.Store.
package pg

// Table names the migrations create.
const (
	RunsTable      = "agent_runs"
	StepsTable     = "agent_steps"
	ApprovalsTable = "agent_approvals"
	EffectsTable   = "agent_tool_effects"
)

// MigrationsFS embeds this package's migrations.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// Store is an agent.Store backed by Postgres.
type Store struct{ /* unexported fields */ }

// New builds a Store over db, usually a *pgxpool.Pool, which must already
// have the schema from agent/pg/migrations applied. keelpg is Keel's own pg
// package: every method here is one pg.InTx.
func New(db keelpg.Beginner) *Store

// Purge deletes runs that ended before olderThan and were not started by
// another run, with their steps, approvals and child runs, and reports how
// many it removed. The keys Once recorded for a removed run's steps go with
// it. A purge that Postgres ends for a deadlock is tried once more.
func (s *Store) Purge(ctx context.Context, olderThan time.Time) (int64, error)

// Once records key in tx and reports whether this is the first time it has
// been recorded. A tool calls it with Invocation.Key inside the transaction
// that makes its change, and skips the change when it reports false.
func Once(ctx context.Context, tx pgx.Tx, key string) (first bool, err error)

var _ agent.Store = (*Store)(nil)
```

*package agenttest: agent/agenttest*

```go
// Package agenttest is what tests of agents are built from.
package agenttest

// Clock is an agent.Clock a test moves by hand.
type Clock struct{ /* unexported fields */ }

// NewClock builds a Clock reading start.
func NewClock(start time.Time) *Clock

// Now implements agent.Clock.
func (c *Clock) Now() time.Time

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration)

// Script chooses the reply to a request. turn is how many assistant turns
// the request already holds.
type Script func(req agent.Request, turn int) (agent.Response, error)

// Replies is a Script that plays replies in order, one per turn.
func Replies(replies ...agent.Response) Script

// ByAgent is a Script that picks another by Request.Agent.
func ByAgent(scripts map[string]Script) Script

// Say is a final answer.
func Say(text string) agent.Response

// Use is a turn that calls tools.
func Use(calls ...agent.Call) agent.Response

// Call builds a tool call from its id, tool name and JSON arguments.
func Call(id, name, input string) agent.Call

// Model is a scripted agent.Model that records what it was asked.
type Model struct{ /* unexported fields */ }

// NewModel builds a Model over script. A nil script is one with no replies.
func NewModel(script Script) *Model

// Generate implements agent.Model.
func (m *Model) Generate(ctx context.Context, req agent.Request) (agent.Response, error)

// Requests returns every request received, in order.
func (m *Model) Requests() []agent.Request

// GuardFunc adapts a function to agent.Guard.
type GuardFunc func(ctx context.Context, a agent.Action) (agent.Decision, error)

// Decide implements agent.Guard.
func (f GuardFunc) Decide(ctx context.Context, a agent.Action) (agent.Decision, error) {
	return f(ctx, a)
}

// ErrKilled is what a FaultStore returns once it has been killed.
var ErrKilled = errors.New("agenttest: store killed")

// ErrFault is what a FaultStore returns for a call that FailBefore or
// FailAfter chose. The store goes on working after it.
var ErrFault = errors.New("agenttest: store fault")

// FaultStore wraps a Store so a test can stop a process at a chosen store
// call, the way a crash would. Until a fault is asked for it is the store it
// wraps. It is safe for concurrent use.
//
// A call that fails returns no part of the store's answer, whether or not it
// reached the store. That holds for a call the store itself refused: a fault
// after the store hides the store's own error too, as a connection lost
// before the reply would.
type FaultStore struct{ /* unexported fields */ }

// NewFaultStore wraps inner.
func NewFaultStore(inner agent.Store) *FaultStore

// Kill makes every later call fail with ErrKilled without reaching the
// store.
func (f *FaultStore) Kill()

// KillBefore kills the store as its nth call arrives, so that call and
// every later one fail and none of them reaches the store. n counts from 1,
// over every call since the store was built; an n already passed kills the
// store at its next call.
func (f *FaultStore) KillBefore(n int)

// KillAfter lets the nth call reach the store, then fails it and every
// later one: the write landed and the caller never learned of it. If the
// store refused the nth call, the caller does not learn that either. n
// counts as it does for KillBefore.
func (f *FaultStore) KillAfter(n int)

// FailBefore makes the next times calls of op fail with ErrFault without
// reaching the store, and leaves the store working: other methods, and op
// itself once the count is spent, go through. op is the name of a Store
// method, as "CompleteModel"; any other name panics, so that a misspelt one
// is not a fault that silently never happens.
func (f *FaultStore) FailBefore(op string, times int)

// FailAfter lets the next times calls of op reach the store and then fails
// each with ErrFault, whatever the store answered: a write that landed is
// reported as failed, and so is a call the store refused. The store goes on
// working. op is as for FailBefore, and a fault asked for with FailBefore is
// spent first.
func (f *FaultStore) FailAfter(op string, times int)

// Calls reports how many calls have arrived, those that failed included.
// Every Store method counts, reads and heartbeats as much as writes, so a
// count taken from one run is the count of another only when both make the
// same calls: keep a real heartbeat timer out of a run that is counted.
func (f *FaultStore) Calls() int

// RunStoreSuite runs the Store contract against the store newStore builds.
func RunStoreSuite(t *testing.T, newStore func(t *testing.T) agent.Store)

var (
	_ agent.Clock = (*Clock)(nil)
	_ agent.Model = (*Model)(nil)
	_ agent.Guard = GuardFunc(nil)
	_ agent.Store = (*FaultStore)(nil)
)
```

Which fault to ask for: `Kill`, `KillBefore` and `KillAfter` are a crash,
after which that `FaultStore` never works again and another process carries
on through the store it wraps, while `FailBefore` and `FailAfter` are a
failure the process outlives, such as a store error the engine answers by
giving the run back as failed, or a heartbeat that fails while the journal
can still be written.

What the engine's methods do beyond their comments:

- `Register` refuses: an empty name; a name already registered; a tool
  name that does not match `^[a-zA-Z0-9_-]{1,64}$` or appears twice; a tool
  with both `Run` and `Delegate`, or neither; a `Schema` or `Output` that is
  not valid JSON. It does not check that a `Delegate` is registered, since
  agents may be registered in any order; an unknown one is an error result
  when the call is made.
- `Start` returns `ErrUnknownAgent` for an agent not registered here. It
  builds the `Snapshot` from the `Definition`: the prompt, the model, the
  output schema, the tools as `ToolSpec`s sorted by name, and the limits
  with defaults filled in (or `StartRequest.Limits`, likewise filled). It
  writes the run with `CreateRun` and publishes `EventRunStarted` when the
  run is new.
- `Execute`, `Tick` and `Work` claim only runs of agents registered in this
  process.
- `Approve`, `Decline` and `Cancel` refuse a `by` that is empty once trimmed,
  is not valid UTF-8 or holds a NUL, and record the trimmed name; they refuse
  a reason that is not valid UTF-8 or holds a NUL.

What each `Store` method does, for both implementations. Every time comes
from the request; a store never reads a clock.

| Method | Effect |
|---|---|
| Every method, in this order | First, what the request alone shows to be wrong is refused, before the store is touched: an id to be kept that is not a UUID in canonical form, a TTL of zero or less, a `To` that is no step status, a `Status` the method does not take, a cause that is none of the three, attributes JSON cannot hold, raw JSON that is not JSON, a claim with no owner, a name that cannot be kept, a list cursor whose id is not a UUID. A Postgres store makes these checks in Go before it opens a transaction. Second, a run or an approval that does not exist is `ErrNotFound`. Third, for a method that takes a `Lease`, a lease that is not the run's is `ErrLeaseLost`. Fourth, and only then, whatever depends on the state of the run, its steps and its approvals: its status, a step's `From`, a parent that does not exist or is not one level above, an approval already recorded. So a call that is wrong in two ways gets the same error from every store: a lost lease with a bad argument is refused for the argument, and a lost lease with a step in the wrong status is `ErrLeaseLost` |
| Every string | A database column holds neither a NUL character nor a byte that is not UTF-8, and a model, a tool or a person can put either in anything they write. **A string a store only records is kept, with each such character as the replacement character U+FFFD**: a run's input, output and error, a reason, a tool's result, a rule and a decision, the name of a tool the model called and of the model that answered, why it stopped, who decided or cancelled and why, and the strings inside `Metadata` and an `Action`, keys included. So no journal write fails, and no run is wedged, for what a model or a tool wrote. **A string a store compares is refused**, as an argument and with nothing changed, because it could only be kept as another name: an agent's name, a start key, a lease's owner, and an id, which has its own rule below. In a filter such a name is no run's, and lists or claims nothing. Inside what is kept as JSON, which is a message and a definition, a NUL is kept, since JSON has an escape for it, and only the byte that is not UTF-8 becomes the replacement character. **Raw JSON is kept as the bytes it came as, or refused**: a call's arguments, the provider's form of a turn, a tool's schema and the output schema must be JSON, in UTF-8. A call the model wrote badly is not lost by this: its arguments are held as one JSON string |
| Every method that changes anything | Adds one to the run's `Rev`, stamps each step and approval it touched with the new `Rev`, and sets `UpdatedAt` to the request's time. One call adds one, however many steps and approvals it touches: they all carry the one new `Rev`. `Heartbeat` is the exception: it changes only the lease's expiry and leaves `Rev` and `UpdatedAt` alone. A call that changes nothing leaves them alone too: a call that is refused, a `Park` that reports false, a `RequestApproval` that returns an approval already recorded, a `RequestCancel` of a run already marked, an `ExpireApprovals` with nothing due |
| Every method taking a `Lease` | Once the request itself has passed, locks the run, and returns `ErrLeaseLost` without changing anything unless the run's owner and epoch equal the lease's. The fence comes before every check that reads the store, and after the checks of the request alone. A lease with an empty `Owner` is never the run's, even when the run records no owner, so nothing writes to a run nobody holds. The hold is the epoch and not the time: under a lease that has lapsed and that no other claim has taken, a write goes through and `Heartbeat` extends the expiry. A run that does not exist is `ErrNotFound`, whatever the lease |
| Every method that makes a waiting run runnable | These are `DecideApproval`, `ExpireApprovals`, `RequestCancel`, and `Finish` for the run's parent. Each sets `Status` runnable and clears `Reason`, since the run no longer waits. A run that is not waiting keeps its status and its lease |
| Every method that is given something to keep | Refuses, with an error that is none of the package's sentinels and with nothing changed, what a database could not keep or would keep as something else, where that would change what the thing is or names. (A string that is only recorded is the exception, and is kept as the row above says.) An id to be stored must be a UUID in the one form `uuid.NewString` writes, lower case with hyphens: a run's `ID` and `ParentID`, an approval's `ID`, a step's `ChildRunID`. Another spelling of a UUID, upper case or without hyphens, is refused, because a database would take it and hand back the canonical one, and the id would no longer be the one given. A `ParentID` must name a run that exists, one level above. The other refusals are in the rows below |
| `CreateRun` | The run is stored as given, with `Rev` 1 whatever `Rev` it was given. `Status` must be `StatusRunnable`: any other is an error and stores nothing. `ID`, and `ParentID` when set, must be UUIDs in canonical form; `Agent`, `Key` and `LeaseOwner` must be strings that can be kept; and each schema in `Definition` must be JSON. These are judged from the request, so a create that would find its key is still refused for them. Then a create that finds its agent and key returns the run that has them, as it now stands, and stores nothing. Otherwise an id already in use is an error, and the run that has it is left alone; then a `ParentID` that names no run is an error; and then a `Depth` that is not the parent's plus one, or not zero for a run with no parent, is an error. The depth is what the order of row locks is taken from (6.10), so a store does not take the caller's word for it. `Metadata` reads back as a JSON round trip gives it, so a byte that is not UTF-8 comes back as the replacement character, and as an empty map, not nil, when the run was given none |
| `Claim` | 6.6. `Owner` must not be empty, must be a string that can be kept, and `TTL` must be more than zero: otherwise an error. Of runs created at the same instant the one whose id sorts first is taken first: a table keeps no order of arrival. A claim leaves `NextAttemptAt` as it is, for the next `Yield` to set or clear. With `RunID`, the same conditions apply to that run alone, its agent being named in `Agents` among them, and `ErrNotClaimable` is returned when they do not hold. A claim by `RunID` locks the run and waits for a write in progress before it decides; only a claim without `RunID` passes over a run that is locked. A `RunID` that names no run is `ErrNotFound` |
| `Heartbeat` | Sets the lease's expiry to `now` plus `ttl` and reports the cancel mark. `ttl` must be more than zero: otherwise an error, and the lease is as it was |
| `Yield` | Owner cleared, expiry cleared, `NextAttemptAt` set as given, which clears it when none is given. With `Failed`: `Failures` plus one, `Error` recorded. Without it, `Failures` and `Error` stay as they were. `Error` is therefore the last failure: it stays through later executions that go well, until `Finish` |
| `Park` | 6.10. Sets `Status` waiting and `Reason`, clears the lease |
| `Finish` | `Status` must be completed, failed or cancelled: any other is an error and changes nothing, and is judged before the run is looked for and before the lease. Sets `Status`, `Reason`, `Output`, `Error`, `FinishedAt`; clears the lease; sets every pending approval of the run `cancelled`; and when the run has a parent that is waiting, sets it runnable and adds one to its `Rev`. `Error` is set as given, so a run that ended well has none, whatever an earlier failure left. A cancelled approval gets that status, `DecidedAt` the request's time and the run's new `Rev`, and no `DecidedBy` or `Reason`, since nobody decided it |
| `BeginModel` | `seq` must be one past the last step, or name a model step that is `started`; otherwise `ErrConflict`. Inserts the step as `started` with `Attempts` 1, or adds one to `Attempts` and resets `StartedAt` |
| `CompleteModel` | Each call's `Input` and the message's `Opaque.Data` must be JSON, in UTF-8, or an error, judged before the lease. The step must be a `started` model step; otherwise `ErrConflict`, which is also the answer when `seq` names no step. Sets it `completed` with the message, stop, model name, usage and `FinishedAt`. On the run: adds the usage, adds one to `ModelCalls`, adds finish less start to `ActiveMillis`, sets `Failures` to 0. Appends one tool step per `Message.Calls` entry at `Seq+1`, `Seq+2`, …: `proposed`, `Turn` the model step's `seq`, `Name` and `Call` from the call, `Key` from `StepKey` |
| `UpdateStep` | `To` must be one of the six step statuses, and `ChildRunID`, when set, a UUID: otherwise an error, and nothing changes. The step must be a tool step in `From`; otherwise `ErrConflict`, which is also the answer when `seq` names no step. Sets `Status` to `To` and records what the request carries: `Decision` and `Rule` only when `Decision` is not empty, `Result` and `IsError` only when `Result` is not nil, and `ChildRunID` only when it is not empty. A `Result` that points at an empty string is a result and is recorded. `To` of `started` adds one to `Attempts` and sets `StartedAt`. A final `To` sets `FinishedAt`, sets the run's `Failures` to 0, and when `From` is `started` adds finish less start to `ActiveMillis`. `Usage` is added to the step and the run. The store checks nothing else about the move; which moves are legal is the engine's business |
| `RequestApproval` | `ID` must be a UUID and `Cause` one of the three causes, or an error. The step must be a tool step: a model step, or a `seq` that names no step, is `ErrConflict`. If an approval exists for the run, `Seq` and the step's current `Attempts`, it is returned and nothing changes. That lookup comes before the check of `From`, and looks at neither the approval's status nor the rest of the request, so a question asked again after its answer comes back answered. Otherwise the step must be in `From`, or `ErrConflict`; it becomes `waiting`, and an approval is inserted as pending with `Attempt` the step's `Attempts`, `Tool` its name and `Input` its call's arguments. The step's `Decision` and `Rule` are set only when the request's `Decision` is not empty, as in `UpdateStep`: the question about an interrupted call does not replace the guard's answer on the step. The approval's `Rule` is the request's either way. `Action.Attrs` reads back as a JSON round trip gives it: a number as a `float64`, an object as a `map[string]any`, a list as a `[]any`, a byte that is not UTF-8 as the replacement character, and an empty map, not nil, when there were none. An integer above 2^53 is therefore no longer exact when it is read back, as it would not be from a database; one that must be exact goes in a string. Attributes JSON cannot hold, a channel or a function, are an error, judged before the lease |
| `DecideApproval` | Locks the run. A pending approval becomes approved or declined with who, why and when; a waiting run becomes runnable. Anything but pending, an expired or a cancelled approval included, is returned as it stands with `ErrAlreadyDecided` |
| `ExpireApprovals` | The same as a decline, for every pending approval whose `ExpiresAt` is not after `now`, with status `expired`: `DecidedAt` is `now`, and `DecidedBy` and `Reason` stay empty, since nobody decided. Approvals of one run that lapse in one call change the run once and carry the one new `Rev`. With `DecideApproval` and `Finish` this keeps one rule: an approval's `DecidedAt` is nil exactly while it is pending |
| `RequestCancel` | Locks the run. Sets the mark, who and why; a waiting run becomes runnable. A run already marked is left exactly as it is: the first request's who and why stand and `Rev` does not move. `ErrFinished` for a run that has ended |
| `Changes` | The run as it stands, always. With it, the steps in `Seq` order and the approvals in the order `ListApprovals` gives, whatever order they changed in. A reader that passes each answer's `Run.Rev` as the next call's `since` misses no change, whatever is being written meanwhile. A step or approval may be newer than the run it is returned with; the next call then returns it again |
| `ListRuns` | Newest first by `CreatedAt`, then `ID` descending. A `Limit` of zero or less is 50, and one over 200 is 200. A `ParentID` that names no run, or is not a UUID, lists nothing. `Before` is an argument, and may have come from a client: a cursor whose `ID` is not a UUID in canonical form is refused with an error, before the filters are looked at and with nothing asked of a database, one with a time and no id included. The zero `Cursor` is the one exception, and lists from the start. A cursor whose id no run has is a position like any other: the runs listed are those whose `(CreatedAt, ID)` sorts below it |
| `ListApprovals` | Oldest first by `RequestedAt`, then `ID`. `Limit` as for `ListRuns`. A `RunID` that names no run, or is not a UUID, lists nothing |
| Any method given an id that does not exist | `ErrNotFound`. An id that is not a UUID does not exist either: it is `ErrNotFound` like any other, and never the database's complaint about its form. Nor does another spelling of an id that does exist: a store knows a run or an approval by the one string it was given, so its id in upper case or without hyphens is `ErrNotFound`, though a database would find the row. A filter is not a lookup, and lists nothing, for an id nothing has, one that is not a UUID, and another spelling of one that exists. A `seq` is not an id: a step that does not exist is `ErrConflict`, as the rows above say |

`agenttest.RunStoreSuite` has a case for each of these, and both stores
pass it.

### 6.3 Tables

`agent/pg/migrations/001_agent_journal.up.sql`. Every timestamp is written
by the engine from its `Clock`; the one database default is on a column no
logic reads.

```sql
-- Every migration is re-applied against any database whose ledger has no row
-- for it, so each one has to be safe to run twice.

-- One row per run. A run is runnable, waiting, or ended; a runnable run whose
-- lease has not lapsed is being executed by lease_owner.
CREATE TABLE IF NOT EXISTS agent_runs (
    id               UUID PRIMARY KEY,
    agent            TEXT NOT NULL,
    status           TEXT NOT NULL
                     CHECK (status IN ('runnable', 'waiting', 'completed', 'failed', 'cancelled')),
    reason           TEXT NOT NULL DEFAULT '',
    input            TEXT NOT NULL,
    output           TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',

    parent_id        UUID REFERENCES agent_runs (id) ON DELETE CASCADE,
    parent_seq       INTEGER NOT NULL DEFAULT 0,
    depth            INTEGER NOT NULL DEFAULT 0,

    start_key        TEXT,
    -- JSON, not JSONB: the snapshot is sent to the model on every call, and
    -- JSON gives back the text that was stored.
    definition       JSON NOT NULL,
    metadata         JSONB NOT NULL DEFAULT '{}'::jsonb,

    input_tokens     BIGINT NOT NULL DEFAULT 0,
    output_tokens    BIGINT NOT NULL DEFAULT 0,
    cost_micros      BIGINT NOT NULL DEFAULT 0,
    model_calls      INTEGER NOT NULL DEFAULT 0,
    active_ms        BIGINT NOT NULL DEFAULT 0,

    rev              BIGINT NOT NULL DEFAULT 0,

    lease_owner      TEXT NOT NULL DEFAULT '',
    lease_epoch      BIGINT NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ,
    failures         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ,

    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
    cancel_by        TEXT NOT NULL DEFAULT '',
    cancel_reason    TEXT NOT NULL DEFAULT '',

    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    finished_at      TIMESTAMPTZ
);

-- Start is idempotent per agent on the caller's key.
CREATE UNIQUE INDEX IF NOT EXISTS agent_runs_start_key_idx
    ON agent_runs (agent, start_key) WHERE start_key IS NOT NULL;

-- Claim reads the oldest runnable run; the lease and backoff columns are
-- filtered on top of this.
CREATE INDEX IF NOT EXISTS agent_runs_runnable_idx
    ON agent_runs (created_at) WHERE status = 'runnable';

-- Park and Cancel look up a run's children.
CREATE INDEX IF NOT EXISTS agent_runs_parent_idx
    ON agent_runs (parent_id) WHERE parent_id IS NOT NULL;

-- The run listing is newest first, with id as the tie-break.
CREATE INDEX IF NOT EXISTS agent_runs_created_idx
    ON agent_runs (created_at DESC, id DESC);

-- The journal: one row per model call and per tool call, in order.
CREATE TABLE IF NOT EXISTS agent_steps (
    run_id        UUID NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    seq           INTEGER NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('model', 'tool')),
    status        TEXT NOT NULL
                  CHECK (status IN ('proposed', 'waiting', 'started', 'completed', 'blocked', 'declined')),
    name          TEXT NOT NULL DEFAULT '',

    -- A model step's reply. JSON, not JSONB: the provider's own form of the
    -- turn is inside it and goes back to the provider as it was stored.
    message       JSON,
    stop          TEXT NOT NULL DEFAULT '',

    -- A tool step: the model step that proposed it, the call as written, the
    -- guard's answer, and what was returned to the model.
    turn          INTEGER NOT NULL DEFAULT 0,
    call          JSON,
    idem_key      TEXT NOT NULL DEFAULT '',
    decision      TEXT NOT NULL DEFAULT '',
    rule          TEXT NOT NULL DEFAULT '',
    result        TEXT NOT NULL DEFAULT '',
    is_error      BOOLEAN NOT NULL DEFAULT FALSE,
    child_run_id  UUID,

    attempts      INTEGER NOT NULL DEFAULT 0,
    input_tokens  BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cost_micros   BIGINT NOT NULL DEFAULT 0,
    rev           BIGINT NOT NULL,

    created_at    TIMESTAMPTZ NOT NULL,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,

    PRIMARY KEY (run_id, seq)
);

-- Changes reads the steps touched since a revision.
CREATE INDEX IF NOT EXISTS agent_steps_rev_idx ON agent_steps (run_id, rev);

-- A question put to a person about one tool step.
CREATE TABLE IF NOT EXISTS agent_approvals (
    id           UUID PRIMARY KEY,
    run_id       UUID NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    attempt      INTEGER NOT NULL,
    cause        TEXT NOT NULL CHECK (cause IN ('guard', 'tool', 'interrupted')),
    tool         TEXT NOT NULL,
    input        JSON NOT NULL,
    action       JSONB NOT NULL,
    rule         TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL
                 CHECK (status IN ('pending', 'approved', 'declined', 'expired', 'cancelled')),
    decided_by   TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    rev          BIGINT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL,
    decided_at   TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,

    -- One question per attempt of a step, so asking again is a no-op.
    UNIQUE (run_id, seq, attempt)
);

-- The operator's queue is the pending approvals, oldest first.
CREATE INDEX IF NOT EXISTS agent_approvals_pending_idx
    ON agent_approvals (requested_at) WHERE status = 'pending';

-- Idempotency keys of tool effects that have been applied. See Once.
CREATE TABLE IF NOT EXISTS agent_tool_effects (
    key        TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

`001_agent_journal.down.sql` drops the four tables, `agent_runs` last, each
with `CASCADE`.

This DDL and the claim statement in 6.6 were applied to Postgres 16 through
`migrate.Replay` while writing this document: applied, applied again, taken
down and up.

How `agent.Step` maps: `message` holds `Step.Message` as JSON, `call` holds
`Step.Call`, `idem_key` is `Step.Key`, the three usage columns are
`Step.Usage`. `agent_runs.start_key` is `Run.Key`, null when empty.

How `agent/pg` writes and reads the columns, beyond what their types say:

- **Ids.** A `uuid` column reads a UUID in any spelling and hands back the
  canonical one, so the store checks an id's form in Go before it sends
  one (6.2) and reads every id back as text.
- **Times.** Every time is bound from the request and none is `now()`. A
  time is kept to the microsecond below it and read back in UTC. The time a
  step spent working is measured in Go, between its start as the column
  kept it and its finish.
- **The `json` columns** (`definition`, `message`, `call`, `input`) keep the
  text they are given. `encoding/json` rewrites a `json.RawMessage` as it
  marshals one, dropping the space between tokens and escaping `<`, `>` and
  `&`, so the store writes the object around each raw value itself and puts
  the raw value in as the bytes it came as. A call's arguments, a turn in
  its provider's form, a tool's schema and the output schema therefore read
  back byte for byte, but for white space around the whole value. A raw
  value that is not JSON, or not UTF-8, is refused in Go before the
  transaction is opened. One that is nil is written as `null` and reads
  back as `null`.
- **The `jsonb` columns** (`metadata`, `action`) keep a value and not its
  text. They read back equal in value, as the table in 6.2 says, and none
  is stored as `{}`.
- **Order.** Rows are read as `id::text`, and Postgres gives that column
  the name `id`. A bare `id` in an `order by` then means the text: sorted in
  the database's collation, and with a sort where the index would have
  served. The statements that order by an id name it with its table.
- **Counts and positions.** A `seq`, a depth or a count that does not fit a
  four-byte column is never bound: a `seq` that does not fit names no step,
  and a run with such a depth is refused.

What each column does with a string it cannot hold, which is one with a NUL
character in it or a byte that is not UTF-8. The rule is the one in 6.2's
table, "Every string"; this is where it lands:

| Columns | Type | What the store does |
|---|---|---|
| `agent_runs.definition`, `agent_steps.message`, `agent_steps.call`, `agent_approvals.input` | `json` | A NUL is kept, as the escape `\u0000`, and reads back as it was written. A byte that is not UTF-8 in a string is written as U+FFFD by `encoding/json`; in a raw value it is refused |
| `agent_runs.metadata`, `agent_approvals.action` | `jsonb` | Postgres would refuse a NUL. The store writes U+FFFD for it, in keys and in values at any depth; `encoding/json` has already done the same for a byte that is not UTF-8 |
| The `TEXT` columns a store only records: `agent_runs.reason`, `input`, `output`, `error`, `cancel_by`, `cancel_reason`; `agent_steps.name`, `stop`, `decision`, `rule`, `result`; `agent_approvals.tool`, `rule`, `decided_by`, `reason` | `text` | Postgres would refuse either character. The store writes U+FFFD for each |
| The `TEXT` columns a store compares: `agent_runs.agent`, `start_key`, `lease_owner`; `agent_tool_effects.key` | `text` | Refused in Go, before a transaction is opened |

So no statement is ever sent that Postgres would refuse for a string, and a
tool result or a model's last words cannot keep a step from being journaled.
`agent/pg/nul_test.go` holds the store to this column by column, and the
suite holds both stores to what reads back.

### 6.4 The journal and the conversation

A run's journal is its steps in `seq` order, from 1 with no gaps. There are
two kinds.

A **model step** is one call to the model. It is `started` when the call is
about to be made and `completed` when the reply is stored.

A **tool step** is one tool call the model asked for. All the tool steps of
a reply are appended in the same transaction that stores the reply, in the
order the model wrote them, as `proposed`. Each then moves on its own:

```
proposed ──► blocked                           the guard said no
proposed ──► waiting ──► declined              a person said no, or the approval lapsed
proposed ──► waiting ──► started ──► completed a person said yes
proposed ──► started ──► completed             allowed
proposed ──► started ──► waiting ──► completed a delegating tool: waiting on the child run
started  ──► waiting ──► started | declined    an at-most-once call was interrupted; a person decides
proposed ──► completed                         malformed arguments, or a tool this build does not have
```

`blocked`, `declined` and `completed` are final and carry the `Result`
returned to the model. A tool step's idempotency key is
`StepKey(runID, seq)`, fixed when the step is appended.

The conversation sent to the model is a pure function of the run and its
journal:

1. One user message holding `Run.Input`.
2. For each completed model step, in order: its `Message`, as stored. Then,
   if that reply made calls and every one of its tool steps is final, one
   `RoleTool` message with their `Result`s in order.

The system prompt and the tool list come from `Run.Definition`, the
snapshot taken when the run started, with tools sorted by name. So two
requests for the same run always agree on everything before the point the
earlier one ended: the conversation is only appended to, never edited. That
is the property section 3 needs, and one of the tests in 6.15 asserts it
directly.

The text of a result that no tool produced is fixed, so it is the same on
every rebuild:

| Outcome | `Result` | `IsError` |
|---|---|---|
| Blocked | `blocked by policy: <rule>` | true |
| Declined | `declined by <who>: <reason>` | true |
| Approval lapsed | `declined: approval expired` | true |
| Interrupted and not run again | `interrupted before its result was recorded; not run again` | true |
| Malformed arguments | `arguments were not valid JSON` | true |
| Tool missing from this build | `tool is not available` | true |
| Tool returned an error | The error's text, made fit for the journal: bytes that are not UTF-8 replaced by U+FFFD, then NUL bytes removed | true |
| Tool panicked | `tool panicked` (the value and the stack go to the log) | true |
| Tool ran past its timeout | `timed out after <duration>` | true |
| Tool given no time, a timeout of zero or less, and so not run | `tool was given no time to run` | true |
| Result over 1 MiB | `result too large: <n> bytes` | true |
| Result not valid UTF-8 | `result is not valid UTF-8` | true |
| Result with a NUL byte | `result contains a NUL byte` | true |
| Child run failed or was cancelled | `<agent> <status>: <reason>` | true |

A result is stored as text, and Postgres refuses, in a text column, a NUL
byte and bytes that are not UTF-8. A write refused for its content would be
refused on every attempt, and the run could never move past the step. So
`invoke` hands the journal only text it can hold. A tool's result with such
bytes is refused with one of the two fixed texts above, as one that is too
large is, and the model is told: a result with bytes changed would no longer
be the tool's answer. An error's text is repaired instead, since it only has
to say what went wrong. The 1 MiB bound is on bytes, and for an error's text
is applied after the repair. A result that fails more than one check is
reported by the first of: too large, not valid UTF-8, a NUL byte.

### 6.5 Execution

An execution is what one process does with a run between claiming it and
letting it go. It is a loop with no state of its own:

```
claim the run                                      (Store.Claim)
if its Failures have reached MaxFailures: finish it as abandoned, and stop
start a heartbeat
loop:
    read the journal, the run's approvals, and its children
    decide the next action from them               (next, a pure function)
    perform it, which writes to the journal
until the run is finished, parked or yielded
stop the heartbeat
```

Because each turn of the loop starts from the journal, resuming after a
crash is not a special path. A new execution reads what is there and decides
the same way.

`next` is the unexported heart of the engine, in `plan.go`:

```go
type actionKind int

const (
	actFinish  actionKind = iota // end the run
	actModel                     // call the model for step seq
	actJudge                     // settle a proposed tool step: refuse it, ask about it, or start it
	actRun                       // execute a tool step
	actSpawn                     // start the child run of a delegating tool step
	actCollect                   // record a finished child's outcome on its tool step
	actResolve                   // record a person's no on a waiting tool step
	actAsk                       // ask a person about an interrupted at-most-once call
	actPark                      // nothing can proceed until a person or a child acts
	actYield                     // give the run up for now: the journal holds what this build cannot read
)

type action struct {
	kind   actionKind
	seq    int    // the step acted on; for actModel, the step to begin
	status Status // actFinish
	reason string // actFinish, actPark
	output string // actFinish
	errmsg string // actYield: what was found
}

// conversation rebuilds what the model is sent. See 6.4.
func conversation(run Run, steps []Step) []Message

// next decides what an execution does now. def is the Definition registered
// under run.Agent in this process. children maps a child run's id to it.
func next(run Run, def Definition, steps []Step, approvals []Approval, children map[string]Run) action
```

`next` applies these six rules in order and returns at the first that gives
an action. They are numbered here as the comment on `next` in `plan.go`
numbers them.

1. **Cancellation.** `run.CancelRequested`: finish, `StatusCancelled`.
2. **A step this build cannot read**, anywhere in the journal: `actYield`,
   naming the first. That is a step whose kind is neither model nor tool;
   a model step that is neither `started` nor `completed`; a tool step
   whose status is none of the six; a completed model step with no
   message.
3. **The last reply's `Stop`**, when the last model step is completed.
   - `StopRefusal`, `StopMaxTokens` or `StopContextWindow`: finish,
     `StatusFailed`, with `ReasonRefusal`, `ReasonTruncated`,
     `ReasonContextWindow`. Such a reply is final whatever came with it,
     so its calls are never judged or run: a refusal is not a turn, and
     the last call of a reply cut at its token limit can be incomplete and
     still valid JSON.
   - `StopEnd`, `StopPause` or `StopToolUse`: no action here. The reply is
     a turn if it made calls, and rule 6 reads it if it made none.
   - Any other, the empty one included: `actYield`, whether or not the
     reply made calls.

   Only the last model step's stop is read. A model step that is still
   `started` has none, and gives no action here.
4. **The walk.** Go through the tool steps that are not final, in `seq`
   order. For each:
   - `proposed`: `actJudge`.
   - `started`, the tool delegates: `actSpawn`.
   - `started`, the tool is neither delegating nor `AtMostOnce`: `actRun`.
   - `started`, the tool is `AtMostOnce` and does not delegate: `actAsk`.
     If an approval for the step's current `Attempts` already exists,
     `actYield` instead: asking again would be handed that approval and
     change nothing, and the same action would come back for ever. No
     store leaves a step so.

     A `started` step seen here was interrupted: an execution that starts
     a step always finishes it, or ends, before `next` is asked again.
   - `waiting` with a `ChildRunID`: `actCollect` if the child has ended;
     otherwise the step is pending. A child that is not in `children` is
     `actYield`.
   - `waiting` without one: look at the approval for the step's current
     `Attempts`, which is the one that was asked when the step began to
     wait. Approved: `actRun`. Declined, expired or cancelled:
     `actResolve`. Pending: the step is pending. None, or one in a status
     this build does not know: `actYield`. An approval for an earlier
     attempt decides nothing: read from a list that was cut short, it
     would run an interrupted call that nobody has approved.

   A pending step is passed over and the walk goes on to the next.

   **When a budget is spent** (time, cost or tokens: 6.9), `actJudge`,
   `actRun`, `actSpawn` and `actAsk` are not given: the step is held back
   and the walk goes on. `actCollect`, `actResolve` and `actYield` are
   still given where they are met, so an ended child's usage reaches the
   run's totals and a person's no is recorded before the run fails, and a
   person is not asked about a call that will not run. The limit on model
   calls holds nothing back here: it stops a model call and nothing else.
5. **The walk gave no action.**
   - It held a step back for a budget: finish, `StatusFailed`, with the
     budget's reason.
   - Else it passed over a step that waits on a person: `actPark`,
     `ReasonApproval`.
   - Else it passed over a step that waits on a child: `actPark`,
     `ReasonChildren`.

   So a run with a spent budget and nothing but pending steps still parks,
   and fails when it is woken with work to do.
6. **No tool step is open.** The end of the journal decides.
   - No step, or the last step is a tool step (the last reply made calls
     and all are final): `actModel` at `len(steps)+1`.
   - The last step is a `started` model step: `actModel` at its `seq`. The
     call was interrupted.
   - The last step is a completed reply that made no calls: by its `Stop`.
     `StopEnd`: finish, `StatusCompleted`, output its text. `StopPause`:
     `actModel` at `len(steps)+1`. `StopToolUse`: `actYield`, since a reply
     that says it used tools and made no calls is not one the rules can
     place.

   In place of any `actModel` here, when a budget is spent: finish,
   `StatusFailed`, with its reason. `Run.ModelCalls` having reached its
   limit, `ReasonModelCalls`, is one of the four at this point and at no
   other.

When more than one budget is spent the reason is the first of time, cost,
tokens and model calls. A run whose work is done is never failed for its
budget: a finish, a park and an `actYield` are given whatever has been
spent.

Calls of one reply are executed one at a time in the order the model wrote
them. A step waiting on a person does not hold up the steps after it:
`next` passes over a pending step and acts on the next one, and the run
parks only when everything left is pending.

`actYield` carries what was found in `errmsg`, and is given at the point in
the rules where it is met, as they say above. The run is given up as a
failed attempt and not ended, because ending a run cannot be undone and
neither cause is the run's own: a value this build does not know is what an
older worker reads during a deploy once a newer build has written to the
journal, and a child or an approval that was not passed in is the
executor's mistake. Given up, the run waits out its back-off and is claimed
again, by a worker that can read it or after a fix, and if none can, the
limit on failed executions ends it with the same message; parking it
instead would leave nothing to wake it. A budget does not replace
`actYield`, and cancellation still comes first.

How each action is performed:

| Action | Store calls | Notes |
|---|---|---|
| `actModel` | `BeginModel`, then `Model.Generate`, then `CompleteModel` | The request is the snapshot plus `conversation`. An error wrapping `ErrPermanent` finishes the run failed; any other error ends the execution as failed (6.6) |
| `actJudge` | `UpdateStep` or `RequestApproval` | A malformed call, or a tool not registered in this process, goes `proposed → completed` with the fixed error result. Otherwise the tool's `Action` is built, `AttrAgent`, `AttrTool`, `AttrRun` and `AttrSeq` are set where absent, and the `Guard` is asked. `Block`, or an effect that is none of the three: `proposed → blocked`. `Ask`, or `Allow` on a tool with `Approval`: `RequestApproval`, cause `guard` or `tool`. `Allow`: `proposed → started`, recording the decision and rule, and then, in the same action, the tool is run as `actRun` runs it, or the child started as `actSpawn` starts it. A `Guard` error ends the execution as failed; it is never read as allow or as block |
| `actRun` | `UpdateStep` twice | First the step is started: `waiting → started` for an approved step, or `started → started` for an interrupted one, which counts the new attempt. Then the tool runs with a context bounded by its `Timeout` and the time left in the budget, inside a `recover`. Then `started → completed` with the result. An error wrapping `ErrTransient` skips the last write and ends the execution as failed, so the call is made again with the same `Key` |
| `actSpawn` | `CreateRun`, `UpdateStep` | The child's `Key` is the step's idempotency key, so asked twice it is created once. Its input is the call's arguments, its depth one more than the parent's, and its cost limit as 6.9 says. Then `started → waiting` with `ChildRunID`. A depth past `MaxDepth`, or an agent not registered, completes the step with an error instead |
| `actCollect` | `UpdateStep` | `waiting → completed` with the child's output, or the fixed error result, and the child's `Usage`, which so counts against the parent's budget |
| `actResolve` | `UpdateStep` | `waiting → declined` with the fixed result |
| `actAsk` | `RequestApproval` | `From: StepStarted`, cause `interrupted`, rule `RuleInterrupted` |
| `actPark` | `Park` | If `Park` reports false, something changed since the journal was read; the loop goes round again |
| `actFinish` | `Finish` | For a run that fails or is cancelled, each child that has not ended is sent `RequestCancel` first |
| `actYield` | `Yield` | The execution ends as one whose step failed (6.6), with `errmsg` as the error: `Yield{Failed: true}` with the back-off, or, when this is failure number `MaxFailures`, `Finish` as failed, `ReasonError`, with the same message. Nothing is written to the journal |

A delegating tool is described to the `Guard` as
`Action{Kind: "delegate", Target: <agent name>}` unless it has its own
`Action`.

Two more unexported pieces are fixed here, because the plan builds them
apart from the loop that uses them. Both are free functions over the
contract types, so each compiles and is tested alone.

```go
// lease.go

// errCancelRequested is the cause of a held context that ended because
// cancellation of the run was requested.
var errCancelRequested = errors.New("agent: cancellation requested")

type keepOptions struct {
	Store Store
	Clock Clock
	TTL   time.Duration
	// Interval must be more than zero and less than TTL. Any other value is
	// replaced by a third of TTL and reported once in the log as an error.
	Interval time.Duration
	Logger   *slog.Logger
}

// keep extends lease every Interval until stop is called. The context it
// returns is ctx, cancelled with cause ErrLeaseLost when a heartbeat finds
// the lease gone or no heartbeat has succeeded for a whole TTL, and with
// cause errCancelRequested when a heartbeat reports the request.
//
// After a cancel request the heartbeats go on until stop, so the caller has
// as long as it needs to finish the run as cancelled; a lease lost after
// that ends the heartbeats and leaves the cause as it was.
//
// A lease another process took is noticed at most one Interval and one
// round trip to the store after the takeover. With the store failing,
// another process may claim the run from the last heartbeat that succeeded
// plus the TTL, and the held context ends less than two Intervals after
// that. Until a first heartbeat has succeeded the TTL is counted from the
// call to keep.
//
// stop returns once the goroutine that makes the heartbeats has exited, and
// releases the held context: one still live ends with context.Canceled as
// its cause. Whatever is written after stop is written under ctx.
func keep(ctx context.Context, lease Lease, opts keepOptions) (held context.Context, stop func())

// invoke.go

// outcome is what one execution of a tool produced. retry is set, and the
// rest empty, when the tool's error wraps ErrTransient. It is then an error
// that unwraps to the tool's own and carries its text, read in the tool's
// goroutine and made fit for the journal, so the caller can record it
// without calling into the tool's code.
//
// retry is also set, and the rest empty, when the context invoke was given
// ended before a result was taken: it is then that context's cause. Either
// way there is nothing to record, and the caller reads its own context to
// tell the two apart.
type outcome struct {
	result  string
	isError bool
	retry   error
}

// invoke runs tool.Run once: under timeout, with a panic recovered and
// logged to logger, an error turned into an error result, and a result the
// journal cannot hold refused: one over 1 MiB, one that is not valid UTF-8,
// one with a NUL byte. The fixed result texts are those in 6.4.
//
// timeout is the whole bound: invoke does not read Tool.Timeout. With a
// timeout of zero or less the tool is not run, and the result says so. A
// nil logger is slog.Default.
func invoke(ctx context.Context, tool Tool, in Invocation, timeout time.Duration, logger *slog.Logger) outcome

// actionFor is the Action the Guard is asked about: the tool's own, or the
// default for its kind, with AttrAgent, AttrTool, AttrRun and AttrSeq set
// where absent.
func actionFor(run Run, tool Tool, in Invocation) Action
```

### 6.6 The lease

Any process with the run's agent registered may execute it. One does at a
time.

**Claim.** One statement takes the oldest runnable run that is free:

```sql
with next as (
    select id as next_id from agent_runs
    where status = 'runnable'
      and agent = any($4)
      and (lease_expires_at is null or lease_expires_at <= $1)
      and (next_attempt_at is null or next_attempt_at <= $1)
    order by created_at, id
    for update skip locked
    limit 1
)
update agent_runs
set failures         = failures + case when lease_owner <> '' then 1 else 0 end,
    lease_owner      = $2,
    lease_epoch      = lease_epoch + 1,
    lease_expires_at = $3,
    rev              = rev + 1,
    updated_at       = $1
from next
where id = next_id
returning <the run's columns>
```

`$1` is the claim's time and `$3` that time plus the TTL, added in Go like
every other time the store writes. `skip locked` means two processes
claiming at once take different runs and neither waits. Runs created at one
instant are taken in the order of their ids, by both stores: the table has
no column for the order runs arrived in, so that is the one order both can
keep.

A claim that names its run (`ClaimRequest.RunID`, which `Execute` uses) is
two statements in one transaction. The first locks the row, and so waits for
a write in progress, and reads whether the run can be taken as it then
stands:

```sql
select status = 'runnable'
   and agent = any($3)
   and (lease_expires_at is null or lease_expires_at <= $2)
   and (next_attempt_at is null or next_attempt_at <= $2)
from agent_runs where id = $1 for update
```

No row is `ErrNotFound` and false is `ErrNotClaimable`. Otherwise the second
statement makes the update above for that id. It is not the first statement
with the id among its conditions and `skip locked` taken out, as this
section first had it. Passing over the row would answer `ErrNotClaimable`
for a lapsed run whose last holder happened to be in the middle of a write.
And a statement that carries the conditions looks for the row in a snapshot
taken before it waits for anything, so it would answer for the run as it
was: a run still waiting, say, while the answer that makes it runnable is
being committed. Locking first and deciding afterwards is what "waits for a
write in progress and then decides" takes.

A lease is free when it was released (`lease_owner` empty) or has lapsed.
Taking over a lapsed one adds a failure, so a run that kills every process
that touches it is finished as `ReasonAbandoned` by the execution that
claims it once `MaxFailures` is reached, before that execution does any of
the run's work.

**Heartbeat.** While an execution holds a run, a goroutine extends
`lease_expires_at` every `HeartbeatInterval`, matching on owner and epoch.
It also reads `cancel_requested`. If the heartbeat finds the epoch has
moved, or cannot reach the store for a whole `LeaseTTL`, it cancels the
execution's context: the step in flight is abandoned and nothing more is
written.

**Fencing.** `lease_epoch` rises by one on every claim. Every journal write
is one transaction that begins by locking the run's row and comparing its
owner and epoch with the caller's `Lease`. What the request alone shows to
be wrong is refused before the transaction is opened; inside it the fence
comes before every check that reads the run's state, as the table in 6.2
orders them:

```sql
select lease_owner, lease_epoch, cancel_requested, coalesce(parent_id::text, '')
from agent_runs where id = $1 for update
```

The last two columns are what `Park`, `Heartbeat` and `Finish` need of the
run, read under the same lock. If owner and epoch differ from the lease's,
the transaction ends with `ErrLeaseLost` and changes nothing. It ends the same way when the lease names no owner, whatever the
row holds: a run nobody holds records an empty owner, and a lease with none
must not match it. A claim takes the same row lock, so it never lands in the
middle of a write: a claim by id waits for the write, and a claim without
one passes over the run until the write is done. Either way the next holder
always reads a journal that includes it.

Every transaction that writes asks for read committed by name, whatever the
database's default. Each locks a row and then reads, in a later statement,
what the lock protects, and only at that level does the later statement see
what was committed while the lock was waited for. Under repeatable read a
`Park` that had waited for its row would look for a pending approval or an
unfinished child in the snapshot it took before it waited. `Changes` is the
one method that reads under repeatable read, so that the run and the steps
and approvals it returns are one moment's: read one statement at a time,
steps before the run, a write landing in between would be lost to the
reader, who would be handed a `Rev` newer than the steps it got.

A process that was paused past its
lease, or partitioned from the database and back, therefore cannot add to a
journal another process now owns. What it can still do is finish the tool
call it was in the middle of. That is the one way a tool can run twice at
the same moment, and 6.7 covers it.

**Clock.** Lease times are written and compared on the engine's `Clock`,
not the database's. Processes sharing runs must keep their clocks within a
small fraction of `LeaseTTL` of each other. A clock that runs fast only
costs work: its process may take a lease early, and fencing then stops the
first holder from writing. No journal is corrupted by a wrong clock.

**Letting go.** An execution ends in one of four ways:

| Ends by | Store call | Leaves the run |
|---|---|---|
| The run ended | `Finish` | Completed, failed or cancelled |
| Nothing can proceed | `Park` | Waiting, with no lease |
| Its context ended and the step in flight finished | `Yield` | Runnable, lease free |
| A step failed | `Yield{Failed: true}` | Runnable, with one more failure and `NextAttemptAt` set to now plus `RetryBase` doubled per failure, capped at `RetryMax`. When this would be failure number `MaxFailures`, the run is finished as failed instead, `ReasonError`, with the step's error |

Any step that completes resets `failures` to zero.

**Shutdown.** `Work` stops claiming when its context is cancelled. A step
in flight is not cancelled with it: the step's context is detached and
given `DrainTimeout` to finish, so a model call that has already been paid
for is recorded rather than thrown away. Then the run is yielded. After
`DrainTimeout` the step is cancelled, nothing is written, and the lease is
left to lapse.

### 6.7 What a crash does at each point

A crash is anything that stops a process from writing: a kill, a panic
outside a tool, a lost network. The table reads the same whether the
process died or merely lost its lease.

| The process stops | The journal holds | The next execution | Done twice |
|---|---|---|---|
| Before `BeginModel` commits | No step | Makes the call | Nothing |
| After `BeginModel`, before `CompleteModel` commits, whether or not the reply arrived | A `started` model step | Makes the call again, as attempt 2 | The model call. The first call's cost is on no record |
| After `CompleteModel` commits | The reply and its proposed tool steps | Judges the first proposed step. When the reply is a refusal, was cut at its token limit or ran out of context window, finishes the run as failed instead and its steps stay `proposed`; when its `Stop` is one this build does not know, gives the run up (6.5, rule 3) | Nothing |
| While judging, before the step leaves `proposed` | A `proposed` step | Asks the guard again | The guard is asked twice, and a guard that records writes two records. No effect on the world |
| After a step is `blocked` or `declined` | The final result | Passes over it | Nothing |
| After `RequestApproval` commits | A `waiting` step and a pending approval | Parks | Nothing. The same approval is found, not a second one |
| After `proposed → started`, before `started → completed` commits, whether or not the tool ran | A `started` tool step | Runs the tool again with the same `Key` and `Attempt` one higher; or asks a person, when the tool is `AtMostOnce` | The tool function. Its effect, unless it honours the key |
| After `started → completed` commits | The result | Passes over it | Nothing. A completed call is never made again |
| Between starting a child and recording it | A `started` delegating step, and perhaps the child | Creates the child under the same key, which returns the one that exists | Nothing |
| After `Park` commits | A waiting run with no lease | Nothing runs until a person or a child acts | Nothing |
| After the last step, before `Finish` commits | A runnable run with nothing left to do | Finishes | Nothing |

### 6.8 What "exactly once" means here

Stated as what holds and what does not:

- **A completed step is never executed again.** The journal is the record,
  and `next` passes over a final step. This holds across any number of
  crashes and takeovers.
- **A step is recorded once.** `(run_id, seq)` is the primary key and every
  write is fenced.
- **A model call that was interrupted is made again.** Model calls have no
  effect on the world, so this costs money and nothing else. The cost of
  the lost call is not in the journal.
- **A tool call that was interrupted is executed again, with the same
  key.** This is at-least-once. Whether the effect happens once depends on
  the tool:
  - A tool whose effect is a write to this Postgres makes it exactly once
    by calling `agentpg.Once(ctx, tx, in.Key)` in the transaction that makes
    the write, and skipping the write when it reports false. The key and
    the write commit together or not at all, and the unique index makes two
    concurrent attempts serialise.
  - A tool whose effect is elsewhere passes `in.Key` to the receiver as its
    idempotency key, or writes an outbox row under `Once` and lets
    `outbox.Relay` deliver it, which is at-least-once with a stable id for
    the consumer to dedupe on.
  - A tool that can do neither sets `AtMostOnce`. An interrupted call is
    then not made again until a person, shown the call, says to. This is
    at-most-once without a person and a human decision with one.
- **What was approved is what runs.** The approval shows the call's
  arguments from the journal, and the journal's arguments are what the tool
  receives. Nothing recomputes them in between.
- **An approval is decided once.** The decision is a compare-and-set from
  pending.
- **A run is started once per key.** `StartRequest.Key` is unique per
  agent.
- **Notifications are not guaranteed.** An `Event` is published after the
  write it reports, at most once. The journal is the record; 6.12 says how
  a consumer catches up.

Nothing here makes an effect outside Postgres happen exactly once on its
own. No system can, and the design says so rather than naming the
at-least-once case something else.

### 6.9 Budgets

A run has four limits, fixed in its snapshot when it starts.

| Limit | Measured as | Checked |
|---|---|---|
| `MaxDuration` | `Run.ActiveMillis`: the sum, over steps that completed, of finish time less start time. Time parked for a person or a child is not in it | Before every action that does work. A step in flight is also given a context that ends when the remainder runs out |
| `MaxCostMicros` | `Run.Usage.CostMicros`: what the `Model` reported, plus each child run's total | Before every action that does work |
| `MaxTokens` | Input plus output tokens, the same way | Before every action that does work |
| `MaxModelCalls` | `Run.ModelCalls` | Before a model call |

The totals are written in the same transaction as the step that incurred
them, so they are exactly as durable as the journal.

The check is "has the budget been reached", made before the next step. A
run can therefore pass a limit by the size of its last step: one model
call's cost, or one tool call's time. This is a deliberate difference from
`llm.Budgeted`, which refuses a call by its worst case before making it.
The engine does not know prices and cannot estimate; a service that wants
the tighter bound wraps its model in `llm.Budgeted` as well, and bounds one
call with `Definition.MaxTokens`.

A child run's cost limit is the smaller of its own and what its parent has
left when it is started.

A model call lost to a crash was billed and is not counted. The overshoot
from this is one call per crash.

### 6.10 Approvals

A tool call is put to a person when the `Guard` says `Ask`, when the tool
is marked `Approval`, or when an `AtMostOnce` call was interrupted.

Asking is one transaction (`RequestApproval`): the step becomes `waiting`
and an approval is inserted as pending, holding the tool's name, the
call's arguments, the action and the rule. When every step left is pending,
the execution parks: the run becomes `waiting` and its lease is released.

**A parked run survives a restart because nothing about it is in a
process.** There is no goroutine, no timer and no lease. There is a row in
`agent_runs` saying `waiting`, a row in `agent_steps` saying `waiting`, and
a row in `agent_approvals` saying `pending`. Every process can be stopped
and the run is exactly where it was.

Deciding is one transaction (`DecideApproval`), run by whichever process
receives the request: lock the run's row, move the approval from pending,
record who and why, and set a waiting run runnable. It writes nothing to
`agent_steps`. The next execution reads the answer and acts: approved, the
step starts and the tool runs; declined, the step is `declined` and the
model is told.

Parking and deciding can race: an execution may be about to park just as
the answer arrives. Both lock the run's row first, and `Park` then checks,
in a statement that runs after it holds the lock, whether anything is still
pending. If the answer came first, `Park` finds nothing pending, reports
false, and the execution carries on. If `Park` came first, the decision
finds a waiting run and sets it runnable. A run is never left waiting with
nothing to wait for. Child runs ending and cancellation use the same lock
for the same reason.

With `ApprovalTTL` set, an approval carries `ExpiresAt`. `Tick` calls
`ExpireApprovals` first, which lapses overdue approvals and sets their runs
runnable; a lapsed approval declines the call.

What each of these does in `agent/pg`, statement by statement. In every one
the row is locked first and what is decided on is read afterwards:

| Method | Statements, in order |
|---|---|
| `Park` | The fence, which locks the run. Then `select exists (a pending approval of the run) or exists (a child run that has not ended)`. Then the update that sets the run waiting and releases the lease. The look and the write are separate statements on purpose: one statement that carried the look as a condition would evaluate it in a snapshot taken before it waited for the row |
| `DecideApproval` | Lock the run the approval belongs to (`select r.id from agent_runs r join agent_approvals a on a.run_id = r.id where a.id = $1 for update of r`). Read the approval again. If it is not pending, return it with `ErrAlreadyDecided`, having changed nothing. Otherwise raise the run's `Rev` and wake it, and write the answer with that `Rev`. Of eight answers at once, seven read an approval that has its answer |
| `ExpireApprovals` | Lock every run that has a pending approval past its time, `order by depth desc, id`, waiting for each and passing over none. Then lapse what is due among those runs' approvals, read again now that the rows are held, each stamped with its run's `Rev` plus one. Then raise each of those runs' `Rev` once and wake it. With nothing due nothing is locked or written |
| `Finish` | The fence, on the run that is ending. The update that ends it. The update that cancels its pending approvals. Then, for a child, `select 1 from agent_runs where id = <parent> for update`, and `update … where id = <parent> and status = 'waiting'` |
| `RequestCancel` | Lock the run and read its status and its mark. `ErrFinished`, or nothing for a run already marked, or the update |

Two of these need saying why. `Finish` locks the parent's row whether or not
the parent is waiting, because an update that matches no row locks none: a
parent about to park would be passed by, and would then park on a child it
still saw as running, with nobody left to wake it. And `ExpireApprovals`
locks before it writes, in one order for every caller, for two reasons. The
order, children before parents, is the order a child's `Finish` takes the
same rows in, so a lapse and a child's end, or two workers' lapses, never
each hold a row the other wants. And an approval stamped with a `Rev`
computed from a run that was not locked could carry a `Rev` the run has
since been given by another write, and a reader that had seen that `Rev`
would never be sent the lapse. The order relies on a child's `Depth` being
its parent's plus one, which `CreateRun` holds every run to.

`Purge` takes rows in the same order. It locks every run it will remove, the
roots that ended before the cut and all that is under them, deepest first,
and only then deletes: the keys `Once` recorded under those runs' ids, and
the roots, whose steps, approvals and child runs go with them. A delete that
started at the root would hold the parent's row while its cascade waited for
a child's, which is the reverse of a child's `Finish`. Should Postgres end a
purge for a deadlock all the same, `Purge` tries once more before it returns
the error.

### 6.11 Child runs and cancellation

A tool with `Delegate` set starts a child run of the named agent and waits
for it. The child is an ordinary run with `ParentID`, `ParentSeq` and a
`Depth` one greater; any process may execute it. When several calls of one
reply delegate, all the children are started before the parent parks, so
they run at once on whatever workers there are.

When a child ends, `Finish` sets a waiting parent runnable in the same
transaction, after locking the parent's row. The parent's next execution
collects each finished child into its tool step and parks again if others
are still running.

`Cancel` marks the run and sets it runnable if it was waiting. A process
executing it learns from its next heartbeat and stops the step in flight;
otherwise the next execution sees the mark first thing. Either way that
execution asks for each unfinished child to be cancelled, cancels the run's
pending approvals, and finishes it `StatusCancelled`.

Lock order is always child before parent: a child's `Finish` holds the
child's row and takes the parent's. Cancellation never holds a parent's row
while taking a child's; it marks each child in its own transaction.

### 6.12 Events

After each change it makes, the engine publishes an `Event` to
`TopicRuns` on `Options.Events`. The interface is the shape of
`events.Publisher`, so the wiring is one line and `agent` imports nothing:

```go
bus := events.NewInMemoryBus(events.InMemoryBusOptions{})
engine, err := agent.New(agent.Options{Model: model, Store: store, Events: bus})
```

The types, and who publishes each: `EventRunStarted` by `Start`, for a run
that is new; `EventApprovalDecided` by `Approve` and `Decline`;
`EventRunCancelRequested` by `Cancel`, for the request that sets the mark;
and by the execution, `EventStepStarted`, `EventStepCompleted`,
`EventStepBlocked`, `EventApprovalRequested`, `EventRunWaiting`, and one of
`EventRunCompleted`, `EventRunFailed` and `EventRunCancelled` when the run
ends. A request to cancel and a run that ended cancelled are two events,
so that no type means two things: `EventRunCancelRequested`
(`"run.cancel_requested"`) says the run was asked to stop and is still to
end, and `EventRunCancelled` says it has.

The event says which run changed and how, and carries nothing from the
journal, so tool arguments and results never travel on a bus. Publishing
happens after the commit and is best effort: a failed publish is logged at
Debug and never fails a step, and `events.InMemoryBus` drops when a
subscriber is slow. A consumer that needs every change does not rely on the
event. It keeps the last `Rev` it saw and reads `Engine.Changes(ctx, runID,
rev)`, using events only as the hint to look. The HTTP stream in 6.13 works
this way, and so works whichever process is executing the run.

Delivery with a guarantee is the outbox's job and is left for later. The
store already makes each change in one transaction, so it will be an
option on `agent/pg` that is handed that transaction and calls
`outbox.Enqueue`.

### 6.13 HTTP surface

*package httpapi: agent/httpapi*

```go
// Package httpapi serves runs, their timelines and their approvals over HTTP.
package httpapi

// Runs is what the API needs from an engine. *agent.Engine satisfies it.
type Runs interface {
	GetRun(ctx context.Context, id string) (agent.Run, error)
	ListRuns(ctx context.Context, f agent.RunFilter) ([]agent.Run, error)
	Changes(ctx context.Context, runID string, since int64) (agent.Changes, error)
	ListApprovals(ctx context.Context, f agent.ApprovalFilter) ([]agent.Approval, error)
	Approve(ctx context.Context, approvalID, by, reason string) (agent.Approval, error)
	Decline(ctx context.Context, approvalID, by, reason string) (agent.Approval, error)
	Cancel(ctx context.Context, runID, by, reason string) error
}

// Options configures an API. Only Runs is required.
type Options struct {
	Runs Runs
	// Actor names who is making a request, for the record of an approval or
	// a cancellation. Nil, or an empty name, refuses those requests with
	// 403, so the zero value serves a read-only API.
	Actor func(r *http.Request) string
	// PollInterval is how often an event stream reads the journal. Default
	// 500 milliseconds.
	PollInterval time.Duration
	// Heartbeat is how often an idle event stream sends a comment line, to
	// keep proxies from closing it. Default 15 seconds.
	Heartbeat time.Duration
	// CrossOrigin guards the three routes that change something against a
	// request a browser makes for a page on another origin: 403, before
	// Actor is asked. Nil means a protection with no trusted origins; a
	// service whose front end is on another origin supplies one that trusts
	// it. Reading and the stream are not guarded.
	CrossOrigin *http.CrossOriginProtection
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// API is the HTTP surface of an engine.
type API struct{ /* unexported fields */ }

// New builds an API. It returns an error when Runs is nil.
func New(opts Options) (*API, error)

// Routes returns the router. Mount it behind whatever guards operator
// traffic: it serves the journal, which holds whatever the tools handled.
func (a *API) Routes() chi.Router

var _ Runs = (*agent.Engine)(nil)
```

| Route | Answers |
|---|---|
| `GET /runs` | `{"runs": […], "next": "<cursor>"}`, newest first. Query: `status`, `agent`, `parent`, `limit` (1 to 200, default 50), `cursor`. An unknown status or a limit out of range is 400, as `pg.ParsePage` refuses rather than clamps; so is a cursor that does not decode, whose time does not parse, or whose id is not a UUID in the canonical lower-case hyphenated form |
| `GET /runs/{id}` | The run, or 404 |
| `GET /runs/{id}/timeline` | `{"run": …, "steps": […], "approvals": […]}`: `Changes` since 0 |
| `GET /runs/{id}/events` | A server-sent event stream, below |
| `POST /runs/{id}/cancel` | 202. Body `{"reason": "…"}`, optional. 409 for a run that has ended |
| `GET /approvals` | `{"approvals": […]}`, oldest first. Query: `status`, `run`, `limit` |
| `POST /approvals/{id}/approve` | The approval as decided. Body `{"reason": "…"}`, optional. 409 when already decided |
| `POST /approvals/{id}/decline` | The same |

Responses go through `httpx.JSON`, and failures through `httpx.NotFound`,
`httpx.BadRequest` and `httpx.Error`, so error bodies are the generic ones
and the reason is in the log. What the engine answered (`ErrNotFound`, the
two conflicts) is told whatever became of the request. After that, a request
whose context was cancelled is 503, logged at debug level and not as a
fault: a cancelled context does not say the client has gone (a server
shutting down, a client that half-closes after sending and a mount's
middleware cancel it with the client still reading), so it is answered, which
costs nothing if the client is gone. One whose time ran out is 503 too. No
failure leaves the status a handler starts with, which would read as success;
a body that cannot be read to the end is treated as a cancelled call, and one
that is too long is 400. Everything but the stream is sent with
`Cache-Control: no-store`: it is journal content. `Message.Opaque` is removed
from every step before it is served: it is the provider's private data,
large, and of no use to a reader.

The three routes that change something need `Options.Actor` to return a
name, and answer 403 without one. A name is recorded for good, so it is
what is left after the white space round it, and it is no name when it is
empty, is not valid UTF-8, is over 256 bytes, holds a control character (a
newline, a NUL, a tab, a line or paragraph separator), or has nothing to see
in it (no letter, mark, number, punctuation or symbol, so a name of only
zero-width characters is nobody). The zero `Options` therefore serves a
read-only surface. `Actor` is a name for the
record and not an authorisation: the package does not authenticate and does
not decide who may decide which approval, so anyone `Actor` names can
approve, decline or cancel anything the API can see, and authorisation is
the mount's. A request body is read through `http.MaxBytesReader` at 4 KiB
and decoded with unknown fields disallowed; a reason that is not valid UTF-8
or has a NUL in it is 400, since a database could not keep it.

Those three routes are also behind `http.CrossOriginProtection`
(`Options.CrossOrigin`), checked before `Actor` is asked. A request that
`Sec-Fetch-Site` marks as anything but same-origin or none, or that has no
such header and an `Origin` other than its host, is refused with 403 unless
the protection trusts that origin; a request with neither header, which is a
server-side client and not a browser acting for a page, passes. It closes the
cross-site form post against a cookie session, and nothing more: it is not
authentication, `Actor` only names who is acting, for the record, and it does not guard
the reads or the stream, which change nothing.

A cursor comes from the client, so it is input and is decoded strictly, in
the package and before the engine is asked: a token that is not what the
package made, a time that does not parse, or an id that `uuid.Parse` does not
read back to the same string is 400.

The package has no route that starts a run. What a run's input is, and who
may start one, belong to the service; the example has its own.

The stream. `GET /runs/{id}/events` answers 404 for an unknown run before
any stream starts. Then, through `httpx.NewEventStream`:

1. `since` is `Last-Event-ID` as a non-negative integer, or 0. A value that
   is not a number, is negative, or overflows is 0, and a value ahead of
   the run's own `Rev` is read again from 0: the stream carries state, so
   sending it all again is always safe, where refusing the header would
   strand an `EventSource`, which cannot change what it sends, and waiting
   for a run to reach a revision it never had would skip every change in
   between.
2. Read `Changes(id, since)`. This first read is the lookup, made before
   the stream opens, so that an unknown run is a 404 and a failing store a
   500. Send one `step` event per step and one `approval` event per
   approval, in `Rev` order (a step before an approval of the same
   revision), with no id. Then send one `run` event whose id is `Run.Rev`,
   and set `since` to it. Sending the id last means a client that
   reconnects mid-batch is sent the whole batch again, which is harmless:
   each event is the current state of one thing. When the read holds
   nothing and `Run.Rev` has not passed `since`, nothing is sent, not even
   the `run` event.
3. If the run has ended, send an `end` event, whose data is `{}` and which
   has no id, and return. A client closes its event source on `end`: a
   reconnect to a finished run is sent `end` again, so one that did not
   would loop at the browser's retry interval.
4. Wait `PollInterval`, sending a comment line every `Heartbeat` while
   nothing is sent (any send restarts the wait), and go to 2. Return when
   the request's context ends, when a send fails (the client has gone and
   the stream is unusable), when a read fails, or when an event cannot be
   encoded (a step whose stored arguments are not JSON). In the last two the
   stream ends with no `end` event, which is how a client knows to reconnect
   from the id it has, and the cause is logged: a read that failed because
   the request is over quietly, and an event that cannot be encoded as a
   fault, since the client will meet it again at every reconnect.

A `HEAD` request on the route is answered with the headers a stream has
and no stream, after a `GetRun` for the 404 (a GET's lookup is its first
`Changes`), and the package keeps the two in step with a test; a stream started for a `HEAD` would only wait for the client to
leave. A writer that cannot flush is answered with an ordinary 500, before
anything is written.

The stream carries state, not history: a step that started and completed
between two polls appears once, completed. The journal is the history.

Under the default router the request context ends after 30 seconds
(`httpx.DefaultTimeout`), which ends the stream; `EventSource` reconnects
with `Last-Event-ID` and loses nothing. A deployment that wants one long
connection mounts the surface on a router built with `Timeout: -1`, as
`RouterOptions` already documents for streaming.

### 6.14 In-process default

`MemoryStore`: the whole `Store` contract behind one mutex, copying what
it is given and what it returns. Runs last as long as the process. It is
what `Options.Store` is when nil, and what the engine's own tests run on.
Two `Engine`s over one `MemoryStore` behave as two processes over one
database, which is how takeover is tested without Postgres.

### 6.15 Tests

Without a database, on `MemoryStore`, `agenttest.Clock` and
`agenttest.Model`:

- `next`, table-driven: one case per row of 6.7 giving the journal a crash
  leaves and the action expected, and one per rule in 6.5.
- `conversation`: a golden for a journal with a blocked, a declined and a
  completed call in one reply; an open reply contributes no tool message.
- The engine, one case each: a run with no tools; a tool error returned to
  the model; `ErrTransient` retried with the same `Key` and a higher
  `Attempt`; a malformed call; a tool missing from the build; the guard
  blocking; the guard asking, then approve, then the tool receives the
  journaled arguments; decline; a tool marked `Approval`; an approval
  lapsing; an `AtMostOnce` call interrupted and approved, and declined;
  each of the four budgets, the time budget driven by the clock; refusal,
  truncation and pause; a guard error neither allowing nor blocking; a
  permanent model error; `MaxFailures`.
- Child runs: three delegations in one reply all started before the parent
  parks; the parent collecting them as they end; a child's usage added to
  the parent; a failed child reported to the model; the cost limit
  inherited; `MaxDepth`.
- Cancellation: of a waiting run, of a run being executed (through the
  heartbeat), reaching the children, cancelling pending approvals.
- The lease, with two engines on one store: a second engine cannot claim a
  live lease; it claims after the clock passes `LeaseTTL`; the first
  engine's next write is `ErrLeaseLost` and it stops; a clean yield leaves
  the lease free at once.
- The snapshot: a `Definition` registered again with a changed prompt and a
  changed tool list between two executions of one run; the model is still
  sent the prompt and tools the run started with.
- **Crash at every point.** One scripted run with a model call, an allowed
  tool, a delegating tool and an approval. First it runs uninterrupted on a
  `FaultStore` to count its store calls, N. Then for every n from 1 to N,
  and for both `KillBefore(n)` and `KillAfter(n)`: engine A runs until the
  store dies; the clock passes the lease; engine B, on the same inner
  store, runs to the end, approving when asked. After each, four things are
  asserted: the run ends with the status and output of the uninterrupted
  run; every tool key was executed once or twice, and exactly once when its
  `completed` write landed before the kill; every request the model was
  sent has, as a prefix, every earlier request sent for that run, with the
  same system prompt and tools; and `seq` runs from 1 with no gaps. The
  control that must fail: the same loop over a store wrapper that discards
  the `started → completed` write must report a tool executed more than
  twice, so a suite that stopped checking would be noticed.
- `Work`: claims up to `Concurrency`; on cancel, lets a step in flight
  finish and yields; past `DrainTimeout`, stops without writing.
- Events: the sequence of types for a run with an approval.
- `agent/httpapi`, against a fake `Runs`: each route's status and body; a
  missing actor is 403; an oversized or unknown-field body is 400; `Opaque`
  is absent from served steps; the stream sends steps before the `run`
  event, resumes from `Last-Event-ID`, ends on a finished run, and returns
  when the client goes away.

With `pg/testdb`:

- `agenttest.RunStoreSuite` against `agent/pg`. The same suite runs against
  `MemoryStore` without a database, so the two cannot drift. It covers
  every method's contract as written on `Store`: idempotent create on a
  key; claim order, `skip locked`, the lapsed-lease failure count,
  `NextAttemptAt`; fenced writes after a second claim; `Park` refusing when
  nothing is pending and when cancellation is requested; `Finish` waking a
  waiting parent and cancelling pending approvals; `RequestApproval` asked
  twice; `DecideApproval` twice; expiry; `Changes` by revision.
- Concurrency, under `-race`: eight goroutines claiming one run, exactly
  one wins; `Park` racing `DecideApproval` a few hundred times, never
  leaving a waiting run with nothing pending; `Once` from two transactions,
  one first.
- The JSON columns give back what was stored: a message whose `Opaque`
  holds keys out of alphabetical order reads back with the same values in
  the same order.
- `Purge` removes ended root runs and their children and nothing else.
- The repository-wide replay check finds `agent/pg/migrations` on its own.

How the scripted model drives these: `agenttest.Model` picks its reply from
the request alone, by the number of assistant turns in it, as `llm.Scripted`
does. A resumed execution sends the model the conversation as far as the
journal has it and gets the reply for that point, so a test needs no
coordination between the model and the store, and a model call made twice
gets the same answer twice.

### 6.16 Left out

- Running the calls of one reply in parallel.
- Streaming tokens into a run's event stream. The journal is per step.
- Compaction. A run that fills the context window fails with
  `ReasonContextWindow`. When it comes it will be the reference's simple
  compaction, which carries no earlier thinking forward and so keeps the
  append-only rule.
- Validating tool arguments against the schema. A tool validates its own
  input; `StrictTools` in the adapter asks the provider to.
- Memory across runs.
- Guaranteed delivery of events, and waking a stream by `LISTEN`/`NOTIFY`
  instead of polling.
- A scheduler entry. `Engine.Work` is a loop the service runs; the example
  shows it. `Tick` is exported for a service that wants a `jobs.Entry`.
- Metrics instruments.

## 7. `httpx`: server-sent events

`httpx` has no way to stream today, though it does not prevent one: the
middleware's response writers implement `Flush` and `Unwrap`, and
`RouterOptions.Timeout` already says to disable the timeout for streaming.
Two things stand in a handler's way. The router's timeout ends the request
context after 30 seconds, and the server's `WriteTimeout`, 60 seconds by
default, closes the connection. One new file, `httpx/sse.go`, gives
handlers the writer. The one existing file it touches is the response
recorder in `httpx/middleware`, described after the code:

*package httpx: httpx/sse.go*

```go
package httpx

// ErrStreamUnsupported is returned by NewEventStream when the response
// cannot be flushed, so events would sit in a buffer.
var ErrStreamUnsupported = errors.New("httpx: response cannot be streamed")

// ServerEvent is one server-sent event.
type ServerEvent struct {
	// ID is sent as the event's id, which a reconnecting client returns in
	// Last-Event-ID. Empty sends none.
	ID string
	// Type is the event name. Empty sends none, which a client reads as
	// "message".
	Type string
	Data []byte
}

// EventStreamOptions configures NewEventStream. The zero value works.
type EventStreamOptions struct {
	// Retry is sent once as the delay a client should wait before
	// reconnecting. Zero sends none.
	Retry time.Duration

	// SendTimeout bounds each write the stream makes, the opening and every
	// Send, SendJSON and Comment, so that a client that stays connected and
	// stops reading cannot hold the handler in a write for ever. It takes
	// the place of the server's WriteTimeout, which no longer applies to the
	// stream once it is open. Zero means 30 seconds. A negative value means
	// no bound, and so does a writer that cannot carry deadlines; writes are
	// then unbounded. A send that passes the limit returns an error, and the
	// stream is unusable afterwards. Time between sends does not count.
	SendTimeout time.Duration
}

// EventStream writes server-sent events to one response. It is for the one
// goroutine that owns the response, and is not safe for concurrent use.
type EventStream struct{ /* unexported fields */ }

// NewEventStream starts an event stream on w: it sets the headers, lifts
// the server's write deadline for this response, and flushes.
func NewEventStream(w http.ResponseWriter, r *http.Request, opts EventStreamOptions) (*EventStream, error)

// Send writes one event and flushes it.
func (s *EventStream) Send(ev ServerEvent) error

// SendJSON writes v as the data of one event.
func (s *EventStream) SendJSON(id, typ string, v any) error

// Comment writes a comment line, which clients ignore.
func (s *EventStream) Comment(text string) error

// LastEventID is the id of the last event a reconnecting client received,
// or "".
func LastEventID(r *http.Request) string
```

`NewEventStream` sets `Content-Type: text/event-stream`,
`Cache-Control: no-cache` and `X-Accel-Buffering: no`, calls
`http.NewResponseController(w).SetWriteDeadline(time.Time{})` to lift the
write deadline for this response (ignoring `http.ErrNotSupported`), writes
the `retry:` line when asked, and flushes. It returns
`ErrStreamUnsupported` when the writer at the end of the `Unwrap` chain
cannot flush, which it finds out before writing any header, so the handler
can still answer with an error. It is the end of the chain that has to
flush, not any writer in it: a wrapper's flush is passed down, so one that
stops short would leave events in a buffer whatever the wrappers above it
offer, and the router's recorders offer `Flush` over whatever they wrap.
`Send` writes
`id:` and `event:` when set and one `data:` line per line of `Data`, a line
ending in a line feed, a carriage return or both, then a blank line, then
flushes. It refuses, and writes nothing for, an `ID` containing a line break
or a NUL character (a browser's `EventSource` ignores an id with a NUL, which
would lose the resume point without a sign), a `Type` containing a line
break, and `Comment` text containing one.

The write deadline stays lifted between sends, which is what lets a stream
outlive `WriteTimeout`: once a stream is open the server's own
`WriteTimeout` no longer applies to it, and the per-send limit takes its
place. Each write the stream makes, the opening and every send, sets the
deadline to now plus `SendTimeout` before it writes and lifts it again
after, so a client that stays connected and stops reading holds the
handler's write for at most that long, and the time between writes does not
count against the next one. Writes are unbounded when `SendTimeout` is
negative, or when the writer cannot carry deadlines. A send that hits the
deadline returns its error and ends the stream: the write may have left part
of an event on the wire, so every later call returns the same error.

The stream does not outlive the request context: once `r.Context()` ends,
`Send`, `SendJSON` and `Comment` write nothing and return an error wrapping
the context's, and the handler returns. The router's timeout ends that
context, so under the default router a stream lasts 30 seconds, and a
deployment that wants a longer one builds its router with `Timeout: -1`
(6.13). Through a writer that discards flush errors, such as chi's
compressor, the send that was held up reports success and the handler has
been held for one limit; net/http closes the connection after the failed
write, which ends the request's context, and the next send fails.

Two defects in the recorder that `RequestLog`, `Observe` and `Recoverer`
put round the writer (`httpx/middleware/responsewriter.go`) would have
undone this. It offered only `Flush`, which cannot return an error, so a
flush that timed out went unreported and a send could not tell; it now also
offers `FlushError`, which `http.ResponseController` prefers. And it
forwarded every `WriteHeader` it was given, so the 504 that chi's `Timeout`
writes after a stream its deadline has ended made net/http log a superfluous
call, a warning for every stream every 30 seconds; it now forwards only a
response's first status, however that status was written or implied, and
passes a 1xx through as the informational response it is, if no final status
has gone out. After a hijack it forwards nothing and reports
`http.ErrHijacked` for a write or a flush.

It belongs in `httpx` and not in `agent/httpapi` because it is what any
service streaming model output to a browser would otherwise write itself.

Tests, without a database: the bytes written for each field combination and
for multi-line data; the three headers; a writer that cannot flush; a line
break in an id; `LastEventID`; and, against a real `httpx.Server` with a
100 millisecond `WriteTimeout`, a stream that keeps delivering past it,
including past its own `SendTimeout`. Also against a real server: a client
that stops reading, whose send returns an error within `SendTimeout`, and a
stream ended by the router's timeout, which logs nothing at warning level.
Without a server: a router mounted under a writer that cannot flush, which
leaves the handler able to answer.

## 8. How the packages meet

`llm`, `policy` and `agent` do not import one another. Each side declares
what it needs, and two adapters connect them. They live in `app`, the one
package the layering lets depend on everything, in one new file,
`app/agents.go`. Nothing in `app.go` changes.

*package app: app/agents.go*

```go
package app

// AgentModelOptions configures AgentModel. The zero value prices nothing.
type AgentModelOptions struct {
	// Prices turns each reply's tokens into the cost a run's budget is
	// measured in. Nil prices nothing: every reply costs zero. Anything else
	// is a table, and a table that lists no model that answers, an empty one
	// included, fails the run on every reply rather than leaving its cost limit
	// silently off: a reply that neither the model it names nor the model
	// priced for the request can price is a permanent error. AgentModel keeps a
	// copy of the table.
	Prices llm.Prices
	// Effort is sent with every request.
	Effort llm.Effort
	// StrictTools asks the provider to guarantee tool arguments match their
	// schemas.
	StrictTools bool
	// DefaultMaxTokens bounds the reply to a request that sets no MaxTokens.
	// Zero is 16000, the number llm.Budgeted assumes for a request that sets
	// none: the adapter always sends a bound, so that a budget's hold is a
	// real upper bound on the call. It goes to every model the adapter
	// serves, so a model with a lower output cap needs it set lower.
	DefaultMaxTokens int
	// Model is the model a request that names none is sent to, and priced as.
	// It is llm.BudgetOptions.Model's counterpart: give both the same name, so
	// that the budget and the adapter settle the same price.
	Model string
	// Logger receives one warning for each refusal, with the provider's reply
	// id, the model and the refusal's reason. Nil uses slog.Default(). Neither
	// the prompt nor the reply's text is logged.
	Logger *slog.Logger
}

// AgentModel adapts an llm.Model to agent.Model.
func AgentModel(m llm.Model, opts AgentModelOptions) agent.Model

// AgentGuard adapts a policy.Decider to agent.Guard.
func AgentGuard(d *policy.Decider) agent.Guard
```

`AgentModel`, per call:

| `agent.Request` | `llm.Request` |
|---|---|
| `Model` | The same field; when it is empty, `AgentModelOptions.Model` |
| `System` | The same field |
| `MaxTokens` | The same field; when it is zero or less, `DefaultMaxTokens`. A request the adapter sends always sets one |
| `Messages`: `Role`, `Text`, `Calls`, `Results`, `Opaque` | `Role`, `Text`, `ToolCalls`, `ToolResults`, `Opaque`, field for field: a call's `ID`, `Name`, `Input` and `Malformed`, a result's `CallID`, `Content` and `IsError`, an opaque form's `Provider` and `Data` |
| `Tools` | `Tools`, with `Strict` from `StrictTools` |
| `Output` | `Output: &llm.Schema{Name: "answer", JSON: …}` when it is not empty |
| | `Effort` from the options |

`RunID` and `Agent` are not sent: `llm.Request` has no place for them. Every
slice and byte string is copied, a nil one stays nil and an empty one stays
empty, and no JSON is decoded or encoded, so an assistant turn, its tool calls
and its opaque provider form go back to the provider exactly as they came, key
order and spacing included.

| `llm.Response` | `agent.Response` |
|---|---|
| `Message` | `Message`, field for field |
| `Stop` | The constant of the same name; `StopSequence` becomes `StopEnd`. A reason `agent` has no name for is a permanent error |
| `Model` | `Model` |
| `Usage`, and `Attempts` when there are some | `InputTokens` is input plus cache reads plus cache writes, and `OutputTokens`, summed over every attempt, or over the one `Model` and `Usage` when there are none; `CostMicros` is `Prices.CostFor(resp, asked)` with the model the request was sent to as `asked`, and zero when `Prices` is nil |

A refused reply is meant to carry no text and no tool calls, and the adapter
makes that so: when the stop is a refusal, the `agent.Response` has the role
and nothing else, no text, no calls and no opaque form (which would replay the
calls dropped), whatever the provider put in the reply, so that an engine that
journals calls before it reads the stop has none to run. The usage and the cost
are still reported, since a refusal is billed. A refusal is a reply, not an
error, including one `Fallback` returns after the models behind it failed. Its
reason has no place in `agent.Response`: the adapter logs one line at warning
level through `Logger`, with the provider's reply id, the model, and the
refusal's category and explanation when the reply carries them, and never the
prompt or the reply's text. Any other content passes as it is given.

Not carried: the run id and the agent name are not sent to the model, and the
provider's reply id and the refusal's details are logged, not returned.

Errors are classified as `llm.Retryable` classifies them, which trusts an
`*llm.Error` in the chain before it looks for a context error:

- The caller's own cancellation or deadline, read from the context given to
  `Generate`, is returned as it is: neither retryable nor permanent. It decides
  before anything else: a budget refusal, an `ErrNoPrice`, a reply that cannot
  be priced or a reply with a stop `agent` has no name for, met while the
  context is done, is returned as it is too. A worker that is shutting down
  must not fail a run for good, and an error that is real comes back, and is
  marked, on the retry.
- `ErrBudgetExceeded`, `ErrNoPrice`, a reply that cannot be priced, and an
  `*llm.Error` that is not retryable are returned wrapped in
  `agent.ErrPermanent`, and still unwrap to themselves.
- A retryable `*llm.Error`, a plain error, and anything else are returned as
  they are, and the engine tries again later.
- A nil reply with a nil error is an error, and not a permanent one.

With `Prices` not nil, a reply that is priced at neither the model it names nor
the model the request was sent to is an `ErrNoPrice` and so permanent, and an
empty table prices nothing: a run with a cost budget must not treat an unpriced
call as free. A request that names no model is sent to, and priced at,
`AgentModelOptions.Model`, as `llm.Budgeted` prices it at its own
`BudgetOptions.Model`. The reply is priced as
`llm`'s budget and meter price it, so a provider that answers an alias with a
dated id costs what the alias costs (decision 59).

`AgentGuard` copies `Kind`, `Target` and `Attrs` into a `policy.Action`,
calls `Decider.Decide`, and copies `Effect` and `Rule` back. The three are
passed through unchanged: no trimming, no case folding, no cleaning of a
target, and no attribute converted to another type, because `policy` matches
names and strings exactly as written and reads an attribute it cannot compare
as a reason to ask or block. When the policy's decision names something that
could not be evaluated in `Decision.Uncertain`, `agent.Decision`, which carries
only `Effect` and `Rule`, gets it in the rule: `<rule> (could not evaluate:
<names joined by ", ">)`. The run's timeline and the approval a person is
shown then say why a rule whose condition looks unmet decided; the structured
field stays in the policy's own record. A decision that could not be recorded
is an error and no decision. A decision that can never be recorded (`policy.ErrUnrecordable`:
a NUL, a value no store holds) comes back wrapped in `agent.ErrPermanent` as
well, so the engine refuses the call instead of retrying it for ever; a store
that is down is returned as it is and tried again; and with the caller's
context done the error is the caller's, as for `AgentModel`.

Composition is the caller's. The `llm` wrappers go outermost first as
`Budgeted`, `Metered`, `Fallback`, then one `Retrying` per provider (4.7); the
budget is outermost, so the meter records only the calls it let through, and a
call it refuses reaches the adapter as a permanent error.

Wiring, as the example does it:

```go
client, err := anthropic.New(anthropic.Options{
	APIKey:          cfg.AnthropicAPIKey,
	RefusalFallback: "default",
})
metered := llm.NewMetered(llm.NewRetrying(client, llm.RetryOptions{}), llm.MeterOptions{Prices: prices})
model, err := llm.NewBudgeted(metered, llm.BudgetOptions{
	MaxCostMicros: 50_000_000, // $50, across every run this process makes
	Prices:        prices,
	Model:         anthropic.DefaultModel,
})

rules, err := policy.Parse(policyJSON)
decider, err := policy.NewDecider(rules, policy.Options{Recorder: policypg.New(pool)})

engine, err := agent.New(agent.Options{
	Store:  agentpg.New(pool),
	Model:  app.AgentModel(model, app.AgentModelOptions{Prices: prices}),
	Guard:  app.AgentGuard(decider),
	Events: bus,
})
```

The other meeting points need no adapter:

- `events.InMemoryBus` is an `agent.Publisher` as it stands.
- `*agent.Engine` is an `httpapi.Runs` as it stands.
- `agentpg.MigrationsFS` and `policypg.MigrationsFS` go in
  `app.Options.Migrations` beside the service's own, sharing the ledger.
- `httpapi.Options.Actor` is the service's: `admin.AdminIDFromContext` when
  the surface is mounted behind `admin.Service.RequireAdmin`.

`app` gains no lifecycle for the worker in this milestone. The example
starts `engine.Work` in a goroutine beside `app.Run` and waits for it,
bounded by the shutdown timeout. Folding that into `app.Options` is worth
doing once there is a second thing to run that way.

## 9. `examples/agent`

A service to be read and run, like the other examples. It needs Postgres
and, by default, nothing else: the model is scripted.

Two agents. `coordinator` lists the batch, delegates each document to a
`reviewer`, sends one digest, and tries to delete the originals.
`reviewer` reads one document and saves a one-line summary.

| Agent | Tool | Action | Under the example's rules |
|---|---|---|---|
| `coordinator` | `list_documents` | `read` | Allowed |
| | `review_document`, delegating to `reviewer` | `delegate` | Allowed |
| | `send_digest` | `send`, `external: true`, and `text_rule` when `textpolicy` matched the body | Asks a person |
| | `delete_document` | `delete` | Blocked |
| `reviewer` | `read_document` | `read` | Allowed |
| | `save_summary` | `write` | Allowed |

`save_summary` and `send_digest` write to Postgres inside a transaction
that begins with `agentpg.Once`, so each happens once however many times the
process is killed. The rules are `policy.json`, embedded and parsed at
startup: data, as the package says rules are.

Files:

```
main.go        wiring: configuration, app, engine, worker, routes
config.go      the environment this service reads, and what it refuses
agents.go      the two definitions and their tools
documents.go   the documents table: list, read, save a summary, record a digest
policy.json    the rules
policy.go      loading them; the textpolicy check that feeds send_digest's action
model.go       building the model: scripted, anthropic or openai
script.go      the scripted model's replies, as a function of the request
prices.go      the price table, with the date and address it was read from
handlers.go    POST /api/batches; the operator-token middleware
migrations/    001_agent_example.up.sql, .down.sql: documents and digests, seeded
main_test.go   the service as the README runs it
eval_test.go   the evaluation-style test and the kill-and-resume test
README.md      the two-minute script
```

Configuration: `DATABASE_URL` and `OPERATOR_TOKEN` are required.
`LLM_PROVIDER` is `scripted` (default), `anthropic` or `openai`.
`ANTHROPIC_API_KEY`; `COORDINATOR_MODEL`, default `claude-opus-5-5`;
`REVIEWER_MODEL`, default `claude-haiku-4-5-20251001`; `OPENAI_BASE_URL`,
`OPENAI_API_KEY`, `OPENAI_MODEL`. `LEASE_TTL`, default 5 seconds, so a
killed run is taken over within the time it takes to say so. `STEP_DELAY`,
default 750 milliseconds, slept by each tool so there is a run to kill.
`RUN_MAX_COST_MICROS`. `MIGRATE_ON_START`, `PORT`, `ENV`,
`SHUTDOWN_TIMEOUT` as the other examples.

Routes: `POST /api/batches` starts a `coordinator` run and answers 202 with
it; an `Idempotency-Key` header becomes `StartRequest.Key`. `agent/httpapi`
is mounted at `/agent`. Both sit behind a middleware that compares a bearer
token with `OPERATOR_TOKEN` in constant time, and `Actor` returns
`"operator"`. The README says what a real service does instead: mount
behind `admin` and name the admin.

The README is the script:

1. Start it. One command, no key.
2. `POST /api/batches`, and open the event stream with `curl -N`.
3. `kill -9` the process while the reviewers are working. Start it again.
   The log says which run it resumed and at which step; the timeline shows
   each tool call once; the summaries table has one row per document.
4. The run is waiting. `GET /agent/approvals` shows the digest it wants to
   send, with the rule that asked. Restart the process; it is still
   waiting. Approve it. The run carries on and the digest is recorded.
5. The timeline shows `delete_document` blocked, and names the rule.
6. `go test ./examples/agent/`.

Tests, with `pg/testdb`, a scripted model and no network:

- `TestBatchEval` is the evaluation. It runs the batch to the approval,
  approves, runs to the end, and scores the outcome the way an evaluation
  does, as named checks over the journal and the tables: every document has
  a summary; one digest was recorded and it names every document; no
  `delete` step is anything but `blocked`; the blocking rule is the one in
  `policy.json`; the run's cost is under its budget; the coordinator made
  no more than four model calls. Each check has a name and the test reports
  which failed.
- `TestKillAndResume` runs the batch on engine A over an
  `agenttest.FaultStore` around `agentpg.Store`, kills the store while a
  `save_summary` is in flight, moves the clock past the lease, and runs
  engine B over a fresh store on the same database to the approval. It
  asserts one summary row per document, that the interrupted tool was
  invoked twice with one key, and that no completed step was run again.
  Then it builds a third engine, as a restart would, and asserts the run is
  still waiting on the same approval before approving it.
- `TestBlockedActionIsRecorded` asserts the `delete` decision is in
  `policy_decisions` with its rule.
- `var _ httpapi.Runs = (*agent.Engine)(nil)`, so the surface and the
  engine cannot drift apart unnoticed.

One name to know about: from the repository root, `go build
./examples/agent` fails, because the binary would be called `agent` and
`agent/` is a directory. `go run ./examples/agent` and `make build` are
unaffected. The README uses `go run`.

## 10. Layering

`ARCHITECTURE.md` lists five import levels and says a package may import
anything strictly below it and no sibling. The new packages go in as
follows, and the document is updated to say so.

| Level | Package | Keel imports |
|---|---|---|
| 2 | `policy` | None |
| 2 | `policy/pg` | `policy`. It sits with its owner, as `auth/pg` does |
| 3 | `llm` | `retry` |
| 3 | `llm/anthropic`, `llm/openai` | `llm`, `llm/internal/sse` |
| 3 | `agent` | None |
| 3 | `agent/pg` | `agent`, `pg` |
| 3 | `agent/httpapi` | `agent`, `httpx` |
| 3 | `agent/agenttest` | `agent` |
| 5 | `app` | Gains `llm`, `policy`, `agent` |
| Outside | `examples/agent` | Whatever it needs |

`policy` is at level 2 beside `textpolicy`: it is pure, and it has no
dependency outside the process. `llm` is at level 3 because it uses `retry`
at level 2 and owns a dependency outside the process. `agent` is at level 3
for the same second reason, though the root package imports nothing from
Keel. `llm` and `agent` are siblings and do not import each other; `policy`
is below both and neither imports it.

The mechanical test in `ARCHITECTURE.md` holds: a service with no HTTP
server can `go get` `agent` and `agent/pg` and compile, since only
`agent/httpapi` reaches `httpx`; and a service can import `llm` without
either provider.

`httpx/sse.go` adds no import to `httpx`.

## 11. Later milestones

Named so that this milestone leaves room. None is planned here.

**`eval`: evaluation as `go test`.** A case is an input, an agent and a set
of named checks over the run it produces: over its output, its journal, its
cost. A suite runs under `go test`, against a scripted model by default and
a real one behind a build tag, and reports a score per check as well as
pass or fail. It needs what this milestone already has: `Engine.Execute`
to run one run until it ends or parks, `Engine.Timeline` to read what
happened, and `Scripted` for determinism. It will add a recording wrapper around
`llm.Model` that writes replies to files a `Script` can play back, which
`llm.Script` being a plain function leaves room for. `TestBatchEval` in the
example is the shape, written by hand.

**Vector search in `search`, and a readable `memory`.** `search.Index`
gains a vector query beside the text one, on Postgres with `pgvector` when
the extension is present and by exact scan when it is not, with
`llm.Embedder` as the source of vectors and `HashEmbedder` as the default
that needs nothing. `memory` is built on it: what an agent has learned,
stored as rows a person can read, edit and delete, not as an opaque blob.
`agent` will consume it through one more small interface, set on the
`Definition`, and recall will arrive in the conversation as appended
messages, which the append-only rule already requires.

**`infer`: small models in process.** Classification, embedding and
reranking that run inside the service with no network, behind `llm.Embedder`
and a small classification interface, with the heavy runtime in a
subpackage as the heavy clients are today. `HashEmbedder` is the
placeholder it replaces. Nothing in this milestone assumes a model is
remote: `Model` and `Embedder` are interfaces, and `Error.Status` is
already zero when there is no HTTP.

**`trace`: timeline, cost and replay.** Reading a run as a tree of steps
with their durations, attempts and costs, across child runs; cost by agent,
model and day; and replay, which re-executes a journal against a script
built from its own recorded replies to find where a changed prompt or tool
changes the outcome. The journal already keeps what this needs: `seq` and
`turn` for order, `attempts`, `started_at` and `finished_at`, tokens and
cost on every step, the reply of every model step as it came, and
`parent_id` with `parent_seq` for the tree. The metrics instruments for
agents and model calls come here too, since `metrics.Instruments` has to
grow for them.

## 12. Decisions the conventions did not settle

Each is a choice the repository's conventions left open, with what was
rejected and why. The first nine are the ones worth a second opinion.

1. **`policy` sits beside `textpolicy`.** Rejected: growing `textpolicy`
   into it. Their inputs differ (text, an action), their matching differs
   (patterns over normalised text, conditions over attributes), and
   `textpolicy`'s yes-or-no has no "ask". Merging would give one package
   two unrelated rule types. They meet through an attribute instead.

2. **The middle effect is `Ask`, written `"ask"`; `"approve"` is read as
   it.** Rejected: `"approve"` as the wire value, matching the reference
   exactly. In an agent's timeline "decision: approve" on a call still
   waiting reads as approved, and `agent.Engine.Approve` means a person
   said yes. The decoder accepts the reference's word so its fixtures
   still load.

3. **No rule matched means block.** Rejected: allow, and making the caller
   choose. The reference cannot reach this case, since its kinds are a
   closed set with a default each. An open set of kinds needs an answer,
   and an action nobody wrote a rule for is not one anybody allowed.
   `Policy.Default` overrides it.

4. **Lease times use the injected clock, not the database's.** Rejected:
   `now()` in SQL. The engine consumes a clock for its budgets and its
   journal already, and a lease that a test clock can move makes takeover
   testable without sleeping. The cost is that workers' clocks must roughly agree. Fencing
   by epoch, not time, is what keeps the journal safe, so a wrong clock
   costs repeated work and never a corrupt journal.

5. **An interrupted tool call is run again with the same key, unless the
   tool is `AtMostOnce`.** Rejected: parking every interrupted call for a
   person. That is the safer default and makes every deploy that lands
   mid-call into a queue of questions. The key is there so that running
   again is safe, and `Once` makes it safe for the common case with one
   line. A tool that cannot honour a key says so.

6. **Events are a hint; the journal is read by revision.** Rejected: an
   append-only events table, and publishing through the outbox in this
   milestone. A second table doubles every write to serve a stream that
   wants current state, and the outbox would make `agent/pg` import a
   sibling or take a hook nothing yet needs. Polling `Changes` by `Rev`
   works from any process and across restarts with what the journal
   already has.

7. **The adapters go in `app`, as one new file.** Rejected: only in the
   example, to be copied. The mapping between `llm.Message` and
   `agent.Message`, with `Opaque`, is where a subtle bug would live, and
   every service needs the same one. The cost is that importing `app` now
   brings in `llm`, `policy` and `agent`, which are light: the standard
   library and one UUID package.

8. **`httpx` gains a server-sent event writer.** Rejected: keeping it
   private to `agent/httpapi`. It is thirty lines any streaming handler
   needs, and the write-deadline detail is easy to get wrong. It is a new
   file; what it changed in `httpx/middleware` is in 40.

9. **Anthropic's server-side refusal fallback is an option, off by
   default, and on in the example.** Rejected: on by default, as
   Anthropic's guidance for application code suggests. It is a beta header,
   and a library that sends a beta header nobody asked for breaks every
   caller the day the beta is renamed.

10. **The scripted model is `llm.Scripted`, not `llm/script`.** Keel puts
    the in-process default in the package (`InMemoryBus`, `MemoryStore`,
    `LogSender`) and the external ones in subpackages.

11. **`Model` has two methods, `Generate` and `Stream`.** Rejected: a
    separate `Streamer`. Every wrapper would then have to test for it and
    silently lose streaming when wrapping a model that had it.

12. **Streaming is a callback.** Rejected: an iterator or a channel. A
    callback returns the finished `Response` the same way `Generate` does,
    and a wrapper forwards it in one line; an iterator needs a place to
    put the final value.

13. **No forced tool choice, and structured output through a schema.**
    Section 3. Rejected: forcing a call to get JSON, which current Claude
    models refuse.

14. **The provider's form of an assistant turn is kept and replayed, and
    the journal's JSON columns are `JSON`.** Rejected: rebuilding the turn
    from text and tool calls, which drops the thinking blocks a later
    request needs; and `JSONB`, which is equally correct for the provider,
    since values are what it compares, but returns something other than
    what was stored, and there is nothing to gain from that.

15. **A run fixes its prompt and tools when it starts.** Rejected: reading
    the registered `Definition` each time. A deploy would then edit the
    conversation of every run in flight, which the provider rejects. Tool
    code still comes from the running build, looked up by name.

16. **Money is an `int64` of millionths of a dollar.** Rejected: `float64`.
    Budgets are compared and summed across thousands of steps.

17. **`llm` ships no prices.** Rejected: a table per provider. It would be
    wrong within months and silently. The example's table says when and
    where it was read.

18. **A run's budget is checked before each step and can be passed by one
    step.** Rejected: refusing by worst case, as `llm.Budgeted` does. The
    engine knows no prices and cannot estimate. The two compose.

19. **The time budget counts time working.** Rejected: wall time. A run
    parked over a weekend for a person would return to find itself over
    budget.

20. **`agent` has a `Store` interface, with `MemoryStore` and `agent/pg`.**
    Rejected: `pgx` in the root package, as `outbox` does. The engine's
    state machine is most of the package, and with a memory store it is
    tested in milliseconds without Docker; `idempotency` and `flags` set
    the same pattern.

21. **`Store` is one interface of nineteen methods.** Rejected: three
    smaller ones. They would always be implemented together and passed
    together, so the split would add names and no seam. `httpapi` declares
    the seven-method interface it actually consumes.

22. **Exactly-once Postgres effects through `Once`.** Rejected: completing
    the journal step inside the tool's own transaction. That is tighter and
    needs the store handed into every tool. A dedupe key in the tool's
    transaction gives the same result with one function.

23. **Tool calls of one reply run in order.** Rejected: in parallel. Order
    makes the journal and the tests deterministic; parallelism across
    documents comes from child runs, which do run at once.

24. **Delegation is a field on `Tool`.** Rejected: a function a tool calls
    to start a child and wait. Waiting must park the run, and a tool
    function blocked on a child holds a goroutine and a lease.

25. **The HTTP subpackage is `agent/httpapi`, read-only without an `Actor`,
    with no route to start a run.** Rejected: `agent/http`, which shadows
    `net/http` in every file that imports it.

26. **The worker is `Engine.Work` in a goroutine.** Rejected: a
    `jobs.Entry` calling `Tick`. `jobs.Runner` logs a line per run, which
    at a one-second interval buries everything else. `Tick` is exported
    for a service that wants it.

27. **`agenttest` is exported.** `mail/testing` is the precedent. The
    store contract suite has to be importable by `agent/pg`.

28. **`anthropic.DefaultModel` is `claude-opus-5-5`; `openai.Options.Model`
    is required.** The first follows Anthropic's own default. The second
    has no value that is right for every server speaking the protocol.

29. **Tool names are limited to 64 characters of `[a-zA-Z0-9_-]`.** The
    stricter of the two providers' limits, so a definition works on both.

30. **Ids are UUIDs made in Go.** Rejected: `gen_random_uuid()` in SQL, as
    `outbox` does. `MemoryStore` needs them too, and `Start` returns the id
    without reading it back.

31. **The example is `examples/agent`, as asked, though the name collides
    with `agent/` for `go build ./examples/agent` at the root.** Rejected:
    renaming it. `go run` and `make build` work; the README says so.

32. **A contracts task lands the exported types first.** Rejected: letting
    each parallel task write the types it needs. `agent/pg`, the engine and
    `httpapi` all compile against `agent.Store` and `agent.Step`; one
    commit of types, transcribed from this document, is what lets the rest
    proceed in separate worktrees.

The contracts task met seven places where this document was silent. Two
were ruled on by the project lead, 35 and 36; the others stand as the task
chose them.

33. **`EstimateInputTokens` sums the bytes and rounds once.** It counts the
    system prompt, each message, each tool definition as its name,
    description and schema, and the output schema as its name, description
    and JSON; divides the total by three, rounding up; and adds eight per
    message. Rejected: rounding each part, which adds up to a token for
    every field and makes the figure depend on how a request is divided.

34. **Ids and names inside a message are not counted.** Tool-call ids and
    names and the call ids on results are short and fixed in number per
    call. Rejected: counting them, for an estimate whose three bytes a
    token and eight tokens a message already leave more room than they
    take.

35. **A message is estimated by the larger of its two forms.** Its text,
    tool arguments and tool results, or the bytes of its `Opaque.Data`.
    Rejected: leaving `Opaque` out, as 4.3 first read, which is low for a
    turn replayed with its thinking blocks and signatures; and counting
    both forms, since a provider is sent one or the other.

36. **A stream that ends in the middle of an event is an error.**
    `sse.Reader.Next` returns an error wrapping `io.ErrUnexpectedEOF` and
    does not return the event. Rejected: returning the partial event, to
    suit a server that closes straight after its last data line. A provider
    cannot tell that from a dropped connection, and a final event cut short
    and read as whole is a wrong reply, where an error is a transport
    failure the caller may retry. An event is under way once any field
    line, or part of one, has been read since the last blank line; a
    stream that ends in or after a comment between events ended cleanly.

37. **An event's `ID` is its own.** Rejected: the standard's rule that the
    last id carries onto the events after it. That rule serves a client
    that reconnects, which this reader does not do, and neither provider
    sends ids.

38. **The reader implements the part of the standard the providers use.**
    Lines end in a line feed, with or without a carriage return before it;
    a bare carriage return is not a line ending; a leading byte order mark
    is not removed; `retry` is ignored. A `data` field with an empty value
    is data, so its event is returned with empty `Data`; an event with no
    `data` field is skipped, and its name does not carry to the next.
    Rejected: the whole standard, which needs a line-ending state machine
    for input neither provider sends.

39. **`MaxEventBytes` measures an event's lines as they arrive.** It is the
    sum of the lengths of the event's field lines, without their line
    endings. Comment lines are not counted and not held, so keep-alive
    comments of any number or length do not reach it. The check is made
    while a line is being read, so a line that never ends is refused at the
    bound. Rejected: checking the assembled event, which allocates first
    and checks after. The error is exported, `ErrEventTooLarge`, since a
    provider has a decision to make about it: a retry meets the same event,
    so it is not a transport failure.

40. **A send on an event stream is bounded, though the stream is not.**
    `NewEventStream` lifts the write deadline so that a stream can outlive
    the server's `WriteTimeout`. Leaving it lifted lets a client that stays
    connected and stops reading hold the handler in `Write` once the socket
    buffers fill, and neither the router's timeout nor the context check can
    interrupt a blocked write. Each send therefore sets a deadline of its
    own, `EventStreamOptions.SendTimeout` (30 seconds by default, none when
    negative), and lifts it again after. Rejected: lifting the deadline
    outright, as section 7 first said; and one deadline for the whole
    stream, which is `WriteTimeout` again. A send that fails ends the stream,
    since the write may have left half an event on the wire. The server's
    `WriteTimeout` therefore no longer applies to a stream once it is open;
    `SendTimeout` is what bounds it. The recorder in `httpx/middleware`
    changed too, to forward only a response's first status, to report flush
    errors, and to take no part in a hijacked connection, and
    `NewEventStream` asks that a flush reach the end of the writer chain,
    which the recorders cannot promise on their own.

41. **The budget is outermost and the meter sits inside it.** `Budgeted`, then
    `Metered`, then `Fallback`. Rejected: the meter outermost, which records a
    call the budget refused as a failed call with no usage, and so counts as a
    call something that never reached a provider.

42. **Only the caller's context ends a `Fallback` chain, and `Retryable` trusts
    the provider's mark.** Rejected: reading `context.DeadlineExceeded` or
    `context.Canceled` in the error chain as the caller's. An `http.Client`'s own
    timeout satisfies `errors.Is(err, context.DeadlineExceeded)` while the
    caller's context is live, so a provider that hangs, which is the case a
    fallback exists for, would end the chain and would not be retried. The
    default test for moving on is now `ErrBudgetExceeded` alone; `Retryable`
    decides by the `*Error` it finds, and a provider returns the context's own
    error and no `*Error` once the caller's context is done.

43. **A refusal that was moved past is billed, and is the reply if no later model
    answers.** With `OnRefusal`, the last refusal comes back with a nil error and
    every refused attempt in `Attempts`, and the failures that followed are
    logged. Rejected: an error, which leaves what was paid for out of the budget
    and the meter, and bills another refusal each time the caller tries the step
    again.

44. **A reply is priced at the model it names, then at the model the request asked
    for, then at the cost that was held for it.** One function does it, for the
    budget and the meter. Rejected: pricing only at the model the reply names. A
    provider commonly answers an alias with a dated id, so a table keyed by alias
    would fail on every call and the budget would refuse far too early.

45. **A stream callback's own error comes back untouched, and a model's nil reply
    with a nil error is an error.** See 4.7. Rejected: letting the retry wrapper
    and the chain wrap the callback's error, which makes the caller's own signal
    to stop read as a model failure; and handling a nil reply differently in each
    wrapper.

46. **A settle that passes its hold is added as it is and logged.** `Budgeted`
    adds a call's real cost and tokens, never clamped to what was held, and logs
    at warning level, through `BudgetOptions.Logger`, a call that settles for more
    than its hold in a dimension that has a limit. Rejected: clamping, which
    forgets real spend and lets the next call through on money already gone; and
    staying silent, which hides that the guarantee in 4.3 did not cover that call.
    The field is new, and its zero value works.

47. **A callback's error after a refusal that was moved past loses the refusal's
    billing, and is logged.** The error comes back with no reply for the usage to
    ride in. Rejected: returning a reply with the error, which breaks the rule that
    a callback's error comes back as the callback returned it.

The memory store task wrote the `Store` contract suite, and so met the
places where the table in 6.2 did not say what a store does. The project
lead confirmed its choices, and the table now states each of them.

48. **The `Store` contract is the table in 6.2 as the suite holds a store to
    it, and `FaultStore` can also fail one named method.** Behaviours were
    chosen where the table was silent and are now written into it;
    `agent/pg` passes the same suite, so they bind both stores. The ones
    that had an alternative: a lease with no owner never holds a run, and
    a claim needs an owner (rejected: comparing owner and epoch and
    nothing else, under which a lease with no owner matches a run nobody
    holds and may write to it); an id that is not a UUID is `ErrNotFound`,
    and so is `Claim` for a `RunID` that names no run (rejected: the
    driver's error for a malformed id, which the HTTP surface would serve
    as a 500, and `ErrNotClaimable` for a run that is not there); a
    waiting run made runnable loses its `Reason` (rejected: leaving it, so
    that a runnable run says it waits on an approval); one call adds one
    to a run's `Rev` however many approvals it touches (rejected: one per
    approval, which makes the number of revisions depend on how a store
    batches its writes); a step that does not exist is `ErrConflict`
    (rejected: `ErrNotFound`, which is for ids, and a `seq` is a
    position); `Changes` returns steps in journal order (rejected: by
    `Rev`, which the stream wants and sorts for itself, while the timeline
    wants the journal). On the test kit: `FailBefore` and `FailAfter` fail
    a named method a given number of times and leave the store working
    (rejected: kills alone, which cannot make a store failure the process
    outlives, the case that ends an execution as failed and has it tried
    again).

    The review of the memory store settled the rest, each so that a test
    passing on one store passes on the other. A claim by `RunID` waits for
    a write in progress and only a claim without one passes over a locked
    run (rejected: `skip locked` for both, as 6.6 first read, which
    refuses a lapsed run because its last holder is still writing). An
    approval's `DecidedAt` is nil exactly while it is pending, so one
    cancelled by its run ending records when, like one that lapsed, and
    neither records a decider (rejected: no time on a cancelled approval,
    which leaves two states to tell apart by status alone).
    `RequestApproval` leaves the step's `Decision` and `Rule` when the
    request carries no decision (rejected: overwriting them, which wipes
    the guard's answer from an interrupted step). `RequestCancel` of a run
    already marked changes nothing (rejected: recording the later request,
    which rewrites who cancelled a run and moves `Rev` for no change of
    state). A store refuses what a database could not keep, or would keep
    as something else: an id that is not a UUID in canonical form, a
    parent that does not exist, a step status that is not one, a lease of
    no length (rejected: leaving each to the database's constraints, which
    `MemoryStore` does not have, so that a test passed on it and failed on
    Postgres). `Metadata` and `Action.Attrs` read back as JSON gives them,
    numbers as `float64` and none as an empty map (rejected: `MemoryStore`
    keeping the Go values it was given, which no database can).
    `Run.Error` is the last failure and stays until `Finish` (rejected:
    clearing it when a later step completes, which loses why a run that
    recovered had been retried).

    A second review fixed the order of the checks, the same in every
    method: the request's own arguments, then whether the run exists,
    then the lease, then the run's state (rejected: the fence before
    everything, as the table first read, which a Postgres store cannot do
    for an argument it refuses in Go before it opens a transaction, so
    that the two stores would answer a call wrong in two ways with
    different errors). It also made the form of an id part of the
    contract: a store knows an id by one string, the canonical spelling,
    refuses another spelling of a UUID where an id is to be kept, and
    does not find a run by one (rejected: leaving the form to
    `MemoryStore` alone, where a Postgres store would find a run by the
    upper-case spelling of its id and keep a new one under a string it
    was not given). `Metadata` goes through JSON as `Action.Attrs` does.
    What a NUL character inside a string of either does is left to the
    Postgres store to settle with its column types in front of it.

49. **`Validate` refuses a rule that matches nothing, or everything, by
    mistake.** A condition with no attribute; an `eq` or `ne` with no value,
    or with a list or object; a number that is not finite; an `in` with an
    empty list or an element that is not a number, a string or a boolean; and
    an empty kind. Rejected: leaving them to `Decide`, where each reads as
    "this rule does not apply". A person who wrote a block rule and left out
    the attribute, or whose `value` was dropped when a policy was
    reserialised, has a rule that looks like protection and protects
    nothing, and nothing says so; the fault is also the sort a reviewer reads
    straight past. Refusing at load time costs a startup error that names the
    rule and the condition. Not refused: an empty `Kinds`, `Target` or
    `Attrs`, which the design defines as "every", so a rule with no `when`
    applies to every action; and two conditions that contradict one another,
    which is analysis this package does not do. A bare `*` target, listed
    here as not refused when this decision was first written, is refused:
    see decision 52.

50. **What cannot be evaluated counts toward the stricter outcome.** A
    condition holds, does not hold, or cannot be told; a rule with nothing
    that fails and something that cannot be told matches if it asks or
    blocks, and does not if it allows; the decision names the attribute in
    `Decision.Uncertain`. Rejected: reading a present attribute of the wrong
    type as "does not hold", which the first version did. Attributes come
    from tool input a model wrote, so under "allow by kind, and a payment
    above 200 asks", an amount written as `"1250"`, a one-element list, a
    pointer or NaN was allowed, the cheapest way round any limit. Also
    rejected: reading `"1250"` as a number, which decides on a guess about
    what the model meant and leaves `"abc"` to be allowed; and returning an
    error, which stops the run for what a person could settle with one
    answer. Asking is the outcome that leaves the decision to a person and
    leaves the reason on the record. `in` follows from the same reading: it
    is `eq` on each element joined by "or", and unknown-or-false is unknown,
    so it does not hold only when every element could be compared and none
    equalled; `in [22, "ssh"]` against `"22"` cannot be told, and a block
    rule blocks it, as `in [22]` and `eq 22` do. Rejected: reading it as not
    held once any one element was comparable, which let the same attribute be
    blocked by a list of one type and allowed by a list of two. The same reading settles a rule that
    cannot be evaluated at all: "does not match" is safe for an allow rule and
    for no other.

51. **Numbers are compared exactly, and a float whose integer value is
    rounded is told under both of its readings.** `Parse` keeps every number
    as written. Rejected: `float64` for everything, which turns
    `9007199254740993` into `9007199254740992` and a threshold into a
    different threshold; the exact binary value of every float, which leaves a
    `float64` 0.3 below a threshold of 0.3; and, for a float with an integer
    value, either single reading. As the exact integer, a `float64` 1e23
    (99999999999999991611392) is below a threshold written 1e23; as its
    shortest decimal, a `float64` 2^70 (1180591620717411300000) is below a
    threshold of 1180591620717411303000 that it is above. Each is a wrong answer
    given with certainty, and the package's rule is that what cannot be told
    counts toward the stricter outcome. So both readings are compared; one
    outcome is the outcome; two make the condition uncertain. Below 2^53 the
    readings are one number and nothing changes. Text is read to 4096 bytes
    and an exponent of 4096, and past that cannot be told, because the value is
    a model's and an exponent of a million is an allocation of that size.

52. **`Parse` refuses what a reader and the decoder would see differently.** A
    repeated key at any depth, compared as the decoder compares keys, without
    regard to case, because the later key wins or merges and the file read
    is not the policy loaded; a document that is not an object, because
    `null` would load as the empty policy, which blocks everything or
    allows by default; and a rule named `RuleDefault`, which would make the
    record ambiguous. `Validate` also refuses a bare `*` target, which
    reads as every target and matches no target with a slash in it, and
    `exists false` with another condition on the same attribute, which an
    absent attribute can never meet. Not refused: conditions that contradict
    one another in general, which needs analysis this package does not do.

53. **A `Decider` is immutable by copying, and its default log is bounded.**
    Every list and nested value of its policy is copied in and out, and a
    `Recorder` is handed a copy. Rejected: sharing a condition's value, which
    let a caller change a live rule through an `in` list and race with
    `Decide`. The copy is bounded in work and not only in depth: a value that
    shares a child twice at every level doubles at each, so more than 10000
    values to copy is refused, by `NewDecider` for a policy and by `Decide`
    with an error and no decision for an action, as when a record cannot be
    written. The in-memory log keeps the most recent 1000 decisions,
    because with the zero `Options` it cannot be reached and growing with
    every decision is a leak. Targets are matched as written, with no
    normalisation, so whatever builds an `Action` puts them in one canonical
    spelling: normalising inside `policy` would pick one spelling for every
    caller's idea of what a target is.

The first review of the two providers settled five more, each for both of
them. These are written from the OpenAI provider's side.

54. **A stream is bounded by its context, an idle limit, an event size and
    the reply's size, and by no whole-request timeout.** Rejected: the
    client's `Timeout`, which covers reading the body and so cuts a long
    reply that is going well; and capping a stream at the body bound as a
    whole, for the same reason. What a stream may not do is stall (the idle
    limit on bytes from the server, default two minutes, `IdleTimeout` in the
    options, negative for none; a keep-alive comment is a byte, so a server
    that sends only those is bounded by the context, and a callback's time is
    not counted) or grow without limit (16 MiB an event, 32 MiB of what the
    reply keeps, ids and names and a fixed charge per call included). A size
    bound that is passed is a plain error and not retryable, since a retry
    meets the same size, where a stall is a transport failure that is.

55. **A response that is not what a call asked for is an error, and the
    stream that delivered nothing is a retryable one.** A 200 to a streaming
    request that is not an event stream, and an error object inside a 200 body
    of any call, embeddings included, are `*llm.Error`s; the first is not
    retryable, the second is when its type or code says it passes. A stream
    that ends cleanly with no chunk that has an id or a choice is an
    incomplete stream, like a cut one, and retryable; a stream whose server
    said it finished, with a finish reason, and said no more is an empty
    reply. Rejected: reading a stream of nothing but `[DONE]` as an empty
    reply, which is what a proxy that dropped the upstream sends.

56. **The caller's context ending is the context's error, and the rest is the
    provider's.** When a call fails and the caller's context is done, the
    provider returns that context's error and it is not retryable. When the
    context is live, a transport failure, a stream that went quiet and the
    client's own timeout are `*llm.Error`s marked retryable. Rejected: an
    `*llm.Error` for a cancellation, which a wrapper could mistake for the
    provider's failure and a retry loop would then repeat.

57. **The default client follows no redirects, and no client sends a
    credential to an origin it was not given.** A 3xx is an `*llm.Error`, not
    retryable, and so is a redirect that a supplied client refuses. A client
    supplied in the options keeps its own policy, and the package withholds the
    key and the caller's headers from any origin other than the configured
    one's, since `net/http` forwards every header but `Authorization`, and drops
    that one only when the host name changes, not the scheme or the port. The
    request body, which holds the prompt, goes wherever a followed 307 or 308
    points: that is the supplied client's policy to set. Rejected: following
    redirects by default, which forwards the caller's headers and, for a 307 or
    308, the whole prompt, and which turns the POST of a 301 into a GET.

58. **After a stream ends cleanly its body is drained a little, so the
    connection is reused.** At most 64 KiB and 100 milliseconds. A stream is
    read to `[DONE]`, which comes before the end of the chunked body, and
    `net/http` returns a connection to the pool only for a body read to its
    end. Rejected: draining without a bound, which makes a server that holds
    its stream open after `[DONE]` hold the call; and draining after a failure
    or an error from the callback, where what is left is more than the
    connection is worth.

59. **`AgentModel` prices a reply as the budget and meter do, always bounds it,
    and lets a refusal act on nothing; `AgentGuard` says what a policy could not
    evaluate.** Section 8 first
    priced a reply with `Prices.CostOf` and made a model the table lacks a
    permanent error. Now `Prices.CostFor`, which is decision 44's one function
    under a name, prices each attempt at the model it names, then at the model
    the request asked for; only a reply neither name prices fails the run,
    permanently, and is never a reply that costs nothing. Rejected: `CostOf`,
    under which a table keyed by alias fails every run whose provider answers
    with a dated id; and a second pricing rule in `app`. The tokens a reply
    reports are summed over its attempts as the cost is, so that a run's token
    limit sees what its cost limit sees. Every request the adapter sends sets
    `MaxTokens`, from the run's own or `DefaultMaxTokens` (16000, the budget's
    number), because a budget's hold is an upper bound only when the request
    bounds the reply. Rejected: leaving it to the provider's default, which the
    hold would have to assume was no larger. Errors are classified as
    `llm.Retryable` classifies them, and the caller's cancellation is read from
    the context, as decision 42 reads it. A retryable `*llm.Error` and a plain
    error are returned as they are, the caller's cancellation neither retryable
    nor permanent. Rejected: `ErrPermanent` on every provider error, which
    fails a run for a rate limit, and on a context error, which fails every run
    a worker is stopped under. `agent.Decision` carries only `Effect` and
    `Rule`, so a policy decision that rests on something it could not evaluate
    comes back with the rule named as `<rule> (could not evaluate: <names>)`,
    where the timeline and the approval read it, and the structured
    `Uncertain` stays in the policy's own record. Rejected: dropping it, which
    leaves a person asked about "Large payments ask" with no reason when the
    amount arrived as the string "1250"; and a field on `agent.Decision`,
    which changes the journal and every store. `AgentGuard` hands `Kind`,
    `Target` and `Attrs` to `policy` as they are, as decision 53 asks of
    whatever builds an `Action`. The review of the adapters added four rulings.
    A refusal reaches the engine with the role and nothing else: the first
    wording, that the adapter passes what it is given, let a refusal a provider
    returned with tool calls in it reach an engine that journals calls before it
    reads the stop, and the opaque form would have replayed them; the narrowest
    place to make that impossible is here, beside the planner reading the stop
    first. The refusal's reason, which `agent.Response` has no place for, is
    logged through `AgentModelOptions.Logger`, and never the prompt or the
    text. `AgentModelOptions.Model` is `BudgetOptions.Model`'s counterpart, so
    that a request naming no model is priced where the budget prices it and
    not as `""`, which a table keyed by alias cannot price. Nil `Prices` prices
    nothing and any other table is set, so that a table that failed to load
    fails the run instead of switching its cost limit off. Rejected: reading an
    empty table as no table. And the caller's context decides before a budget
    refusal, an `ErrNoPrice` and an unpriceable reply as it does before a
    provider error.

60. **The routes that change something refuse a cross-origin browser request, and
    a cursor is input.** Approve, decline and cancel are how people govern a
    run, and each accepts a POST with no body of any content type, which a form
    on another site can make a browser send with the user's cookie. They are
    therefore guarded in the package, with the standard library's
    `http.CrossOriginProtection` (Go 1.25), and `Options.CrossOrigin` lets a
    service trust the origin of its own front end. The guard refuses by
    `Sec-Fetch-Site` or by `Origin`, answers the generic 403 and runs before
    `Actor`. Rejected: leaving it to the mount, which `Routes` documents as
    guarding operator traffic and which cannot know that these three routes
    accept a request that carries nothing to tell a form from a client;
    requiring a content type, which a form can send as `text/plain` and which
    would refuse a client that sends none; and `CrossOriginProtection.Handler`,
    whose refusal is plain text. The package calls `Check` and answers with its
    own generic 403, so a deny handler set on a supplied protection is not
    used. The guard is not authentication, and does not cover the reads or
    the stream. A cursor is the client's to send, so it is decoded strictly
    before the engine is asked: a token that does not decode, a time that does
    not parse, or an id that is not a UUID in the canonical form (`uuid.Parse`
    reads upper case, braces, a URN and no hyphens, and a store would be given
    a name it does not know) is 400. Rejected: handing the id to the store to
    judge as the ids in a path are, which for a cursor has no 404 to answer
    with and, against a `uuid` column, a 500.

61. **The decision log is read by position, a record that can never be stored
    is told from a store that is down, and a policy that could not be recorded
    is refused at load.** `policy/pg` pages back through the log with a keyset
    cursor, `Cursor{At, ID}` on `Filter.Before`, the position of the last record
    of the page before, in the order the query already had and the index already
    served; each listed record carries its `ID` (a new field of `policy.Record`,
    zero where no store numbered it). Rejected: an offset, which skips or repeats
    records as new ones arrive and costs a scan of everything it skips; a cursor
    of the time alone, which splits a group of records that share one between
    pages and loses or repeats some of them; and returning a type of the store's
    own from `List`, which would have changed the signature that other packages
    build against. The limit's ceiling of 1000 stays, and the cursor is what
    makes it a page size and not a limit on how far back the log can be read: an
    audit log whose older entries cannot be reached is not one. `ErrUnrecordable`
    is one sentinel in `policy`, wrapped by whichever recorder refuses a record
    for good; the server's own error stays in the chain beside it. Rejected: a
    sentinel for the failures of the moment, since what is not known to be final
    must be treated as retryable, and a type that carries a reason, which every
    caller would have to switch on. `Validate` refuses a NUL or bytes that are
    not UTF-8 in a rule's name, an attribute name and the version, a name or
    version of more than 256 bytes, and a NUL in a kind and a target pattern,
    because the alternative is a rule that loads and then fails, closed, on every
    record under it, for ever; the refusal is at load, where it costs one startup
    error that names the rule. An empty `json.Number` in an attribute is refused as
    unrecordable, not stored as the 0 `encoding/json` writes for it: a record is
    what was decided on, and nobody sent a zero; it is looked for in what decoded
    JSON holds (a map, a list, a number) and a struct or a typed list is left to
    `encoding/json`, whose rules for a struct a copy would only risk disagreeing
    with. A store built over a transaction runs its insert in a savepoint, since a
    refused statement aborts a transaction that the store shares with its caller,
    and an error that says the record is final does not help a caller whose
    transaction can no longer run; rejected: refusing everything in Go, which
    cannot be done for what only the server knows (a number past `numeric`, a
    value nested past its stack).
    The rule index is `(rule, decided_at DESC, id DESC)`, the order of the
    listing, since on `(rule, id DESC)` a filter on a rare rule sorted its rows
    and a cursor on it filtered them after the fact. Invalid UTF-8 in an attribute
    is replaced by U+FFFD, as `encoding/json` does, and the log says so;
    rejected: refusing it, which would fail the record of an action the policy
    decided on without a word.

62. **A journal the planner cannot read gives the run up, a reply the model
    did not finish is final, and a request to cancel has its own event.**
    `next` has a tenth action, `actYield`, for a journal that does not add up:
    a step whose kind or status this build does not know, a completed model
    step with no message, a `Stop` it does not know, a waiting step with no
    approval for its current attempt or whose child was not passed in, an
    interrupted at-most-once step already asked about. The execution ends as a
    failed attempt with what was found as the error, and the run stays
    claimable (rejected: finishing the run as failed, `ReasonError`, which
    cannot be undone, for what may be a newer build's writing during a deploy
    or the executor's own mistake; and parking it, which leaves a run nothing
    will wake). A refusal, a spent budget and cancellation still end a run,
    and so does a reply cut at its token limit or out of context window,
    whatever calls came with it (rejected: judging and running those calls as
    a turn, when the last of them may be cut short and still parse, so that a
    guard keyed on an argument that is missing would let it through). A
    waiting step is decided by the approval for its current attempt and no
    other (rejected: the approval with the highest attempt, which, read from
    a list that was cut short, is an older yes that runs an interrupted call
    nobody approved). An interrupted at-most-once step is always asked about
    (the clause "and has no approved approval for its current attempts" could
    not fire against a store that keeps the contract, and is gone). A spent
    budget stops work and asking, and lets what does no work finish first
    (rejected: failing at the first working step, which left a decline
    unrecorded and an ended child's usage out of the failed run's totals).
    `Cancel` publishes `EventRunCancelRequested`, and `EventRunCancelled` is
    kept for the run that has ended cancelled (rejected: `EventRunCancelled`
    for both, which makes one type mean two things, and nothing at all, which
    leaves a subscriber no hint that a run is about to stop).

**A store keeps every string it is handed or refuses it for what it is, and
    the two stores agree where the suite used to be silent.** A text column
    holds neither a NUL nor a byte that is not UTF-8, and a `jsonb` column no
    NUL, so the DDL as first written let a tool's result, a model's final
    answer or an error's text fail a journal write: the call was made again
    until the run failed. A string a store only records is now kept with
    U+FFFD in place of each such character, by both stores, and a string a
    store compares (an agent's name, a start key, an owner, a `Once` key) is
    refused before the store is touched (rejected: `bytea` or a JSON string
    for the free-text columns, which keeps one character for a model at the
    cost of every person who reads the table; refusing every such string,
    which is the wedge with a better message; and replacing in a name too,
    under which two keys could become one). Raw JSON is kept byte for byte
    or refused: `agent/pg` writes the object around each raw value itself,
    because `encoding/json` respaces and re-escapes one, and both stores
    refuse a raw value that is not JSON or not UTF-8 (rejected: leaving it
    to the column, which refuses it after the lease has been checked and
    with the database's error).

    The suite now pins what it left open, so that a test passing on one
    store passes on the other. Runs created at one instant are claimed in
    the order of their ids (rejected: the order they arrived in, which
    `MemoryStore` had and a table does not keep). A list cursor is an
    argument: one whose id is not a canonical UUID is refused, a time with
    no id included, and the zero cursor lists from the start (rejected:
    comparing the id as whatever string it is, which a `uuid` column cannot
    do, so that a malformed cursor from a client reached the database). A
    cause that is none of the three is refused (rejected: leaving it to the
    table's CHECK, which fires after the lease). A run's `Depth` must be its
    parent's plus one, or zero with no parent (rejected: taking the caller's
    word, when the order rows are locked in is taken from it and a wrong
    depth is a deadlock). A claim by id for another agent's run is
    `ErrNotClaimable`, as it already was.

    `Purge` locks what it will remove in the order every writer takes rows,
    children first, removes the `Once` keys recorded under those runs' ids,
    and is tried once more if Postgres ends it for a deadlock (rejected:
    leaving the retry to the caller, who cannot tell a deadlock from any
    other failure without knowing the driver; and a delete that starts at
    the root, whose cascade takes a child's row after its parent's). A
    `Once` key that is not a step's, one a tool made up, is not removed:
    the table cannot tell whose it is.
