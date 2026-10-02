package agent

import (
	"fmt"
	"time"
)

// actionKind is what next asks an execution to do.
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

// action is one thing for an execution to do, and what it needs to do it.
type action struct {
	kind   actionKind
	seq    int    // the step acted on; for actModel, the step to begin
	status Status // actFinish
	reason string // actFinish, actPark
	output string // actFinish
	errmsg string // actYield: what was found
}

// conversation rebuilds what the model is sent: the run's input, then each
// stored reply as it was stored, each followed by its calls' results once
// every one of them is final.
//
// It is a function of the run and its journal and nothing else, and what it
// gives for a journal is the start of what it gives for any journal that one
// grows into. A reply's results appear all at once or not at all, so nothing
// already sent is ever edited: that is what lets a provider check a replayed
// turn against what came before it, and reuse its cache of the prompt.
//
// The messages share the journal's memory. A caller that changes one copies
// it first.
func conversation(run Run, steps []Step) []Message {
	msgs := []Message{{Role: RoleUser, Text: run.Input}}
	for i, st := range steps {
		if st.Kind != StepModel || st.Status != StepCompleted || st.Message == nil {
			continue
		}
		msgs = append(msgs, *st.Message)
		if results := turnResults(st.Seq, steps[i+1:]); results != nil {
			msgs = append(msgs, Message{Role: RoleTool, Results: results})
		}
	}
	return msgs
}

// turnResults returns the results of the calls the reply at turn made, in
// the order it made them, or nil when it made none or one is not yet final.
// after is the journal from the step after the reply: its calls' steps come
// first there.
func turnResults(turn int, after []Step) []Result {
	var results []Result
	for _, st := range after {
		if st.Turn != turn {
			break
		}
		if !st.Status.Done() {
			return nil
		}
		result := Result{Content: st.Result, IsError: st.IsError}
		if st.Call != nil {
			result.CallID = st.Call.ID
		}
		results = append(results, result)
	}
	return results
}

