// Package pg is a Postgres-backed implementation of the auth package's
// storage interfaces: UserStore, SessionStore, VerificationStore,
// PasswordResetStore and AttemptStore. It lives in its own package, importing both auth and
// keel's pg package, rather than inside auth itself, so auth continues to
// build without ever importing database/sql or pgx.
//
// Apply the schema in this package's migrations directory before using these
// stores. MigrationsFS embeds it; pass it to keel's pg/migrate package:
//
//	migrate.Run(ctx, pool, migrate.Options{FS: authpg.MigrationsFS, Dir: "migrations"})
//
// Email columns use Postgres's citext type, making the uniqueness
// constraint and every lookup case-insensitive at the database level. The
// auth package itself already lowercases an email before use, so this is a
// backstop against a row written some other way — a data import, a manual
// fix — rather than the only thing standing between two accounts for the
// same address spelled differently.
package pg
