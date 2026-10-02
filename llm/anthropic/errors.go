package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ManavA/keel/llm"
)

// maxMessageBytes bounds the part of a body that is not the API's error
// shape, a proxy's error page for one, that is kept as an error's message.
const maxMessageBytes = 1024

// wireError is the error object of an error body and of a stream's error
// event.
type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type wireErrorBody struct {
	Error     wireError `json:"error"`
	RequestID string    `json:"request_id"`
}

// apiError turns a response that is not 2xx into an *llm.Error.
func apiError(resp *http.Response, body []byte) *llm.Error {
	e := &llm.Error{Provider: Name, Status: resp.StatusCode, RequestID: requestID(resp)}

	var wire wireErrorBody
	if json.Unmarshal(body, &wire) == nil {
		e.Type, e.Message = wire.Error.Type, wire.Error.Message
		if e.RequestID == "" {
			e.RequestID = wire.RequestID
		}
	}
	if e.Type == "" && e.Message == "" {
		// Not the API's error shape: a proxy or the edge answered.
		e.Message = excerpt(body, resp.StatusCode)
	}

	wait, asked := retryAfter(resp.Header)
	e.RetryAfter = wait
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// A rate limit says how long to wait. A 429 that does not is a spend
		// cap, which keeps failing until the cap is lifted.
		e.Retryable = asked
	case resp.StatusCode == http.StatusRequestTimeout, resp.StatusCode == http.StatusConflict, resp.StatusCode >= 500:
		e.Retryable = true
	}
	return e
}

// streamError turns a stream's error event into an *llm.Error. The status is
// the one the stream opened with: the failure came after it.
func streamError(resp *http.Response, wire wireError) *llm.Error {
	return &llm.Error{
		Provider:  Name,
		Status:    resp.StatusCode,
		Type:      wire.Type,
		Message:   wire.Message,
		RequestID: requestID(resp),
		Retryable: wire.Type == "overloaded_error" || wire.Type == "api_error",
	}
}

// replyError reports a response that arrived whole and is not what the
// reference describes. The same request would be answered the same way, so
// it is not retryable.
func replyError(resp *http.Response, format string, args ...any) *llm.Error {
	return &llm.Error{
		Provider:  Name,
		Status:    resp.StatusCode,
		Message:   fmt.Sprintf(format, args...),
		RequestID: requestID(resp),
	}
}

// transportError reports a call that got no response, or lost it part way.
// It is retryable unless the caller's context is what ended it.
func transportError(ctx context.Context, id string, err error) *llm.Error {
	return &llm.Error{Provider: Name, RequestID: id, Err: err, Retryable: ctx.Err() == nil}
}

// readError reports a failure to read a response body.
func readError(ctx context.Context, resp *http.Response, err error) *llm.Error {
	if errors.Is(err, errTooLarge) {
		return replyError(resp, "%v", errTooLarge)
	}
	return transportError(ctx, requestID(resp), err)
}

func requestID(resp *http.Response) string {
	return resp.Header.Get("request-id")
}

// retryAfter reads the retry-after header, which the API sends as a number
// of seconds. asked reports whether the header was there at all, whatever
// it held.
func retryAfter(h http.Header) (wait time.Duration, asked bool) {
	values := h.Values("retry-after")
	if len(values) == 0 {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(values[0]), 64)
	if err != nil || math.IsNaN(seconds) || seconds <= 0 {
		return 0, true
	}
	if seconds >= float64(math.MaxInt64/int64(time.Second)) {
		return math.MaxInt64, true
	}
	return time.Duration(seconds * float64(time.Second)), true
}

// excerpt is the start of a body that is not the API's error shape, or the
// status text when there is no body.
func excerpt(body []byte, status int) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return http.StatusText(status)
	}
	if len(text) > maxMessageBytes {
		// The cut may fall inside a character; drop the half that is left.
		text = strings.ToValidUTF8(text[:maxMessageBytes], "")
	}
	return text
}
