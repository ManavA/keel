package agent_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/agent"
)

// These are the bodies agent/httpapi serves, so each is pinned as text.

func TestRun_JSON(t *testing.T) {
	expires := contractTime.Add(30 * time.Second)
	retryAt := contractTime.Add(2 * time.Second)
	finished := contractTime.Add(time.Minute)

	tests := []struct {
		name string
		run  agent.Run
		want string
	}{
		{
			name: "a run just started, with nothing optional set",
			run: agent.Run{
				ID: "run-1", Agent: "coordinator", Status: agent.StatusRunnable, Input: "batch 7",
				Definition: agent.Snapshot{System: "You coordinate reviews."},
				Rev:        1, CreatedAt: contractTime, UpdatedAt: contractTime,
			},
			want: `{
  "id": "run-1",
  "agent": "coordinator",
  "status": "runnable",
  "input": "batch 7",
  "definition": {
    "system": "You coordinate reviews.",
    "limits": {
      "max_duration_ns": 0,
      "max_cost_micros": 0,
      "max_tokens": 0,
      "max_model_calls": 0
    }
  },
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cost_micros": 0
  },
  "model_calls": 0,
  "active_ms": 0,
  "rev": 1,
  "lease_epoch": 0,
  "created_at": "2026-10-02T09:00:00Z",
  "updated_at": "2026-10-02T09:00:00Z"
}`,
		},
		{
			name: "a child run with every field set",
			run: agent.Run{
				ID: "run-2", Agent: "reviewer", Status: agent.StatusFailed, Reason: agent.ReasonError,
				Input: `{"id":7}`, Output: "partial", Error: "model unavailable",
				ParentID: "run-1", ParentSeq: 4, Depth: 1,
				Key: "run-1:4",
				Definition: agent.Snapshot{
					System: "You review one document.",
					Model:  "model-a",
					Tools:  []agent.ToolSpec{{Name: "read_document", Schema: json.RawMessage(`{"type":"object"}`)}},
					Limits: agent.Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: -1, MaxTokens: -1, MaxModelCalls: 50},
				},
				Metadata:     map[string]string{"batch": "7"},
				Usage:        agent.Usage{InputTokens: 1200, OutputTokens: 300, CostMicros: 8100},
				ModelCalls:   2,
				ActiveMillis: 4500,
				Rev:          9,
				LeaseOwner:   "worker-a", LeaseEpoch: 3, LeaseExpiresAt: &expires,
				Failures: 2, NextAttemptAt: &retryAt,
				CancelRequested: true, CancelBy: "operator", CancelReason: "wrong batch",
				CreatedAt: contractTime, UpdatedAt: finished, FinishedAt: &finished,
			},
			want: `{
  "id": "run-2",
  "agent": "reviewer",
  "status": "failed",
  "reason": "error",
  "input": "{\"id\":7}",
  "output": "partial",
  "error": "model unavailable",
  "parent_id": "run-1",
  "parent_seq": 4,
  "depth": 1,
  "key": "run-1:4",
  "definition": {
    "system": "You review one document.",
    "model": "model-a",
    "tools": [
      {
        "name": "read_document",
        "schema": {
          "type": "object"
        }
      }
    ],
    "limits": {
      "max_duration_ns": 900000000000,
      "max_cost_micros": -1,
      "max_tokens": -1,
      "max_model_calls": 50
    }
  },
  "metadata": {
    "batch": "7"
  },
  "usage": {
    "input_tokens": 1200,
    "output_tokens": 300,
    "cost_micros": 8100
  },
  "model_calls": 2,
  "active_ms": 4500,
  "rev": 9,
  "lease_owner": "worker-a",
  "lease_epoch": 3,
  "lease_expires_at": "2026-10-02T09:00:30Z",
  "failures": 2,
  "next_attempt_at": "2026-10-02T09:00:02Z",
  "cancel_requested": true,
  "cancel_by": "operator",
  "cancel_reason": "wrong batch",
  "created_at": "2026-10-02T09:00:00Z",
  "updated_at": "2026-10-02T09:01:00Z",
  "finished_at": "2026-10-02T09:01:00Z"
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, indentedJSON(t, tt.run))
		})
	}
}

func TestStep_JSON(t *testing.T) {
	started := contractTime.Add(time.Second)
	finished := contractTime.Add(3 * time.Second)

	tests := []struct {
		name string
		step agent.Step
		want string
	}{
		{
			name: "a model step that has started",
			step: agent.Step{
				RunID: "run-1", Seq: 1, Kind: agent.StepModel, Status: agent.StepStarted,
				Attempts: 1, Rev: 2, CreatedAt: contractTime, StartedAt: &started,
			},
			want: `{
  "run_id": "run-1",
  "seq": 1,
  "kind": "model",
  "status": "started",
  "attempts": 1,
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cost_micros": 0
  },
  "rev": 2,
  "created_at": "2026-10-02T09:00:00Z",
  "started_at": "2026-10-02T09:00:01Z"
}`,
		},
		{
			name: "a completed model step with its reply",
			step: agent.Step{
				RunID: "run-1", Seq: 1, Kind: agent.StepModel, Status: agent.StepCompleted, Name: "model-a",
				Message: &agent.Message{
					Role:   agent.RoleAssistant,
					Text:   "sending the digest",
					Calls:  []agent.Call{{ID: "call_1", Name: "send_digest", Input: json.RawMessage(`{"to":"ap"}`)}},
					Opaque: &agent.Opaque{Provider: "anthropic", Data: json.RawMessage(`[{"type":"thinking"}]`)},
				},
				Stop:     agent.StopToolUse,
				Attempts: 2,
				Usage:    agent.Usage{InputTokens: 900, OutputTokens: 60, CostMicros: 3600},
				Rev:      3, CreatedAt: contractTime, StartedAt: &started, FinishedAt: &finished,
			},
			want: `{
  "run_id": "run-1",
  "seq": 1,
  "kind": "model",
  "status": "completed",
  "name": "model-a",
  "message": {
    "role": "assistant",
    "text": "sending the digest",
    "calls": [
      {
        "id": "call_1",
        "name": "send_digest",
        "input": {
          "to": "ap"
        }
      }
    ],
    "opaque": {
      "provider": "anthropic",
      "data": [
        {
          "type": "thinking"
        }
      ]
    }
  },
  "stop": "tool_use",
  "attempts": 2,
  "usage": {
    "input_tokens": 900,
    "output_tokens": 60,
    "cost_micros": 3600
  },
  "rev": 3,
  "created_at": "2026-10-02T09:00:00Z",
  "started_at": "2026-10-02T09:00:01Z",
  "finished_at": "2026-10-02T09:00:03Z"
}`,
		},
		{
			name: "a tool step as it is proposed",
			step: agent.Step{
				RunID: "run-1", Seq: 2, Kind: agent.StepTool, Status: agent.StepProposed, Name: "send_digest",
				Turn: 1,
				Call: &agent.Call{ID: "call_1", Name: "send_digest", Input: json.RawMessage(`{"to":"ap"}`)},
				Key:  agent.StepKey("run-1", 2),
				Rev:  3, CreatedAt: contractTime,
			},
			want: `{
  "run_id": "run-1",
  "seq": 2,
  "kind": "tool",
  "status": "proposed",
  "name": "send_digest",
  "turn": 1,
  "call": {
    "id": "call_1",
    "name": "send_digest",
    "input": {
      "to": "ap"
    }
  },
  "key": "run-1:2",
  "attempts": 0,
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cost_micros": 0
  },
  "rev": 3,
  "created_at": "2026-10-02T09:00:00Z"
}`,
		},
		{
			name: "a tool step the guard blocked",
			step: agent.Step{
				RunID: "run-1", Seq: 3, Kind: agent.StepTool, Status: agent.StepBlocked, Name: "delete_document",
				Turn:     1,
				Call:     &agent.Call{ID: "call_2", Name: "delete_document", Input: json.RawMessage(`{"id":7}`)},
				Key:      agent.StepKey("run-1", 3),
				Decision: agent.Block, Rule: "Deleting documents is never allowed",
				Result: "blocked by policy: Deleting documents is never allowed", IsError: true,
				Rev: 5, CreatedAt: contractTime, FinishedAt: &finished,
			},
			want: `{
  "run_id": "run-1",
  "seq": 3,
  "kind": "tool",
  "status": "blocked",
  "name": "delete_document",
  "turn": 1,
  "call": {
    "id": "call_2",
    "name": "delete_document",
    "input": {
      "id": 7
    }
  },
  "key": "run-1:3",
  "decision": "block",
  "rule": "Deleting documents is never allowed",
  "result": "blocked by policy: Deleting documents is never allowed",
  "is_error": true,
  "attempts": 0,
  "usage": {
    "input_tokens": 0,
    "output_tokens": 0,
    "cost_micros": 0
  },
  "rev": 5,
  "created_at": "2026-10-02T09:00:00Z",
  "finished_at": "2026-10-02T09:00:03Z"
}`,
		},
		{
			name: "a delegating tool step that collected its child run",
			step: agent.Step{
				RunID: "run-1", Seq: 4, Kind: agent.StepTool, Status: agent.StepCompleted, Name: "review_document",
				Turn:     1,
				Call:     &agent.Call{ID: "call_3", Name: "review_document", Input: json.RawMessage(`{"id":7}`)},
				Key:      agent.StepKey("run-1", 4),
				Decision: agent.Allow, Rule: "Delegating is allowed",
				Result:     "a one-line summary",
				ChildRunID: "run-2",
				Attempts:   1,
				Usage:      agent.Usage{InputTokens: 1200, OutputTokens: 300, CostMicros: 8100},
				Rev:        8, CreatedAt: contractTime, StartedAt: &started, FinishedAt: &finished,
			},
			want: `{
  "run_id": "run-1",
  "seq": 4,
  "kind": "tool",
  "status": "completed",
  "name": "review_document",
  "turn": 1,
  "call": {
    "id": "call_3",
    "name": "review_document",
    "input": {
      "id": 7
    }
  },
  "key": "run-1:4",
  "decision": "allow",
  "rule": "Delegating is allowed",
  "result": "a one-line summary",
  "child_run_id": "run-2",
  "attempts": 1,
  "usage": {
    "input_tokens": 1200,
    "output_tokens": 300,
    "cost_micros": 8100
  },
  "rev": 8,
  "created_at": "2026-10-02T09:00:00Z",
  "started_at": "2026-10-02T09:00:01Z",
  "finished_at": "2026-10-02T09:00:03Z"
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, indentedJSON(t, tt.step))
		})
	}
}

