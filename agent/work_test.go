package agent_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// The worker is tested in a synctest bubble, with the executor's fixture.
// Work's poll timer, the drain and the keeper all run on the bubble's clock,
// so "promptly" is no time at all and "after DrainTimeout" is exactly that,
// on any machine.

const workDrain = 4 * time.Second

// workOutcomes is five agents, one for each way an execution ends that a
// Report counts.
func workOutcomes(calls *execCalls) ([]agent.Definition, agenttest.Script) {
	send := calls.tool("send", nil)
	send.Approval = true
	defs := []agent.Definition{
		{Name: "finisher"},
		{Name: "refuser"},
		{Name: "asker", Tools: []agent.Tool{send}},
		{Name: "stumbler"},
	}
	script := agenttest.ByAgent(map[string]agenttest.Script{
		"finisher": agenttest.Replies(agenttest.Say("done")),
		"refuser": agenttest.Replies(agent.Response{
			Message: agent.Message{Role: agent.RoleAssistant}, Stop: agent.StopRefusal,
		}),
		"asker": agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "send", `{}`)), agenttest.Say("sent")),
		"stumbler": func(agent.Request, int) (agent.Response, error) {
			return agent.Response{}, errors.New("rate limited")
		},
	})
	return defs, script
}

// startApart starts a run of each named agent a second apart, so that the
// order they are claimed in is the order they were started in on any store.
func (f *execFixture) startApart(names ...string) []agent.Run {
	f.t.Helper()
	runs := make([]agent.Run, len(names))
	for i, name := range names {
		runs[i] = f.start(name, fmt.Sprintf("run %d", i+1))
		f.clock.Advance(time.Second)
	}
	return runs
}

func TestTick_ReportsHowEachExecutionEnded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		defs, script := workOutcomes(calls)
		f := newExecFixture(t, execConfig{
			defs: defs, script: script,
			tune: func(o *agent.Options) { o.Concurrency = 6 },
		})
		runs := f.startApart("finisher", "refuser", "asker", "stumbler", "finisher", "finisher")
		require.NoError(t, f.engine.Cancel(t.Context(), runs[4].ID, "ops@example.test", "not wanted"))

		report, err := f.engine.Tick(t.Context())

		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 6, Completed: 2, Failed: 1, Cancelled: 1, Parked: 1, Yielded: 1}, report)
		for i, want := range []agent.Status{
			agent.StatusCompleted, agent.StatusFailed, agent.StatusWaiting,
			agent.StatusRunnable, agent.StatusCancelled, agent.StatusCompleted,
		} {
			got := f.run(runs[i].ID)
			assert.Equal(t, want, got.Status, "run %d", i+1)
			assert.Empty(t, got.LeaseOwner, "run %d is held by nobody once the pass returns", i+1)
		}
		assert.Equal(t, 1, f.run(runs[3].ID).Failures)

		// A second pass finds only what the first left runnable and due.
		report, err = f.engine.Tick(t.Context())
		require.NoError(t, err)
		assert.Equal(t, agent.Report{}, report, "the run that failed waits out its back-off")
	})
}

func TestTick_ClaimsNoMoreThanConcurrency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{{Name: "finisher"}},
			script: agenttest.Replies(agenttest.Say("done")),
			tune:   func(o *agent.Options) { o.Concurrency = 2 },
			over:   hooked(&store),
		})
		runs := f.startApart("finisher", "finisher", "finisher", "finisher", "finisher")

		report, err := f.engine.Tick(t.Context())

		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 2, Completed: 2}, report)
		claims, _, _ := store.count()
		assert.Equal(t, 2, claims, "a pass asks for a run no more often than it has slots")
		for i, run := range runs {
			got := f.run(run.ID)
			if i < 2 {
				assert.Equal(t, agent.StatusCompleted, got.Status, "run %d, one of the two oldest", i+1)
				continue
			}
			assert.Equal(t, agent.StatusRunnable, got.Status, "run %d", i+1)
			assert.Zero(t, got.LeaseEpoch, "run %d was not claimed", i+1)
		}

		report, err = f.engine.Tick(t.Context())
		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 2, Completed: 2}, report)
		report, err = f.engine.Tick(t.Context())
		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 1, Completed: 1}, report, "a pass with fewer runs than slots stops asking")
		report, err = f.engine.Tick(t.Context())
		require.NoError(t, err)
		assert.Equal(t, agent.Report{}, report)
	})
}

