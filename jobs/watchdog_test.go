package jobs

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompleteWatchdog_ExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		status   WatchdogStatus
		wantExit int
	}{
		{"verified exits zero", WatchdogVerified, 0},
		{"diverged exits non-zero", WatchdogDiverged, 1},
		{"inconclusive exits non-zero, same as diverged", WatchdogInconclusive, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			exit := CompleteWatchdog(logger, "delivery-check", WatchdogResult{
				Status: tt.status,
				Reason: "test reason",
			})
			assert.Equal(t, tt.wantExit, exit)
		})
	}
}

// TestCompleteWatchdog_InconclusiveIsNotLoggedAsAPass is the control this
// type exists for: an empty result set must never be reported the way a
// real, checked-clean pass is.
func TestCompleteWatchdog_InconclusiveIsNotLoggedAsAPass(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	exit := CompleteWatchdog(logger, "delivery-check", WatchdogResult{
		Status: WatchdogInconclusive,
		Reason: "no claims found to check",
	})

	require.Equal(t, 1, exit)
	out := buf.String()
	assert.Contains(t, out, "inconclusive")
	assert.NotContains(t, out, "delivery-check verified")
}

func TestCompleteWatchdog_NilLoggerFallsBackToDefault(t *testing.T) {
	assert.NotPanics(t, func() {
		CompleteWatchdog(nil, "check", WatchdogResult{Status: WatchdogVerified})
	})
}
