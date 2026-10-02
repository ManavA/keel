package pg

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
	if hasEmptyNumber(reflect.ValueOf(attrs)) {
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

var (
	numberType        = reflect.TypeFor[json.Number]()
	marshalerType     = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// hasEmptyNumber reports whether v holds a json.Number with no digits where
// encoding/json would write it. It writes such a number as 0, which nobody sent.
// It follows what encoding/json follows: through pointers, interfaces, maps,
// lists, arrays and the written fields of structs, and not into a value that
// writes itself. It runs only on a value that json.Marshal has accepted, which
// bounds how deep it can go.
func hasEmptyNumber(v reflect.Value) bool {
	if !v.IsValid() {
		return false
	}
	if t := v.Type(); t == numberType {
		return v.String() == ""
	} else if t.Implements(marshalerType) || t.Implements(textMarshalerType) {
		return false
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		return !v.IsNil() && hasEmptyNumber(v.Elem())
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			if hasEmptyNumber(it.Value()) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return false // bytes are written as text
		}
		for i := range v.Len() {
			if hasEmptyNumber(v.Index(i)) {
				return true
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); writtenField(f, v.Field(i)) && hasEmptyNumber(v.Field(i)) {
				return true
			}
		}
	}
	return false
}

// writtenField reports whether encoding/json would write the field f of a
// struct, whose value is v: not when it is unexported (an embedded struct's own
// fields are promoted), tagged "-", or empty and tagged omitempty or omitzero.
func writtenField(f reflect.StructField, v reflect.Value) bool {
	if !f.IsExported() && !f.Anonymous {
		return false
	}
	name, options, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "-" && options == "" {
		return false
	}
	for _, option := range strings.Split(options, ",") {
		switch option {
		case "omitzero":
			if v.IsZero() {
				return false
			}
		case "omitempty":
			if emptyForJSON(v) {
				return false
			}
		}
	}
	return true
}

// emptyForJSON is encoding/json's test for a value that omitempty leaves out.
func emptyForJSON(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return v.IsZero()
	default:
		return false
	}
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
