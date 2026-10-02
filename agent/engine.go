package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

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
	// HeartbeatInterval defaults to a third of LeaseTTL.
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

// The defaults Options documents, and those of Limits.
const (
	defaultLeaseTTL     = 30 * time.Second
	defaultPollInterval = time.Second
	defaultConcurrency  = 4
	defaultMaxFailures  = 5
	defaultRetryBase    = time.Second
	defaultRetryMax     = time.Minute
	defaultDrainTimeout = 10 * time.Second
	defaultMaxDepth     = 3

	defaultMaxDuration   = 15 * time.Minute
	defaultMaxModelCalls = 50
	// noLimit is how a filled Limits writes a limit there is none of.
	noLimit = -1
)

// withDefaults returns opts with every default applied. A field whose
// default is a number or a length of time takes it when it is zero or less.
// ApprovalTTL has no default: zero or less never lapses, and is left zero.
func withDefaults(opts Options) Options {
	if opts.Store == nil {
		opts.Store = NewMemoryStore()
	}
	if opts.Clock == nil {
		opts.Clock = systemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.NewID == nil {
		opts.NewID = uuid.NewString
	}
	if opts.WorkerID == "" {
		opts.WorkerID = newWorkerID()
	}
	if opts.LeaseTTL <= 0 {
		opts.LeaseTTL = defaultLeaseTTL
	}
	if opts.HeartbeatInterval <= 0 {
		// Never zero, which no ticker takes: a lease too short to have a
		// third is refused by New for the heartbeat it gets here.
		opts.HeartbeatInterval = max(opts.LeaseTTL/3, 1)
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	if opts.MaxFailures <= 0 {
		opts.MaxFailures = defaultMaxFailures
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = defaultRetryBase
	}
	if opts.RetryMax <= 0 {
		opts.RetryMax = defaultRetryMax
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = defaultDrainTimeout
	}
	if opts.ApprovalTTL < 0 {
		opts.ApprovalTTL = 0
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = defaultMaxDepth
	}
	return opts
}

// systemClock reads the machine's clock, in UTC and to the microsecond. That
// is what a timestamptz column keeps, so a time the engine writes reads back
// from either store as it was written.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// newWorkerID names this process: its host, its process id, and a random
// suffix, since two engines in one process are two workers.
func newWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return host + "-" + strconv.Itoa(os.Getpid()) + "-" + uuid.NewString()[:8]
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
type Engine struct {
	// opts is the Options with every default applied, so nothing that reads
	// a field of it checks for zero again. Guard and Events may still be
	// nil, and ApprovalTTL zero: for those, nil and zero are the meaning.
	opts Options
	// store, clock and log are opts.Store, opts.Clock and opts.Logger.
	store Store
	clock Clock
	log   *slog.Logger

	// mu guards defs, the registered agents by name.
	mu   sync.RWMutex
	defs map[string]Definition
}

// New builds an Engine. It returns an error when Model is nil, and when
// HeartbeatInterval is not below LeaseTTL once both have their defaults: a
// lease that is not extended before it lapses is taken over while its
// holder still works.
func New(opts Options) (*Engine, error) {
	if opts.Model == nil {
		return nil, errors.New("agent: Options.Model is required")
	}
	opts = withDefaults(opts)
	if opts.HeartbeatInterval >= opts.LeaseTTL {
		return nil, fmt.Errorf("agent: Options.HeartbeatInterval is %s, which is not below Options.LeaseTTL of %s",
			opts.HeartbeatInterval, opts.LeaseTTL)
	}
	return &Engine{
		opts:  opts,
		store: opts.Store,
		clock: opts.Clock,
		log:   opts.Logger,
		defs:  map[string]Definition{},
	}, nil
}

// toolNamePattern is what a tool's name must match. It is the stricter of
// the two providers' limits, so a definition works on both.
const toolNamePattern = `^[a-zA-Z0-9_-]{1,64}$`

var toolNameRE = regexp.MustCompile(toolNamePattern)

// Register adds an agent. It returns an error for a Definition that does
// not validate or a name already registered.
func (e *Engine) Register(def Definition) error {
	if err := validateDefinition(def); err != nil {
		return fmt.Errorf("agent: register %q: %w", def.Name, err)
	}
	def = cloneDefinition(def)

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.defs[def.Name]; ok {
		return fmt.Errorf("agent: register %q: name is already registered", def.Name)
	}
	e.defs[def.Name] = def
	return nil
}

// validateDefinition reports what is wrong with def by itself. Whether a
// tool's Delegate is registered is not checked: agents may be registered in
// any order, and a call to one that never was is answered with an error
// result when it is made.
func validateDefinition(def Definition) error {
	if def.Name == "" {
		return errors.New("name is empty")
	}
	seen := make(map[string]bool, len(def.Tools))
	for _, tool := range def.Tools {
		switch {
		case !toolNameRE.MatchString(tool.Name):
			return fmt.Errorf("tool %q: name does not match %s", tool.Name, toolNamePattern)
		case seen[tool.Name]:
			return fmt.Errorf("tool %q: name appears twice", tool.Name)
		case tool.Run != nil && tool.Delegate != "":
			return fmt.Errorf("tool %q: has both Run and Delegate", tool.Name)
		case tool.Run == nil && tool.Delegate == "":
			return fmt.Errorf("tool %q: has neither Run nor Delegate", tool.Name)
		case len(tool.Schema) > 0 && !json.Valid(tool.Schema):
			return fmt.Errorf("tool %q: schema is not valid JSON", tool.Name)
		}
		seen[tool.Name] = true
	}
	if len(def.Output) > 0 && !json.Valid(def.Output) {
		return errors.New("output schema is not valid JSON")
	}
	return nil
}

// cloneDefinition copies what def holds by reference, so a registered agent
// does not change when its caller's slices do.
func cloneDefinition(def Definition) Definition {
	def.Tools = slices.Clone(def.Tools)
	for i := range def.Tools {
		def.Tools[i].Schema = cloneRaw(def.Tools[i].Schema)
	}
	def.Output = cloneRaw(def.Output)
	return def
}

// cloneRaw copies b. Empty comes back nil: an empty raw message cannot be
// marshalled, and nil is how the snapshot says there is none.
func cloneRaw(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return slices.Clone(b)
}

// definition returns the Definition registered as name in this process. The
// caller must not change its Tools.
func (e *Engine) definition(name string) (Definition, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	def, ok := e.defs[name]
	return def, ok
}

// snapshotOf is the part of def a run keeps: what the model sees, with the
// tools sorted by name so the list is the same in every process, and limits
// filled in. Start passes the definition's limits or the request's; a child
// run is given its own.
func snapshotOf(def Definition, limits Limits) Snapshot {
	var tools []ToolSpec
	for _, tool := range def.Tools {
		tools = append(tools, ToolSpec{
			Name:        tool.Name,
			Description: tool.Description,
			Schema:      cloneRaw(tool.Schema),
		})
	}
	slices.SortFunc(tools, func(a, b ToolSpec) int { return cmp.Compare(a.Name, b.Name) })
	return Snapshot{
		System:    def.System,
		Model:     def.Model,
		Tools:     tools,
		Output:    cloneRaw(def.Output),
		MaxTokens: def.MaxTokens,
		Limits:    filledLimits(limits),
	}
}

// filledLimits is l with its defaults applied, as Limits documents them: a
// zero field takes its default and a negative field is no limit. No field of
// the result is zero: a limit whose default is none is written as -1, so a
// snapshot says what bounds its run without reference to the defaults of the
// build that reads it. Filling what is already filled changes nothing.
func filledLimits(l Limits) Limits {
	if l.MaxDuration == 0 {
		l.MaxDuration = defaultMaxDuration
	}
	if l.MaxCostMicros == 0 {
		l.MaxCostMicros = noLimit
	}
	if l.MaxTokens == 0 {
		l.MaxTokens = noLimit
	}
	if l.MaxModelCalls == 0 {
		l.MaxModelCalls = defaultMaxModelCalls
	}
	return l
}

// Start records a new runnable run and returns it. It executes nothing.
func (e *Engine) Start(ctx context.Context, req StartRequest) (Run, error) {
	def, ok := e.definition(req.Agent)
	if !ok {
		return Run{}, fmt.Errorf("agent: start: %w: %q", ErrUnknownAgent, req.Agent)
	}
	limits := def.Limits
	if req.Limits != nil {
		limits = *req.Limits
	}

	now := e.clock.Now()
	run, created, err := e.store.CreateRun(ctx, Run{
		ID:         e.opts.NewID(),
		Agent:      def.Name,
		Status:     StatusRunnable,
		Input:      req.Input,
		Key:        req.Key,
		Definition: snapshotOf(def, limits),
		Metadata:   req.Metadata,
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	if err != nil {
		return Run{}, fmt.Errorf("agent: start a run of %q: %w", req.Agent, err)
	}
	// A start that found its key made no change, and announces none.
	if created {
		e.publish(ctx, EventRunStarted, run, 0, now)
	}
	return run, nil
}

// GetRun returns one run.
func (e *Engine) GetRun(ctx context.Context, id string) (Run, error) {
	run, err := e.store.GetRun(ctx, id)
	if err != nil {
		return Run{}, fmt.Errorf("agent: get run %s: %w", id, err)
	}
	return run, nil
}

// ListRuns lists runs, newest first.
func (e *Engine) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	runs, err := e.store.ListRuns(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("agent: list runs: %w", err)
	}
	return runs, nil
}

// Timeline returns a run's journal in order.
func (e *Engine) Timeline(ctx context.Context, runID string) ([]Step, error) {
	steps, err := e.store.Steps(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("agent: timeline of run %s: %w", runID, err)
	}
	return steps, nil
}

// Changes returns what happened to a run after revision since.
func (e *Engine) Changes(ctx context.Context, runID string, since int64) (Changes, error) {
	changes, err := e.store.Changes(ctx, runID, since)
	if err != nil {
		return Changes{}, fmt.Errorf("agent: changes to run %s: %w", runID, err)
	}
	return changes, nil
}

// ListApprovals lists approvals, oldest first.
func (e *Engine) ListApprovals(ctx context.Context, f ApprovalFilter) ([]Approval, error) {
	approvals, err := e.store.ListApprovals(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("agent: list approvals: %w", err)
	}
	return approvals, nil
}

// Approve answers an approval yes, on behalf of by.
func (e *Engine) Approve(ctx context.Context, approvalID, by, reason string) (Approval, error) {
	return e.decide(ctx, "approve", approvalID, by, reason, true)
}

// Decline answers an approval no, on behalf of by.
func (e *Engine) Decline(ctx context.Context, approvalID, by, reason string) (Approval, error) {
	return e.decide(ctx, "decline", approvalID, by, reason, false)
}

// decide records a person's answer. It writes nothing to the journal: the
// run's next execution reads the answer and acts on it.
func (e *Engine) decide(ctx context.Context, op, approvalID, by, reason string, approved bool) (Approval, error) {
	by, err := recordable(by, reason)
	if err != nil {
		return Approval{}, fmt.Errorf("agent: %s %s: %w", op, approvalID, err)
	}
	now := e.clock.Now()
	approval, err := e.store.DecideApproval(ctx, DecideRequest{
		ID: approvalID, Approved: approved, By: by, Reason: reason, Now: now,
	})
	if err != nil {
		// With ErrAlreadyDecided the store hands back the answer that stands.
		return approval, fmt.Errorf("agent: %s %s: %w", op, approvalID, err)
	}

	// An approval does not name its agent, which an Event carries. The read
	// is skipped when nobody is listening, and when it fails the event is
	// lost and the answer stands: events are a hint, the journal the record.
	if e.opts.Events != nil {
		run, err := e.store.GetRun(ctx, approval.RunID)
		if err != nil {
			e.log.DebugContext(ctx, "agent: event not published: its run could not be read",
				"type", EventApprovalDecided, "run", approval.RunID, "error", err)
			return approval, nil
		}
		e.publish(ctx, EventApprovalDecided, run, approval.Seq, now)
	}
	return approval, nil
}

// Cancel asks for a run and its child runs to be cancelled.
//
// It marks the run and no more, and publishes EventRunCancelRequested. The
// execution that sees the mark, the one in flight at its next heartbeat or
// else the next to claim the run, asks the same of each child that has not
// ended and finishes the run cancelled, which is when EventRunCancelled is
// published.
func (e *Engine) Cancel(ctx context.Context, runID, by, reason string) error {
	by, err := recordable(by, reason)
	if err != nil {
		return fmt.Errorf("agent: cancel %s: %w", runID, err)
	}
	// Read first: the event carries the run's agent, and only the request
	// that sets the mark is a change to announce.
	run, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("agent: cancel %s: %w", runID, err)
	}
	now := e.clock.Now()
	err = e.store.RequestCancel(ctx, CancelRequest{RunID: runID, By: by, Reason: reason, Now: now})
	if err != nil {
		return fmt.Errorf("agent: cancel %s: %w", runID, err)
	}
	if !run.CancelRequested {
		e.publish(ctx, EventRunCancelRequested, run, 0, now)
	}
	return nil
}

// recordable checks who and why before they are written to a run's record,
// and returns the name without the space around it. The name must be one: a
// database keeps neither text that is not UTF-8 nor a NUL, and a name of
// spaces alone names nobody. The reason is the person's own text and is kept
// as given, so long as it can be kept.
func recordable(by, reason string) (string, error) {
	by = strings.TrimSpace(by)
	switch {
	case by == "":
		return "", errors.New("by is empty")
	case !utf8.ValidString(by):
		return "", errors.New("by is not valid UTF-8")
	case strings.ContainsRune(by, 0):
		return "", errors.New("by holds a NUL")
	case !utf8.ValidString(reason):
		return "", errors.New("reason is not valid UTF-8")
	case strings.ContainsRune(reason, 0):
		return "", errors.New("reason holds a NUL")
	}
	return by, nil
}

// publish tells Options.Events that run changed: typ is the Event's type,
// seq the step it is about or zero, and at the time of the change, which is
// the time given to the store call that made it. It is called after that
// call has returned, and is best effort: with no publisher it does nothing,
// and a publisher's error is logged at Debug and goes no further. Nor does a
// publisher's panic, which is logged at Error: every event of an execution
// comes through here from a worker's goroutine, where a panic would take
// the process down for the sake of a hint.
func (e *Engine) publish(ctx context.Context, typ string, run Run, seq int, at time.Time) {
	if e.opts.Events == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			e.log.ErrorContext(ctx, "agent: publisher panicked",
				"type", typ, "run", run.ID, "seq", seq, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	event := Event{Type: typ, RunID: run.ID, Agent: run.Agent, Seq: seq, At: at}
	if err := e.opts.Events.Publish(ctx, TopicRuns, event); err != nil {
		e.log.DebugContext(ctx, "agent: event not published",
			"type", typ, "run", run.ID, "seq", seq, "error", err)
	}
}
