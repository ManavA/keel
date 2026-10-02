package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/policy"
)

// defaultMaxTokens is the reply bound llm.Budgeted assumes for a request that
// sets none. A request the adapter sends always sets one, so that the hold a
// budget takes for the call is a bound the call keeps to.
const defaultMaxTokens = 16000

// answerSchemaName labels the schema of a run's final answer for providers
// that require a name.
const answerSchemaName = "answer"

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
	// real upper bound on the call. It goes to every model the adapter serves,
	// so a model with a lower output cap needs it set lower.
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

// AgentModel adapts an llm.Model to agent.Model. It uses Generate only.
//
// Compose the llm wrappers before adapting: the budget outermost, with the
// meter inside it, so the meter records only the calls the budget let through,
// then any fallback chain, and one retrying wrapper for each provider. A call
// the budget refuses comes back as an error that wraps agent.ErrPermanent, so
// the run fails at once instead of trying again.
//
// An assistant turn's tool calls, results and opaque provider form are copied
// across field for field and byte for byte: the adapter never decodes or
// re-encodes JSON, so a turn goes back to its provider exactly as the provider
// wrote it. Each reply's cost is Prices.CostFor with the model sent as the
// model asked for, and is zero when Prices is nil.
//
// A refused reply reaches the engine with nothing to act on: its Message has
// the role and nothing else, no text, no tool calls and no opaque form, which
// would replay the calls dropped.
//
// What is not carried: the run id and the agent name are not sent to the model,
// and the provider's reply id and the refusal's details are logged, not
// returned. DefaultMaxTokens bounds the reply of every model the adapter
// serves, so a model with a lower output cap needs it set lower.
//
// Errors are classified as llm.Retryable classifies them. The caller's own
// cancellation or deadline, read from ctx, decides before anything else: an
// error met while ctx is done is returned as it is. Otherwise an
// ErrBudgetExceeded, an ErrNoPrice, a reply that cannot be priced and an
// *llm.Error that is not retryable are wrapped in agent.ErrPermanent and still
// unwrap to themselves. Every other error, a retryable *llm.Error included, is
// returned as it is, and the engine tries again later.
func AgentModel(m llm.Model, opts AgentModelOptions) agent.Model {
	if opts.DefaultMaxTokens <= 0 {
		opts.DefaultMaxTokens = defaultMaxTokens
	}
	opts.Prices = maps.Clone(opts.Prices)
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &agentModel{model: m, opts: opts, logger: logger}
}

type agentModel struct {
	model  llm.Model
	opts   AgentModelOptions
	logger *slog.Logger
}

var _ agent.Model = (*agentModel)(nil)

// Generate implements agent.Model.
func (a *agentModel) Generate(ctx context.Context, req agent.Request) (agent.Response, error) {
	if a.model == nil {
		return agent.Response{}, fmt.Errorf("%w: app: agent model: no llm.Model to ask", agent.ErrPermanent)
	}
	sent := a.request(req)
	resp, err := a.model.Generate(ctx, sent)
	if err != nil {
		return agent.Response{}, classify(ctx, err)
	}
	if resp == nil {
		return agent.Response{}, errNoReply
	}
	return a.response(ctx, resp, sent.Model)
}

// errNoReply is what a model that returns neither a reply nor an error has
// done. It is a bug in the model, and the next call may do better.
var errNoReply = errors.New("app: agent model: the model returned neither a reply nor an error")

// request is the llm.Request for req. Nothing in it shares memory with req.
func (a *agentModel) request(req agent.Request) llm.Request {
	out := llm.Request{
		Model:     cmp.Or(req.Model, a.opts.Model),
		System:    req.System,
		Messages:  mapSlice(req.Messages, llmMessage),
		Tools:     mapSlice(req.Tools, a.llmTool),
		MaxTokens: req.MaxTokens,
		Effort:    a.opts.Effort,
	}
	if len(req.Output) > 0 {
		out.Output = &llm.Schema{Name: answerSchemaName, JSON: cloneRaw(req.Output)}
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = a.opts.DefaultMaxTokens
	}
	return out
}

