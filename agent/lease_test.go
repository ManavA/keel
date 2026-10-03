package agent_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// The keeper is tested inside a synctest bubble. Its timer runs on the
// bubble's clock, which moves only when the test sleeps, and its lease times
// come from the kit's clock, which moves only when the test advances it. So a
// test says exactly how many times the keeper has woken and what time each
// heartbeat carried, and nothing waits on the machine's clock.
//
// The bubble also checks for a keeper left running. Its clock stops when the
// test's own goroutine returns, so a keeper still waiting on its timer then
// could never wake, and synctest fails the test as deadlocked. Unless a test
// says otherwise, the keeper's parent context is one nothing cancels, so
// only the keeper itself or its stop can have ended it.

var leaseStart = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

const (
	leaseTTL      = 30 * time.Second
	leaseInterval = 10 * time.Second

	leaseRunID   = "6f1b6c0e-3d5a-4f0e-9a51-0c2d7e8b9a10"
	leaseNoRunID = "6f1b6c0e-3d5a-4f0e-9a51-0c2d7e8b9a11"
	leaseAgent   = "alpha"
	leaseOwner   = "worker-a"
	leaseRival   = "worker-b"
)

// leaseBeat is one heartbeat as the keeper made it.
type leaseBeat struct {
	lease agent.Lease
	now   time.Time
	ttl   time.Duration
	// bound is how long the call was given, and zero when it had no deadline.
	bound time.Duration
	// ended is the error of the call's context as the call arrived: nil for
	// a context that was live.
	ended error
}

// leaseStore is the store a keeper under test beats against. It notes every
// heartbeat as it arrives, before any fault, and can hold one up.
type leaseStore struct {
	agent.Store

	mu    sync.Mutex
	beats []leaseBeat
	// hold, when set, is called with the heartbeat's context before the call
	// goes on. An error it returns is the heartbeat's.
	hold func(ctx context.Context) error
	// cancelWithError makes an error from hold come back with the cancel
	// mark set as well, which no store should do.
	cancelWithError bool
}

func (s *leaseStore) Heartbeat(ctx context.Context, lease agent.Lease, now time.Time, ttl time.Duration) (bool, error) {
	beat := leaseBeat{lease: lease, now: now, ttl: ttl, ended: ctx.Err()}
	if deadline, ok := ctx.Deadline(); ok {
		beat.bound = time.Until(deadline)
	}
	s.mu.Lock()
	s.beats = append(s.beats, beat)
	hold, cancelWithError := s.hold, s.cancelWithError
	s.mu.Unlock()

	if hold != nil {
		if err := hold(ctx); err != nil {
			return cancelWithError, err
		}
	}
	return s.Store.Heartbeat(ctx, lease, now, ttl)
}

func (s *leaseStore) seen() []leaseBeat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]leaseBeat(nil), s.beats...)
}

func (s *leaseStore) holdWith(hold func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = hold
}

// leaseLog collects what a keeper logs.
type leaseLog struct {
	mu      sync.Mutex
	entries []leaseEntry
}

type leaseEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (l *leaseLog) all() []leaseEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]leaseEntry(nil), l.entries...)
}

func (l *leaseLog) logger() *slog.Logger { return slog.New(leaseHandler{log: l}) }

// leaseHandler writes to a leaseLog, with the attributes a logger was given
// ahead of the ones a record carries.
type leaseHandler struct {
	log   *leaseLog
	attrs []slog.Attr
}

func (h leaseHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h leaseHandler) Handle(_ context.Context, r slog.Record) error {
	entry := leaseEntry{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	for _, a := range h.attrs {
		entry.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		entry.attrs[a.Key] = a.Value.String()
		return true
	})
	h.log.mu.Lock()
	defer h.log.mu.Unlock()
	h.log.entries = append(h.log.entries, entry)
	return nil
}