// next decides what an execution does now. def is the Definition registered
// under run.Agent in this process. approvals are the run's, and children
// maps a child run's id to it.
//
// It is a function of what it is given and nothing else. It reads no clock
// and no store, so the execution that resumes a run decides as the one that
// was interrupted would have. The rules are tried in this order, and the
// first that gives an action gives next's:
//
//  1. Cancellation was requested: the run finishes cancelled.
//  2. A step this build cannot read, anywhere in the journal: the run is
//     given up. That is a kind that is neither model nor tool, a status its
//     kind does not have, and a completed model step with no message.
//  3. The last reply's stop says it is no turn, whatever came with it: a
//     refusal, a reply cut at its token limit and one that ran out of
//     context window fail the run, and their calls are never judged or run;
//     a stop this build does not know gives the run up.
//  4. The tool steps that are not final, in order. A proposed step is judged.
//     A started step was interrupted: its child is started when its tool
//     delegates; a person is asked when its tool is at-most-once; otherwise
//     it is run again. A waiting step with a child is collected once the
//     child has ended. Any other waiting step goes by the approval for its
//     current attempt: approved, it is run; pending, it is passed over;
//     declined, expired or cancelled, the no is recorded.
//  5. Steps were passed over and nothing else could be done: the run parks,
//     for a person when any step waits on one, and else for its children.
//  6. No tool step is open: the model is called, or called again where its
//     call was interrupted, or the run completes with the last reply's text.
//
// Over them sits the budget. Once one is spent, no step is judged, run,
// spawned or asked about, and the model is not called. The walk still gives
// what does no work, collecting an ended child (whose usage belongs in the
// run's totals) and recording a person's no, and the run fails for its
// budget only when the walk has none of those left. A run whose work is done
// is never failed for its budget.
//
// A journal this build cannot make sense of gives the run up, with actYield
// and what was found, at the point in that order where it is met: rule 2's
// steps and rule 3's stop; a waiting step whose child was not given, or
// that has no approval for its current attempt, or whose approval has a
// status this build does not know; a started at-most-once step that already
// has an approval for its current attempt, which asking again would only
// hand back; a last reply that made no calls and did not end or pause. The
// run is not ended for it: ending cannot be undone, and what this build
// cannot read may be a newer build's writing during a deploy, or the
// executor's mistake. Given up as a failed attempt, the run waits out its
// back-off and is claimed again, by a worker that can read it or after a
// fix; if none can, the limit on failed attempts ends it, with the same
// message. Parking it instead would leave nothing to wake it.
func next(run Run, def Definition, steps []Step, approvals []Approval, children map[string]Run) action {
	if run.CancelRequested {
		return action{kind: actFinish, status: StatusCancelled, reason: ReasonCancelled}
	}

	for _, st := range steps {
		if a, unread := unreadable(st); unread {
			return a
		}
	}

	if last := lastModelStep(steps); last != nil && last.Status == StepCompleted {
		switch last.Stop {
		case StopRefusal:
			return failRun(ReasonRefusal)
		case StopMaxTokens:
			return failRun(ReasonTruncated)
		case StopContextWindow:
			return failRun(ReasonContextWindow)
		case StopEnd, StopPause, StopToolUse:
		default:
			return giveUp("model reply at step %d has the stop %q", last.Seq, last.Stop)
		}
	}

	// over is the budget that is spent, if one is. held records that it kept
	// a step from being worked on, and onPerson and onChild the steps passed
	// over because nothing can be done for them yet.
	over := spent(run, false)
	held, onPerson, onChild := false, false, false
	for _, st := range steps {
		if st.Kind != StepTool || st.Status.Done() {
			continue
		}
		// a is the work this step calls for. A case that finds none returns
		// what does no work, or goes on to the next step.
		a := action{seq: st.Seq}
		switch st.Status {
		case StepProposed:
			a.kind = actJudge

		case StepStarted:
			// A step seen here as started was interrupted: an execution that
			// starts a step finishes it, or ends, before next is asked again.
			// A tool this build does not have is run, for the executor to
			// answer as it answers any call to one.
			tool, _ := toolOf(def, st.Name)
			_, asked := approvalFor(approvals, run.ID, st)
			switch {
			case tool.Delegate != "":
				a.kind = actSpawn
			case !tool.AtMostOnce:
				a.kind = actRun
			case asked:
				// Asking again would be handed the approval that exists and
				// move nothing, and this action would come back for ever.
				return giveUp("step %d is started and already has an approval for attempt %d", st.Seq, st.Attempts)
			default:
				a.kind = actAsk
			}

		case StepWaiting:
			if st.ChildRunID != "" {
				child, ok := children[st.ChildRunID]
				switch {
				case !ok:
					return giveUp("step %d waits on child run %s, which is not among the run's children",
						st.Seq, st.ChildRunID)
				case child.Terminal():
					return action{kind: actCollect, seq: st.Seq}
				}
				onChild = true
				continue
			}
			// A step waiting on a person was asked about at its current
			// attempt, so that approval is the one that decides. An older
			// one must not: given a list cut short, it would run an
			// interrupted call that nobody has approved.
			asked, ok := approvalFor(approvals, run.ID, st)
			if !ok {
				return giveUp("step %d is waiting with no approval for attempt %d and no child run", st.Seq, st.Attempts)
			}
			switch asked.Status {
			case ApprovalApproved:
				a.kind = actRun
			case ApprovalPending:
				onPerson = true
				continue
			case ApprovalDeclined, ApprovalExpired, ApprovalCancelled:
				return action{kind: actResolve, seq: st.Seq}
			default:
				return giveUp("step %d's approval has the status %q", st.Seq, asked.Status)
			}
		}
		if over != "" {
			held = true
			continue
		}
		return a
	}
	switch {
	case held:
		return failRun(over)
	case onPerson:
		return action{kind: actPark, reason: ReasonApproval}
	case onChild:
		return action{kind: actPark, reason: ReasonChildren}
	}

	// No tool step is open, so what the journal ends with says what is next:
	// nothing or a call's result, and the model is owed a turn; a model step
	// still started, and its call is made again; a reply that made no calls,
	// and its stop decides.
	if len(steps) == 0 || steps[len(steps)-1].Kind == StepTool {
		return callModel(run, len(steps)+1)
	}
	last := steps[len(steps)-1]
	if last.Status == StepStarted {
		return callModel(run, last.Seq)
	}
	switch last.Stop {
	case StopEnd:
		return action{kind: actFinish, status: StatusCompleted, output: last.Message.Text}
	case StopPause:
		return callModel(run, len(steps)+1)
	}
	return giveUp("model reply at step %d ended with stop %q and made no calls", last.Seq, last.Stop)
}

