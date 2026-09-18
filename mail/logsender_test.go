package mail

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogSender_Send_DefaultDoesNotLogRecipientOrModel is the regression
// test for LogSender writing a recipient address and a full template model
// (which can carry a password-reset token or a magic link) to the log by
// default, since LogSender is what runs in production when a provider was
// never configured.
func TestLogSender_Send_DefaultDoesNotLogRecipientOrModel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := NewLogSender(LogSenderOptions{Logger: logger})

	err := s.Send(context.Background(), "victim@example.com", "password-reset",
		map[string]any{"reset_token": "SECRET-TOKEN-abc123"})
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "password-reset", "the template alias is not sensitive and is always logged")
	assert.NotContains(t, out, "victim@example.com")
	assert.NotContains(t, out, "SECRET-TOKEN-abc123")
}

func TestLogSender_Send_HashIsStableForTheSameRecipient(t *testing.T) {
	var buf1, buf2 bytes.Buffer
	s1 := NewLogSender(LogSenderOptions{Logger: slog.New(slog.NewTextHandler(&buf1, nil))})
	s2 := NewLogSender(LogSenderOptions{Logger: slog.New(slog.NewTextHandler(&buf2, nil))})

	require.NoError(t, s1.Send(context.Background(), "Jane@Example.com", "welcome", nil))
	require.NoError(t, s2.Send(context.Background(), " jane@example.com ", "welcome", nil))

	// Extract the to_hash value the crude way: both logs must agree.
	require.NotEmpty(t, buf1.String())
	assert.Equal(t, extractField(t, buf1.String(), "to_hash"), extractField(t, buf2.String(), "to_hash"),
		"the same address (modulo case and whitespace) must hash the same, so repeated sends can be correlated")
}

func TestLogSender_Send_LogBodiesOptsIntoTheFullModelAtDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := NewLogSender(LogSenderOptions{Logger: logger, LogBodies: true})

	require.NoError(t, s.Send(context.Background(), "jane@example.com", "welcome", map[string]any{testNameKey: testName}))

	out := buf.String()
	assert.Contains(t, out, "jane@example.com")
	assert.Contains(t, out, "level=DEBUG")
}

func TestLogSender_Send_LogBodiesFalseSuppressesDebugLineEvenAtDebugLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := NewLogSender(LogSenderOptions{Logger: logger}) // LogBodies defaults false

	require.NoError(t, s.Send(context.Background(), "jane@example.com", "welcome", map[string]any{testNameKey: testName}))

	assert.NotContains(t, buf.String(), "jane@example.com",
		"LogBodies must default false, even when the logger's own level would allow Debug output")
}

func TestLogSender_DeliversToProvider_IsFalse(t *testing.T) {
	s := NewLogSender(LogSenderOptions{})
	assert.False(t, s.DeliversToProvider())
	assert.False(t, DeliversToProvider(s))
}

func TestLogSender_NilLoggerFallsBackToDefault(t *testing.T) {
	s := NewLogSender(LogSenderOptions{})
	assert.NotPanics(t, func() {
		_ = s.Send(context.Background(), "a@b.com", "welcome", nil)
	})
}

// extractField pulls a "key=value" pair out of slog's text handler output,
// good enough for a test assertion without pulling in a full log parser.
func extractField(t *testing.T, log, key string) string {
	t.Helper()
	idx := indexOf(log, key+"=")
	require.GreaterOrEqual(t, idx, 0, "field %q not found in log output: %s", key, log)
	rest := log[idx+len(key)+1:]
	end := indexOf(rest, " ")
	if end < 0 {
		end = indexOf(rest, "\n")
	}
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