func TestTick_ExecutesItsRunsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		release := make(chan struct{})
		var mu sync.Mutex
		inFlight, most := 0, 0
		slow := calls.tool("slow", func(context.Context, agent.Invocation) (string, error) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return "done", nil
		})
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(slow)},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "slow", `{}`)),
				agenttest.Say("done"),
			),
			tune: func(o *agent.Options) { o.Concurrency = 3 },
		})
		f.startApart("clerk", "clerk", "clerk", "clerk")

		type ticked struct {
			report agent.Report
			err    error
		}
		done := make(chan ticked, 1)
		go func() {
			report, err := f.engine.Tick(t.Context())
			done <- ticked{report, err}
		}()
		synctest.Wait()

		mu.Lock()
		assert.Equal(t, 3, inFlight, "every claimed run is in flight together")
		mu.Unlock()
		select {
		case <-done:
			require.Fail(t, "Tick returned with its runs still executing")
		default:
		}
		close(release)
		got := <-done
		require.NoError(t, got.err)
		assert.Equal(t, agent.Report{Claimed: 3, Completed: 3}, got.report)
		assert.Equal(t, 3, most)
	})
}

func TestTick_LapsesAnApprovalPastApprovalTTLWhichDeclinesTheCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		send := calls.tool("send", nil)
		send.Approval = true
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(send)},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "send", `{"to":"all"}`)),
				agenttest.Say("nobody answered, so nothing was sent"),
			),
			tune: func(o *agent.Options) { o.ApprovalTTL = time.Hour },
		})
		started := f.start("clerk", "send the digest")
		report, err := f.engine.Tick(t.Context())
		require.NoError(t, err)
		require.Equal(t, agent.Report{Claimed: 1, Parked: 1}, report)
		approvals := f.approvals(started.ID)
		require.Len(t, approvals, 1)
		require.NotNil(t, approvals[0].ExpiresAt)
		assert.Equal(t, execStart.Add(time.Hour), *approvals[0].ExpiresAt, "an approval lapses ApprovalTTL after it is asked")

		// Until the hour is up a pass finds nothing to do.
		f.clock.Advance(time.Hour - time.Nanosecond)
		report, err = f.engine.Tick(t.Context())
		require.NoError(t, err)
		require.Equal(t, agent.Report{}, report)
		require.Equal(t, agent.StatusWaiting, f.run(started.ID).Status)

		f.clock.Advance(time.Nanosecond)
		report, err = f.engine.Tick(t.Context())

		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 1, Completed: 1}, report, "the pass that lapses the approval executes its run")
		got := f.run(started.ID)
		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "nobody answered, so nothing was sent", got.Output)
		approvals = f.approvals(started.ID)
		assert.Equal(t, agent.ApprovalExpired, approvals[0].Status)
		step := f.steps(started.ID)[1]
		assert.Equal(t, agent.StepDeclined, step.Status)
		assert.Equal(t, "declined: approval expired", step.Result)
		assert.True(t, step.IsError)
		assert.Empty(t, calls.of("send"))
	})
}

func TestTick_ClaimsOnlyRunsOfAgentsRegisteredHere(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{{Name: "finisher"}},
			script: agenttest.Replies(agenttest.Say("done")),
		})
		stranger := f.rival(execConfig{defs: []agent.Definition{{Name: "other"}}})
		bare := f.rival(execConfig{})
		started := f.start("finisher", "hello")

		for name, engine := range map[string]*execFixture{"another agent's": stranger, "no agent's": bare} {
			report, err := engine.engine.Tick(t.Context())
			require.NoError(t, err, name)
			assert.Equal(t, agent.Report{}, report, name)
		}
		assert.Zero(t, f.run(started.ID).LeaseEpoch)

		report, err := f.engine.Tick(t.Context())
		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 1, Completed: 1}, report)
	})
}

