package pg_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// claimAtOnce makes n claims from n goroutines released together, each under
// an owner of its own.
func claimAtOnce(k *kit, n int, req agent.ClaimRequest) ([]*agent.Run, []error) {
	var wg sync.WaitGroup
	runs := make([]*agent.Run, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			mine := req
			mine.Owner = workerA + "-" + string(rune('0'+i))
			runs[i], errs[i] = k.store.Claim(k.ctx, mine)
		})
	}
	close(start)
	wg.Wait()
	return runs, errs
}

func TestClaim_EightAtOnceForOneRun(t *testing.T) {
	k, _ := newKit(t)

	for range 25 {
		run := k.create()

		runs, errs := claimAtOnce(k, 8, agent.ClaimRequest{Agents: testAgents, Now: k.tick(), TTL: testTTL})

		var winners []string
		for i, got := range runs {
			require.NoError(t, errs[i])
			if got != nil {
				assert.Equal(t, run.ID, got.ID)
				winners = append(winners, got.LeaseOwner)
			}
		}
		require.Len(t, winners, 1, "one claim takes the run and seven are given nothing")
		after := k.run(run.ID)
		assert.Equal(t, winners[0], after.LeaseOwner)
		assert.Equal(t, int64(1), after.LeaseEpoch, "the run was claimed once")
		assert.Equal(t, run.Rev+1, after.Rev)

		// Out of the way of the next round.
		k.finish(after.Lease(), agent.StatusCompleted)
	}
}

func TestClaim_EightAtOnceForEightRuns(t *testing.T) {
	k, _ := newKit(t)

	for range 25 {
		want := make([]string, 8)
		for i := range want {
			want[i] = k.create().ID
		}

		// One call each and no second try. Eight claims hold at most one run
		// apiece, so a claim that passes over the runs the other seven are
		// taking always finds one left.
		runs, errs := claimAtOnce(k, 8, agent.ClaimRequest{Agents: testAgents, Now: k.tick(), TTL: testTTL})

		var took []string
		for i, got := range runs {
			require.NoError(t, errs[i])
			require.NotNil(t, got, "a claim was given nothing while a run was free")
			assert.Equal(t, int64(1), got.LeaseEpoch)
			took = append(took, got.ID)
			k.finish(got.Lease(), agent.StatusCompleted)
		}
		assert.ElementsMatch(t, want, took, "each claim took a different run")
	}
}

// A claim that names no run passes over a run that is locked: it takes the
// next one and does not wait.
func TestClaim_WithoutRunIDPassesOverALockedRun(t *testing.T) {
	// A claim that waited for the row would wait as long as the test holds
	// it. The limit on a lock wait turns that into an error within seconds;
	// a claim that passes over the row never waits at all.
	pool := newDatabase(t).pool(t, "lock_timeout=5s")
	k := kitOver(t, agentpg.New(pool))
	older := k.create()
	newer := k.create()

	// Another session is in the middle of a write to the older run.
	writer := begin(t, pool)
	_, err := writer.Exec(k.ctx, "select 1 from "+agentpg.RunsTable+" where id = $1 for update", older.ID)
	require.NoError(t, err)

	got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: testAgents, Now: k.tick(), TTL: testTTL})

	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, newer.ID, got.ID, "the older run is being written to, so the claim took the next")

	// With every run that is left locked, there is nothing to take.
	got, err = k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerB, Agents: testAgents, Now: k.tick(), TTL: testTTL})
	require.NoError(t, err)
	assert.Nil(t, got)

	// The write over, the run is there to take.
	require.NoError(t, writer.Commit(k.ctx))
	got, err = k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerB, Agents: testAgents, Now: k.tick(), TTL: testTTL})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, older.ID, got.ID)
}

