package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"time"
)

const (
	// defaultToolTimeout bounds one execution of a tool whose Timeout is zero.
	defaultToolTimeout = 2 * time.Minute
	// lastWriteTimeout bounds the write that ends an execution once the
	// context it would have been made under has ended, and the read that
	// follows it in Execute.
	lastWriteTimeout = 5 * time.Second
)

// The results no tool produced, as 6.4 of the design fixes them, and the
// rules recorded for a block that no rule of a Guard made.
const (
	resultMalformed    = "arguments were not valid JSON"
	resultUnavailable  = "tool is not available"
	resultBlocked      = "blocked by policy: "
	resultExpired      = "declined: approval expired"
	resultCancelled    = "declined: approval cancelled"
	resultInterrupted  = "interrupted before its result was recorded; not run again"
	resultUnrecordable = "not run: the policy decision could not be recorded"

	ruleUnrecordable   = "decision could not be recorded"
	ruleActionPanicked = "the tool's Action function panicked"
)

var (
	// errDrained is the cause of a step's context that ended because the
	// context its execution was given ended and DrainTimeout then passed.
	errDrained = errors.New("agent: the step in flight outlived the drain timeout")
	// errTimeBudget is the cause of a model call's context that ended because
	// what was left of the run's time budget ran out.
	errTimeBudget = errors.New("agent: the run's time budget ran out")
)

// ending is how an execution let its run go.
type ending int

const (
	// going is no ending: the execution asks next again.
	going ending = iota
	// endedUnwritten is an execution that made no last write: its lease was
	// lost, a tool was still running when its time ran out, or the store did
	// not take the write. The lease is left to lapse.
	endedUnwritten
	endedCompleted
	endedFailed
	endedCancelled
	endedParked
	endedYielded
)

// Execute claims run runID and executes it until it ends or parks, and
// returns it as it then stands. It returns ErrNotClaimable when another
// process holds the run or it is not runnable.
//
// The error is nil exactly when the run ended, whatever its status, or
// parked. An execution that ended any other way returns the run with the
// reason: the error of the step that failed, when the run was given back to
// be tried again; ctx's, when ctx ended and the run was given back for
// another worker; ErrLeaseLost, when another process took the run and
// nothing more was written.
func (e *Engine) Execute(ctx context.Context, runID string) (Run, error) {
	claimed, err := e.claim(ctx, runID)
	if err != nil {
		return Run{}, fmt.Errorf("agent: execute run %s: %w", runID, err)
	}
	_, failure := e.execute(ctx, *claimed)

	// The run is read back even when ctx has ended: that is one of the ways
	// an execution ends, and its caller is owed the run as it was left.
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), lastWriteTimeout)
	defer cancel()
	run, err := e.store.GetRun(read, runID)
	switch {
	case failure != nil:
		return run, fmt.Errorf("agent: execute run %s: %w", runID, failure)
	case err != nil:
		return Run{}, fmt.Errorf("agent: execute run %s: read it back: %w", runID, err)
	}
	return run, nil
}

// claim takes runID, or the oldest run that can be taken when runID is
// empty, for this engine: only a run of an agent registered here.
func (e *Engine) claim(ctx context.Context, runID string) (*Run, error) {
	e.mu.RLock()
	agents := slices.Sorted(maps.Keys(e.defs))
	e.mu.RUnlock()

	run, err := e.store.Claim(ctx, ClaimRequest{
		Owner: e.opts.WorkerID, Agents: agents, RunID: runID, Now: e.clock.Now(), TTL: e.opts.LeaseTTL,
	})
	if err == nil && run == nil && runID != "" {
		return nil, ErrNotClaimable
	}
	return run, err
}

// execution is one hold on a run: everything between a claim and letting the
// run go. It keeps nothing about the run between turns of its loop. What it
// does keep is which context to work under, and why the last one ended.
type execution struct {
	e     *Engine
	lease Lease

	// caller is the context Execute, Tick or Work was given. Once it has
	// ended, the action in flight is finished and no other is begun.
	caller context.Context
	// step is caller without its end: it ends DrainTimeout after caller
	// does, with errDrained as its cause, and not before.
	step context.Context
	// held is step for as long as the lease is kept, and stop ends the
	// keeper that keeps it. stop is nil once the keeper has been stopped,
	// and cause is then why held had ended at that moment, or nil.
	held  context.Context
	stop  func()
	cause error
	// work is the context the store, the model, the guard and the tools are
	// called under: held, or step once the keeper has been stopped or
	// cancellation of the run has been requested, since the run is then
	// still this execution's to finish.
	work context.Context

	// refused says Park has reported that there was nothing to wait for, and
	// refusedAt the run's Rev when it last did.
	refused   bool
	refusedAt int64
}

