// Package httpx is the HTTP layer a service would otherwise write from scratch:
// a server that shuts down without dropping requests, a router with a
// middleware stack already in the right order, JSON responses, and health
// endpoints that check something.
//
// Two decisions are worth knowing before reading the rest.
//
// Error responses are generic. A handler answers "not found"; it does not say
// which record, and it does not return the underlying error. Distinguishing
// "does not exist" from "exists but is not yours" lets a caller enumerate
// records, and quoting the input back invites a reflection bug. The detail goes
// to the log with the request id.
//
// Middleware is composed by the caller. NewRouter assembles the usual stack
// because the order matters — a rate limiter above the real-IP middleware
// buckets every client behind the proxy together — but every piece is exported
// from httpx/middleware and a service can build its own chain.
package httpx
