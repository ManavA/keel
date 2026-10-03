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
	// Interval is how often the lease is extended. It must be more than zero
	// and less than TTL: with a longer one the lease would lapse between
	// heartbeats while every one of them succeeded. keep has no error to
	// return, so any other value is replaced by a third of TTL and reported
	// once in the log, as an error, with both values.
	Interval time.Duration
	// Logger defaults to slog.Default.
	Logger *slog.Logger
	// Lost, when set, is called once, from the keeper's goroutine, when the
	// lease is lost or given up. It is called after a cancel request too,
	// when the held context has already ended with another cause.
	Lost func()
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
//     longer there. The lease is gone: the held context ends and so do the
//     heartbeats.
//   - The store fails in any other way, which says nothing about the lease.
//     keep goes on trying, since a lease that lapsed and that nobody took is
//     still extended by the next heartbeat to arrive. It gives up, as for a
//     lease that is gone, when the Clock stands a whole TTL past the last
//     heartbeat that succeeded, and not before.
//   - The store reports a cancel request. The held context ends, and the
//     heartbeats go on until stop: the run is still the caller's to finish
//     as cancelled, which may take longer than a TTL when it has child runs
//     to cancel. If the lease is then lost the heartbeats end, and the
//     cause stays errCancelRequested.
//
// Lease times are read from the Clock. The wait between heartbeats is a
// timer, and each heartbeat is given one Interval to answer, so a store that
// hangs is a store that fails. That gives how long the caller may go on
// working on a run it no longer holds:
//
//   - With the store answering, a lease another process took is noticed at
//     the first heartbeat answered after the takeover: at most one Interval
//     and one round trip to the store later.
//   - With the store failing, another process may claim the run from the
//     last heartbeat that succeeded plus the TTL. The held context ends at
//     the first failed answer at or after that moment, which is less than
//     two Intervals later: up to one for the next heartbeat to begin, and up
//     to one for it to be cut off when the store hangs.
//   - Until a first heartbeat has succeeded, the TTL is counted from the
//     call to keep, which follows the claim by however long the caller took.
//
// stop ends the heartbeats and returns once the goroutine that makes them
// has exited, so no heartbeat is made after stop returns. It ends the
// context of a heartbeat in flight and blocks until the store returns from
// it, so a store must honour its context: over one that does not, stop can
// block for as long as the store does, and a loss goes unnoticed for as
// long too. stop also releases the held context: one that is still live
// ends with context.Canceled as its cause, which is neither of the causes
// above, so whatever is written after stop is written under ctx. stop may
// be called more than once, and from any goroutine. The goroutine also
// exits when ctx ends; stop must be called all the same.
//
// A TTL of zero or less keeps nothing, since no store takes such a lease:
// the context returned has already ended, with cause ErrLeaseLost. A nil
// Store or Clock is a fault in the caller, and panics at the call.
func keep(ctx context.Context, lease Lease, opts keepOptions) (held context.Context, stop func()) {
	if opts.Store == nil {
		panic("agent: keep: Store is nil")
	}
	if opts.Clock == nil {
		panic("agent: keep: Clock is nil")
	}
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
	if interval <= 0 || interval >= opts.TTL {
		// Rounded up, so that the shortest TTL still has an interval.
		interval = (opts.TTL + 2) / 3
		logger.Error("agent: the heartbeat interval must be more than zero and less than the lease's TTL; using a third of the TTL",
			"interval", opts.Interval, "ttl", opts.TTL, "using", interval)
	}

	lost := func() {
		if opts.Lost != nil {
			opts.Lost()
		}
	}

	// beating is the keeper's own context. It is not held, because a cancel
	// request ends held and leaves the heartbeats going.
	beating, quit := context.WithCancel(ctx)
	last := opts.Clock.Now()
	ticker := time.NewTicker(interval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		cancelSeen := false
		for {
			select {
			case <-beating.Done():
			case <-ticker.C:
			}
			// Both may be ready at once. Whichever was chosen, no heartbeat
			// is begun once the keeper has been told to end.
			if beating.Err() != nil {
				return
			}

			now := opts.Clock.Now()
			beat, release := context.WithTimeout(beating, interval)
			cancelRequested, err := opts.Store.Heartbeat(beat, lease, now, opts.TTL)
			release()

			switch {
			case beating.Err() != nil:
				// The keeper was ended while the store had the call. Its
				// answer is then the context's, and says nothing about the
				// lease.
				return
			case err == nil:
				last = now
				if cancelRequested && !cancelSeen {
					cancelSeen = true
					logger.Info("agent: cancellation of the run was requested")
					cancel(errCancelRequested)
				}
			case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrNotFound):
				logger.Warn("agent: lease lost", "error", err)
				cancel(ErrLeaseLost)
				lost()
				return
			default:
				silent := opts.Clock.Now().Sub(last)
				if silent >= opts.TTL {
					logger.Warn("agent: lease given up: no heartbeat has succeeded for a whole TTL",
						"error", err, "silent", silent, "ttl", opts.TTL)
					cancel(ErrLeaseLost)
					lost()
					return
				}
				logger.Warn("agent: heartbeat failed", "error", err, "silent", silent, "ttl", opts.TTL)
			}
		}
	}()

	return held, func() {
		cancel(nil)
		quit()
		<-done
	}
}
