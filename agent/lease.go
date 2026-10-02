package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// errCancelRequested is the cause of a held context that ended because
// cancellation of the run was requested.
var errCancelRequested = errors.New("agent: cancellation requested")

// keepOptions configures keep. Store, Clock and TTL are required.
type keepOptions struct {
	Store Store
	// Clock gives the time each heartbeat carries, and the time a store that
	// fails is measured against.
	Clock Clock
	// TTL is how long each heartbeat extends the lease for. It is the TTL the
	// run was claimed with.
	TTL time.Duration
	// Interval is how often the lease is extended. Zero or less is a third
	// of TTL.
	Interval time.Duration
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// keep extends lease every Interval until stop is called. The context it
// returns is ctx, cancelled with cause ErrLeaseLost when a heartbeat finds
// the lease gone or no heartbeat has succeeded for a whole TTL, and with
// cause errCancelRequested when a heartbeat reports the request.
//
// The hold on a run is its epoch and not its expiry, so what a heartbeat
// can say is kept apart three ways:
//
//   - The store answers ErrLeaseLost, or ErrNotFound for a run that is no
//     longer there. The lease is gone, and the held context ends at once.
//   - The store fails in any other way, which says nothing about the lease.
//     keep goes on trying, since a lease that lapsed and that nobody took is
//     still extended by the next heartbeat to arrive. It gives up when the
//     Clock stands a whole TTL past the last heartbeat that succeeded, which
//     is when another process may have claimed the run, and not before.
//     Until a heartbeat has succeeded, the TTL is counted from the call to
//     keep, which follows the claim.
//   - The store reports a cancel request. The same heartbeat extended the
//     lease, so the run is still the caller's to finish as cancelled.
//
// Lease times are read from the Clock. The wait between heartbeats is a
// timer, and each heartbeat is given one Interval to answer, so a store that
// hangs is a store that fails. Each of the three is noticed by the
// heartbeat that finds it, which is within an Interval or so of its
// happening.
//
// stop ends the heartbeats and returns once the goroutine that makes them
// has exited, so no heartbeat is made after stop returns. It ends the
// context of a heartbeat in flight and waits for the store to return from
// it. It also releases the held context: one that is still live ends with
// context.Canceled as its cause, which is neither of the causes above, so
// whatever is written after stop is written under ctx. stop may be called
// more than once, and from any goroutine. The goroutine also exits when
// ctx ends; stop must be called all the same.
//
// A TTL of zero or less keeps nothing, since no store takes such a lease:
// the context returned has already ended, with cause ErrLeaseLost.
func keep(ctx context.Context, lease Lease, opts keepOptions) (held context.Context, stop func()) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("run", lease.RunID, "owner", lease.Owner, "epoch", lease.Epoch)

	held, cancel := context.WithCancelCause(ctx)
	if opts.TTL <= 0 {
		logger.Error("agent: a lease cannot be kept with no time to live", "ttl", opts.TTL)
		cancel(ErrLeaseLost)
		return held, func() {}
	}
	interval := opts.Interval
	if interval <= 0 {
		// Rounded up, so that the shortest TTL still has an interval.
		interval = (opts.TTL + 2) / 3
	}

	last := opts.Clock.Now()
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-held.Done():
			case <-ticker.C:
			}
			// Both may be ready at once. Whichever was chosen, no heartbeat
			// is begun for a hold that has ended.
			if held.Err() != nil {
				return
			}

			now := opts.Clock.Now()
			beat, release := context.WithTimeout(held, interval)
			cancelRequested, err := opts.Store.Heartbeat(beat, lease, now, opts.TTL)
			release()

			switch {
			case held.Err() != nil:
				// The hold ended while the store had the call. Its answer is
				// then the context's, and says nothing about the lease.
				return
			case err == nil && cancelRequested:
				logger.Info("agent: cancellation of the run was requested")
				cancel(errCancelRequested)
				return
			case err == nil:
				last = now
			case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrNotFound):
				logger.Warn("agent: lease lost", "error", err)
				cancel(ErrLeaseLost)
				return
			default:
				silent := opts.Clock.Now().Sub(last)
				if silent >= opts.TTL {
					logger.Warn("agent: lease given up: no heartbeat has succeeded for a whole TTL",
						"error", err, "silent", silent, "ttl", opts.TTL)
					cancel(ErrLeaseLost)
					return
				}
				logger.Warn("agent: heartbeat failed", "error", err, "silent", silent, "ttl", opts.TTL)
			}
		}
	}()

	return held, func() {
		cancel(nil)
		<-done
	}
}
