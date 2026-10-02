package pg_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/agent/agenttest"
	agentpg "github.com/ManavA/keel/agent/pg"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// complete journals message as the reply of a first model step.
func (k *kit) complete(lease agent.Lease, message agent.Message) {
	k.t.Helper()
	require.NoError(k.t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
	require.NoError(k.t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
		Seq: 1, Message: message, Stop: agent.StopToolUse, Model: "model-a", Now: k.tick(),
	}))
}

func TestCompleteModel_OpaqueReadsBackWithItsKeysInTheOrderWritten(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.held()
	data := `{"z":1,"a":2}`

	k.complete(lease, agent.Message{
		Role:   agent.RoleAssistant,
		Text:   "thinking",
		Opaque: &agent.Opaque{Provider: "provider-a", Data: raw(data)},
	})

	step := k.steps(run.ID)[0]
	require.NotNil(t, step.Message)
	require.NotNil(t, step.Message.Opaque)
	assert.Equal(t, "provider-a", step.Message.Opaque.Provider)
	assert.Equal(t, data, string(step.Message.Opaque.Data), "z is before a, as the provider wrote it")

	changes, err := k.store.Changes(k.ctx, run.ID, 0)
	require.NoError(t, err)
	require.Len(t, changes.Steps, 1)
	assert.Equal(t, data, string(changes.Steps[0].Message.Opaque.Data), "and so it is in Changes")
}

// What a model or a provider wrote as JSON comes back as the bytes it wrote:
// with its spacing, its key order, its escapes, and the characters
// encoding/json would rewrite. A call's arguments are what the person who
// approves the call is shown and what the tool is then given.
func TestStore_KeepsRawJSONByteForByte(t *testing.T) {
	values := []struct {
		name string
		json string
	}{
		{"spaced as a model writes it", `{"to": "a",  "n": 2, "list": [1, 2,3]}`},
		{"keys out of order, and one twice", `{"z":1,"a":2,"z":3}`},
		{"characters encoding/json escapes", `{"html":"<a href=\"x\">&</a>","sep":" "}`},
		{"escapes as they were written", `{"e":"é","slash":"\/","nul":"\u0000","pair":"😀"}`},
		{"a number no float64 holds", `{"n":12345678901234567890.123456789,"e":1E400}`},
		{"new lines inside", "{\n\t\"a\": 1\n}"},
		{"a string", `"just text"`},
		{"null", `null`},
	}
	for _, v := range values {
		t.Run(v.name, func(t *testing.T) {
			k, _ := newKit(t)

			run := k.newRun(agentAlpha)
			run.Definition = agent.Snapshot{
				System: "system",
				Tools:  []agent.ToolSpec{{Name: toolSend, Schema: raw(v.json)}},
				Output: raw(v.json),
			}
			stored := k.insert(run)
			assert.Equal(t, v.json, string(stored.Definition.Tools[0].Schema), "a tool's schema, as returned")
			assert.Equal(t, v.json, string(stored.Definition.Output), "the output schema, as returned")
			read := k.run(run.ID)
			assert.Equal(t, v.json, string(read.Definition.Tools[0].Schema), "a tool's schema, as read")
			assert.Equal(t, v.json, string(read.Definition.Output), "the output schema, as read")

			lease := k.claim(workerA, run.ID)
			k.complete(lease, agent.Message{
				Role:   agent.RoleAssistant,
				Calls:  []agent.Call{{ID: "call-1", Name: toolSend, Input: raw(v.json)}},
				Opaque: &agent.Opaque{Provider: "provider-a", Data: raw(v.json)},
			})
			steps := k.steps(run.ID)
			require.Len(t, steps, 2)
			assert.Equal(t, v.json, string(steps[0].Message.Calls[0].Input), "the call in the reply")
			assert.Equal(t, v.json, string(steps[0].Message.Opaque.Data), "the provider's form of the reply")
			assert.Equal(t, v.json, string(steps[1].Call.Input), "the call on its own step")

			approval := k.ask(lease, 2, nil)
			assert.Equal(t, v.json, string(approval.Input), "the approval, as returned")
			assert.Equal(t, v.json, string(k.approval(approval.ID).Input), "the approval, as read")
		})
	}
}

