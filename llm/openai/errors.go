package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ManavA/keel/llm"
)

// maxMessageBytes bounds the part of a body that is kept as a message when
// the body is not an error object, so a gateway's whole error page does not
// become an error string.
const maxMessageBytes = 512

// finalCodes are the 429 error codes no wait fixes: the account is out of
// credit, or has reached a spend or usage limit. The error guide lists the
// codes and says error.type can still be insufficient_quota, so both fields
// are checked.
var finalCodes = map[string]bool{
	"insufficient_quota":                true,
	"credit_balance_exhausted":          true,
	"organization_spend_limit_exceeded": true,
	"project_spend_limit_exceeded":      true,
	"organization_usage_limit_exceeded": true,
}

// transientCodes are the error types and codes the reference gives for a
// failure that passes: the error guide's overload and rate limit, and the
// server error and rate limit the specification lists. They stand in for a
// status on an error that arrives inside a 200 response, which has none to
// say so. The final codes above take precedence.
var transientCodes = map[string]bool{
	"service_unavailable_error": true,
	"server_is_overloaded":      true,
	"slow_down":                 true,
	"rate_limit_error":          true,
	"rate_limit_exceeded":       true,
	"server_error":              true,
}

// errorBody is what an error object says.
type errorBody struct {
	Type, Message, Code string
}

// present reports whether the object is an error to report: it says what went
// wrong or what kind. An empty string or an empty object, which some servers
// send beside a good reply, is not.
func (f errorBody) present() bool { return f.Message != "" || f.Type != "" }

// isFinal reports whether the error is one that no wait fixes.
func (f errorBody) isFinal() bool { return finalCodes[f.Code] || finalCodes[f.Type] }

// isTransient reports whether the error is one the reference gives as passing.
func (f errorBody) isTransient() bool {
	return !f.isFinal() && (transientCodes[f.Type] || transientCodes[f.Code])
}

// httpError maps a response that is not 2xx. body is as much of the body as
// could be read, and may be empty.
func httpError(resp *http.Response, body []byte) *llm.Error {
	fields, ok := errorFromBody(body)
	if !ok {
		fields.Message = snippet(body)
	}
	if loc, err := resp.Location(); err == nil && resp.StatusCode >= 300 && resp.StatusCode < 400 {
		fields.Message = fmt.Sprintf("redirect to %s not followed", loc.Scheme+"://"+loc.Host+loc.Path)
	}
	if fields.Message == "" {
		fields.Message = http.StatusText(resp.StatusCode)
	}
	return &llm.Error{
		Provider:   Name,
		Status:     resp.StatusCode,
		Type:       typeOf(fields),
		Message:    fields.Message,
		RequestID:  resp.Header.Get("X-Request-Id"),
		RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now()),
		Retryable:  retryableStatus(resp.StatusCode, fields),
	}
}

// embeddedError maps an error object that arrived in a 200 response, in the
// body or in the middle of a stream. The server answered, so it is not a
// transport failure, and it is retryable only when the error says it passes.
func embeddedError(status int, requestID string, fields errorBody) *llm.Error {
	if fields.Message == "" {
		fields.Message = "the server reported an error in a successful response"
	}
	return &llm.Error{
		Provider:  Name,
		Status:    status,
		Type:      typeOf(fields),
		Message:   fields.Message,
		RequestID: requestID,
		Retryable: fields.isTransient(),
	}
}

// notAStream is the error for a 200 that is not an event stream, in answer to
// a request for one. Asking again gets the same answer, so it is not retryable.
func notAStream(resp *http.Response) *llm.Error {
	what := "no content type"
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		what = fmt.Sprintf("content type %q", ct)
	}
	return &llm.Error{
		Provider:  Name,
		Status:    resp.StatusCode,
		Message:   "expected an event stream in answer to a streaming request, got " + what,
		RequestID: resp.Header.Get("X-Request-Id"),
	}
}

// isEventStream reports whether the response is a server-sent event stream.
func isEventStream(resp *http.Response) bool {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}

// typeOf is the error's type, or its code for a server that sends only that.
func typeOf(f errorBody) string {
	if f.Type != "" {
		return f.Type
	}
	return f.Code
}

// retryableStatus reports whether the same request may succeed later: a
// timeout, any server error, and a 429 that is not one of the final codes.
func retryableStatus(status int, f errorBody) bool {
	switch {
	case status == http.StatusRequestTimeout, status >= 500 && status < 600:
		return true
	case status == http.StatusTooManyRequests:
		return !f.isFinal()
	}
	return false
}

// errorFromBody reads an error response body: the documented
// {"error":{"message","type","param","code"}}, an error that is only a
// string, or the message, type and code at the top level, as some local
// servers send them. It reports false for a body that is none of these.
func errorFromBody(body []byte) (errorBody, bool) {
	var env struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	}
	if json.Unmarshal(body, &env) != nil {
		return errorBody{}, false
	}
	if f, ok := errorFields(env.Error); ok && f != (errorBody{}) {
		return f, true
	}
	top := errorBody{Message: env.Message, Type: env.Type, Code: codeString(env.Code)}
	return top, top.present()
}

// errorFields reads the value of an "error" key.
func errorFields(raw json.RawMessage) (errorBody, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return errorBody{}, false
	}
	switch raw[0] {
	case '"':
		var msg string
		if json.Unmarshal(raw, &msg) == nil {
			return errorBody{Message: msg}, true
		}
	case '{':
		var obj struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		}
		if json.Unmarshal(raw, &obj) == nil {
			return errorBody{Message: obj.Message, Type: obj.Type, Code: codeString(obj.Code)}, true
		}
	}
	return errorBody{}, false
}

// codeString reads a code that is a string, as the reference says, or a
// number, as some servers send.
func codeString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// snippet is the start of a body that is not an error object, as text.
func snippet(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) <= maxMessageBytes {
		return strings.ToValidUTF8(text, "?")
	}
	return strings.ToValidUTF8(text[:maxMessageBytes], "?") + "..."
}

// retryAfter reads a Retry-After header: a number of seconds, which may be
// fractional, or an HTTP date. A value that is neither, or is not in the
// future, is no wait.
func retryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(value, 64); err == nil {
		switch {
		case math.IsNaN(secs) || secs <= 0:
			return 0
		case secs >= float64(math.MaxInt64)/float64(time.Second):
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(secs * float64(time.Second))
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(0, at.Sub(now))
	}
	return 0
}
