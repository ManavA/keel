package agenttest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

func claimCases() []storeCase {
	claim := func(k *kit, owner string, agents ...string) *agent.Run {
		k.t.Helper()
		run, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: owner, Agents: agents, Now: k.tick(), TTL: suiteTTL})
		require.NoError(k.t, err)
		return run
	}
	return []storeCase{
		{"with nothing to take, returns nil", func(k *kit) {
			assert.Nil(k.t, claim(k, workerA, agentAlpha))
		}},
		{"takes the oldest runnable run first", func(k *kit) {
			first := k.newRun(agentAlpha)
			second := k.newRun(agentAlpha)
			third := k.newRun(agentAlpha)
			// Stored out of order, so oldest is by CreatedAt and not by
			// arrival.
			k.insert(third)
			k.insert(first)
			k.insert(second)

			for _, want := range []agent.Run{first, second, third} {
				got := claim(k, workerA, agentAlpha)
				require.NotNil(k.t, got)
				assert.Equal(k.t, want.ID, got.ID)
			}
			assert.Nil(k.t, claim(k, workerA, agentAlpha), "every run is held")
		}},
		{"runs created at one instant are taken in the order of their ids", func(k *kit) {
			older := k.create(agentAlpha)
			instant := k.tick()
			var same []string
			for range 6 {
				run := k.newRun(agentAlpha)
				run.CreatedAt, run.UpdatedAt = instant, instant
				same = append(same, k.insert(run).ID)
			}
			newer := k.create(agentAlpha)
			// Not the order they arrived in, which a table does not keep.
			slices.Sort(same)
			want := append(append([]string{older.ID}, same...), newer.ID)

			var took []string
			for range want {
				got := claim(k, workerA, agentAlpha)
				require.NotNil(k.t, got)
				took = append(took, got.ID)
			}

			assert.Equal(k.t, want, took)
		}},
		{"records the hold on the run it returns", func(k *kit) {
			created := k.create(agentAlpha)
			now := k.tick()

			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: []string{agentAlpha}, Now: now, TTL: suiteTTL,
			})

			require.NoError(k.t, err)
			require.NotNil(k.t, got)
			expires := now.Add(suiteTTL)
			want := created
			want.LeaseOwner, want.LeaseEpoch, want.LeaseExpiresAt = workerA, 1, &expires
			want.Rev, want.UpdatedAt = 2, now
			k.equalRun(want, *got)
			k.equalRun(want, k.run(created.ID))
			assert.Equal(k.t, agent.Lease{RunID: created.ID, Owner: workerA, Epoch: 1}, got.Lease())
			assert.True(k.t, got.Running(now))
		}},
		{"takes only runs of the agents named", func(k *kit) {
			alpha := k.create(agentAlpha)
			beta := k.create(agentBeta)

			assert.Nil(k.t, claim(k, workerA), "no agent named")
			assert.Nil(k.t, claim(k, workerA, "gamma"), "an agent with no runs")

			got := claim(k, workerA, agentBeta)
			require.NotNil(k.t, got)
			assert.Equal(k.t, beta.ID, got.ID, "the older run is another agent's")
			assert.Nil(k.t, claim(k, workerA, agentBeta))

			got = claim(k, workerA, agentBeta, agentAlpha)
			require.NotNil(k.t, got)
			assert.Equal(k.t, alpha.ID, got.ID)
		}},
		{"takes nothing while a lease is live", func(k *kit) {
			run, _ := k.held(agentAlpha)
			k.clock.Advance(suiteTTL - 2*time.Millisecond)
			before := k.snapshot(run.ID)

			// One millisecond before the lease lapses.
			assert.Nil(k.t, claim(k, workerB, agentAlpha))
			k.unchanged(before)
		}},
		{"takes a lease at the instant it lapses", func(k *kit) {
			run, _ := k.held(agentAlpha)
			k.clock.Advance(run.LeaseExpiresAt.Sub(k.now()) - time.Millisecond)

			got := claim(k, workerB, agentAlpha)

			require.NotNil(k.t, got)
			assert.Equal(k.t, run.ID, got.ID)
		}},
		{"taking over a lapsed lease raises the epoch and counts a failure", func(k *kit) {
			run, _ := k.held(agentAlpha)

			k.lapse()
			now := k.tick()
			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerB, Agents: []string{agentAlpha}, Now: now, TTL: suiteTTL,
			})
			require.NoError(k.t, err)
			require.NotNil(k.t, got)
			assert.Equal(k.t, workerB, got.LeaseOwner)
			assert.Equal(k.t, int64(2), got.LeaseEpoch)
			assert.Equal(k.t, 1, got.Failures)
			assert.Equal(k.t, run.Rev+1, got.Rev)
			k.timeIs(now.Add(suiteTTL), got.LeaseExpiresAt, "the lease runs from the new claim")

			k.lapse()
			again := claim(k, workerA, agentAlpha)
			require.NotNil(k.t, again)
			assert.Equal(k.t, int64(3), again.LeaseEpoch)
			assert.Equal(k.t, 2, again.Failures, "each takeover counts")
		}},
		{"a released lease is taken at once, with Failures unchanged", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Now: k.tick()}))

			got := claim(k, workerB, agentAlpha)

			require.NotNil(k.t, got, "the first lease would not have lapsed yet")
			assert.Equal(k.t, run.ID, got.ID)
			assert.Equal(k.t, workerB, got.LeaseOwner)
			assert.Equal(k.t, int64(2), got.LeaseEpoch, "every claim raises the epoch")
			assert.Zero(k.t, got.Failures)
		}},
		{"NextAttemptAt in the future hides a run", func(k *kit) {
			hidden, lease := k.held(agentAlpha)
			later := k.create(agentAlpha)
			retryAt := k.now().Add(5 * time.Second)
			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{NextAttemptAt: &retryAt, Now: k.tick()}))

			got := claim(k, workerB, agentAlpha)
			require.NotNil(k.t, got)
			assert.Equal(k.t, later.ID, got.ID, "the older run is hidden, and does not hold up the one after it")

			k.clock.Advance(retryAt.Sub(k.now()) - 2*time.Millisecond)
			assert.Nil(k.t, claim(k, workerB, agentAlpha), "a millisecond before its time")

			got = claim(k, workerB, agentAlpha)
			require.NotNil(k.t, got, "at its time")
			assert.Equal(k.t, hidden.ID, got.ID)
			k.timeIs(retryAt, got.NextAttemptAt, "a claim leaves NextAttemptAt for the next Yield to set")
		}},
		{"a waiting run is never claimed", func(k *kit) {
			run, _ := k.parked(agentAlpha, nil)
			before := k.snapshot(run.ID)

			assert.Nil(k.t, claim(k, workerB, agentAlpha))
			k.lapse()
			assert.Nil(k.t, claim(k, workerB, agentAlpha), "however long it waits")
			k.unchanged(before)
		}},
		{"an ended run is never claimed", func(k *kit) {
			for _, status := range finalStatuses {
				run := k.ended(agentAlpha, status)
				before := k.snapshot(run.ID)

				assert.Nil(k.t, claim(k, workerB, agentAlpha), status)
				k.lapse()
				assert.Nil(k.t, claim(k, workerB, agentAlpha), status)
				k.unchanged(before)
			}
		}},
		{"with RunID, takes that run and no other", func(k *kit) {
			older := k.create(agentAlpha)
			wanted := k.create(agentAlpha)
			before := k.snapshot(older.ID)

			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: []string{agentAlpha}, RunID: wanted.ID, Now: k.tick(), TTL: suiteTTL,
			})

			require.NoError(k.t, err)
			require.NotNil(k.t, got)
			assert.Equal(k.t, wanted.ID, got.ID)
			assert.Equal(k.t, int64(1), got.LeaseEpoch)
			k.unchanged(before)
		}},
		{"with RunID, a lapsed lease is taken over and counts a failure", func(k *kit) {
			run, _ := k.held(agentAlpha)
			k.lapse()

			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerB, Agents: suiteAgents, RunID: run.ID, Now: k.tick(), TTL: suiteTTL,
			})

			require.NoError(k.t, err)
			require.NotNil(k.t, got)
			assert.Equal(k.t, workerB, got.LeaseOwner)
			assert.Equal(k.t, int64(2), got.LeaseEpoch)
			assert.Equal(k.t, 1, got.Failures)
		}},
		{"with RunID, a run that cannot be taken is ErrNotClaimable", func(k *kit) {
			tests := []struct {
				name   string
				agents []string
				run    func() agent.Run
			}{
				{"its lease is live", suiteAgents, func() agent.Run {
					run, _ := k.held(agentAlpha)
					return run
				}},
				{"its agent is not named", []string{agentBeta}, func() agent.Run {
					return k.create(agentAlpha)
				}},
				{"no agent is named", nil, func() agent.Run {
					return k.create(agentAlpha)
				}},
				{"its NextAttemptAt is in the future", suiteAgents, func() agent.Run {
					run, lease := k.held(agentAlpha)
					retryAt := k.now().Add(time.Minute)
					require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{NextAttemptAt: &retryAt, Now: k.tick()}))
					return run
				}},
				{"it is waiting", suiteAgents, func() agent.Run {
					run, _ := k.parked(agentAlpha, nil)
					return run
				}},
				{"it completed", suiteAgents, func() agent.Run { return k.ended(agentAlpha, agent.StatusCompleted) }},
				{"it failed", suiteAgents, func() agent.Run { return k.ended(agentAlpha, agent.StatusFailed) }},
				{"it was cancelled", suiteAgents, func() agent.Run { return k.ended(agentAlpha, agent.StatusCancelled) }},
			}
			for _, tt := range tests {
				run := tt.run()
				before := k.snapshot(run.ID)

				got, err := k.store.Claim(k.ctx, agent.ClaimRequest{
					Owner: workerB, Agents: tt.agents, RunID: run.ID, Now: k.tick(), TTL: suiteTTL,
				})

				require.ErrorIs(k.t, err, agent.ErrNotClaimable, tt.name)
				assert.Nil(k.t, got, tt.name)
				k.unchanged(before)
			}
		}},
		{"a claim with no owner is refused", func(k *kit) {
			run := k.create(agentAlpha)
			before := k.snapshot(run.ID)

			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Agents: suiteAgents, Now: k.tick(), TTL: suiteTTL})
			require.Error(k.t, err)
			assert.Nil(k.t, got)

			got, err = k.store.Claim(k.ctx, agent.ClaimRequest{Agents: suiteAgents, RunID: run.ID, Now: k.tick(), TTL: suiteTTL})
			require.Error(k.t, err)
			assert.Nil(k.t, got)
			k.unchanged(before)
		}},
		{"a claim with a TTL of zero or less is refused", func(k *kit) {
			run := k.create(agentAlpha)
			before := k.snapshot(run.ID)

			for _, ttl := range []time.Duration{0, -time.Second} {
				got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: suiteAgents, Now: k.tick(), TTL: ttl})
				require.Error(k.t, err, "ttl %s", ttl)
				assert.Nil(k.t, got)

				got, err = k.store.Claim(k.ctx, agent.ClaimRequest{
					Owner: workerA, Agents: suiteAgents, RunID: run.ID, Now: k.tick(), TTL: ttl,
				})
				require.Error(k.t, err, "ttl %s, by id", ttl)
				assert.NotErrorIs(k.t, err, agent.ErrNotClaimable, "the claim is refused, not the run")
				assert.Nil(k.t, got)
			}
			k.unchanged(before)
		}},
		{"eight claims at once for one run: one takes it", func(k *kit) {
			run := k.create(agentAlpha)
			now := k.tick()

			got := claimAtOnce(k, 8, func(owner string) agent.ClaimRequest {
				return agent.ClaimRequest{Owner: owner, Agents: suiteAgents, Now: now, TTL: suiteTTL}
			})

			var winners []string
			for _, r := range got {
				require.NoError(k.t, r.err)
				if r.run != nil {
					assert.Equal(k.t, run.ID, r.run.ID)
					winners = append(winners, r.run.LeaseOwner)
				}
			}
			require.Len(k.t, winners, 1, "exactly one claim takes the run")
			after := k.run(run.ID)
			assert.Equal(k.t, winners[0], after.LeaseOwner)
			assert.Equal(k.t, int64(1), after.LeaseEpoch)
			assert.Equal(k.t, run.Rev+1, after.Rev)
		}},
		{"eight claims at once for one run by id: one takes it, the rest cannot", func(k *kit) {
			run := k.create(agentAlpha)
			now := k.tick()

			got := claimAtOnce(k, 8, func(owner string) agent.ClaimRequest {
				return agent.ClaimRequest{Owner: owner, Agents: suiteAgents, RunID: run.ID, Now: now, TTL: suiteTTL}
			})

			won, refused := 0, 0
			for _, r := range got {
				switch {
				case r.err == nil:
					require.NotNil(k.t, r.run)
					won++
				case errors.Is(r.err, agent.ErrNotClaimable):
					assert.Nil(k.t, r.run)
					refused++
				default:
					require.NoError(k.t, r.err)
				}
			}
			assert.Equal(k.t, 1, won)
			assert.Equal(k.t, 7, refused)
			assert.Equal(k.t, int64(1), k.run(run.ID).LeaseEpoch)
		}},
		{"eight claims at once for eight runs: each takes a different one", func(k *kit) {
			want := make([]string, 8)
			for i := range want {
				want[i] = k.create(agentAlpha).ID
			}
			now := k.tick()

			// Each claim asks until it is given a run: a store may pass over
			// a run another claim is taking and answer nil while runs remain.
			var wg sync.WaitGroup
			took := make([]string, 8)
			errs := make([]error, 8)
			gate := make(chan struct{})
			for i := range 8 {
				wg.Go(func() {
					<-gate
					for range 50 {
						run, err := k.store.Claim(k.ctx, agent.ClaimRequest{
							Owner: workerName(i), Agents: suiteAgents, Now: now, TTL: suiteTTL,
						})
						if err != nil || run != nil {
							if run != nil {
								took[i] = run.ID
							}
							errs[i] = err
							return
						}
					}
				})
			}
			close(gate)
			wg.Wait()

			for _, err := range errs {
				require.NoError(k.t, err)
			}
			assert.ElementsMatch(k.t, want, took, "no run is taken twice and none is left")
		}},
	}
}

