package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/httpx/middleware"
)

// DefaultRateLimitRequests and DefaultRateLimitWindow bound how often a
// single client IP may call the rate-limited routes Router builds, applied
// whenever Options.RateLimit leaves Requests or Window at zero. A
// credentialed endpoint must not be brute-forceable out of the box.
const (
	DefaultRateLimitRequests = 15
	DefaultRateLimitWindow   = time.Minute
)

// minSecretLength is the shortest Options.Secret this package accepts under
// SessionJWT. An HS256 secret shorter than this is weak enough to brute
// force offline against a captured token; refusing it at startup is cheaper
// than discovering it in an incident.
const minSecretLength = 16

// rateLimitWithDefaults fills in Requests and Window when the caller left
// them at zero. httpx/middleware.RateLimit does not default them itself — a
// zero Requests there is a limiter that refuses everything, which is never
// what a caller means by not setting the field — so this package supplies
// the default before passing the options through.
func rateLimitWithDefaults(opts middleware.RateLimitOptions) middleware.RateLimitOptions {
	if opts.Requests <= 0 {
		opts.Requests = DefaultRateLimitRequests
	}
	if opts.Window <= 0 {
		opts.Window = DefaultRateLimitWindow
	}
	return opts
}

// Source identifies where an identity may be established from. A Service can
// run more than one at once — see Options.Sources.
type Source string

const (
	// SourceLocal is email + password against Options.Users, plus email
	// verification and password-reset tokens. It needs nothing outside the
	// process: no external service, and no database if Options.Users is left
	// at its default (an in-memory store).
	SourceLocal Source = "local"
	// SourceFirebase exchanges a Firebase ID token (see FirebaseVerifier) for
	// a session. Needs a Firebase project.
	SourceFirebase Source = "firebase"
	// SourceOIDC exchanges an ID token from a generic OpenID Connect issuer
	// (see OIDCVerifier) for a session — Auth0, Google, Apple, Cognito,
	// Clerk, or any other standard OIDC provider. Needs that issuer's URL and
	// the audience it issues tokens for; no vendor SDK.
	SourceOIDC Source = "oidc"
)

// SessionMode selects how a session token is represented.
type SessionMode string

const (
	// SessionOpaque backs sessions with Options.Sessions (a SessionStore): the
	// token is a random reference, not self-describing, and is revocable by
	// deleting its row. This is the default — a leaked session can be ended
	// without waiting for it to expire.
	SessionOpaque SessionMode = "opaque"
	// SessionJWT backs sessions with a signed, self-contained JWT
	// (Options.Secret). Cheaper to validate (no storage lookup) and simpler
	// to scale across processes with no shared session store, at the cost of
	// not being revocable before it expires: Logout, ResetPassword and
	// DeleteAccount still call through to session revocation, but under
	// SessionJWT that call is a no-op (see jwtSessionBackend.Revoke) and an
	// already-issued token keeps working until it expires. Choose
	// SessionOpaque instead where a token must stop working the moment a
	// caller asks.
	SessionJWT SessionMode = "jwt"
)

