package agenttest

import (
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
)

func createRunCases() []storeCase {
	return []storeCase{
		{"stores the run as given, with Rev 1", func(k *kit) {
			parent := k.create(agentAlpha)
			run := k.newRun(agentBeta)
			run.Input = lookupInput
			run.ParentID, run.ParentSeq, run.Depth = parent.ID, 2, 1
			run.Key = agent.StepKey(parent.ID, 2)
			run.Definition = agent.Snapshot{
				System: "Review one document.",
				Model:  suiteModel,
				Tools: []agent.ToolSpec{
					{Name: toolLookup, Description: "Looks a thing up.", Schema: raw(`{"type":"object"}`)},
					{Name: toolSend},
				},
				Output:    raw(`{"type":"string"}`),
				MaxTokens: 1024,
				Limits: agent.Limits{
					MaxDuration: 15 * time.Minute, MaxCostMicros: 5_000_000, MaxTokens: -1, MaxModelCalls: 50,
				},
			}
			run.Metadata = map[string]string{"batch": "7", "source": "upload"}
			run.Rev = 9

			stored, created, err := k.store.CreateRun(k.ctx, run)
			require.NoError(k.t, err)
			assert.True(k.t, created)

			want := run
			want.Rev = 1
			k.equalRun(want, stored)
			k.equalRun(want, k.run(run.ID))
		}},
		{"stores a run that has nothing optional set, and reads its metadata back empty", func(k *kit) {
			run := k.newRun(agentAlpha)
			require.Nil(k.t, run.Metadata)

			stored, created, err := k.store.CreateRun(k.ctx, run)
			require.NoError(k.t, err)
			assert.True(k.t, created)

			// No metadata reads back as none, and never as nil, so a test
			// that compares it says the same of every store.
			want := run
			want.Rev, want.Metadata = 1, map[string]string{}
			k.equalRun(want, stored)
			k.equalRun(want, k.run(run.ID))
		}},
		{"a second create with the same agent and key returns the first", func(k *kit) {
			first := k.newRun(agentAlpha)
			first.Key, first.Input = suiteStartKey, "first"
			stored := k.insert(first)

			second := k.newRun(agentAlpha)
			second.Key, second.Input = suiteStartKey, "second"
			got, created, err := k.store.CreateRun(k.ctx, second)

			require.NoError(k.t, err)
			assert.False(k.t, created)
			k.equalRun(stored, got)
			k.equalRun(stored, k.run(first.ID))
			_, err = k.store.GetRun(k.ctx, second.ID)
			assert.ErrorIs(k.t, err, agent.ErrNotFound, "the second run was not stored")
		}},
		{"a create that finds its key returns the run as it now stands", func(k *kit) {
			first := k.newRun(agentAlpha)
			first.Key = suiteStartKey
			k.insert(first)
			k.claim(workerA, first.ID)
			standing := k.run(first.ID)

			second := k.newRun(agentAlpha)
			second.Key = suiteStartKey
			got, created, err := k.store.CreateRun(k.ctx, second)

			require.NoError(k.t, err)
			assert.False(k.t, created)
			k.equalRun(standing, got)
			assert.Equal(k.t, workerA, got.LeaseOwner)
		}},
		{"the same key under another agent creates", func(k *kit) {
			first := k.newRun(agentAlpha)
			first.Key = suiteStartKey
			k.insert(first)

			second := k.newRun(agentBeta)
			second.Key = suiteStartKey
			got, created, err := k.store.CreateRun(k.ctx, second)

			require.NoError(k.t, err)
			assert.True(k.t, created)
			assert.Equal(k.t, second.ID, got.ID)
			assert.Equal(k.t, second.ID, k.run(second.ID).ID)
		}},
		{"runs with no key never stand for one another", func(k *kit) {
			first := k.create(agentAlpha)
			second := k.create(agentAlpha)

			assert.NotEqual(k.t, first.ID, second.ID)
			assert.Equal(k.t, first.ID, k.run(first.ID).ID)
			assert.Equal(k.t, second.ID, k.run(second.ID).ID)
		}},
		{"a status other than runnable is refused, and nothing is stored", func(k *kit) {
			statuses := append([]agent.Status{agent.StatusWaiting, ""}, finalStatuses...)
			for _, status := range statuses {
				run := k.newRun(agentAlpha)
				run.Status = status

				_, created, err := k.store.CreateRun(k.ctx, run)

				require.Error(k.t, err, "status %q", status)
				assert.False(k.t, created, "status %q", status)
				_, err = k.store.GetRun(k.ctx, run.ID)
				assert.ErrorIs(k.t, err, agent.ErrNotFound, "status %q", status)
			}
		}},
		{"a parent that does not exist is refused", func(k *kit) {
			run := k.newRun(agentBeta)
			run.ParentID, run.ParentSeq, run.Depth = uuid.NewString(), 2, 1

			_, created, err := k.store.CreateRun(k.ctx, run)

			require.Error(k.t, err)
			assert.False(k.t, created)
			_, err = k.store.GetRun(k.ctx, run.ID)
			assert.ErrorIs(k.t, err, agent.ErrNotFound, "the child was not stored")
		}},
		{"an id or a parent id that is not a UUID is refused", func(k *kit) {
			parent := k.create(agentAlpha)
			valid := k.newRun(agentBeta)

			badID := valid
			badID.ID = malformedID
			_, created, err := k.store.CreateRun(k.ctx, badID)
			require.Error(k.t, err, "the run's own id")
			assert.False(k.t, created)

			badParent := valid
			badParent.ParentID = malformedID
			_, created, err = k.store.CreateRun(k.ctx, badParent)
			require.Error(k.t, err, "its parent's id")
			assert.False(k.t, created)
			_, err = k.store.GetRun(k.ctx, valid.ID)
			assert.ErrorIs(k.t, err, agent.ErrNotFound, "nothing was stored")

			// The same run under ids a store can keep is accepted.
			valid.ParentID = parent.ID
			k.insert(valid)
		}},
		{"another spelling of a UUID is refused, as the run's id and as its parent's", func(k *kit) {
			parent := k.create(agentAlpha)
			for _, other := range otherSpellings {
				asID := k.newRun(agentBeta)
				canonical := asID.ID
				asID.ID = other.spell(canonical)
				_, created, err := k.store.CreateRun(k.ctx, asID)
				require.Error(k.t, err, "the run's id %s", other.name)
				assert.False(k.t, created)
				_, err = k.store.GetRun(k.ctx, canonical)
				assert.ErrorIs(k.t, err, agent.ErrNotFound, "it was not stored under the form a database would keep")

				// The parent exists, under the one spelling of its id.
				asParent := k.newRun(agentBeta)
				asParent.ParentID = other.spell(parent.ID)
				_, created, err = k.store.CreateRun(k.ctx, asParent)
				require.Error(k.t, err, "the parent's id %s", other.name)
				assert.False(k.t, created)
				_, err = k.store.GetRun(k.ctx, asParent.ID)
				assert.ErrorIs(k.t, err, agent.ErrNotFound, "the child was not stored")
			}
		}},
		{"reads metadata back as JSON gives it", func(k *kit) {
			run := k.newRun(agentAlpha)
			// A byte that is no UTF-8 cannot be kept as text, and comes
			// back as the replacement character JSON writes for it.
			run.Metadata = map[string]string{"note": "caf\xff", "markup": "<a href=\"x\">&</a>", "empty": ""}
			want := map[string]string{"note": "caf\ufffd", "markup": "<a href=\"x\">&</a>", "empty": ""}

			stored, _, err := k.store.CreateRun(k.ctx, run)

			require.NoError(k.t, err)
			assert.Equal(k.t, want, stored.Metadata, "as returned")
			assert.Equal(k.t, want, k.run(run.ID).Metadata, "as read")
		}},
		{"an id already taken is refused, and the run that has it is left alone", func(k *kit) {
			first := k.create(agentAlpha)

			second := k.newRun(agentBeta)
			second.ID, second.Input = first.ID, "another"
			_, created, err := k.store.CreateRun(k.ctx, second)

			require.Error(k.t, err)
			assert.False(k.t, created)
			k.equalRun(first, k.run(first.ID))
		}},
	}
}

