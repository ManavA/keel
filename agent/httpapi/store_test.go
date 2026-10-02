package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

// storeRuns is a Runs over a Store alone: the three operations an engine
// would add to the store's reads are one call each. It lets the API be tried
// against the in-process store's own answers, the ones a fake can only copy.
type storeRuns struct {
	agent.Store
	now time.Time
}

func (s storeRuns) Approve(ctx context.Context, id, by, reason string) (agent.Approval, error) {
	return s.DecideApproval(ctx, agent.DecideRequest{ID: id, Approved: true, By: by, Reason: reason, Now: s.now})
}

func (s storeRuns) Decline(ctx context.Context, id, by, reason string) (agent.Approval, error) {
	return s.DecideApproval(ctx, agent.DecideRequest{ID: id, By: by, Reason: reason, Now: s.now})
}

func (s storeRuns) Cancel(ctx context.Context, runID, by, reason string) error {
	return s.RequestCancel(ctx, agent.CancelRequest{RunID: runID, By: by, Reason: reason, Now: s.now})
}

// post sends a POST with an actor and no body.
func post(t *testing.T, srv string, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv+path, strings.NewReader(`{"reason":"fine"}`))
	require.NoError(t, err)
	req.Header.Set("X-Actor", "sam")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

// A run is parked for approval, followed over the stream by a client that
// approves it over HTTP, and carried to its end. Every change reaches the
// stream, in the store's own revisions.
func TestOverTheMemoryStore_AnApprovalFromAskingToTheEnd(t *testing.T) {
	ctx := t.Context()
	store := agent.NewMemoryStore()

	_, _, err := store.CreateRun(ctx, agent.Run{
		ID: uid(1), Agent: "a", Status: agent.StatusRunnable, Input: "go", CreatedAt: at(0), UpdatedAt: at(0),
	}) // rev 1
	require.NoError(t, err)
	claim := func(now int) agent.Lease {
		run, err := store.Claim(ctx, agent.ClaimRequest{
			Owner: "worker", Agents: []string{"a"}, RunID: uid(1), Now: at(now), TTL: time.Minute,
		})
		require.NoError(t, err)
		require.NotNil(t, run)
		return run.Lease()
	}
	lease := claim(1)                                          // rev 2
	require.NoError(t, store.BeginModel(ctx, lease, 1, at(2))) // rev 3
	require.NoError(t, store.CompleteModel(ctx, lease, agent.CompleteModelRequest{
		Seq: 1, Model: "m", Stop: agent.StopToolUse, Now: at(3),
		Message: agent.Message{
			Role:   agent.RoleAssistant,
			Text:   "I will send it",
			Calls:  []agent.Call{{ID: "c1", Name: "send", Input: json.RawMessage(`{"to":"somebody"}`)}},
			Opaque: &agent.Opaque{Provider: "p", Data: json.RawMessage(`{"thinking":"` + secretThought + `"}`)},
		},
	})) // rev 4: the reply, and the call it proposes as step 2
	_, err = store.RequestApproval(ctx, lease, agent.ApprovalRequest{
		ID: uid(50), Seq: 2, From: agent.StepProposed, Cause: agent.CauseGuard,
		Action: agent.Action{Kind: "run", Target: "send"}, Decision: agent.Ask, Rule: "ask first", Now: at(4),
	}) // rev 5
	require.NoError(t, err)
	parked, err := store.Park(ctx, lease, agent.ParkRequest{Reason: agent.ReasonApproval, Now: at(5)}) // rev 6
	require.NoError(t, err)
	require.True(t, parked)

	runs := storeRuns{Store: store, now: at(6)}
	srv := serve(t, newAPI(t, runs, quick))
	waitForRev := func(s *liveStream, rev string) {
		t.Helper()
		for {
			ev := s.nextEvent()
			if ev.Type == "run" {
				require.Equal(t, rev, ev.ID)
				return
			}
		}
	}

	t.Run("the timeline and the listings", func(t *testing.T) {
		resp, body := get(t, srv.URL+"/runs/"+uid(1)+"/timeline")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotContains(t, body, secretThought)
		assert.Contains(t, body, "I will send it")

		resp2, body2 := get(t, srv.URL+"/approvals?status=pending")
		require.Equal(t, http.StatusOK, resp2.StatusCode)
		assert.Contains(t, body2, uid(50))

		_, body3 := get(t, srv.URL+"/runs?status=waiting")
		assert.Contains(t, body3, uid(1))
	})

	s := openStream(t, srv, stream1)
	assert.Equal(t, []string{"step 1", "step 2", "approval 50", "run rev 6"},
		[]string{label(t, s.nextEvent()), label(t, s.nextEvent()), label(t, s.nextEvent()), label(t, s.nextEvent())})

	resp, body := post(t, srv.URL, "/approvals/"+uid(50)+"/approve")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var decided agent.Approval
	require.NoError(t, json.Unmarshal([]byte(body), &decided))
	assert.Equal(t, agent.ApprovalApproved, decided.Status)
	assert.Equal(t, "sam", decided.DecidedBy)
	assert.Equal(t, "fine", decided.Reason)

	assert.Equal(t, []string{"approval 50", "run rev 7"},
		[]string{label(t, s.nextEvent()), label(t, s.nextEvent())})

	resp, _ = post(t, srv.URL, "/approvals/"+uid(50)+"/decline")
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "an approval is decided once")

	// The worker takes the run up again and finishes it.
	lease = claim(7) // rev 8
	waitForRev(s, "8")
	require.NoError(t, store.UpdateStep(ctx, lease, agent.StepUpdate{Seq: 2, From: agent.StepWaiting, To: agent.StepStarted, Now: at(8)})) // rev 9
	assert.Equal(t, "step 2", label(t, s.nextEvent()))
	waitForRev(s, "9")
	result := "sent"
	require.NoError(t, store.UpdateStep(ctx, lease, agent.StepUpdate{
		Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Result: &result, Now: at(9),
	})) // rev 10
	assert.Equal(t, "step 2", label(t, s.nextEvent()))
	waitForRev(s, "10")
	require.NoError(t, store.Finish(ctx, lease, agent.FinishRequest{Status: agent.StatusCompleted, Output: "done", Now: at(10)})) // rev 11
	assert.Equal(t, []string{"run rev 11", "end"}, labels(t, s.rest()))

	t.Run("a client that reconnects at the end is told it is over", func(t *testing.T) {
		again := openStream(t, srv, stream1, "Last-Event-ID", "11")
		assert.Equal(t, []string{"end"}, labels(t, again.rest()))
	})

	t.Run("what the store answers a run that has ended, and one that never was", func(t *testing.T) {
		resp, _ := post(t, srv.URL, "/runs/"+uid(1)+"/cancel")
		assert.Equal(t, http.StatusConflict, resp.StatusCode, "a run that has ended cannot be cancelled")

		for _, path := range []string{"/runs/not-an-id", "/runs/not-an-id/timeline", "/runs/not-an-id/events", "/runs/" + uid(9)} {
			resp, _ := get(t, srv.URL+path)
			assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
		}
		resp, _ = post(t, srv.URL, "/approvals/not-an-id/approve")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func get(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}