// What is not JSON cannot be kept in a JSON column. It is refused for what it
// is, before the run is looked for and before the lease.
func TestStore_RefusesRawJSONThatIsNotJSON(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.held()
	require.NoError(t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
	k.lapse()
	current := k.claim(workerB, run.ID)
	before, journal := k.run(run.ID), k.steps(run.ID)

	bad := []struct {
		name string
		json string
	}{
		{"cut short", `{"id":`},
		{"not UTF-8", "{\"a\":\"caf\xff\"}"},
		{"a raw control character", "{\"a\":\"x\x00y\"}"},
		{"empty space", " "},
	}
	for _, b := range bad {
		messages := []struct {
			name    string
			message agent.Message
		}{
			{"a call's arguments", agent.Message{
				Role: agent.RoleAssistant, Calls: []agent.Call{{ID: "call-1", Name: toolSend, Input: raw(b.json)}},
			}},
			{"the provider's form", agent.Message{
				Role: agent.RoleAssistant, Opaque: &agent.Opaque{Provider: "provider-a", Data: raw(b.json)},
			}},
		}
		for _, m := range messages {
			// lease was lost to current, and the argument is judged first.
			for _, held := range []agent.Lease{lease, current, {RunID: newID(), Owner: workerA, Epoch: 1}} {
				err := k.store.CompleteModel(k.ctx, held, agent.CompleteModelRequest{
					Seq: 1, Message: m.message, Stop: agent.StopToolUse, Now: k.tick(),
				})
				require.Error(t, err, "%s, %s", m.name, b.name)
				assert.NotErrorIs(t, err, agent.ErrLeaseLost, "%s, %s", m.name, b.name)
				assert.NotErrorIs(t, err, agent.ErrNotFound, "%s, %s", m.name, b.name)
				assert.NotErrorIs(t, err, agent.ErrConflict, "%s, %s", m.name, b.name)
			}
		}

		created := k.newRun(agentAlpha)
		created.Definition.Output = raw(b.json)
		_, _, err := k.store.CreateRun(k.ctx, created)
		require.Error(t, err, "an output schema, %s", b.name)
		created.Definition = agent.Snapshot{Tools: []agent.ToolSpec{{Name: toolSend, Schema: raw(b.json)}}}
		_, _, err = k.store.CreateRun(k.ctx, created)
		require.Error(t, err, "a tool's schema, %s", b.name)
		_, err = k.store.GetRun(k.ctx, created.ID)
		require.ErrorIs(t, err, agent.ErrNotFound)
	}

	assert.Equal(t, before, k.run(run.ID))
	assert.Equal(t, journal, k.steps(run.ID))
}

// A call the model made with no arguments at all is kept as JSON's null, and
// reads back as that.
func TestCompleteModel_ACallWithNoInputReadsBackAsNull(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.held()

	k.complete(lease, agent.Message{
		Role:  agent.RoleAssistant,
		Calls: []agent.Call{{ID: "call-1", Name: toolSend}, {ID: "call-2", Name: toolSend, Input: json.RawMessage{}}},
	})

	steps := k.steps(run.ID)
	require.Len(t, steps, 3)
	for _, step := range steps[1:] {
		assert.Equal(t, "null", string(step.Call.Input))
	}
	assert.Equal(t, "null", string(steps[0].Message.Calls[0].Input))
	approval := k.ask(lease, 2, nil)
	assert.Equal(t, "null", string(approval.Input))
}

// Every part of a message is kept: the suite's cases carry a reply with
// calls, and this one carries the rest.
func TestCompleteModel_KeepsEveryPartOfAMessage(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.held()
	message := agent.Message{
		Role: agent.RoleAssistant,
		Text: "a \"quoted\" line\nand <b>markup</b> & more é",
		Calls: []agent.Call{
			{ID: "call-1", Name: toolSend, Input: raw(sendInput)},
			{ID: "call-2", Name: "lookup", Input: raw(`"{\"id\":"`), Malformed: true},
		},
		Results: []agent.Result{
			{CallID: "call-0", Content: "found"},
			{CallID: "call-00", Content: "", IsError: true},
		},
		Opaque: &agent.Opaque{Provider: "provider-a", Data: raw(`[{"type":"text"}]`)},
	}

	k.complete(lease, message)

	step := k.steps(run.ID)[0]
	require.NotNil(t, step.Message)
	assert.Equal(t, message, *step.Message)
	assert.Equal(t, agent.StopToolUse, step.Stop)
	assert.Equal(t, "model-a", step.Name)
}

// A seq is kept in a four-byte column. One that does not fit names no step,
// and is answered as any other seq that names none: after the lease, and
// never with the driver's complaint about the number.
func TestStore_ASeqNoColumnHoldsNamesNoStep(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.proposed()
	result := "never recorded"
	seqs := []int{math.MaxInt32 + 1, math.MinInt32 - 1, math.MaxInt64, -1, 0}

	calls := []struct {
		name string
		call func(lease agent.Lease, seq int) error
	}{
		{"BeginModel", func(lease agent.Lease, seq int) error { return k.store.BeginModel(k.ctx, lease, seq, k.tick()) }},
		{"CompleteModel", func(lease agent.Lease, seq int) error {
			return k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
				Seq: seq, Message: agenttest.Say("done").Message, Stop: agent.StopEnd, Now: k.tick(),
			})
		}},
		{"UpdateStep", func(lease agent.Lease, seq int) error {
			return k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
				Seq: seq, From: agent.StepProposed, To: agent.StepCompleted, Result: &result, Now: k.tick(),
			})
		}},
		{"RequestApproval", func(lease agent.Lease, seq int) error {
			_, err := k.store.RequestApproval(k.ctx, lease, k.askRequest(seq))
			return err
		}},
	}

	before, journal := k.run(run.ID), k.steps(run.ID)
	for _, c := range calls {
		for _, seq := range seqs {
			require.ErrorIs(t, c.call(lease, seq), agent.ErrConflict, "%s at %d", c.name, seq)
		}
	}
	assert.Equal(t, before, k.run(run.ID))
	assert.Equal(t, journal, k.steps(run.ID))

	k.lapse()
	k.claim(workerB, run.ID)
	for _, c := range calls {
		for _, seq := range seqs {
			require.ErrorIs(t, c.call(lease, seq), agent.ErrLeaseLost, "%s at %d, under a lease that was lost", c.name, seq)
		}
	}
}