// Options configures a Service. The zero value is a complete, working
// configuration: [SourceLocal] only, an in-memory user store, and opaque
// in-memory sessions — enough to run signup/login/refresh with nothing
// outside the process. Every other field has a documented default.
//
// # What each source needs
//
//   - [SourceLocal] needs nothing beyond Options.Users (defaults to
//     [MemoryUserStore]) — no external service.
//   - [SourceFirebase] needs a Firebase project: build a [FirebaseVerifier]
//     with [NewFirebaseVerifier] and set it as Verifier.
//   - [SourceOIDC] needs an issuer URL and audience: build an [OIDCVerifier]
//     with [NewOIDCVerifier] and set it as Verifier. See that constructor's
//     doc for the issuer/audience pattern of common providers (Auth0,
//     Google, Apple, Cognito, Clerk).
//
// Sources compose: a Service running SourceLocal and SourceFirebase (or
// SourceOIDC) together links a federated sign-in to an existing local account
// with the same VERIFIED email, rather than creating a second account for the
// same person — see ExchangeIDToken.
//
// # Secret is not shared with the admin package
//
// Under SessionJWT, Options.Secret MUST be different from whatever secret an
// admin.Service in the same deployment uses. Both packages sign HS256 tokens
// of the same shape; the audience claim each embeds (see session.go) stops a
// token issued by one from validating against the other even when a secret
// is reused by mistake, but two independent secrets is what actually keeps a
// compromise of one credential from reaching the other's sessions.
type Options struct {
	// Sources lists which identity sources this Service accepts. Defaults to
	// []Source{SourceLocal}.
	Sources []Source
	// Verifier authenticates an identity token when Sources includes
	// SourceFirebase or SourceOIDC. Required in that case; refused if it is a
	// nil implementation (see SetIDTokenVerifier).
	Verifier IDTokenVerifier

	// SessionMode selects opaque or JWT sessions. Defaults to SessionOpaque.
	SessionMode SessionMode
	// AllowUnrevocableSessions must be true to build a Service with
	// SessionMode SessionJWT; NewService refuses SessionJWT otherwise, with
	// an error naming this field. The name is meant to be read at the call
	// site: choosing SessionJWT is choosing that Logout, ResetPassword and
	// DeleteAccount cannot force an already-issued token to stop working
	// before it expires (see SessionJWT's own doc comment), and that should
	// never be an accidental default.
	AllowUnrevocableSessions bool
	// Secret signs and verifies session tokens. Required, and must be at
	// least minSecretLength bytes, when SessionMode is SessionJWT; unused
	// otherwise.
	Secret string
	// Sessions backs SessionOpaque. Defaults to an in-memory SessionStore.
	Sessions SessionStore
	// TokenTTL is how long an issued session token is valid. Defaults to
	// DefaultTokenTTL.
	TokenTTL time.Duration

	// Users backs SourceLocal's accounts and every source's identity
	// records. Defaults to an in-memory UserStore.
	Users UserStore
	// Verifications backs the email-verification flow. Defaults to an
	// in-memory VerificationStore.
	Verifications VerificationStore
	// PasswordResets backs the password-reset flow. Defaults to an in-memory
	// PasswordResetStore.
	PasswordResets PasswordResetStore
	// Emailer sends verification and password-reset links. Nil disables both
	// flows (their endpoints answer 503 rather than panicking).
	Emailer Emailer
	// SiteURL is the base URL used to build verification and reset links.
	SiteURL string

	// RealIP configures client-address recovery for the rate limiter below.
	// Its zero value ignores forwarding headers, which is correct only when
	// this Router is reachable directly; behind a proxy, set TrustedProxies
	// (or TrustAnyPeer on a platform where nothing else can reach the
	// process) or every client shares one rate-limit bucket.
	RealIP middleware.RealIPOptions
	// RateLimit bounds requests per client IP to the routes Router builds.
	RateLimit middleware.RateLimitOptions
	// Logger receives this package's diagnostic output. Defaults to
	// slog.Default(); never overridden globally by this package.
	Logger *slog.Logger
}

// Service holds the wiring for the auth HTTP routes across whichever sources
// are configured: sessions, local password checks, and identity-token
// exchange. Build one with NewService and mount its Router.
type Service struct {
	sources        map[Source]bool
	users          UserStore
	verifications  VerificationStore
	passwordResets PasswordResetStore
	emailer        Emailer
	session        sessionBackend
	verifierMu     sync.Mutex
	idVerifier     IDTokenVerifier
	siteURL        string
	realIP         middleware.RealIPOptions
	rateLimit      middleware.RateLimitOptions
	log            *slog.Logger
}

