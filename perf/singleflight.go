package perf

import (
	"context"

	"golang.org/x/sync/singleflight"
)

// SingleFlight collapses concurrent calls sharing a key into one underlying
// call: when a cached entry expires, multiple concurrent requests for the
// same key would otherwise all recompute it at the same time; SingleFlight
// makes only one of them do the work, and the rest wait for its result. The
// zero value is ready to use.
//
// It is a context-aware wrapper over golang.org/x/sync/singleflight, so a
// caller already using perf's other middleware does not need a second
// import for this pattern.
type SingleFlight struct {
	group singleflight.Group
}

// Do calls fn for key unless a call for the same key is already in flight, in
// which case it waits for that call and shares its result. shared reports
// whether the result was shared with another caller.
func (s *SingleFlight) Do(ctx context.Context, key string, fn func(ctx context.Context) (any, error)) (v any, err error, shared bool) {
	return s.group.Do(key, func() (any, error) { return fn(ctx) })
}

// Forget tells SingleFlight that key is no longer in flight, so the next Do
// for it starts a fresh call instead of waiting on one already finished. A
// Do that has already returned is forgotten automatically; Forget is for a
// caller that wants the next Do to reach the underlying source immediately,
// for example right after invalidating whatever fn reads from.
func (s *SingleFlight) Forget(key string) {
	s.group.Forget(key)
}