// execute carries out a run this engine has just claimed until it ends,
// parks or is given back, and reports which. Whatever happens, the keeper is
// stopped before it returns: one left running under a context nothing
// cancels would extend the lease for the life of the process.
//
// A panic on the execution's own goroutine, from the model, the guard or
// this package, is logged and ends the execution as one whose step failed.
// A worker runs many runs, and one that panics must not take the others
// with it. What a later execution then finds is what a crash at that point
// would have left.
func (e *Engine) execute(ctx context.Context, claimed Run) (end ending, err error) {
	step, release := e.detach(ctx)
	defer release()

	x := &execution{e: e, lease: claimed.Lease(), caller: ctx, step: step}
	x.hold()
	defer x.letGo()
	defer func() {
		if r := recover(); r != nil {
			e.log.ErrorContext(step, "agent: an execution panicked",
				"run", claimed.ID, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			end, err = x.fail(claimed, fmt.Errorf("the execution panicked: %v", r), false)
		}
	}()
	return x.loop(claimed)
}

// detach returns the context an execution's steps run under: ctx's values,
// without its end. When ctx ends, a step in flight has DrainTimeout to
// finish, and then the context ends with errDrained as its cause. release
// ends it at once and must be called.
func (e *Engine) detach(ctx context.Context) (step context.Context, release func()) {
	step, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	unwatch := context.AfterFunc(ctx, func() {
		drain := time.NewTimer(e.opts.DrainTimeout)
		defer drain.Stop()
		select {
		case <-drain.C:
			cancel(errDrained)
		case <-step.Done():
		}
	})
	return step, func() {
		unwatch()
		cancel(nil)
	}
}

// hold starts a keeper for the lease, and works under what it holds.
func (x *execution) hold() {
	x.held, x.stop = keep(x.step, x.lease, keepOptions{
		Store:    x.e.store,
		Clock:    x.e.clock,
		TTL:      x.e.opts.LeaseTTL,
		Interval: x.e.opts.HeartbeatInterval,
		Logger:   x.e.log,
	})
	x.work, x.cause = x.held, nil
}

// letGo stops the keeper, and keeps in cause why the held context had ended
// by then, or nil. The cause is read first: once the keeper is stopped it is
// always at least context.Canceled. An execution calls it before its last
// write, since a heartbeat that landed after a Finish, a Park or a Yield
// would find the lease gone and say it was lost. What is written afterwards
// is written under step.
func (x *execution) letGo() {
	if x.stop != nil {
		x.cause = context.Cause(x.held)
		x.stop()
		x.stop = nil
		x.work = x.step
	}
}

// released stops the keeper and reports whether the lease had been lost by
// then, in which case nothing more is to be written.
func (x *execution) released() (lost bool) {
	x.letGo()
	return errors.Is(x.cause, ErrLeaseLost)
}

// interrupted reports why the execution cannot go on working, or nil when it
// can. A request to cancel the run does not stop it: the run is this
// execution's to finish as cancelled, the keeper beats on until it is
// stopped, and the work goes on under step.
func (x *execution) interrupted() error {
	if x.work.Err() == nil {
		return nil
	}
	if x.step.Err() != nil {
		return context.Cause(x.step)
	}
	cause := context.Cause(x.work)
	if errors.Is(cause, errCancelRequested) {
		x.work = x.step
		return nil
	}
	return cause
}

// last is the context of the write that ends an execution: step while it
// lives, and after the drain has run out one of its own, so that a run whose
// model call was cut off by a shutdown can still be given back.
func (x *execution) last() (context.Context, context.CancelFunc) {
	if x.step.Err() == nil {
		return x.step, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(x.step), lastWriteTimeout)
}

// loop is 6.5 of the design: read everything, ask next, do the one thing it
// says, and read everything again. Nothing next said is carried across a
// write.
func (x *execution) loop(run Run) (ending, error) {
	if run.Failures >= x.e.opts.MaxFailures {
		lost := fmt.Sprintf("abandoned: %d executions in a row failed or lost the run", run.Failures)
		if run.Error != "" {
			lost += "; the last error recorded: " + run.Error
		}
		return x.finish(run, StatusFailed, ReasonAbandoned, "", lost)
	}

	for {
		if cause := x.interrupted(); cause != nil {
			return x.cut(run, cause, false)
		}
		if x.caller.Err() != nil {
			// The action in flight when the caller's context ended has
			// finished and is recorded. Another worker carries on.
			return x.yield(run, context.Cause(x.caller))
		}

		changes, children, err := x.read()
		if err != nil {
			return x.broke(run, err)
		}
		run = changes.Run
		// Claim took a run of an agent registered here, and no agent is ever
		// unregistered.
		def, _ := x.e.definition(run.Agent)

		a := next(run, def, changes.Steps, changes.Approvals, children)
		if end, err := x.perform(a, run, def, changes, children); end != going {
			return end, err
		}
	}
}

// read is what next is given, fresh: the run, its whole journal from the
// first step, every approval it has, and every child a waiting step names.
// A child that is not there is left out, and next gives the run up for it.
func (x *execution) read() (Changes, map[string]Run, error) {
	changes, err := x.e.store.Changes(x.work, x.lease.RunID, 0)
	if err != nil {
		return Changes{}, nil, fmt.Errorf("read the run: %w", err)
	}
	for i, st := range changes.Steps {
		if st.Seq != i+1 {
			return Changes{}, nil, fmt.Errorf("read the run: its journal has step %d where step %d belongs", st.Seq, i+1)
		}
	}
	children := map[string]Run{}
	for _, st := range changes.Steps {
		if st.Kind != StepTool || st.Status != StepWaiting || st.ChildRunID == "" {
			continue
		}
		child, err := x.e.store.GetRun(x.work, st.ChildRunID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return Changes{}, nil, fmt.Errorf("read child run %s of step %d: %w", st.ChildRunID, st.Seq, err)
		default:
			children[child.ID] = child
		}
	}
	return changes, children, nil
}

// perform carries out the one action next gave. It returns going when the
// loop is to go round again, and otherwise how the execution ended.
func (x *execution) perform(a action, run Run, def Definition, changes Changes, children map[string]Run) (ending, error) {
	var st Step
	if a.seq >= 1 && a.seq <= len(changes.Steps) {
		st = changes.Steps[a.seq-1]
	}
	switch a.kind {
	case actFinish:
		return x.finish(run, a.status, a.reason, a.output, "")
	case actModel:
		return x.model(run, a.seq, changes.Steps)
	case actJudge:
		return x.judge(run, def, st)
	case actRun:
		return x.start(run, def, st, "", "")
	case actSpawn:
		tool, _ := toolOf(def, st.Name)
		return x.spawn(run, tool, st)
	case actCollect:
		return x.collect(run, st, children[st.ChildRunID])
	case actResolve:
		asked, _ := approvalFor(changes.Approvals, run.ID, st)
		return x.resolve(run, st, asked)
	case actAsk:
		return x.ask(run, def, st)
	case actPark:
		return x.park(run, a.reason)
	default:
		// actYield: the run is given up, with the longest wait from the
		// first time. It is most often an old worker meeting a newer build's
		// journal during a deploy, and the usual wait would spend the
		// failure limit in seconds.
		return x.fail(run, errors.New(a.errmsg), true)
	}
}

// model performs actModel: the step is begun, the model asked, and its reply
// stored with the tool steps it proposes. A crash after the first write
// leaves a started model step, and the call is made again.
func (x *execution) model(run Run, seq int, steps []Step) (ending, error) {
	now := x.e.clock.Now()
	if err := x.e.store.BeginModel(x.work, x.lease, seq, now); err != nil {
		return x.broke(run, fmt.Errorf("begin the model call at step %d: %w", seq, err))
	}
	x.e.publish(x.step, EventStepStarted, run, seq, now)

	snapshot := run.Definition
	req := Request{
		RunID:     run.ID,
		Agent:     run.Agent,
		Model:     snapshot.Model,
		System:    snapshot.System,
		Messages:  conversation(run, steps),
		Tools:     snapshot.Tools,
		Output:    snapshot.Output,
		MaxTokens: snapshot.MaxTokens,
	}
	// The call is given what is left of the time budget. Its time is counted
	// only when it completes, so a call cut off for the budget ends the run
	// here: tried again, it would be given the same time and cut off again.
	ctx, cancel := x.work, context.CancelFunc(func() {})
	if left, bounded := timeLeft(run); bounded {
		ctx, cancel = context.WithTimeoutCause(x.work, left, errTimeBudget)
	}
	resp, err := x.e.opts.Model.Generate(ctx, req)
	outOfTime := errors.Is(context.Cause(ctx), errTimeBudget)
	cancel()

	switch {
	case x.work.Err() != nil:
		// The lease is gone, the run is being cancelled or the drain ran
		// out. Whatever came back, nothing is recorded for the call.
		return x.halted(run, false)
	case err != nil && outOfTime:
		return x.finish(run, StatusFailed, ReasonTimeBudget, "",
			fmt.Sprintf("the model call at step %d was cut off when the time budget ran out", seq))
	case err != nil && errors.Is(err, ErrPermanent):
		return x.finish(run, StatusFailed, ReasonError, "", err.Error())
	case err != nil:
		return x.fail(run, err, false)
	case !knownStop(resp.Stop):
		// Journaled, a reply the planner cannot place would be given up once
		// for every execution allowed. No retry changes what a model's
		// adapter does not know, so it is refused here as a permanent error.
		return x.finish(run, StatusFailed, ReasonError, "",
			fmt.Sprintf("the model's reply at step %d stopped for %q, which this build does not know", seq, resp.Stop))
	}

	// A reply is the assistant's turn, whatever role its model wrote on it.
	msg := resp.Message
	msg.Role = RoleAssistant
	now = x.e.clock.Now()
	err = x.e.store.CompleteModel(x.work, x.lease, CompleteModelRequest{
		Seq: seq, Message: msg, Stop: resp.Stop, Model: resp.Model, Usage: resp.Usage, Now: now,
	})
	if err != nil {
		return x.broke(run, fmt.Errorf("record the model's reply at step %d: %w", seq, err))
	}
	x.e.publish(x.step, EventStepCompleted, run, seq, now)
	return going, nil
}

func knownStop(stop Stop) bool {
	switch stop {
	case StopEnd, StopToolUse, StopMaxTokens, StopRefusal, StopPause, StopContextWindow:
		return true
	}
	return false
}

// timeLeft is what is left of run's time budget, and false when it has none.
func timeLeft(run Run) (time.Duration, bool) {
	limit := filledLimits(run.Definition.Limits).MaxDuration
	if limit < 0 {
		return 0, false
	}
	return limit - time.Duration(run.ActiveMillis)*time.Millisecond, true
}

// judge performs actJudge: a proposed step is refused, put to a person, or
// started and carried out. Until its one write lands the step is proposed,
// and a later execution asks the guard again.
func (x *execution) judge(run Run, def Definition, st Step) (ending, error) {
	tool, known := toolOf(def, st.Name)
	switch {
	case st.Call == nil || st.Call.Malformed:
		return x.settle(run, st, StepCompleted, resultMalformed, Usage{})
	case !known:
		return x.settle(run, st, StepCompleted, resultUnavailable, Usage{})
	}

	act, described := x.describe(run, tool, invocation(run, st, st.Attempts+1))
	if !described {
		return x.block(run, st, ruleActionPanicked, resultBlocked+ruleActionPanicked)
	}

	decision := Decision{Effect: Allow, Rule: RuleNoGuard}
	if x.e.opts.Guard != nil {
		var err error
		decision, err = x.e.opts.Guard.Decide(x.work, act)
		switch {
		case err == nil:
		case x.work.Err() != nil:
			return x.halted(run, false)
		case errors.Is(err, ErrPermanent):
			// No retry will get this decision recorded, and a call whose
			// decision is on no record is not run.
			x.e.log.ErrorContext(x.step, "agent: a guard's decision can never be recorded; the call is refused",
				"run", run.ID, "seq", st.Seq, "tool", st.Name, "error", err)
			return x.block(run, st, ruleUnrecordable, resultUnrecordable)
		default:
			// Neither an allow nor a block: the step stays proposed.
			return x.fail(run, err, false)
		}
	}

	switch {
	case decision.Effect == Ask:
		return x.request(run, st, ApprovalRequest{
			From: StepProposed, Cause: CauseGuard, Action: act, Decision: Ask, Rule: decision.Rule,
		})
	case decision.Effect == Allow && tool.Approval:
		return x.request(run, st, ApprovalRequest{
			From: StepProposed, Cause: CauseTool, Action: act, Decision: Allow, Rule: RuleToolApproval,
		})
	case decision.Effect == Allow:
		if x.caller.Err() != nil {
			// The caller's context ended while the guard was asked. No call
			// is started that a shutdown would then cut off.
			return x.yield(run, context.Cause(x.caller))
		}
		return x.start(run, def, st, Allow, decision.Rule)
	}
	return x.block(run, st, decision.Rule, resultBlocked+decision.Rule)
}

// describe is actionFor with a panic in the tool's own Action function
// recovered and logged. It reports false for one that panicked.
func (x *execution) describe(run Run, tool Tool, in Invocation) (act Action, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			x.e.log.ErrorContext(x.step, "agent: a tool's Action function panicked",
				"tool", tool.Name, "run", run.ID, "seq", in.Seq,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			act, ok = Action{}, false
		}
	}()
	return actionFor(run, tool, in), true
}

