package pg_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

type createResult struct {
	run     agent.Run
	created bool
	err     error
}

func TestCreateRun_EightCreatorsOfOneKeyMakeOneRun(t *testing.T) {
	k, _ := newKit(t)

	for round := range 30 {
		key := "start-" + string(rune('a'+round))
		runs := make([]agent.Run, 8)
		for i := range runs {
			runs[i] = k.newRun(agentAlpha)
			runs[i].Key = key
		}

		var wg sync.WaitGroup
		got := make([]createResult, len(runs))
		start := make(chan struct{})
		for i, run := range runs {
			wg.Go(func() {
				<-start
				stored, created, err := k.store.CreateRun(k.ctx, run)
				got[i] = createResult{stored, created, err}
			})
		}
		close(start)
		wg.Wait()

		var made []string
		for i, r := range got {
			require.NoError(t, r.err, "creator %d", i)
			if r.created {
				assert.Equal(t, runs[i].ID, r.run.ID, "the creator that made the run was handed its own")
				made = append(made, r.run.ID)
			}
		}
		require.Len(t, made, 1, "one creator makes the run")
		for i, r := range got {
			assert.Equal(t, made[0], r.run.ID, "creator %d was handed the one run", i)
		}
		listed, err := k.store.ListRuns(k.ctx, agent.RunFilter{Limit: 200})
		require.NoError(t, err)
		assert.Len(t, listed, round+1, "and one run was stored")
	}
}

// The two ends a create can meet a create of the same key at, each made to
// happen: the second waits for the first to commit or be undone, and then
// answers for the run that is there.
func TestCreateRun_TheSecondCreatorOfAKeyWaitsForTheFirst(t *testing.T) {
	tests := []struct {
		name string
		// commit says how the first creator's transaction ends.
		commit bool
	}{
		{"the first commits: the second is handed the first's run", true},
		{"the first is undone: the second makes the run", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newDatabase(t).pool(t)
			k := kitOver(t, agentpg.New(pool))
			first, second := k.newRun(agentAlpha), k.newRun(agentAlpha)
			first.Key, second.Key = "start-1", "start-1"
			first.Input, second.Input = "first", "second"

			// The first creator, stopped with its run written and not
			// committed. A context it can be undone through stands in for a
			// process that dies there.
			ctx, die := context.WithCancel(k.ctx)
			defer die()
			written := newGate(t)
			creating := agentpg.New(stepped{Beginner: pool, before: beforeCommit(written)})
			firstDone := make(chan createResult, 1)
			go func() {
				stored, created, err := creating.CreateRun(ctx, first)
				firstDone <- createResult{stored, created, err}
			}()
			written.arrived(t)

			secondDone := make(chan createResult, 1)
			go func() {
				stored, created, err := k.store.CreateRun(k.ctx, second)
				secondDone <- createResult{stored, created, err}
			}()
			waits(t, pool, secondDone)

			if !tt.commit {
				die()
			}
			written.release()
			gotFirst, gotSecond := result(t, firstDone), result(t, secondDone)
			require.NoError(t, gotSecond.err)

			if tt.commit {
				require.NoError(t, gotFirst.err)
				assert.True(t, gotFirst.created)
				assert.False(t, gotSecond.created)
				assert.Equal(t, first.ID, gotSecond.run.ID)
				assert.Equal(t, "first", gotSecond.run.Input)
				_, err := k.store.GetRun(k.ctx, second.ID)
				assert.ErrorIs(t, err, agent.ErrNotFound, "the second run was not stored")
				return
			}
			require.Error(t, gotFirst.err)
			assert.True(t, gotSecond.created)
			assert.Equal(t, second.ID, gotSecond.run.ID)
			_, err := k.store.GetRun(k.ctx, first.ID)
			assert.ErrorIs(t, err, agent.ErrNotFound, "the first run was undone")
		})
	}
}