func (h leaseHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return leaseHandler{log: h.log, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h leaseHandler) WithGroup(string) slog.Handler { return h }

// leaseFixture is one run, claimed by leaseOwner at leaseStart, behind a
// store that can be made to fail.
type leaseFixture struct {
	t      *testing.T
	clock  *agenttest.Clock
	memory *agent.MemoryStore
	faults *agenttest.FaultStore
	store  *leaseStore
	lease  agent.Lease
	logs   *leaseLog
}

func newLeaseFixture(t *testing.T) *leaseFixture {
	t.Helper()
	ctx := context.Background()
	f := &leaseFixture{
		t:      t,
		clock:  agenttest.NewClock(leaseStart),
		memory: agent.NewMemoryStore(),
		logs:   &leaseLog{},
	}
	f.faults = agenttest.NewFaultStore(f.memory)
	f.store = &leaseStore{Store: f.faults}

	_, _, err := f.memory.CreateRun(ctx, agent.Run{
		ID: leaseRunID, Agent: leaseAgent, Status: agent.StatusRunnable, Input: "input",
		CreatedAt: leaseStart, UpdatedAt: leaseStart,
	})
	require.NoError(t, err)
	claimed, err := f.memory.Claim(ctx, agent.ClaimRequest{
		Owner: leaseOwner, Agents: []string{leaseAgent}, RunID: leaseRunID, Now: f.clock.Now(), TTL: leaseTTL,
	})
	require.NoError(t, err)
	require.NotNil(t, claimed)
	f.lease = claimed.Lease()
	return f
}

func (f *leaseFixture) options() agent.KeepOptions {
	return agent.KeepOptions{
		Store: f.store, Clock: f.clock, TTL: leaseTTL, Interval: leaseInterval,
		Logger: f.logs.logger(),
	}
}

// keep starts a keeper of the fixture's lease under a context nothing
// cancels.
func (f *leaseFixture) keep() (held context.Context, stop func()) {
	return agent.Keep(context.Background(), f.lease, f.options())
}

// wake lets one Interval pass on the keeper's timer and returns once the
// keeper has done what that woke it to do and is blocked again.
func (f *leaseFixture) wake() {
	time.Sleep(leaseInterval)
	synctest.Wait()
}

// pass moves the kit's clock and the keeper's timer on together by d, as
// both move for a process running in real time.
func (f *leaseFixture) pass(d time.Duration) {
	f.clock.Advance(d)
	time.Sleep(d)
	synctest.Wait()
}

// takeOver lets the lease lapse on the clock without the keeper waking, as it
// would for a process that was paused, and claims the run for leaseRival.
func (f *leaseFixture) takeOver() agent.Run {
	f.t.Helper()
	f.clock.Advance(leaseTTL)
	rival, err := f.memory.Claim(context.Background(), agent.ClaimRequest{
		Owner: leaseRival, Agents: []string{leaseAgent}, RunID: leaseRunID, Now: f.clock.Now(), TTL: leaseTTL,
	})
	require.NoError(f.t, err)
	require.NotNil(f.t, rival)
	require.Equal(f.t, int64(2), rival.LeaseEpoch)
	return *rival
}

func (f *leaseFixture) run() agent.Run {
	f.t.Helper()
	run, err := f.memory.GetRun(context.Background(), leaseRunID)
	require.NoError(f.t, err)
	return run
}

func (f *leaseFixture) expiry() time.Time {
	f.t.Helper()
	run := f.run()
	require.NotNil(f.t, run.LeaseExpiresAt)
	return *run.LeaseExpiresAt
}

// since is how far the kit's clock has moved from leaseStart.
func (f *leaseFixture) since() time.Duration { return f.clock.Now().Sub(leaseStart) }

// requireLeaseHeld fails unless the held context is still live.
func requireLeaseHeld(t *testing.T, held context.Context, when string) {
	t.Helper()
	require.NoError(t, held.Err(), when)
	require.NoError(t, context.Cause(held), when)
}

// requireLeaseEnded fails unless the held context has ended with exactly
// cause.
func requireLeaseEnded(t *testing.T, held context.Context, cause error, when string) {
	t.Helper()
	require.ErrorIs(t, held.Err(), context.Canceled, when)
	// The cause is the sentinel itself, not something wrapping it or
	// something that reads the same.
	require.Same(t, cause, context.Cause(held), when)
}

// requireLeaseQuiet lets the keeper's timer run on and fails if another
// heartbeat is made.
func requireLeaseQuiet(t *testing.T, f *leaseFixture) {
	t.Helper()
	before := len(f.store.seen())
	for range 5 {
		f.clock.Advance(leaseInterval)
		f.wake()
	}
	require.Len(t, f.store.seen(), before, "a heartbeat was made after the keeper ended")
}

func TestKeep_ExtendsTheLeaseEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()

		// Nothing happens before the first Interval is up.
		time.Sleep(leaseInterval - time.Nanosecond)
		synctest.Wait()
		require.Empty(t, f.store.seen())
		assert.Equal(t, leaseStart.Add(leaseTTL), f.expiry(), "the claim's own expiry")

		// The clock moves by something other than the Interval, so an expiry
		// worked out from the timer would not match.
		f.clock.Advance(7 * time.Second)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		require.Len(t, f.store.seen(), 1)
		assert.Equal(t, leaseStart.Add(7*time.Second+leaseTTL), f.expiry())

		for i := 2; i <= 6; i++ {
			f.clock.Advance(7 * time.Second)
			f.wake()
			require.Len(t, f.store.seen(), i)
			assert.Equal(t, f.clock.Now().Add(leaseTTL), f.expiry(), "after heartbeat %d", i)
		}
		requireLeaseHeld(t, held, "after six heartbeats")

		for i, beat := range f.store.seen() {
			assert.Equal(t, f.lease, beat.lease, "heartbeat %d", i+1)
			assert.Equal(t, leaseTTL, beat.ttl, "heartbeat %d", i+1)
			assert.Equal(t, leaseStart.Add(time.Duration(i+1)*7*time.Second), beat.now, "heartbeat %d", i+1)
			assert.Equal(t, leaseInterval, beat.bound, "heartbeat %d is given one Interval", i+1)
		}
		assert.Empty(t, f.logs.all(), "a heartbeat that goes well is not logged")

		run := f.run()
		assert.Equal(t, leaseOwner, run.LeaseOwner)
		assert.Equal(t, int64(1), run.LeaseEpoch)
	})
}