// workerName names the ith of several workers acting at once.
func workerName(i int) string { return fmt.Sprintf("worker-%d", i) }

type claimResult struct {
	run *agent.Run
	err error
}

// claimAtOnce makes n claims from n goroutines released together, each under
// an owner of its own.
func claimAtOnce(k *kit, n int, req func(owner string) agent.ClaimRequest) []claimResult {
	var wg sync.WaitGroup
	results := make([]claimResult, n)
	gate := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-gate
			run, err := k.store.Claim(k.ctx, req(workerName(i)))
			results[i] = claimResult{run: run, err: err}
		})
	}
	close(gate)
	wg.Wait()
	return results
}

func heartbeatCases() []storeCase {
	return []storeCase{
		{"extends the lease to now plus ttl, and leaves Rev and UpdatedAt alone", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.clock.Advance(10 * time.Second)
			now := k.now()

			cancelRequested, err := k.store.Heartbeat(k.ctx, lease, now, time.Minute)

			require.NoError(k.t, err)
			assert.False(k.t, cancelRequested)
			expires := now.Add(time.Minute)
			want := run
			want.LeaseExpiresAt = &expires
			k.equalRun(want, k.run(run.ID))
		}},
		{"keeps the run from being claimed past its first expiry", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.clock.Advance(20 * time.Second)
			_, err := k.store.Heartbeat(k.ctx, lease, k.now(), suiteTTL)
			require.NoError(k.t, err)

			k.clock.Advance(15 * time.Second)
			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerB, Agents: suiteAgents, Now: k.now(), TTL: suiteTTL})
			require.NoError(k.t, err)
			assert.Nil(k.t, got, "35 seconds after the claim, 15 after the heartbeat")

			k.clock.Advance(15 * time.Second)
			got, err = k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerB, Agents: suiteAgents, Now: k.now(), TTL: suiteTTL})
			require.NoError(k.t, err)
			require.NotNil(k.t, got, "30 seconds after the heartbeat")
			assert.Equal(k.t, run.ID, got.ID)
		}},
		{"reports a request to cancel", func(k *kit) {
			run, lease := k.held(agentAlpha)
			require.NoError(k.t, k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: run.ID, By: personA, Now: k.tick()}))

			cancelRequested, err := k.store.Heartbeat(k.ctx, lease, k.tick(), suiteTTL)

			require.NoError(k.t, err)
			assert.True(k.t, cancelRequested)
		}},
		{"a TTL of zero or less is refused, and the lease is as it was", func(k *kit) {
			run, lease := k.held(agentAlpha)
			before := k.snapshot(run.ID)

			for _, ttl := range []time.Duration{0, -time.Second} {
				_, err := k.store.Heartbeat(k.ctx, lease, k.tick(), ttl)
				require.Error(k.t, err, "ttl %s", ttl)
				assert.NotErrorIs(k.t, err, agent.ErrLeaseLost, "the heartbeat is refused, not the lease")
			}
			k.unchanged(before)

			_, err := k.store.Heartbeat(k.ctx, lease, k.tick(), suiteTTL)
			assert.NoError(k.t, err)
		}},
		{"extends a lapsed lease that nobody has taken: the hold is the epoch, not the time", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.lapse()

			_, err := k.store.Heartbeat(k.ctx, lease, k.tick(), suiteTTL)
			require.NoError(k.t, err)

			got, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerB, Agents: suiteAgents, Now: k.tick(), TTL: suiteTTL})
			require.NoError(k.t, err)
			assert.Nil(k.t, got)
			assert.Equal(k.t, lease, k.run(run.ID).Lease())
		}},
	}
}