// Changes is one view of the run. A write that commits while it is being
// read is in the answer whole or not at all, and when it is not, the run
// comes back at the revision before it, so the next call returns it.
func TestChanges_AWriteCommittedInTheMiddleOfARead(t *testing.T) {
	pool := newDatabase(t).pool(t)
	k := kitOver(t, agentpg.New(pool))
	run, lease := k.held()
	k.reply(lease, agenttest.Call("call-1", toolSend, sendInput))
	before := k.run(run.ID)

	// The reader, stopped after its first statement.
	reading := newGate(t)
	reader := agentpg.New(stepped{Beginner: pool, before: func(st statement) {
		if st.n == 2 {
			reading.wait()
		}
	}})
	type answer struct {
		changes agent.Changes
		err     error
	}
	read := make(chan answer, 1)
	go func() {
		changes, err := reader.Changes(k.ctx, run.ID, 0)
		read <- answer{changes, err}
	}()
	reading.arrived(t)

	// Three writes land: a step moved, a reply begun, and its two calls.
	require.NoError(t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
		Seq: 2, From: agent.StepProposed, To: agent.StepCompleted, Now: k.tick(),
	}))
	k.reply(lease, agenttest.Call("call-2", toolSend, sendInput), agenttest.Call("call-3", toolSend, sendInput))
	k.ask(lease, 4, nil)
	after := k.run(run.ID)
	require.Greater(t, after.Rev, before.Rev)

	reading.release()
	got := result(t, read)
	require.NoError(t, got.err)

	// The answer is the run as it stood at one moment: before the writes or
	// after them, and never a mixture.
	for _, step := range got.changes.Steps {
		assert.LessOrEqual(t, step.Rev, got.changes.Run.Rev, "step %d is ahead of the run it came with", step.Seq)
	}
	for _, approval := range got.changes.Approvals {
		assert.LessOrEqual(t, approval.Rev, got.changes.Run.Rev, "an approval is ahead of the run it came with")
	}
	have := map[int]agent.Step{}
	for _, step := range got.changes.Steps {
		have[step.Seq] = step
	}

	// The reader carries on from the revision it was given, and has the
	// journal.
	next, err := k.store.Changes(k.ctx, run.ID, got.changes.Run.Rev)
	require.NoError(t, err)
	for _, step := range next.Steps {
		have[step.Seq] = step
	}
	journal := k.steps(run.ID)
	require.Len(t, journal, 5)
	require.Len(t, have, len(journal), "a step was lost to the reader for good")
	for _, step := range journal {
		assert.Equal(t, step, have[step.Seq], "step %d", step.Seq)
	}
	approvals := slices.Concat(got.changes.Approvals, next.Approvals)
	assert.Equal(t, k.approvals(run.ID), approvals)
	assert.Equal(t, after.Rev, next.Run.Rev)
}

// The same, left to chance: one goroutine journals a run while another reads
// its changes as the event stream does. No answer mixes two moments, and
// what the reader pieces together is the journal.
func TestChanges_ReadWhileTheRunIsWritten(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.held()
	const turns = 40

	written := make(chan error, 1)
	go func() {
		written <- func() error {
			for turn := range turns {
				seq := turn*3 + 1
				if err := k.store.BeginModel(k.ctx, lease, seq, k.tick()); err != nil {
					return err
				}
				if err := k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
					Seq:     seq,
					Message: agenttest.Use(agenttest.Call("call-1", toolSend, sendInput), agenttest.Call("call-2", toolSend, sendInput)).Message,
					Stop:    agent.StopToolUse,
					Now:     k.tick(),
				}); err != nil {
					return err
				}
				req := k.askRequest(seq + 1)
				if _, err := k.store.RequestApproval(k.ctx, lease, req); err != nil {
					return err
				}
				if _, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: req.ID, Approved: true, By: person, Now: k.tick()}); err != nil {
					return err
				}
				for _, move := range [][2]agent.StepStatus{
					{agent.StepProposed, agent.StepStarted},
					{agent.StepStarted, agent.StepCompleted},
				} {
					if err := k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
						Seq: seq + 2, From: move[0], To: move[1], Now: k.tick(),
					}); err != nil {
						return err
					}
				}
			}
			return nil
		}()
	}()

	var since int64
	reads := 0
	steps := map[int]agent.Step{}
	approvals := map[string]agent.Approval{}
	read := func() {
		got, err := k.store.Changes(k.ctx, run.ID, since)
		require.NoError(t, err)
		reads++
		require.GreaterOrEqual(t, got.Run.Rev, since, "a run's revision never goes back")
		for _, step := range got.Steps {
			require.LessOrEqual(t, step.Rev, got.Run.Rev, "step %d is ahead of the run it came with", step.Seq)
			require.Greater(t, step.Rev, since, "step %d was not asked for", step.Seq)
			steps[step.Seq] = step
		}
		for _, approval := range got.Approvals {
			require.LessOrEqual(t, approval.Rev, got.Run.Rev, "an approval is ahead of the run it came with")
			approvals[approval.ID] = approval
		}
		since = got.Run.Rev
	}
	for writing := true; writing; {
		select {
		case err := <-written:
			require.NoError(t, err)
			writing = false
		default:
			read()
		}
	}
	read()

	journal := k.steps(run.ID)
	require.Len(t, journal, turns*3)
	require.Len(t, steps, len(journal))
	for _, step := range journal {
		assert.Equal(t, step, steps[step.Seq], "step %d", step.Seq)
	}
	asked := k.approvals(run.ID)
	require.Len(t, asked, turns)
	for _, approval := range asked {
		assert.Equal(t, approval, approvals[approval.ID])
	}
	assert.Equal(t, k.run(run.ID).Rev, since)
	t.Logf("%d reads while %d writes were made", reads, turns*6)
}

