package auth

import "context"

// Emailer sends a single-use link to a user. kind names which flow the link
// belongs to ("verify-email" or "password-reset"), so one implementation can
// route both through whatever provider or template set it wraps, rather than
// this package depending on two near-identical interfaces.
type Emailer interface {
	Send(ctx context.Context, to, kind, url string) error
}
