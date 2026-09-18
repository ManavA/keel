package jobs

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutcome_Status(t *testing.T) {
	tests := []struct {
		name string
		o    Outcome
		want string
	}{
		{"fatal wins over everything", Outcome{Attempted: 5, Succeeded: 5, Fatal: true}, StatusFailed},
		{"did nothing: work attempted, none landed", Outcome{Attempted: 10, Succeeded: 0}, StatusDidNothing},
		{"idle: nothing attempted", Outcome{Attempted: 0}, StatusIdle},
		{"partial: some landed, some failed", Outcome{Attempted: 10, Succeeded: 7, Failed: 3}, StatusPartial},
		{"success: everything attempted landed", Outcome{Attempted: 10, Succeeded: 10}, StatusSuccess},
		{"negative succeeded still counts as did-nothing", Outcome{Attempted: 3, Succeeded: -1}, StatusDidNothing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.o.Status())
		})
	}
}

func TestOutcome_OKAndExitCode(t *testing.T) {
	tests := []struct {
		name     string
		o        Outcome
		wantOK   bool
		wantExit int
	}{
		{"idle queue is OK", Outcome{Attempted: 0}, true, 0},
		{"full success is OK", Outcome{Attempted: 5, Succeeded: 5}, true, 0},
		{"partial success is still OK", Outcome{Attempted: 5, Succeeded: 3, Failed: 2}, true, 0},
		{"did-nothing is NOT OK", Outcome{Attempted: 5, Succeeded: 0}, false, 1},
		{"fatal is NOT OK even with full counts", Outcome{Attempted: 5, Succeeded: 5, Fatal: true}, false, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantOK, tt.o.OK())
			assert.Equal(t, tt.wantExit, tt.o.ExitCode())
		})
	}
}

// TestComplete_MessageChoiceTracksOK is the control for Complete's whole
// reason for existing: a monitoring rule that matches on okMsg text alone
// must never see that message for a run that did not actually succeed.
func TestComplete_MessageChoiceTracksOK(t *testing.T) {
	tests := []struct {
		name       string
		o          Outcome
		wantOKMsg  bool
		wantExit   int
		wantStatus string
	}{
		{"healthy idle run logs okMsg", Outcome{Attempted: 0}, true, 0, StatusIdle},
		{"a run that did nothing logs failMsg, not okMsg", Outcome{Attempted: 8, Succeeded: 0}, false, 1, StatusDidNothing},
		{"a fatal run logs failMsg", Outcome{Attempted: 8, Succeeded: 8, Fatal: true}, false, 1, StatusFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			exit := Complete(logger, "OKMSG-MARKER", "FAILMSG-MARKER", tt.o)

			require.Equal(t, tt.wantExit, exit)
			out := buf.String()
			if tt.wantOKMsg {
				assert.Contains(t, out, "OKMSG-MARKER")
				assert.NotContains(t, out, "FAILMSG-MARKER")
			} else {
				assert.Contains(t, out, "FAILMSG-MARKER")
				assert.NotContains(t, out, "OKMSG-MARKER")
			}
			assert.Contains(t, out, "status="+tt.wantStatus)
		})
	}
}

func TestComplete_NilLoggerFallsBackToDefault(t *testing.T) {
	// Must not panic, and must not require slog.SetDefault to have been
	// called by the caller.
	assert.NotPanics(t, func() {
		Complete(nil, "ok", "fail", Outcome{Attempted: 0})
	})
}