func TestDecideApproval_EightAtOnceRaiseRevOnce(t *testing.T) {
	k, _ := newKit(t)

	for range 25 {
		run, pending := k.parked(nil)
		now := k.tick()

		var wg sync.WaitGroup
		got := make([]agent.Approval, 8)
		errs := make([]error, 8)
		start := make(chan struct{})
		for i := range 8 {
			wg.Go(func() {
				<-start
				got[i], errs[i] = k.store.DecideApproval(k.ctx, agent.DecideRequest{
					ID: pending.ID, Approved: i%2 == 0, By: workerA + "-" + string(rune('0'+i)), Reason: "mine", Now: now,
				})
			})
		}
		close(start)
		wg.Wait()

		recorded := k.approval(pending.ID)
		won := 0
		for i := range 8 {
			if errs[i] == nil {
				won++
				assert.Equal(t, recorded.DecidedBy, workerA+"-"+string(rune('0'+i)))
			} else {
				require.ErrorIs(t, errs[i], agent.ErrAlreadyDecided)
			}
			assert.Equal(t, recorded, got[i], "every caller is handed the one answer")
		}
		assert.Equal(t, 1, won, "an approval is decided once")
		after := k.run(run.ID)
		assert.Equal(t, run.Rev+1, after.Rev, "eight answers change the run once")
		assert.Equal(t, after.Rev, recorded.Rev)
		assert.Equal(t, agent.StatusRunnable, after.Status)
	}
}

// deadlockSettings make a deadlock say so at once, and a wait that never
// ends fail the test rather than hang it.
var deadlockSettings = []string{"deadlock_timeout=50ms", "statement_timeout=60s"}

// family is a parent run that waits on its child and on a person, and the
// child, held by a worker, with a question of its own. Both questions lapse
// at due.
type family struct {
	parent, child agent.Run
	childLease    agent.Lease
	due           time.Time
}

func (k *kit) family() family {
	k.t.Helper()
	due := k.clock.Now().Add(time.Hour)
	parent, parentLease := k.proposed()
	k.ask(parentLease, 2, &due)
	child := k.createChild(parent)
	childLease := k.claim(workerB, child.ID)
	k.reply(childLease, agenttest.Call("call-1", toolSend, sendInput))
	k.ask(childLease, 2, &due)
	k.park(parentLease, agent.ReasonApproval)
	return family{parent: k.run(parent.ID), child: k.run(child.ID), childLease: childLease, due: due}
}

// ExpireApprovals takes the rows of every run it will change before it
// changes any, children before parents, which is the order a child's Finish
// takes them in. Here the child holds its own row and wants its parent's,
// while the lapse wants both: taken in the other order, each would hold what
// the other waits for.
func TestExpireApprovals_AndAChildEndingTakeRowsInOneOrder(t *testing.T) {
	pool := newDatabase(t).pool(t, deadlockSettings...)
	k := kitOver(t, agentpg.New(pool))
	f := k.family()

	// The child's Finish, stopped holding the child's row.
	holding := newGate(t)
	ending := agentpg.New(stepped{Beginner: pool, before: afterLock(holding)})
	ended := make(chan error, 1)
	now := k.tick()
	go func() {
		ended <- ending.Finish(k.ctx, f.childLease, agent.FinishRequest{Status: agent.StatusCompleted, Now: now})
	}()
	holding.arrived(t)

	type lapse struct {
		n   int
		err error
	}
	lapsed := make(chan lapse, 1)
	go func() {
		n, err := k.store.ExpireApprovals(k.ctx, f.due)
		lapsed <- lapse{n, err}
	}()
	waits(t, pool, lapsed)

	holding.release()
	require.NoError(t, result(t, ended))
	got := result(t, lapsed)
	require.NoError(t, got.err, "the lapse and the child's end each held a row the other wanted")

	// The child ended first, so its question was cancelled with it and only
	// the parent's lapsed.
	assert.Equal(t, 1, got.n)
	assert.Equal(t, agent.ApprovalCancelled, k.approvals(f.child.ID)[0].Status)
	assert.Equal(t, agent.ApprovalExpired, k.approvals(f.parent.ID)[0].Status)
	parent := k.run(f.parent.ID)
	assert.Equal(t, agent.StatusRunnable, parent.Status)
	assert.Equal(t, f.parent.Rev+2, parent.Rev, "woken by its child, and changed again by the lapse")
}

