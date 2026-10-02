package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Ids are UUIDs: a store keeps a run or an approval under nothing else.
const (
	planRunID      = "00000000-0000-4000-8000-0000000000a1"
	planOtherRunID = "00000000-0000-4000-8000-0000000000a2"
	planChildID    = "00000000-0000-4000-8000-0000000000c1"
	planChildID2   = "00000000-0000-4000-8000-0000000000c2"
)

var planStart = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

// planDefinition has a tool of each kind next tells apart: one that runs,
// one that must not run twice unasked, one that delegates, and one marked
// both ways.
func planDefinition() Definition {
	run := func(context.Context, Invocation) (string, error) { return "", nil }
	return Definition{
		Name: "planner",
		Tools: []Tool{
			{Name: "lookup", Run: run},
			{Name: "charge", Run: run, AtMostOnce: true},
			{Name: "helper", Delegate: "assistant"},
			{Name: "courier", Delegate: "assistant", AtMostOnce: true},
		},
	}
}

// planRun is a run that has spent nothing, with its limits as Start fills
// them in.
func planRun(changes ...func(*Run)) Run {
	run := Run{
		ID:     planRunID,
		Agent:  "planner",
		Status: StatusRunnable,
		Input:  "what is the total?",
		Definition: Snapshot{
			System: "be exact",
			Limits: Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: -1, MaxTokens: -1, MaxModelCalls: 50},
		},
	}
	for _, change := range changes {
		change(&run)
	}
	return run
}

// planLimits sets the run's limits as they stand in its snapshot.
func planLimits(limits Limits) func(*Run) {
	return func(run *Run) { run.Definition.Limits = limits }
}

func planCancelRequested(run *Run) { run.CancelRequested = true }

// planReply is a completed model step.
func planReply(seq int, stop Stop, text string, calls ...Call) Step {
	return Step{
		RunID:    planRunID,
		Seq:      seq,
		Kind:     StepModel,
		Status:   StepCompleted,
		Name:     "scripted",
		Message:  &Message{Role: RoleAssistant, Text: text, Calls: calls},
		Stop:     stop,
		Attempts: 1,
	}
}

// planModelStarted is a model step whose reply was never stored.
func planModelStarted(seq int) Step {
	return Step{RunID: planRunID, Seq: seq, Kind: StepModel, Status: StepStarted, Attempts: 1}
}

// planTool is a tool step the reply at turn proposed.
func planTool(seq, turn int, tool string, status StepStatus, changes ...func(*Step)) Step {
	st := Step{
		RunID:  planRunID,
		Seq:    seq,
		Kind:   StepTool,
		Status: status,
		Name:   tool,
		Turn:   turn,
		Call:   &Call{ID: "call-" + strconv.Itoa(seq), Name: tool, Input: json.RawMessage(`{}`)},
		Key:    StepKey(planRunID, seq),
	}
	for _, change := range changes {
		change(&st)
	}
	return st
}

// planAttempts is how many times the step has started.
func planAttempts(n int) func(*Step) { return func(st *Step) { st.Attempts = n } }

func planChildOf(id string) func(*Step) { return func(st *Step) { st.ChildRunID = id } }

func planResult(text string, isError bool) func(*Step) {
	return func(st *Step) { st.Result, st.IsError = text, isError }
}

// planJournal is a reply at step 1 that made the calls of the tool steps
// given, and then those steps.
func planJournal(tools ...Step) []Step {
	calls := make([]Call, len(tools))
	for i, st := range tools {
		calls[i] = *st.Call
	}
	return append([]Step{planReply(1, StopToolUse, "", calls...)}, tools...)
}

func planApproval(seq, attempt int, status ApprovalStatus) Approval {
	return Approval{
		ID:          fmt.Sprintf("00000000-0000-4000-8000-%06d%06d", seq, attempt),
		RunID:       planRunID,
		Seq:         seq,
		Attempt:     attempt,
		Cause:       CauseGuard,
		Status:      status,
		RequestedAt: planStart,
	}
}

func planChildren(children ...Run) map[string]Run {
	out := map[string]Run{}
	for _, child := range children {
		out[child.ID] = child
	}
	return out
}

func planChild(id string, status Status) Run {
	return Run{ID: id, Agent: "assistant", Status: status, ParentID: planRunID, Depth: 1}
}

// planCase is a journal and the action it calls for. A zero run is planRun.
type planCase struct {
	name      string
	run       Run
	steps     []Step
	approvals []Approval
	children  map[string]Run
	want      action
}

func planInputs(t *testing.T, tc planCase) string {
	t.Helper()
	text, err := json.Marshal([]any{tc.run, tc.steps, tc.approvals, tc.children})
	require.NoError(t, err)
	return string(text)
}

// checkPlan asks next about each case. Every case is also asked twice and
// its inputs compared before and after, since the engine's resumption rests
// on next being a function of the journal and nothing else.
func checkPlan(t *testing.T, cases []planCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.run.ID == "" {
				tc.run = planRun()
			}
			before := planInputs(t, tc)

			got := next(tc.run, planDefinition(), tc.steps, tc.approvals, tc.children)

			assert.Equal(t, tc.want, got)
			assert.Equal(t, got, next(tc.run, planDefinition(), tc.steps, tc.approvals, tc.children),
				"the same journal gave two actions")
			assert.Equal(t, before, planInputs(t, tc), "next changed what it was given")
		})
	}
}

// One case per row of the design's table of what a crash does at each point
// (6.7): the journal the crash leaves, and what the next execution does.
func TestNext_AfterACrash(t *testing.T) {
	checkPlan(t, []planCase{
		{
			name: "before BeginModel commits: makes the call",
			want: action{kind: actModel, seq: 1},
		},
		{
			name:  "after BeginModel, before CompleteModel commits: makes the call again",
			steps: []Step{planModelStarted(1)},
			want:  action{kind: actModel, seq: 1},
		},
		{
			name:  "after CompleteModel commits: judges the first proposed step",
			steps: planJournal(planTool(2, 1, "lookup", StepProposed), planTool(3, 1, "lookup", StepProposed)),
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name:  "while judging, before the step leaves proposed: asks the guard again",
			steps: planJournal(planTool(2, 1, "charge", StepProposed)),
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name: "after a step is blocked or declined: passes over it",
			steps: planJournal(
				planTool(2, 1, "lookup", StepBlocked),
				planTool(3, 1, "lookup", StepDeclined),
				planTool(4, 1, "lookup", StepProposed),
			),
			want: action{kind: actJudge, seq: 4},
		},
		{
			name:      "after RequestApproval commits: parks",
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			want:      action{kind: actPark, reason: ReasonApproval},
		},
		{
			name:  "after proposed to started, before started to completed commits: runs the tool again",
			steps: planJournal(planTool(2, 1, "lookup", StepStarted, planAttempts(1))),
			want:  action{kind: actRun, seq: 2},
		},
		{
			name:  "after proposed to started, before started to completed commits, an at-most-once tool: asks a person",
			steps: planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(1))),
			want:  action{kind: actAsk, seq: 2},
		},
		{
			name:  "after started to completed commits: passes over it",
			steps: planJournal(planTool(2, 1, "lookup", StepCompleted), planTool(3, 1, "lookup", StepProposed)),
			want:  action{kind: actJudge, seq: 3},
		},
		{
			name:  "between starting a child and recording it, the child not yet created: creates the child",
			steps: planJournal(planTool(2, 1, "helper", StepStarted, planAttempts(1))),
			want:  action{kind: actSpawn, seq: 2},
		},
		{
			name:     "between starting a child and recording it, the child created: creates it again under the same key",
			steps:    planJournal(planTool(2, 1, "helper", StepStarted, planAttempts(1))),
			children: planChildren(planChild(planChildID, StatusRunnable)),
			want:     action{kind: actSpawn, seq: 2},
		},
		{
			name:      "after Park commits, waiting on a person: nothing runs",
			run:       planRun(func(run *Run) { run.Status, run.Reason = StatusWaiting, ReasonApproval }),
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			want:      action{kind: actPark, reason: ReasonApproval},
		},
		{
			name:     "after Park commits, waiting on a child: nothing runs",
			run:      planRun(func(run *Run) { run.Status, run.Reason = StatusWaiting, ReasonChildren }),
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusRunnable)),
			want:     action{kind: actPark, reason: ReasonChildren},
		},
		{
			name:  "after the last step, before Finish commits: finishes",
			steps: []Step{planReply(1, StopEnd, "the total is 42")},
			want:  action{kind: actFinish, status: StatusCompleted, output: "the total is 42"},
		},
	})
}

// Rule 1: a run asked to stop does nothing else.
func TestNext_Cancellation(t *testing.T) {
	cancelled := action{kind: actFinish, status: StatusCancelled, reason: ReasonCancelled}
	checkPlan(t, []planCase{
		{
			name: "on an empty journal",
			run:  planRun(planCancelRequested),
			want: cancelled,
		},
		{
			name:  "with a call still to judge",
			run:   planRun(planCancelRequested),
			steps: planJournal(planTool(2, 1, "lookup", StepProposed)),
			want:  cancelled,
		},
		{
			name:  "with a model call interrupted",
			run:   planRun(planCancelRequested),
			steps: []Step{planModelStarted(1)},
			want:  cancelled,
		},
		{
			name:      "with a person still to answer: it does not park",
			run:       planRun(planCancelRequested),
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			want:      cancelled,
		},
		{
			name:     "with a child that has ended: it is not collected",
			run:      planRun(planCancelRequested),
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusCompleted)),
			want:     cancelled,
		},
		{
			name:  "with the final answer already stored",
			run:   planRun(planCancelRequested),
			steps: []Step{planReply(1, StopEnd, "the total is 42")},
			want:  cancelled,
		},
		{
			name:  "with a refusal stored",
			run:   planRun(planCancelRequested),
			steps: []Step{planReply(1, StopRefusal, "")},
			want:  cancelled,
		},
		{
			name: "with a budget spent as well: the run is cancelled, not failed",
			run: planRun(planCancelRequested, func(run *Run) {
				run.ActiveMillis = (15 * time.Minute).Milliseconds()
			}),
			steps: planJournal(planTool(2, 1, "lookup", StepProposed)),
			want:  cancelled,
		},
	})
}

