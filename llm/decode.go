package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Decode reads a structured reply into T. It returns an error when the reply
// did not end with StopEnd, since a refused or truncated reply need not
// match the schema.
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
	var v T
	if err := json.Unmarshal([]byte(resp.Message.Text), &v); err != nil {
		return zero, fmt.Errorf("llm: decode: %w", err)
	}
	return v, nil
}