// A lapse does not pass over a run that is being written to, and does not
// write around it either: it waits for the run's row before it touches the
// run's approvals. So the approval it lapses carries the Rev the lapse gives
// the run, whatever was written while it waited. An approval stamped with a
// Rev the run had already been given by somebody else would be behind a
// reader who had seen that Rev, and never reach it.
func TestExpireApprovals_WaitsForARunBeingWritten(t *testing.T) {
	pool := newDatabase(t).pool(t)
	k := kitOver(t, agentpg.New(pool))
	due := k.clock.Now().Add(time.Hour)
	run, pending := k.parked(&due)

	// Another session is in the middle of a write to the run.
	writer := begin(t, pool)
	_, err := writer.Exec(k.ctx, "update "+agentpg.RunsTable+" set rev = rev + 1 where id = $1", run.ID)
	require.NoError(t, err)

	type lapse struct {
		n   int
		err error
	}
	lapsed := make(chan lapse, 1)
	go func() {
		n, err := k.store.ExpireApprovals(k.ctx, due)
		lapsed <- lapse{n, err}
	}()
	waits(t, pool, lapsed)
	require.NoError(t, writer.Commit(k.ctx))

	got := result(t, lapsed)
	require.NoError(t, got.err)
	assert.Equal(t, 1, got.n)
	after := k.run(run.ID)
	assert.Equal(t, agent.StatusRunnable, after.Status)
	assert.Equal(t, run.Rev+2, after.Rev, "the write, and then the lapse")
	assert.Equal(t, after.Rev, k.approval(pending.ID).Rev, "the approval carries the Rev the lapse gave its run")
}

// The same contention left to chance, many times over: a child ending while
// two workers' ticks lapse approvals of the child and of its parent. A
// deadlock would be an error from one of the three within the round.
func TestExpireApprovals_RacingAChildEndingAndAnotherLapse(t *testing.T) {
	pool := newDatabase(t).pool(t, deadlockSettings...)
	k := kitOver(t, agentpg.New(pool))

	for range 100 {
		// Three families, so that each lapse has several runs to lock.
		families := []family{k.family(), k.family(), k.family()}
		due := families[2].due
		now := k.tick()

		var wg sync.WaitGroup
		start := make(chan struct{})
		finishErrs := make([]error, len(families))
		for i, f := range families {
			wg.Go(func() {
				<-start
				finishErrs[i] = k.store.Finish(k.ctx, f.childLease, agent.FinishRequest{Status: agent.StatusCompleted, Now: now})
			})
		}
		lapsed := make([]int, 2)
		lapseErrs := make([]error, 2)
		for i := range lapsed {
			wg.Go(func() {
				<-start
				lapsed[i], lapseErrs[i] = k.store.ExpireApprovals(k.ctx, due)
			})
		}
		close(start)
		wg.Wait()

		for _, err := range finishErrs {
			require.NoError(t, err)
		}
		for _, err := range lapseErrs {
			require.NoError(t, err)
		}

		// Each approval lapsed once or was cancelled once, and the two
		// lapses between them count the ones that lapsed.
		expired := 0
		for _, f := range families {
			parent := k.run(f.parent.ID)
			assert.Equal(t, agent.StatusRunnable, parent.Status)
			assert.Equal(t, agent.StatusCompleted, k.run(f.child.ID).Status)
			assert.Equal(t, agent.ApprovalExpired, k.approvals(f.parent.ID)[0].Status)
			expired++
			if status := k.approvals(f.child.ID)[0].Status; status == agent.ApprovalExpired {
				expired++
			} else {
				assert.Equal(t, agent.ApprovalCancelled, status)
			}
		}
		assert.Equal(t, expired, lapsed[0]+lapsed[1])

		// Ended, so that the next round's lapses do not find them.
		for _, f := range families {
			k.finish(k.claim(workerA, f.parent.ID), agent.StatusCompleted)
		}
	}
}
