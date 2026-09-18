package geocode

import (
	"context"
	"strings"
)

// Store persists a Provider's answer for a normalised address key, so a
// repeated lookup of the same address costs one network call instead of
// one per lookup. This package provides MemoryStore. A caller that wants a
// durable or shared cache (Postgres, Redis, or another service) implements
// this two-method interface directly; this package does not import a
// database driver for it.
type Store interface {
	Get(ctx context.Context, key string) (Coordinates, bool, error)
	Set(ctx context.Context, key string, coords Coordinates) error
}

// NormalizeKey collapses an address into a cache key that treats
// "123 Main St, Springfield, CA 94000" and "123   MAIN st, springfield,ca
// 94000" as the same lookup.
//
// It does not expand abbreviations ("St" vs "Street") or parse the address
// into components. Either requires an address-standardization library to do
// correctly; doing it partially here risks merging two different addresses
// under one cache key, which returns a wrong answer instead of a cache
// miss.
func NormalizeKey(address, city, state, postalCode string) string {
	fields := []string{address, city, state, postalCode}
	for i, f := range fields {
		fields[i] = strings.Join(strings.Fields(strings.ToLower(f)), " ")
	}
	return strings.Join(fields, "|")
}
