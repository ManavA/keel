package agent_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

// The crash sweep itself is TestExecute_CrashAtEveryStoreCall, beside the
// batch it plays and the report it reads, and over Postgres it is
// TestExecutor_CrashAtEveryStoreCallOverPostgres in agent/pg. What is here is
// the sweep's control: the case its report must fail.

// crashForgetful is a store that loses the write that records a tool's
// result, the first times it is asked to make one, and says nothing: the
// journal is left holding less than what happened, which is what the order
// of an execution's writes exists to prevent.
type crashForgetful struct {
	agent.Store

	mu   sync.Mutex
	left int
	lost []int
}

func (s *crashForgetful) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	if req.From == agent.StepStarted && req.To == agent.StepCompleted {
		s.mu.Lock()
		lose := s.left > 0
		if lose {
			s.left--
			s.lost = append(s.lost, req.Seq)
		}
		s.mu.Unlock()
		if lose {
			return nil
		}
	}
	return s.Store.UpdateStep(ctx, lease, req)
}

// The control. Over a store that discards the started to completed write, a
// tool is executed again though nothing interrupted it, and the report the
// sweeps assert on says so. A report that had stopped counting executions
// would pass the sweeps and fail here.
func TestCrash_TheReportCatchesAToolExecutedMoreOftenThanAnInterruptionAllows(t *testing.T) {
	want, _ := crashUninterrupted(t, func(f *execFixture) int { return f.faults.Calls() })

	tests := []struct {
		name string
		// lose is how many results the store discards, and allow how many
		// interruptions the report is told of.
		lose, allow int
		wrong       []string
	}{
		{"one result lost with nothing interrupted", 1, 0, []string{
			"lookup (%s) ran 2 times across 0 interruptions",
		}},
		{"two results lost across one interruption: executed more than twice", 2, 1, []string{
			"lookup (%s) ran 3 times across 1 interruptions",
		}},
		{"a result lost that an interruption accounts for is no fault", 1, 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := &execCalls{}
				var store *crashForgetful
				cfg := crashConfig(calls)
				cfg.over = func(inner agent.Store) agent.Store {
					store = &crashForgetful{Store: inner, left: tt.lose}
					return store
				}
				f := newExecFixture(t, cfg)

				require.NoError(t, crashDrive(f), "the batch still ends: a started step is run again")

				lead, _ := f.batch()
				require.Len(t, store.lost, tt.lose)
				for _, seq := range store.lost {
					require.Equal(t, 2, seq, "the result lost each time was lookup's")
				}
				require.Len(t, calls.byKey()[agent.StepKey(lead.ID, 2)], 1+tt.lose)
				var wrong []string
				for _, line := range tt.wrong {
					wrong = append(wrong, fmt.Sprintf(line, agent.StepKey(lead.ID, 2)))
				}
				assert.Equal(t, wrong, f.crashReport(calls, want, nil, tt.allow))
			})
		})
	}
}

// The same control for the at-most-once call: its result lost, it is a
// started step with nothing to say it was interrupted, and it is put to a
// person before it is run again. The report is content exactly because a
// person said yes in between.
func TestCrash_AnAtMostOnceCallWhoseResultIsLostIsNotExecutedAgainWithoutAPerson(t *testing.T) {
	want, _ := crashUninterrupted(t, func(f *execFixture) int { return f.faults.Calls() })

	synctest.Test(t, func(t *testing.T) {
		calls := &execCalls{}
		cfg := crashConfig(calls)
		cfg.over = func(inner agent.Store) agent.Store { return &crashLosesCharge{Store: inner} }
		f := newExecFixture(t, cfg)

		// The first round starts the batch and executes its lead once.
		_, _, err := crashRound(f)
		require.NoError(t, err)

		lead, _ := f.batch()
		require.Equal(t, agent.StatusWaiting, lead.Status, "the run parks on a person, and charge is not run again")
		require.Len(t, calls.of("charge"), 1)
		asked := f.approvals(lead.ID)
		require.Len(t, asked, 1)
		assert.Equal(t, agent.CauseInterrupted, asked[0].Cause)
		assert.Equal(t, 3, asked[0].Seq)

		// The rounds that follow approve whatever is asked.
		require.NoError(t, crashDrive(f))

		assert.Len(t, calls.of("charge"), 2, "run again once a person said to")
		assert.Empty(t, f.crashReport(calls, want, nil, 1))
	})
}

// crashLosesCharge loses the first result of step 3, the at-most-once call.
type crashLosesCharge struct {
	agent.Store
	once sync.Once
}

func (s *crashLosesCharge) UpdateStep(ctx context.Context, lease agent.Lease, req agent.StepUpdate) error {
	lose := false
	if req.Seq == 3 && req.From == agent.StepStarted && req.To == agent.StepCompleted {
		s.once.Do(func() { lose = true })
	}
	if lose {
		return nil
	}
	return s.Store.UpdateStep(ctx, lease, req)
}
