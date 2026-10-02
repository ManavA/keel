package agent_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
)

// Two engines over one store are two processes over one database. These are
// the ways a run passes from one to the other, 6.6 of the design: by a lease
// that lapsed, by a run given back, and not at all while the lease is live.

func TestTakeover_ARunIsNotClaimedWhileItsLeaseIsLiveAndIsOnceItHasLapsed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The first process holds the run with its tool in flight. Only the
		// kit's clock is moved, so its keeper never wakes: a process paused.
		tk := newExecTakeover(t)
		held := tk.first.state(tk.run.ID)
		require.Equal(t, "worker-1", held.run.LeaseOwner)

		for _, wait := range []time.Duration{0, execTTL - time.Millisecond} {
			tk.first.clock.Advance(wait)

			report, err := tk.second.engine.Tick(t.Context())

			require.NoError(t, err)
			assert.Zero(t, report, "nothing is claimed %s into a lease of %s", wait, execTTL)
			_, err = tk.second.engine.Execute(t.Context(), tk.run.ID)
			require.ErrorIs(t, err, agent.ErrNotClaimable)
			assert.Equal(t, held, tk.first.state(tk.run.ID), "and nothing is changed")
		}

		// The clock reaches the moment the lease lapses.
		tk.first.clock.Advance(time.Millisecond)
		ticked := make(chan agent.Report, 1)
		go func() {
			report, err := tk.second.engine.Tick(t.Context())
			assert.NoError(t, err)
			ticked <- report
		}()
		<-tk.began2
		taken := tk.first.state(tk.run.ID)
		assert.Equal(t, "worker-2", taken.run.LeaseOwner)
		assert.Equal(t, held.run.LeaseEpoch+1, taken.run.LeaseEpoch, "the claim raised the epoch")
		assert.Equal(t, 1, taken.run.Failures, "taking over a lapsed lease counts a failure")
		assert.Equal(t, []string{"1 model completed", "2 slow started"}, execJournal(taken.steps))
		assert.Equal(t, 2, taken.steps[1].Attempts, "the interrupted call is made again as its second attempt")

		// The first process's tool returns, and its next write is refused:
		// its execution ends there, having written nothing.
		asked := tk.first.wire.total()
		close(tk.release1)
		got := <-tk.outcome
		require.ErrorIs(t, got.err, agent.ErrLeaseLost)
		assert.Equal(t, 2, tk.first.wire.total()-asked,
			"the write the store refused and Execute's read of the run: no Yield, no second try")
		assert.Equal(t, taken, tk.first.state(tk.run.ID), "the journal is its new holder's alone")

		close(tk.release2)
		report := <-ticked
		assert.Equal(t, agent.Report{Claimed: 1, Completed: 1}, report)
		ended := tk.first.run(tk.run.ID)
		assert.Equal(t, agent.StatusCompleted, ended.Status)
		assert.Equal(t, "the second worker's result", tk.first.steps(tk.run.ID)[1].Result)
		assert.Zero(t, ended.Failures, "a step that completed reset the count")
		ran := tk.calls.of("slow")
		require.Len(t, ran, 2)
		assert.Equal(t, ran[0].Key, ran[1].Key, "the same idempotency key both times")
	})
}

func TestTakeover_ACleanYieldFreesTheLeaseAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		began, release := make(chan struct{}), make(chan struct{})
		defs := func() []agent.Definition {
			return []agent.Definition{execClerk(calls.tool("slow", func(context.Context, agent.Invocation) (string, error) {
				close(began)
				<-release
				return "done", nil
			}))}
		}
		first := newExecFixture(t, execConfig{
			defs:   defs(),
			script: agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "slow", `{}`)), agenttest.Say("all done")),
		})
		second := first.rival(execConfig{defs: defs()})
		started := first.start("clerk", "take your time")

		// The first process is told to stop while its tool runs. The tool
		// finishes, its result is recorded, and the run is given back.
		stopping := errors.New("the service is stopping")
		ctx, cancel := context.WithCancelCause(t.Context())
		outcome := first.begin(ctx, started.ID)
		<-began
		cancel(stopping)
		close(release)
		got := <-outcome

		require.ErrorIs(t, got.err, stopping)
		assert.Equal(t, agent.StatusRunnable, got.run.Status)
		assert.Empty(t, got.run.LeaseOwner)
		assert.Nil(t, got.run.LeaseExpiresAt)
		assert.Zero(t, got.run.Failures)
		assert.Nil(t, got.run.NextAttemptAt)
		assert.Equal(t, []string{"1 model completed", "2 slow completed"}, first.journal(started.ID))

		// No time has passed: the lease would be live for another thirty
		// seconds had it been left to lapse.
		report, err := second.engine.Tick(t.Context())

		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 1, Completed: 1}, report)
		ended := first.run(started.ID)
		assert.Equal(t, "all done", ended.Output)
		assert.Equal(t, got.run.LeaseEpoch+1, ended.LeaseEpoch)
		assert.Zero(t, ended.Failures, "a run that was given back did not fail")
		assert.Len(t, calls.of("slow"), 1, "the recorded call is not made again")
	})
}