func TestKeep_TheHeldContextIsTheParent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		type key struct{}
		parent := context.WithValue(context.Background(), key{}, "carried")

		held, stop := agent.Keep(parent, f.lease, f.options())
		defer stop()

		assert.Equal(t, "carried", held.Value(key{}))
		requireLeaseHeld(t, held, "at the start")
	})
}

func TestKeep_StopEndsTheHeartbeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()

		f.clock.Advance(leaseInterval)
		f.wake()
		require.Len(t, f.store.seen(), 1)
		expiry := f.expiry()

		stop()

		// Stopping releases the held context, and its cause is neither of
		// the two a caller acts on.
		requireLeaseEnded(t, held, context.Canceled, "after stop")
		require.Len(t, f.store.seen(), 1, "stop makes no heartbeat of its own")
		requireLeaseQuiet(t, f)
		assert.Equal(t, expiry, f.expiry(), "the lease is as the last heartbeat left it")
		assert.Empty(t, f.logs.all(), "stopping is not logged")

		stop()
		requireLeaseEnded(t, held, context.Canceled, "after a second stop")
	})
}

func TestKeep_StopWaitsForAHeartbeatInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		// A store that does not look at its context: the call ends only
		// when the test lets it.
		gate := make(chan struct{})
		f.store.holdWith(func(context.Context) error {
			<-gate
			return nil
		})
		release := sync.OnceFunc(func() { close(gate) })
		_, stop := f.keep()
		defer stop()
		defer release()

		f.wake()
		require.Len(t, f.store.seen(), 1, "the heartbeat is in flight")

		stopped := make(chan struct{})
		go func() {
			stop()
			close(stopped)
		}()
		synctest.Wait()
		select {
		case <-stopped:
			t.Fatal("stop returned while a heartbeat was still in the store")
		default:
		}

		release()
		<-stopped
	})
}

func TestKeep_ASecondClaimEndsTheHoldAsLeaseLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()

		f.clock.Advance(leaseInterval)
		f.wake()
		require.Equal(t, leaseStart.Add(leaseInterval+leaseTTL), f.expiry())

		rival := f.takeOver()
		requireLeaseHeld(t, held, "before the keeper has looked")

		f.wake()

		requireLeaseEnded(t, held, agent.ErrLeaseLost, "after the heartbeat that found the epoch moved")
		run := f.run()
		assert.Equal(t, leaseRival, run.LeaseOwner)
		assert.Equal(t, int64(2), run.LeaseEpoch)
		assert.Equal(t, rival.LeaseExpiresAt, run.LeaseExpiresAt, "the rival's lease is not touched")
		requireLeaseQuiet(t, f)

		logged := f.logs.all()
		require.Len(t, logged, 1)
		assert.Equal(t, slog.LevelWarn, logged[0].level)
		assert.Equal(t, leaseRunID, logged[0].attrs["run"])
		assert.Equal(t, leaseOwner, logged[0].attrs["owner"])
		assert.Equal(t, "1", logged[0].attrs["epoch"])
	})
}

func TestKeep_ALapsedLeaseNobodyTookIsKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()

		// The hold is the epoch, not the time: twice the TTL goes by with no
		// heartbeat and no other claim, and the next heartbeat extends it.
		f.clock.Advance(2 * leaseTTL)
		f.wake()

		requireLeaseHeld(t, held, "after a heartbeat on a lapsed lease")
		assert.Equal(t, f.clock.Now().Add(leaseTTL), f.expiry())
		assert.Empty(t, f.logs.all())
	})
}

func TestKeep_EveryWayTheStoreSaysTheLeaseIsGone(t *testing.T) {
	cases := []struct {
		name string
		// lose arranges for the next heartbeat to find the lease gone.
		lose func(t *testing.T, f *leaseFixture)
	}{
		{"the run was claimed again", func(_ *testing.T, f *leaseFixture) { f.takeOver() }},
		{"the run was given back", func(t *testing.T, f *leaseFixture) {
			require.NoError(t, f.memory.Yield(context.Background(), f.lease, agent.YieldRequest{Now: f.clock.Now()}))
		}},
		{"the store wraps the error", func(_ *testing.T, f *leaseFixture) {
			f.store.holdWith(func(context.Context) error {
				return fmt.Errorf("store: heartbeat: %w", fmt.Errorf("fenced: %w", agent.ErrLeaseLost))
			})
		}},
		{"the run does not exist", func(_ *testing.T, f *leaseFixture) {
			f.store.holdWith(func(context.Context) error {
				return fmt.Errorf("store: heartbeat: %w", agent.ErrNotFound)
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				held, stop := f.keep()
				defer stop()

				tc.lose(t, f)
				// The loss is known from the store's answer at once. No time
				// has to pass on the clock, as it must for a store that fails.
				now := f.clock.Now()
				f.wake()

				require.Equal(t, now, f.clock.Now())
				requireLeaseEnded(t, held, agent.ErrLeaseLost, "after one heartbeat")
				requireLeaseQuiet(t, f)
				assert.Len(t, f.logs.all(), 1)
			})
		})
	}
}