// A cause the table's check would refuse is refused for what it is, in Go and
// before the lease, like every other argument.
func TestRequestApproval_ACauseThatIsNoneIsRefusedBeforeTheLease(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.proposed()
	k.lapse()
	current := k.claim(workerB, run.ID)
	before, journal := k.run(run.ID), k.steps(run.ID)

	for _, cause := range []agent.ApprovalCause{"", "policy", "Guard"} {
		for _, held := range []agent.Lease{lease, current, {RunID: newID(), Owner: workerA, Epoch: 1}} {
			req := k.askRequest(2)
			req.Cause = cause

			_, err := k.store.RequestApproval(k.ctx, held, req)

			require.Error(t, err, "cause %q", cause)
			assert.NotErrorIs(t, err, agent.ErrLeaseLost, "cause %q", cause)
			assert.NotErrorIs(t, err, agent.ErrNotFound, "cause %q", cause)
			assert.NotErrorIs(t, err, agent.ErrConflict, "cause %q", cause)
			_, err = k.store.GetApproval(k.ctx, req.ID)
			require.ErrorIs(t, err, agent.ErrNotFound)
		}
	}
	assert.Equal(t, before, k.run(run.ID))
	assert.Equal(t, journal, k.steps(run.ID))
}

// An approval's id already in use is a refusal that reads the store, so it
// comes after the lease, and it undoes nothing but itself.
func TestRequestApproval_AnIDAlreadyInUse(t *testing.T) {
	k, _ := newKit(t)
	_, otherLease := k.proposed()
	taken := k.ask(otherLease, 2, nil)

	run, lease := k.proposed()
	before, journal := k.run(run.ID), k.steps(run.ID)
	req := k.askRequest(2)
	req.ID = taken.ID

	_, err := k.store.RequestApproval(k.ctx, lease, req)
	require.ErrorContains(t, err, "already in use")
	assert.NotErrorIs(t, err, agent.ErrConflict)
	assert.NotErrorIs(t, err, agent.ErrLeaseLost)
	var fromDatabase *pgconn.PgError
	assert.NotErrorAs(t, err, &fromDatabase, "the refusal is the store's own, not a constraint's")
	assert.Equal(t, before, k.run(run.ID), "the run was not touched")
	assert.Equal(t, journal, k.steps(run.ID), "the step is still proposed")
	assert.Equal(t, taken, k.approval(taken.ID), "the approval that has the id is as it was")
	assert.Empty(t, k.approvals(run.ID))

	k.lapse()
	k.claim(workerB, run.ID)
	_, err = k.store.RequestApproval(k.ctx, lease, req)
	require.ErrorIs(t, err, agent.ErrLeaseLost, "the lease is judged before the id is looked for")
}

