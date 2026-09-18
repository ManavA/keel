package jobs

import "log/slog"

// WatchdogStatus is the result of a job that checks whether another job's
// claimed effect actually happened — for example, that messages a notifier
// logged as sent are present at the downstream provider.
//
// It has three states, not two, for the same reason [Outcome] does: a
// watchdog that finds no evidence either way has not verified anything, and
// must not report the same result as one that checked and found the claim
// correct. A watchdog that treats an empty query result as a pass will
// report success for a pipeline that has stopped running.
type WatchdogStatus string

const (
	// WatchdogVerified means the watchdog found evidence and it matched
	// what was claimed.
	WatchdogVerified WatchdogStatus = "verified"
	// WatchdogDiverged means the watchdog found evidence and it did NOT
	// match what was claimed.
	WatchdogDiverged WatchdogStatus = "diverged"
	// WatchdogInconclusive means the watchdog could not reach a verdict —
	// no evidence to check, or a control that should have failed did not.
	// This is deliberately NOT the same exit behavior as WatchdogVerified;
	// see [CompleteWatchdog].
	WatchdogInconclusive WatchdogStatus = "inconclusive"
)

// WatchdogResult is one watchdog run's verdict.
type WatchdogResult struct {
	Status WatchdogStatus
	// Checked is how many claims the watchdog actually compared against
	// evidence. Zero alongside WatchdogInconclusive is the expected,
	// honest shape for "there was nothing to check yet."
	Checked int
	// Reason is a human-readable explanation, always set — including for
	// WatchdogVerified, where it names what was checked rather than
	// leaving a bare "ok".
	Reason string
}

// CompleteWatchdog emits a watchdog run's terminal log line and returns the
// exit code the run earned.
//
// WatchdogInconclusive exits non-zero, the same as WatchdogDiverged.
// "Nothing to check" and "verified clean" must not share an exit code, or a
// caller that only checks the exit code cannot tell them apart.
func CompleteWatchdog(logger *slog.Logger, name string, r WatchdogResult, attrs ...any) int {
	if logger == nil {
		logger = slog.Default()
	}
	attrs = append(attrs, "status", string(r.Status), "checked", r.Checked, "reason", r.Reason)
	switch r.Status {
	case WatchdogVerified:
		logger.Info(name+" verified", attrs...)
		return 0
	case WatchdogDiverged:
		logger.Error(name+" diverged", attrs...)
		return 1
	default:
		logger.Error(name+" inconclusive", attrs...)
		return 1
	}
}