func TestApproval_JSON(t *testing.T) {
	decided := contractTime.Add(time.Minute)
	expires := contractTime.Add(time.Hour)

	pending := agent.Approval{
		ID: "approval-1", RunID: "run-1", Seq: 2, Attempt: 0,
		Cause: agent.CauseGuard, Tool: "send_digest",
		Input: json.RawMessage(`{"to":"ap"}`),
		Action: agent.Action{
			Kind: "send", Target: "email:ap@example.com",
			Attrs: map[string]any{"external": true},
		},
		Rule:   "Sending needs a person",
		Status: agent.ApprovalPending,
		Rev:    4, RequestedAt: contractTime,
	}

	approved := pending
	approved.Status = agent.ApprovalApproved
	approved.DecidedBy = "operator"
	approved.Reason = "checked the recipients"
	approved.Rev = 6
	approved.DecidedAt = &decided
	approved.ExpiresAt = &expires

	tests := []struct {
		name     string
		approval agent.Approval
		want     string
	}{
		{
			name:     "pending, with no expiry",
			approval: pending,
			want: `{
  "id": "approval-1",
  "run_id": "run-1",
  "seq": 2,
  "attempt": 0,
  "cause": "guard",
  "tool": "send_digest",
  "input": {
    "to": "ap"
  },
  "action": {
    "kind": "send",
    "target": "email:ap@example.com",
    "attrs": {
      "external": true
    }
  },
  "rule": "Sending needs a person",
  "status": "pending",
  "rev": 4,
  "requested_at": "2026-10-02T09:00:00Z"
}`,
		},
		{
			name:     "approved",
			approval: approved,
			want: `{
  "id": "approval-1",
  "run_id": "run-1",
  "seq": 2,
  "attempt": 0,
  "cause": "guard",
  "tool": "send_digest",
  "input": {
    "to": "ap"
  },
  "action": {
    "kind": "send",
    "target": "email:ap@example.com",
    "attrs": {
      "external": true
    }
  },
  "rule": "Sending needs a person",
  "status": "approved",
  "decided_by": "operator",
  "reason": "checked the recipients",
  "rev": 6,
  "requested_at": "2026-10-02T09:00:00Z",
  "decided_at": "2026-10-02T09:01:00Z",
  "expires_at": "2026-10-02T10:00:00Z"
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, indentedJSON(t, tt.approval))
		})
	}
}

func TestChanges_JSON(t *testing.T) {
	changes := agent.Changes{
		Run:       agent.Run{ID: "run-1", Agent: "coordinator", Status: agent.StatusWaiting, Reason: agent.ReasonApproval, CreatedAt: contractTime, UpdatedAt: contractTime},
		Steps:     []agent.Step{},
		Approvals: []agent.Approval{},
	}

	var keys map[string]json.RawMessage
	assert.NoError(t, json.Unmarshal([]byte(indentedJSON(t, changes)), &keys))
	assert.Len(t, keys, 3)
	assert.Contains(t, keys, "run")
	assert.JSONEq(t, `[]`, string(keys["steps"]))
	assert.JSONEq(t, `[]`, string(keys["approvals"]))
}

func TestStepKey(t *testing.T) {
	tests := []struct {
		name  string
		runID string
		seq   int
		want  string
	}{
		{name: "the run and the step", runID: "0b6f2f0e-6a57-4b7e-9d0c-3f5f8f1f2a11", seq: 4, want: "0b6f2f0e-6a57-4b7e-9d0c-3f5f8f1f2a11:4"},
		{name: "a step past nine", runID: "run-1", seq: 12, want: "run-1:12"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, agent.StepKey(tt.runID, tt.seq))
		})
	}

	assert.NotEqual(t, agent.StepKey("run-1", 2), agent.StepKey("run-1", 3), "two steps of one run have different keys")
	assert.NotEqual(t, agent.StepKey("run-1", 2), agent.StepKey("run-2", 2), "one step number in two runs has different keys")
}

func TestRun_Lease(t *testing.T) {
	expires := contractTime.Add(30 * time.Second)
	run := agent.Run{ID: "run-1", LeaseOwner: "worker-a", LeaseEpoch: 3, LeaseExpiresAt: &expires}

	assert.Equal(t, agent.Lease{RunID: "run-1", Owner: "worker-a", Epoch: 3}, run.Lease())
	assert.Equal(t, agent.Lease{RunID: "run-2"}, agent.Run{ID: "run-2"}.Lease(), "a run nobody has claimed")
}

func TestRun_Terminal(t *testing.T) {
	tests := []struct {
		status agent.Status
		want   bool
	}{
		{status: agent.StatusRunnable, want: false},
		{status: agent.StatusWaiting, want: false},
		{status: agent.StatusCompleted, want: true},
		{status: agent.StatusFailed, want: true},
		{status: agent.StatusCancelled, want: true},
		{status: "", want: false},
	}

	for _, tt := range tests {
		t.Run("status "+string(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.want, agent.Run{Status: tt.status}.Terminal())
		})
	}
}

func TestRun_Running(t *testing.T) {
	expires := contractTime.Add(30 * time.Second)

	tests := []struct {
		name string
		run  agent.Run
		now  time.Time
		want bool
	}{
		{
			name: "before the lease expires",
			run:  agent.Run{Status: agent.StatusRunnable, LeaseExpiresAt: &expires},
			now:  expires.Add(-time.Nanosecond),
			want: true,
		},
		{
			// Claim takes a lease whose expiry is not after now, so at the
			// instant of expiry the run is no longer held.
			name: "at the instant the lease expires",
			run:  agent.Run{Status: agent.StatusRunnable, LeaseExpiresAt: &expires},
			now:  expires,
			want: false,
		},
		{
			name: "after the lease expires",
			run:  agent.Run{Status: agent.StatusRunnable, LeaseExpiresAt: &expires},
			now:  expires.Add(time.Nanosecond),
			want: false,
		},
		{
			name: "runnable with no lease",
			run:  agent.Run{Status: agent.StatusRunnable},
			now:  contractTime,
			want: false,
		},
		{
			name: "waiting is not running, whatever the lease says",
			run:  agent.Run{Status: agent.StatusWaiting, LeaseExpiresAt: &expires},
			now:  contractTime,
			want: false,
		},
		{
			name: "an ended run is not running",
			run:  agent.Run{Status: agent.StatusCompleted, LeaseExpiresAt: &expires},
			now:  contractTime,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.run.Running(tt.now))
		})
	}
}

func TestStepStatus_Done(t *testing.T) {
	tests := []struct {
		status agent.StepStatus
		want   bool
	}{
		{status: agent.StepProposed, want: false},
		{status: agent.StepWaiting, want: false},
		{status: agent.StepStarted, want: false},
		{status: agent.StepCompleted, want: true},
		{status: agent.StepBlocked, want: true},
		{status: agent.StepDeclined, want: true},
		{status: "", want: false},
	}

	for _, tt := range tests {
		t.Run("status "+string(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.status.Done())
		})
	}
}