func TestKeep_ARunThatDoesNotExistIsLeaseLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		lease := agent.Lease{RunID: leaseNoRunID, Owner: leaseOwner, Epoch: 1}
		held, stop := agent.Keep(context.Background(), lease, f.options())
		defer stop()

		f.wake()

		requireLeaseEnded(t, held, agent.ErrLeaseLost, "after one heartbeat")
		requireLeaseQuiet(t, f)
	})
}

func TestKeep_ACancelRequestEndsTheHoldAsCancelRequested(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()

		f.clock.Advance(leaseInterval)
		f.wake()
		requireLeaseHeld(t, held, "before the request")

		require.NoError(t, f.memory.RequestCancel(context.Background(), agent.CancelRequest{
			RunID: leaseRunID, By: "operator", Reason: "no longer wanted", Now: f.clock.Now(),
		}))
		requireLeaseHeld(t, held, "before the keeper has looked")

		f.clock.Advance(leaseInterval)
		f.wake()

		requireLeaseEnded(t, held, agent.ErrCancelRequested, "after the heartbeat that reported the request")
		assert.NotErrorIs(t, context.Cause(held), agent.ErrLeaseLost)
		// The run is still this worker's to finish as cancelled: the
		// heartbeat that carried the news also extended the lease.
		run := f.run()
		assert.Equal(t, leaseOwner, run.LeaseOwner)
		assert.Equal(t, f.clock.Now().Add(leaseTTL), f.expiry())
		require.Len(t, f.store.seen(), 2)

		// And it stays this worker's for as long as finishing takes, which
		// may be longer than a TTL: the heartbeats go on until stop.
		for i := 3; i <= 8; i++ {
			f.clock.Advance(leaseInterval)
			f.wake()
			require.Len(t, f.store.seen(), i)
			assert.Equal(t, f.clock.Now().Add(leaseTTL), f.expiry(), "after heartbeat %d", i)
			requireLeaseEnded(t, held, agent.ErrCancelRequested, "while the heartbeats go on")
		}

		// A heartbeat made under the held context would arrive already
		// cancelled, and a store that honours its context would refuse it.
		for i, beat := range f.store.seen() {
			assert.NoError(t, beat.ended, "heartbeat %d arrived with its context ended", i+1)
		}

		// Seven heartbeats carried the mark. It is logged once, and not as a
		// fault.
		logged := f.logs.all()
		require.Len(t, logged, 1)
		assert.Less(t, logged[0].level, slog.LevelWarn)
		assert.Equal(t, leaseRunID, logged[0].attrs["run"])

		stop()
		requireLeaseEnded(t, held, agent.ErrCancelRequested, "after stop")
		require.Len(t, f.store.seen(), 8)
		requireLeaseQuiet(t, f)
	})
}

func TestKeep_ALossAfterACancelRequestEndsTheHeartbeatsAndKeepsTheCause(t *testing.T) {
	cases := []struct {
		name string
		// lose arranges for the lease to be lost at the next heartbeat.
		lose func(f *leaseFixture)
	}{
		{"the lease is taken", func(f *leaseFixture) { f.takeOver() }},
		{"the store fails for a whole TTL", func(f *leaseFixture) {
			f.faults.Kill()
			f.clock.Advance(leaseTTL)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				var lost atomic.Int32
				opts := f.options()
				opts.Lost = func() { lost.Add(1) }
				held, stop := agent.Keep(context.Background(), f.lease, opts)
				defer stop()

				require.NoError(t, f.memory.RequestCancel(context.Background(), agent.CancelRequest{
					RunID: leaseRunID, By: "operator", Now: f.clock.Now(),
				}))
				f.wake()
				requireLeaseEnded(t, held, agent.ErrCancelRequested, "after the request")
				f.wake()
				require.Len(t, f.store.seen(), 2, "the heartbeats go on")

				tc.lose(f)
				f.wake()

				// The caller was told to cancel and is doing so. That the
				// lease then went does not change what it was told.
				requireLeaseEnded(t, held, agent.ErrCancelRequested, "after the loss")
				assert.Equal(t, int32(1), lost.Load(), "the loss is reported all the same, once")
				require.Len(t, f.store.seen(), 3)
				requireLeaseQuiet(t, f)

				logged := f.logs.all()
				require.Len(t, logged, 2, "the request, then the loss")
				assert.Equal(t, slog.LevelWarn, logged[1].level)
			})
		})
	}
}

