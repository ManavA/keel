package perf

import (
	"context"
	"fmt"
	"runtime/debug"

	"golang.org/x/sync/singleflight"
)

// SingleFlight collapses concurrent calls sharing a key into one underlying
// call: when a cached entry expires, multiple concurrent requests for the
// same key would otherwise all recompute it at the same time; SingleFlight
// makes only one of them do the work, and the rest wait for its result. The
// zero value is ready to use.
//
// The underlying call runs with context.Background(), not any one caller's
// context: several callers with independent contexts and lifetimes can be
// waiting on the same key, and tying the shared work to one of them would
// mean that caller's cancellation either kills the work for everyone still
// waiting, or — if the cancelled caller happened to be the one whose
// context was in use — surfaces that caller's cancellation error to callers
// who never cancelled anything. Do still honors each caller's own context
// for how long that caller personally waits: a caller whose context is
// cancelled stops waiting immediately and gets its own ctx.Err(), while the
// underlying call keeps running for whoever else is still waiting on it.
type SingleFlight struct {
	group singleflight.Group
}

// ErrPanicked reports that fn panicked. Do recovers the panic itself and
// returns it as this error rather than letting it reach
// golang.org/x/sync/singleflight's own handling: that package deliberately
// crashes the process on an unrecovered panic whenever more than one caller
// is waiting on DoChan, by spawning a goroutine that re-panics — a caller's
// own recover cannot catch a panic raised in a different goroutine, so
// there is no way to prevent the crash from outside. Recovering here, before
// fn's panic ever reaches that code, avoids it entirely and reports the
// panic as an ordinary error to every caller waiting on the key instead.
type ErrPanicked struct {
	Value any
	Stack []byte
}

func (e *ErrPanicked) Error() string {
	return fmt.Sprintf("singleflight: panic: %v", e.Value)
}

// Do calls fn for key unless a call for the same key is already in flight,
// in which case it waits for that call and shares its result. shared
// reports whether the result was shared with another caller. If ctx is
// done before a result is available, Do returns ctx.Err() without waiting
// further; the underlying call is not affected and other callers waiting on
// the same key are not woken by this caller's cancellation. If fn panics,
// Do returns an *ErrPanicked instead of propagating the panic.
func (s *SingleFlight) Do(ctx context.Context, key string, fn func(ctx context.Context) (any, error)) (v any, shared bool, err error) {
	ch := s.group.DoChan(key, func() (result any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = &ErrPanicked{Value: r, Stack: debug.Stack()}
			}
		}()
		return fn(context.Background())
	})
	select {
	case res := <-ch:
		return res.Val, res.Shared, res.Err
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

// Forget tells SingleFlight that key is no longer in flight, so the next Do
// for it starts a fresh call instead of waiting on one already finished. A
// Do that has already returned is forgotten automatically; Forget is for a
// caller that wants the next Do to reach the underlying source immediately,
// for example right after invalidating whatever fn reads from.
func (s *SingleFlight) Forget(key string) {
	s.group.Forget(key)
}