// A refusal is the run's last word, whatever came with it.
func TestNext_Refusal(t *testing.T) {
	refused := action{kind: actFinish, status: StatusFailed, reason: ReasonRefusal}
	checkPlan(t, []planCase{
		{
			name:  "a refusal fails the run",
			steps: []Step{planReply(1, StopRefusal, "")},
			want:  refused,
		},
		{
			name: "a refusal after earlier turns",
			steps: append(planJournal(planTool(2, 1, "lookup", StepCompleted)),
				planReply(3, StopRefusal, "")),
			want: refused,
		},
		{
			name: "a refusal that came with calls is not a turn: nothing is judged",
			steps: []Step{
				planReply(1, StopRefusal, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(2, 1, "lookup", StepProposed),
			},
			want: refused,
		},
		{
			name: "a refusal that came with calls, one of them left started: nothing is run",
			steps: []Step{
				planReply(1, StopRefusal, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(2, 1, "lookup", StepStarted, planAttempts(1)),
			},
			want: refused,
		},
		{
			name: "a refusal that came with calls, all of them answered: the model is not asked again",
			steps: []Step{
				planReply(1, StopRefusal, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(2, 1, "lookup", StepCompleted),
			},
			want: refused,
		},
		{
			name:  "a refusal with a budget spent keeps its reason",
			run:   planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: []Step{planReply(1, StopRefusal, "")},
			want:  refused,
		},
		{
			name: "an earlier turn's refusal is not the last word once a later reply is stored",
			steps: []Step{
				planReply(1, StopRefusal, ""),
				planReply(2, StopEnd, "the total is 42"),
			},
			want: action{kind: actFinish, status: StatusCompleted, output: "the total is 42"},
		},
	})
}

// Rule 2: the tool steps that are not final, in order.
func TestNext_ToolSteps(t *testing.T) {
	checkPlan(t, []planCase{
		{
			name:  "a reply with two proposed calls: the first is judged",
			steps: planJournal(planTool(2, 1, "lookup", StepProposed), planTool(3, 1, "charge", StepProposed)),
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name:  "one call final and one proposed: the proposed one is judged",
			steps: planJournal(planTool(2, 1, "lookup", StepCompleted), planTool(3, 1, "charge", StepProposed)),
			want:  action{kind: actJudge, seq: 3},
		},
		{
			name:  "a proposed call to a tool this build does not have is still judged",
			steps: planJournal(planTool(2, 1, "gone", StepProposed)),
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name:  "a started step for a plain tool: run again",
			steps: planJournal(planTool(2, 1, "lookup", StepStarted, planAttempts(2))),
			want:  action{kind: actRun, seq: 2},
		},
		{
			name:  "a started step for a delegating tool: its child is started",
			steps: planJournal(planTool(2, 1, "helper", StepStarted, planAttempts(1))),
			want:  action{kind: actSpawn, seq: 2},
		},
		{
			name:  "a started step for a delegating tool marked at-most-once: its child is started, since a child is created once under its key",
			steps: planJournal(planTool(2, 1, "courier", StepStarted, planAttempts(1))),
			want:  action{kind: actSpawn, seq: 2},
		},
		{
			name:  "a started step for an at-most-once tool with no approval: a person is asked",
			steps: planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(1))),
			want:  action{kind: actAsk, seq: 2},
		},
		{
			name:      "a started step for an at-most-once tool with an approved approval for its attempt: run",
			steps:     planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(2))),
			approvals: []Approval{planApproval(2, 1, ApprovalApproved), planApproval(2, 2, ApprovalApproved)},
			want:      action{kind: actRun, seq: 2},
		},
		{
			name:      "a started step for an at-most-once tool approved for an earlier attempt only: a person is asked again",
			steps:     planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(2))),
			approvals: []Approval{planApproval(2, 1, ApprovalApproved)},
			want:      action{kind: actAsk, seq: 2},
		},
		{
			name:      "a started step for an at-most-once tool approved before it first started: a person is asked",
			steps:     planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(1))),
			approvals: []Approval{planApproval(2, 0, ApprovalApproved)},
			want:      action{kind: actAsk, seq: 2},
		},
		{
			name:      "a started step for an at-most-once tool whose approval for its attempt was declined: a person is asked",
			steps:     planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(1))),
			approvals: []Approval{planApproval(2, 1, ApprovalDeclined)},
			want:      action{kind: actAsk, seq: 2},
		},
		{
			name: "a started step for an at-most-once tool: another step's approval is not its own",
			steps: planJournal(
				planTool(2, 1, "lookup", StepCompleted, planAttempts(1)),
				planTool(3, 1, "charge", StepStarted, planAttempts(1)),
			),
			approvals: []Approval{planApproval(2, 1, ApprovalApproved)},
			want:      action{kind: actAsk, seq: 3},
		},
		{
			name:  "a started step for an at-most-once tool: another run's approval is not its own",
			steps: planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(1))),
			approvals: []Approval{func() Approval {
				a := planApproval(2, 1, ApprovalApproved)
				a.RunID = planOtherRunID
				return a
			}()},
			want: action{kind: actAsk, seq: 2},
		},
		{
			name:  "a started step for a tool this build does not have: run, for the executor to answer",
			steps: planJournal(planTool(2, 1, "gone", StepStarted, planAttempts(1))),
			want:  action{kind: actRun, seq: 2},
		},
		{
			name:      "a waiting step whose approval is approved: run",
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalApproved)},
			want:      action{kind: actRun, seq: 2},
		},
		{
			name:      "a waiting step whose approval is declined: the no is recorded",
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalDeclined)},
			want:      action{kind: actResolve, seq: 2},
		},
		{
			name:      "a waiting step whose approval expired: the no is recorded",
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalExpired)},
			want:      action{kind: actResolve, seq: 2},
		},
		{
			name:      "a waiting step whose approval was cancelled: the no is recorded",
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalCancelled)},
			want:      action{kind: actResolve, seq: 2},
		},
		{
			name:      "a waiting step for a delegating tool whose approval is approved: run, which starts its child",
			steps:     planJournal(planTool(2, 1, "helper", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalApproved)},
			want:      action{kind: actRun, seq: 2},
		},
		{
			name:      "the approval with the highest attempt decides: an old yes and a new question",
			steps:     planJournal(planTool(2, 1, "charge", StepWaiting, planAttempts(2))),
			approvals: []Approval{planApproval(2, 1, ApprovalApproved), planApproval(2, 2, ApprovalPending)},
			want:      action{kind: actPark, reason: ReasonApproval},
		},
		{
			name:      "the approval with the highest attempt decides: an old no and a new yes",
			steps:     planJournal(planTool(2, 1, "charge", StepWaiting, planAttempts(2))),
			approvals: []Approval{planApproval(2, 1, ApprovalDeclined), planApproval(2, 2, ApprovalApproved)},
			want:      action{kind: actRun, seq: 2},
		},
		{
			name:      "the approval with the highest attempt decides, whatever order they come in",
			steps:     planJournal(planTool(2, 1, "charge", StepWaiting, planAttempts(2))),
			approvals: []Approval{planApproval(2, 2, ApprovalDeclined), planApproval(2, 1, ApprovalApproved)},
			want:      action{kind: actResolve, seq: 2},
		},
		{
			name: "a waiting step: another step's approval is not its own",
			steps: planJournal(
				planTool(2, 1, "lookup", StepWaiting),
				planTool(3, 1, "lookup", StepWaiting),
			),
			approvals: []Approval{planApproval(2, 0, ApprovalPending), planApproval(3, 0, ApprovalDeclined)},
			want:      action{kind: actResolve, seq: 3},
		},
		{
			name:  "a waiting step: another run's approval is not its own",
			steps: planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{
				planApproval(2, 0, ApprovalPending),
				func() Approval {
					a := planApproval(2, 1, ApprovalApproved)
					a.RunID = planOtherRunID
					return a
				}(),
			},
			want: action{kind: actPark, reason: ReasonApproval},
		},
		{
			name: "two waiting steps, one pending and one approved: the approved one runs",
			steps: planJournal(
				planTool(2, 1, "lookup", StepWaiting),
				planTool(3, 1, "lookup", StepWaiting),
			),
			approvals: []Approval{planApproval(2, 0, ApprovalPending), planApproval(3, 0, ApprovalApproved)},
			want:      action{kind: actRun, seq: 3},
		},
		{
			name: "a step waiting on a person does not hold up a proposed step after it",
			steps: planJournal(
				planTool(2, 1, "lookup", StepWaiting),
				planTool(3, 1, "lookup", StepProposed),
			),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			want:      action{kind: actJudge, seq: 3},
		},
		{
			name: "a step waiting on a child does not hold up a proposed step after it",
			steps: planJournal(
				planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID)),
				planTool(3, 1, "helper", StepProposed),
			),
			children: planChildren(planChild(planChildID, StatusRunnable)),
			want:     action{kind: actJudge, seq: 3},
		},
		{
			name: "steps are taken in order: a started step before a proposed one",
			steps: planJournal(
				planTool(2, 1, "lookup", StepStarted, planAttempts(1)),
				planTool(3, 1, "lookup", StepProposed),
			),
			want: action{kind: actRun, seq: 2},
		},
		{
			name:     "a waiting step whose child completed: collected",
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusCompleted)),
			want:     action{kind: actCollect, seq: 2},
		},
		{
			name:     "a waiting step whose child failed: collected",
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusFailed)),
			want:     action{kind: actCollect, seq: 2},
		},
		{
			name:     "a waiting step whose child was cancelled: collected",
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusCancelled)),
			want:     action{kind: actCollect, seq: 2},
		},
		{
			name: "two waiting steps, one child running and one ended: the ended one is collected",
			steps: planJournal(
				planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID)),
				planTool(3, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID2)),
			),
			children: planChildren(planChild(planChildID, StatusRunnable), planChild(planChildID2, StatusCompleted)),
			want:     action{kind: actCollect, seq: 3},
		},
		{
			name:      "a delegating step that was approved and now waits on its child is not run again",
			steps:     planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			approvals: []Approval{planApproval(2, 0, ApprovalApproved)},
			children:  planChildren(planChild(planChildID, StatusRunnable)),
			want:      action{kind: actPark, reason: ReasonChildren},
		},
		{
			name: "every call final: the model is called with the results",
			steps: planJournal(
				planTool(2, 1, "lookup", StepCompleted),
				planTool(3, 1, "lookup", StepBlocked),
				planTool(4, 1, "lookup", StepDeclined),
			),
			want: action{kind: actModel, seq: 5},
		},
	})
}

