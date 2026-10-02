package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"time"
)

// maxResultBytes is the largest result returned to the model.
const maxResultBytes = 1 << 20

// outcome is what one execution of a tool produced. retry is set, and the
// rest empty, when the tool's error wraps ErrTransient.
//
// retry is also set, and the rest empty, when the context invoke was given
// ended before a result was taken: it is then that context's cause. Either
// way there is nothing to record, and the caller reads its own context to
// tell the two apart.
type outcome struct {
	result  string
	isError bool
	retry   error
}

// toolReturn is how a tool's goroutine ended, in values that are safe to
// read outside it.
type toolReturn struct {
	// text is the tool's result, or its error's text when failed is set.
	text   string
	failed bool
	// transient is the tool's error when it wraps ErrTransient.
	transient error
	// panicked is also set for a tool that ended its goroutine without
	// returning.
	panicked bool
}

// invoke runs tool.Run once: under timeout, with a panic recovered and
// logged, an error turned into an error result, and a result over 1 MiB
// refused. The fixed result texts are those in 6.4.
//
// timeout is the whole bound. The tool's own Timeout, its default, and what
// is left of the run's budget are the caller's to work out: invoke does not
// read Tool.Timeout. A timeout of zero or less has already passed, so the
// tool is not run and the result is the timed-out text.
//
// What comes back, in the order the cases are tried:
//
//   - ctx has ended, before the call or by the time the tool returned:
//     retry is ctx's cause and nothing else is set, whatever the tool
//     returned. A context that has ended before the call does not run the
//     tool.
//   - The timeout has passed: "timed out after <timeout>", an error result,
//     whatever the tool returned once its time was up. A context of the
//     caller's that runs out first is the case above and not this one.
//   - The tool panicked, a tool with no Run among them: "tool panicked",
//     an error result. The value and the stack go to the default logger,
//     since invoke is given none, with the tool, the run, the step and the
//     attempt. The call's arguments are not logged.
//   - The tool's error wraps ErrTransient: retry is that error.
//   - Any other error: its text, as an error result.
//   - Otherwise the tool's result.
//
// A result, or an error's text, of more than 1 MiB is replaced by
// "result too large: <n> bytes", an error result.
//
// The tool runs in a goroutine of its own, so one that ignores its context
// does not hold invoke past the timeout or past the end of ctx. That
// goroutine cannot be stopped. It is left to run until the tool returns,
// what it returns is dropped, and a panic in it is still recovered and
// logged. A tool that never returns therefore costs a goroutine for the
// life of the process, and may still be doing its work after the model has
// been told it timed out: honouring the context is the tool's part.
func invoke(ctx context.Context, tool Tool, in Invocation, timeout time.Duration) outcome {
	if ctx.Err() != nil {
		return outcome{retry: context.Cause(ctx)}
	}
	if timeout <= 0 {
		return timedOut(timeout)
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Room for one, so a tool that returns after invoke has stopped waiting
	// does not block for ever on a result nobody takes.
	returned := make(chan toolReturn, 1)
	go func() {
		ret := toolReturn{panicked: true}
		defer func() {
			if ret.panicked {
				value := recover()
				if value == nil {
					// Not a panic: the tool called runtime.Goexit, which
					// leaves no result either.
					value = "the tool ended its goroutine without returning"
				}
				slog.ErrorContext(ctx, "agent: tool panicked",
					"tool", tool.Name, "run", in.RunID, "agent", in.Agent, "seq", in.Seq, "attempt", in.Attempt,
					"panic", fmt.Sprint(value), "stack", string(debug.Stack()))
			}
			returned <- ret
		}()

		result, err := tool.Run(bounded, in)
		// An error's methods are the tool's code too, so they are called
		// here, where a panic in one is recovered.
		switch {
		case err == nil:
			ret.text = result
		case errors.Is(err, ErrTransient):
			ret.transient = err
		default:
			ret.text, ret.failed = err.Error(), true
		}
		ret.panicked = false
	}()

	var ret toolReturn
	select {
	case ret = <-returned:
	case <-bounded.Done():
	}
	switch {
	case ctx.Err() != nil:
		return outcome{retry: context.Cause(ctx)}
	case bounded.Err() != nil:
		return timedOut(timeout)
	case ret.panicked:
		return outcome{result: "tool panicked", isError: true}
	case ret.transient != nil:
		return outcome{retry: ret.transient}
	case len(ret.text) > maxResultBytes:
		return outcome{result: fmt.Sprintf("result too large: %d bytes", len(ret.text)), isError: true}
	}
	return outcome{result: ret.text, isError: ret.failed}
}

func timedOut(timeout time.Duration) outcome {
	return outcome{result: "timed out after " + timeout.String(), isError: true}
}

// actionFor is the Action the Guard is asked about: the tool's own, or the
// default for its kind, with AttrAgent, AttrTool, AttrRun and AttrSeq set
// where absent.
//
// The default is Action{Kind: "run", Target: tool.Name}, and for a tool that
// delegates Action{Kind: "delegate", Target: tool.Delegate}.
//
// The four attributes, under the exact names a rule must use for them:
//
//	"agent"  run.Agent, a string
//	"tool"   tool.Name, a string
//	"run"    run.ID, a string
//	"seq"    in.Seq, an int
//
// Each is added only when the tool's Action has no attribute of that name.
// One the tool set, to anything at all, nil included, is left as it is.
//
// Nothing is normalised. A guard matches kinds, targets, attribute names
// and string values exactly as written and compares numbers exactly, so
// what a tool's Action returns reaches the guard as the tool wrote it: no
// case folded, no space trimmed, no type converted. One canonical spelling
// is the tool's to keep. actionFor decodes nothing; an Action that reads
// the call's arguments decodes them itself, with json.Decoder.UseNumber so
// that a number arrives unrounded.
//
// The Attrs returned are a map of their own: the one the tool returned,
// which it may hand out again for another call, is not written to.
func actionFor(run Run, tool Tool, in Invocation) Action {
	var a Action
	switch {
	case tool.Action != nil:
		a = tool.Action(in)
	case tool.Delegate != "":
		a = Action{Kind: "delegate", Target: tool.Delegate}
	default:
		a = Action{Kind: "run", Target: tool.Name}
	}

	attrs := make(map[string]any, len(a.Attrs)+4)
	maps.Copy(attrs, a.Attrs)
	for _, added := range []struct {
		name  string
		value any
	}{
		{AttrAgent, run.Agent},
		{AttrTool, tool.Name},
		{AttrRun, run.ID},
		{AttrSeq, in.Seq},
	} {
		if _, set := attrs[added.name]; !set {
			attrs[added.name] = added.value
		}
	}
	a.Attrs = attrs
	return a
}