func TestKeep_AStoreThatFailsLosesTheLeaseAfterAWholeTTLAndNotBefore(t *testing.T) {
	cases := []struct {
		name string
		// before runs with a working store and returns how far the clock
		// stood from leaseStart at the last heartbeat that succeeded.
		before func(t *testing.T, f *leaseFixture) time.Duration
		// fail makes every heartbeat from here on fail.
		fail func(f *leaseFixture)
	}{
		{
			name:   "from the start, counted from when keeping began",
			before: func(*testing.T, *leaseFixture) time.Duration { return 0 },
			fail:   func(f *leaseFixture) { f.faults.Kill() },
		},
		{
			name: "after heartbeats that succeeded, counted from the last of them",
			before: func(t *testing.T, f *leaseFixture) time.Duration {
				for range 3 {
					f.clock.Advance(8 * time.Second)
					f.wake()
				}
				require.Len(t, f.store.seen(), 3)
				return 24 * time.Second
			},
			fail: func(f *leaseFixture) { f.faults.Kill() },
		},
		{
			name: "after failures the store recovered from, counted from the recovery",
			before: func(t *testing.T, f *leaseFixture) time.Duration {
				f.faults.FailBefore("Heartbeat", 2)
				for range 2 {
					f.clock.Advance(12 * time.Second)
					f.wake()
				}
				require.Equal(t, leaseStart.Add(leaseTTL), f.expiry(), "neither heartbeat reached the store")
				f.clock.Advance(5 * time.Second)
				f.wake()
				require.Equal(t, leaseStart.Add(29*time.Second+leaseTTL), f.expiry(), "the third did")
				return 29 * time.Second
			},
			fail: func(f *leaseFixture) { f.faults.FailBefore("Heartbeat", 1000) },
		},
		{
			name: "after a heartbeat that was slow to answer, counted from the time it carried",
			before: func(t *testing.T, f *leaseFixture) time.Duration {
				f.store.holdWith(func(context.Context) error {
					f.clock.Advance(5 * time.Second)
					return nil
				})
				f.clock.Advance(10 * time.Second)
				f.wake()
				f.store.holdWith(nil)
				// The lease runs from the time the heartbeat carried, which
				// is what the store was told, not from when it answered.
				require.Equal(t, leaseStart.Add(10*time.Second+leaseTTL), f.expiry())
				require.Equal(t, 15*time.Second, f.since())
				return 10 * time.Second
			},
			fail: func(f *leaseFixture) { f.faults.Kill() },
		},
		{
			name:   "when a failing answer also carries the cancel mark, a failure and not a cancel request",
			before: func(*testing.T, *leaseFixture) time.Duration { return 0 },
			fail: func(f *leaseFixture) {
				f.store.mu.Lock()
				f.store.cancelWithError = true
				f.store.mu.Unlock()
				f.store.holdWith(func(context.Context) error { return errors.New("store: connection reset") })
			},
		},
		{
			name:   "when the write lands and the answer is lost, counted as a failure all the same",
			before: func(*testing.T, *leaseFixture) time.Duration { return 0 },
			fail:   func(f *leaseFixture) { f.faults.FailAfter("Heartbeat", 1000) },
		},
		{
			name:   "when the store's error is a context error of its own",
			before: func(*testing.T, *leaseFixture) time.Duration { return 0 },
			fail: func(f *leaseFixture) {
				f.store.holdWith(func(context.Context) error {
					return fmt.Errorf("store: dial: %w", context.DeadlineExceeded)
				})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				held, stop := f.keep()
				defer stop()

				last := tc.before(t, f)
				tc.fail(f)

				// Any number of failures, on the timer alone, do not lose the
				// lease: the clock has not moved since the last success.
				beats, logged := len(f.store.seen()), len(f.logs.all())
				for range 20 {
					f.wake()
				}
				require.Len(t, f.store.seen(), beats+20, "the keeper keeps trying")
				requireLeaseHeld(t, held, "after twenty failures and no time on the clock")

				f.clock.Advance(last + leaseTTL - f.since() - time.Nanosecond)
				f.wake()
				requireLeaseHeld(t, held, "one nanosecond short of a whole TTL since the last success")

				f.clock.Advance(time.Nanosecond)
				f.wake()
				requireLeaseEnded(t, held, agent.ErrLeaseLost, "a whole TTL after the last success")
				requireLeaseQuiet(t, f)

				// Every failed heartbeat is logged once with the store's
				// error, the last of them as the loss.
				failures := f.logs.all()[logged:]
				require.Len(t, failures, len(f.store.seen())-beats)
				for _, entry := range failures {
					assert.Equal(t, slog.LevelWarn, entry.level)
					assert.NotEmpty(t, entry.attrs["error"])
				}
			})
		})
	}
}

func TestKeep_AFailedHeartbeatIsLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()
		f.faults.FailBefore("Heartbeat", 1)

		f.clock.Advance(leaseInterval)
		f.wake()

		requireLeaseHeld(t, held, "after one failure")
		logged := f.logs.all()
		require.Len(t, logged, 1)
		assert.Equal(t, slog.LevelWarn, logged[0].level)
		assert.Equal(t, leaseRunID, logged[0].attrs["run"])
		assert.Equal(t, agenttest.ErrFault.Error(), logged[0].attrs["error"])

		f.clock.Advance(leaseInterval)
		f.wake()
		assert.Len(t, f.logs.all(), 1, "the heartbeat that then went well is not logged")
	})
}