// Rule 2's end: nothing can proceed, and the reason says who is waited for.
func TestNext_Parks(t *testing.T) {
	checkPlan(t, []planCase{
		{
			name:      "everything left waits on a person",
			steps:     planJournal(planTool(2, 1, "lookup", StepCompleted), planTool(3, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(3, 0, ApprovalPending)},
			want:      action{kind: actPark, reason: ReasonApproval},
		},
		{
			name:     "everything left waits on a child that is runnable",
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusRunnable)),
			want:     action{kind: actPark, reason: ReasonChildren},
		},
		{
			name:     "everything left waits on a child that is itself waiting",
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusWaiting)),
			want:     action{kind: actPark, reason: ReasonChildren},
		},
		{
			name: "a person and then a child: the reason is the person",
			steps: planJournal(
				planTool(2, 1, "lookup", StepWaiting),
				planTool(3, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID)),
			),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			children:  planChildren(planChild(planChildID, StatusRunnable)),
			want:      action{kind: actPark, reason: ReasonApproval},
		},
		{
			name: "a child and then a person: the reason is the person",
			steps: planJournal(
				planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID)),
				planTool(3, 1, "lookup", StepWaiting),
			),
			approvals: []Approval{planApproval(3, 0, ApprovalPending)},
			children:  planChildren(planChild(planChildID, StatusRunnable)),
			want:      action{kind: actPark, reason: ReasonApproval},
		},
		{
			name:      "a budget spent does not stop a run from parking",
			run:       planRun(func(run *Run) { run.ActiveMillis = (15 * time.Minute).Milliseconds() }),
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			want:      action{kind: actPark, reason: ReasonApproval},
		},
	})
}

// Rule 3: no tool step is open, and the last model step says what is next.
func TestNext_LastModelStep(t *testing.T) {
	checkPlan(t, []planCase{
		{
			name: "an empty journal: the model is called at step 1",
			want: action{kind: actModel, seq: 1},
		},
		{
			name:  "a started model step: the call is made again at its step",
			steps: []Step{planModelStarted(1)},
			want:  action{kind: actModel, seq: 1},
		},
		{
			name:  "a started model step after a turn: the call is made again at its step",
			steps: append(planJournal(planTool(2, 1, "lookup", StepCompleted)), planModelStarted(3)),
			want:  action{kind: actModel, seq: 3},
		},
		{
			name:  "a reply that made calls, all final: the model is called at the next step",
			steps: planJournal(planTool(2, 1, "lookup", StepCompleted), planTool(3, 1, "lookup", StepCompleted)),
			want:  action{kind: actModel, seq: 4},
		},
		{
			name:  "a final reply that ended: the run completes with its text",
			steps: []Step{planReply(1, StopEnd, "the total is 42")},
			want:  action{kind: actFinish, status: StatusCompleted, output: "the total is 42"},
		},
		{
			name: "a final reply after a turn: the run completes with the last reply's text",
			steps: append(planJournal(planTool(2, 1, "lookup", StepCompleted)),
				planReply(3, StopEnd, "the total is 42")),
			want: action{kind: actFinish, status: StatusCompleted, output: "the total is 42"},
		},
		{
			name:  "a final reply with no text: the run completes with no output",
			steps: []Step{planReply(1, StopEnd, "")},
			want:  action{kind: actFinish, status: StatusCompleted},
		},
		{
			name:  "a reply that paused: the model is called at the next step",
			steps: []Step{planReply(1, StopPause, "working")},
			want:  action{kind: actModel, seq: 2},
		},
		{
			name:  "a reply cut off at its token limit: the run fails as truncated",
			steps: []Step{planReply(1, StopMaxTokens, "the total is")},
			want:  action{kind: actFinish, status: StatusFailed, reason: ReasonTruncated},
		},
		{
			name:  "a reply that filled the context window: the run fails",
			steps: []Step{planReply(1, StopContextWindow, "")},
			want:  action{kind: actFinish, status: StatusFailed, reason: ReasonContextWindow},
		},
		{
			name:  "a refusal: the run fails",
			steps: []Step{planReply(1, StopRefusal, "")},
			want:  action{kind: actFinish, status: StatusFailed, reason: ReasonRefusal},
		},
		{
			name:  "a reply that says it used tools and made no calls: the run is given up, not ended",
			steps: []Step{planReply(1, StopToolUse, "")},
			want: action{
				kind:   actYield,
				errmsg: `model reply at step 1 ended with stop "tool_use" and made no calls`,
			},
		},
		{
			name: "a reply that made calls is a turn whatever its stop: ended",
			steps: []Step{
				planReply(1, StopEnd, "one moment", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(2, 1, "lookup", StepCompleted),
			},
			want: action{kind: actModel, seq: 3},
		},
		{
			name: "a reply that made calls is a turn whatever its stop: cut off",
			steps: []Step{
				planReply(1, StopMaxTokens, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(2, 1, "lookup", StepProposed),
			},
			want: action{kind: actJudge, seq: 2},
		},
		{
			name: "a reply stored without its message: the run completes with no output",
			steps: []Step{func() Step {
				st := planReply(1, StopEnd, "")
				st.Message = nil
				return st
			}()},
			want: action{kind: actFinish, status: StatusCompleted},
		},
	})
}

// The rule that sits over the others: an action that would do work is
// replaced by a failure when a budget is spent (6.9), and no other is.
func TestNext_Budgets(t *testing.T) {
	quarter := (15 * time.Minute).Milliseconds()
	timeSpent := func(run *Run) { run.ActiveMillis = quarter }
	failed := func(reason string) action {
		return action{kind: actFinish, status: StatusFailed, reason: reason}
	}
	proposed := planJournal(planTool(2, 1, "lookup", StepProposed))

	checkPlan(t, []planCase{
		// Each budget, with work outstanding.
		{
			name:  "time spent, a call to judge: fails",
			run:   planRun(timeSpent),
			steps: proposed,
			want:  failed(ReasonTimeBudget),
		},
		{
			name: "cost spent, a call to judge: fails",
			run: planRun(planLimits(Limits{MaxCostMicros: 1000}), func(run *Run) {
				run.Usage.CostMicros = 1000
			}),
			steps: proposed,
			want:  failed(ReasonCostBudget),
		},
		{
			name: "tokens spent, a call to judge: fails",
			run: planRun(planLimits(Limits{MaxTokens: 100}), func(run *Run) {
				run.Usage = Usage{InputTokens: 60, OutputTokens: 40}
			}),
			steps: proposed,
			want:  failed(ReasonTokenBudget),
		},
		{
			name: "model calls spent, the model to call: fails",
			run:  planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: planJournal(
				planTool(2, 1, "lookup", StepCompleted),
			),
			want: failed(ReasonModelCalls),
		},

		// Each action that does work.
		{
			name: "time spent, the model to call on an empty journal: fails",
			run:  planRun(timeSpent),
			want: failed(ReasonTimeBudget),
		},
		{
			name:  "time spent, the model to call again: fails",
			run:   planRun(timeSpent),
			steps: []Step{planModelStarted(1)},
			want:  failed(ReasonTimeBudget),
		},
		{
			name:  "time spent, the model to call after a pause: fails",
			run:   planRun(timeSpent),
			steps: []Step{planReply(1, StopPause, "working")},
			want:  failed(ReasonTimeBudget),
		},
		{
			name:  "time spent, the model to call with results: fails",
			run:   planRun(timeSpent),
			steps: planJournal(planTool(2, 1, "lookup", StepCompleted)),
			want:  failed(ReasonTimeBudget),
		},
		{
			name:  "time spent, a started tool to run: fails",
			run:   planRun(timeSpent),
			steps: planJournal(planTool(2, 1, "lookup", StepStarted, planAttempts(1))),
			want:  failed(ReasonTimeBudget),
		},
		{
			name:      "time spent, an approved tool to run: fails",
			run:       planRun(timeSpent),
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalApproved)},
			want:      failed(ReasonTimeBudget),
		},
		{
			name:      "time spent, an at-most-once tool approved for its attempt to run: fails",
			run:       planRun(timeSpent),
			steps:     planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(1))),
			approvals: []Approval{planApproval(2, 1, ApprovalApproved)},
			want:      failed(ReasonTimeBudget),
		},
		{
			name:  "time spent, a child to start: fails",
			run:   planRun(timeSpent),
			steps: planJournal(planTool(2, 1, "helper", StepStarted, planAttempts(1))),
			want:  failed(ReasonTimeBudget),
		},
		{
			name: "time spent, a pending step and then a call to judge: fails rather than parks",
			run:  planRun(timeSpent),
			steps: planJournal(
				planTool(2, 1, "lookup", StepWaiting),
				planTool(3, 1, "lookup", StepProposed),
			),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			want:      failed(ReasonTimeBudget),
		},

		// Each action that does none.
		{
			name:  "time spent, the work already done: completes",
			run:   planRun(timeSpent),
			steps: []Step{planReply(1, StopEnd, "the total is 42")},
			want:  action{kind: actFinish, status: StatusCompleted, output: "the total is 42"},
		},
		{
			name: "every budget spent, the work already done: completes",
			run: planRun(planLimits(Limits{MaxDuration: time.Minute, MaxCostMicros: 1, MaxTokens: 1, MaxModelCalls: 1}),
				func(run *Run) {
					run.ActiveMillis = quarter
					run.Usage = Usage{InputTokens: 60, OutputTokens: 40, CostMicros: 1000}
					run.ModelCalls = 1
				}),
			steps: []Step{planReply(1, StopEnd, "the total is 42")},
			want:  action{kind: actFinish, status: StatusCompleted, output: "the total is 42"},
		},
		{
			name:     "time spent, a child to collect: collects",
			run:      planRun(timeSpent),
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID, StatusCompleted)),
			want:     action{kind: actCollect, seq: 2},
		},
		{
			name:      "time spent, a no to record: records it",
			run:       planRun(timeSpent),
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalDeclined)},
			want:      action{kind: actResolve, seq: 2},
		},
		{
			name:  "time spent, an interrupted at-most-once call: asks",
			run:   planRun(timeSpent),
			steps: planJournal(planTool(2, 1, "charge", StepStarted, planAttempts(1))),
			want:  action{kind: actAsk, seq: 2},
		},
		{
			name:  "time spent, a truncated reply: fails as truncated",
			run:   planRun(timeSpent),
			steps: []Step{planReply(1, StopMaxTokens, "")},
			want:  failed(ReasonTruncated),
		},

		// The limit on model calls is checked before a model call and nothing
		// else.
		{
			name:  "model calls spent, a call to judge: judges",
			run:   planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name:  "model calls spent, a started tool to run: runs",
			run:   planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: planJournal(planTool(2, 1, "lookup", StepStarted, planAttempts(1))),
			want:  action{kind: actRun, seq: 2},
		},
		{
			name:  "model calls spent, a child to start: starts it",
			run:   planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: planJournal(planTool(2, 1, "helper", StepStarted, planAttempts(1))),
			want:  action{kind: actSpawn, seq: 2},
		},
		{
			name: "model calls spent on an empty journal: fails",
			run: planRun(planLimits(Limits{MaxModelCalls: 1}), func(run *Run) {
				run.ModelCalls = 1
			}),
			want: failed(ReasonModelCalls),
		},
		{
			name:  "model calls spent, a paused reply: fails rather than calling for ever",
			run:   planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: []Step{planReply(1, StopPause, "working")},
			want:  failed(ReasonModelCalls),
		},
		{
			name:  "model calls spent, a model call interrupted: fails",
			run:   planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: []Step{planModelStarted(1)},
			want:  failed(ReasonModelCalls),
		},
		{
			name:  "model calls spent, the work already done: completes",
			run:   planRun(func(run *Run) { run.ModelCalls = 50 }),
			steps: []Step{planReply(1, StopEnd, "the total is 42")},
			want:  action{kind: actFinish, status: StatusCompleted, output: "the total is 42"},
		},

		// A budget is spent when it is reached, not when it is passed.
		{
			name:  "time one millisecond short of the limit: not spent",
			run:   planRun(func(run *Run) { run.ActiveMillis = quarter - 1 }),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name:  "time past the limit: spent",
			run:   planRun(func(run *Run) { run.ActiveMillis = quarter + 1 }),
			steps: proposed,
			want:  failed(ReasonTimeBudget),
		},
		{
			name: "cost one short of the limit: not spent",
			run: planRun(planLimits(Limits{MaxCostMicros: 1000}), func(run *Run) {
				run.Usage.CostMicros = 999
			}),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name: "cost past the limit: spent",
			run: planRun(planLimits(Limits{MaxCostMicros: 1000}), func(run *Run) {
				run.Usage.CostMicros = 1001
			}),
			steps: proposed,
			want:  failed(ReasonCostBudget),
		},
		{
			name: "tokens one short of the limit: not spent",
			run: planRun(planLimits(Limits{MaxTokens: 100}), func(run *Run) {
				run.Usage = Usage{InputTokens: 60, OutputTokens: 39}
			}),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name: "tokens are input plus output: input alone at the limit is spent",
			run: planRun(planLimits(Limits{MaxTokens: 100}), func(run *Run) {
				run.Usage = Usage{InputTokens: 100}
			}),
			steps: proposed,
			want:  failed(ReasonTokenBudget),
		},
		{
			name: "tokens are input plus output: output alone at the limit is spent",
			run: planRun(planLimits(Limits{MaxTokens: 100}), func(run *Run) {
				run.Usage = Usage{OutputTokens: 100}
			}),
			steps: proposed,
			want:  failed(ReasonTokenBudget),
		},
		{
			name: "model calls one short of the limit: not spent",
			run:  planRun(func(run *Run) { run.ModelCalls = 49 }),
			want: action{kind: actModel, seq: 1},
		},
		{
			name: "model calls past the limit: spent",
			run:  planRun(func(run *Run) { run.ModelCalls = 51 }),
			want: failed(ReasonModelCalls),
		},

		// A negative limit is no limit.
		{
			name: "no limit on time",
			run: planRun(planLimits(Limits{MaxDuration: -1}), func(run *Run) {
				run.ActiveMillis = 1000 * quarter
			}),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name: "no limit on cost",
			run: planRun(planLimits(Limits{MaxCostMicros: -1}), func(run *Run) {
				run.Usage.CostMicros = 1 << 40
			}),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name: "no limit on tokens",
			run: planRun(planLimits(Limits{MaxTokens: -1}), func(run *Run) {
				run.Usage = Usage{InputTokens: 1 << 40, OutputTokens: 1 << 40}
			}),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name: "no limit on model calls",
			run: planRun(planLimits(Limits{MaxModelCalls: -1}), func(run *Run) {
				run.ModelCalls = 1 << 20
			}),
			want: action{kind: actModel, seq: 1},
		},

		// A snapshot whose limits were never filled is read as Limits says: a
		// zero field takes its default.
		{
			name: "limits left zero: time takes its default of fifteen minutes",
			run: planRun(planLimits(Limits{}), func(run *Run) {
				run.ActiveMillis = quarter
			}),
			steps: proposed,
			want:  failed(ReasonTimeBudget),
		},
		{
			name: "limits left zero: time short of the default is not spent",
			run: planRun(planLimits(Limits{}), func(run *Run) {
				run.ActiveMillis = quarter - 1
			}),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},
		{
			name: "limits left zero: model calls take their default of fifty",
			run: planRun(planLimits(Limits{}), func(run *Run) {
				run.ModelCalls = 50
			}),
			want: failed(ReasonModelCalls),
		},
		{
			name: "limits left zero: model calls short of the default are not spent",
			run: planRun(planLimits(Limits{}), func(run *Run) {
				run.ModelCalls = 49
			}),
			want: action{kind: actModel, seq: 1},
		},
		{
			name: "limits left zero: cost and tokens have no limit",
			run: planRun(planLimits(Limits{}), func(run *Run) {
				run.Usage = Usage{InputTokens: 1 << 40, OutputTokens: 1 << 40, CostMicros: 1 << 40}
			}),
			steps: proposed,
			want:  action{kind: actJudge, seq: 2},
		},

		// More than one spent: the reason is the first of time, cost, tokens
		// and model calls.
		{
			name: "time and cost spent: time",
			run: planRun(planLimits(Limits{MaxCostMicros: 1000}), func(run *Run) {
				run.ActiveMillis = quarter
				run.Usage.CostMicros = 1000
			}),
			steps: proposed,
			want:  failed(ReasonTimeBudget),
		},
		{
			name: "cost and tokens spent: cost",
			run: planRun(planLimits(Limits{MaxCostMicros: 1000, MaxTokens: 100}), func(run *Run) {
				run.Usage = Usage{InputTokens: 100, CostMicros: 1000}
			}),
			steps: proposed,
			want:  failed(ReasonCostBudget),
		},
		{
			name: "tokens and model calls spent: tokens",
			run: planRun(planLimits(Limits{MaxTokens: 100}), func(run *Run) {
				run.Usage = Usage{InputTokens: 100}
				run.ModelCalls = 50
			}),
			want: failed(ReasonTokenBudget),
		},
	})
}