// invocation is the execution of st's call that is numbered attempt.
func invocation(run Run, st Step, attempt int) Invocation {
	in := Invocation{RunID: run.ID, Agent: run.Agent, Seq: st.Seq, Attempt: attempt, Key: st.Key}
	if st.Call != nil {
		in.Call = *st.Call
	}
	return in
}

// block refuses a proposed step for good: proposed to blocked.
func (x *execution) block(run Run, st Step, rule, result string) (ending, error) {
	return x.record(run, StepUpdate{
		Seq: st.Seq, From: StepProposed, To: StepBlocked,
		Decision: Block, Rule: rule, Result: &result, IsError: true,
	})
}

// settle gives st a final status and an error result that no tool produced.
// usage is a child run's, when the result is that child's failure.
func (x *execution) settle(run Run, st Step, to StepStatus, result string, usage Usage) (ending, error) {
	return x.record(run, StepUpdate{
		Seq: st.Seq, From: st.Status, To: to, Result: &result, IsError: true, Usage: usage,
	})
}

// record writes one move of a step, and the loop goes round again.
func (x *execution) record(run Run, update StepUpdate) (ending, error) {
	if err := x.move(run, update); err != nil {
		return x.broke(run, err)
	}
	return going, nil
}

// move writes one move of a tool step, with the time and with texts the
// journal can hold, and publishes it.
func (x *execution) move(run Run, update StepUpdate) error {
	update.Now = x.e.clock.Now()
	update.Rule = journalText(update.Rule)
	if update.Result != nil {
		result := journalText(*update.Result)
		update.Result = &result
	}
	if err := x.e.store.UpdateStep(x.work, x.lease, update); err != nil {
		return fmt.Errorf("move step %d from %s to %s: %w", update.Seq, update.From, update.To, err)
	}
	switch update.To {
	case StepStarted:
		x.e.publish(x.step, EventStepStarted, run, update.Seq, update.Now)
	case StepBlocked:
		x.e.publish(x.step, EventStepBlocked, run, update.Seq, update.Now)
	case StepCompleted, StepDeclined:
		x.e.publish(x.step, EventStepCompleted, run, update.Seq, update.Now)
	}
	return nil
}

