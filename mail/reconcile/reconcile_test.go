package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/jobs"
)

// memStore is a Store backed by a slice, so the tests seed sends and read
// back recorded outcomes without a database.
type memStore struct {
	sends     []Send
	outcomes  map[string]Status
	recordErr map[string]error
	listErr   error
}

func (m *memStore) RecentSends(_ context.Context, _ int) ([]Send, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.sends, nil
}

func (m *memStore) RecordOutcome(_ context.Context, id string, status Status) error {
	if err := m.recordErr[id]; err != nil {
		return err
	}
	if m.outcomes == nil {
		m.outcomes = make(map[string]Status)
	}
	m.outcomes[id] = status
	return nil
}

// fakeProvider answers each message id from a map, so one send can bounce
// while its neighbor confirms.
type fakeProvider struct {
	statuses map[string]Status
	errs     map[string]error
}

func (f *fakeProvider) Status(_ context.Context, messageID string) (Status, error) {
	if err := f.errs[messageID]; err != nil {
		return StatusUnknown, err
	}
	if s, ok := f.statuses[messageID]; ok {
		return s, nil
	}
	return StatusUnknown, nil
}

// TestABounceIsRecordedAndTheOutcomeReflectsWorkDone is the acceptance check:
// seeded sends, a provider reporting one bounce, and the run records it with
// an outcome that reads as work done rather than as an empty success.
func TestABounceIsRecordedAndTheOutcomeReflectsWorkDone(t *testing.T) {
	store := &memStore{sends: []Send{
		{ID: "1", MessageID: "msg-1", Recipient: "buyer@example.com"},
		{ID: "2", MessageID: "msg-2", Recipient: "owner@example.com"},
	}}
	provider := &fakeProvider{statuses: map[string]Status{
		"msg-1": StatusConfirmed,
		"msg-2": StatusBounced,
	}}

	job, err := New(Options{Store: store, Provider: provider})
	require.NoError(t, err)

	outcome, err := job.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusConfirmed, store.outcomes["1"])
	assert.Equal(t, StatusBounced, store.outcomes["2"])
	assert.Equal(t, 2, outcome.Attempted)
	assert.Equal(t, 2, outcome.Succeeded)
	assert.Equal(t, jobs.StatusSuccess, outcome.Status())
}

// TestAnEmptyCheckRunReportsDidNothing: a run that recorded nothing must not
// read as success. An empty send log and a broken one look identical from
// here, so both take the same non-zero exit.
func TestAnEmptyCheckRunReportsDidNothing(t *testing.T) {
	job, err := New(Options{Store: &memStore{}, Provider: &fakeProvider{}})
	require.NoError(t, err)

	outcome, err := job.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, jobs.StatusDidNothing, outcome.Status())
	assert.False(t, outcome.OK())
	assert.Equal(t, 1, outcome.ExitCode())
}

// TestOnlyComplainedUnknownAndFailedSends: complained counts as recorded work
// alongside confirmed; a send the provider has no verdict for stays
// unrecorded for a later run; a send the provider errors on counts as failed
// without failing the whole run.
func TestOnlyComplainedUnknownAndFailedSends(t *testing.T) {
	store := &memStore{sends: []Send{
		{ID: "1", MessageID: "msg-confirmed", Recipient: "a@example.com"},
		{ID: "2", MessageID: "msg-complained", Recipient: "b@example.com"},
		{ID: "3", MessageID: "msg-pending", Recipient: "c@example.com"},
		{ID: "4", MessageID: "msg-broken", Recipient: "d@example.com"},
	}}
	provider := &fakeProvider{
		statuses: map[string]Status{
			"msg-confirmed":  StatusConfirmed,
			"msg-complained": StatusComplained,
		},
		errs: map[string]error{"msg-broken": errors.New("provider timeout")},
	}

	job, err := New(Options{Store: store, Provider: provider})
	require.NoError(t, err)

	outcome, err := job.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusConfirmed, store.outcomes["1"])
	assert.Equal(t, StatusComplained, store.outcomes["2"])
	assert.NotContains(t, store.outcomes, "3", "unknown stays unrecorded for a later run")
	assert.Equal(t, 4, outcome.Attempted)
	assert.Equal(t, 2, outcome.Succeeded)
	assert.Equal(t, 1, outcome.Failed)
	assert.Equal(t, jobs.StatusPartial, outcome.Status())
}