func listRunsCases() []storeCase {
	list := func(k *kit, f agent.RunFilter) []string {
		k.t.Helper()
		runs, err := k.store.ListRuns(k.ctx, f)
		require.NoError(k.t, err)
		return runIDs(runs)
	}
	return []storeCase{
		{"an empty store lists nothing", func(k *kit) {
			assert.Empty(k.t, list(k, agent.RunFilter{}))
		}},
		{"lists every run, newest first", func(k *kit) {
			first := k.newRun(agentAlpha)
			second := k.newRun(agentBeta)
			third := k.newRun(agentAlpha)
			// Stored out of order, so the order listed is by CreatedAt and
			// not by arrival.
			k.insert(second)
			k.insert(third)
			k.insert(first)

			runs, err := k.store.ListRuns(k.ctx, agent.RunFilter{})

			require.NoError(k.t, err)
			assert.Equal(k.t, []string{third.ID, second.ID, first.ID}, runIDs(runs))
			k.equalRun(k.run(third.ID), runs[0])
		}},
		{"runs created at one instant are listed by id, descending", func(k *kit) {
			older := k.create(agentAlpha)
			instant := k.tick()
			var same []string
			for range 5 {
				run := k.newRun(agentAlpha)
				run.CreatedAt, run.UpdatedAt = instant, instant
				k.insert(run)
				same = append(same, run.ID)
			}
			newer := k.create(agentAlpha)
			slices.Sort(same)
			slices.Reverse(same)

			want := append(append([]string{newer.ID}, same...), older.ID)
			assert.Equal(k.t, want, list(k, agent.RunFilter{}))
		}},
		{"narrows by status, by agent and by parent", func(k *kit) {
			runnable := k.create(agentAlpha)
			waiting, _ := k.parked(agentAlpha, nil)
			completed := k.ended(agentBeta, agent.StatusCompleted)
			failed := k.ended(agentBeta, agent.StatusFailed)
			cancelled := k.ended(agentAlpha, agent.StatusCancelled)
			child := k.createChild(runnable)

			tests := []struct {
				name   string
				filter agent.RunFilter
				want   []string
			}{
				{"runnable", agent.RunFilter{Status: agent.StatusRunnable}, []string{child.ID, runnable.ID}},
				{"waiting", agent.RunFilter{Status: agent.StatusWaiting}, []string{waiting.ID}},
				{"completed", agent.RunFilter{Status: agent.StatusCompleted}, []string{completed.ID}},
				{"failed", agent.RunFilter{Status: agent.StatusFailed}, []string{failed.ID}},
				{"cancelled", agent.RunFilter{Status: agent.StatusCancelled}, []string{cancelled.ID}},
				{"one agent", agent.RunFilter{Agent: agentBeta}, []string{child.ID, failed.ID, completed.ID}},
				{"one parent", agent.RunFilter{ParentID: runnable.ID}, []string{child.ID}},
				{"a parent with no children", agent.RunFilter{ParentID: waiting.ID}, []string{}},
				{"an agent and a status together", agent.RunFilter{Agent: agentBeta, Status: agent.StatusRunnable}, []string{child.ID}},
				{"an agent with no runs", agent.RunFilter{Agent: "gamma"}, []string{}},
				{"a parent id nothing has", agent.RunFilter{ParentID: uuid.NewString()}, []string{}},
				{"a parent id that is not a UUID", agent.RunFilter{ParentID: malformedID}, []string{}},
			}
			for _, tt := range tests {
				assert.Equal(k.t, tt.want, list(k, tt.filter), tt.name)
			}
		}},
		{"a parent's id in another spelling lists nothing", func(k *kit) {
			parent := k.create(agentAlpha)
			child := k.createChild(parent)
			require.Equal(k.t, []string{child.ID}, list(k, agent.RunFilter{ParentID: parent.ID}))

			for _, other := range otherSpellings {
				assert.Empty(k.t, list(k, agent.RunFilter{ParentID: other.spell(parent.ID)}), other.name)
			}
		}},
		{"returns 50 by default and never more than 200", func(k *kit) {
			const total = 205
			all := make([]string, total)
			for i := range total {
				all[total-1-i] = k.create(agentAlpha).ID
			}

			tests := []struct {
				name  string
				limit int
				want  int
			}{
				{"no limit given", 0, 50},
				{"a negative limit", -1, 50},
				{"a small limit", 3, 3},
				{"the most allowed", 200, 200},
				{"more than allowed", 1000, 200},
			}
			for _, tt := range tests {
				assert.Equal(k.t, all[:tt.want], list(k, agent.RunFilter{Limit: tt.limit}), tt.name)
			}
		}},
		{"Before pages through the listing with no gap and no repeat", func(k *kit) {
			// Seven runs at four instants, so that a page ends in the middle
			// of runs the cursor's time alone cannot tell apart.
			for _, n := range []int{1, 3, 2, 1} {
				instant := k.tick()
				for range n {
					run := k.newRun(agentAlpha)
					run.CreatedAt, run.UpdatedAt = instant, instant
					k.insert(run)
				}
			}
			all, err := k.store.ListRuns(k.ctx, agent.RunFilter{})
			require.NoError(k.t, err)
			require.Len(k.t, all, 7)

			var paged []string
			var before *agent.Cursor
			for range 4 {
				page, err := k.store.ListRuns(k.ctx, agent.RunFilter{Before: before, Limit: 2})
				require.NoError(k.t, err)
				if len(page) == 0 {
					break
				}
				paged = append(paged, runIDs(page)...)
				last := page[len(page)-1]
				before = &agent.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
			}

			assert.Equal(k.t, runIDs(all), paged)
			end, err := k.store.ListRuns(k.ctx, agent.RunFilter{Before: before, Limit: 2})
			require.NoError(k.t, err)
			assert.Empty(k.t, end, "nothing is older than the oldest run")
		}},
		{"Before narrows what the filters leave", func(k *kit) {
			first := k.create(agentAlpha)
			k.create(agentBeta)
			third := k.create(agentAlpha)
			fourth := k.create(agentBeta)

			got := list(k, agent.RunFilter{
				Agent:  agentAlpha,
				Before: &agent.Cursor{CreatedAt: fourth.CreatedAt, ID: fourth.ID},
			})
			assert.Equal(k.t, []string{third.ID, first.ID}, got)

			got = list(k, agent.RunFilter{
				Agent:  agentAlpha,
				Before: &agent.Cursor{CreatedAt: third.CreatedAt, ID: third.ID},
			})
			assert.Equal(k.t, []string{first.ID}, got)
		}},
	}
}

