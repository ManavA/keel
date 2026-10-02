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
)

// action is one thing for an execution to do, and what it needs to do it.
type action struct {
	kind   actionKind
	seq    int    // the step acted on; for actModel, the step to begin
	status Status // actFinish
	reason string // actFinish, actPark
	output string // actFinish
	errmsg string // actFinish
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
//  2. The last reply is a refusal: the run fails. A refusal is the model's
//     last word and never a turn, so calls that came with one are not judged.
//  3. The tool steps that are not final, in order. A proposed step is judged.
//     A started step was interrupted: its child is started when its tool
//     delegates; a person is asked when its tool is at-most-once and nobody
//     has approved this attempt; otherwise it is run again. A waiting step
//     with a child is collected once the child has ended. Any other waiting
//     step goes by the approval with the highest attempt: approved, it is
//     run; pending, it is passed over; anything else is a no to record.
//  4. Steps were passed over and nothing else could be done: the run parks,
//     for a person when any step waits on one, and else for its children.
//  5. No tool step is open: the model is called, or called again where its
//     call was interrupted, or the run ends as the last reply's stop says.
//
// Over them all sits work: an action that would do work is replaced by the
// run's failure when a budget is spent.
//
// A journal no store writes (a waiting step with nothing to wait for, a
// child that was not given, a kind, a status or a stop this build does not
// know) fails the run with ReasonError and says what was found. The alternatives
// are a run that parks with nothing to wake it and a guess at what an
// unknown value meant.
func next(run Run, def Definition, steps []Step, approvals []Approval, children map[string]Run) action {
	if run.CancelRequested {
		return action{kind: actFinish, status: StatusCancelled, reason: ReasonCancelled}
	}
	if last := lastModelStep(steps); last != nil && last.Stop == StopRefusal {
		return failRun(ReasonRefusal, "")
	}

	// onPerson and onChild record the steps passed over because nothing can
	// be done for them yet.
	onPerson, onChild := false, false
	for _, st := range steps {
		if st.Kind != StepTool || st.Status.Done() {
			continue
		}
		switch st.Status {
		case StepProposed:
			return work(run, action{kind: actJudge, seq: st.Seq})

		case StepStarted:
			// A step seen here as started was interrupted: an execution that
			// starts a step finishes it, or ends, before next is asked again.
			// A tool this build does not have is run, for the executor to
			// answer as it answers any call to one.
			tool, _ := toolOf(def, st.Name)
			switch {
			case tool.Delegate != "":
				return work(run, action{kind: actSpawn, seq: st.Seq})
			case tool.AtMostOnce && !approvedFor(approvals, run.ID, st):
				return action{kind: actAsk, seq: st.Seq}
			}
			return work(run, action{kind: actRun, seq: st.Seq})

		case StepWaiting:
			if st.ChildRunID != "" {
				child, ok := children[st.ChildRunID]
				switch {
				case !ok:
					return failRun(ReasonError, fmt.Sprintf(
						"step %d waits on child run %s, which is not among the run's children", st.Seq, st.ChildRunID))
				case child.Terminal():
					return action{kind: actCollect, seq: st.Seq}
				}
				onChild = true
				continue
			}
			asked, ok := latestApproval(approvals, run.ID, st.Seq)
			switch {
			case !ok:
				return failRun(ReasonError, fmt.Sprintf("step %d is waiting with no approval and no child run", st.Seq))
			case asked.Status == ApprovalApproved:
				return work(run, action{kind: actRun, seq: st.Seq})
			case asked.Status == ApprovalPending:
				onPerson = true
			default:
				return action{kind: actResolve, seq: st.Seq}
			}

		default:
			return failRun(ReasonError, fmt.Sprintf("step %d has the status %q", st.Seq, st.Status))
		}
	}
	switch {
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
		return work(run, action{kind: actModel, seq: len(steps) + 1})
	}
	last := steps[len(steps)-1]
	switch {
	case last.Kind != StepModel:
		return failRun(ReasonError, fmt.Sprintf("step %d has the kind %q", last.Seq, last.Kind))
	case last.Status == StepStarted:
		return work(run, action{kind: actModel, seq: last.Seq})
	}
	switch last.Stop {
	case StopEnd:
		a := action{kind: actFinish, status: StatusCompleted}
		if last.Message != nil {
			a.output = last.Message.Text
		}
		return a
	case StopPause:
		return work(run, action{kind: actModel, seq: len(steps) + 1})
	case StopMaxTokens:
		return failRun(ReasonTruncated, "")
	case StopContextWindow:
		return failRun(ReasonContextWindow, "")
	}
	return failRun(ReasonError,
		fmt.Sprintf("model reply at step %d ended with stop %q and made no calls", last.Seq, last.Stop))
}

// approvedFor reports whether a person has said yes to st's current attempt.
func approvedFor(approvals []Approval, runID string, st Step) bool {
	for _, a := range approvals {
		if a.RunID == runID && a.Seq == st.Seq && a.Attempt == st.Attempts && a.Status == ApprovalApproved {
			return true
		}
	}
	return false
}

// latestApproval returns the approval with the highest Attempt for the step
// at seq of run runID.
func latestApproval(approvals []Approval, runID string, seq int) (Approval, bool) {
	var latest Approval
	found := false
	for _, a := range approvals {
		if a.RunID != runID || a.Seq != seq {
			continue
		}
		if !found || a.Attempt > latest.Attempt {
			latest, found = a, true
		}
	}
	return latest, found
}

// work returns a, an action that does work, or in its place the run's
// failure when a budget is spent. The rule sits over the others: every
// action that calls the model, judges or runs a tool, or starts a child goes
// through here, and no other does, so a run whose work is done is never
// failed for its budget.
func work(run Run, a action) action {
	if reason := spent(run, a.kind == actModel); reason != "" {
		return failRun(reason, "")
	}
	return a
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

// failRun is the action that ends the run as failed.
func failRun(reason, errmsg string) action {
	return action{kind: actFinish, status: StatusFailed, reason: reason, errmsg: errmsg}
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
