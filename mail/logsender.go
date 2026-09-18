package mail

import (
	"context"
	"log/slog"
)

// LogSenderOptions configures a [LogSender]. The zero value works: logging
// falls back to [slog.Default].
type LogSenderOptions struct {
	// Logger receives one line per send. Nil falls back to slog.Default();
	// this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// LogSender writes each send through slog instead of delivering it. Use it
// for local development or as the default when no mail provider is
// configured, in place of silently discarding sends.
type LogSender struct {
	opts LogSenderOptions
}

// NewLogSender builds a LogSender from opts.
func NewLogSender(opts LogSenderOptions) *LogSender {
	return &LogSender{opts: opts}
}

// Send logs the message and returns nil.
func (s *LogSender) Send(_ context.Context, to, templateAlias string, templateModel map[string]any) error {
	logger := s.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("mail not sent: no provider configured", "to", to, "template", templateAlias, "model", templateModel)
	return nil
}

// DeliversToProvider reports false: LogSender never contacts a provider.
func (s *LogSender) DeliversToProvider() bool { return false }
