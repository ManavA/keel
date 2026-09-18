package auth

import (
	"net/http"

	"github.com/ManavA/keel/httpx/middleware"
)

// Router builds the HTTP routes for whichever sources are configured:
//
//	POST /exchange                (SourceFirebase or SourceOIDC; rate-limited; 503 until a verifier is wired)
//
// and, only when Options.Sources includes SourceLocal:
//
//	POST /signup                  (rate-limited)
//	POST /login                   (rate-limited)
//	POST /verify-email            (unauthenticated; the token is the credential)
//	POST /forgot-password         (rate-limited; always answers 200)
//	POST /reset-password          (unauthenticated; the token is the credential)
//	POST /resend-verification     (requires a bearer session token)
//
// and always:
//
//	POST /refresh                 (requires a bearer session token)
//	POST /logout                  (requires a bearer session token)
//	POST /delete-account          (requires a bearer session token)
//
// Every rate-limited route runs Options.RealIP ahead of the limiter, so the
// bucket key is the client address this deployment says to trust rather than
// always r.RemoteAddr (see Options.RealIP's doc comment for the proxy case).
//
// Mount the result under whatever prefix a caller likes
// (http.StripPrefix("/auth", s.Router())), so this package has no opinion
// about where in a larger API it lives.
func (s *Service) Router() http.Handler {
	realIP := middleware.RealIP(s.realIP)
	limit := func(next http.Handler) http.Handler {
		return realIP(middleware.RateLimit(s.rateLimit)(next))
	}

	mux := http.NewServeMux()

	if s.hasSource(SourceFirebase) || s.hasSource(SourceOIDC) {
		mux.Handle("POST /exchange", limit(http.HandlerFunc(s.ExchangeIDToken)))
	}

	if s.hasSource(SourceLocal) {
		mux.Handle("POST /signup", limit(http.HandlerFunc(s.Signup)))
		mux.Handle("POST /login", limit(http.HandlerFunc(s.Login)))
		mux.Handle("POST /verify-email", http.HandlerFunc(s.VerifyEmail))
		mux.Handle("POST /forgot-password", limit(http.HandlerFunc(s.ForgotPassword)))
		mux.Handle("POST /reset-password", http.HandlerFunc(s.ResetPassword))
		mux.Handle("POST /resend-verification", s.RequireAuth(http.HandlerFunc(s.ResendVerification)))
	}

	// Refresh, logout and account deletion apply to a session regardless of
	// which source established it.
	mux.Handle("POST /refresh", s.RequireAuth(http.HandlerFunc(s.Refresh)))
	mux.Handle("POST /logout", s.RequireAuth(http.HandlerFunc(s.Logout)))
	mux.Handle("POST /delete-account", s.RequireAuth(http.HandlerFunc(s.DeleteAccount)))

	return mux
}
