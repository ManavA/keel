package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/auth"
	authpg "github.com/ManavA/keel/auth/pg"
)

// buildAuthService wires the auth package the way this service runs it: the
// local source only — email and password accounts over the Postgres-backed
// stores sharing the service's own pool, so sessions, accounts and their
// tokens live in the same database as everything else.
//
// There is no mail sender, so verification and reset links are never built:
// signup still returns a session, and reports verification as not sent.
func buildAuthService(logger *slog.Logger, pool *pgxpool.Pool, siteURL string) (*auth.Service, error) {
	svc, err := auth.NewService(auth.Options{
		Sources:        []auth.Source{auth.SourceLocal},
		Users:          authpg.NewUserStore(pool),
		Verifications:  authpg.NewVerificationStore(pool),
		PasswordResets: authpg.NewPasswordResetStore(pool),
		Sessions:       authpg.NewSessionStore(pool),
		SiteURL:        siteURL,
		Logger:         logger,
	})
	if err != nil {
		return nil, err
	}
	return svc, nil
}
