package mail

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogSender_Send_LogsAndReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := NewLogSender(LogSenderOptions{Logger: logger})

	err := s.Send(context.Background(), "a@b.com", "welcome", map[string]any{"name": "Jane"})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "a@b.com")
	assert.Contains(t, buf.String(), "welcome")
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
