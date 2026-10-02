package pg_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

// An id that is not a UUID in the one form names nothing, and the store says
// so without asking the database: no transaction is opened. The same goes
// for what a request gets wrong in itself.
func TestStore_AnswersForTheRequestAloneWithoutATransaction(t *testing.T) {
	store := agentpg.New(neverBegun{t})
	ctx := t.Context()
	canonical := newID()
	now := testStart
	result := "sent"

	for _, id := range []string{"no-such-id", "", strings.ToUpper(canonical), strings.ReplaceAll(canonical, "-", ""), "{" + canonical + "}"} {
		lease := agent.Lease{RunID: id, Owner: workerA, Epoch: 1}
		lookups := map[string]func() error{
			"GetRun": func() error { _, err := store.GetRun(ctx, id); return err },
			"Steps":  func() error { _, err := store.Steps(ctx, id); return err },
			"Changes": func() error {
				_, err := store.Changes(ctx, id, 0)
				return err
			},
			"RequestCancel": func() error {
				return store.RequestCancel(ctx, agent.CancelRequest{RunID: id, By: person, Now: now})
			},
			"GetApproval": func() error { _, err := store.GetApproval(ctx, id); return err },
			"DecideApproval": func() error {
				_, err := store.DecideApproval(ctx, agent.DecideRequest{ID: id, Approved: true, By: person, Now: now})
				return err
			},
			"Heartbeat": func() error { _, err := store.Heartbeat(ctx, lease, now, testTTL); return err },
			"Yield":     func() error { return store.Yield(ctx, lease, agent.YieldRequest{Now: now}) },
			"Park": func() error {
				_, err := store.Park(ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: now})
				return err
			},
			"Finish": func() error {
				return store.Finish(ctx, lease, agent.FinishRequest{Status: agent.StatusCompleted, Now: now})
			},
			"BeginModel": func() error { return store.BeginModel(ctx, lease, 1, now) },
			"CompleteModel": func() error {
				return store.CompleteModel(ctx, lease, agent.CompleteModelRequest{
					Seq: 1, Message: agenttest.Say("done").Message, Stop: agent.StopEnd, Now: now,
				})
			},
			"UpdateStep": func() error {
				return store.UpdateStep(ctx, lease, agent.StepUpdate{
					Seq: 2, From: agent.StepProposed, To: agent.StepCompleted, Result: &result, Now: now,
				})
			},
			"RequestApproval": func() error {
				_, err := store.RequestApproval(ctx, lease, agent.ApprovalRequest{
					ID: newID(), Seq: 2, From: agent.StepProposed, Cause: agent.CauseGuard, Now: now,
				})
				return err
			},
		}
		if id != "" {
			// With no RunID a claim names no run, and takes the oldest.
			lookups["Claim"] = func() error {
				_, err := store.Claim(ctx, agent.ClaimRequest{Owner: workerA, Agents: testAgents, RunID: id, Now: now, TTL: testTTL})
				return err
			}
		}
		for name, lookup := range lookups {
			require.ErrorIs(t, lookup(), agent.ErrNotFound, "%s, given %q", name, id)
		}

		if id == "" {
			// An empty filter is no filter.
			continue
		}
		runs, err := store.ListRuns(ctx, agent.RunFilter{ParentID: id})
		require.NoError(t, err)
		assert.Empty(t, runs, "ListRuns, given parent %q", id)
		approvals, err := store.ListApprovals(ctx, agent.ApprovalFilter{RunID: id})
		require.NoError(t, err)
		assert.Empty(t, approvals, "ListApprovals, given run %q", id)
	}

	lease := agent.Lease{RunID: canonical, Owner: workerA, Epoch: 1}
	refusals := map[string]func() error{
		"CreateRun with an id that is not a UUID": func() error {
			_, _, err := store.CreateRun(ctx, agent.Run{ID: "no-such-id", Agent: agentAlpha, Status: agent.StatusRunnable})
			return err
		},
		"CreateRun with a status that is not runnable": func() error {
			_, _, err := store.CreateRun(ctx, agent.Run{ID: canonical, Agent: agentAlpha, Status: agent.StatusWaiting})
			return err
		},
		"CreateRun with a depth no column holds": func() error {
			_, _, err := store.CreateRun(ctx, agent.Run{ID: canonical, Agent: agentAlpha, Status: agent.StatusRunnable, Depth: 1 << 40})
			return err
		},
		"Claim with no owner": func() error {
			_, err := store.Claim(ctx, agent.ClaimRequest{Agents: testAgents, Now: now, TTL: testTTL})
			return err
		},
		"Claim with no TTL": func() error {
			_, err := store.Claim(ctx, agent.ClaimRequest{Owner: workerA, Agents: testAgents, Now: now})
			return err
		},
		"Heartbeat with no TTL": func() error { _, err := store.Heartbeat(ctx, lease, now, 0); return err },
		"Finish with a status that is not final": func() error {
			return store.Finish(ctx, lease, agent.FinishRequest{Status: agent.StatusRunnable, Now: now})
		},
		"UpdateStep with a To that is no status": func() error {
			return store.UpdateStep(ctx, lease, agent.StepUpdate{Seq: 2, From: agent.StepProposed, To: "done", Now: now})
		},
		"UpdateStep with a child run id that is not a UUID": func() error {
			return store.UpdateStep(ctx, lease, agent.StepUpdate{
				Seq: 2, From: agent.StepStarted, To: agent.StepWaiting, ChildRunID: "no-such-id", Now: now,
			})
		},
		"RequestApproval with an id that is not a UUID": func() error {
			_, err := store.RequestApproval(ctx, lease, agent.ApprovalRequest{ID: "no-such-id", Seq: 2, Cause: agent.CauseGuard, Now: now})
			return err
		},
		"RequestApproval with a cause that is none": func() error {
			_, err := store.RequestApproval(ctx, lease, agent.ApprovalRequest{ID: newID(), Seq: 2, Cause: "policy", Now: now})
			return err
		},
		"RequestApproval with attributes JSON cannot hold": func() error {
			_, err := store.RequestApproval(ctx, lease, agent.ApprovalRequest{
				ID: newID(), Seq: 2, Cause: agent.CauseGuard, Now: now,
				Action: agent.Action{Kind: "run", Attrs: map[string]any{"reply": make(chan string)}},
			})
			return err
		},
		"RequestApproval with a number in its attributes that no float64 holds": func() error {
			_, err := store.RequestApproval(ctx, lease, agent.ApprovalRequest{
				ID: newID(), Seq: 2, Cause: agent.CauseGuard, Now: now,
				Action: agent.Action{Kind: "run", Attrs: map[string]any{"input": raw(`{"n":1e400000}`)}},
			})
			return err
		},
		"RequestApproval with raw attributes that are not JSON": func() error {
			_, err := store.RequestApproval(ctx, lease, agent.ApprovalRequest{
				ID: newID(), Seq: 2, Cause: agent.CauseGuard, Now: now,
				Action: agent.Action{Kind: "run", Attrs: map[string]any{"input": raw(`{"n":`)}},
			})
			return err
		},
		"CompleteModel with arguments that are not JSON": func() error {
			return store.CompleteModel(ctx, lease, agent.CompleteModelRequest{
				Seq: 1, Message: agent.Message{Calls: []agent.Call{{ID: "c", Name: toolSend, Input: raw(`{"a":`)}}}, Now: now,
			})
		},
	}
	for name, refuse := range refusals {
		err := refuse()
		require.Error(t, err, name)
		for _, sentinel := range []error{agent.ErrNotFound, agent.ErrLeaseLost, agent.ErrConflict, agent.ErrNotClaimable} {
			assert.NotErrorIs(t, err, sentinel, name)
		}
		var fromDatabase *pgconn.PgError
		assert.NotErrorAs(t, err, &fromDatabase, name)
	}
}

