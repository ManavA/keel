package mail

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/keighl/postmark"
)

// PostmarkSenderOptions configures a [PostmarkSender]. The zero value works
// except for the two required fields; FromName may be left empty.
type PostmarkSenderOptions struct {
	// ServerToken authenticates against one Postmark server. Required.
	ServerToken string
	// FromEmail is the address every message is sent from. Required.
	FromEmail string
	// FromName is the display name placed ahead of FromEmail, e.g. "Acme"
	// renders as `Acme <hello@acme.com>`. Empty sends the bare address.
	FromName string
	// Logger receives one line per send attempt. Nil falls back to
	// slog.Default(); this package never calls slog.SetDefault.
	Logger *slog.Logger
}

// PostmarkSender sends templated email through Postmark.
type PostmarkSender struct {
	client *postmark.Client
	opts   PostmarkSenderOptions
}

// NewPostmarkSender builds a PostmarkSender from opts.
func NewPostmarkSender(opts PostmarkSenderOptions) *PostmarkSender {
	return &PostmarkSender{
		client: postmark.NewClient(opts.ServerToken, ""),
		opts:   opts,
	}
}

// Send delivers a templated email via Postmark.
//
// The `res.ErrorCode != 0` check is required. The client's request path
// returns a nil Go error even when Postmark's response body carries a
// nonzero ErrorCode, for example 1101 for an unknown template alias. See
// the package doc.
func (s *PostmarkSender) Send(_ context.Context, to, templateAlias string, templateModel map[string]any) error {
	logger := s.opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	email := postmark.TemplatedEmail{
		TemplateAlias: templateAlias,
		TemplateModel: templateModel,
		From:          FormatAddress(s.opts.FromName, s.opts.FromEmail),
		To:            to,
	}

	res, err := s.client.SendTemplatedEmail(email)
	if err == nil && res.ErrorCode != 0 {
		err = classifyPostmarkError(res.ErrorCode, res.Message)
	}
	if err != nil {
		logger.Error("postmark send failed", "to", to, "template", templateAlias, "error", err)
		return fmt.Errorf("send email to %s: %w", to, err)
	}

	logger.Info("email sent", "to", to, "template", templateAlias, "message_id", res.MessageID)
	return nil
}

// DeliversToProvider reports true: a nil error from Send means Postmark
// accepted the message and issued the MessageID logged above.
func (s *PostmarkSender) DeliversToProvider() bool { return true }

// postmarkInactiveRecipient is Postmark's error code for an address that
// hard-bounced, drew a spam complaint, or was suppressed by hand.
const postmarkInactiveRecipient = 406

// classifyPostmarkError turns a Postmark error code into an error that says
// whether retrying could ever help.
//
// Only 406 is treated as permanent. Erring the other way would silently
// treat a real recipient as unreachable over a transient fault or a mistake
// in our own configuration, which is worse than a few wasted retries.
func classifyPostmarkError(code int64, message string) error {
	base := fmt.Errorf("postmark error %d: %s", code, message)
	if code == postmarkInactiveRecipient {
		return fmt.Errorf("%w: %w", ErrRecipientUndeliverable, base)
	}
	return base
}