func (a *agentModel) llmTool(t agent.ToolSpec) llm.Tool {
	return llm.Tool{
		Name:        t.Name,
		Description: t.Description,
		Schema:      cloneRaw(t.Schema),
		Strict:      a.opts.StrictTools,
	}
}

func llmMessage(m agent.Message) llm.Message {
	out := llm.Message{
		Role: llm.Role(m.Role),
		Text: m.Text,
		ToolCalls: mapSlice(m.Calls, func(c agent.Call) llm.ToolCall {
			return llm.ToolCall{ID: c.ID, Name: c.Name, Input: cloneRaw(c.Input), Malformed: c.Malformed}
		}),
		ToolResults: mapSlice(m.Results, func(r agent.Result) llm.ToolResult {
			return llm.ToolResult{CallID: r.CallID, Content: r.Content, IsError: r.IsError}
		}),
	}
	if m.Opaque != nil {
		out.Opaque = &llm.Opaque{Provider: m.Opaque.Provider, Data: cloneRaw(m.Opaque.Data)}
	}
	return out
}

func agentMessage(m llm.Message) agent.Message {
	out := agent.Message{
		Role: agent.Role(m.Role),
		Text: m.Text,
		Calls: mapSlice(m.ToolCalls, func(c llm.ToolCall) agent.Call {
			return agent.Call{ID: c.ID, Name: c.Name, Input: cloneRaw(c.Input), Malformed: c.Malformed}
		}),
		Results: mapSlice(m.ToolResults, func(r llm.ToolResult) agent.Result {
			return agent.Result{CallID: r.CallID, Content: r.Content, IsError: r.IsError}
		}),
	}
	if m.Opaque != nil {
		out.Opaque = &agent.Opaque{Provider: m.Opaque.Provider, Data: cloneRaw(m.Opaque.Data)}
	}
	return out
}

// response is the agent.Response for resp. asked is the model the request was
// sent to, which prices a reply whose own model the table does not list.
func (a *agentModel) response(ctx context.Context, resp *llm.Response, asked string) (agent.Response, error) {
	stop, ok := agentStop(resp.Stop)
	if !ok {
		return agent.Response{}, permanentUnlessDone(ctx,
			fmt.Errorf("the reply stopped for %q, which agent has no name for", resp.Stop))
	}
	// What the run's limits count is what was billed: every attempt, as the
	// cost is, and not only the last model's share.
	attempts := resp.Attempts
	if len(attempts) == 0 {
		attempts = []llm.Attempt{{Model: resp.Model, Usage: resp.Usage}}
	}
	var billed llm.Usage
	for _, at := range attempts {
		billed = billed.Add(at.Usage)
	}
	usage := agent.Usage{
		InputTokens:  billed.InputTokens + billed.CacheReadTokens + billed.CacheWriteTokens,
		OutputTokens: billed.OutputTokens,
	}
	msg := agentMessage(resp.Message)
	if stop == agent.StopRefusal {
		a.logRefusal(ctx, resp)
		msg = agent.Message{Role: msg.Role}
	}
	if a.opts.Prices != nil {
		cost, err := a.opts.Prices.CostFor(resp, asked)
		if err != nil {
			return agent.Response{}, permanentUnlessDone(ctx, err)
		}
		usage.CostMicros = cost
	}
	return agent.Response{
		Message: msg,
		Stop:    stop,
		Usage:   usage,
		Model:   resp.Model,
	}, nil
}

// logRefusal says why a model refused, which agent.Response has no place for.
// The prompt and the reply's text stay out of the log.
func (a *agentModel) logRefusal(ctx context.Context, resp *llm.Response) {
	attrs := []any{"id", resp.ID, "model", resp.Model}
	if r := resp.Refusal; r != nil {
		if r.Category != "" {
			attrs = append(attrs, "category", r.Category)
		}
		if r.Explanation != "" {
			attrs = append(attrs, "explanation", r.Explanation)
		}
	}
	a.logger.WarnContext(ctx, "app: agent model: the model refused the request", attrs...)
}

