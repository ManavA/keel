package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/httpx/middleware"
)

// DefaultLoginRateLimitRequests and DefaultLoginRateLimitWindow bound how
// often a single client IP may call POST /login, applied whenever
// Options.LoginRateLimit leaves Requests or Window at zero. Tighter than a
// typical end-user login (see auth's DefaultRateLimitRequests): a guessed
// admin password exposes everything an admin console can see.
const (
	DefaultLoginRateLimitRequests = 5
	DefaultLoginRateLimitWindow   = time.Minute
)

// minSecretLength is the shortest Options.Secret this package accepts. See
// auth's identical constant for why.
const minSecretLength = 16

func loginRateLimitWithDefaults(opts middleware.RateLimitOptions) middleware.RateLimitOptions {
	if opts.Requests <= 0 {
		opts.Requests = DefaultLoginRateLimitRequests
	}
	if opts.Window <= 0 {
		opts.Window = DefaultLoginRateLimitWindow
	}
	return opts
}

// Options configures a Service. Users and Secret are required; every other
// field has a documented default.
//
// Secret MUST be different from whatever secret an auth.Service in the same
// deployment uses — see auth.Options's doc comment on why a shared secret is
// a configuration mistake this package's audience check defends against, but
// two independent secrets is what actually keeps a compromise of one
// credential from reaching the other's sessions. The app package's
// CheckSecretSeparation enforces this at startup for deployments that mount
// both services.
type Options struct {
	// Users is required.
	Users AdminStore
	// Secret signs and verifies session tokens. Required, and must be at
	// least minSecretLength bytes.
	Secret string
	// TokenTTL is how long an issued session token is valid. Defaults to
	// DefaultTokenTTL. It is the token's absolute lifetime — exp is always
	// iat plus this duration, and no activity extends it. There is no idle
	// timeout: admin sessions are stateless JWTs with no activity record to
	// measure idleness against. Keep this short (the 12-hour default is
	// already generous for a privileged console) and require re-login rather
	// than relying on Refresh to carry a session indefinitely; every refresh
	// issues a full new ttl.
	TokenTTL time.Duration
	// CORSOrigin is the single origin Router's CORS policy allows, typically
	// the admin console's own origin. Required to allow any cross-origin
	// request; if empty, Router applies no CORS headers at all, which is
	// correct only when the admin console is served from the same origin as
	// this API.
	CORSOrigin string
	// RealIP configures client-address recovery for the login rate limiter.
	// Its zero value ignores forwarding headers, which is correct only when
	// this Router is reachable directly; behind a proxy, set TrustedProxies
	// (or TrustAnyPeer on a platform where nothing else can reach the
	// process) or every client shares one rate-limit bucket.
	RealIP middleware.RealIPOptions
	// LoginRateLimit bounds requests per client IP to POST /login.
	LoginRateLimit middleware.RateLimitOptions
	// Audit is the append-only store the Audit middleware writes to and the
	// AuditList handler reads from. When nil, NewService uses an in-memory
	// store: fine for tests, but a deployment that needs the trail to
	// survive a restart passes the Postgres-backed store from admin/pg.
	Audit AuditStore
	// Logger receives this package's diagnostic output. Defaults to
	// slog.Default(); never overridden globally by this package.
	Logger *slog.Logger
}

// Service holds the wiring for the admin HTTP routes: sessions and password
// checks. Build one with NewService and mount its Router.
type Service struct {
	users          AdminStore
	audit          AuditStore
	session        *sessionIssuer
	corsOrigin     string
	realIP         middleware.RealIPOptions
	loginRateLimit middleware.RateLimitOptions
	log            *slog.Logger
}

// NewService validates Options and builds a Service.
func NewService(opts Options) (*Service, error) {
	if opts.Users == nil {
		return nil, errors.New("admin: Options.Users is required")
	}
	if opts.Secret == "" {
		return nil, errors.New("admin: Options.Secret is required")
	}
	if len(opts.Secret) < minSecretLength {
		return nil, fmt.Errorf("admin: Options.Secret must be at least %d bytes", minSecretLength)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	audit := opts.Audit
	if audit == nil {
		audit = NewMemoryAuditStore()
	}
	return &Service{
		users:          opts.Users,
		audit:          audit,
		session:        newSessionIssuer(opts.Secret, opts.TokenTTL),
		corsOrigin:     opts.CORSOrigin,
		realIP:         opts.RealIP,
		loginRateLimit: loginRateLimitWithDefaults(opts.LoginRateLimit),
		log:            logger,
	}, nil
}

// logger prefers the request-scoped logger httpx.NewRouter installs on ctx,
// falling back to Options.Logger (or slog.Default()) when this Service's
// routes are not mounted under an httpx-built router. See auth.Service's
// identical method for the full rationale.
func (s *Service) logger(ctx context.Context) *slog.Logger {
	if l := httpx.Logger(ctx); l != slog.Default() {
		return l
	}
	return s.log
}