// start performs actRun, and the second half of actJudge for a call that is
// allowed: the step is started from where it stands, which counts the
// attempt, and then its tool is run or its child started. decision and rule
// are the guard's answer, when this is the move that records it.
//
// The start is written before anything is done, so whatever stops the
// execution after it leaves a started step: the journal never holds less
// than what was done.
func (x *execution) start(run Run, def Definition, st Step, decision Effect, rule string) (ending, error) {
	err := x.move(run, StepUpdate{Seq: st.Seq, From: st.Status, To: StepStarted, Decision: decision, Rule: rule})
	if err != nil {
		return x.broke(run, err)
	}
	st.Status = StepStarted
	st.Attempts++

	tool, known := toolOf(def, st.Name)
	switch {
	case !known:
		// The call was in flight, or waiting for a person, when a deploy
		// removed its tool.
		return x.settle(run, st, StepCompleted, resultUnavailable, Usage{})
	case tool.Delegate != "":
		return x.spawn(run, tool, st)
	}

	// The time left in the budget goes in the timeout and not in the
	// context: a context that ran out would leave nothing to record, and the
	// run would be tried again until its failures ended it. next gives no
	// work once the time is spent, so what is left is more than nothing.
	timeout := tool.Timeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	if left, bounded := timeLeft(run); bounded && left < timeout {
		timeout = left
	}
	out := invoke(x.work, tool, invocation(run, st, st.Attempts), timeout, x.e.log)
	if out.retry != nil {
		if x.work.Err() != nil {
			// The tool was abandoned, and may be running still.
			return x.halted(run, true)
		}
		// The tool's own transient error: the step stays started, and the
		// call is made again with the same key.
		return x.fail(run, out.retry, false)
	}
	return x.record(run, StepUpdate{
		Seq: st.Seq, From: StepStarted, To: StepCompleted, Result: &out.result, IsError: out.isError,
	})
}

