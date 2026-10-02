package pg

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// A TEXT column holds neither a NUL character nor a byte that is not UTF-8,
// and a jsonb column no NUL inside a string. A model, a tool or a person can
// put either in any string they write, and a run must not be wedged by one:
// a tool result that could not be journaled would have its call made again
// until the run failed. So the store never lets Postgres refuse a string.
//
// What it records and never compares, it keeps with each such character as
// the replacement character, U+FFFD, which is what JSON already makes of a
// byte that is not UTF-8. What it compares, it refuses before it opens a
// transaction: an agent's name, a start key, an owner and a tool effect's
// key name things, and a name kept as something else would name something
// else.

// replacement is what a character no column can hold is kept as.
const replacement = "�"

// storable reports whether a TEXT column holds s as it is.
func storable(s string) bool {
	return strings.IndexByte(s, 0) < 0 && utf8.ValidString(s)
}

// kept is s as a TEXT column keeps it.
func kept(s string) string {
	if storable(s) {
		return s
	}
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", replacement), replacement)
}

// keptJSON is text, a JSON value encoding/json wrote, as a jsonb column
// keeps it: with every NUL inside a string, key or value, as the replacement
// character. encoding/json has already written each byte that is not UTF-8
// as one.
func keptJSON(text []byte) ([]byte, error) {
	// A NUL in a string is written as this escape and no other way, so
	// text without it has none. Text with it may only have the six
	// characters, after a backslash that is itself escaped; the walk
	// below then changes nothing.
	if !bytes.Contains(text, []byte(`\u0000`)) {
		return text, nil
	}
	dec := json.NewDecoder(bytes.NewReader(text))
	// Numbers are kept as the digits they were written with.
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(withoutNUL(value))
}

func withoutNUL(value any) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(v, "\x00", replacement)
	case []any:
		for i, e := range v {
			v[i] = withoutNUL(e)
		}
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, e := range v {
			out[strings.ReplaceAll(key, "\x00", replacement)] = withoutNUL(e)
		}
		return out
	}
	return value
}