func TestNewRefusesAnIncompleteJob(t *testing.T) {
	_, err := New(Options{Provider: &fakeProvider{}})
	assert.Error(t, err)
	_, err = New(Options{Store: &memStore{}})
	assert.Error(t, err)
}

// TestABrokenSendLogFailsTheRun: a send list the job cannot read fails the
// run outright. Without the authoritative send set, "measured, and fine" is a
// claim this run cannot make, so it reports Fatal rather than a failure
// count.
func TestABrokenSendLogFailsTheRun(t *testing.T) {
	store := &memStore{listErr: errors.New("send log unavailable")}
	job, err := New(Options{Store: store, Provider: &fakeProvider{}})
	require.NoError(t, err)

	outcome, err := job.Run(context.Background())
	assert.Error(t, err)
	assert.True(t, outcome.Fatal)
	assert.Equal(t, jobs.StatusFailed, outcome.Status())
	assert.False(t, outcome.OK())
	assert.Equal(t, 1, outcome.ExitCode())
}

// TestARecordWriteFailureCountsAsFailed: a send the provider confirmed but
// the store refused still counts as work attempted, and lands in Failed
// rather than vanishing or failing the whole run. The good neighbor records
// normally, so the run reads partial.
func TestARecordWriteFailureCountsAsFailed(t *testing.T) {
	store := &memStore{
		sends: []Send{
			{ID: "1", MessageID: "msg-good", Recipient: "a@example.com"},
			{ID: "2", MessageID: "msg-unwritable", Recipient: "b@example.com"},
		},
		recordErr: map[string]error{"2": errors.New("outcome write refused")},
	}
	provider := &fakeProvider{statuses: map[string]Status{
		"msg-good":       StatusConfirmed,
		"msg-unwritable": StatusConfirmed,
	}}

	job, err := New(Options{Store: store, Provider: provider})
	require.NoError(t, err)

	outcome, err := job.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusConfirmed, store.outcomes["1"])
	assert.NotContains(t, store.outcomes, "2", "refused write stays unrecorded")
	assert.Equal(t, 2, outcome.Attempted)
	assert.Equal(t, 1, outcome.Succeeded)
	assert.Equal(t, 1, outcome.Failed)
	assert.Equal(t, jobs.StatusPartial, outcome.Status())
}

// TestASendWithNoMessageIDCountsAsFailed: a send logged with no provider key
// cannot be checked, so it counts as failed without consulting the provider
// at all. The checkable neighbor records normally, so the run reads partial.
func TestASendWithNoMessageIDCountsAsFailed(t *testing.T) {
	store := &memStore{sends: []Send{
		{ID: "1", MessageID: "msg-good", Recipient: "a@example.com"},
		{ID: "2", Recipient: "noid@example.com"},
	}}
	provider := &fakeProvider{statuses: map[string]Status{
		"msg-good": StatusConfirmed,
	}}

	job, err := New(Options{Store: store, Provider: provider})
	require.NoError(t, err)

	outcome, err := job.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusConfirmed, store.outcomes["1"])
	assert.NotContains(t, store.outcomes, "2", "keyless send stays unrecorded")
	assert.Equal(t, 2, outcome.Attempted)
	assert.Equal(t, 1, outcome.Succeeded)
	assert.Equal(t, 1, outcome.Failed)
	assert.Equal(t, jobs.StatusPartial, outcome.Status())
}