// spawn performs actSpawn, and what start does for a tool that delegates:
// the child run is created under the step's key, so that asked twice it is
// created once, and the step waits on it. A delegation that cannot be made
// completes the step with an error instead.
func (x *execution) spawn(run Run, tool Tool, st Step) (ending, error) {
	def, registered := x.e.definition(tool.Delegate)
	switch {
	case run.Depth >= x.e.opts.MaxDepth:
		return x.settle(run, st, StepCompleted,
			fmt.Sprintf("delegation refused: the depth limit of %d is reached", x.e.opts.MaxDepth), Usage{})
	case !registered:
		return x.settle(run, st, StepCompleted,
			fmt.Sprintf("delegation refused: agent %q is not registered", tool.Delegate), Usage{})
	}

	// The child's cost limit is the smaller of its own and what its parent
	// has left, where a limit below zero is none. next gives no work once
	// the cost is spent, so what is left is more than nothing.
	limits := def.Limits
	if parent := filledLimits(run.Definition.Limits).MaxCostMicros; parent > 0 {
		left := parent - run.Usage.CostMicros
		if own := filledLimits(limits).MaxCostMicros; own < 0 || left < own {
			limits.MaxCostMicros = left
		}
	}
	var input string
	if st.Call != nil {
		input = journalText(string(st.Call.Input))
	}
	now := x.e.clock.Now()
	child, created, err := x.e.store.CreateRun(x.work, Run{
		ID:         x.e.opts.NewID(),
		Agent:      def.Name,
		Status:     StatusRunnable,
		Input:      input,
		ParentID:   run.ID,
		ParentSeq:  st.Seq,
		Depth:      run.Depth + 1,
		Key:        st.Key,
		Definition: snapshotOf(def, limits),
		CreatedAt:  now,
		UpdatedAt:  now,
	})
	switch {
	case err != nil:
		return x.broke(run, fmt.Errorf("start the child run of step %d: %w", st.Seq, err))
	case child.ParentID != run.ID || child.ParentSeq != st.Seq:
		// A run somebody started with this step's key. It is not this run's
		// child, and its end would never wake this run.
		return x.settle(run, st, StepCompleted, "delegation refused: its key belongs to another run", Usage{})
	case created:
		x.e.publish(x.step, EventRunStarted, child, 0, now)
	}
	return x.record(run, StepUpdate{Seq: st.Seq, From: StepStarted, To: StepWaiting, ChildRunID: child.ID})
}

