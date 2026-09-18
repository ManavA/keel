package admin

import (
	"encoding/json"
	"fmt"
)

// ForbiddenFieldError names the key CheckNoForbiddenFields found.
type ForbiddenFieldError struct {
	Key string
}

func (e *ForbiddenFieldError) Error() string {
	return fmt.Sprintf("admin: response contains forbidden field %q", e.Key)
}

// CheckNoForbiddenFields decodes body as JSON and reports an error naming the
// first key in forbidden it finds, at any depth (an object key or, for an
// array of objects, a key in any element). It reports nil for keys it cannot
// find and for JSON it cannot parse as an object or array.
//
// This exists to test a boundary that a type system alone does not enforce:
// a public response type built by embedding, composing, or copying an
// admin-only type can pick up an admin-only field through no change visible
// at its own call site. Running this check against a public handler's
// encoded response, in that handler's own test, catches the field arriving
// however it arrived — a shared struct, a missing json:"-" tag, a change to
// an embedded type — rather than only catching the specific mistake someone
// anticipated.
func CheckNoForbiddenFields(body []byte, forbidden ...string) error {
	forbiddenSet := make(map[string]bool, len(forbidden))
	for _, k := range forbidden {
		forbiddenSet[k] = true
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		// Malformed JSON is not this function's concern to report: a caller
		// checking a response for a forbidden field already has a separate,
		// more direct test that the response is valid JSON at all.
		return nil //nolint:nilerr // deliberate, see comment above
	}
	if key, found := findForbiddenKey(v, forbiddenSet); found {
		return &ForbiddenFieldError{Key: key}
	}
	return nil
}

func findForbiddenKey(v any, forbidden map[string]bool) (string, bool) {
	switch val := v.(type) {
	case map[string]any:
		for key, nested := range val {
			if forbidden[key] {
				return key, true
			}
			if k, found := findForbiddenKey(nested, forbidden); found {
				return k, true
			}
		}
	case []any:
		for _, item := range val {
			if k, found := findForbiddenKey(item, forbidden); found {
				return k, true
			}
		}
	}
	return "", false
}