// unreadable reports whether st is a step this build cannot read, and gives
// the action that says so: a kind that is neither model nor tool, a status
// its kind does not have, or a completed model step with no message.
func unreadable(st Step) (action, bool) {
	switch st.Kind {
	case StepModel:
		switch {
		case st.Status != StepStarted && st.Status != StepCompleted:
			return giveUp("step %d, a model step, has the status %q", st.Seq, st.Status), true
		case st.Status == StepCompleted && st.Message == nil:
			return giveUp("step %d is a completed model step with no message", st.Seq), true
		}
	case StepTool:
		if !isStepStatus(st.Status) {
			return giveUp("step %d has the status %q", st.Seq, st.Status), true
		}
	default:
		return giveUp("step %d has the kind %q", st.Seq, st.Kind), true
	}
	return action{}, false
}

// approvalFor returns the approval of run runID for st's current attempt.
// The store records at most one.
func approvalFor(approvals []Approval, runID string, st Step) (Approval, bool) {
	for _, a := range approvals {
		if a.RunID == runID && a.Seq == st.Seq && a.Attempt == st.Attempts {
			return a, true
		}
	}
	return Approval{}, false
}

// callModel is the call of the model for step seq, or the run's failure when
// a budget is spent. The limit on model calls is checked here and nowhere
// else, since it bounds model calls and nothing more.
func callModel(run Run, seq int) action {
	if reason := spent(run, true); reason != "" {
		return failRun(reason)
	}
	return action{kind: actModel, seq: seq}
}

// spent names the budget run has used up, or is empty when it has none. A
// budget is spent once it is reached. The limit on model calls counts only
// when model is set, since it is checked before a model call and nothing
// else. When more than one is spent the reason is the first of time, cost,
// tokens and model calls.
func spent(run Run, model bool) string {
	limits := filledLimits(run.Definition.Limits)
	switch {
	case limits.MaxDuration > 0 && time.Duration(run.ActiveMillis)*time.Millisecond >= limits.MaxDuration:
		return ReasonTimeBudget
	case limits.MaxCostMicros > 0 && run.Usage.CostMicros >= limits.MaxCostMicros:
		return ReasonCostBudget
	case limits.MaxTokens > 0 && run.Usage.InputTokens+run.Usage.OutputTokens >= limits.MaxTokens:
		return ReasonTokenBudget
	case model && limits.MaxModelCalls > 0 && run.ModelCalls >= limits.MaxModelCalls:
		return ReasonModelCalls
	}
	return ""
}

// failRun is the action that ends the run as failed, for one of the reasons
// the design names as ending a run.
func failRun(reason string) action {
	return action{kind: actFinish, status: StatusFailed, reason: reason}
}

// giveUp is the action that hands the run back as a failed attempt, saying
// what in its journal this build could not read. It is not work, and no
// budget turns it into the run's end.
func giveUp(format string, args ...any) action {
	return action{kind: actYield, errmsg: fmt.Sprintf(format, args...)}
}

// lastModelStep returns the journal's last model step, or nil.
func lastModelStep(steps []Step) *Step {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Kind == StepModel {
			return &steps[i]
		}
	}
	return nil
}

// toolOf returns def's tool named name.
func toolOf(def Definition, name string) (Tool, bool) {
	for _, tool := range def.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}
