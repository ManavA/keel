package mail

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/keighl/postmark"

	"github.com/ManavA/keel/retry"
)

// postmarkTransportRetry bounds how hard Send retries a request the
// client itself failed to complete — a dropped connection, a timeout —
// before giving up. It does not apply to a response Postmark returned
// successfully with a nonzero ErrorCode; those are classified by
// classifyPostmarkError, and retrying an error like an unknown template
// alias would just fail the same way every time.
var postmarkTransportRetry = retry.Options{
	MaxAttempts: 3,
	BaseDelay:   200 * time.Millisecond,
	MaxDelay:    2 * time.Second,
}

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
	client := postmark.NewClient(opts.ServerToken, "")
	client.HTTPClient = &http.Client{
		Timeout:   client.HTTPClient.Timeout,
		Transport: &postmark5xxTransport{base: client.HTTPClient.Transport},
	}
	return &PostmarkSender{
		client: client,
		opts:   opts,
	}
}

// postmark5xxTransport turns an HTTP 5xx response into a transport error, so
// Send's retry covers it. The client library never looks at the status code:
// it parses any body, so a 5xx with a well-formed body would otherwise be
// classified as an API error and sent once.
type postmark5xxTransport struct {
	base http.RoundTripper
}

func (t *postmark5xxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	resp, err := base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode < 500 {
		return resp, nil
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if readErr != nil {
		body = nil
	}
	return nil, fmt.Errorf("postmark: transient server error (status %d): %s", resp.StatusCode, body)
}

// Send delivers a templated email via Postmark.
//
// The `res.ErrorCode != 0` check is required. The client's request path
// returns a nil Go error even when Postmark's response body carries a
// nonzero ErrorCode, for example 1101 for an unknown template alias. See
// the package doc.
func (s *PostmarkSender) Send(ctx context.Context, to, templateAlias string, templateModel map[string]any) error {
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

	var res postmark.EmailResponse
	err := retry.Do(ctx, func() error {
		var sendErr error
		res, sendErr = s.client.SendTemplatedEmail(email)
		return sendErr
	}, postmarkTransportRetry)
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
// treat a real recipient as unreachable over a transient fault or a
// configuration mistake, which is worse than a few wasted retries.
func classifyPostmarkError(code int64, message string) error {
	base := fmt.Errorf("postmark error %d: %s", code, message)
	if code == postmarkInactiveRecipient {
		return fmt.Errorf("%w: %w", ErrRecipientUndeliverable, base)
	}
	return base
}
