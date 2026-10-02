// Package storerule is the part of the agent.Store contract that says what
// a store keeps, what it refuses and what it keeps as something else, written
// once.
//
// agent.MemoryStore and agent/pg both follow these rules, and a test passing
// on one must pass on the other. When each store had its own copy the copies
// drifted: one replaced a run of bad bytes with one character and the other
// with one per byte. So neither store decides any of this for itself. A rule
// here is a rule of the contract: change it and agenttest.RunStoreSuite
// should change with it.
//
// The package imports the standard library and uuid and nothing else, so
// that agent can import it.
package storerule

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Replacement is what a character no database column holds is kept as.
const Replacement = "�"

// Comparable reports whether a database keeps s as it is: with no NUL
// character in it, and in UTF-8. A string a store compares, which is a name
// of something, must be such a string: kept as anything else it would be
// another name.
func Comparable(s string) bool {
	return strings.IndexByte(s, 0) < 0 && utf8.ValidString(s)
}

// Kept is s as a store keeps a string it only records. Each NUL, and each
// byte that is no part of a valid UTF-8 encoding, is one replacement
// character: byte by byte, as encoding/json replaces, so that three bytes of
// a character cut short are three replacements and not one.
func Kept(s string) string {
	if Comparable(s) {
		return s
	}
	return replaced(s, true)
}

// KeptInJSON is s as a store keeps a string inside a JSON value. A NUL has
// an escape there and is kept. A byte that is not UTF-8 is replaced as Kept
// replaces it, which is what encoding/json writes for one.
func KeptInJSON(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return replaced(s, false)
}

func replaced(s string, nul bool) string {
	var b strings.Builder
	b.Grow(len(s) + len(Replacement))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1, nul && r == 0:
			b.WriteString(Replacement)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// IsUUID reports whether id is a UUID as uuid.NewString writes one, lower
// case with hyphens. A store knows a run or an approval by that one string:
// a uuid column reads other spellings of the same UUID and hands back this
// one, and the id would no longer be the one given.
func IsUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

// Cursor judges the position a listing is to start after, from its id and
// its time. The zero cursor, with neither, is no position: start is true and
// the listing begins at the start. Any other must carry an id that is a UUID
// in the one form, though not one that a run has: a cursor is an argument,
// which may have come from a client, and is refused for its form before a
// store is asked anything.
func Cursor(id string, createdAt time.Time) (start bool, err error) {
	if id == "" && createdAt.IsZero() {
		return true, nil
	}
	if !IsUUID(id) {
		return false, fmt.Errorf("cursor id %q is not a UUID", id)
	}
	return false, nil
}

// ValidRaw reports why a store could not keep value, which is JSON somebody
// else wrote: a call's arguments, a turn in its provider's form, a schema.
// It must be JSON, and in UTF-8, which encoding/json does not check. None is
// valid: see OrNull.
func ValidRaw(value []byte) error {
	switch {
	case len(value) == 0:
		return nil
	case !json.Valid(value):
		return errors.New("not valid JSON")
	case !utf8.Valid(value):
		return errors.New("not UTF-8")
	}
	return nil
}

// OrNull is a raw value as a store hands it back: a copy of the bytes it
// came as, and JSON's null for none. What reads it, a tool for one, can
// always decode it.
func OrNull(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage("null")
	}
	return append(json.RawMessage(nil), value...)
}

// IsCause reports whether cause is one of the three an approval may have.
func IsCause(cause string) bool {
	switch cause {
	case "guard", "tool", "interrupted":
		return true
	}
	return false
}

// IsStepStatus reports whether status is one of the six a step may be in.
func IsStepStatus(status string) bool {
	switch status {
	case "proposed", "waiting", "started", "completed", "blocked", "declined":
		return true
	}
	return false
}

// The bounds on a listing's length.
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ListLimit is how many a listing returns when asked for limit: 50 for none
// or less, and never more than 200.
func ListLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultListLimit
	case limit > MaxListLimit:
		return MaxListLimit
	}
	return limit
}

// Attrs is an action's attributes as a store keeps them and hands them back:
// through JSON, so a number is a float64, an object a map[string]any and a
// list a []any, none is an empty map, and a byte that is not UTF-8 is the
// replacement character; and then with each NUL as the replacement character
// too, in a key or in a string at any depth.
//
// It fails for attributes JSON cannot hold, and for a number no float64
// holds. An attribute may carry JSON of its own, a json.RawMessage or a
// json.Number, which is why the values are decoded and not only encoded:
// what comes back holds nothing but plain values, whatever went in.
func Attrs(attrs map[string]any) (map[string]any, error) {
	text, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("action attributes: %w", err)
	}
	var read map[string]any
	if err := json.Unmarshal(text, &read); err != nil {
		return nil, fmt.Errorf("action attributes: %w", err)
	}
	out, _ := withoutNUL(read).(map[string]any)
	return out, nil
}

// withoutNUL is a value that has been through JSON, with each NUL in its
// strings and keys as the replacement character.
func withoutNUL(value any) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, "\x00", Replacement)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = withoutNUL(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, e := range v {
			out[strings.ReplaceAll(key, "\x00", Replacement)] = withoutNUL(e)
		}
		return out
	}
	return value
}

// Metadata is a run's metadata as a store keeps it and hands it back: a
// copy, with each key and value as Kept keeps it, and an empty map for none.
func Metadata(metadata map[string]string) map[string]string {
	out := make(map[string]string, len(metadata))
	for key, value := range metadata {
		out[Kept(key)] = Kept(value)
	}
	return out
}

// Instant is t as a store keeps a time: to the microsecond below it, which
// is what a timestamp column holds, and with no monotonic reading.
func Instant(t time.Time) time.Time {
	return t.Truncate(time.Microsecond)
}

// InstantPtr is Instant for a time that may be none. It returns a copy.
func InstantPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	kept := Instant(*t)
	return &kept
}

// Expiry is t as a store keeps the time a lease lapses: to the microsecond
// above it. Kept to the one below, a lease would be free to take up to a
// microsecond before its holder, who added the same numbers, counts it
// lapsed.
func Expiry(t time.Time) time.Time {
	kept := Instant(t)
	if kept.Before(t) {
		kept = kept.Add(time.Microsecond)
	}
	return kept
}

// ActiveMillis is the time a step that started at start and finished at end
// spent working: between the two times as a store keeps them, in whole
// milliseconds, and less than nothing when the clock went back. It is a
// measurement, and is not corrected. A step that never started spent none.
func ActiveMillis(start *time.Time, end time.Time) int64 {
	if start == nil {
		return 0
	}
	return Instant(end).Sub(Instant(*start)).Milliseconds()
}
