package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/ManavA/keel/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capture runs fn against a logger writing into a buffer and returns the single
// decoded record it produced.
func capture(t *testing.T, opts log.Options, fn func(*slog.Logger)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	opts.Output = &buf
	fn(log.New(opts))

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec), "output was %q", buf.String())
	return rec
}

func TestNewWritesJSON(t *testing.T) {
	rec := capture(t, log.Options{}, func(l *slog.Logger) {
		l.Info("listening", "port", 8080)
	})
	assert.Equal(t, "listening", rec["msg"])
	assert.Equal(t, float64(8080), rec["port"])
	assert.Equal(t, "INFO", rec["level"])
	assert.Contains(t, rec, "time")
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(log.Options{Output: &buf, Level: slog.LevelWarn})
	l.Info("not this one")
	assert.Empty(t, buf.String())
	l.Warn("this one")
	assert.Contains(t, buf.String(), "this one")
}

func TestCloudSeverity(t *testing.T) {
	tests := []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARNING"}, // not slog's "WARN"
		{slog.LevelError, "ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			rec := capture(t, log.Options{Cloud: true, Level: slog.LevelDebug}, func(l *slog.Logger) {
				l.Log(context.Background(), tt.level, "m")
			})
			assert.Equal(t, tt.want, rec["severity"])
			assert.NotContains(t, rec, "level", "the level key is replaced, not duplicated")
		})
	}
}

func TestCloudSeverityOffByDefault(t *testing.T) {
	rec := capture(t, log.Options{}, func(l *slog.Logger) { l.Warn("m") })
	assert.Equal(t, "WARN", rec["level"])
	assert.NotContains(t, rec, "severity")
}

func TestRedactsSecretValues(t *testing.T) {
	tests := []struct {
		key      string
		value    string
		redacted bool
	}{
		{"password", "hunter2", true},
		{"api_token", "abc", true},
		{"apiToken", "abc", true},
		{"JWT_SECRET", "abc", true},
		{"authorization", "Bearer abc", true},
		{"session_id", "abc", true},
		{"cookie", "sid=abc", true},
		{"user_credentials", "abc", true},

		// The control: ordinary attributes must survive, or the rule is just a
		// way of making logs useless.
		{"sort_key", "created_at", false},
		{"cache_key", "listings:1", false},
		{"idempotency_key", "abc", false},
		{"user_id", "42", false},
		{"monkey", "banana", false},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			rec := capture(t, log.Options{}, func(l *slog.Logger) {
				l.Info("m", tt.key, tt.value)
			})
			if tt.redacted {
				assert.Equal(t, log.Placeholder, rec[tt.key])
			} else {
				assert.Equal(t, tt.value, rec[tt.key])
			}
		})
	}
}

func TestRedactsInsideGroupsAndWith(t *testing.T) {
	rec := capture(t, log.Options{}, func(l *slog.Logger) {
		l.With("password", "hunter2").WithGroup("db").Info("m", "token", "abc", "host", "db.internal")
	})
	assert.Equal(t, log.Placeholder, rec["password"])
	group, ok := rec["db"].(map[string]any)
	require.True(t, ok, "expected a db group, got %#v", rec["db"])
	assert.Equal(t, log.Placeholder, group["token"])
	assert.Equal(t, "db.internal", group["host"])
}

func TestRedactsURLCredentialsKeepingTheHost(t *testing.T) {
	rec := capture(t, log.Options{}, func(l *slog.Logger) {
		l.Info("connecting", "database_url", "postgres://app:hunter2@db.internal:5432/main")
	})
	got, _ := rec["database_url"].(string)
	assert.NotContains(t, got, "hunter2")
	assert.Contains(t, got, "db.internal:5432")
	assert.Contains(t, got, "app")
}

func TestExtraSecretKeys(t *testing.T) {
	rec := capture(t, log.Options{SecretKeys: []string{"internal-ref"}}, func(l *slog.Logger) {
		l.Info("m", "internal_ref", "abc", "public_ref", "def")
	})
	assert.Equal(t, log.Placeholder, rec["internal_ref"])
	assert.Equal(t, "def", rec["public_ref"])
}

func TestNoRedact(t *testing.T) {
	rec := capture(t, log.Options{NoRedact: true}, func(l *slog.Logger) {
		l.Info("m", "password", "hunter2")
	})
	assert.Equal(t, "hunter2", rec["password"])
}

func TestRequestIDTravelsOnTheContext(t *testing.T) {
	ctx := log.WithRequestID(context.Background(), "req-123")
	rec := capture(t, log.Options{}, func(l *slog.Logger) {
		l.InfoContext(ctx, "handled")
	})
	assert.Equal(t, "req-123", rec[log.RequestIDKey])
}

func TestRequestIDAbsentWhenNotSet(t *testing.T) {
	rec := capture(t, log.Options{}, func(l *slog.Logger) {
		l.Info("handled")
	})
	assert.NotContains(t, rec, log.RequestIDKey)
}

func TestRequestIDSurvivesWithAndGroup(t *testing.T) {
	ctx := log.WithRequestID(context.Background(), "req-456")
	rec := capture(t, log.Options{}, func(l *slog.Logger) {
		l.With("component", "api").InfoContext(ctx, "handled")
	})
	assert.Equal(t, "req-456", rec[log.RequestIDKey])
	assert.Equal(t, "api", rec["component"])
}

func TestWithRequestIDIgnoresEmpty(t *testing.T) {
	ctx := log.WithRequestID(context.Background(), "")
	assert.Empty(t, log.RequestID(ctx))
	assert.Empty(t, log.RequestID(context.Background()))
}