// collect performs actCollect: the ended child's outcome becomes the step's
// result, and its usage the run's.
func (x *execution) collect(run Run, st Step, child Run) (ending, error) {
	if child.Status != StatusCompleted {
		return x.settle(run, st, StepCompleted,
			fmt.Sprintf("%s %s: %s", child.Agent, child.Status, child.Reason), child.Usage)
	}
	return x.record(run, StepUpdate{
		Seq: st.Seq, From: StepWaiting, To: StepCompleted, Result: &child.Output, Usage: child.Usage,
	})
}

// resolve performs actResolve: an approval that will never be a yes becomes
// the step's result. An interrupted call's result says so first, whoever
// declined it: the model must know the call may have happened.
func (x *execution) resolve(run Run, st Step, asked Approval) (ending, error) {
	var result string
	switch asked.Status {
	case ApprovalExpired:
		result = resultExpired
	case ApprovalCancelled:
		result = resultCancelled
	default:
		result = "declined by " + asked.DecidedBy
		if asked.Reason != "" {
			result += ": " + asked.Reason
		}
	}
	if asked.Cause == CauseInterrupted {
		if asked.Status == ApprovalDeclined {
			result = resultInterrupted + ": " + result
		} else {
			result = resultInterrupted
		}
	}
	return x.settle(run, st, StepDeclined, result, Usage{})
}

// ask performs actAsk: a person is asked whether an interrupted at-most-once
// call is to be made again. No decision goes with the question, so the
// guard's answer stays on the step.
func (x *execution) ask(run Run, def Definition, st Step) (ending, error) {
	tool, _ := toolOf(def, st.Name)
	in := invocation(run, st, st.Attempts+1)
	act, described := x.describe(run, tool, in)
	if !described {
		// The person is asked all the same, and shown the call as a tool
		// with no Action of its own is described.
		act = actionFor(run, Tool{Name: tool.Name}, in)
	}
	return x.request(run, st, ApprovalRequest{
		From: StepStarted, Cause: CauseInterrupted, Action: act, Rule: RuleInterrupted,
	})
}

// request puts st to a person. The store hands back the approval already
// recorded for the step's attempt when there is one, and then the step has
// not moved: asking again would be handed it again, so the execution ends as
// failed.
func (x *execution) request(run Run, st Step, req ApprovalRequest) (ending, error) {
	req.ID = x.e.opts.NewID()
	req.Seq = st.Seq
	req.Rule = journalText(req.Rule)
	req.Now = x.e.clock.Now()
	if x.e.opts.ApprovalTTL > 0 {
		expires := req.Now.Add(x.e.opts.ApprovalTTL)
		req.ExpiresAt = &expires
	}
	asked, err := x.e.store.RequestApproval(x.work, x.lease, req)
	switch {
	case err != nil:
		return x.broke(run, fmt.Errorf("ask a person about step %d: %w", st.Seq, err))
	case asked.ID != req.ID:
		return x.fail(run, fmt.Errorf("step %d is %s and already has approval %s for attempt %d, which is %s",
			st.Seq, st.Status, asked.ID, asked.Attempt, asked.Status), false)
	}
	x.e.publish(x.step, EventApprovalRequested, run, st.Seq, req.Now)
	return going, nil
}

