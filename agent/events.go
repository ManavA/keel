package agent

import "time"

// TopicRuns is the topic every Event is published to.
const TopicRuns = "agent.runs"

// Event types. EventRunCancelRequested says a run was asked to stop and is
// still to end; EventRunCancelled says it has ended, cancelled.
const (
	EventRunStarted         = "run.started"
	EventRunWaiting         = "run.waiting"
	EventRunCompleted       = "run.completed"
	EventRunFailed          = "run.failed"
	EventRunCancelled       = "run.cancelled"
	EventRunCancelRequested = "run.cancel_requested"
	EventStepStarted        = "step.started"
	EventStepCompleted      = "step.completed"
	EventStepBlocked        = "step.blocked"
	EventApprovalRequested  = "approval.requested"
	EventApprovalDecided    = "approval.decided"
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
