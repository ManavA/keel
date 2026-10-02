package pg_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// parkFunc is Park, or something that claims to be.
type parkFunc func(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error)

// raceParkAgainstAnswer has an execution park a run at the moment the answer
// to its one question arrives, rounds times over, and returns how many rounds
// left the run stranded: waiting, with nothing pending to wake it.
func raceParkAgainstAnswer(t *testing.T, k *kit, park parkFunc, rounds int, stopAtFirst bool) (stranded int) {
	t.Helper()
	for range rounds {
		run, lease := k.proposed()
		approval := k.ask(lease, 2, nil)
		now := k.tick()

		var (
			wg                 sync.WaitGroup
			parkErr, answerErr error
			start              = make(chan struct{})
		)
		wg.Go(func() {
			<-start
			_, parkErr = park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
		})
		wg.Go(func() {
			<-start
			_, answerErr = k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: approval.ID, Approved: true, By: person, Now: now})
		})
		close(start)
		wg.Wait()

		require.NoError(t, parkErr)
		require.NoError(t, answerErr)
		require.Equal(t, agent.ApprovalApproved, k.approval(approval.ID).Status)
		if k.run(run.ID).Status == agent.StatusWaiting {
			stranded++
			if stopAtFirst {
				return stranded
			}
		}
	}
	return stranded
}

func TestPark_RacingAnAnswerNeverStrandsARun(t *testing.T) {
	k, _ := newKit(t)

	stranded := raceParkAgainstAnswer(t, k, k.store.Park, 300, false)

	assert.Zero(t, stranded, "runs left waiting with no pending approval")
}

// The case that must fail. The same race, against a Park that looks for
// something pending without first locking the run's row, does strand a run:
// the answer lands between the look and the write, finds a run that is not
// yet waiting, and leaves it for a Park that then sets it waiting for good.
// If this stopped stranding runs, the test above would be proving nothing.
func TestPark_WithoutTheRowLockStrandsARun(t *testing.T) {
	k, pool := newKit(t)
	unlocked := func(ctx context.Context, lease agent.Lease, req agent.ParkRequest) (bool, error) {
		return agentpg.ParkWithoutLock(ctx, pool, lease, req)
	}

	stranded := raceParkAgainstAnswer(t, k, unlocked, 300, true)

	assert.Positive(t, stranded, "three hundred rounds never reached the race")
}

// The two orders a Park and an answer can meet in, each made to happen. Both
// lock the run's row first, so the one that arrives second waits, and then
// sees what the first one did.
func TestPark_AndAnAnswerMeetInEitherOrder(t *testing.T) {
	t.Run("the park holds the row first: the answer waits, and wakes the run", func(t *testing.T) {
		db := newDatabase(t)
		pool := db.pool(t)
		k := kitOver(t, agentpg.New(pool))
		run, lease := k.proposed()
		approval := k.ask(lease, 2, nil)

		holding := newGate(t)
		parking := agentpg.New(stepped{Beginner: pool, before: afterLock(holding)})
		parked := make(chan error, 1)
		now := k.tick()
		go func() {
			ok, err := parking.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
			if err == nil && !ok {
				err = assert.AnError
			}
			parked <- err
		}()
		holding.arrived(t)

		answered := make(chan error, 1)
		go func() {
			_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: approval.ID, Approved: true, By: person, Now: now})
			answered <- err
		}()
		waits(t, pool, answered)

		holding.release()
		require.NoError(t, result(t, parked), "the park found the question still pending, and parked")
		require.NoError(t, result(t, answered))
		after := k.run(run.ID)
		assert.Equal(t, agent.StatusRunnable, after.Status, "the answer found the run waiting, and woke it")
		assert.Empty(t, after.LeaseOwner)
	})

	t.Run("the answer holds the row first: the park waits, and finds nothing pending", func(t *testing.T) {
		db := newDatabase(t)
		pool := db.pool(t)
		k := kitOver(t, agentpg.New(pool))
		run, lease := k.proposed()
		approval := k.ask(lease, 2, nil)

		holding := newGate(t)
		answering := agentpg.New(stepped{Beginner: pool, before: afterLock(holding)})
		answered := make(chan error, 1)
		now := k.tick()
		go func() {
			_, err := answering.DecideApproval(k.ctx, agent.DecideRequest{ID: approval.ID, Approved: true, By: person, Now: now})
			answered <- err
		}()
		holding.arrived(t)

		type parkResult struct {
			parked bool
			err    error
		}
		parked := make(chan parkResult, 1)
		go func() {
			ok, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
			parked <- parkResult{ok, err}
		}()
		waits(t, pool, parked)

		holding.release()
		require.NoError(t, result(t, answered))
		got := result(t, parked)
		require.NoError(t, got.err)
		assert.False(t, got.parked, "the answer had landed by the time the park looked")
		after := k.run(run.ID)
		assert.Equal(t, agent.StatusRunnable, after.Status)
		assert.Equal(t, lease.Owner, after.LeaseOwner, "the execution still holds the run and carries on")
	})
}