func TestTick_AStoreThatFails(t *testing.T) {
	t.Run("lapsing approvals: nothing is claimed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{{Name: "finisher"}},
				script: agenttest.Replies(agenttest.Say("done")),
			})
			started := f.start("finisher", "hello")
			f.faults.FailBefore("ExpireApprovals", 1)

			report, err := f.engine.Tick(t.Context())

			require.ErrorIs(t, err, agenttest.ErrFault)
			require.ErrorContains(t, err, "agent: tick: lapse overdue approvals")
			assert.Equal(t, agent.Report{}, report)
			assert.Zero(t, f.run(started.ID).LeaseEpoch)
		})
	})

	t.Run("claiming: what was already claimed is executed, and the error returned", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var store *execHooked
			f := newExecFixture(t, execConfig{
				defs:   []agent.Definition{{Name: "finisher"}},
				script: agenttest.Replies(agenttest.Say("done")),
				over:   hooked(&store),
			})
			store.claim = func(n int) error {
				if n == 2 {
					return errors.New("connection reset")
				}
				return nil
			}
			runs := f.startApart("finisher", "finisher", "finisher")

			report, err := f.engine.Tick(t.Context())

			require.ErrorContains(t, err, "agent: tick: claim a run: connection reset")
			assert.Equal(t, agent.Report{Claimed: 1, Completed: 1}, report)
			assert.Equal(t, agent.StatusCompleted, f.run(runs[0].ID).Status)
			assert.Zero(t, f.run(runs[1].ID).LeaseEpoch)
			claims, _, _ := store.count()
			assert.Equal(t, 2, claims, "a pass stops asking once the store has failed")
		})
	})
}

// working starts Work on a goroutine of its own and returns the function
// that stops it and the channel its error arrives on.
func (f *execFixture) working() (stop context.CancelFunc, done <-chan error) {
	ctx, cancel := context.WithCancel(f.t.Context())
	out := make(chan error, 1)
	go func() { out <- f.engine.Work(ctx) }()
	return cancel, out
}

func TestWork_PicksUpARunStartedAfterItBegan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk()},
			script: agenttest.Replies(agenttest.Say("the total is 42")),
			tune:   func(o *agent.Options) { o.PollInterval = 3 * time.Second },
		})
		stop, done := f.working()
		// More polls that find nothing than the worker has slots.
		time.Sleep(6 * 3 * time.Second)
		synctest.Wait()

		started := f.start("clerk", "what is the total?")
		time.Sleep(3*time.Second - time.Nanosecond)
		synctest.Wait()
		require.Equal(t, agent.StatusRunnable, f.run(started.ID).Status, "Work looks again after PollInterval, not before")
		time.Sleep(time.Nanosecond)
		synctest.Wait()

		got := f.run(started.ID)
		assert.Equal(t, agent.StatusCompleted, got.Status)
		assert.Equal(t, "the total is 42", got.Output)

		// A second run is picked up the same way, by the same worker.
		again := f.start("clerk", "and again?")
		time.Sleep(3 * time.Second)
		synctest.Wait()
		assert.Equal(t, agent.StatusCompleted, f.run(again.ID).Status)

		stop()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWork_CancelledWhileIdleReturnsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{defs: []agent.Definition{execClerk()}})
		stop, done := f.working()
		synctest.Wait()
		select {
		case err := <-done:
			require.Fail(t, "Work returned before its context was cancelled", "%v", err)
		default:
		}
		began := time.Now()

		stop()
		err := <-done

		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, time.Since(began), "an idle worker waits for nothing")
		assert.Empty(t, f.logs.at(slog.LevelWarn), "and has nothing to warn of")
		assert.Empty(t, f.logs.at(slog.LevelError))
	})
}

func TestWork_AContextThatHasEndedClaimsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk()},
			script: agenttest.Replies(agenttest.Say("done")),
		})
		started := f.start("clerk", "hello")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := f.engine.Work(ctx)

		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, f.run(started.ID).LeaseEpoch)
	})
}