func TestKeep_AStoreThatHangsIsCutOffAndCountedAsFailing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		f.store.holdWith(leaseHang)
		held, stop := f.keep()
		defer stop()

		f.wake()
		require.Len(t, f.store.seen(), 1, "the first heartbeat is in flight")
		assert.Equal(t, leaseInterval, f.store.seen()[0].bound)

		// The first is cut off after one Interval and the second begins.
		f.clock.Advance(leaseTTL - time.Nanosecond)
		f.wake()
		require.Len(t, f.store.seen(), 2)
		requireLeaseHeld(t, held, "one nanosecond short of a whole TTL")

		f.clock.Advance(time.Nanosecond)
		f.wake()
		requireLeaseEnded(t, held, agent.ErrLeaseLost, "a whole TTL with no answer")
		requireLeaseQuiet(t, f)
	})
}

func TestKeep_CancellingTheParentEndsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		parent, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		held, stop := agent.Keep(parent, f.lease, f.options())
		defer stop()

		f.clock.Advance(leaseInterval)
		f.wake()
		require.Len(t, f.store.seen(), 1)

		cancel(errLeaseShutdown)
		synctest.Wait()

		requireLeaseEnded(t, held, errLeaseShutdown, "after the parent ended")
		// The keeper ends with its parent, before stop is called, and makes
		// no heartbeat on its way out.
		require.Len(t, f.store.seen(), 1)
		requireLeaseQuiet(t, f)
		assert.Empty(t, f.logs.all())
		stop()
	})
}

func TestKeep_EndingInTheMiddleOfAHeartbeatIsNotAFailure(t *testing.T) {
	cases := []struct {
		name string
		// end ends the keeper while its heartbeat is in the store.
		end   func(stop func(), cancelParent context.CancelCauseFunc)
		cause error
	}{
		{"stopped", func(stop func(), _ context.CancelCauseFunc) { stop() }, context.Canceled},
		{"the parent ended", func(_ func(), cancel context.CancelCauseFunc) { cancel(nil) }, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				f.store.holdWith(leaseHang)
				parent, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				held, stop := agent.Keep(parent, f.lease, f.options())
				defer stop()

				f.wake()
				require.Len(t, f.store.seen(), 1, "the heartbeat is in flight")
				// A whole TTL has passed, so a keeper that took the store's
				// answer for a failure would call the lease lost.
				f.clock.Advance(2 * leaseTTL)

				// The heartbeat's context ends with the hold, so the store
				// returns at once, not when the heartbeat's own time is up.
				ended := time.Now()
				tc.end(stop, cancel)
				synctest.Wait()

				require.Equal(t, ended, time.Now(), "ending waited on the timer")
				requireLeaseEnded(t, held, tc.cause, "after ending")
				assert.Empty(t, f.logs.all(), "the context's own error is not a store failure")
				requireLeaseQuiet(t, f)
			})
		})
	}
}

// leaseHang is a store that never answers and gives up only when its context
// ends, as a connection into a network that drops packets does.
func leaseHang(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

var errLeaseShutdown = errors.New("shutting down")

// However a hold ends, nothing is left running once stop has returned. The
// check is the bubble's, as the note at the top of this file says: each case
// ends with stop, and a keeper that outlived it would fail the test.
func TestKeep_NothingIsLeftRunningOnceStopReturns(t *testing.T) {
	cases := []struct {
		name string
		// end brings the hold to where stop finds it.
		end func(t *testing.T, f *leaseFixture, cancelParent context.CancelCauseFunc)
		// cause is the held context's once stop has returned.
		cause error
	}{
		{
			name:  "stopped before the first heartbeat",
			end:   func(*testing.T, *leaseFixture, context.CancelCauseFunc) {},
			cause: context.Canceled,
		},
		{
			name:  "stopped between heartbeats",
			end:   func(_ *testing.T, f *leaseFixture, _ context.CancelCauseFunc) { f.wake() },
			cause: context.Canceled,
		},
		{
			name: "stopped with a heartbeat in the store",
			end: func(_ *testing.T, f *leaseFixture, _ context.CancelCauseFunc) {
				f.store.holdWith(leaseHang)
				f.wake()
			},
			cause: context.Canceled,
		},
		{
			name: "the lease was taken",
			end: func(_ *testing.T, f *leaseFixture, _ context.CancelCauseFunc) {
				f.takeOver()
				f.wake()
			},
			cause: agent.ErrLeaseLost,
		},
		{
			name: "cancellation was requested",
			end: func(t *testing.T, f *leaseFixture, _ context.CancelCauseFunc) {
				require.NoError(t, f.memory.RequestCancel(context.Background(), agent.CancelRequest{
					RunID: leaseRunID, By: "operator", Now: f.clock.Now(),
				}))
				f.wake()
			},
			cause: agent.ErrCancelRequested,
		},
		{
			name: "the store failed for a whole TTL",
			end: func(_ *testing.T, f *leaseFixture, _ context.CancelCauseFunc) {
				f.faults.Kill()
				f.clock.Advance(leaseTTL)
				f.wake()
			},
			cause: agent.ErrLeaseLost,
		},
		{
			name: "the store hung for a whole TTL",
			end: func(_ *testing.T, f *leaseFixture, _ context.CancelCauseFunc) {
				f.store.holdWith(leaseHang)
				f.wake()
				f.clock.Advance(leaseTTL)
				f.wake()
			},
			cause: agent.ErrLeaseLost,
		},
		{
			name: "the parent context ended",
			end: func(_ *testing.T, f *leaseFixture, cancelParent context.CancelCauseFunc) {
				f.wake()
				cancelParent(errLeaseShutdown)
			},
			cause: errLeaseShutdown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				// The parent is cancelled only by the case that says so.
				parent, cancelParent := context.WithCancelCause(context.Background())
				held, stop := agent.Keep(parent, f.lease, f.options())
				defer stop()
				tc.end(t, f, cancelParent)

				stop()

				requireLeaseEnded(t, held, tc.cause, "after stop")
				requireLeaseQuiet(t, f)
			})
		})
	}
}

