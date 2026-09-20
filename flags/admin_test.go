package flags_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/flags"
)

func seedAdminStore(t *testing.T) *flags.MemoryStore {
	t.Helper()
	s := flags.NewMemoryStore()
	ctx := t.Context()
	require.NoError(t, s.Upsert(ctx, flags.Flag{Key: "a", Enabled: true, Percentage: 50}))
	require.NoError(t, s.Upsert(ctx, flags.Flag{Key: "b", Enabled: false, Allow: []string{"vip"}}))
	return s
}

func TestAdminAPI_List(t *testing.T) {
	api := &flags.AdminAPI{Flags: seedAdminStore(t)}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got []flags.Flag
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Key)
	assert.Equal(t, "b", got[1].Key)
}

func TestAdminAPI_Get(t *testing.T) {
	api := &flags.AdminAPI{Flags: seedAdminStore(t)}

	req := httptest.NewRequest(http.MethodGet, "/b", nil)
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var got flags.Flag
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "b", got.Key)
	assert.Equal(t, []string{"vip"}, got.Allow)
}

func TestAdminAPI_GetMissingIs404(t *testing.T) {
	api := &flags.AdminAPI{Flags: seedAdminStore(t)}

	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminAPI_ListEmptyIsArray(t *testing.T) {
	api := &flags.AdminAPI{Flags: flags.NewMemoryStore()}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	api.Routes().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}
