package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/metrics"
)

// TestRunner_Run_RecordsOutcomeStatus pins the metrics hook: one run
// counts once under its own status word, and a run that errored counts
// as failed even when its counts alone would read as success.
func TestRunner_Run_RecordsOutcomeStatus(t *testing.T) {
	mem := metrics.NewInMemory()
	r := NewRunner(RunnerOptions{Metrics: mem.Metrics()})

	r.Run(context.Background(), "nightly", func(context.Context) (Outcome, error) {
		return Outcome{Attempted: 3, Succeeded: 1, Failed: 2}, nil
	})
	r.Run(context.Background(), "nightly", func(context.Context) (Outcome, error) {
		return Outcome{Attempted: 3, Succeeded: 3}, errors.New("boom")
	})

	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameJobRuns,
		metrics.String(metrics.AttrJobName, "nightly"),
		metrics.String(metrics.AttrJobStatus, StatusPartial)))
	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NameJobRuns,
		metrics.String(metrics.AttrJobName, "nightly"),
		metrics.String(metrics.AttrJobStatus, StatusFailed)))
}