// park performs actPark. When the store says there is nothing to wait for,
// something changed after the journal was read, and the loop goes round
// again under a new keeper. Twice at one Rev, nothing changed and nothing
// will: the execution ends as failed.
func (x *execution) park(run Run, reason string) (ending, error) {
	if x.released() {
		return x.lost(run, x.cause)
	}
	ctx, cancel := x.last()
	defer cancel()
	now := x.e.clock.Now()
	parked, err := x.e.store.Park(ctx, x.lease, ParkRequest{Reason: reason, Now: now})
	switch {
	case err != nil:
		return x.unwritten(run, fmt.Errorf("park the run: %w", err))
	case parked:
		x.e.publish(ctx, EventRunWaiting, run, 0, now)
		return endedParked, nil
	case x.refused && x.refusedAt == run.Rev:
		return x.fail(run, fmt.Errorf("the run waits for %s, and the store twice found nothing for it to wait for at revision %d",
			reason, run.Rev), false)
	}
	x.refused, x.refusedAt = true, run.Rev
	x.hold()
	return going, nil
}

// finish performs actFinish, and ends a run for what next does not see: a
// model's permanent error, the last failure allowed, a run abandoned.
//
// A run that fails or is cancelled first asks each of its children that has
// not ended to stop, under its own lease and before the write that ends it:
// a crash in between leaves a run that is still to be finished, and the
// execution that finishes it asks again. The other way round, a crash would
// leave children running for a parent that has ended, and nothing to stop
// them.
func (x *execution) finish(run Run, status Status, reason, output, failure string) (ending, error) {
	if status != StatusCompleted {
		if err := x.cancelChildren(run, status, reason); err != nil {
			return x.unwritten(run, fmt.Errorf("ask the run's children to stop: %w", err))
		}
	}
	if x.released() {
		return x.lost(run, x.cause)
	}
	ctx, cancel := x.last()
	defer cancel()
	now := x.e.clock.Now()
	err := x.e.store.Finish(ctx, x.lease, FinishRequest{
		Status: status, Reason: reason, Output: journalText(output), Error: journalText(failure), Now: now,
	})
	if err != nil {
		return x.unwritten(run, fmt.Errorf("finish the run as %s: %w", status, err))
	}
	switch status {
	case StatusCompleted:
		x.e.publish(ctx, EventRunCompleted, run, 0, now)
		return endedCompleted, nil
	case StatusCancelled:
		x.e.publish(ctx, EventRunCancelled, run, 0, now)
		return endedCancelled, nil
	}
	x.e.publish(ctx, EventRunFailed, run, 0, now)
	return endedFailed, nil
}