// A claim that names its run waits for a write in progress and then decides,
// on the run as the write left it.
func TestClaim_WithRunIDWaitsForAWriteInProgress(t *testing.T) {
	tests := []struct {
		name string
		// write is what the other session does to the run before it commits.
		write string
		check func(t *testing.T, got *agent.Run, err error)
	}{
		{
			name:  "a write that leaves the run free: the claim takes it",
			write: "update " + agentpg.RunsTable + " set rev = rev + 1 where id = $1",
			check: func(t *testing.T, got *agent.Run, err error) {
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, workerB, got.LeaseOwner)
				assert.Equal(t, int64(4), got.Rev, "the claim was made on the run the write left")
			},
		},
		{
			name:  "a write that sets the run waiting: the claim is told it cannot be taken",
			write: "update " + agentpg.RunsTable + " set status = 'waiting' where id = $1",
			check: func(t *testing.T, got *agent.Run, err error) {
				require.ErrorIs(t, err, agent.ErrNotClaimable)
				assert.Nil(t, got)
			},
		},
		{
			name:  "a write that removes the run: the claim finds none",
			write: "delete from " + agentpg.RunsTable + " where id = $1",
			check: func(t *testing.T, got *agent.Run, err error) {
				require.ErrorIs(t, err, agent.ErrNotFound)
				assert.Nil(t, got)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, pool := newKit(t)
			run, _ := k.held()
			k.lapse()
			require.Equal(t, int64(2), run.Rev)

			writer := begin(t, pool)
			_, err := writer.Exec(k.ctx, tt.write, run.ID)
			require.NoError(t, err)

			type claimed struct {
				run *agent.Run
				err error
			}
			done := make(chan claimed, 1)
			now := k.tick()
			go func() {
				got, err := k.store.Claim(k.ctx, agent.ClaimRequest{
					Owner: workerB, Agents: testAgents, RunID: run.ID, Now: now, TTL: testTTL,
				})
				done <- claimed{got, err}
			}()

			// The claim is waiting on the row, and has not answered.
			waits(t, pool, done)

			require.NoError(t, writer.Commit(k.ctx))
			got := result(t, done)
			tt.check(t, got.run, got.err)
		})
	}
}

// A journal write under a lease that another claim has since taken is
// refused, and adds nothing.
func TestFence_AWriteUnderALeaseThatWasTakenOver(t *testing.T) {
	k, _ := newKit(t)
	run, first := k.held()
	require.NoError(t, k.store.BeginModel(k.ctx, first, 1, k.tick()))

	k.lapse()
	second := k.claim(workerB, run.ID)
	require.Equal(t, first.Epoch+1, second.Epoch)
	before, journal := k.run(run.ID), k.steps(run.ID)

	writes := []struct {
		name  string
		write func(lease agent.Lease) error
	}{
		{"BeginModel", func(lease agent.Lease) error { return k.store.BeginModel(k.ctx, lease, 1, k.tick()) }},
		{"CompleteModel", func(lease agent.Lease) error {
			return k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: agent.Message{Role: agent.RoleAssistant, Text: "late"}, Stop: agent.StopEnd, Now: k.tick(),
			})
		}},
		{"Finish", func(lease agent.Lease) error {
			return k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: agent.StatusCompleted, Output: "late", Now: k.tick()})
		}},
		{"Yield", func(lease agent.Lease) error {
			return k.store.Yield(k.ctx, lease, agent.YieldRequest{Now: k.tick()})
		}},
		{"Heartbeat", func(lease agent.Lease) error {
			_, err := k.store.Heartbeat(k.ctx, lease, k.tick(), testTTL)
			return err
		}},
	}
	for _, w := range writes {
		require.ErrorIs(t, w.write(first), agent.ErrLeaseLost, w.name)
		assert.Equal(t, before, k.run(run.ID), "%s changed the run", w.name)
		assert.Equal(t, journal, k.steps(run.ID), "%s changed the journal", w.name)
	}

	// The holder's own write goes through.
	require.NoError(t, k.store.BeginModel(k.ctx, second, 1, k.tick()))
	assert.Equal(t, 2, k.steps(run.ID)[0].Attempts)
}