// NewService validates Options and builds a Service. The zero Options value
// succeeds and builds a SourceLocal-only Service over in-memory storage.
func NewService(opts Options) (*Service, error) {
	sources := opts.Sources
	if len(sources) == 0 {
		sources = []Source{SourceLocal}
	}
	sourceSet := make(map[Source]bool, len(sources))
	for _, s := range sources {
		sourceSet[s] = true
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	users := opts.Users
	if users == nil {
		users = NewMemoryUserStore()
	}
	verifications := opts.Verifications
	if verifications == nil {
		verifications = NewMemoryVerificationStore()
	}
	passwordResets := opts.PasswordResets
	if passwordResets == nil {
		passwordResets = NewMemoryPasswordResetStore()
	}

	ttl := opts.TokenTTL
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}

	var backend sessionBackend
	switch opts.SessionMode {
	case SessionJWT:
		if !opts.AllowUnrevocableSessions {
			return nil, errors.New("auth: SessionMode is SessionJWT but Options.AllowUnrevocableSessions is false; " +
				"a JWT session cannot be forced to stop working before it expires, and that must be an explicit choice")
		}
		if opts.Secret == "" {
			return nil, errors.New("auth: Options.Secret is required when SessionMode is SessionJWT")
		}
		if len(opts.Secret) < minSecretLength {
			return nil, fmt.Errorf("auth: Options.Secret must be at least %d bytes", minSecretLength)
		}
		backend = &jwtSessionBackend{issuer: newSessionIssuer(opts.Secret, ttl)}
	case SessionOpaque, "":
		store := opts.Sessions
		if store == nil {
			store = NewMemorySessionStore()
		}
		backend = &opaqueSessionBackend{store: store, ttl: ttl}
	default:
		return nil, errors.New("auth: unknown SessionMode")
	}

	svc := &Service{
		sources:        sourceSet,
		users:          users,
		verifications:  verifications,
		passwordResets: passwordResets,
		emailer:        opts.Emailer,
		session:        backend,
		siteURL:        strings.TrimRight(opts.SiteURL, "/"),
		realIP:         opts.RealIP,
		rateLimit:      rateLimitWithDefaults(opts.RateLimit),
		log:            logger,
	}

	if opts.Verifier != nil {
		if err := svc.SetIDTokenVerifier(opts.Verifier); err != nil {
			return nil, err
		}
	}
	if (sourceSet[SourceFirebase] || sourceSet[SourceOIDC]) && svc.idVerifier == nil {
		return nil, errors.New("auth: Options.Verifier is required when Sources includes SourceFirebase or SourceOIDC")
	}

	return svc, nil
}

func (s *Service) hasSource(src Source) bool {
	return s.sources[src]
}

// SetIDTokenVerifier wires an identity-token verifier (see IDTokenVerifier),
// enabling ExchangeIDToken. It refuses a nil implementation — INCLUDING a
// typed nil, such as a nil *FirebaseVerifier passed as the interface — rather
// than storing it, because a caller that stores a typed nil directly in a
// field ends up with an interface value that is non-nil (it has a concrete
// type) but panics the moment anything calls through it. Refusing at wiring
// time turns that into a clear error instead of a panic on the first request
// that needed the feature.
//
// Safe to call concurrently with itself and with request handling: it holds
// s.mu for the single pointer assignment, and every handler that reads
// s.idVerifier does so through the same mutex.
func (s *Service) SetIDTokenVerifier(v IDTokenVerifier) error {
	if isNilInterfaceValue(v) {
		return errors.New("auth: SetIDTokenVerifier refused a nil implementation")
	}
	s.verifierMu.Lock()
	s.idVerifier = v
	s.verifierMu.Unlock()
	return nil
}

// idTokenVerifier reads the configured verifier under s.mu, the counterpart
// to SetIDTokenVerifier's write.
func (s *Service) idTokenVerifier() IDTokenVerifier {
	s.verifierMu.Lock()
	defer s.verifierMu.Unlock()
	return s.idVerifier
}

// logger prefers the request-scoped logger httpx.NewRouter installs on ctx
// (so a log line from this package carries the same request id and other
// attributes as everything else that request touched), falling back to
// Options.Logger (or slog.Default()) when this Service's routes are not
// mounted under an httpx-built router.
func (s *Service) logger(ctx context.Context) *slog.Logger {
	if l := httpx.Logger(ctx); l != slog.Default() {
		return l
	}
	return s.log
}

// isNilInterfaceValue reports whether v is nil once unwrapped to its
// underlying value — the check a plain `v == nil` cannot perform, because
// that comparison is false for an interface holding a nil pointer, map,
// slice, channel or function.
func isNilInterfaceValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}
