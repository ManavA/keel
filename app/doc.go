// Package app wires keel packages into a running service.
//
// Every service repeats the same startup sequence — logger, pool, migrations,
// router, health checks, background jobs, graceful shutdown — and the order
// matters: migrations must run before the server accepts traffic, and the
// readiness check must report what the router actually depends on. This
// package owns that sequence. The caller supplies the behaviour: its routes,
// its migration files, its health checks, its jobs, and optionally an auth
// and an admin handler built with those packages.
//
// A service needing only Postgres comes up with no further choices:
//
//	app := app.New(app.Options{DB: pg.Options{URL: cfg.DatabaseURL}})
//	if err := app.Run(ctx); err != nil { ... }
//
// app sits at the top import level: it may import any keel package, and no
// keel package may import it. The existing packages stay usable alone; app
// only connects them through values the caller hands in.
package app