func yieldCases() []storeCase {
	return []storeCase{
		{"releases the lease and leaves the run runnable", func(k *kit) {
			run, lease := k.held(agentAlpha)
			now := k.tick()

			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Now: now}))

			want := run
			want.LeaseOwner, want.LeaseExpiresAt = "", nil
			want.Rev, want.UpdatedAt = run.Rev+1, now
			after := k.run(run.ID)
			k.equalRun(want, after)
			assert.Equal(k.t, agent.StatusRunnable, after.Status)
			assert.Equal(k.t, lease.Epoch, after.LeaseEpoch, "the epoch moves on a claim, not on a release")
		}},
		{"with Failed, counts a failure and records the error", func(k *kit) {
			run, lease := k.held(agentAlpha)
			now := k.tick()
			retryAt := now.Add(2 * time.Second)

			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{
				Failed: true, Error: "model unavailable", NextAttemptAt: &retryAt, Now: now,
			}))

			want := run
			want.LeaseOwner, want.LeaseExpiresAt = "", nil
			want.Failures, want.Error, want.NextAttemptAt = 1, "model unavailable", &retryAt
			want.Rev, want.UpdatedAt = run.Rev+1, now
			k.equalRun(want, k.run(run.ID))
		}},
		{"each failed execution counts", func(k *kit) {
			run, lease := k.held(agentAlpha)
			for want := 1; want <= 3; want++ {
				require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Failed: true, Error: "boom", Now: k.tick()}))
				assert.Equal(k.t, want, k.run(run.ID).Failures)
				lease = k.claim(workerA, run.ID)
			}
		}},
		{"without Failed, leaves Failures and Error as they were", func(k *kit) {
			run, lease := k.held(agentAlpha)
			lease = k.failOnce(lease)

			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Error: "not recorded", Now: k.tick()}))

			after := k.run(run.ID)
			assert.Equal(k.t, 1, after.Failures)
			assert.Equal(k.t, "boom", after.Error)
		}},
		{"sets NextAttemptAt as given, and clears it when given none", func(k *kit) {
			run, lease := k.held(agentAlpha)
			retryAt := k.now().Add(time.Second)
			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{NextAttemptAt: &retryAt, Now: k.tick()}))
			k.timeIs(retryAt, k.run(run.ID).NextAttemptAt, "NextAttemptAt")

			k.clock.Advance(time.Second)
			lease = k.claim(workerA, run.ID)
			require.NoError(k.t, k.store.Yield(k.ctx, lease, agent.YieldRequest{Now: k.tick()}))

			assert.Nil(k.t, k.run(run.ID).NextAttemptAt)
		}},
	}
}

