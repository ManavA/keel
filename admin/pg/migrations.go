package pg

import "embed"

// MigrationsFS embeds this package's own migrations. See the package doc for
// how to run them.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS
