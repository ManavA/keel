package agent_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

// contractTime is the instant every fixture in these contract tests is
// stamped with.
var contractTime = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// indentedJSON marshals v the way the literals in these tests are written: two
// spaces a level, fields in declaration order. The literals are compared as
// text, so a renamed tag, a reordered field or a dropped omitempty fails.
func indentedJSON(t *testing.T, v any) string {
	t.Helper()

	out, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	return string(out)
}

func TestUsage_Add(t *testing.T) {
	a := agent.Usage{InputTokens: 1, OutputTokens: 2, CostMicros: 3}
	b := agent.Usage{InputTokens: 10, OutputTokens: 20, CostMicros: 30}

	assert.Equal(t, agent.Usage{InputTokens: 11, OutputTokens: 22, CostMicros: 33}, a.Add(b))
	assert.Equal(t, a, a.Add(agent.Usage{}), "adding nothing changes nothing")
	assert.Equal(t, agent.Usage{InputTokens: 1, OutputTokens: 2, CostMicros: 3}, a,
		"Add returns the sum and leaves its receiver alone")
}

func TestWireForms_JSON(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{
			name:  "a user message",
			value: agent.Message{Role: agent.RoleUser, Text: "review the batch"},
			want: `{
  "role": "user",
  "text": "review the batch"
}`,
		},
		{
			name: "an assistant message with a call, a malformed call and its provider's form",
			value: agent.Message{
				Role: agent.RoleAssistant,
				Text: "reading it now",
				Calls: []agent.Call{
					{ID: "call_1", Name: "read_document", Input: json.RawMessage(`{"id":7}`)},
					{ID: "call_2", Name: "read_document", Input: json.RawMessage(`"{\"id\":"`), Malformed: true},
				},
				Opaque: &agent.Opaque{Provider: "anthropic", Data: json.RawMessage(`[{"type":"text","text":"reading it now"}]`)},
			},
			want: `{
  "role": "assistant",
  "text": "reading it now",
  "calls": [
    {
      "id": "call_1",
      "name": "read_document",
      "input": {
        "id": 7
      }
    },
    {
      "id": "call_2",
      "name": "read_document",
      "input": "{\"id\":",
      "malformed": true
    }
  ],
  "opaque": {
    "provider": "anthropic",
    "data": [
      {
        "type": "text",
        "text": "reading it now"
      }
    ]
  }
}`,
		},
		{
			name: "a tool message with a result and a failed result",
			value: agent.Message{
				Role: agent.RoleTool,
				Results: []agent.Result{
					{CallID: "call_1", Content: "the text"},
					{CallID: "call_2", Content: "arguments were not valid JSON", IsError: true},
				},
			},
			want: `{
  "role": "tool",
  "results": [
    {
      "call_id": "call_1",
      "content": "the text"
    },
    {
      "call_id": "call_2",
      "content": "arguments were not valid JSON",
      "is_error": true
    }
  ]
}`,
		},
		{
			name: "a tool as the model is told of it",
			value: agent.ToolSpec{
				Name: "read_document", Description: "Read one document.", Schema: json.RawMessage(`{"type":"object"}`),
			},
			want: `{
  "name": "read_document",
  "description": "Read one document.",
  "schema": {
    "type": "object"
  }
}`,
		},
		{
			name:  "a tool with only a name",
			value: agent.ToolSpec{Name: "list_documents"},
			want: `{
  "name": "list_documents"
}`,
		},
		{
			name:  "usage",
			value: agent.Usage{InputTokens: 100, OutputTokens: 40, CostMicros: 900},
			want: `{
  "input_tokens": 100,
  "output_tokens": 40,
  "cost_micros": 900
}`,
		},
		{
			name: "an action",
			value: agent.Action{
				Kind: "send", Target: "email:ap@example.com",
				Attrs: map[string]any{"external": true, agent.AttrTool: "send_digest"},
			},
			want: `{
  "kind": "send",
  "target": "email:ap@example.com",
  "attrs": {
    "external": true,
    "tool": "send_digest"
  }
}`,
		},
		{
			name:  "an action with only a kind",
			value: agent.Action{Kind: "run"},
			want: `{
  "kind": "run"
}`,
		},
		{
			name:  "a decision is written as the reference writes a verdict",
			value: agent.Decision{Effect: agent.Ask, Rule: "Sending needs a person"},
			want: `{
  "decision": "ask",
  "rule": "Sending needs a person"
}`,
		},
		{
			name: "a snapshot",
			value: agent.Snapshot{
				System:    "You coordinate reviews.",
				Model:     "model-a",
				Tools:     []agent.ToolSpec{{Name: "list_documents"}},
				Output:    json.RawMessage(`{"type":"object"}`),
				MaxTokens: 4000,
				Limits: agent.Limits{
					MaxDuration: 15 * time.Minute, MaxCostMicros: 2_000_000, MaxTokens: -1, MaxModelCalls: 50,
				},
			},
			want: `{
  "system": "You coordinate reviews.",
  "model": "model-a",
  "tools": [
    {
      "name": "list_documents"
    }
  ],
  "output": {
    "type": "object"
  },
  "max_tokens": 4000,
  "limits": {
    "max_duration_ns": 900000000000,
    "max_cost_micros": 2000000,
    "max_tokens": -1,
    "max_model_calls": 50
  }
}`,
		},
		{
			name:  "an event",
			value: agent.Event{Type: agent.EventStepCompleted, RunID: "run-1", Agent: "coordinator", Seq: 3, At: contractTime},
			want: `{
  "type": "step.completed",
  "run_id": "run-1",
  "agent": "coordinator",
  "seq": 3,
  "at": "2026-10-02T09:00:00Z"
}`,
		},
		{
			name:  "an event about the run as a whole names no step",
			value: agent.Event{Type: agent.EventRunStarted, RunID: "run-1", Agent: "coordinator", At: contractTime},
			want: `{
  "type": "run.started",
  "run_id": "run-1",
  "agent": "coordinator",
  "at": "2026-10-02T09:00:00Z"
}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, indentedJSON(t, tt.value))
		})
	}
}

// A journal round trip must give back the provider's own form of a turn with
// its keys in the order they came.
func TestMessage_JSONKeepsRawKeyOrder(t *testing.T) {
	msg := agent.Message{
		Role:   agent.RoleAssistant,
		Calls:  []agent.Call{{ID: "call_1", Name: "save", Input: json.RawMessage(`{"z":1,"a":2}`)}},
		Opaque: &agent.Opaque{Provider: "anthropic", Data: json.RawMessage(`[{"z":1,"a":2}]`)},
	}

	encoded, err := json.Marshal(msg)
	require.NoError(t, err)

	var back agent.Message
	require.NoError(t, json.Unmarshal(encoded, &back))
	assert.Equal(t, msg, back)
	assert.Equal(t, `{"z":1,"a":2}`, string(back.Calls[0].Input))
	assert.Equal(t, `[{"z":1,"a":2}]`, string(back.Opaque.Data))
}

// The values of these constants are stored in the journal, checked by the
// table constraints in agent/pg, and served over HTTP, so each is pinned to
// the word the design gives it.
func TestConstants_WireValues(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "RoleUser", got: string(agent.RoleUser), want: "user"},
		{name: "RoleAssistant", got: string(agent.RoleAssistant), want: "assistant"},
		{name: "RoleTool", got: string(agent.RoleTool), want: "tool"},

		{name: "StopEnd", got: string(agent.StopEnd), want: "end"},
		{name: "StopToolUse", got: string(agent.StopToolUse), want: "tool_use"},
		{name: "StopMaxTokens", got: string(agent.StopMaxTokens), want: "max_tokens"},
		{name: "StopRefusal", got: string(agent.StopRefusal), want: "refusal"},
		{name: "StopPause", got: string(agent.StopPause), want: "pause"},
		{name: "StopContextWindow", got: string(agent.StopContextWindow), want: "context_window"},

		{name: "AttrAgent", got: agent.AttrAgent, want: "agent"},
		{name: "AttrTool", got: agent.AttrTool, want: "tool"},
		{name: "AttrRun", got: agent.AttrRun, want: "run"},
		{name: "AttrSeq", got: agent.AttrSeq, want: "seq"},

		{name: "Allow", got: string(agent.Allow), want: "allow"},
		{name: "Ask", got: string(agent.Ask), want: "ask"},
		{name: "Block", got: string(agent.Block), want: "block"},

		{name: "RuleNoGuard", got: agent.RuleNoGuard, want: "no guard configured"},
		{name: "RuleToolApproval", got: agent.RuleToolApproval, want: "tool requires approval"},
		{name: "RuleInterrupted", got: agent.RuleInterrupted, want: "call was interrupted; outcome unknown"},

		{name: "StatusRunnable", got: string(agent.StatusRunnable), want: "runnable"},
		{name: "StatusWaiting", got: string(agent.StatusWaiting), want: "waiting"},
		{name: "StatusCompleted", got: string(agent.StatusCompleted), want: "completed"},
		{name: "StatusFailed", got: string(agent.StatusFailed), want: "failed"},
		{name: "StatusCancelled", got: string(agent.StatusCancelled), want: "cancelled"},

		{name: "ReasonApproval", got: agent.ReasonApproval, want: "approval"},
		{name: "ReasonChildren", got: agent.ReasonChildren, want: "children"},
		{name: "ReasonTimeBudget", got: agent.ReasonTimeBudget, want: "time_budget"},
		{name: "ReasonCostBudget", got: agent.ReasonCostBudget, want: "cost_budget"},
		{name: "ReasonTokenBudget", got: agent.ReasonTokenBudget, want: "token_budget"},
		{name: "ReasonModelCalls", got: agent.ReasonModelCalls, want: "model_calls"},
		{name: "ReasonRefusal", got: agent.ReasonRefusal, want: "refusal"},
		{name: "ReasonTruncated", got: agent.ReasonTruncated, want: "truncated"},
		{name: "ReasonContextWindow", got: agent.ReasonContextWindow, want: "context_window"},
		{name: "ReasonError", got: agent.ReasonError, want: "error"},
		{name: "ReasonAbandoned", got: agent.ReasonAbandoned, want: "abandoned"},
		{name: "ReasonCancelled", got: agent.ReasonCancelled, want: "cancelled"},

		{name: "StepModel", got: string(agent.StepModel), want: "model"},
		{name: "StepTool", got: string(agent.StepTool), want: "tool"},

		{name: "StepProposed", got: string(agent.StepProposed), want: "proposed"},
		{name: "StepWaiting", got: string(agent.StepWaiting), want: "waiting"},
		{name: "StepStarted", got: string(agent.StepStarted), want: "started"},
		{name: "StepCompleted", got: string(agent.StepCompleted), want: "completed"},
		{name: "StepBlocked", got: string(agent.StepBlocked), want: "blocked"},
		{name: "StepDeclined", got: string(agent.StepDeclined), want: "declined"},

		{name: "ApprovalPending", got: string(agent.ApprovalPending), want: "pending"},
		{name: "ApprovalApproved", got: string(agent.ApprovalApproved), want: "approved"},
		{name: "ApprovalDeclined", got: string(agent.ApprovalDeclined), want: "declined"},
		{name: "ApprovalExpired", got: string(agent.ApprovalExpired), want: "expired"},
		{name: "ApprovalCancelled", got: string(agent.ApprovalCancelled), want: "cancelled"},

		{name: "CauseGuard", got: string(agent.CauseGuard), want: "guard"},
		{name: "CauseTool", got: string(agent.CauseTool), want: "tool"},
		{name: "CauseInterrupted", got: string(agent.CauseInterrupted), want: "interrupted"},

		{name: "TopicRuns", got: agent.TopicRuns, want: "agent.runs"},
		{name: "EventRunStarted", got: agent.EventRunStarted, want: "run.started"},
		{name: "EventRunWaiting", got: agent.EventRunWaiting, want: "run.waiting"},
		{name: "EventRunCompleted", got: agent.EventRunCompleted, want: "run.completed"},
		{name: "EventRunFailed", got: agent.EventRunFailed, want: "run.failed"},
		{name: "EventRunCancelled", got: agent.EventRunCancelled, want: "run.cancelled"},
		{name: "EventStepStarted", got: agent.EventStepStarted, want: "step.started"},
		{name: "EventStepCompleted", got: agent.EventStepCompleted, want: "step.completed"},
		{name: "EventStepBlocked", got: agent.EventStepBlocked, want: "step.blocked"},
		{name: "EventApprovalRequested", got: agent.EventApprovalRequested, want: "approval.requested"},
		{name: "EventApprovalDecided", got: agent.EventApprovalDecided, want: "approval.decided"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

// A caller tells these apart with errors.Is, so no two may be the same value.
func TestErrors_AreDistinct(t *testing.T) {
	errs := map[string]error{
		"ErrNotFound":       agent.ErrNotFound,
		"ErrUnknownAgent":   agent.ErrUnknownAgent,
		"ErrLeaseLost":      agent.ErrLeaseLost,
		"ErrNotClaimable":   agent.ErrNotClaimable,
		"ErrConflict":       agent.ErrConflict,
		"ErrAlreadyDecided": agent.ErrAlreadyDecided,
		"ErrFinished":       agent.ErrFinished,
		"ErrTransient":      agent.ErrTransient,
		"ErrPermanent":      agent.ErrPermanent,
	}

	messages := map[string]string{}
	for name, err := range errs {
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "agent: ", name)
		if other, dup := messages[err.Error()]; dup {
			t.Errorf("%s and %s have the same message %q", name, other, err.Error())
		}
		messages[err.Error()] = name

		for otherName, other := range errs {
			if otherName != name {
				assert.NotErrorIs(t, err, other, "%s must not match %s", name, otherName)
			}
		}
	}
}