// agentStop is the agent.Stop for s. A reason agent has no name for is not
// guessed at: the planner reads a reply by its stop.
func agentStop(s llm.StopReason) (agent.Stop, bool) {
	switch s {
	case llm.StopEnd, llm.StopSequence:
		return agent.StopEnd, true
	case llm.StopToolUse:
		return agent.StopToolUse, true
	case llm.StopMaxTokens:
		return agent.StopMaxTokens, true
	case llm.StopRefusal:
		return agent.StopRefusal, true
	case llm.StopPause:
		return agent.StopPause, true
	case llm.StopContextWindow:
		return agent.StopContextWindow, true
	default:
		return "", false
	}
}

// classify says what the engine should make of a model's error: permanent when
// no retry will fix it, and otherwise as it is. A provider's own mark is
// trusted before a context error in the chain, as llm.Retryable trusts it.
func classify(ctx context.Context, err error) error {
	if errors.Is(err, llm.ErrBudgetExceeded) || errors.Is(err, llm.ErrNoPrice) {
		return permanentUnlessDone(ctx, err)
	}
	var provider *llm.Error
	if errors.As(err, &provider) && !llm.Retryable(err) {
		return permanentUnlessDone(ctx, err)
	}
	return err
}

// permanentUnlessDone marks err as one no retry will fix, and it still unwraps
// to err. The caller's own context ending decides before that: a worker that is
// shutting down must not fail a run for good, and an error that is real comes
// back, and is marked, on the retry.
func permanentUnlessDone(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("app: agent model: %w: %w", agent.ErrPermanent, err)
}

// AgentGuard adapts a policy.Decider to agent.Guard.
//
// The action's Kind, Target and Attrs are handed to the Decider unchanged:
// nothing is trimmed, lower-cased or cleaned, and no attribute is converted to
// another type. policy matches names and strings exactly as written, and reads
// an attribute of a type a rule cannot compare as a reason to ask or block, so
// whatever builds the action must use one spelling for each name and decode
// numbers with json.Decoder.UseNumber.
//
// The Effect and Rule of the policy's decision come back. When the decision
// rests on something that could not be evaluated, the policy's own record
// names it in Decision.Uncertain, and agent.Decision has no field for that, so
// the rule comes back as
//
//	<rule> (could not evaluate: <names joined by ", ">)
//
// where the run's timeline and the approval a person is shown will say why a
// rule whose condition looks unmet decided. A decision that could not be
// recorded is an error and no decision, never an Allow.
func AgentGuard(d *policy.Decider) agent.Guard { return agentGuard{decider: d} }

type agentGuard struct{ decider *policy.Decider }

var _ agent.Guard = agentGuard{}

// Decide implements agent.Guard.
func (g agentGuard) Decide(ctx context.Context, a agent.Action) (agent.Decision, error) {
	if g.decider == nil {
		return agent.Decision{}, errors.New("app: agent guard: no policy.Decider to ask")
	}
	d, err := g.decider.Decide(ctx, policy.Action{Kind: a.Kind, Target: a.Target, Attrs: a.Attrs})
	if err != nil {
		return agent.Decision{}, fmt.Errorf("app: agent guard: %w", err)
	}
	rule := d.Rule
	if len(d.Uncertain) > 0 {
		rule += " (could not evaluate: " + strings.Join(d.Uncertain, ", ") + ")"
	}
	return agent.Decision{Effect: agent.Effect(d.Effect), Rule: rule}, nil
}

// mapSlice maps each element of in, keeping a nil slice nil and an empty one
// empty.
func mapSlice[T, U any](in []T, f func(T) U) []U {
	if in == nil {
		return nil
	}
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

// cloneRaw copies b, keeping nil nil and empty empty.
func cloneRaw(b json.RawMessage) json.RawMessage { return bytes.Clone(b) }