func TestTakeover_ARunThatKillsEveryExecutionIsFinishedAsAbandonedAtMaxFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const maxFailures = 3
		calls := &execCalls{}
		// Each process's tool takes the process down with it: the store dies
		// before the result can be recorded.
		dying := func(build func(execConfig) *execFixture, script agenttest.Script) *execFixture {
			var p *execFixture
			p = build(execConfig{
				defs: []agent.Definition{execClerk(calls.tool("fatal", func(context.Context, agent.Invocation) (string, error) {
					p.faults.Kill()
					return "done", nil
				}))},
				script: script,
				tune:   func(o *agent.Options) { o.MaxFailures = maxFailures },
			})
			return p
		}
		first := dying(func(cfg execConfig) *execFixture { return newExecFixture(t, cfg) },
			agenttest.Replies(agenttest.Use(agenttest.Call("call-1", "fatal", `{}`)), agenttest.Say("never said")))
		started := first.start("clerk", "do the fatal thing")

		p := first
		for n := 1; n <= maxFailures; n++ {
			_, err := p.engine.Execute(t.Context(), started.ID)
			require.ErrorIs(t, err, agenttest.ErrKilled, "execution %d dies", n)
			left := first.run(started.ID)
			require.Equal(t, agent.StatusRunnable, left.Status)
			require.Equal(t, n-1, left.Failures, "a death is counted by the claim that takes the run over")
			first.clock.Advance(execTTL)
			p = dying(first.rival, nil)
		}
		require.Len(t, calls.of("fatal"), maxFailures)

		// The claim that takes it over this time is failure number
		// MaxFailures, and the execution ends the run before doing any of its
		// work.
		report, err := p.engine.Tick(t.Context())

		require.NoError(t, err)
		assert.Equal(t, agent.Report{Claimed: 1, Failed: 1}, report)
		got := first.run(started.ID)
		assert.Equal(t, agent.StatusFailed, got.Status)
		assert.Equal(t, agent.ReasonAbandoned, got.Reason)
		assert.Equal(t, maxFailures, got.Failures)
		assert.Equal(t, "abandoned: 3 executions in a row failed or lost the run", got.Error)
		assert.Empty(t, got.LeaseOwner)
		ran := calls.of("fatal")
		require.Len(t, ran, maxFailures, "the tool is not run a fourth time")
		for i, in := range ran {
			assert.Equal(t, i+1, in.Attempt)
			assert.Equal(t, ran[0].Key, in.Key)
		}
		assert.Equal(t, []string{"1 model completed", "2 fatal started"}, first.journal(started.ID),
			"the journal is left as the last death left it")
		assert.Len(t, first.model.Requests(), 1)
	})
}

