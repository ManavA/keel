package pg_test

import (
	"net/url"
	"testing"

	"github.com/ManavA/keel/pg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func query(t *testing.T, raw string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(raw)
	require.NoError(t, err)
	return values
}

func TestParsePage(t *testing.T) {
	opts := pg.PageOptions{DefaultLimit: 20, MaxLimit: 100, MaxOffset: 10_000}

	tests := []struct {
		name  string
		query string
		want  pg.Page
	}{
		{"nothing sent takes the defaults", "", pg.Page{Limit: 20}},
		{"limit", "limit=50", pg.Page{Limit: 50}},
		{"offset", "limit=10&offset=30", pg.Page{Limit: 10, Offset: 30}},
		{"page one is the first page", "limit=10&page=1", pg.Page{Limit: 10}},
		{"page is 1-based, in units of limit", "limit=10&page=4", pg.Page{Limit: 10, Offset: 30}},
		{"surrounding whitespace is tolerated", "limit=%2050%20", pg.Page{Limit: 50}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pg.ParsePage(query(t, tt.query), opts)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParsePageRejections(t *testing.T) {
	opts := pg.PageOptions{DefaultLimit: 20, MaxLimit: 100, MaxOffset: 10_000}

	tests := []struct {
		name     string
		query    string
		contains string
	}{
		{
			name:     "an alias this endpoint does not read",
			query:    "per_page=25",
			contains: "per_page",
		},
		{"another alias", "pageSize=25", "pageSize"},
		{"an offset alias", "skip=10", "skip"},
		{
			name:     "offset and page together",
			query:    "offset=10&page=1",
			contains: "not both",
		},
		{
			name:     "a limit over the ceiling is refused, not clamped",
			query:    "limit=1000000",
			contains: "between 1 and 100",
		},
		{"limit below one", "limit=0", "between 1 and 100"},
		{"a limit that is not a number", "limit=abc", "whole number"},
		{"a negative offset", "offset=-5", "between 0 and"},
		{
			name:     "a page index that would overflow the offset",
			query:    "limit=10&page=9223372036854775807",
			contains: "page",
		},
		{
			// 1-based, so page=0 is a caller who believes it is 0-based and
			// would otherwise silently get the first page under a wrong name.
			name:     "page zero",
			query:    "limit=10&page=0",
			contains: `"page" must be a whole number between 1`,
		},
		{
			name:     "a parameter sent twice",
			query:    "limit=10&limit=50",
			contains: "sent 2 times",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pg.ParsePage(query(t, tt.query), opts)
			require.Error(t, err, "a parameter that cannot be honoured must not be silently dropped")
			assert.ErrorIs(t, err, pg.ErrPaging)
			assert.Contains(t, err.Error(), tt.contains)
		})
	}
}

func TestParsePageDefaults(t *testing.T) {
	got, err := pg.ParsePage(url.Values{}, pg.PageOptions{})
	require.NoError(t, err)
	assert.Equal(t, pg.Page{Limit: 20}, got)

	_, err = pg.ParsePage(query(t, "limit=201"), pg.PageOptions{})
	assert.Error(t, err)
}

func TestParsePageClampsTheDefaultToTheCeiling(t *testing.T) {
	// Serving 500 rows to a caller who sent nothing while refusing limit=500
	// would be two answers to the same question.
	got, err := pg.ParsePage(url.Values{}, pg.PageOptions{DefaultLimit: 500, MaxLimit: 200})
	require.NoError(t, err)
	assert.Equal(t, 200, got.Limit)

	_, err = pg.ParsePage(query(t, "limit=500"), pg.PageOptions{DefaultLimit: 500, MaxLimit: 200})
	assert.Error(t, err)
}

func TestRejectPaging(t *testing.T) {
	for _, name := range []string{"limit", "offset", "page", "per_page", "skip"} {
		t.Run(name, func(t *testing.T) {
			err := pg.RejectPaging(query(t, name+"=5"), "the whole price history")
			require.Error(t, err)
			assert.ErrorIs(t, err, pg.ErrPaging)
			assert.Contains(t, err.Error(), "the whole price history")
		})
	}

	assert.NoError(t, pg.RejectPaging(query(t, "since=2026-01-01"), "everything"))
}

func TestKeysetOrderBy(t *testing.T) {
	tests := []struct {
		name    string
		keyset  pg.Keyset
		want    string
		wantErr bool
	}{
		{
			name:   "one column",
			keyset: pg.Keyset{Sort: []pg.SortKey{{Column: "created_at"}}},
			want:   "created_at",
		},
		{
			name: "descending, with a tiebreaker",
			keyset: pg.Keyset{Sort: []pg.SortKey{
				{Column: "created_at", Desc: true},
				{Column: "id", Desc: true},
			}},
			want: "created_at desc, id desc",
		},
		{
			name:   "a qualified column",
			keyset: pg.Keyset{Sort: []pg.SortKey{{Column: "listings.created_at"}}},
			want:   "listings.created_at",
		},
		{
			name:    "no sort key at all",
			keyset:  pg.Keyset{},
			wantErr: true,
		},
		{
			name:    "a column name that is not an identifier",
			keyset:  pg.Keyset{Sort: []pg.SortKey{{Column: "id; drop table users"}}},
			wantErr: true,
		},
		{
			name:    "a column name with a quote in it",
			keyset:  pg.Keyset{Sort: []pg.SortKey{{Column: `id" , (select 1)`}}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.keyset.OrderBy()
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestKeysetWhere(t *testing.T) {
	t.Run("the first page has no predicate", func(t *testing.T) {
		clause, args, err := pg.Keyset{Sort: []pg.SortKey{{Column: "id"}}}.Where(1)
		require.NoError(t, err)
		assert.Empty(t, clause)
		assert.Empty(t, args)
	})

	t.Run("a row comparison, not a chain of ORs", func(t *testing.T) {
		k := pg.Keyset{
			Sort:  []pg.SortKey{{Column: "created_at"}, {Column: "id"}},
			After: []any{"2026-01-01", 42},
		}
		clause, args, err := k.Where(3)
		require.NoError(t, err)
		assert.Equal(t, "(created_at, id) > ($3, $4)", clause)
		assert.Equal(t, []any{"2026-01-01", 42}, args)
	})

	t.Run("descending flips the operator", func(t *testing.T) {
		k := pg.Keyset{
			Sort:  []pg.SortKey{{Column: "created_at", Desc: true}, {Column: "id", Desc: true}},
			After: []any{"2026-01-01", 42},
		}
		clause, _, err := k.Where(1)
		require.NoError(t, err)
		assert.Equal(t, "(created_at, id) < ($1, $2)", clause)
	})

	t.Run("a mixed ordering is refused rather than rendered wrong", func(t *testing.T) {
		k := pg.Keyset{
			Sort:  []pg.SortKey{{Column: "created_at", Desc: true}, {Column: "id"}},
			After: []any{"2026-01-01", 42},
		}
		_, _, err := k.Where(1)
		assert.Error(t, err)
	})

	t.Run("the cursor must have one value per sort key", func(t *testing.T) {
		k := pg.Keyset{
			Sort:  []pg.SortKey{{Column: "created_at"}, {Column: "id"}},
			After: []any{"2026-01-01"},
		}
		_, _, err := k.Where(1)
		assert.Error(t, err)
	})

	t.Run("a bad column name is refused", func(t *testing.T) {
		k := pg.Keyset{
			Sort:  []pg.SortKey{{Column: "id); drop table users --"}},
			After: []any{1},
		}
		_, _, err := k.Where(1)
		assert.Error(t, err)
	})
}

func TestCursorRoundTrip(t *testing.T) {
	encoded, err := pg.EncodeCursor([]any{"2026-01-01T00:00:00Z", 42})
	require.NoError(t, err)
	assert.NotContains(t, encoded, "2026", "a cursor is opaque so its shape can change later")

	decoded, err := pg.DecodeCursor(encoded)
	require.NoError(t, err)
	require.Len(t, decoded, 2)
	assert.Equal(t, "2026-01-01T00:00:00Z", decoded[0])
	assert.Equal(t, float64(42), decoded[1])
}

func TestDecodeCursorRejectsRubbish(t *testing.T) {
	// Answering with the first page would loop a client over the same rows.
	_, err := pg.DecodeCursor("not-base64-!!")
	assert.Error(t, err)

	_, err = pg.DecodeCursor("aGVsbG8")
	assert.Error(t, err)

	empty, err := pg.DecodeCursor("")
	require.NoError(t, err)
	assert.Nil(t, empty)
}