// A child's Finish takes its parent's row as well as its own, whether or not
// the parent is waiting. Otherwise a parent that parks while its last child
// is ending is never woken: the park sees a child that has not ended, and
// the child sees a parent that is not waiting.
func TestPark_AndAChildEndingMeetInEitherOrder(t *testing.T) {
	type world struct {
		k           *kit
		pool        *pgxpool.Pool
		parent      agent.Run
		parentLease agent.Lease
		childLease  agent.Lease
	}
	setup := func(t *testing.T) world {
		pool := newDatabase(t).pool(t)
		k := kitOver(t, agentpg.New(pool))
		parent, parentLease := k.held()
		child := k.createChild(parent)
		return world{k: k, pool: pool, parent: parent, parentLease: parentLease, childLease: k.claim(workerB, child.ID)}
	}

	t.Run("the child holds its parent's row first: the park waits, and finds the child ended", func(t *testing.T) {
		w := setup(t)
		k := w.k

		// Stopped with all its work done and none of it committed.
		holding := newGate(t)
		ending := agentpg.New(stepped{Beginner: w.pool, before: beforeCommit(holding)})
		ended := make(chan error, 1)
		now := k.tick()
		go func() {
			ended <- ending.Finish(k.ctx, w.childLease, agent.FinishRequest{Status: agent.StatusCompleted, Now: now})
		}()
		holding.arrived(t)

		type parkResult struct {
			parked bool
			err    error
		}
		parked := make(chan parkResult, 1)
		go func() {
			ok, err := k.store.Park(k.ctx, w.parentLease, agent.ParkRequest{Reason: agent.ReasonChildren, Now: now})
			parked <- parkResult{ok, err}
		}()
		waits(t, w.pool, parked)

		holding.release()
		require.NoError(t, result(t, ended))
		got := result(t, parked)
		require.NoError(t, got.err)
		assert.False(t, got.parked, "the child had ended by the time the park looked")
		assert.Equal(t, agent.StatusRunnable, k.run(w.parent.ID).Status)
	})

	t.Run("the park holds the row first: the child waits, and wakes its parent", func(t *testing.T) {
		w := setup(t)
		k := w.k

		holding := newGate(t)
		parking := agentpg.New(stepped{Beginner: w.pool, before: afterLock(holding)})
		parked := make(chan error, 1)
		now := k.tick()
		go func() {
			ok, err := parking.Park(k.ctx, w.parentLease, agent.ParkRequest{Reason: agent.ReasonChildren, Now: now})
			if err == nil && !ok {
				err = assert.AnError
			}
			parked <- err
		}()
		holding.arrived(t)

		ended := make(chan error, 1)
		go func() {
			ended <- k.store.Finish(k.ctx, w.childLease, agent.FinishRequest{Status: agent.StatusCompleted, Now: now})
		}()
		waits(t, w.pool, ended)

		holding.release()
		require.NoError(t, result(t, parked))
		require.NoError(t, result(t, ended))
		after := k.run(w.parent.ID)
		assert.Equal(t, agent.StatusRunnable, after.Status, "the child found its parent waiting, and woke it")
		assert.Empty(t, after.Reason)
	})
}

// The store's transactions are read committed whatever the database's own
// default is. Under repeatable read a Park that waited for its row would
// look for something pending in a snapshot taken before it waited, and park
// on a child that has since ended.
func TestStore_DoesNotDependOnTheDatabasesDefaultIsolation(t *testing.T) {
	db := newDatabase(t)
	pool := db.pool(t, "default_transaction_isolation=repeatable read")
	var level string
	require.NoError(t, pool.QueryRow(t.Context(), "show transaction_isolation").Scan(&level))
	require.Equal(t, "repeatable read", level, "the pool's sessions default to repeatable read")

	k := kitOver(t, agentpg.New(pool))
	parent, parentLease := k.held()
	childLease := k.claim(workerB, k.createChild(parent).ID)

	holding := newGate(t)
	ending := agentpg.New(stepped{Beginner: pool, before: beforeCommit(holding)})
	ended := make(chan error, 1)
	now := k.tick()
	go func() {
		ended <- ending.Finish(k.ctx, childLease, agent.FinishRequest{Status: agent.StatusCompleted, Now: now})
	}()
	holding.arrived(t)

	type parkResult struct {
		parked bool
		err    error
	}
	parked := make(chan parkResult, 1)
	go func() {
		ok, err := k.store.Park(k.ctx, parentLease, agent.ParkRequest{Reason: agent.ReasonChildren, Now: now})
		parked <- parkResult{ok, err}
	}()
	waits(t, pool, parked)

	holding.release()
	require.NoError(t, result(t, ended))
	got := result(t, parked)
	require.NoError(t, got.err)
	assert.False(t, got.parked)
	assert.Equal(t, agent.StatusRunnable, k.run(parent.ID).Status)
}
