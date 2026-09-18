// Package httpx is the HTTP layer a service would otherwise write from scratch
// every time: a server that shuts down without dropping requests, a router with
// a middleware stack already in the right order, JSON responses that never echo
// what the caller sent, and health endpoints that check something.
//
// Two decisions are worth knowing before you read the rest.
//
// Error responses are generic. A handler says "not found"; it does not say
// "listing 7f3a is not yours". The difference matters because an error that
// distinguishes "does not exist" from "exists but is not yours" is an
// enumeration oracle, and because an error that quotes the input back is a
// reflection bug waiting for someone to put markup in an identifier. Respond
// takes a status and produces the body; the detail goes in the log, with the
// request id, where it is useful and not public.
//
// Middleware is composed by the caller, not imposed. NewRouter assembles the
// usual stack because getting the order wrong is easy — a recoverer above the
// request log records a panic nobody can attribute, a rate limiter above the
// real-IP middleware buckets every client behind the proxy together — but every
// piece is exported from httpx/middleware and a service is free to build its
// own chain instead.
package httpx
