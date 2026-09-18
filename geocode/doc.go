// Package geocode converts addresses to coordinates behind one interface.
//
// A Provider that cannot place an address returns a nil *Coordinates, not a
// zero-value Coordinates. (0, 0) is a valid point on Earth (in the Gulf of
// Guinea), so using it to mean "unknown" makes an unresolved address
// indistinguishable from a resolved one at that exact point. A caller that
// receives nil has no location for this address and should skip whatever
// downstream operation needed one (a map pin, a distance calculation, a
// commute-time lookup) rather than pass (0, 0) into it.
//
// Provider implementations may return an approximate result instead of no
// result: an address that cannot be resolved exactly may still resolve to a
// postal-code or city centroid. Coordinates.Precision records which of
// these produced the result, so a caller can tell an approximate result
// from an exact one.
//
// Cached and RateLimited each wrap a Provider without depending on its
// implementation. Cached takes a caller-supplied Store (a Postgres table, a
// KV store, an in-memory map — this package provides only the in-memory
// one, MemoryStore). NoopProvider implements Provider by always returning
// nil, for a caller that has not configured a real geocoding service; code
// built on Cached or RateLimited works the same whether the underlying
// Provider is Mapbox or NoopProvider.
package geocode
