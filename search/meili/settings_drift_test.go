package meili

import (
	"context"
	"testing"

	"github.com/meilisearch/meilisearch-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsReport_OK(t *testing.T) {
	assert.False(t, SettingsReport{}.OK(), "an unmeasured report must never read as OK")
	assert.False(t, SettingsReport{Measured: true, Drifts: []SettingDrift{{Setting: "x"}}}.OK())
	assert.True(t, SettingsReport{Measured: true}.OK())
}

func TestSettingsReport_Describe_DistinguishesAllThreeStates(t *testing.T) {
	assert.Contains(t, SettingsReport{}.Describe(), "NOT CHECKED")
	assert.Contains(t, SettingsReport{Measured: true}.Describe(), "match")
	assert.Contains(t, SettingsReport{Measured: true, Drifts: []SettingDrift{{Setting: "x", Want: "a", Got: "b"}}}.Describe(), "drifted")
}

func TestCompareIndexSettings_NoDriftWhenEverythingMatches(t *testing.T) {
	cfg := Config{
		Searchable:        []string{"title"},
		Filterable:        []string{testStatus},
		Sortable:          []string{testPrice},
		RankingRules:      []string{testWords, testTypo},
		Synonyms:          map[string][]string{"sf": {testSF}},
		MaxTotalHits:      1000,
		MaxValuesPerFacet: 100,
	}
	got := meilisearch.Settings{
		SearchableAttributes: []string{"title"},
		FilterableAttributes: []string{testStatus},
		SortableAttributes:   []string{testPrice},
		RankingRules:         []string{testWords, testTypo},
		Synonyms:             map[string][]string{"sf": {"san francisco"}},
		Pagination:           &meilisearch.Pagination{MaxTotalHits: 1000},
		Faceting:             &meilisearch.Faceting{MaxValuesPerFacet: 100},
	}
	assert.Empty(t, compareIndexSettings(cfg, got))
}

func TestCompareIndexSettings_MissingFilterableIsDrift(t *testing.T) {
	cfg := Config{Filterable: []string{testStatus, testCity}}
	got := meilisearch.Settings{FilterableAttributes: []string{testStatus}}
	drifts := compareIndexSettings(cfg, got)
	require.Len(t, drifts, 1)
	assert.Equal(t, "filterableAttributes", drifts[0].Setting)
	assert.Equal(t, []string{testCity}, drifts[0].Missing)
	assert.Contains(t, drifts[0].Impact, "REJECTED")
}

func TestCompareIndexSettings_SortableSetIsOrderInsensitive(t *testing.T) {
	cfg := Config{Sortable: []string{testPrice, testSqft}}
	got := meilisearch.Settings{SortableAttributes: []string{testSqft, testPrice}}
	assert.Empty(t, compareIndexSettings(cfg, got), "sortable/filterable are sets: Meilisearch may reorder them without that being drift")
}

func TestCompareIndexSettings_RankingRulesOrderMatters(t *testing.T) {
	cfg := Config{RankingRules: []string{testWords, testTypo, "proximity"}}
	got := meilisearch.Settings{RankingRules: []string{testTypo, testWords, "proximity"}}
	drifts := compareIndexSettings(cfg, got)
	require.Len(t, drifts, 1)
	assert.Equal(t, "rankingRules", drifts[0].Setting)
}

func TestCompareIndexSettings_SearchableOrderMatters(t *testing.T) {
	cfg := Config{Searchable: []string{"city", "address"}}
	got := meilisearch.Settings{SearchableAttributes: []string{"address", "city"}}
	drifts := compareIndexSettings(cfg, got)
	require.Len(t, drifts, 1)
	assert.Equal(t, "searchableAttributes", drifts[0].Setting)
}

func TestCompareIndexSettings_MaxTotalHitsBelowDeclaredIsDrift(t *testing.T) {
	cfg := Config{MaxTotalHits: 20000}
	got := meilisearch.Settings{Pagination: &meilisearch.Pagination{MaxTotalHits: 1000}}
	drifts := compareIndexSettings(cfg, got)
	require.Len(t, drifts, 1)
	assert.Equal(t, "pagination.maxTotalHits", drifts[0].Setting)
	assert.Contains(t, drifts[0].Impact, "CAPPED")
}

func TestCompareIndexSettings_UnsetMaxTotalHitsIsNotChecked(t *testing.T) {
	cfg := Config{} // MaxTotalHits unset
	got := meilisearch.Settings{Pagination: &meilisearch.Pagination{MaxTotalHits: 1000}}
	assert.Empty(t, compareIndexSettings(cfg, got), "a Config that never declared MaxTotalHits must not flag Meilisearch's own default as drift")
}

func TestCompareIndexSettings_MissingSynonymIsDrift(t *testing.T) {
	cfg := Config{Synonyms: map[string][]string{"sf": {"san francisco"}}}
	got := meilisearch.Settings{Synonyms: map[string][]string{}}
	drifts := compareIndexSettings(cfg, got)
	require.Len(t, drifts, 1)
	assert.Equal(t, "synonyms", drifts[0].Setting)
	assert.Equal(t, []string{"sf"}, drifts[0].Missing)
}

func TestCheckSettings_MeasuredFlagAndDrifts(t *testing.T) {
	idx := &fakeIndex{getSettingsFn: func() (*meilisearch.Settings, error) {
		return &meilisearch.Settings{FilterableAttributes: []string{}}, nil
	}}
	s := testSearcher(&fakeClient{}, idx, Config{Filterable: []string{testStatus}})

	report, err := s.CheckSettings(context.Background())
	require.NoError(t, err)
	assert.True(t, report.Measured)
	assert.False(t, report.OK())
}

func TestCheckSettings_ReadFailureLeavesMeasuredFalse(t *testing.T) {
	idx := &fakeIndex{getSettingsFn: func() (*meilisearch.Settings, error) { return nil, errFake }}
	s := testSearcher(&fakeClient{}, idx, Config{})

	report, err := s.CheckSettings(context.Background())
	require.Error(t, err)
	assert.False(t, report.Measured, "a failed read must not be mistaken for a clean report")
}
