package jobs

import "log/slog"

// Outcome is a job run's result, computed from what the run counted rather
// than set directly by the code that logs the run's last line. See the
// package doc for why.
type Outcome struct {
	// Attempted is how many units of work this run picked up. Zero means
	// the queue was empty, which is a healthy state, distinct from "there
	// was work, and none of it landed."
	Attempted int

	// Succeeded is how many of those units landed.
	Succeeded int

	// Failed is how many did not. Attempted may exceed Succeeded+Failed
	// when a run is cut short — a deadline, a canceled context — and that
	// gap is deliberately visible rather than reconciled away.
	Failed int

	// Fatal marks a step that had to succeed and did not: a prune skipped
	// because the authoritative set could not be read, a dependency that
	// returned an error before any work could even be attempted. "Could
	// not measure" is not the same claim as "measured, and fine."
	Fatal bool
}

// Status words a run can report. Five, not two, because the interesting
// cases — the ones a boolean cannot express — are the middle three.
const (
	// StatusFailed means a step that had to work did not.
	StatusFailed = "failed"
	// StatusDidNothing means there was work, and none of it landed. This
	// is the state a hardcoded "success" cannot distinguish from StatusIdle.
	StatusDidNothing = "did-nothing"
	// StatusPartial means some work landed and some did not.
	StatusPartial = "partial"
	// StatusIdle means the queue was empty: nothing was attempted, so
	// nothing succeeded — which is not the same claim as "success".
	StatusIdle = "idle"
	// StatusSuccess means work was attempted and all of it landed.
	StatusSuccess = "success"
)

// DidNothing reports the case a hardcoded "success" cannot see: the run had
// work in front of it and finished none of it.
func (o Outcome) DidNothing() bool {
	return o.Attempted > 0 && o.Succeeded <= 0
}

// Status is the word to print, derived from the counts above rather than
// chosen independently of them.
func (o Outcome) Status() string {
	switch {
	case o.Fatal:
		return StatusFailed
	case o.DidNothing():
		return StatusDidNothing
	case o.Attempted == 0:
		return StatusIdle
	case o.Failed > 0:
		return StatusPartial
	default:
		return StatusSuccess
	}
}

// OK reports whether this run may be called a success. An empty queue is
// OK; a full queue that produced nothing is not.
func (o Outcome) OK() bool {
	return !o.Fatal && !o.DidNothing()
}

// ExitCode is the process status this run earned. A run that had work and
// landed none of it must exit non-zero, or a scheduler that only restarts
// non-zero exits never retries it.
func (o Outcome) ExitCode() int {
	if o.OK() {
		return 0
	}
	return 1
}

// Complete emits a job run's terminal log line and returns the exit code the
// run earned.
//
// okMsg is emitted at Info, and ONLY when the counts say work actually
// landed — a monitoring rule that matches on message text alone (a common
// pattern for "this job did its work" alerting) must not see okMsg for a run
// that succeeded at nothing. failMsg is emitted instead, at Error, so it
// also lands in whatever "severity >= ERROR" alerting already exists.
//
// Callers pass their own domain counts in attrs; "status" and "ok" are
// appended from the Outcome, so the printed word and the printed numbers
// cannot disagree with each other.
func Complete(logger *slog.Logger, okMsg, failMsg string, o Outcome, attrs ...any) int {
	if logger == nil {
		logger = slog.Default()
	}
	attrs = append(attrs, "status", o.Status(), "ok", o.OK())
	if o.OK() {
		logger.Info(okMsg, attrs...)
	} else {
		logger.Error(failMsg, attrs...)
	}
	return o.ExitCode()
}