// Rows are read with their id as text, under the column's own name, and an
// order by that names a bare id then sorts that text: in the database's
// collation, and with a sort where the index would have served. Every
// statement the store makes is planned here, and none may sort an id as
// text.
func TestStore_NoStatementSortsAnIDAsText(t *testing.T) {
	pool := newDatabase(t).pool(t)
	var (
		mu   sync.Mutex
		made = map[string][]any{}
	)
	store := agentpg.New(stepped{Beginner: pool, before: func(st statement) {
		mu.Lock()
		defer mu.Unlock()
		if st.sql != commitStep {
			made[st.sql] = st.args
		}
	}})
	k := kitOver(t, store)

	// A life that makes every statement the store has.
	due := k.clock.Now().Add(time.Hour)
	parent, parentLease := k.proposed()
	asked := k.ask(parentLease, 2, &due)
	child := k.createChild(parent)
	childLease := k.claim(workerB, child.ID)
	k.reply(childLease)
	_, err := k.store.Heartbeat(k.ctx, childLease, k.tick(), testTTL)
	require.NoError(t, err)
	k.park(parentLease, agent.ReasonApproval)
	k.finish(childLease, agent.StatusCompleted)
	taken, err := k.store.Claim(k.ctx, agent.ClaimRequest{Owner: workerA, Agents: testAgents, Now: k.tick(), TTL: testTTL})
	require.NoError(t, err)
	require.NotNil(t, taken)
	require.NoError(t, k.store.Yield(k.ctx, taken.Lease(), agent.YieldRequest{Now: k.tick()}))
	n, err := k.store.ExpireApprovals(k.ctx, due)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, err = k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: asked.ID, By: person, Now: k.tick()})
	require.ErrorIs(t, err, agent.ErrAlreadyDecided)
	_, second := k.parked(nil)
	_, err = k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: second.ID, Approved: true, By: person, Now: k.tick()})
	require.NoError(t, err)
	lease := k.claim(workerA, parent.ID)
	require.NoError(t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
		Seq: 2, From: agent.StepWaiting, To: agent.StepDeclined, Now: k.tick(),
	}))
	require.NoError(t, k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: parent.ID, By: person, Now: k.tick()}))
	_, err = k.store.Changes(k.ctx, parent.ID, 0)
	require.NoError(t, err)
	_, err = k.store.ListRuns(k.ctx, agent.RunFilter{
		Status: agent.StatusRunnable, Agent: agentAlpha, ParentID: parent.ID,
		Before: &agent.Cursor{CreatedAt: k.clock.Now(), ID: parent.ID},
	})
	require.NoError(t, err)
	_, err = k.store.ListRuns(k.ctx, agent.RunFilter{})
	require.NoError(t, err)
	_, err = k.store.ListApprovals(k.ctx, agent.ApprovalFilter{Status: agent.ApprovalExpired, RunID: parent.ID})
	require.NoError(t, err)
	_, err = k.store.ListApprovals(k.ctx, agent.ApprovalFilter{})
	require.NoError(t, err)
	k.approval(asked.ID)
	k.run(parent.ID)
	k.finish(lease, agent.StatusCancelled)
	again := k.newRun(agentAlpha)
	again.Key = "start-1"
	k.insert(again)
	_, created, err := k.store.CreateRun(k.ctx, again)
	require.NoError(t, err)
	require.False(t, created)
	_, err = store.Purge(k.ctx, k.clock.Now())
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(made), 40, "the life made fewer statements than the store has")
	ordered := 0
	for sql, args := range made {
		if strings.Contains(sql, "order by") {
			ordered++
		}
		rows, err := pool.Query(k.ctx, "explain (costs off) "+sql, args...)
		require.NoError(t, err, sql)
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			if strings.Contains(line, "Sort Key") {
				assert.NotContains(t, line, "::text", "this statement sorts an id as text:\n%s", sql)
			}
		}
		require.NoError(t, rows.Err(), sql)
	}
	assert.GreaterOrEqual(t, ordered, 9, "the statements that sort")
	t.Logf("%d statements planned, %d of them ordered", len(made), ordered)
}
