package jobs

import (
	"context"
	"log/slog"
)

// Trigger starts another job by name. It is the interface a pipeline stage
// uses to start the next stage directly, instead of waiting for that next
// stage's own independent [Entry] tick — useful when stage B should start
// the moment stage A finishes rather than up to one whole Interval later.
//
// jobs/cloudrun implements this for Google Cloud Run, in its own package so
// that a caller using only the in-process default here does not pull in
// golang.org/x/oauth2/google merely by importing jobs.
type Trigger interface {
	Run(ctx context.Context, jobName string) error
}

// NoopTrigger stands in when nothing should actually be started — local
// runs, tests, or a pipeline stage that has not been wired to a real
// scheduler yet. It is the default Trigger: no external service is
// required to use this package.
type NoopTrigger struct {
	// Logger, if set, receives a debug line per call. Nil is silent.
	Logger *slog.Logger
}

// Run logs the request (if a Logger is set) and returns nil without
// starting anything.
func (t NoopTrigger) Run(_ context.Context, jobName string) error {
	if t.Logger != nil {
		t.Logger.Debug("job trigger disabled", "job", jobName)
	}
	return nil
}
