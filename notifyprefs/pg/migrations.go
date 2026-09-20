package pg

import "embed"

// MigrationsFS embeds this package's own migrations, so a binary using the
// store carries its schema without a separate deploy step to copy SQL files
// alongside it. Apply them with keel's pg/migrate package:
//
//	migrate.Run(ctx, pool, migrate.Options{FS: notifypg.MigrationsFS, Dir: "migrations"})
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS
