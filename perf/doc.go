// Package perf provides small, composable HTTP performance middleware:
// ResponseCache (an in-process cache with explicit tag-based invalidation),
// ETag (conditional GET via If-None-Match), Gzip (response compression), and
// SingleFlight (collapsing concurrent identical work). Each piece works on
// its own; use only the ones a given handler needs.
//
// ResponseCache's Store is in-memory and size-bounded, not a Redis client.
// A response cache is per-instance state, not data that needs to survive a
// restart or be visible to other instances, so it needs no external
// service by default. Store is an interface so a caller who needs a cache
// shared across replicas can implement it against Redis or another service;
// this package does not ship that implementation, so that a consumer who
// only wants ETag or Gzip does not have to depend on a Redis client.
//
// # Prepared statements with pgx
//
// pgx/v5's pool prepares and caches statements per connection through the
// extended query protocol. A separate prepared-statement cache on top of
// pgx is redundant. One detail matters for callers of this package: pgx's
// statement cache is keyed on the exact SQL text, so building a query by
// concatenating a WHERE clause into the query string defeats it, the same
// way a URL that varies per request defeats ResponseCache's key function.
// Use $1, $2 placeholders and vary only the arguments, not the SQL text.
// This package does not add a query-result cache in front of pgx for this
// reason: a query-result cache belongs at the HTTP handler boundary, as a
// ResponseCache keyed and invalidated by the caller, not inside the SQL
// layer.
package perf