func TestWork_KeepsNoMoreThanConcurrencyExecutionsGoing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		release := make(chan struct{})
		var mu sync.Mutex
		inFlight, most := 0, 0
		slow := calls.tool("slow", func(context.Context, agent.Invocation) (string, error) {
			mu.Lock()
			inFlight++
			most = max(most, inFlight)
			mu.Unlock()
			<-release
			mu.Lock()
			inFlight--
			mu.Unlock()
			return "done", nil
		})
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(slow)},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "slow", `{}`)),
				agenttest.Say("done"),
			),
			tune: func(o *agent.Options) { o.Concurrency = 2 },
		})
		runs := f.startApart("clerk", "clerk", "clerk", "clerk", "clerk")
		stop, done := f.working()
		synctest.Wait()

		mu.Lock()
		assert.Equal(t, 2, inFlight)
		mu.Unlock()
		claimed := 0
		for _, run := range runs {
			if f.run(run.ID).LeaseEpoch > 0 {
				claimed++
			}
		}
		assert.Equal(t, 2, claimed, "nothing is claimed that there is no slot for")

		// Each call let go frees a slot, and the slot is filled at once.
		for range runs {
			release <- struct{}{}
			synctest.Wait()
		}
		for i, run := range runs {
			assert.Equal(t, agent.StatusCompleted, f.run(run.ID).Status, "run %d", i+1)
		}
		assert.Equal(t, 2, most)

		stop()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWork_LapsesOverdueApprovals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		send := calls.tool("send", nil)
		send.Approval = true
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(send)},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "send", `{}`)),
				agenttest.Say("nothing was sent"),
			),
			tune: func(o *agent.Options) { o.ApprovalTTL = time.Minute },
		})
		started := f.start("clerk", "send the digest")
		stop, done := f.working()
		synctest.Wait()
		require.Equal(t, agent.StatusWaiting, f.run(started.ID).Status)

		f.pass(time.Minute)
		time.Sleep(time.Second)
		synctest.Wait()

		got := f.run(started.ID)
		assert.Equal(t, agent.StatusCompleted, got.Status, "a worker lapses approvals as a pass does")
		assert.Equal(t, "declined: approval expired", f.steps(started.ID)[1].Result)

		stop()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWork_OnCancelAStepInFlightFinishesAndIsRecordedAndTheRunIsYielded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		began, release := make(chan struct{}), make(chan struct{})
		ended := make(chan error, 1)
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("slow", func(ctx context.Context, _ agent.Invocation) (string, error) {
				close(began)
				<-release
				ended <- ctx.Err()
				return "done in time", nil
			}))},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "slow", `{}`), agenttest.Call("call-2", "slow", `{}`)),
				agenttest.Say("never said by this worker"),
			),
			// One slot, so that the worker is waiting for it when it is stopped.
			tune: func(o *agent.Options) {
				o.DrainTimeout = workDrain
				o.Concurrency = 1
			},
		})
		started := f.start("clerk", "take your time")
		stop, done := f.working()
		<-began

		stop()
		synctest.Wait()
		select {
		case err := <-done:
			require.Fail(t, "Work returned with a step in flight", "%v", err)
		default:
		}
		require.Equal(t, []string{"1 model completed", "2 slow started", "3 slow proposed"}, f.journal(started.ID))
		// The step is let finish inside the drain.
		time.Sleep(workDrain - time.Nanosecond)
		close(release)
		err := <-done

		require.ErrorIs(t, err, context.Canceled)
		require.NoError(t, <-ended, "the step in flight is not cancelled with the worker")
		got := f.run(started.ID)
		assert.Equal(t, agent.StatusRunnable, got.Status)
		assert.Empty(t, got.LeaseOwner, "the run is given back at once")
		assert.Nil(t, got.LeaseExpiresAt)
		assert.Zero(t, got.Failures, "a shutdown is not the run's failure")
		assert.Empty(t, got.Error)
		assert.Nil(t, got.NextAttemptAt)
		assert.Equal(t, []string{"1 model completed", "2 slow completed", "3 slow proposed"}, f.journal(started.ID),
			"the step is recorded, and no other is begun")
		assert.Equal(t, "done in time", f.steps(started.ID)[1].Result)
		assert.Len(t, calls.of("slow"), 1)

		// Another worker carries on from the journal, with no wait.
		second := f.rival(execConfig{defs: []agent.Definition{execClerk(calls.tool("slow", nil))}})
		assert.Equal(t, agent.StatusCompleted, second.execute(started.ID).Status)
	})
}