// A run with two questions out, one of which is answered after its execution
// has read the journal and before it parks. Parked on the other question, the
// approved call would wait for an answer that has nothing to do with it. The
// store refuses the park, and the execution reads the answer and acts on it.
func TestTakeover_AnAnswerThatLandsAsARunWithTwoQuestionsParksIsActedOnAtOnce(t *testing.T) {
	answers := []struct {
		name    string
		approve bool
		journal []string
		result  string
		ran     int
	}{
		{"approved: the call runs", true,
			[]string{"1 model completed", "2 refund completed", "3 refund waiting"}, "refund ok", 1},
		{"declined: the call is declined", false,
			[]string{"1 model completed", "2 refund declined", "3 refund waiting"}, "declined by ops@example.test: not this one", 0},
	}
	for _, tt := range answers {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				var store *execHooked
				f := newExecFixture(t, execConfig{
					defs:  []agent.Definition{execClerk(calls.tool("refund", nil))},
					guard: &execGuard{answers: map[string]agent.Decision{"refund": {Effect: agent.Ask, Rule: "ask-first"}}},
					script: agenttest.Replies(
						agenttest.Use(
							agenttest.Call("call-1", "refund", `{"order":7}`),
							agenttest.Call("call-2", "refund", `{"order":8}`),
						),
						agenttest.Say("both are settled"),
					),
					over: hooked(&store),
				})
				started := f.start("clerk", "refund orders 7 and 8")
				bySeq := func() map[int]agent.Approval {
					out := map[int]agent.Approval{}
					for _, approval := range f.approvals(started.ID) {
						out[approval.Seq] = approval
					}
					return out
				}
				answered := false
				store.on("Park", func() {
					if answered {
						return
					}
					answered = true
					asked := bySeq()
					if !assert.Len(t, asked, 2, "both questions are out when the execution parks") {
						return
					}
					var err error
					if tt.approve {
						_, err = f.engine.Approve(t.Context(), asked[2].ID, "ops@example.test", "")
					} else {
						_, err = f.engine.Decline(t.Context(), asked[2].ID, "ops@example.test", "not this one")
					}
					assert.NoError(t, err)
				})

				got := f.execute(started.ID)

				assert.Equal(t, agent.StatusWaiting, got.Status, "the second question is still pending")
				assert.Equal(t, agent.ReasonApproval, got.Reason)
				assert.Empty(t, got.LeaseOwner)
				assert.Equal(t, tt.journal, f.journal(started.ID), "the answer was acted on by the execution that was about to park")
				assert.Equal(t, tt.result, f.steps(started.ID)[1].Result)
				assert.Len(t, calls.of("refund"), tt.ran)
				_, _, parks := store.count()
				assert.Equal(t, 2, parks, "refused once, and then parked on what was left")
				assert.Zero(t, got.Failures)

				// The other answer wakes the run, and it ends.
				_, err := f.engine.Approve(t.Context(), bySeq()[3].ID, "ops@example.test", "")
				require.NoError(t, err)
				ended := f.execute(started.ID)
				assert.Equal(t, agent.StatusCompleted, ended.Status)
				assert.Equal(t, "both are settled", ended.Output)
			})
		})
	}
}

// The same for two children: one ends after its parent's execution has read
// the journal and before it parks on both. Its result is collected by that
// execution, which then parks on the other.
func TestTakeover_AChildThatEndsAsItsParentParksOnTwoIsCollectedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var store *execHooked
		script := agenttest.ByAgent(map[string]agenttest.Script{
			"lead": agenttest.Replies(
				agenttest.Use(
					agenttest.Call("call-1", "review", `{"doc":1}`),
					agenttest.Call("call-2", "review", `{"doc":2}`),
				),
				agenttest.Say("both are reviewed"),
			),
			"reviewer": execSummaries,
		})
		f := newExecFixture(t, execConfig{
			defs: []agent.Definition{execLead(), execReviewer()}, script: script, over: hooked(&store),
		})
		worker := f.rival(execConfig{defs: []agent.Definition{execLead(), execReviewer()}})
		started := f.start("lead", "review both")
		ended := false
		store.on("Park", func() {
			if ended {
				return
			}
			ended = true
			children := f.childrenInOrder(started.ID)
			if !assert.Len(t, children, 2) {
				return
			}
			// Another process executes the first child to its end.
			_, err := worker.engine.Execute(t.Context(), children[0].ID)
			assert.NoError(t, err)
		})

		got := f.execute(started.ID)

		assert.Equal(t, agent.StatusWaiting, got.Status)
		assert.Equal(t, agent.ReasonChildren, got.Reason)
		assert.Equal(t, []string{"1 model completed", "2 review completed", "3 review waiting"}, f.journal(started.ID))
		assert.Equal(t, `summary of {"doc":1}`, f.steps(started.ID)[1].Result)
		_, _, parks := store.count()
		assert.Equal(t, 2, parks)

		_, err := worker.engine.Execute(t.Context(), f.childrenInOrder(started.ID)[1].ID)
		require.NoError(t, err)
		done := f.execute(started.ID)
		assert.Equal(t, agent.StatusCompleted, done.Status)
		assert.Equal(t, "both are reviewed", done.Output)
	})
}
