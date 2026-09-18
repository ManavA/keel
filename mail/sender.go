package mail

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Sender sends one templated email.
type Sender interface {
	Send(ctx context.Context, to, templateAlias string, templateModel map[string]any) error
}

// ProviderBackedSender reports whether a Sender's nil error means the
// provider accepted the message, as opposed to meaning only that nothing
// failed locally. A no-op fallback Sender and a real accepted send both
// return nil from Send; this interface lets a caller distinguish them.
//
// A Sender that does not implement this interface is treated as not
// provider-backed by [DeliversToProvider]. This default understates
// delivery counts rather than overstating them.
type ProviderBackedSender interface {
	DeliversToProvider() bool
}

// DeliversToProvider reports whether a send through s may be counted as a
// message a real provider accepted.
func DeliversToProvider(s Sender) bool {
	p, ok := s.(ProviderBackedSender)
	return ok && p.DeliversToProvider()
}

// ErrRecipientUndeliverable marks an address the provider has reported it
// will never accept again: hard-bounced, suppressed, or a spam complaint.
//
// This is distinguished from other send failures because the correct
// response differs: most failures (a bad token, a missing template) are
// worth retrying, while a permanently undeliverable recipient is not. A
// retry loop that does not distinguish the two either retries a dead
// address indefinitely or stops retrying a recipient that had a transient
// failure.
var ErrRecipientUndeliverable = errors.New("mail: recipient is permanently undeliverable")

// FormatAddress builds an RFC 5322 mailbox string, e.g. `Jane <jane@example.com>`.
//
// name is quoted when it contains a character RFC 5322 classifies as a
// special. An unquoted comma is the notable case: `Smith, Jane <a@b>`
// parses as two addresses, so an unquoted comma in a display name would
// misroute the message rather than just look wrong. A name containing a
// quote or backslash is escaped rather than dropped.
func FormatAddress(name, email string) string {
	name = strings.TrimSpace(name)
	email = strings.TrimSpace(email)
	if name == "" || email == "" {
		return email
	}
	if strings.ContainsAny(name, `"\(),:;<>@[]`) {
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name)
		return fmt.Sprintf(`"%s" <%s>`, escaped, email)
	}
	return fmt.Sprintf("%s <%s>", name, email)
}
