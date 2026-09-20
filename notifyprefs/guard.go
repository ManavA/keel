package notifyprefs

import (
	"context"
	"errors"
	"fmt"

	"github.com/ManavA/keel/mail"
)

// ErrSuppressed reports that a send was skipped because the recipient opted
// out of its category on its channel. It is returned by [GuardedSender.Send]
// instead of delivering the message. A caller that counts deliveries — via
// [mail.DeliversToProvider], say — must treat it as not delivered: it is a
// non-nil error, so only a caller that ignores Send errors could miscount.
var ErrSuppressed = errors.New("notifyprefs: send suppressed by recipient preferences")

// ResolveFunc maps one mail send to the user and category the preference
// check runs against. to is the recipient address and templateAlias the
// template the caller is about to send; the function returns whose
// preferences govern and what kind of message this is. Returning an empty
// user id means the send cannot be attributed — a welcome message to an
// address with no account yet — and the send proceeds unchecked.
type ResolveFunc func(ctx context.Context, to, templateAlias string) (userID string, category Category)

// GuardedSender wraps a [mail.Sender] and enforces notification preferences
// on the mail send path. Before each send it resolves the recipient to a
// user and category and asks the store whether email for that pair sends; a
// suppressed send returns [ErrSuppressed] without touching the inner sender.
//
// Security and transactional mail always send (see the package doc), and a
// send that cannot be attributed to a user proceeds unchecked rather than
// silently dropping a message the store knows nothing about.
type GuardedSender struct {
	inner   mail.Sender
	store   Store
	resolve ResolveFunc
}

// NewGuardedSender builds a GuardedSender over inner, consulting store with
// resolve's attribution for every send.
func NewGuardedSender(inner mail.Sender, store Store, resolve ResolveFunc) *GuardedSender {
	return &GuardedSender{inner: inner, store: store, resolve: resolve}
}

// Send implements [mail.Sender].
func (s *GuardedSender) Send(ctx context.Context, to, templateAlias string, templateModel map[string]any) error {
	userID, category := s.resolve(ctx, to, templateAlias)
	if userID != "" {
		allowed, err := s.store.Allowed(ctx, userID, category, ChannelEmail)
		if err != nil {
			return fmt.Errorf("notifyprefs: check preferences for send to %s: %w", to, err)
		}
		if !allowed {
			return fmt.Errorf("%w: user %s opted out of %s email", ErrSuppressed, userID, category)
		}
	}
	return s.inner.Send(ctx, to, templateAlias, templateModel)
}

// DeliversToProvider mirrors [mail.DeliversToProvider] for the inner sender:
// a nil error from Send means the provider accepted the message exactly when
// it would have through the inner sender alone. A suppressed send returns
// [ErrSuppressed], a non-nil error, so it is never counted as delivered.
func (s *GuardedSender) DeliversToProvider() bool {
	return mail.DeliversToProvider(s.inner)
}
