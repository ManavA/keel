package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/auth"
	authpg "github.com/ManavA/keel/auth/pg"
	"github.com/ManavA/keel/httpx/middleware"
	"github.com/ManavA/keel/mail"
)

// authMailer adapts a mail.Sender to the auth package's Emailer. The auth
// package thinks in links (to, kind, url); the mail package thinks in
// templates (to, alias, model). The kind travels as the template alias and
// the link as the model's URL, so a deployment that swaps the log sender
// for a provider-backed one only has to create templates named
// "verify-email" and "password-reset" taking a URL.
type authMailer struct {
	sender mail.Sender
}

func (m *authMailer) Send(ctx context.Context, to, kind, url string) error {
	return m.sender.Send(ctx, to, kind, map[string]any{
		"email": to,
		"url":   url,
	})
}

// buildAuthService wires the auth package the way this service runs it:
// email and password accounts with verification and reset, over the
// Postgres-backed stores sharing the service's own pool, so sessions,
// accounts and their tokens live in the same database as everything else.
// Firebase and OIDC need nothing more than their environment variables;
// the local source needs nothing at all.
func buildAuthService(ctx context.Context, cfg Config, logger *slog.Logger, pool *pgxpool.Pool, sender mail.Sender) (*auth.Service, error) {
	sources := make([]auth.Source, 0, len(cfg.AuthSources))
	for _, source := range cfg.AuthSources {
		sources = append(sources, auth.Source(strings.TrimSpace(source)))
	}

	opts := auth.Options{
		Sources:        sources,
		Users:          authpg.NewUserStore(pool),
		Verifications:  authpg.NewVerificationStore(pool),
		PasswordResets: authpg.NewPasswordResetStore(pool),
		Sessions:       authpg.NewSessionStore(pool),
		Emailer:        &authMailer{sender: sender},
		SiteURL:        strings.TrimSpace(cfg.SiteURL),
		TokenTTL:       cfg.AuthTokenTTL,
		RealIP:         middleware.RealIPOptions{TrustedProxies: cfg.TrustedProxies},
		Logger:         logger,
	}
	if cfg.AuthRateLimitRequests > 0 || cfg.AuthRateLimitWindow > 0 {
		opts.RateLimit = middleware.RateLimitOptions{
			Requests: cfg.AuthRateLimitRequests,
			Window:   cfg.AuthRateLimitWindow,
		}
	}

	// The federated sources each need their verifier, built here so a bad
	// issuer or project id stops startup rather than the first exchange.
	// Config.Validate already refused the combinations that cannot work
	// (missing settings, both federated sources at once); these checks keep
	// the constructor honest when it is called with a config that skipped
	// validation.
	for _, source := range sources {
		switch source {
		case auth.SourceFirebase:
			if strings.TrimSpace(cfg.FirebaseProjectID) == "" {
				return nil, fmt.Errorf("firebase source selected but FIREBASE_PROJECT_ID is empty")
			}
			verifier, err := auth.NewFirebaseVerifier(ctx, strings.TrimSpace(cfg.FirebaseProjectID))
			if err != nil {
				return nil, fmt.Errorf("firebase verifier: %w", err)
			}
			opts.Verifier = verifier
		case auth.SourceOIDC:
			if strings.TrimSpace(cfg.OIDCIssuerURL) == "" || strings.TrimSpace(cfg.OIDCAudience) == "" {
				return nil, fmt.Errorf("oidc source selected but OIDC_ISSUER_URL or OIDC_AUDIENCE is empty")
			}
			verifier, err := auth.NewOIDCVerifier(ctx, auth.OIDCOptions{
				IssuerURL: strings.TrimSpace(cfg.OIDCIssuerURL),
				Audience:  strings.TrimSpace(cfg.OIDCAudience),
				JWKSURL:   strings.TrimSpace(cfg.OIDCJWKSURL),
			})
			if err != nil {
				return nil, fmt.Errorf("oidc verifier: %w", err)
			}
			opts.Verifier = verifier
		}
	}

	svc, err := auth.NewService(opts)
	if err != nil {
		return nil, fmt.Errorf("auth service: %w", err)
	}
	return svc, nil
}
