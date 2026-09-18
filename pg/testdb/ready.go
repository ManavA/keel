package testdb

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A Probe answers the two questions Ready asks. Start supplies one backed by a
// real connection; the tests supply scripted ones, which is the only way to put
// a restart at a precise moment.
type Probe interface {
	// Accepting runs a trivial query over the connection the tests will use.
	Accepting(context.Context) error

	// StartTime reads pg_postmaster_start_time().
	StartTime(context.Context) (time.Time, error)
}

// Errors from Ready, so a caller can tell a timeout apart from a bad probe.
var (
	// ErrNotAccepting means the database never answered a query over host TCP
	// within the deadline.
	ErrNotAccepting = errors.New("testdb: database never accepted a connection")

	// ErrUnstable means the database answered but kept restarting, so no two
	// reads of its start time ever agreed.
	ErrUnstable = errors.New("testdb: database kept restarting")
)

// Ready blocks until the database behind probe is usable, or ctx expires. Ready
// means a query succeeds and two reads of the postmaster start time, gap apart,
// return the same value.
//
// Comparing the two reads is what makes this more than a retry. A probe that
// merely succeeds twice accepts a pair straddling a restart, where both halves
// succeeded against different servers.
func Ready(ctx context.Context, probe Probe, gap time.Duration) error {
	if probe == nil {
		return errors.New("testdb: Ready needs a probe")
	}
	if gap < 0 {
		gap = 0
	}

	var (
		everAccepted  bool
		everReadClock bool
		lastErr       error
	)

	for {
		if err := ctx.Err(); err != nil {
			return timeoutError(everAccepted, everReadClock, lastErr)
		}

		if err := probe.Accepting(ctx); err != nil {
			lastErr = err
			if !sleep(ctx, 250*time.Millisecond) {
				return timeoutError(everAccepted, everReadClock, lastErr)
			}
			continue
		}
		everAccepted = true

		first, err := probe.StartTime(ctx)
		if err != nil {
			lastErr = err
			if !sleep(ctx, 250*time.Millisecond) {
				return timeoutError(everAccepted, everReadClock, lastErr)
			}
			continue
		}
		everReadClock = true

		if !sleep(ctx, gap) {
			return timeoutError(everAccepted, everReadClock, lastErr)
		}

		// Re-probe in full: a restart during the gap can break the query too,
		// not only move the start time.
		if err := probe.Accepting(ctx); err != nil {
			lastErr = err
			continue
		}
		second, err := probe.StartTime(ctx)
		if err != nil {
			lastErr = err
			continue
		}

		if first.Equal(second) {
			return nil
		}
		// The postmaster restarted between the two reads. Round again.
	}
}

// timeoutError names the probe that never succeeded rather than reporting a
// generic timeout. The query failing points at the port or a firewall, the
// clock failing at permissions, and neither agreeing at a restart loop.
func timeoutError(everAccepted, everReadClock bool, lastErr error) error {
	switch {
	case !everAccepted:
		return fmt.Errorf("%w over host TCP within the deadline (last error: %w)", ErrNotAccepting, orNil(lastErr))
	case !everReadClock:
		return fmt.Errorf("testdb: queries succeeded but pg_postmaster_start_time() never returned a value (last error: %w)", orNil(lastErr))
	default:
		return fmt.Errorf("%w: no two reads of its start time agreed before the deadline", ErrUnstable)
	}
}

var errNoDetail = errors.New("none recorded")

func orNil(err error) error {
	if err == nil {
		return errNoDetail
	}
	return err
}

// sleep waits for d, and reports false if ctx expired first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
