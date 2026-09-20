// Package admin provides authentication and a router skeleton for an
// operator-facing HTTP API, separate from the auth package's end-user
// sessions. It does not import auth: the two packages sit at the same layer
// (see the repository's ARCHITECTURE.md) and each is usable without the
// other.
//
// # Sessions
//
// [Service] issues short-lived JWT sessions (12 hours by default) for admin
// accounts stored behind [AdminStore]. [MemoryAdminStore] is a complete
// in-memory implementation for tests; admin/pg provides a Postgres-backed
// one, along with the migrations it needs.
//
// Options.Secret must be at least 16 bytes, and must be a DIFFERENT secret
// from whatever auth.Options.Secret this deployment's end-user sessions use.
// Every token this package issues carries an "aud" claim ("keel:admin") that
// the sibling auth package's tokens do not share ("keel:auth"), so a token
// cannot validate against the wrong package even if the two secrets are
// reused by mistake — but two independent secrets is what actually keeps a
// compromise of one credential from reaching the other's sessions.
//
// # Router
//
// [Service.Router] returns a chi.Router mounting POST /login (rate-limited
// via keel's httpx/middleware, with Options.RealIP configuring which
// forwarding headers it trusts) and, behind [Service.RequireAdmin], POST
// /refresh. It applies its own CORS policy (Options.CORSOrigin), independent
// of any CORS policy the rest of an API uses, because an admin console is
// typically served from a different origin than the public API. A caller
// adds its own operator-only routes to the returned router, protected by the
// same [Service.RequireAdmin] middleware.
//
// # Audit trail
//
// [Service.Audit] is middleware that appends one [AuditEntry] per request —
// actor, action, target, timestamp, and outcome — to the store in
// Options.Audit (in-memory by default, Postgres-backed via admin/pg). Mount
// it inside [Service.RequireAdmin] on every operator-only route, and review
// the trail through GET /audit, served by [Service.AuditList].
//
// # Protecting fields that must not reach a public response
//
// [CheckNoForbiddenFields] inspects an encoded JSON response for a set of
// key names and reports an error if any are present. It is meant to run
// inside a test for every public-facing response type in a service that
// also has an admin-only view of the same data (for example, an internal
// note or a cost field visible only through this package's routes) — the
// test fails if a public handler's response ever starts including that
// field, whether because of a shared struct, a forgotten `json:"-"` tag, or
// an embedding change.
//
// # Seeding an initial account
//
// [Seed] creates the first admin account against an AdminStore, hashing its
// password. admin/cmd/seed is a runnable command built on it and admin/pg:
// it reads the password from ADMIN_SEED_PASSWORD or stdin, never a
// command-line argument, and refuses to run against a non-empty admin_users
// table unless passed -force.
package admin
