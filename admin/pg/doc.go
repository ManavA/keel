// Package pg is a Postgres-backed implementation of admin.AdminStore. It
// lives in its own package, importing both admin and keel's pg package,
// rather than inside admin itself, so admin continues to build without ever
// importing database/sql or pgx.
//
// Apply the schema in this package's migrations directory before using this
// store. MigrationsFS embeds it; pass it to keel's pg/migrate package:
//
//	migrate.Run(ctx, pool, migrate.Options{FS: adminpg.MigrationsFS, Dir: "migrations"})
//
// The same schema holds the admin_audit table behind [AuditStore], the
// append-only trail the admin package's Audit middleware writes.
//
// The email column uses Postgres's citext type, making the uniqueness
// constraint and every lookup case-insensitive at the database level.
package pg