// cancelChildren asks every child of run that has not ended to stop. The
// children are found by their parent and not from the journal: one created
// just before a crash is on no step. Each is marked in a transaction of its
// own, and one that ended meanwhile is left as it is.
//
// The request is not fenced, so the hold is checked before each: a worker
// that has lost the run asks nothing of its children.
func (x *execution) cancelChildren(run Run, status Status, reason string) error {
	by, why := "run "+run.ID, fmt.Sprintf("parent run %s failed: %s", run.ID, reason)
	if status == StatusCancelled {
		why = fmt.Sprintf("parent run %s was cancelled", run.ID)
		if run.CancelBy != "" {
			by = run.CancelBy
		}
		if run.CancelReason != "" {
			why += ": " + run.CancelReason
		}
	}

	var before *Cursor
	for {
		children, err := x.e.store.ListRuns(x.work, RunFilter{ParentID: run.ID, Before: before, Limit: maxListLimit})
		if err != nil {
			return err
		}
		for _, child := range children {
			if child.Terminal() {
				continue
			}
			if cause := x.interrupted(); cause != nil {
				return cause
			}
			now := x.e.clock.Now()
			err := x.e.store.RequestCancel(x.work, CancelRequest{
				RunID: child.ID, By: journalText(by), Reason: journalText(why), Now: now,
			})
			switch {
			case errors.Is(err, ErrFinished):
			case err != nil:
				return fmt.Errorf("child run %s: %w", child.ID, err)
			case !child.CancelRequested:
				x.e.publish(x.step, EventRunCancelRequested, child, 0, now)
			}
		}
		if len(children) < maxListLimit {
			return nil
		}
		last := children[len(children)-1]
		before = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// fail ends the execution as one whose step failed (6.6): the run is given
// back with one more failure and hidden for a wait that doubles from
// RetryBase to RetryMax, or for RetryMax at once when longest is set. When
// this is failure number MaxFailures the run is finished as failed instead,
// with the failure as its error.
//
// The count is read fresh, since a step that completed during this
// execution has reset it.
func (x *execution) fail(run Run, failure error, longest bool) (ending, error) {
	current, err := x.e.store.GetRun(x.work, run.ID)
	if err != nil {
		return x.unwritten(run, errors.Join(failure, fmt.Errorf("read the run to record the failure: %w", err)))
	}
	text := journalText(failure.Error())
	number := current.Failures + 1
	if number >= x.e.opts.MaxFailures {
		return x.finish(current, StatusFailed, ReasonError, "", text)
	}

	if x.released() {
		return x.lost(run, x.cause)
	}
	wait := x.e.opts.RetryMax
	if !longest {
		wait = x.e.opts.RetryBase
		for range number - 1 {
			if wait > x.e.opts.RetryMax/2 {
				wait = x.e.opts.RetryMax
				break
			}
			wait *= 2
		}
		wait = min(wait, x.e.opts.RetryMax)
	}
	ctx, cancel := x.last()
	defer cancel()
	now := x.e.clock.Now()
	again := now.Add(wait)
	err = x.e.store.Yield(ctx, x.lease, YieldRequest{Failed: true, Error: text, NextAttemptAt: &again, Now: now})
	if err != nil {
		return x.unwritten(run, errors.Join(failure, fmt.Errorf("give the run back as failed: %w", err)))
	}
	x.e.log.WarnContext(ctx, "agent: an execution failed; the run will be tried again",
		"run", run.ID, "agent", run.Agent, "failures", number, "again", again, "error", text)
	return endedYielded, failure
}

// yield gives the run back as it is, with no failure counted, because the
// context the execution was given has ended. That is the caller stopping,
// and not something the run did.
func (x *execution) yield(run Run, why error) (ending, error) {
	if x.released() {
		return x.lost(run, x.cause)
	}
	ctx, cancel := x.last()
	defer cancel()
	if err := x.e.store.Yield(ctx, x.lease, YieldRequest{Now: x.e.clock.Now()}); err != nil {
		return x.unwritten(run, errors.Join(why, fmt.Errorf("give the run back: %w", err)))
	}
	x.e.log.InfoContext(ctx, "agent: the run was given back for another worker",
		"run", run.ID, "agent", run.Agent, "reason", why)
	return endedYielded, why
}

// broke ends the execution for a store call that failed in the middle of an
// action.
func (x *execution) broke(run Run, err error) (ending, error) {
	switch {
	case errors.Is(err, ErrLeaseLost):
		return x.lost(run, err)
	case x.work.Err() != nil:
		// The call failed because its context ended under it.
		return x.halted(run, false)
	}
	return x.fail(run, err, false)
}

// halted is where an action goes once the context it works under has ended.
// When that was a request to cancel the run, the loop goes round again and
// finishes the run as cancelled. toolRunning says the action had a tool in
// flight, which may be running still.
func (x *execution) halted(run Run, toolRunning bool) (ending, error) {
	if cause := x.interrupted(); cause != nil {
		return x.cut(run, cause, toolRunning)
	}
	return going, nil
}

// cut ends an execution that cannot go on working. With the lease lost
// nothing is written. Otherwise the drain ran out, and the run is given back
// with no failure counted, unless a tool may be running still: then nothing
// is written and the lease is left to lapse, so that no other worker starts
// the call again while this one is still making it.
func (x *execution) cut(run Run, cause error, toolRunning bool) (ending, error) {
	switch {
	case x.step.Err() == nil:
		return x.lost(run, cause)
	case toolRunning:
		x.letGo()
		x.e.log.WarnContext(x.step, "agent: a tool was still running when the drain ran out; nothing is written and the lease is left to lapse",
			"run", run.ID, "agent", run.Agent)
		return endedUnwritten, cause
	}
	return x.yield(run, cause)
}

// lost ends an execution whose lease another process took: it writes nothing
// more, and does not give the run back, which is no longer its to give.
func (x *execution) lost(run Run, err error) (ending, error) {
	x.letGo()
	x.e.log.WarnContext(x.step, "agent: the run was lost to another worker; nothing more is written",
		"run", run.ID, "agent", run.Agent, "error", err)
	return endedUnwritten, err
}

// unwritten ends an execution whose last write the store did not take.
// Nothing is tried in its place: a write that failed says nothing good about
// the next, and the lease lapsing gives the run to an execution that makes
// it again.
func (x *execution) unwritten(run Run, err error) (ending, error) {
	x.letGo()
	if errors.Is(err, ErrLeaseLost) {
		return x.lost(run, err)
	}
	x.e.log.WarnContext(x.step, "agent: an execution could not let its run go; the lease is left to lapse",
		"run", run.ID, "agent", run.Agent, "error", err)
	return endedUnwritten, err
}