// Journals this build cannot make sense of. For each the run is given up as
// a failed attempt, with what was found, and is not ended: ending a run
// cannot be undone, and what this build cannot read may be a newer build's
// writing, or the executor's mistake. The alternatives next must not take
// are a panic in a worker and a run that parks with nothing to wake it.
func TestNext_AJournalThatDoesNotAddUp(t *testing.T) {
	broken := func(errmsg string) action {
		return action{kind: actYield, errmsg: errmsg}
	}
	checkPlan(t, []planCase{
		{
			name: "a step kind this build does not know: yield, not fail",
			steps: append(planJournal(planTool(2, 1, "lookup", StepCompleted)),
				Step{RunID: planRunID, Seq: 3, Kind: StepKind("summary"), Status: StepCompleted}),
			want: broken(`step 3 has the kind "summary"`),
		},
		{
			name:  "a step status this build does not know: yield, not fail",
			steps: planJournal(planTool(2, 1, "lookup", StepStatus("paused"))),
			want:  broken(`step 2 has the status "paused"`),
		},
		{
			name:      "an approval status this build does not know: yield, and not a no",
			steps:     planJournal(planTool(2, 1, "lookup", StepWaiting)),
			approvals: []Approval{planApproval(2, 0, ApprovalStatus("escalated"))},
			want:      broken(`step 2's approval has the status "escalated"`),
		},
		{
			name:  "a stop this build does not know: yield, not fail",
			steps: []Step{planReply(1, Stop("length"), "the total is")},
			want:  broken(`model reply at step 1 ended with stop "length" and made no calls`),
		},
		{
			name: "a spent budget does not turn giving up into failing",
			run: planRun(func(run *Run) {
				run.ActiveMillis = (15 * time.Minute).Milliseconds()
				run.ModelCalls = 50
			}),
			steps: planJournal(planTool(2, 1, "lookup", StepStatus("paused"))),
			want:  broken(`step 2 has the status "paused"`),
		},
		{
			name:  "cancellation still ends a run whose journal does not add up",
			run:   planRun(planCancelRequested),
			steps: planJournal(planTool(2, 1, "lookup", StepStatus("paused"))),
			want:  action{kind: actFinish, status: StatusCancelled, reason: ReasonCancelled},
		},
		{
			name:  "a waiting step with no approval and no child",
			steps: planJournal(planTool(2, 1, "lookup", StepWaiting)),
			want:  broken("step 2 is waiting with no approval and no child run"),
		},
		{
			name:     "a waiting step whose child is not among the run's children",
			steps:    planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			children: planChildren(planChild(planChildID2, StatusCompleted)),
			want:     broken("step 2 waits on child run " + planChildID + ", which is not among the run's children"),
		},
		{
			name:  "a waiting step whose child is not there, with no children at all",
			steps: planJournal(planTool(2, 1, "helper", StepWaiting, planAttempts(1), planChildOf(planChildID))),
			want:  broken("step 2 waits on child run " + planChildID + ", which is not among the run's children"),
		},
		{
			name: "a last step of a kind this build does not know, still started",
			steps: append(planJournal(planTool(2, 1, "lookup", StepCompleted)),
				Step{RunID: planRunID, Seq: 3, Kind: StepKind("summary"), Status: StepStarted}),
			want: broken(`step 3 has the kind "summary"`),
		},
		{
			name: "it does not hide behind a step that is pending",
			steps: planJournal(
				planTool(2, 1, "lookup", StepWaiting),
				planTool(3, 1, "lookup", StepWaiting),
			),
			approvals: []Approval{planApproval(2, 0, ApprovalPending)},
			want:      broken("step 3 is waiting with no approval and no child run"),
		},
	})
}