func TestKeep_StopIsSafeFromManyGoroutinesWhileTheLeaseIsBeingLost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()

		rival := f.takeOver()

		// The stoppers sleep to the instant the keeper's timer fires, so the
		// heartbeat that finds the lease gone and the calls to stop start
		// together.
		var stoppers sync.WaitGroup
		for range 8 {
			stoppers.Go(func() {
				time.Sleep(leaseInterval)
				stop()
				stop()
			})
		}
		stoppers.Wait()

		require.ErrorIs(t, held.Err(), context.Canceled)
		cause := context.Cause(held)
		assert.True(t, errors.Is(cause, agent.ErrLeaseLost) || errors.Is(cause, context.Canceled), "cause is %v", cause)
		run := f.run()
		assert.Equal(t, leaseRival, run.LeaseOwner)
		assert.Equal(t, rival.LeaseExpiresAt, run.LeaseExpiresAt)
		requireLeaseQuiet(t, f)
	})
}

func TestKeep_AnIntervalThatCannotKeepTheLeaseIsReplacedByAThirdOfTheTTL(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		ttl      time.Duration
		// want is the interval the keeper beats at.
		want time.Duration
		// replaced is whether want is not the interval given.
		replaced bool
	}{
		{"one nanosecond below the TTL is used as given", 30*time.Second - time.Nanosecond, 30 * time.Second, 30*time.Second - time.Nanosecond, false},
		{"a third of the TTL is used as given", 10 * time.Second, 30 * time.Second, 10 * time.Second, false},
		{"one nanosecond is used as given", time.Nanosecond, 30 * time.Second, time.Nanosecond, false},
		{"equal to the TTL", 30 * time.Second, 30 * time.Second, 10 * time.Second, true},
		{"twice the TTL", time.Minute, 30 * time.Second, 10 * time.Second, true},
		{"zero", 0, 30 * time.Second, 10 * time.Second, true},
		{"negative", -time.Second, 30 * time.Second, 10 * time.Second, true},
		{"a TTL too short to divide", 0, 2 * time.Nanosecond, time.Nanosecond, true},
		{"a third that does not divide evenly is rounded up", 0, 10 * time.Nanosecond, 4 * time.Nanosecond, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				opts := f.options()
				opts.Interval, opts.TTL = tc.interval, tc.ttl
				held, stop := agent.Keep(context.Background(), f.lease, opts)
				defer stop()

				if tc.want > time.Nanosecond {
					time.Sleep(tc.want - time.Nanosecond)
					synctest.Wait()
					require.Empty(t, f.store.seen(), "before the interval is up")
					time.Sleep(time.Nanosecond)
				} else {
					time.Sleep(tc.want)
				}
				synctest.Wait()

				require.Len(t, f.store.seen(), 1)
				assert.Equal(t, tc.want, f.store.seen()[0].bound)
				assert.Equal(t, tc.ttl, f.store.seen()[0].ttl)
				requireLeaseHeld(t, held, "after the first heartbeat")

				// keep has no error to return, so a replaced interval is
				// said once, as an error, with both values.
				logged := f.logs.all()
				if !tc.replaced {
					assert.Empty(t, logged)
					return
				}
				require.Len(t, logged, 1)
				assert.Equal(t, slog.LevelError, logged[0].level)
				assert.Equal(t, tc.interval.String(), logged[0].attrs["interval"])
				assert.Equal(t, tc.ttl.String(), logged[0].attrs["ttl"])
				assert.Equal(t, tc.want.String(), logged[0].attrs["using"])
				assert.Equal(t, leaseRunID, logged[0].attrs["run"])
			})
		})
	}
}