// errNotParked is what a fenced Park reports when the store took the lease
// and then found nothing to wait for.
var errNotParked = errors.New("park reported false")

func fencingCases() []storeCase {
	// Each way a hold stops being the run's. lose makes it so, and returns
	// the lease that is now stale and how to get the one that is good.
	losses := []struct {
		name string
		lose func(k *kit, held agent.Lease) (stale agent.Lease, current func() agent.Lease)
	}{
		{"after another worker took the lapsed lease", func(k *kit, held agent.Lease) (agent.Lease, func() agent.Lease) {
			k.lapse()
			taken := k.claim(workerB, held.RunID)
			return held, func() agent.Lease { return taken }
		}},
		{"after the same worker claimed the run again", func(k *kit, held agent.Lease) (agent.Lease, func() agent.Lease) {
			k.lapse()
			taken := k.claim(held.Owner, held.RunID)
			return held, func() agent.Lease { return taken }
		}},
		{"after the lease was given back", func(k *kit, held agent.Lease) (agent.Lease, func() agent.Lease) {
			require.NoError(k.t, k.store.Yield(k.ctx, held, agent.YieldRequest{Now: k.tick()}))
			return held, func() agent.Lease { return k.claim(workerB, held.RunID) }
		}},
		{"under another owner's name", func(_ *kit, held agent.Lease) (agent.Lease, func() agent.Lease) {
			return agent.Lease{RunID: held.RunID, Owner: workerB, Epoch: held.Epoch}, func() agent.Lease { return held }
		}},
		{"under an epoch the run has not reached", func(_ *kit, held agent.Lease) (agent.Lease, func() agent.Lease) {
			return agent.Lease{RunID: held.RunID, Owner: held.Owner, Epoch: held.Epoch + 1}, func() agent.Lease { return held }
		}},
		{"with no owner, on a run that is held", func(_ *kit, held agent.Lease) (agent.Lease, func() agent.Lease) {
			return agent.Lease{RunID: held.RunID, Epoch: held.Epoch}, func() agent.Lease { return held }
		}},
		{"with no owner, on a run nobody holds", func(k *kit, held agent.Lease) (agent.Lease, func() agent.Lease) {
			require.NoError(k.t, k.store.Yield(k.ctx, held, agent.YieldRequest{Now: k.tick()}))
			// Owner and epoch are now exactly what the run records.
			return k.run(held.RunID).Lease(), func() agent.Lease { return k.claim(workerB, held.RunID) }
		}},
	}

	// Every method that takes a Lease. prepare journals what the call needs
	// to be one the store would accept; call makes it under the lease given.
	writes := []struct {
		name    string
		prepare func(k *kit, held agent.Lease)
		call    func(k *kit, lease agent.Lease) error
	}{
		{"Heartbeat", nil, func(k *kit, lease agent.Lease) error {
			_, err := k.store.Heartbeat(k.ctx, lease, k.tick(), suiteTTL)
			return err
		}},
		{"Yield", nil, func(k *kit, lease agent.Lease) error {
			return k.store.Yield(k.ctx, lease, agent.YieldRequest{Failed: true, Error: "boom", Now: k.tick()})
		}},
		{
			"Park",
			func(k *kit, held agent.Lease) {
				k.reply(held, Call("call-1", toolSend, sendInput))
				k.ask(held, 2, nil)
			},
			func(k *kit, lease agent.Lease) error {
				parked, err := k.store.Park(k.ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: k.tick()})
				if err == nil && !parked {
					return errNotParked
				}
				return err
			},
		},
		{"Finish", nil, func(k *kit, lease agent.Lease) error {
			return k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: agent.StatusCompleted, Output: "done", Now: k.tick()})
		}},
		{"BeginModel", nil, func(k *kit, lease agent.Lease) error {
			return k.store.BeginModel(k.ctx, lease, 1, k.tick())
		}},
		{
			"CompleteModel",
			func(k *kit, held agent.Lease) {
				require.NoError(k.t, k.store.BeginModel(k.ctx, held, 1, k.tick()))
			},
			func(k *kit, lease agent.Lease) error {
				return k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
					Seq:     1,
					Message: Use(Call("call-1", toolLookup, lookupInput)).Message,
					Stop:    agent.StopToolUse,
					Model:   suiteModel,
					Usage:   agent.Usage{InputTokens: 10, OutputTokens: 5, CostMicros: 20},
					Now:     k.tick(),
				})
			},
		},
		{
			"UpdateStep",
			func(k *kit, held agent.Lease) { k.reply(held, Call("call-1", toolSend, sendInput)) },
			func(k *kit, lease agent.Lease) error {
				return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
					Seq: 2, From: agent.StepProposed, To: agent.StepStarted, Decision: agent.Allow, Rule: suiteRule, Now: k.tick(),
				})
			},
		},
		{
			"RequestApproval",
			func(k *kit, held agent.Lease) { k.reply(held, Call("call-1", toolSend, sendInput)) },
			func(k *kit, lease agent.Lease) error {
				_, err := k.store.RequestApproval(k.ctx, lease, k.askRequest(2))
				return err
			},
		},
	}

	var cases []storeCase
	for _, w := range writes {
		for _, l := range losses {
			cases = append(cases, storeCase{w.name + " " + l.name + " is ErrLeaseLost", func(k *kit) {
				run, held := k.held(agentAlpha)
				if w.prepare != nil {
					w.prepare(k, held)
				}
				stale, current := l.lose(k, held)
				before := k.snapshot(run.ID)

				err := w.call(k, stale)

				require.ErrorIs(k.t, err, agent.ErrLeaseLost)
				k.unchanged(before)
				// The same call under the lease that holds the run goes
				// through, so it was the lease that the store refused.
				require.NoError(k.t, w.call(k, current()))
			}})
		}
	}

	cases = append(cases,
		storeCase{"a write under a lease that has lapsed but not been taken goes through", func(k *kit) {
			run, lease := k.held(agentAlpha)
			k.lapse()
			k.clock.Advance(time.Hour)

			require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))

			assert.Len(k.t, k.steps(run.ID), 1, "a hold is lost to another claim, not to the clock")
		}},
		storeCase{"a write racing a takeover lands before the claim or not at all", func(k *kit) {
			// The claim names the run, and a claim by id waits for a write
			// in progress and then decides: it must take the run in every
			// round, whichever of the two reaches the run first. A claim
			// that passed over a run being written to, as a claim without
			// RunID may, would fail here with ErrNotClaimable, and leave a
			// lapsed run unclaimed because its last holder was still writing.
			const rounds = 50
			for range rounds {
				run, held := k.held(agentAlpha)
				k.lapse()
				now := k.tick()

				var (
					wg                 sync.WaitGroup
					writeErr, claimErr error
					seen               []agent.Step
					gate               = make(chan struct{})
				)
				wg.Go(func() {
					<-gate
					writeErr = k.store.BeginModel(k.ctx, held, 1, now)
				})
				wg.Go(func() {
					<-gate
					_, claimErr = k.store.Claim(k.ctx, agent.ClaimRequest{
						Owner: workerB, Agents: suiteAgents, RunID: run.ID, Now: now, TTL: suiteTTL,
					})
					if claimErr == nil {
						// What the new holder finds when it starts work.
						seen, claimErr = k.store.Steps(k.ctx, run.ID)
					}
				})
				close(gate)
				wg.Wait()

				require.NoError(k.t, claimErr)
				final := k.steps(run.ID)
				k.equalSteps(seen, final)
				if writeErr == nil {
					assert.Len(k.t, final, 1, "the write landed, so the new holder read it")
				} else {
					require.ErrorIs(k.t, writeErr, agent.ErrLeaseLost)
					assert.Empty(k.t, final, "the write was refused, so nothing was added")
				}
			}
		}},
	)
	return cases
}
