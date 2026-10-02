package pg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/policy"
)

// encodeAttrs is the attributes as the JSON object to store, {} when there are
// none. It refuses what cannot be recorded as the caller meant it: a value JSON
// cannot hold, a NUL character, and a number with no digits.
func encodeAttrs(attrs map[string]any) ([]byte, error) {
	if len(attrs) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("the attributes cannot be written as JSON: %w", err)
	}
	if hasNULEscape(b) {
		return nil, errNUL("an attribute name or value")
	}
	if hasEmptyNumber(attrs) {
		return nil, errors.New("an attribute holds an empty json.Number, which is not a number and which JSON would write as 0")
	}
	return b, nil
}

// hasNULEscape reports whether JSON text spells a NUL character, which is the
// escape \u0000, in a string or a key. It reads escapes as JSON does, so the
// text of an escaped backslash followed by u0000 is not one.
func hasNULEscape(text []byte) bool {
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			continue
		}
		if bytes.HasPrefix(text[i+1:], []byte("u0000")) {
			return true
		}
		i++ // whatever follows a backslash is part of its escape
	}
	return false
}

// hasEmptyNumber reports whether v holds a json.Number with no digits. For
// such a number encoding/json writes 0, which nobody sent.
//
// It looks at what [encoding/json.Decoder.UseNumber] produces, which is what
// attribute values are when they come from decoded tool input: a map[string]any,
// a []any and a json.Number, however deeply they nest. Anything else, a struct,
// a pointer or a list of another type, it leaves to encoding/json, which has
// its own rules for what it writes of a struct and of its fields; a copy of
// them here could disagree. It runs only on a value that json.Marshal has
// accepted, so it is not given a cycle.
func hasEmptyNumber(v any) bool {
	switch v := v.(type) {
	case json.Number:
		return v == ""
	case map[string]any:
		for _, e := range v {
			if hasEmptyNumber(e) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if hasEmptyNumber(e) {
				return true
			}
		}
	}
	return false
}

// unrecordableError is an error that no retry will cure: its text is the
// error's, and it wraps both policy.ErrUnrecordable and the error, so a caller
// can find the sentinel and the server's own error alike. The policy package
// marks the records it refuses the same way.
type unrecordableError struct{ err error }

func unrecordable(err error) error { return unrecordableError{err} }

func (e unrecordableError) Error() string   { return e.err.Error() }
func (e unrecordableError) Unwrap() []error { return []error{policy.ErrUnrecordable, e.err} }

// asUnrecordable marks an error from the database as one no retry will cure
// when the server says the fault is in the record: SQLSTATE class 22, a data
// exception (a number past numeric's range, an escape or a byte sequence jsonb
// or text refuses), and class 54, a program limit (a value nested too deeply, a
// row or an index entry too large). Both depend on the record and nothing else.
// Every other error is the database's or the moment's, and comes back as it was.
func asUnrecordable(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (strings.HasPrefix(pgErr.Code, "22") || strings.HasPrefix(pgErr.Code, "54")) {
		return unrecordable(err)
	}
	return err
}