// Whatever a journal holds, next answers with an action an executor can
// carry out or with the run's end, and neither it nor conversation panics: a
// planner that panicked on a journal it did not expect would take its worker
// down with it, and every worker after that one.
func TestNext_AnyJournalGetsAnAction(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 2026))
	pick := func(n int) int { return rng.IntN(n) }
	kinds := []StepKind{StepModel, StepTool, StepKind("note")}
	statuses := []StepStatus{
		StepProposed, StepWaiting, StepStarted, StepCompleted, StepBlocked, StepDeclined, StepStatus("paused"),
	}
	stops := []Stop{"", StopEnd, StopToolUse, StopMaxTokens, StopRefusal, StopPause, StopContextWindow, Stop("length")}
	answers := []ApprovalStatus{
		ApprovalPending, ApprovalApproved, ApprovalDeclined, ApprovalExpired, ApprovalCancelled, ApprovalStatus("escalated"),
	}
	ends := []Status{StatusRunnable, StatusWaiting, StatusCompleted, StatusFailed, StatusCancelled}
	tools := []string{"lookup", "charge", "helper", "courier", "gone", ""}
	childIDs := []string{"", planChildID, planChildID2}

	for range 5000 {
		steps := make([]Step, pick(7))
		for i := range steps {
			st := Step{
				RunID:      planRunID,
				Seq:        i + 1,
				Kind:       kinds[pick(len(kinds))],
				Status:     statuses[pick(len(statuses))],
				Name:       tools[pick(len(tools))],
				Stop:       stops[pick(len(stops))],
				Turn:       pick(i + 1),
				Attempts:   pick(3),
				ChildRunID: childIDs[pick(len(childIDs))],
			}
			if pick(4) > 0 {
				st.Message = &Message{Role: RoleAssistant, Text: "text"}
			}
			if pick(4) > 0 {
				st.Call = &Call{ID: "call-" + strconv.Itoa(i+1), Name: st.Name}
			}
			steps[i] = st
		}
		approvals := make([]Approval, pick(4))
		for i := range approvals {
			approvals[i] = planApproval(1+pick(7), pick(3), answers[pick(len(answers))])
		}
		children := map[string]Run{}
		for _, id := range childIDs[1:] {
			if pick(2) == 0 {
				children[id] = planChild(id, ends[pick(len(ends))])
			}
		}
		run := planRun(func(run *Run) {
			run.CancelRequested = pick(10) == 0
			run.ModelCalls = pick(60)
			run.ActiveMillis = int64(pick(20)) * time.Minute.Milliseconds()
		})

		a := next(run, planDefinition(), steps, approvals, children)

		open := func(seq int) bool {
			return seq >= 1 && seq <= len(steps) && steps[seq-1].Kind == StepTool && !steps[seq-1].Status.Done()
		}
		switch a.kind {
		case actFinish:
			require.True(t, Run{Status: a.status}.Terminal(), "finish with status %q for %+v", a.status, steps)
			require.NotEqual(t, ReasonError, a.reason, "next ended a run for what it could not read: %+v", steps)
			require.Empty(t, a.errmsg)
		case actModel:
			started := len(steps) > 0 && a.seq == len(steps) && steps[a.seq-1].Kind == StepModel
			require.True(t, started || a.seq == len(steps)+1, "model call at step %d for %+v", a.seq, steps)
		case actJudge, actRun, actSpawn, actCollect, actResolve, actAsk:
			require.True(t, open(a.seq), "action %d on step %d, which is not an open tool step, for %+v", a.kind, a.seq, steps)
		case actPark:
			require.Contains(t, []string{ReasonApproval, ReasonChildren}, a.reason)
		case actYield:
			require.NotEmpty(t, a.errmsg, "the run was given up with nothing said for %+v", steps)
		default:
			require.Fail(t, "an action of no kind", "%+v for %+v", a, steps)
		}
		require.NotEmpty(t, conversation(run, steps))
	}
}

// The golden of the design's 6.15: a blocked, a declined and a completed call
// in one reply, and the reply that followed.
func TestConversation_Golden(t *testing.T) {
	first := Message{
		Role: RoleAssistant,
		Text: "three things to do",
		Calls: []Call{
			{ID: "call-a", Name: "charge", Input: json.RawMessage(`{"amount":900}`)},
			{ID: "call-b", Name: "charge", Input: json.RawMessage(`{"amount":40}`)},
			{ID: "call-c", Name: "lookup", Input: json.RawMessage(`{"z":1,"a":2}`)},
		},
		Opaque: &Opaque{Provider: "scripted", Data: json.RawMessage(`{"z":1,"a":[{"signature":"s1"}]}`)},
	}
	last := Message{Role: RoleAssistant, Text: "the total is 42"}
	tool := func(seq int, status StepStatus, result string, isError bool) Step {
		return Step{
			RunID: planRunID, Seq: seq, Kind: StepTool, Status: status, Turn: 1,
			Name: first.Calls[seq-2].Name, Call: &first.Calls[seq-2], Key: StepKey(planRunID, seq),
			Result: result, IsError: isError,
		}
	}
	steps := []Step{
		{RunID: planRunID, Seq: 1, Kind: StepModel, Status: StepCompleted, Message: &first, Stop: StopToolUse},
		tool(2, StepBlocked, "blocked by policy: no charge over 500", true),
		tool(3, StepDeclined, "declined by ops@example.test: not today", true),
		tool(4, StepCompleted, `{"total":42}`, false),
		{RunID: planRunID, Seq: 5, Kind: StepModel, Status: StepCompleted, Message: &last, Stop: StopEnd},
	}

	got := conversation(planRun(), steps)

	assert.Equal(t, []Message{
		{Role: RoleUser, Text: "what is the total?"},
		first,
		{Role: RoleTool, Results: []Result{
			{CallID: "call-a", Content: "blocked by policy: no charge over 500", IsError: true},
			{CallID: "call-b", Content: "declined by ops@example.test: not today", IsError: true},
			{CallID: "call-c", Content: `{"total":42}`},
		}},
		last,
	}, got)
}