func TestWork_AStepThatOutlivesDrainTimeoutIsLeftAndNothingIsWrittenForIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		began, release := make(chan struct{}), make(chan struct{})
		stopped := make(chan error, 1)
		slow := func(ctx context.Context, in agent.Invocation) (string, error) {
			if in.Attempt > 1 {
				return "done by the second worker", nil
			}
			close(began)
			<-ctx.Done()
			stopped <- context.Cause(ctx)
			// The tool does not stop when told to.
			<-release
			return "too late", nil
		}
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk(calls.tool("slow", slow))},
			script: agenttest.Replies(
				agenttest.Use(agenttest.Call("call-1", "slow", `{}`)),
				agenttest.Say("done"),
			),
			tune: func(o *agent.Options) { o.DrainTimeout = workDrain },
		})
		started := f.start("clerk", "take your time")
		stop, done := f.working()
		<-began
		before := f.run(started.ID)

		stop()
		at := time.Now()
		err := <-done

		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, workDrain, time.Since(at), "Work waits DrainTimeout for the step, and no longer")
		assert.Same(t, agent.ErrDrained, <-stopped, "the step is cancelled, and told why")
		got := f.run(started.ID)
		assert.Equal(t, before, got, "nothing is written: no result, no failure, and the run is not given back")
		assert.Equal(t, "worker-1", got.LeaseOwner, "the lease is left to lapse")
		assert.Equal(t, []string{"1 model completed", "2 slow started"}, f.journal(started.ID))

		// No keeper is left extending the lease: it lapses, and another worker
		// takes the run and makes the call again.
		second := f.rival(execConfig{defs: []agent.Definition{execClerk(calls.tool("slow", slow))}})
		_, err = second.engine.Execute(t.Context(), started.ID)
		require.ErrorIs(t, err, agent.ErrNotClaimable, "while the tool may be running the run cannot be taken")
		f.pass(execTTL)
		ended := second.execute(started.ID)
		assert.Equal(t, agent.StatusCompleted, ended.Status)
		ran := calls.of("slow")
		require.Len(t, ran, 2)
		assert.Equal(t, 2, ran[1].Attempt)
		assert.Equal(t, ran[0].Key, ran[1].Key)

		// The first worker's call returns at last, to nobody.
		close(release)
		synctest.Wait()
		assert.Equal(t, "done by the second worker", f.steps(started.ID)[1].Result)
	})
}

func TestWork_CancelledDuringAModelCallTheRunIsYieldedWithNoFailureCounted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped := make(chan error, 1)
		began := make(chan struct{})
		model := execModelFunc(func(ctx context.Context, _ agent.Request) (agent.Response, error) {
			close(began)
			<-ctx.Done()
			stopped <- context.Cause(ctx)
			return agent.Response{}, ctx.Err()
		})
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk()},
			model: model,
			tune:  func(o *agent.Options) { o.DrainTimeout = workDrain },
		})
		started := f.start("clerk", "what is the total?")
		stop, done := f.working()
		<-began

		stop()
		at := time.Now()
		err := <-done

		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, workDrain, time.Since(at), "the call has DrainTimeout to finish")
		assert.Same(t, agent.ErrDrained, <-stopped, "and then its context ends")
		got := f.run(started.ID)
		assert.Equal(t, agent.StatusRunnable, got.Status)
		assert.Empty(t, got.LeaseOwner, "nothing of the call is still running, so the run is given back")
		assert.Zero(t, got.Failures, "a shutdown is not the run's failure")
		assert.Empty(t, got.Error)
		assert.Nil(t, got.NextAttemptAt)
		assert.Equal(t, []string{"1 model started"}, f.journal(started.ID), "nothing is recorded for the call")

		// Another worker takes the run with no wait, and makes the call again.
		second := f.rival(execConfig{
			defs:   []agent.Definition{execClerk()},
			script: agenttest.Replies(agenttest.Say("the total is 42")),
		})
		ended := second.execute(started.ID)
		assert.Equal(t, agent.StatusCompleted, ended.Status)
		assert.Zero(t, ended.Failures)
		assert.Equal(t, 2, f.steps(started.ID)[0].Attempts)
	})
}