// A question about a step that is no tool step, or no step at all, is a
// refusal that reads the journal, so it comes after the lease.
func TestRequestApproval_AStepThatIsNoToolStepIsJudgedAfterTheLease(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.proposed()
	before, journal := k.run(run.ID), k.steps(run.ID)
	seqs := map[string]int{"a model step": 1, "no such step": 3}

	for name, seq := range seqs {
		req := k.askRequest(seq)
		req.From = agent.StepCompleted
		_, err := k.store.RequestApproval(k.ctx, lease, req)
		require.ErrorIs(t, err, agent.ErrConflict, name)
	}
	assert.Equal(t, before, k.run(run.ID))
	assert.Equal(t, journal, k.steps(run.ID))

	k.lapse()
	k.claim(workerB, run.ID)
	for name, seq := range seqs {
		req := k.askRequest(seq)
		req.From = agent.StepCompleted
		_, err := k.store.RequestApproval(k.ctx, lease, req)
		require.ErrorIs(t, err, agent.ErrLeaseLost, "%s, under a lease that was lost", name)
	}
}

// CompleteModel with calls on a step that is not the journal's last has
// nowhere to put them: the sequence numbers after it are taken.
func TestCompleteModel_CallsOnAStepThatIsNotTheLastAreAConflict(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.held()
	// Two model steps under way, which only a caller that is not the engine
	// would journal: the store checks a new step's place and nothing else.
	require.NoError(t, k.store.BeginModel(k.ctx, lease, 1, k.tick()))
	require.NoError(t, k.store.BeginModel(k.ctx, lease, 2, k.tick()))
	before, journal := k.run(run.ID), k.steps(run.ID)
	withCall := func(seq int) agent.CompleteModelRequest {
		return agent.CompleteModelRequest{
			Seq:     seq,
			Message: agenttest.Use(agenttest.Call("call-9", toolSend, sendInput)).Message,
			Stop:    agent.StopToolUse,
			Now:     k.tick(),
		}
	}

	err := k.store.CompleteModel(k.ctx, lease, withCall(1))
	require.ErrorIs(t, err, agent.ErrConflict)
	assert.Equal(t, before, k.run(run.ID))
	assert.Equal(t, journal, k.steps(run.ID))

	// Under a lease that was lost, the lease is what is wrong: it is judged
	// before the journal is read.
	k.lapse()
	current := k.claim(workerB, run.ID)
	require.ErrorIs(t, k.store.CompleteModel(k.ctx, lease, withCall(1)), agent.ErrLeaseLost)

	// A reply that makes no calls needs no room, and the last step has room.
	require.NoError(t, k.store.CompleteModel(k.ctx, current, agent.CompleteModelRequest{
		Seq: 1, Message: agenttest.Say("done").Message, Stop: agent.StopEnd, Now: k.tick(),
	}))
	require.NoError(t, k.store.CompleteModel(k.ctx, current, withCall(2)))
	assert.Len(t, k.steps(run.ID), 3)
}

