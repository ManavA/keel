// Package jobs runs background work: one-shot batch jobs and simple
// scheduled loops. It ensures a run that did nothing cannot report itself
// as a success.
//
// # Why Outcome and Complete exist
//
// A job's terminal log line is often written as a literal:
//
//	slog.Info("sync complete", "succeeded", succeeded, "failed", failed, "status", "success")
//
// The literal "success" is correct on the first run, and stays in the
// source unchanged after a dependency starts failing every request: the
// counters go to zero, the literal still reads "success", and the process
// still exits 0. A queue that is legitimately empty and a worker that is
// broken produce the same log line, and nothing downstream can distinguish
// them.
//
// [Outcome] computes the status word from the run's own counts instead.
// [Complete] applies that computation and returns the exit code the run
// computes. A run that had work and completed none of it is a distinct state,
// [StatusDidNothing], not a variant of success.
//
// # In-process by default
//
// [Scheduler] runs registered [Entry] values on their own tickers inside
// the calling process. No external scheduler service is required. [Trigger]
// and [CloudRunTrigger] are optional: use them only when one job should
// start another directly on a platform like Google Cloud Run; [NoopTrigger]
// is the default when no such wiring exists yet.
//
// # Other contents
//
//   - [Guard] and [Idempotent]: skip work that has already completed for a
//     given key, without a distributed lock.
//   - [WatchdogStatus] and [CompleteWatchdog]: the same three-state
//     status computation as Outcome, applied to a job that checks whether
//     another job's claimed effect actually happened. A watchdog that found
//     no evidence to check must not report the same result as one that
//     checked and found everything correct.
//
// Every dependency on an external system here — [Trigger] above all — is
// reached through a small interface, so this package's own tests use fakes
// rather than a live service.
package jobs
