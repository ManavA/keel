package admin

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The types below are a generic stand-in for a common shape: one underlying
// record with a public view and an operator-only view that carries
// additional fields. assertNoProtectedFieldFor is the pattern this package
// exports as CheckNoForbiddenFields; the test below is written the way a
// consuming service would write its own.

type publicWidget struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
}

type internalWidget struct {
	publicWidget
	InternalNotes string `json:"internal_notes"`
	OwnerSecret   string `json:"owner_secret"`
}

// forbiddenWidgetFields lists the keys that must never reach a public
// response, the way a consuming service would define its own list once for
// every place a check like this runs.
var forbiddenWidgetFields = []string{"internal_notes", "owner_secret"}

func assertNoProtectedFieldFor(t *testing.T, body []byte) {
	t.Helper()
	if err := CheckNoForbiddenFields(body, forbiddenWidgetFields...); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNoForbiddenFieldsPassesOnAPublicResponse(t *testing.T) {
	body, err := json.Marshal(publicWidget{ID: "w1", Name: "Widget", Price: 100})
	require.NoError(t, err)
	assertNoProtectedFieldFor(t, body)
}

func TestCheckNoForbiddenFieldsCatchesAnAdminFieldInAPublicResponse(t *testing.T) {
	// Simulates the exact mistake this check exists to catch: a public
	// handler accidentally serializing the admin-only type.
	leaked := internalWidget{
		publicWidget:  publicWidget{ID: "w1", Name: "Widget", Price: 100},
		InternalNotes: "not for external display",
		OwnerSecret:   "internal cost basis is 40",
	}
	body, err := json.Marshal(leaked)
	require.NoError(t, err)

	err = CheckNoForbiddenFields(body, forbiddenWidgetFields...)
	require.Error(t, err)
	var ffErr *ForbiddenFieldError
	require.ErrorAs(t, err, &ffErr)
	assert.Contains(t, forbiddenWidgetFields, ffErr.Key)
}

func TestCheckNoForbiddenFieldsCatchesANestedField(t *testing.T) {
	leaked := map[string]any{
		"items": []map[string]any{
			{"id": "w1", "name": "Widget"},
			{"id": "w2", "name": "Other Widget", "owner_secret": "leaked here"},
		},
	}
	body, err := json.Marshal(leaked)
	require.NoError(t, err)

	err = CheckNoForbiddenFields(body, forbiddenWidgetFields...)
	require.Error(t, err)
}

func TestCheckNoForbiddenFieldsIgnoresUnrelatedFields(t *testing.T) {
	body, err := json.Marshal(map[string]any{"a": 1, "b": map[string]any{"c": "d"}})
	require.NoError(t, err)
	assert.NoError(t, CheckNoForbiddenFields(body, "owner_secret"))
}
