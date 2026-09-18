package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSort(t *testing.T) {
	table := SortTable{
		"price_asc": {Field: "price", Dir: Asc},
		"newest":    {Field: "list_date", Dir: Desc},
	}

	assert.Equal(t, []SortField{{Field: "price", Dir: Asc}}, BuildSort(table, "price_asc"))
	assert.Nil(t, BuildSort(table, ""), "an empty key must fall back to default order, not error")
	assert.Nil(t, BuildSort(table, "unknown_key"), "an unknown key must fall back to default order, not error")
}

type decodeTarget struct {
	ID    string `json:"id"`
	Price int    `json:"price"`
}

func TestDecodeHits(t *testing.T) {
	hits := []map[string]any{
		{"id": "1", "price": float64(500000)},
		{"id": "2", "price": float64(750000)},
	}
	out, err := DecodeHits[decodeTarget](hits)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, decodeTarget{ID: "1", Price: 500000}, out[0])
	assert.Equal(t, decodeTarget{ID: "2", Price: 750000}, out[1])
}

func TestDecodeHits_Empty(t *testing.T) {
	out, err := DecodeHits[decodeTarget](nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestMapDocument_ID(t *testing.T) {
	assert.Equal(t, "abc", MapDocument{"id": "abc"}.ID())
	assert.Equal(t, "", MapDocument{}.ID(), "a missing id must yield empty, not panic")
	assert.Equal(t, "", MapDocument{"id": 123}.ID(), "a non-string id field must yield empty, not the wrong type")
}
