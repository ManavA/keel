package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
)

// LogSenderOptions configures a [LogSender]. The zero value works: logging
// falls back to [slog.Default], and no recipient address or template model
// is logged.
type LogSenderOptions struct {
	// Logger receives each send's log lines. Nil falls back to
	// slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
	// LogBodies, when true, additionally logs the raw recipient address
	// and the full template model, at Debug. This is for local
	// development only: a template model commonly carries values such as
	// password-reset tokens and magic links, and LogSender is the default
	// Sender when nothing else is configured, so it is what runs in
	// production on a missed configuration step. Leave this false outside
	// development.
	LogBodies bool
}

// LogSender writes each send through slog instead of delivering it. Use it
// for local development or as the default when no mail provider is
// configured, in place of silently discarding sends.
//
// Every call logs the template alias and a hash of the recipient address
// at Info. The recipient address and the full template model are logged
// only when LogBodies is true, at Debug.
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

	logger.Info("mail not sent: no provider configured",
		"template", templateAlias, "to_hash", hashRecipientForLog(to))

	if s.opts.LogBodies {
		logger.Debug("mail not sent: recipient and template model",
			"to", to, "template", templateAlias, "model", templateModel)
	}
	return nil
}

// DeliversToProvider reports false: LogSender never contacts a provider.
func (s *LogSender) DeliversToProvider() bool { return false }

// hashRecipientForLog returns a short, non-reversible identifier for an
// address, so repeated sends to the same recipient can be correlated in a
// log without the address itself appearing in it.
func hashRecipientForLog(to string) string {
	normalized := strings.ToLower(strings.TrimSpace(to))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:12]
}