func TestWork_AModelCallThatFinishesInsideTheDrainIsRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		began, release := make(chan struct{}), make(chan struct{})
		model := execModelFunc(func(ctx context.Context, _ agent.Request) (agent.Response, error) {
			close(began)
			<-release
			if err := ctx.Err(); err != nil {
				return agent.Response{}, err
			}
			return execSpent(agenttest.Use(agenttest.Call("call-1", "lookup", `{}`)), agent.Usage{CostMicros: 90}), nil
		})
		calls := &execCalls{}
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk(calls.tool("lookup", nil))},
			model: model,
			tune:  func(o *agent.Options) { o.DrainTimeout = workDrain },
		})
		started := f.start("clerk", "what is the total?")
		stop, done := f.working()
		<-began

		stop()
		time.Sleep(workDrain - time.Nanosecond)
		close(release)
		err := <-done

		require.ErrorIs(t, err, context.Canceled)
		got := f.run(started.ID)
		assert.Equal(t, int64(90), got.Usage.CostMicros, "a call that was paid for is recorded")
		assert.Equal(t, []string{"1 model completed", "2 lookup proposed"}, f.journal(started.ID))
		assert.Empty(t, got.LeaseOwner)
		assert.Zero(t, got.Failures)
		assert.Empty(t, calls.of("lookup"), "no step is begun once the worker is stopping")
	})
}

func TestWork_AModelThatIgnoresItsContextIsLeftBehind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		began, release := make(chan struct{}), make(chan struct{})
		model := execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
			close(began)
			<-release
			return agenttest.Say("too late"), nil
		})
		f := newExecFixture(t, execConfig{
			defs:  []agent.Definition{execClerk()},
			model: model,
			tune:  func(o *agent.Options) { o.DrainTimeout = workDrain },
		})
		started := f.start("clerk", "what is the total?")
		stop, done := f.working()
		<-began

		stop()
		at := time.Now()
		err := <-done

		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, workDrain+agent.LastWriteTimeout, time.Since(at),
			"Work waits for the drain and for a last write, and then returns without the execution")
		assert.Contains(t, f.logs.at(slog.LevelWarn), fmt.Sprintf(
			"agent: work: returning with executions that have not ended drain=%s", workDrain))
		held := f.run(started.ID)
		assert.Equal(t, "worker-1", held.LeaseOwner)

		// The keeper ended with the drain: the lease is not extended for an
		// execution that may never return, and so it lapses.
		expires := *held.LeaseExpiresAt
		f.pass(execTTL)
		assert.Equal(t, expires, *f.run(started.ID).LeaseExpiresAt)

		// When the model returns at last, its reply is not recorded.
		close(release)
		synctest.Wait()
		got := f.run(started.ID)
		assert.Equal(t, []string{"1 model started"}, f.journal(started.ID))
		assert.Empty(t, got.LeaseOwner, "the run nobody took meanwhile is given back")
		assert.Zero(t, got.Failures)
	})
}