// An interval longer than the TTL would let the lease lapse between
// heartbeats while the store was answering every one: a rival could take the
// run with nothing to tell the holder for the rest of the interval.
func TestKeep_AnIntervalLongerThanTheTTLDoesNotLetTheLeaseLapse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		opts := f.options()
		opts.Interval = 2 * leaseTTL
		held, stop := agent.Keep(context.Background(), f.lease, opts)
		defer stop()

		// Clock and timer together, a second at a time, for three TTLs, with
		// a rival asking for the run at every one.
		for range 3 * leaseTTL / time.Second {
			f.pass(time.Second)
			rival, err := f.memory.Claim(context.Background(), agent.ClaimRequest{
				Owner: leaseRival, Agents: []string{leaseAgent}, RunID: leaseRunID, Now: f.clock.Now(), TTL: leaseTTL,
			})
			require.ErrorIs(t, err, agent.ErrNotClaimable, "%s after the claim", f.since())
			require.Nil(t, rival)
		}
		requireLeaseHeld(t, held, "after three TTLs")
		assert.Len(t, f.store.seen(), 9)
	})
}

// The latest a store that fails is given up on. A heartbeat fails at once
// one nanosecond short of the TTL, so the hold goes on; the next one hangs,
// and its answer, an interval after it began, is the first at or after the
// moment a rival may claim.
func TestKeep_AStoreThatFailsIsGivenUpLessThanTwoIntervalsAfterARivalMayClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		held, stop := f.keep()
		defer stop()
		mayClaim := leaseStart.Add(leaseTTL)

		// The clock runs one nanosecond behind the timer from here on.
		f.faults.FailBefore("Heartbeat", 3)
		f.clock.Advance(leaseInterval - time.Nanosecond)
		f.wake()
		f.pass(leaseInterval)
		f.pass(leaseInterval)
		require.Len(t, f.store.seen(), 3)
		require.Equal(t, mayClaim.Add(-time.Nanosecond), f.clock.Now())
		requireLeaseHeld(t, held, "after a failure one nanosecond short of the TTL")

		f.store.holdWith(leaseHang)
		f.pass(leaseInterval)
		require.Len(t, f.store.seen(), 4, "the heartbeat that hangs has begun")
		requireLeaseHeld(t, held, "an interval on, with that heartbeat unanswered")

		f.pass(leaseInterval - time.Nanosecond)
		requireLeaseHeld(t, held, "a nanosecond before it is cut off")

		f.pass(time.Nanosecond)
		requireLeaseEnded(t, held, agent.ErrLeaseLost, "when it is cut off")
		late := f.clock.Now().Sub(mayClaim)
		assert.Equal(t, 2*leaseInterval-time.Nanosecond, late)
		assert.Less(t, late, 2*leaseInterval)
		requireLeaseQuiet(t, f)
	})
}

func TestKeep_PanicsAtTheCallWithoutAStoreOrAClock(t *testing.T) {
	cases := []struct {
		name string
		drop func(opts *agent.KeepOptions)
		want string
	}{
		{"no store", func(opts *agent.KeepOptions) { opts.Store = nil }, "agent: keep: Store is nil"},
		{"no clock", func(opts *agent.KeepOptions) { opts.Clock = nil }, "agent: keep: Clock is nil"},
		{"neither", func(opts *agent.KeepOptions) { opts.Store, opts.Clock = nil, nil }, "agent: keep: Store is nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// In a bubble, so that a goroutine started before the fault
			// showed would fail the test when it was left behind.
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				opts := f.options()
				tc.drop(&opts)

				assert.PanicsWithValue(t, tc.want, func() {
					agent.Keep(context.Background(), f.lease, opts)
				})
			})
		})
	}
}

func TestKeep_ATTLOfZeroOrLessHoldsNothing(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newLeaseFixture(t)
				opts := f.options()
				opts.TTL = ttl
				// No store takes a lease that lasts no time, so there is
				// none to keep, whatever the Interval.
				for _, interval := range []time.Duration{0, leaseInterval} {
					opts.Interval = interval
					held, stop := agent.Keep(context.Background(), f.lease, opts)

					requireLeaseEnded(t, held, agent.ErrLeaseLost, "as soon as it is returned")
					stop()
					stop()
				}
				requireLeaseQuiet(t, f)
				assert.Empty(t, f.store.seen())

				logged := f.logs.all()
				require.Len(t, logged, 2)
				assert.Equal(t, slog.LevelError, logged[0].level)
				assert.Equal(t, leaseRunID, logged[0].attrs["run"])
			})
		})
	}
}

func TestKeep_LogsToTheDefaultLoggerWhenGivenNone(t *testing.T) {
	logs := &leaseLog{}
	// Setting the default logger also points the log package at it, and
	// setting it back does not undo that.
	previous, writer, flags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(logs.logger())
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
	})

	synctest.Test(t, func(t *testing.T) {
		f := newLeaseFixture(t)
		opts := f.options()
		opts.Logger = nil
		f.faults.FailBefore("Heartbeat", 1)
		held, stop := agent.Keep(context.Background(), f.lease, opts)
		defer stop()

		f.wake()

		requireLeaseHeld(t, held, "after one failure")
		require.Len(t, logs.all(), 1)
		assert.Empty(t, f.logs.all())
	})
}