func notFoundCases() []storeCase {
	// Each call names a run or an approval by id, and is otherwise one the
	// store would accept.
	calls := []struct {
		name string
		// approval says the id names an approval, where the others name a
		// run.
		approval bool
		call     func(k *kit, id string) error
	}{
		{"GetRun", false, func(k *kit, id string) error {
			_, err := k.store.GetRun(k.ctx, id)
			return err
		}},
		{"Claim", false, func(k *kit, id string) error {
			run, err := k.store.Claim(k.ctx, agent.ClaimRequest{
				Owner: workerA, Agents: suiteAgents, RunID: id, Now: k.tick(), TTL: suiteTTL,
			})
			assert.Nil(k.t, run)
			return err
		}},
		{"Heartbeat", false, func(k *kit, id string) error {
			_, err := k.store.Heartbeat(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1}, k.tick(), suiteTTL)
			return err
		}},
		{"Yield", false, func(k *kit, id string) error {
			return k.store.Yield(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1}, agent.YieldRequest{Now: k.tick()})
		}},
		{"Park", false, func(k *kit, id string) error {
			parked, err := k.store.Park(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1},
				agent.ParkRequest{Reason: agent.ReasonApproval, Now: k.tick()})
			assert.False(k.t, parked)
			return err
		}},
		{"Finish", false, func(k *kit, id string) error {
			return k.store.Finish(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1},
				agent.FinishRequest{Status: agent.StatusCompleted, Now: k.tick()})
		}},
		{"Steps", false, func(k *kit, id string) error {
			_, err := k.store.Steps(k.ctx, id)
			return err
		}},
		{"BeginModel", false, func(k *kit, id string) error {
			return k.store.BeginModel(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1}, 1, k.tick())
		}},
		{"CompleteModel", false, func(k *kit, id string) error {
			return k.store.CompleteModel(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1},
				agent.CompleteModelRequest{Seq: 1, Message: Say("done").Message, Stop: agent.StopEnd, Now: k.tick()})
		}},
		{"UpdateStep", false, func(k *kit, id string) error {
			return k.store.UpdateStep(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1},
				agent.StepUpdate{Seq: 2, From: agent.StepProposed, To: agent.StepStarted, Now: k.tick()})
		}},
		{"RequestApproval", false, func(k *kit, id string) error {
			_, err := k.store.RequestApproval(k.ctx, agent.Lease{RunID: id, Owner: workerA, Epoch: 1}, k.askRequest(2))
			return err
		}},
		{"GetApproval", true, func(k *kit, id string) error {
			_, err := k.store.GetApproval(k.ctx, id)
			return err
		}},
		{"DecideApproval", true, func(k *kit, id string) error {
			_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: id, Approved: true, By: personA, Now: k.tick()})
			return err
		}},
		{"RequestCancel", false, func(k *kit, id string) error {
			return k.store.RequestCancel(k.ctx, agent.CancelRequest{RunID: id, By: personA, Now: k.tick()})
		}},
		{"Changes", false, func(k *kit, id string) error {
			_, err := k.store.Changes(k.ctx, id, 0)
			return err
		}},
	}
	// Each id names nothing. The last two would name the run, or the
	// approval, to a database, which reads a UUID in any spelling: a store
	// knows an id by the one string it was given.
	ids := []struct {
		name string
		id   func(run agent.Run, approval agent.Approval, forApproval bool) string
	}{
		{"an id nothing has", func(agent.Run, agent.Approval, bool) string { return newID() }},
		{"an id no store could have issued", func(agent.Run, agent.Approval, bool) string { return malformedID }},
		{"the upper-case spelling of an id that exists", func(run agent.Run, approval agent.Approval, forApproval bool) string {
			if forApproval {
				return upperCase(approval.ID)
			}
			return upperCase(run.ID)
		}},
		{"the spelling without hyphens of an id that exists", func(run agent.Run, approval agent.Approval, forApproval bool) string {
			if forApproval {
				return noHyphens(approval.ID)
			}
			return noHyphens(run.ID)
		}},
	}

	var cases []storeCase
	for _, c := range calls {
		for _, id := range ids {
			cases = append(cases, storeCase{c.name + ", given " + id.name, func(k *kit) {
				// A store that holds something, so that not found is about
				// the id and not about an empty store.
				run, approval := k.parked(agentAlpha, nil)
				before := k.snapshot(run.ID)

				err := c.call(k, id.id(run, approval, c.approval))

				require.ErrorIs(k.t, err, agent.ErrNotFound)
				k.unchanged(before)
			}})
		}
	}
	return cases
}
