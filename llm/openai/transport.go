package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ManavA/keel/llm"
)

const (
	// defaultTimeout bounds a call that is not a stream, made by the default
	// client. A stream has no bound of this kind: see Options.IdleTimeout.
	defaultTimeout = 10 * time.Minute
	// defaultIdleTimeout is how long a stream may go without a byte.
	defaultIdleTimeout = 2 * time.Minute
	// maxBodyBytes is the most of a response body this package reads, and the
	// most a streamed reply may add up to.
	maxBodyBytes = 32 << 20
	// maxDrainBytes and drainTimeout bound the reading of what follows the
	// end of a stream, done so that the connection can be used again.
	maxDrainBytes = 64 << 10
	drainTimeout  = 100 * time.Millisecond
	// maxRedirects is the limit net/http applies when a client has no policy.
	maxRedirects = 10
)

// noRedirects is the default client's redirect policy: the 3xx is the answer.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// redirectRefused is a redirect the client's policy would not follow, or the
// ten-hop stop of a client with none. It is not the server's fault and asking
// again meets the same redirect.
type redirectRefused struct{ err error }

func (e *redirectRefused) Error() string { return e.err.Error() }
func (e *redirectRefused) Unwrap() error { return e.err }

// guardRedirects returns a copy of a client the caller supplied that keeps its
// policy on redirects and adds one rule: a request sent on to an origin other
// than the configured one, another scheme, host or port, loses the key and the
// headers this package added. net/http would forward them: it drops
// Authorization only when the host name changes, not when the scheme or port
// does, and drops no other header. The caller's own client is not edited.
func (c *Client) guardRedirects(hc *http.Client) *http.Client {
	origin := ""
	if base, err := url.Parse(c.baseURL); err == nil {
		origin = originOf(base)
	}
	guarded := *hc
	policy := hc.CheckRedirect
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if originOf(req.URL) != origin {
			req.Header.Del("Authorization")
			for k := range c.header {
				req.Header.Del(k)
			}
		}
		var err error
		switch {
		case policy != nil:
			err = policy(req, via)
		case len(via) >= maxRedirects:
			err = fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if err != nil && err != http.ErrUseLastResponse { //nolint:errorlint // net/http compares the sentinel by value
			return &redirectRefused{err: err}
		}
		return err
	}
	return &guarded
}

// originOf is the scheme, host and port of u, with the default port of the
// scheme the same as none and the host in lower case. net/url has already
// lowered the scheme.
func originOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// newRequest builds the POST of body to path.
func (c *Client) newRequest(ctx context.Context, path string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	for k, vs := range c.header {
		req.Header[textproto.CanonicalMIMEHeaderKey(k)] = slices.Clone(vs)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

// answer is a successful response, read whole.
type answer struct {
	body      []byte
	status    int
	requestID string
}

// fetch posts body to path and reads the whole answer. An answer that is not
// 2xx comes back as an *llm.Error. The call is bounded by the caller's context
// and, for the default client, by defaultTimeout.
func (c *Client) fetch(ctx context.Context, path string, body []byte) (answer, error) {
	callCtx := ctx
	if c.callTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, c.callTimeout)
		defer cancel()
	}
	req, err := c.newRequest(callCtx, path, body)
	if err != nil {
		return answer{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return answer{}, requestFailed(ctx, resp, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := readBounded(resp.Body)
	switch {
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		// The status is the answer; a body cut short only costs the message.
		return answer{}, httpError(resp, raw)
	case errors.Is(err, errBodyTooLarge):
		return answer{}, err
	case err != nil:
		return answer{}, failed(ctx, err)
	}
	return answer{body: raw, status: resp.StatusCode, requestID: resp.Header.Get("X-Request-Id")}, nil
}

// requestFailed maps an error from sending a request. resp is what came back
// with it, which is only ever the last response of a redirect the client
// refused to follow. That is an *llm.Error with the status the server gave and
// not retryable, like a 3xx the default client does not follow; anything else
// is failed's.
func requestFailed(ctx context.Context, resp *http.Response, err error) error {
	var refused *redirectRefused
	if ctx.Err() != nil || !errors.As(err, &refused) {
		return failed(ctx, err)
	}
	out := &llm.Error{Provider: Name, Err: err, Message: "redirect not followed: " + refused.err.Error()}
	if resp != nil {
		out.Status = resp.StatusCode
		out.RequestID = resp.Header.Get("X-Request-Id")
		if msg := redirectMessage(resp); msg != "" {
			out.Message = msg + ": " + refused.err.Error()
		}
	}
	return out
}

// failed maps an error from the transport. When the caller's context is done
// the call ended because the caller said so, and that error is the answer,
// not a failure of the provider. Otherwise the call got no usable answer
// (the connection failed or was cut, or the client's own timeout passed) and
// the same request may get one.
func failed(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("openai: %w", ctxErr)
	}
	return &llm.Error{Provider: Name, Err: err, Retryable: true}
}

var errBodyTooLarge = fmt.Errorf("openai: response body is larger than %d bytes", maxBodyBytes)

// readBounded reads at most maxBodyBytes of r.
func readBounded(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
	if err != nil {
		return raw, err
	}
	if len(raw) > maxBodyBytes {
		return nil, errBodyTooLarge
	}
	return raw, nil
}

// modelFor is the model a chat request is for.
func (c *Client) modelFor(req llm.Request) string {
	if req.Model != "" {
		return req.Model
	}
	return c.model
}