// Every time a store hands back is in UTC, whatever zone the session, the
// server or the process is in, and is the instant it was given.
func TestStore_ReadsTimesBackInUTC(t *testing.T) {
	db := newDatabase(t)
	pool := db.pool(t, "timezone=America/Denver")
	k := kitOver(t, agentpg.New(pool))
	zone := time.FixedZone("elsewhere", 5*60*60+30*60)
	k.clock = agenttest.NewClock(testStart.In(zone))

	expires := k.clock.Now().Add(time.Hour)
	retry := k.clock.Now().Add(time.Minute)
	run, lease := k.proposed()
	approval := k.ask(lease, 2, &expires)
	require.NoError(t, k.store.Yield(k.ctx, lease, agent.YieldRequest{NextAttemptAt: &retry, Now: k.tick()}))
	k.clock.Advance(time.Minute)
	lease = k.claim(workerA, run.ID)
	decided := k.tick()
	_, err := k.store.DecideApproval(k.ctx, agent.DecideRequest{ID: approval.ID, Approved: true, By: person, Now: decided})
	require.NoError(t, err)
	require.NoError(t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
		Seq: 2, From: agent.StepWaiting, To: agent.StepStarted, Now: k.tick(),
	}))
	require.NoError(t, k.store.UpdateStep(k.ctx, lease, agent.StepUpdate{
		Seq: 2, From: agent.StepStarted, To: agent.StepCompleted, Now: k.tick(),
	}))
	held := k.run(run.ID)
	finished := k.tick()
	require.NoError(t, k.store.Finish(k.ctx, lease, agent.FinishRequest{Status: agent.StatusCompleted, Now: finished}))

	utc := func(what string, got time.Time) {
		t.Helper()
		assert.Same(t, time.UTC, got.Location(), what)
	}
	is := func(what string, want time.Time, got *time.Time) {
		t.Helper()
		if assert.NotNil(t, got, what) {
			utc(what, *got)
			assert.True(t, want.Equal(*got), "%s: want %s, got %s", what, want, *got)
		}
	}

	is("LeaseExpiresAt", decided.Add(-time.Millisecond).Add(testTTL), held.LeaseExpiresAt)
	is("NextAttemptAt", retry, held.NextAttemptAt)
	ended := k.run(run.ID)
	utc("CreatedAt", ended.CreatedAt)
	is("UpdatedAt", finished, &ended.UpdatedAt)
	is("FinishedAt", finished, ended.FinishedAt)

	changes, err := k.store.Changes(k.ctx, run.ID, 0)
	require.NoError(t, err)
	utc("Changes: Run.CreatedAt", changes.Run.CreatedAt)
	require.Len(t, changes.Steps, 2)
	for _, step := range changes.Steps {
		utc("step CreatedAt", step.CreatedAt)
		require.NotNil(t, step.FinishedAt)
		utc("step FinishedAt", *step.FinishedAt)
	}
	require.NotNil(t, changes.Steps[1].StartedAt)
	utc("step StartedAt", *changes.Steps[1].StartedAt)

	got := k.approval(approval.ID)
	utc("RequestedAt", got.RequestedAt)
	is("DecidedAt", decided, got.DecidedAt)
	is("ExpiresAt", expires, got.ExpiresAt)
}

// A time finer than a microsecond is kept to the microsecond below it, and
// the time a step spent working is its finish less its start as that was
// kept, in whole milliseconds, and less than nothing when the clock went
// back: it is a measurement, and the store does not correct it.
func TestStore_KeepsTimesToTheMicrosecond(t *testing.T) {
	k, _ := newKit(t)
	run, lease := k.held()
	started := testStart.Add(time.Hour + 999*time.Nanosecond)
	finished := testStart.Add(time.Hour + 1500*time.Millisecond + 500*time.Nanosecond)

	require.NoError(t, k.store.BeginModel(k.ctx, lease, 1, started))
	require.NoError(t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
		Seq: 1, Message: agenttest.Say("done").Message, Stop: agent.StopEnd, Now: finished,
	}))

	step := k.steps(run.ID)[0]
	require.NotNil(t, step.StartedAt)
	require.NotNil(t, step.FinishedAt)
	assert.True(t, testStart.Add(time.Hour).Equal(*step.StartedAt), "got %s", step.StartedAt)
	assert.True(t, testStart.Add(time.Hour+1500*time.Millisecond).Equal(*step.FinishedAt), "got %s", step.FinishedAt)
	assert.Equal(t, int64(1500), k.run(run.ID).ActiveMillis)

	// A step that finished before it started, by a clock that was set back.
	require.NoError(t, k.store.BeginModel(k.ctx, lease, 2, finished.Add(10*time.Second)))
	require.NoError(t, k.store.CompleteModel(k.ctx, lease, agent.CompleteModelRequest{
		Seq: 2, Message: agenttest.Say("done").Message, Stop: agent.StopEnd, Now: finished.Add(7500 * time.Millisecond),
	}))
	assert.Equal(t, int64(-1000), k.run(run.ID).ActiveMillis, "1500 and then 2500 less")
}
