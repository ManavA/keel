package main

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/admin"
	adminpg "github.com/ManavA/keel/admin/pg"
)

// buildAdminService wires the admin package the way this service runs it:
// operator accounts over the Postgres-backed store sharing the service's
// own pool, signing its sessions with a secret that must differ from any
// secret the end-user auth service uses.
func buildAdminService(logger *slog.Logger, pool *pgxpool.Pool, secret string) (*admin.Service, error) {
	svc, err := admin.NewService(admin.Options{
		Users:  adminpg.NewAdminStore(pool),
		Secret: secret,
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	return svc, nil
}
