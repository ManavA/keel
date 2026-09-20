package auth

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultAccountRateLimitRequests and DefaultAccountRateLimitWindow bound
// failed logins per account email, applied whenever
// Options.AccountRateLimit leaves Requests or Window at zero. Ten failures in
// fifteen minutes is generous to a person mistyping a password and tight
// against a spray: an attacker spreading attempts across IPs to stay under
// the per-IP limiter still shares one per-account bucket, so a few hundred
// guesses an hour against one address stops working after the first ten.
const (
	DefaultAccountRateLimitRequests = 10
	DefaultAccountRateLimitWindow   = 15 * time.Minute
)

// AccountRateLimitOptions configures the per-account login throttle. Unlike
// the per-IP limiter in Options.RateLimit it is keyed by the normalized
// account email, so attempts from many IPs against one account share a
// bucket. Only failed attempts count; a success neither resets the bucket
// nor needs to, since the window expires on its own.
type AccountRateLimitOptions struct {
	// Requests failed attempts allowed per Window. Zero means the default.
	Requests int
	// Window over which failures are counted. Zero means the default.
	Window time.Duration
}

func accountRateLimitWithDefaults(opts AccountRateLimitOptions) AccountRateLimitOptions {
	if opts.Requests <= 0 {
		opts.Requests = DefaultAccountRateLimitRequests
	}
	if opts.Window <= 0 {
		opts.Window = DefaultAccountRateLimitWindow
	}
	return opts
}

// LoginAttempt is one recorded authentication attempt. It carries no password
// material — not the guess, not its hash — so the audit trail cannot become
// a credential store if it leaks.
type LoginAttempt struct {
	// Email is the normalized account address the attempt targeted. It may
	// name an address with no account: recording unknown addresses too is
	// what makes enumeration attempts visible.
	Email string
	// Success reports whether the attempt established a session.
	Success bool
	// IP is the client address as the service saw it (after Options.RealIP),
	// without the port.
	IP string
	// At is when the attempt happened. Stores default a zero value to now on
	// Record.
	At time.Time
}

// AttemptStore persists login attempts for the per-account throttle and the
// audit trail. MemoryAttemptStore is the in-process implementation;
// auth/pg provides the Postgres one.
type AttemptStore interface {
	// Record appends one attempt.
	Record(ctx context.Context, attempt LoginAttempt) error
	// FailuresSince counts failed attempts for email at or after since.
	FailuresSince(ctx context.Context, email string, since time.Time) (int, error)
	// Recent returns up to limit attempts for email, newest first. A
	// non-positive limit returns nothing.
	Recent(ctx context.Context, email string, limit int) ([]LoginAttempt, error)
}

// MemoryAttemptStore is an in-memory AttemptStore, safe for concurrent use.
// It is a complete implementation, not a mock: useful directly in tests and
// in a service too small to need a database yet. Entries older than a day
// are dropped on Record, so an uncleared bucket cannot grow without bound
// under a sustained spray.
type MemoryAttemptStore struct {
	mu       sync.Mutex
	attempts []LoginAttempt
}

// memoryAttemptRetention bounds how long MemoryAttemptStore keeps an entry.
const memoryAttemptRetention = 24 * time.Hour

// NewMemoryAttemptStore returns an empty MemoryAttemptStore.
func NewMemoryAttemptStore() *MemoryAttemptStore {
	return &MemoryAttemptStore{}
}

// Record implements AttemptStore.
func (s *MemoryAttemptStore) Record(_ context.Context, attempt LoginAttempt) error {
	if attempt.At.IsZero() {
		attempt.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-memoryAttemptRetention)
	kept := make([]LoginAttempt, 0, len(s.attempts)+1)
	for _, a := range s.attempts {
		if !a.At.Before(cutoff) {
			kept = append(kept, a)
		}
	}
	kept = append(kept, attempt)
	s.attempts = kept
	return nil
}

// FailuresSince implements AttemptStore.
func (s *MemoryAttemptStore) FailuresSince(_ context.Context, email string, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	for _, a := range s.attempts {
		if a.Email == email && !a.Success && !a.At.Before(since) {
			n++
		}
	}
	return n, nil
}

// Recent implements AttemptStore.
func (s *MemoryAttemptStore) Recent(_ context.Context, email string, limit int) ([]LoginAttempt, error) {
	if limit <= 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LoginAttempt
	for i := len(s.attempts) - 1; i >= 0 && len(out) < limit; i-- {
		if s.attempts[i].Email == email {
			out = append(out, s.attempts[i])
		}
	}
	return out, nil
}

// accountLoginBlocked reports whether email has exhausted its failed-login
// budget inside the configured window. A store failure answers false — open,
// not closed: the per-IP limiter still applies, and refusing every login
// because the audit trail is unreachable turns an observability outage into
// an authentication outage. The failure is logged so that outage is visible.
func (s *Service) accountLoginBlocked(ctx context.Context, email string) bool {
	since := time.Now().Add(-s.accountLimit.Window)
	n, err := s.attempts.FailuresSince(ctx, email, since)
	if err != nil {
		s.logger(ctx).Warn("auth: failed to count recent login attempts, allowing the attempt", "error", err)
		return false
	}
	return n >= s.accountLimit.Requests
}

// recordLoginAttempt appends attempt to the audit trail, filling in the
// client IP from r when the caller left it empty. Best effort for the same
// reason accountLoginBlocked fails open: a login that succeeded must still
// succeed when the audit write does not.
func (s *Service) recordLoginAttempt(ctx context.Context, r *http.Request, attempt LoginAttempt) {
	if attempt.IP == "" {
		attempt.IP = clientIPFromRequest(r)
	}
	if err := s.attempts.Record(ctx, attempt); err != nil {
		s.logger(ctx).Warn("auth: failed to record login attempt", "error", err)
	}
}

// clientIPFromRequest returns the client address of r without the port. The
// Router's RealIP middleware has already rewritten RemoteAddr to the
// deployment's notion of the client by the time a handler runs, so reading
// RemoteAddr here — rather than re-reading a forwarding header — keeps the
// recorded address consistent with the address the rate limiter bucketed by.
func clientIPFromRequest(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
