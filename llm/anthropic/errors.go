package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// sentError turns an error object the API sent under a 200 into an
// *llm.Error: a stream's error event, or an error body where a reply was
// expected. The status is the one the response came with, since the failure
// came after it. The reference names overloaded_error as the usual one, the
// counterpart of a 529, and api_error is the counterpart of a 500; those two
// are worth another try.
func sentError(resp *http.Response, wire wireError) *llm.Error {
	return &llm.Error{
		Provider:  Name,
		Status:    resp.StatusCode,
		Type:      wire.Type,
		Message:   wire.Message,
		RequestID: requestID(resp),
		Retryable: wire.Type == "overloaded_error" || wire.Type == "api_error",
	}
}

// errorObject returns the error a 2xx body holds in place of a reply, or nil
// when the body is not the API's error shape.
func errorObject(resp *http.Response, body []byte) *llm.Error {
	var wire struct {
		Type string `json:"type"`
		wireErrorBody
	}
	// A body that is not JSON is not an error object, whatever else it is.
	_ = json.Unmarshal(body, &wire)
	if wire.Type != eventError {
		return nil
	}
	e := sentError(resp, wire.Error)
	if e.RequestID == "" {
		e.RequestID = wire.RequestID
	}
	return e
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

// callError reports a call that got no response, or lost it part way. When
// the caller's context has ended, that is why, and the error is the
// context's: nothing of the provider's and nothing to retry. With the
// context live it is a transport failure or the HTTP client's own timeout,
// and the same request may well succeed, unless what failed it was a
// redirect the client's policy would not follow.
func callError(ctx context.Context, id string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("anthropic: %w", ctxErr)
	}
	// A redirect the client's policy refused would be refused again.
	var refused *refusedRedirect
	return &llm.Error{Provider: Name, RequestID: id, Err: err, Retryable: !errors.As(err, &refused)}
}

// sendError reports a request that got no response to read. resp is what
// net/http returned beside the error, which it does only for the 3xx of a
// redirect the client's policy refused. That one is the server's answer and
// the policy's decision: an error with the status, and not retryable, like
// the 3xx the default client does not follow.
func sendError(ctx context.Context, resp *http.Response, err error) error {
	var refused *refusedRedirect
	if ctx.Err() != nil || resp == nil || !errors.As(err, &refused) {
		return callError(ctx, "", err)
	}
	return &llm.Error{
		Provider:  Name,
		Status:    resp.StatusCode,
		Message:   "the redirect was not followed: " + refused.Error(),
		RequestID: requestID(resp),
		Err:       err,
	}
}

// readError reports a failure to read a response body that is read whole.
func (c *Client) readError(ctx context.Context, resp *http.Response, err error) error {
	if errors.Is(err, errTooLarge) {
		return sizeError("the response body", c.bound)
	}
	return callError(ctx, requestID(resp), err)
}

// sizeError reports a reply past one of this package's size bounds. It is a
// plain error: the API did not fail, and the same request would pass the
// bound again.
func sizeError(what string, bound int64) error {
	return fmt.Errorf("anthropic: %s is larger than %d bytes", what, bound)
}

// streamFailure reports a streamed call that failed before its events began:
// no response, or a body that is not a stream and could not be read. id is
// the response's request id, when there was a response. stalled says the idle
// watch ended the wait, in which case err is only the cancellation the watch
// caused.
func (c *Client) streamFailure(ctx context.Context, id string, err error, stalled bool) error {
	switch {
	case ctx.Err() != nil:
		return callError(ctx, id, err)
	case stalled:
		// The cause is a new error and not the cancelled read: the caller
		// cancelled nothing, and a stalled connection is worth another try.
		return &llm.Error{
			Provider:  Name,
			RequestID: id,
			Err:       fmt.Errorf("the stream sent nothing for %s", c.idle),
			Retryable: true,
		}
	case errors.Is(err, errTooLarge):
		return sizeError("the response body", c.bound)
	default:
		return callError(ctx, id, err)
	}
}

// eventFailure reports a stream whose events stopped before the reply was
// whole.
func (c *Client) eventFailure(ctx context.Context, id string, err error, stalled bool) error {
	var failed *readFailure
	switch {
	case ctx.Err() != nil, stalled, errors.As(err, &failed), errors.Is(err, io.ErrUnexpectedEOF):
		// The caller gave up, the stream stalled, or the connection failed
		// or was cut in the middle of an event.
		return c.streamFailure(ctx, id, err, stalled)
	case errors.Is(err, io.EOF):
		// The stream ended between events and message_stop never came.
		// Whether the connection dropped or the server stopped early, the
		// reply is not whole.
		return callError(ctx, id, fmt.Errorf("the stream ended before message_stop: %w", io.ErrUnexpectedEOF))
	default:
		// The body was read and the event reader would not take it: an
		// event past its bound. This is an inference from what the error is
		// not, until the reader's own error for it can be tested for, so the
		// reader's words are kept. Like any size bound it is a plain error.
		return fmt.Errorf("anthropic: %w", err)
	}
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
