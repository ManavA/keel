package pg_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// The indexes in the migration are there for three statements: the claim,
// the run listing and the queue of pending approvals. On a table with
// nothing in it the planner reads the table whatever the indexes are, so
// this fills the tables with a few thousand rows, most of them in a state
// those statements pass over, and reads the plan each of them gets.
func TestStore_PlansUseTheirIndexes(t *testing.T) {
	pool := newDatabase(t).pool(t)
	ctx := t.Context()

	const runs = 4000
	_, err := pool.Exec(ctx, `
insert into agent_runs (id, agent, status, input, definition, rev, created_at, updated_at)
select gen_random_uuid(), 'alpha', case when n % 40 = 0 then 'runnable' else 'completed' end,
       '', '{}', 1, $1::timestamptz + n * interval '1 second', $1
from generate_series(1, $2::int) as n`, testStart, runs)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
insert into agent_approvals (id, run_id, seq, attempt, cause, tool, input, action, status, rev, requested_at)
select gen_random_uuid(), id, 2, 1, 'guard', 'send', '{}', '{}',
       case when row_number() over (order by created_at) % 100 = 0 then 'pending' else 'approved' end,
       1, created_at
from agent_runs`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `analyze agent_runs; analyze agent_approvals`)
	require.NoError(t, err)

	var (
		mu   sync.Mutex
		made = map[string]statement{}
	)
	store := agentpg.New(stepped{Beginner: pool, before: func(st statement) {
		mu.Lock()
		defer mu.Unlock()
		made[st.sql] = st
	}})

	// The cursor is a position in the middle of the table.
	var middle agent.Cursor
	require.NoError(t, pool.QueryRow(ctx, `select created_at, id::text from agent_runs order by created_at offset $1 limit 1`, runs/2).
		Scan(&middle.CreatedAt, &middle.ID))

	claimed, err := store.Claim(ctx, agent.ClaimRequest{Owner: workerA, Agents: testAgents, Now: testStart.Add(24 * 365 * 3600e9), TTL: testTTL})
	require.NoError(t, err)
	require.NotNil(t, claimed)
	_, err = store.ListRuns(ctx, agent.RunFilter{Before: &middle})
	require.NoError(t, err)
	_, err = store.ListApprovals(ctx, agent.ApprovalFilter{Status: agent.ApprovalPending})
	require.NoError(t, err)

	planOf := func(contains string) string {
		t.Helper()
		for sql, st := range made {
			if !strings.Contains(sql, contains) {
				continue
			}
			rows, err := pool.Query(ctx, "explain (costs off) "+sql, st.args...)
			require.NoError(t, err, sql)
			var plan []string
			for rows.Next() {
				var line string
				require.NoError(t, rows.Scan(&line))
				plan = append(plan, line)
			}
			require.NoError(t, rows.Err(), sql)
			return strings.Join(plan, "\n")
		}
		require.FailNow(t, "the store made no statement containing", contains)
		return ""
	}

	t.Run("the claim reads the runnable index", func(t *testing.T) {
		plan := planOf("skip locked")
		assert.Contains(t, plan, "agent_runs_runnable_idx", plan)
		assert.NotContains(t, plan, "Seq Scan on agent_runs", plan)
	})

	t.Run("a listing reads the created index and keeps the position in the index condition", func(t *testing.T) {
		plan := planOf("order by created_at desc")
		assert.Contains(t, plan, "agent_runs_created_idx", plan)
		assert.NotContains(t, plan, "Seq Scan", plan)
		assert.NotContains(t, plan, "Sort", "the index gives the order:\n%s", plan)
		assert.Regexp(t, `Index Cond: \(ROW\(created_at, id\) < ROW\(`, plan)
	})

	t.Run("the pending approvals read their index", func(t *testing.T) {
		plan := planOf("from agent_approvals where status =")
		assert.Contains(t, plan, "agent_approvals_pending_idx", plan)
		assert.NotContains(t, plan, "Seq Scan", plan)
		// The index is ordered by time and the id breaks a tie, so the sort
		// that is left is of equal times only.
		assert.Contains(t, plan, "Presorted Key: requested_at", plan)
	})
}
