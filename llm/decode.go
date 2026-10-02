package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Decode reads a structured reply into T. It returns an error when the reply
// did not end with StopEnd, since a refused or truncated reply need not
// match the schema.
//
// A reply that is JSON null is an error too: encoding/json reads null into
// any type without complaint, and a model that answered with null has not
// answered.
//
// Nothing is returned with an error: a value that was only partly read is not
// handed back.
func Decode[T any](resp *Response) (T, error) {
	var zero T
	if resp == nil {
		return zero, errors.New("llm: decode: no response")
	}
	if resp.Stop != StopEnd {
		return zero, fmt.Errorf("llm: decode: reply ended with %q, not %q", resp.Stop, StopEnd)
	}
	text := []byte(resp.Message.Text)
	// The white space JSON allows around a value, which Unmarshal skips too.
	if bytes.Equal(bytes.Trim(text, " \t\r\n"), []byte("null")) {
		return zero, errors.New("llm: decode: the model's output was null")
	}
	var v T
	if err := json.Unmarshal(text, &v); err != nil {
		return zero, fmt.Errorf("llm: decode: %w", err)
	}
	return v, nil
}
