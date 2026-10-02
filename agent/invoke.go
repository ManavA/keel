package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
)

// maxResultBytes is the largest result returned to the model.
const maxResultBytes = 1 << 20

// outcome is what one execution of a tool produced. retry is set, and the
// rest empty, when the tool's error wraps ErrTransient. It is then an error
// that unwraps to the tool's own and carries its text, read in the tool's
// goroutine and made fit for the journal, so the caller can record it
// without calling into the tool's code.
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
	// transient is the tool's error, wrapped, when it wraps ErrTransient.
	transient error
	// panicked is also set for a tool that ended its goroutine without
	// returning.
	panicked bool
}

// invoke runs tool.Run once: under timeout, with a panic recovered and
// logged to logger, an error turned into an error result, and a result the
// journal cannot hold refused: one over 1 MiB, one that is not valid UTF-8,
// one with a NUL byte. The fixed result texts are those in 6.4.
//
// timeout is the whole bound. The tool's own Timeout, its default, and what
// is left of the run's budget are the caller's to work out: invoke does not
// read Tool.Timeout. With a timeout of zero or less the tool is not run,
// and the result says so: "tool was given no time to run", an error result.
// A nil logger is slog.Default.
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
//     an error result. The value and the stack go to logger, with the tool,
//     the run, the step and the attempt. The call's arguments are not
//     logged, and the value is not put in the result.
//   - The tool's error wraps ErrTransient: retry is a transientError
//     holding it. Its text was read where a panic is recovered and made fit
//     for the journal by journalText, and errors.Is and errors.As see
//     through it to the tool's error.
//   - Any other error: its text, as an error result, made fit for the
//     journal by journalText. Only then is it measured: over 1 MiB it is
//     replaced by "result too large: <n> bytes".
//   - A result of more than 1 MiB: "result too large: <n> bytes", an error
//     result. The bound is on bytes.
//   - A result that is not valid UTF-8: "result is not valid UTF-8", an
//     error result.
//   - A result with a NUL byte in it: "result contains a NUL byte", an
//     error result.
//   - Otherwise the tool's result, unchanged.
//
// A result is refused where an error's text is repaired because the two are
// owed differently. A result is the tool's answer, and one with bytes
// changed is no longer that answer; an error's text only has to say what
// went wrong. Either way what invoke returns as a result is text Postgres
// will store, so the write that records it cannot fail for its content on
// every attempt and leave the run unable to move.
//
// The tool runs in a goroutine of its own, so one that ignores its context
// does not hold invoke past the timeout or past the end of ctx. That
// goroutine cannot be stopped. It is left to run until the tool returns,
// what it returns is dropped, and a panic in it is still recovered and
// logged. A tool that never returns therefore costs a goroutine for the
// life of the process, and may still be doing its work after the model has
// been told it timed out. After a takeover, such a tool runs on beside the
// next worker's execution of the same call, for as long as it likes: the
// two share the idempotency key and nothing else. Honouring the context is
// the tool's part.
func invoke(ctx context.Context, tool Tool, in Invocation, timeout time.Duration, logger *slog.Logger) outcome {
	if logger == nil {
		logger = slog.Default()
	}
	if ctx.Err() != nil {
		return outcome{retry: context.Cause(ctx)}
	}
	if timeout <= 0 {
		return outcome{result: "tool was given no time to run", isError: true}
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
				logger.ErrorContext(ctx, "agent: tool panicked",
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
			ret.transient = &transientError{text: journalText(err.Error()), err: err}
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
		return outcome{result: "timed out after " + timeout.String(), isError: true}
	case ret.panicked:
		return outcome{result: "tool panicked", isError: true}
	case ret.transient != nil:
		return outcome{retry: ret.transient}
	case ret.failed:
		text := journalText(ret.text)
		if len(text) > maxResultBytes {
			return tooLarge(len(text))
		}
		return outcome{result: text, isError: true}
	case len(ret.text) > maxResultBytes:
		return tooLarge(len(ret.text))
	case !utf8.ValidString(ret.text):
		return outcome{result: "result is not valid UTF-8", isError: true}
	case strings.IndexByte(ret.text, 0) >= 0:
		return outcome{result: "result contains a NUL byte", isError: true}
	}
	return outcome{result: ret.text}
}

func tooLarge(bytes int) outcome {
	return outcome{result: fmt.Sprintf("result too large: %d bytes", bytes), isError: true}
}

// transientError is a tool's transient error with its text already read.
// Whatever records the error reads the text, and does so on the executor's
// goroutine, where a panic in the tool's own Error method would not be
// recovered.
type transientError struct {
	text string
	err  error
}

func (e *transientError) Error() string { return e.text }
func (e *transientError) Unwrap() error { return e.err }

// journalText is text as the journal can hold it. Postgres refuses, in a
// text column, bytes that are not UTF-8 and the NUL byte: the first are
// replaced by U+FFFD, a run of them by one, and then the second are removed.
// In that order, so that a NUL between the two halves of a character does
// not leave a character the text never had. Text that has neither comes
// back as it is.
func journalText(text string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(text, "\uFFFD"), "\x00", "")
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