// The fence is the row lock and the comparison together. A write that meets
// a takeover in progress waits for it, and is judged against the hold the
// takeover left: it does not read the old hold, pass, and then write into a
// journal that has changed hands.
func TestFence_AWriteWaitsForATakeoverInProgress(t *testing.T) {
	tests := []struct {
		name string
		// commit says whether the takeover goes through.
		commit bool
	}{
		{"the takeover commits: the write is refused, and adds nothing", true},
		{"the takeover is undone: the lease still holds, and the write lands", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, pool := newKit(t)
			run, lease := k.held()
			k.lapse()

			// Another worker's claim, stopped before it commits.
			ctx, die := context.WithCancel(k.ctx)
			defer die()
			taking := newGate(t)
			claiming := agentpg.New(stepped{Beginner: pool, before: beforeCommit(taking)})
			claimed := make(chan error, 1)
			now := k.tick()
			go func() {
				_, err := claiming.Claim(ctx, agent.ClaimRequest{Owner: workerB, Agents: testAgents, RunID: run.ID, Now: now, TTL: testTTL})
				claimed <- err
			}()
			taking.arrived(t)

			written := make(chan error, 1)
			go func() { written <- k.store.BeginModel(k.ctx, lease, 1, now) }()
			waits(t, pool, written)

			if !tt.commit {
				die()
			}
			taking.release()
			claimErr, writeErr := result(t, claimed), result(t, written)

			if tt.commit {
				require.NoError(t, claimErr)
				require.ErrorIs(t, writeErr, agent.ErrLeaseLost)
				assert.Empty(t, k.steps(run.ID), "the journal the new holder reads has nothing of the old one's write")
				assert.Equal(t, workerB, k.run(run.ID).LeaseOwner)
				return
			}
			require.Error(t, claimErr)
			require.NoError(t, writeErr)
			assert.Len(t, k.steps(run.ID), 1)
			assert.Equal(t, lease, k.run(run.ID).Lease())
		})
	}
}

// Runs created at one instant are claimed in the order of their ids, which
// is the one order a database can give them twice.
func TestClaim_RunsOfOneInstantAreTakenInIDOrder(t *testing.T) {
	k, _ := newKit(t)
	older := k.create()
	instant := k.tick()
	var same []string
	for range 6 {
		run := k.newRun(agentAlpha)
		run.CreatedAt, run.UpdatedAt = instant, instant
		same = append(same, k.insert(run).ID)
	}
	newer := k.create()
	slices.Sort(same)
	want := append(append([]string{older.ID}, same...), newer.ID)

	var took []string
	for range want {
		got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: testAgents, Now: k.tick(), TTL: testTTL})
		require.NoError(t, err)
		require.NotNil(t, got)
		took = append(took, got.ID)
	}

	assert.Equal(t, want, took)
}

// A claim that names no run reads the run in one snapshot and locks it a
// moment later. A write that commits in between leaves the claim holding a
// newer version of the row than the one it found, and the claim must be made
// on that version: with the write's revision counted and the conditions
// judged again. Here one goroutine changes a run as fast as it can while
// another claims it and gives it back, and no change of either is lost.
func TestClaim_OfARunThatIsBeingChanged(t *testing.T) {
	k, pool := newKit(t)
	run := k.create()
	const bumps = 400

	bumped := make(chan error, 1)
	go func() {
		for range bumps {
			if _, err := pool.Exec(k.ctx, "update "+agentpg.RunsTable+" set rev = rev + 1 where id = $1", run.ID); err != nil {
				bumped <- err
				return
			}
		}
		bumped <- nil
	}()

	claims, lastRev := 0, run.Rev
	claimAndRelease := func() {
		got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: testAgents, Now: k.tick(), TTL: testTTL})
		require.NoError(t, err)
		if got == nil {
			// The other goroutine held the row, and the claim passed over it.
			return
		}
		claims++
		require.Equal(t, int64(claims), got.LeaseEpoch, "each claim raises the epoch by one")
		require.Greater(t, got.Rev, lastRev, "a claim is made on the newest version of the run")
		require.NoError(t, k.store.Yield(k.ctx, got.Lease(), agent.YieldRequest{Now: k.tick()}))
		lastRev = got.Rev + 1
	}
	for changing := true; changing; {
		select {
		case err := <-bumped:
			require.NoError(t, err)
			changing = false
		default:
			claimAndRelease()
		}
	}
	claimAndRelease()

	after := k.run(run.ID)
	require.Positive(t, claims)
	assert.Equal(t, int64(claims), after.LeaseEpoch)
	assert.Equal(t, run.Rev+int64(bumps)+2*int64(claims), after.Rev, "every bump, claim and release is counted once")
	assert.Zero(t, after.Failures, "a lease that was given back is not a failure")
	t.Logf("%d claims while %d changes were made", claims, bumps)
}
