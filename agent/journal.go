package agent

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

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
	// Metadata is the caller's own. A run started with none reads back with
	// an empty map.
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
	// gives them, a number as a float64, and as an empty map when there
	// were none.
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