func TestWork_AModelCallThatFailsInsideTheDrainIsAFailureAndIsRecorded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		began, release := make(chan struct{}), make(chan struct{})
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk()},
			model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
				close(began)
				<-release
				return agent.Response{}, errors.New("rate limited")
			}),
			tune: func(o *agent.Options) { o.DrainTimeout = workDrain },
		})
		started := f.start("clerk", "what is the total?")
		stop, done := f.working()
		<-began

		stop()
		synctest.Wait()
		close(release)
		require.ErrorIs(t, <-done, context.Canceled)

		got := f.run(started.ID)
		assert.Equal(t, 1, got.Failures, "the model's own error is the run's failure, whenever it comes")
		assert.Equal(t, "rate limited", got.Error)
		assert.Empty(t, got.LeaseOwner)
		require.NotNil(t, got.NextAttemptAt)
	})
}

func TestWork_AModelThatIgnoresItsContextOverAStoreThatIgnoresItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		began, release := make(chan struct{}), make(chan struct{})
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execClerk()},
			model: execModelFunc(func(context.Context, agent.Request) (agent.Response, error) {
				close(began)
				<-release
				return agenttest.Say("too late"), nil
			}),
			tune: func(o *agent.Options) { o.DrainTimeout = workDrain },
			lax:  true,
		})
		started := f.start("clerk", "what is the total?")
		stop, done := f.working()
		<-began
		stop()
		require.ErrorIs(t, <-done, context.Canceled)

		close(release)
		synctest.Wait()

		got := f.run(started.ID)
		assert.Equal(t, []string{"1 model started"}, f.journal(started.ID),
			"a reply that arrives after the drain ran out is not recorded, whatever the store would take")
		assert.Zero(t, got.ModelCalls)
		assert.Empty(t, got.LeaseOwner)
		assert.Zero(t, got.Failures)
	})
}

func TestWork_AStoreThatFailsIsLoggedAndTheWorkerGoesOn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var store *execHooked
		f := newExecFixture(t, execConfig{
			defs:   []agent.Definition{execClerk()},
			script: agenttest.Replies(agenttest.Say("done")),
			over:   hooked(&store),
		})
		store.claim = func(n int) error {
			if n == 1 {
				return errors.New("connection reset")
			}
			return nil
		}
		f.faults.FailBefore("ExpireApprovals", 1)
		started := f.start("clerk", "hello")

		stop, done := f.working()
		synctest.Wait()

		warned := f.logs.at(slog.LevelWarn)
		require.Len(t, warned, 2)
		assert.Contains(t, strings.Join(warned, "\n"), "agent: work: no run could be claimed error=connection reset")
		assert.Contains(t, strings.Join(warned, "\n"), "agent: work: overdue approvals could not be lapsed error=agenttest: store fault")
		assert.Equal(t, agent.StatusRunnable, f.run(started.ID).Status)

		// The next poll goes through, and nothing more is logged.
		time.Sleep(time.Second)
		synctest.Wait()
		assert.Equal(t, agent.StatusCompleted, f.run(started.ID).Status)
		assert.Len(t, f.logs.at(slog.LevelWarn), 2)

		stop()
		require.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestWork_ACallCutOffByItsOwnShutdownIsNotLogged(t *testing.T) {
	for _, op := range []string{"Claim", "ExpireApprovals"} {
		t.Run(op, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newExecFixture(t, execConfig{defs: []agent.Definition{execClerk()}})
				// The call is in flight when the worker is stopped.
				f.wire.hold(op, func(ctx context.Context) { <-ctx.Done() })
				stop, done := f.working()
				synctest.Wait()

				stop()

				require.ErrorIs(t, <-done, context.Canceled)
				assert.Empty(t, f.logs.at(slog.LevelWarn), "a call the shutdown cut off is not a store that failed")
			})
		})
	}
}

func TestWork_ReturnsOnlyOnceItsLastCallToTheStoreHas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExecFixture(t, execConfig{defs: []agent.Definition{execClerk()}})
		// A store that does not answer, and does not look at its context.
		release := make(chan struct{})
		f.wire.hold("ExpireApprovals", func(context.Context) { <-release })
		stop, done := f.working()
		synctest.Wait()

		stop()
		synctest.Wait()
		select {
		case err := <-done:
			require.Fail(t, "Work returned with a call to the store in flight", "%v", err)
		default:
		}
		close(release)
		require.ErrorIs(t, <-done, context.Canceled)
	})
}
