package testdb_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ManavA/keel/pg/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scripted is a Probe driven by canned sequences, which is the only way to put
// a restart at an exact moment. Every fixture below is one of the situations
// the real oracle has to survive.
type scripted struct {
	accept     []error     // consumed one per call; the last entry repeats
	startTimes []time.Time // likewise
	clockErr   error

	acceptCalls int
	clockCalls  int
}

func (s *scripted) Accepting(context.Context) error {
	err := next(s.accept, s.acceptCalls)
	s.acceptCalls++
	return err
}

func (s *scripted) StartTime(context.Context) (time.Time, error) {
	if s.clockErr != nil {
		s.clockCalls++
		return time.Time{}, s.clockErr
	}
	t := next(s.startTimes, s.clockCalls)
	s.clockCalls++
	return t, nil
}

func next[T any](items []T, i int) T {
	var zero T
	if len(items) == 0 {
		return zero
	}
	if i >= len(items) {
		return items[len(items)-1]
	}
	return items[i]
}

func at(sec int) time.Time { return time.Unix(int64(sec), 0).UTC() }

func TestReadyAcceptsAHealthyDatabase(t *testing.T) {
	// The control: without it, a probe that never returns nil would pass every
	// other case here.
	probe := &scripted{startTimes: []time.Time{at(1000)}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, testdb.Ready(ctx, probe, 0))
	assert.GreaterOrEqual(t, probe.clockCalls, 2, "readiness must compare two reads, not one")
}

func TestReadyWaitsOutARestart(t *testing.T) {
	// The start time moves once — a restart visible on a port that is already
	// accepting connections. The first pair of reads straddles it and must not
	// be accepted; the pair after it must be.
	probe := &scripted{
		startTimes: []time.Time{at(100), at(150), at(200)},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, testdb.Ready(ctx, probe, 0))
	assert.GreaterOrEqual(t, probe.clockCalls, 3,
		"ready was declared on the pair straddling the restart, so the two reads are not being compared")
}

// naiveReady tries the query twice and calls it ready. It exists only to show
// that the restart fixture above discriminates between the two implementations.
func naiveReady(ctx context.Context, probe testdb.Probe) error {
	if err := probe.Accepting(ctx); err != nil {
		return err
	}
	return probe.Accepting(ctx)
}

func TestTheNaiveProbeAcceptsTheRestartFixture(t *testing.T) {
	probe := &scripted{startTimes: []time.Time{at(100), at(150), at(200)}}
	require.NoError(t, naiveReady(context.Background(), probe),
		"if this ever fails the restart fixture stopped discriminating and "+
			"TestReadyWaitsOutARestart is proving nothing")
}

func TestReadyTimesOutWhenNothingAccepts(t *testing.T) {
	probe := &scripted{accept: []error{errors.New("connection refused")}}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := testdb.Ready(ctx, probe, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, testdb.ErrNotAccepting,
		"the error must name the probe that failed, so that a port problem is not read as a restart loop")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestReadyTimesOutWhenTheClockNeverReads(t *testing.T) {
	probe := &scripted{clockErr: errors.New("permission denied")}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := testdb.Ready(ctx, probe, 0)
	require.Error(t, err)
	assert.NotErrorIs(t, err, testdb.ErrNotAccepting)
	assert.Contains(t, err.Error(), "pg_postmaster_start_time")
}

// crashLooping answers every query but reports a new start time each time it is
// asked, which is what a container stuck in a restart loop looks like from
// outside.
type crashLooping struct{ calls int }

func (c *crashLooping) Accepting(context.Context) error { return nil }

func (c *crashLooping) StartTime(context.Context) (time.Time, error) {
	c.calls++
	return at(c.calls), nil
}

func TestReadyTimesOutOnAContinuousRestartLoop(t *testing.T) {
	probe := &crashLooping{}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := testdb.Ready(ctx, probe, 0)
	require.Error(t, err)
	assert.ErrorIs(t, err, testdb.ErrUnstable)
}

func TestReadyRecoversFromAnOutageMidWait(t *testing.T) {
	// The query fails for a while — a full outage, connection refused — and
	// then comes back. Ready must not give up early.
	probe := &scripted{
		accept: []error{
			errors.New("refused"), errors.New("refused"), errors.New("refused"), nil,
		},
		startTimes: []time.Time{at(500)},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, testdb.Ready(ctx, probe, 0))
}

func TestReadyRejectsANilProbe(t *testing.T) {
	assert.Error(t, testdb.Ready(context.Background(), nil, 0))
}

func TestReadyRespectsAnAlreadyCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Error(t, testdb.Ready(ctx, &scripted{startTimes: []time.Time{at(1)}}, 0))
}

func TestReadyReprobesAfterTheGap(t *testing.T) {
	// A restart during the gap can break the query as well as move the start
	// time. Reading the clock alone after the gap would report ready against a
	// database that is no longer accepting connections.
	probe := &scripted{
		accept:     []error{nil, errors.New("gone"), nil},
		startTimes: []time.Time{at(100)},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, testdb.Ready(ctx, probe, 0))
	assert.GreaterOrEqual(t, probe.acceptCalls, 3,
		"ready was declared without re-running the query after the gap")
}
