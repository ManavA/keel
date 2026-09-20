// Package pg holds the Postgres schema [outbox.Relay] needs.
//
// Apply the schema in this package's migrations directory before building a
// relay over the pool. MigrationsFS embeds it; pass it to keel's pg/migrate
// package:
//
//	migrate.Run(ctx, pool, migrate.Options{FS: outboxpg.MigrationsFS, Dir: "migrations"})
package pg
