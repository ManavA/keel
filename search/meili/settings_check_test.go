package meili

import (
	"testing"

	"github.com/meilisearch/meilisearch-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDriftCheck_ReportsRemovedFilterableAttribute is the acceptance check
// for the drift-check job: an index that matched Config drifts when a
// filterable attribute is removed out from under it, and the scheduled check
// must report that exact attribute with the rejected-filter consequence.
func TestDriftCheck_ReportsRemovedFilterableAttribute(t *testing.T) {
	cfg := Config{UID: testUID, Filterable: []string{testStatus, testCity}}
	live := meilisearch.Settings{FilterableAttributes: []string{testStatus, testCity}}
	idx := &fakeIndex{
		getSettingsFn: func() (*meilisearch.Settings, error) { return &live, nil },
	}
	s := testSearcher(&fakeClient{}, idx, cfg)

	// The live index starts clean: the check passes on an index that matches.
	outcome, err := s.DriftCheck()(t.Context())
	require.NoError(t, err)
	assert.True(t, outcome.OK())
	assert.Zero(t, outcome.ExitCode())

	// Drift the live index the way an out-of-band settings edit does: the
	// attribute leaves the live index while Config still declares it.
	live.FilterableAttributes = []string{testStatus}

	outcome, err = s.DriftCheck()(t.Context())
	require.Error(t, err, "drift must fail loudly: a scheduler that only retries non-zero exits never retries a silent pass")
	assert.False(t, outcome.OK())
	assert.NotZero(t, outcome.ExitCode())
	assert.Contains(t, err.Error(), testCity)
	assert.Contains(t, err.Error(), "REJECTED")
}

func TestDriftCheck_UnmeasuredIndexFailsLoudly(t *testing.T) {
	idx := &fakeIndex{getSettingsFn: func() (*meilisearch.Settings, error) { return nil, errFake }}
	s := testSearcher(&fakeClient{}, idx, Config{})

	outcome, err := s.DriftCheck()(t.Context())
	require.Error(t, err)
	assert.False(t, outcome.OK(), "could not measure is not measured clean")
	assert.NotZero(t, outcome.ExitCode())
	assert.Zero(t, outcome.Attempted, "nothing was measured, so nothing was attempted")
}

func TestRequireSettings_CleanIndexPasses(t *testing.T) {
	idx := &fakeIndex{
		getSettingsFn: func() (*meilisearch.Settings, error) {
			return &meilisearch.Settings{FilterableAttributes: []string{testStatus}}, nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{Filterable: []string{testStatus}})

	assert.NoError(t, s.RequireSettings(t.Context()))
}

func TestRequireSettings_DriftedIndexRefusesToStart(t *testing.T) {
	idx := &fakeIndex{
		getSettingsFn: func() (*meilisearch.Settings, error) {
			return &meilisearch.Settings{FilterableAttributes: []string{}}, nil
		},
	}
	s := testSearcher(&fakeClient{}, idx, Config{Filterable: []string{testStatus}})

	err := s.RequireSettings(t.Context())
	require.Error(t, err, "the startup gate must refuse a drifted index, not log and continue")
	assert.Contains(t, err.Error(), testStatus)
	assert.Contains(t, err.Error(), "REJECTED")
}

func TestRequireSettings_UnmeasuredIndexRefusesToStart(t *testing.T) {
	idx := &fakeIndex{getSettingsFn: func() (*meilisearch.Settings, error) { return nil, errFake }}
	s := testSearcher(&fakeClient{}, idx, Config{})

	err := s.RequireSettings(t.Context())
	require.Error(t, err, "an index whose settings could not be read must not pass the gate")
	assert.Contains(t, err.Error(), "NOT CHECKED")
}