func TestConversation(t *testing.T) {
	user := Message{Role: RoleUser, Text: "what is the total?"}
	results := func(rs ...Result) Message { return Message{Role: RoleTool, Results: rs} }
	answered := func(seq int, status StepStatus, result string, isError bool) Step {
		return planTool(seq, 1, "lookup", status, planResult(result, isError))
	}

	cases := []struct {
		name  string
		steps []Step
		want  func(steps []Step) []Message
	}{
		{
			name: "an empty journal is the run's input",
			want: func([]Step) []Message { return []Message{user} },
		},
		{
			name:  "a model step that has only started says nothing",
			steps: []Step{planModelStarted(1)},
			want:  func([]Step) []Message { return []Message{user} },
		},
		{
			name: "a model step that has only started says nothing, whatever it holds",
			steps: []Step{func() Step {
				st := planModelStarted(1)
				st.Message = &Message{Role: RoleAssistant, Text: "half a reply"}
				return st
			}()},
			want: func([]Step) []Message { return []Message{user} },
		},
		{
			name: "a tool step is not a reply, whatever it holds",
			steps: planJournal(func() Step {
				st := answered(2, StepCompleted, "first", false)
				st.Message = &Message{Role: RoleAssistant, Text: "not a reply"}
				return st
			}()),
			want: func(steps []Step) []Message {
				return []Message{user, *steps[0].Message, results(Result{CallID: "call-2", Content: "first"})}
			},
		},
		{
			name:  "a reply with no calls gets no tool message",
			steps: []Step{planReply(1, StopPause, "working"), planReply(2, StopEnd, "the total is 42")},
			want: func(steps []Step) []Message {
				return []Message{user, *steps[0].Message, *steps[1].Message}
			},
		},
		{
			name:  "a reply whose calls are all proposed gets no tool message",
			steps: planJournal(planTool(2, 1, "lookup", StepProposed), planTool(3, 1, "lookup", StepProposed)),
			want:  func(steps []Step) []Message { return []Message{user, *steps[0].Message} },
		},
		{
			name: "a reply with one call final and one proposed gets no tool message",
			steps: planJournal(
				answered(2, StepCompleted, "first", false),
				planTool(3, 1, "lookup", StepProposed),
			),
			want: func(steps []Step) []Message { return []Message{user, *steps[0].Message} },
		},
		{
			name: "a reply with its first call open and its second final gets no tool message",
			steps: planJournal(
				planTool(2, 1, "lookup", StepWaiting),
				answered(3, StepCompleted, "second", false),
			),
			want: func(steps []Step) []Message { return []Message{user, *steps[0].Message} },
		},
		{
			name: "a reply with a call started gets no tool message",
			steps: planJournal(
				answered(2, StepCompleted, "first", false),
				planTool(3, 1, "lookup", StepStarted, planAttempts(1)),
			),
			want: func(steps []Step) []Message { return []Message{user, *steps[0].Message} },
		},
		{
			name: "a reply with a call waiting gets no tool message",
			steps: planJournal(
				answered(2, StepCompleted, "first", false),
				planTool(3, 1, "lookup", StepWaiting),
			),
			want: func(steps []Step) []Message { return []Message{user, *steps[0].Message} },
		},
		{
			name: "a reply with every call final gets one tool message, results in the order of the calls",
			steps: planJournal(
				answered(2, StepCompleted, "first", false),
				answered(3, StepCompleted, "second", false),
			),
			want: func(steps []Step) []Message {
				return []Message{user, *steps[0].Message, results(
					Result{CallID: "call-2", Content: "first"},
					Result{CallID: "call-3", Content: "second"},
				)}
			},
		},
		{
			name: "the fixed result texts appear as stored",
			steps: planJournal(
				answered(2, StepBlocked, "blocked by policy: no-refunds", true),
				answered(3, StepDeclined, "declined by ops@example.test: wrong account", true),
				answered(4, StepDeclined, "declined: approval expired", true),
				answered(5, StepDeclined, "interrupted before its result was recorded; not run again", true),
				answered(6, StepCompleted, "arguments were not valid JSON", true),
				answered(7, StepCompleted, "tool is not available", true),
				answered(8, StepCompleted, "connection refused", true),
				answered(9, StepCompleted, "tool panicked", true),
				answered(10, StepCompleted, "timed out after 2m0s", true),
				answered(11, StepCompleted, "result too large: 1048577 bytes", true),
				answered(12, StepCompleted, "assistant failed: time_budget", true),
			),
			want: func(steps []Step) []Message {
				return []Message{user, *steps[0].Message, results(
					Result{CallID: "call-2", Content: "blocked by policy: no-refunds", IsError: true},
					Result{CallID: "call-3", Content: "declined by ops@example.test: wrong account", IsError: true},
					Result{CallID: "call-4", Content: "declined: approval expired", IsError: true},
					Result{CallID: "call-5", Content: "interrupted before its result was recorded; not run again", IsError: true},
					Result{CallID: "call-6", Content: "arguments were not valid JSON", IsError: true},
					Result{CallID: "call-7", Content: "tool is not available", IsError: true},
					Result{CallID: "call-8", Content: "connection refused", IsError: true},
					Result{CallID: "call-9", Content: "tool panicked", IsError: true},
					Result{CallID: "call-10", Content: "timed out after 2m0s", IsError: true},
					Result{CallID: "call-11", Content: "result too large: 1048577 bytes", IsError: true},
					Result{CallID: "call-12", Content: "assistant failed: time_budget", IsError: true},
				)}
			},
		},
		{
			name:  "a result that is the empty string is a result",
			steps: planJournal(answered(2, StepCompleted, "", false)),
			want: func(steps []Step) []Message {
				return []Message{user, *steps[0].Message, results(Result{CallID: "call-2"})}
			},
		},
		{
			name: "each turn's results follow its own reply",
			steps: []Step{
				planReply(1, StopToolUse, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				answered(2, StepCompleted, "first", false),
				planReply(3, StopToolUse, "", Call{ID: "call-4", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(4, 3, "lookup", StepCompleted, planResult("second", false)),
				planReply(5, StopEnd, "the total is 42"),
			},
			want: func(steps []Step) []Message {
				return []Message{
					user,
					*steps[0].Message, results(Result{CallID: "call-2", Content: "first"}),
					*steps[2].Message, results(Result{CallID: "call-4", Content: "second"}),
					*steps[4].Message,
				}
			},
		},
		{
			name: "a later turn that is still open does not hide an earlier turn's results",
			steps: []Step{
				planReply(1, StopToolUse, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				answered(2, StepCompleted, "first", false),
				planReply(3, StopToolUse, "", Call{ID: "call-4", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(4, 3, "lookup", StepProposed),
			},
			want: func(steps []Step) []Message {
				return []Message{
					user,
					*steps[0].Message, results(Result{CallID: "call-2", Content: "first"}),
					*steps[2].Message,
				}
			},
		},
		{
			name: "a refusal that came with calls is sent as it was stored, with no tool message",
			steps: []Step{
				planReply(1, StopRefusal, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				planTool(2, 1, "lookup", StepProposed),
			},
			want: func(steps []Step) []Message { return []Message{user, *steps[0].Message} },
		},
		{
			name: "a reply stored without its message is passed over, and so are its results",
			steps: []Step{
				func() Step {
					st := planReply(1, StopToolUse, "")
					st.Message = nil
					return st
				}(),
				answered(2, StepCompleted, "first", false),
			},
			want: func([]Step) []Message { return []Message{user} },
		},
		{
			name: "a tool step stored without its call still gives its result",
			steps: []Step{
				planReply(1, StopToolUse, "", Call{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{}`)}),
				func() Step {
					st := answered(2, StepCompleted, "first", false)
					st.Call = nil
					return st
				}(),
			},
			want: func(steps []Step) []Message {
				return []Message{user, *steps[0].Message, results(Result{Content: "first"})}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before, err := json.Marshal(tc.steps)
			require.NoError(t, err)

			got := conversation(planRun(), tc.steps)

			assert.Equal(t, tc.want(tc.steps), got)
			after, err := json.Marshal(tc.steps)
			require.NoError(t, err)
			assert.JSONEq(t, string(before), string(after), "conversation changed the journal")
		})
	}
}

// An assistant turn goes back to its provider exactly as it came: the
// provider's own form of it, and the calls as the model wrote them.
func TestConversation_SendsAReplyAsItWasStored(t *testing.T) {
	stored := Message{
		Role:   RoleAssistant,
		Text:   "one moment",
		Calls:  []Call{{ID: "call-2", Name: "lookup", Input: json.RawMessage(`{"z":1,"a":2}`)}},
		Opaque: &Opaque{Provider: "scripted", Data: json.RawMessage(`{"z":[1,2],"a":"signature"}`)},
	}
	steps := []Step{
		{RunID: planRunID, Seq: 1, Kind: StepModel, Status: StepCompleted, Message: &stored, Stop: StopToolUse},
		planTool(2, 1, "lookup", StepCompleted, planResult("done", false)),
	}

	got := conversation(planRun(), steps)

	require.Len(t, got, 3)
	assert.Equal(t, stored, got[1])
	assert.Equal(t, `{"z":[1,2],"a":"signature"}`, string(got[1].Opaque.Data))
	assert.Equal(t, `{"z":1,"a":2}`, string(got[1].Calls[0].Input))
}

// planSeen is what the grown journals covered, summed over every seed.
type planSeen struct {
	kinds     map[actionKind]int
	approvals map[ApprovalStatus]int
	ends      map[string]int
	// growths counts the journal writes after which the property was checked.
	growths int
	// refusedWithCalls counts refusals that came with calls, and childEnded
	// and childOpen the children next was shown in each state.
	refusedWithCalls, childEnded, childOpen int
	// eagerBroke counts the writes after which planEagerConversation did not
	// start with what it gave before.
	eagerBroke int
}

// planWorld drives one run on a MemoryStore the way an executor would: it
// asks next what to do and does it, choosing at random among the outcomes
// the action can have, a crash in the middle of it included. It is the
// journal's only writer, so a write the store refuses means next asked for
// something the journal does not allow.
type planWorld struct {
	t     *testing.T
	rng   *rand.Rand
	store *MemoryStore
	def   Definition
	seen  *planSeen
	now   time.Time
	lease Lease
	ids   int
	// last is the conversation the journal gave after its latest write, and
	// eager the same from planEagerConversation.
	last, eager []Message
}

func (w *planWorld) tick() time.Time {
	w.now = w.now.Add(time.Second)
	return w.now
}

func (w *planWorld) id() string {
	w.ids++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", w.ids)
}

func (w *planWorld) chance(percent int) bool { return w.rng.IntN(100) < percent }

// crashes reports whether the execution dies here, leaving the journal as
// the last write left it.
func (w *planWorld) crashes() bool { return w.chance(15) }

func (w *planWorld) read() (Run, []Step, []Approval, map[string]Run) {
	ctx := w.t.Context()
	changes, err := w.store.Changes(ctx, planRunID, 0)
	require.NoError(w.t, err)
	children, err := w.store.ListRuns(ctx, RunFilter{ParentID: planRunID, Limit: maxListLimit})
	require.NoError(w.t, err)
	return changes.Run, changes.Steps, changes.Approvals, planChildren(children...)
}

// grown checks the property after a write: the conversation the journal now
// gives starts with the one it gave before.
func (w *planWorld) grown() {
	run, steps, _, _ := w.read()

	now := conversation(run, steps)
	require.GreaterOrEqual(w.t, len(now), len(w.last), "the conversation got shorter")
	for i, said := range w.last {
		require.Equal(w.t, said, now[i], "the conversation changed message %d, which it had already sent", i)
	}
	w.last = now
	w.seen.growths++

	eager := planEagerConversation(run, steps)
	if !planStartsWith(eager, w.eager) {
		w.seen.eagerBroke++
	}
	w.eager = eager
}

// planStartsWith reports whether now begins with every message of before,
// unchanged.
func planStartsWith(now, before []Message) bool {
	if len(now) < len(before) {
		return false
	}
	for i, said := range before {
		if !assert.ObjectsAreEqual(said, now[i]) {
			return false
		}
	}
	return true
}

func (w *planWorld) update(req StepUpdate) {
	req.Now = w.tick()
	require.NoError(w.t, w.store.UpdateStep(w.t.Context(), w.lease, req))
	w.grown()
}

func (w *planWorld) ask(req ApprovalRequest) {
	req.ID = w.id()
	req.Now = w.tick()
	if w.chance(50) {
		expires := req.Now.Add(time.Minute)
		req.ExpiresAt = &expires
	}
	asked, err := w.store.RequestApproval(w.t.Context(), w.lease, req)
	require.NoError(w.t, err)
	require.Equal(w.t, ApprovalPending, asked.Status, "next asked about a step already asked about")
	w.grown()
}

// reply is the model's answer to the call at seq: mostly tool calls, and
// each way a reply can end.
func (w *planWorld) reply(run Run, seq int) (Message, Stop) {
	calls := func() []Call {
		tools := []string{"lookup", "charge", "helper", "courier", "gone"}
		out := make([]Call, 1+w.rng.IntN(3))
		for i := range out {
			out[i] = Call{
				ID:    fmt.Sprintf("call-%d-%d", seq, i),
				Name:  tools[w.rng.IntN(len(tools))],
				Input: json.RawMessage(`{"n":1}`),
			}
			if w.chance(5) {
				out[i].Input, out[i].Malformed = json.RawMessage(`"{not json"`), true
			}
		}
		return out
	}
	msg := Message{
		Role:   RoleAssistant,
		Opaque: &Opaque{Provider: "scripted", Data: json.RawMessage(fmt.Sprintf(`{"z":%d,"a":1}`, seq))},
	}
	if run.ModelCalls >= 8 {
		msg.Text = "the total is 42"
		return msg, StopEnd
	}
	switch roll := w.rng.IntN(100); {
	case roll < 72:
		msg.Calls = calls()
		return msg, StopToolUse
	case roll < 78:
		msg.Text = "the total is 42"
		return msg, StopEnd
	case roll < 88:
		msg.Text = "working"
		return msg, StopPause
	case roll < 91:
		return msg, StopRefusal
	case roll < 94:
		msg.Calls = calls()
		w.seen.refusedWithCalls++
		return msg, StopRefusal
	case roll < 97:
		msg.Text = "the total is"
		return msg, StopMaxTokens
	}
	return msg, StopContextWindow
}

// perform does what an executor does for a, and reports whether the run
// ended.
func (w *planWorld) perform(a action, run Run, steps []Step, children map[string]Run) bool {
	ctx := w.t.Context()
	switch a.kind {
	case actFinish:
		require.NoError(w.t, w.store.Finish(ctx, w.lease, FinishRequest{
			Status: a.status, Reason: a.reason, Output: a.output, Error: a.errmsg, Now: w.tick(),
		}))
		w.grown()
		return true

	case actModel:
		require.NoError(w.t, w.store.BeginModel(ctx, w.lease, a.seq, w.tick()))
		w.grown()
		if w.crashes() {
			return false
		}
		msg, stop := w.reply(run, a.seq)
		require.NoError(w.t, w.store.CompleteModel(ctx, w.lease, CompleteModelRequest{
			Seq: a.seq, Message: msg, Stop: stop, Model: "scripted",
			Usage: Usage{InputTokens: 40, OutputTokens: 10, CostMicros: 50}, Now: w.tick(),
		}))
		w.grown()

	case actJudge:
		w.judge(run, steps[a.seq-1])

	case actRun:
		st := steps[a.seq-1]
		w.update(StepUpdate{Seq: st.Seq, From: st.Status, To: StepStarted})
		if w.crashes() {
			return false
		}
		w.execute(run, st)

	case actSpawn:
		w.spawn(run, steps[a.seq-1])

	case actCollect:
		st := steps[a.seq-1]
		child := children[st.ChildRunID]
		result, isError := child.Output, false
		if child.Status != StatusCompleted {
			result, isError = fmt.Sprintf("%s %s: %s", child.Agent, child.Status, child.Reason), true
		}
		w.update(StepUpdate{
			Seq: st.Seq, From: StepWaiting, To: StepCompleted,
			Result: &result, IsError: isError, Usage: child.Usage,
		})

	case actResolve:
		result := "declined by ops@example.test: not today"
		w.update(StepUpdate{Seq: a.seq, From: StepWaiting, To: StepDeclined, Result: &result, IsError: true})

	case actAsk:
		w.ask(ApprovalRequest{Seq: a.seq, From: StepStarted, Cause: CauseInterrupted, Rule: RuleInterrupted})

	case actPark:
		parked, err := w.store.Park(ctx, w.lease, ParkRequest{Reason: a.reason, Now: w.tick()})
		require.NoError(w.t, err)
		require.True(w.t, parked, "next parked a run the store says has nothing to wait for")
		w.grown()
		w.wake()

	case actYield:
		require.FailNow(w.t, "next gave up on a journal the store wrote", "%s", a.errmsg)
	}
	return false
}

// judge settles a proposed step each way a guard and a build can.
func (w *planWorld) judge(run Run, st Step) {
	if _, known := toolOf(w.def, st.Name); st.Call.Malformed || !known {
		result := "tool is not available"
		if st.Call.Malformed {
			result = "arguments were not valid JSON"
		}
		w.update(StepUpdate{Seq: st.Seq, From: StepProposed, To: StepCompleted, Result: &result, IsError: true})
		return
	}
	switch roll := w.rng.IntN(100); {
	case roll < 15:
		result := "blocked by policy: no-charges"
		w.update(StepUpdate{
			Seq: st.Seq, From: StepProposed, To: StepBlocked,
			Decision: Block, Rule: "no-charges", Result: &result, IsError: true,
		})
	case roll < 45:
		w.ask(ApprovalRequest{Seq: st.Seq, From: StepProposed, Cause: CauseGuard, Decision: Ask, Rule: "ask-first"})
	default:
		w.update(StepUpdate{Seq: st.Seq, From: StepProposed, To: StepStarted, Decision: Allow, Rule: RuleNoGuard})
		if w.crashes() {
			return
		}
		w.execute(run, st)
	}
}

// execute carries out a step that has just started: the tool's result is
// recorded, or its child run started.
func (w *planWorld) execute(run Run, st Step) {
	if tool, _ := toolOf(w.def, st.Name); tool.Delegate != "" {
		w.spawn(run, st)
		return
	}
	result, isError := "result of "+st.Key, false
	if w.chance(20) {
		result, isError = "connection refused", true
	}
	w.update(StepUpdate{Seq: st.Seq, From: StepStarted, To: StepCompleted, Result: &result, IsError: isError})
}

func (w *planWorld) spawn(run Run, st Step) {
	now := w.tick()
	child, _, err := w.store.CreateRun(w.t.Context(), Run{
		ID: w.id(), Agent: "assistant", Status: StatusRunnable, Input: string(st.Call.Input),
		ParentID: run.ID, ParentSeq: st.Seq, Depth: run.Depth + 1, Key: st.Key,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(w.t, err)
	w.grown()
	if w.crashes() {
		return
	}
	w.update(StepUpdate{Seq: st.Seq, From: StepStarted, To: StepWaiting, ChildRunID: child.ID})
}

// wake is everyone else, acting on a parked run until it is runnable again:
// a person answers, an approval lapses, a child ends, someone cancels. Then
// the run is claimed, as the next execution would claim it.
func (w *planWorld) wake() {
	ctx := w.t.Context()
	for {
		run, steps, approvals, children := w.read()
		if run.Status != StatusWaiting {
			break
		}
		var moves []func()
		for _, a := range approvals {
			if a.Status != ApprovalPending {
				continue
			}
			moves = append(moves, func() {
				_, err := w.store.DecideApproval(ctx, DecideRequest{
					ID: a.ID, Approved: w.chance(60), By: "ops@example.test", Reason: "not today", Now: w.tick(),
				})
				require.NoError(w.t, err)
			})
			if a.ExpiresAt != nil {
				moves = append(moves, func() {
					if a.ExpiresAt.After(w.now) {
						w.now = *a.ExpiresAt
					}
					lapsed, err := w.store.ExpireApprovals(ctx, w.now)
					require.NoError(w.t, err)
					require.Positive(w.t, lapsed)
				})
			}
		}
		for _, st := range steps {
			child, ok := children[st.ChildRunID]
			if !ok || child.Terminal() {
				continue
			}
			moves = append(moves, func() { w.endChild(child) })
		}
		require.NotEmpty(w.t, moves, "the run is parked and nothing can wake it")
		if w.chance(4) {
			require.NoError(w.t, w.store.RequestCancel(ctx, CancelRequest{
				RunID: planRunID, By: "ops@example.test", Reason: "enough", Now: w.tick(),
			}))
		} else {
			moves[w.rng.IntN(len(moves))]()
		}
		w.grown()
	}

	claimed, err := w.store.Claim(ctx, ClaimRequest{
		Owner: "planner-test", Agents: []string{"planner"}, RunID: planRunID, Now: w.tick(), TTL: time.Minute,
	})
	require.NoError(w.t, err)
	w.lease = claimed.Lease()
}

func (w *planWorld) endChild(child Run) {
	ctx := w.t.Context()
	claimed, err := w.store.Claim(ctx, ClaimRequest{
		Owner: "child-worker", Agents: []string{"assistant"}, RunID: child.ID, Now: w.tick(), TTL: time.Minute,
	})
	require.NoError(w.t, err)
	end := FinishRequest{Status: StatusCompleted, Output: "child of " + child.Key, Now: w.tick()}
	switch roll := w.rng.IntN(100); {
	case roll < 20:
		end = FinishRequest{Status: StatusFailed, Reason: ReasonTimeBudget, Now: end.Now}
	case roll < 30:
		end = FinishRequest{Status: StatusCancelled, Reason: ReasonCancelled, Now: end.Now}
	}
	require.NoError(w.t, w.store.Finish(ctx, claimed.Lease(), end))
}

// planGrow runs one seeded run to its end, checking the property after
// every write, and returns the run as it ended.
func planGrow(t *testing.T, seed uint64, seen *planSeen) Run {
	t.Helper()
	ctx := t.Context()
	w := &planWorld{
		t:     t,
		rng:   rand.New(rand.NewPCG(seed, 2026)),
		store: NewMemoryStore(),
		def:   planDefinition(),
		seen:  seen,
		now:   planStart,
	}

	// One seed in each five is given one tight limit, so that every budget
	// ends some run.
	limits := Limits{MaxDuration: 15 * time.Minute, MaxCostMicros: -1, MaxTokens: -1, MaxModelCalls: 50}
	switch seed % 5 {
	case 1:
		limits.MaxDuration = 10 * time.Second
	case 2:
		limits.MaxCostMicros = 250
	case 3:
		limits.MaxTokens = 250
	case 4:
		limits.MaxModelCalls = 5
	}
	_, created, err := w.store.CreateRun(ctx, Run{
		ID: planRunID, Agent: "planner", Status: StatusRunnable, Input: "what is the total?",
		Definition: Snapshot{System: "be exact", Limits: limits}, CreatedAt: w.now, UpdatedAt: w.now,
	})
	require.NoError(t, err)
	require.True(t, created)
	claimed, err := w.store.Claim(ctx, ClaimRequest{
		Owner: "planner-test", Agents: []string{"planner"}, RunID: planRunID, Now: w.tick(), TTL: time.Minute,
	})
	require.NoError(t, err)
	w.lease = claimed.Lease()
	w.grown()

	for range 400 {
		if w.chance(1) {
			require.NoError(t, w.store.RequestCancel(ctx, CancelRequest{
				RunID: planRunID, By: "ops@example.test", Reason: "enough", Now: w.tick(),
			}))
		}
		run, steps, approvals, children := w.read()
		for _, a := range approvals {
			seen.approvals[a.Status]++
		}
		for _, child := range children {
			if child.Terminal() {
				seen.childEnded++
			} else {
				seen.childOpen++
			}
		}

		a := next(run, w.def, steps, approvals, children)
		seen.kinds[a.kind]++
		if !w.perform(a, run, steps, children) {
			continue
		}

		ended, _, approvals, _ := w.read()
		for _, a := range approvals {
			seen.approvals[a.Status]++
		}
		seen.ends[string(ended.Status)+" "+ended.Reason]++
		return ended
	}
	require.FailNow(t, "the run did not end", "seed %d", seed)
	return Run{}
}

// planEagerConversation is conversation without the rule that keeps it
// append-only: it hands a reply's results over as they arrive, without
// waiting for all of them. It is here so the property test is seen to fail
// for a conversation that deserves it.
func planEagerConversation(run Run, steps []Step) []Message {
	msgs := []Message{{Role: RoleUser, Text: run.Input}}
	for _, st := range steps {
		if st.Kind != StepModel || st.Status != StepCompleted {
			continue
		}
		msgs = append(msgs, *st.Message)
		var results []Result
		for _, tool := range steps {
			if tool.Kind == StepTool && tool.Turn == st.Seq && tool.Status.Done() {
				results = append(results, Result{CallID: tool.Call.ID, Content: tool.Result, IsError: tool.IsError})
			}
		}
		if len(results) > 0 {
			msgs = append(msgs, Message{Role: RoleTool, Results: results})
		}
	}
	return msgs
}

// The property a provider's prompt cache, and its check of a replayed turn,
// rest on: as a journal grows one write at a time, each conversation it
// gives starts with the one before. The journals are grown by next itself,
// with tool calls of every kind, approvals in every state, child runs,
// refusals, crashes and cancellations, and every write is one the store
// accepted.
func TestConversation_OnlyEverGrows(t *testing.T) {
	seen := &planSeen{
		kinds:     map[actionKind]int{},
		approvals: map[ApprovalStatus]int{},
		ends:      map[string]int{},
	}
	for seed := range uint64(400) {
		ended := planGrow(t, seed, seen)
		require.True(t, ended.Terminal(), "seed %d", seed)
	}

	t.Run("the journals grown cover what a run can do", func(t *testing.T) {
		for kind := actFinish; kind <= actPark; kind++ {
			assert.Positive(t, seen.kinds[kind], "no journal called for action kind %d", kind)
		}
		for _, status := range []ApprovalStatus{
			ApprovalPending, ApprovalApproved, ApprovalDeclined, ApprovalExpired, ApprovalCancelled,
		} {
			assert.Positive(t, seen.approvals[status], "no journal had an approval that was %s", status)
		}
		for _, end := range []string{
			"completed ",
			"cancelled " + ReasonCancelled,
			"failed " + ReasonRefusal,
			"failed " + ReasonTruncated,
			"failed " + ReasonContextWindow,
			"failed " + ReasonTimeBudget,
			"failed " + ReasonCostBudget,
			"failed " + ReasonTokenBudget,
			"failed " + ReasonModelCalls,
		} {
			assert.Positive(t, seen.ends[end], "no run ended %q", end)
		}
		assert.Zero(t, seen.kinds[actYield], "next gave up on a journal the store wrote")
		assert.Zero(t, seen.ends["failed "+ReasonError], "a run was ended for what next could not read")
		assert.Positive(t, seen.refusedWithCalls, "no refusal came with calls")
		assert.Positive(t, seen.childEnded, "no child run had ended when next was asked")
		assert.Positive(t, seen.childOpen, "no child run was still going when next was asked")
		assert.Greater(t, seen.growths, 8_000, "the journals were not grown far")
		t.Logf("writes checked: %d; actions by kind: %v; approvals seen: %v; ends: %v",
			seen.growths, seen.kinds, seen.approvals, seen.ends)
	})

	t.Run("a conversation that hands results over early is caught", func(t *testing.T) {
		assert.Positive(t, seen.eagerBroke,
			"the journals never showed a conversation that edits itself to be doing so")
	})
}

// What the engine holds for the executor. These read the engine's unexported
// fields, and so sit in this file: engine_test.go is in package agent_test,
// where it can use the agenttest kit, which a test in this package cannot
// import.

type planNoModel struct{}

func (planNoModel) Generate(context.Context, Request) (Response, error) {
	return Response{}, errors.New("no model in this test")
}

func TestNew_AppliesTheDefaults(t *testing.T) {
	e, err := New(Options{Model: planNoModel{}})
	require.NoError(t, err)

	assert.IsType(t, &MemoryStore{}, e.opts.Store)
	assert.IsType(t, systemClock{}, e.opts.Clock)
	assert.Same(t, slog.Default(), e.opts.Logger)
	assert.Nil(t, e.opts.Guard)
	assert.Nil(t, e.opts.Events)
	assert.Equal(t, 30*time.Second, e.opts.LeaseTTL)
	assert.Equal(t, 10*time.Second, e.opts.HeartbeatInterval)
	assert.Equal(t, time.Second, e.opts.PollInterval)
	assert.Equal(t, 4, e.opts.Concurrency)
	assert.Equal(t, 5, e.opts.MaxFailures)
	assert.Equal(t, time.Second, e.opts.RetryBase)
	assert.Equal(t, time.Minute, e.opts.RetryMax)
	assert.Equal(t, 10*time.Second, e.opts.DrainTimeout)
	assert.Zero(t, e.opts.ApprovalTTL)
	assert.Equal(t, 3, e.opts.MaxDepth)

	first, second := e.opts.NewID(), e.opts.NewID()
	assert.True(t, isUUID(first), "NewID gave %q", first)
	assert.NotEqual(t, first, second)

	host, err := os.Hostname()
	require.NoError(t, err)
	assert.Contains(t, e.opts.WorkerID, host)
	assert.Contains(t, e.opts.WorkerID, strconv.Itoa(os.Getpid()))
	other, err := New(Options{Model: planNoModel{}})
	require.NoError(t, err)
	assert.NotEqual(t, e.opts.WorkerID, other.opts.WorkerID, "two engines in one process share a worker id")

	assert.Same(t, e.opts.Store, e.store)
	assert.Same(t, e.opts.Logger, e.log)
	assert.Equal(t, e.opts.Clock, e.clock)
}

func TestNew_KeepsWhatItWasGiven(t *testing.T) {
	store := NewMemoryStore()
	logger := slog.New(slog.DiscardHandler)
	given := Options{
		Model:             planNoModel{},
		Store:             store,
		Clock:             planClock{},
		Logger:            logger,
		NewID:             func() string { return planRunID },
		WorkerID:          "worker-7",
		LeaseTTL:          90 * time.Second,
		HeartbeatInterval: 7 * time.Second,
		PollInterval:      3 * time.Second,
		Concurrency:       9,
		MaxFailures:       2,
		RetryBase:         4 * time.Second,
		RetryMax:          5 * time.Minute,
		DrainTimeout:      6 * time.Second,
		ApprovalTTL:       time.Hour,
		MaxDepth:          1,
	}

	e, err := New(given)
	require.NoError(t, err)

	assert.Same(t, store, e.store)
	assert.Same(t, logger, e.log)
	assert.Equal(t, planClock{}, e.clock)
	assert.Equal(t, planRunID, e.opts.NewID())
	assert.Equal(t, "worker-7", e.opts.WorkerID)
	assert.Equal(t, 90*time.Second, e.opts.LeaseTTL)
	assert.Equal(t, 7*time.Second, e.opts.HeartbeatInterval)
	assert.Equal(t, 3*time.Second, e.opts.PollInterval)
	assert.Equal(t, 9, e.opts.Concurrency)
	assert.Equal(t, 2, e.opts.MaxFailures)
	assert.Equal(t, 4*time.Second, e.opts.RetryBase)
	assert.Equal(t, 5*time.Minute, e.opts.RetryMax)
	assert.Equal(t, 6*time.Second, e.opts.DrainTimeout)
	assert.Equal(t, time.Hour, e.opts.ApprovalTTL)
	assert.Equal(t, 1, e.opts.MaxDepth)
}

type planClock struct{}

func (planClock) Now() time.Time { return planStart }

func TestNew_Defaults(t *testing.T) {
	t.Run("the heartbeat is a third of the lease it was given", func(t *testing.T) {
		e, err := New(Options{Model: planNoModel{}, LeaseTTL: 90 * time.Second})
		require.NoError(t, err)
		assert.Equal(t, 30*time.Second, e.opts.HeartbeatInterval)
	})

	t.Run("a value below zero takes the default, as zero does", func(t *testing.T) {
		e, err := New(Options{
			Model:             planNoModel{},
			LeaseTTL:          -time.Second,
			HeartbeatInterval: -time.Second,
			PollInterval:      -time.Second,
			Concurrency:       -1,
			MaxFailures:       -1,
			RetryBase:         -time.Second,
			RetryMax:          -time.Second,
			DrainTimeout:      -time.Second,
			MaxDepth:          -1,
		})
		require.NoError(t, err)
		assert.Equal(t, 30*time.Second, e.opts.LeaseTTL)
		assert.Equal(t, 10*time.Second, e.opts.HeartbeatInterval)
		assert.Equal(t, time.Second, e.opts.PollInterval)
		assert.Equal(t, 4, e.opts.Concurrency)
		assert.Equal(t, 5, e.opts.MaxFailures)
		assert.Equal(t, time.Second, e.opts.RetryBase)
		assert.Equal(t, time.Minute, e.opts.RetryMax)
		assert.Equal(t, 10*time.Second, e.opts.DrainTimeout)
		assert.Equal(t, 3, e.opts.MaxDepth)
	})

	t.Run("an approval lifetime below zero never lapses, as zero does not", func(t *testing.T) {
		e, err := New(Options{Model: planNoModel{}, ApprovalTTL: -time.Minute})
		require.NoError(t, err)
		assert.Zero(t, e.opts.ApprovalTTL)
	})

	t.Run("the system clock reads in UTC, to the microsecond a database keeps", func(t *testing.T) {
		now := systemClock{}.Now()
		assert.Equal(t, time.UTC, now.Location())
		assert.Equal(t, now.Truncate(time.Microsecond), now)
		assert.Equal(t, now, now.Round(0), "the reading carries a monotonic clock")
	})
}

func TestEngine_Definition(t *testing.T) {
	e, err := New(Options{Model: planNoModel{}})
	require.NoError(t, err)
	require.NoError(t, e.Register(planDefinition()))

	def, ok := e.definition("planner")
	require.True(t, ok)
	assert.Equal(t, "planner", def.Name)
	assert.Len(t, def.Tools, 4)

	_, ok = e.definition("nobody")
	assert.False(t, ok)
}

func TestToolOf(t *testing.T) {
	def := planDefinition()

	tool, ok := toolOf(def, "charge")
	require.True(t, ok)
	assert.Equal(t, "charge", tool.Name)
	assert.True(t, tool.AtMostOnce)

	tool, ok = toolOf(def, "gone")
	assert.False(t, ok)
	assert.Zero(t, tool.Name)
}
