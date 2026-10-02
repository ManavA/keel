package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"slices"
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

// guardRedirects returns a copy of a client the caller supplied that keeps its
// policy on redirects and adds one rule: a request sent on to a host other
// than the one first asked loses the key and the headers this package added,
// which net/http would otherwise forward (it drops Authorization only when the
// host name changes, not when the port does, and drops no other header). The
// caller's own client is not edited.
func (c *Client) guardRedirects(hc *http.Client) *http.Client {
	guarded := *hc
	policy := hc.CheckRedirect
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
			for k := range c.header {
				req.Header.Del(k)
			}
		}
		if policy != nil {
			return policy(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
	return &guarded
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
		return answer{}, failed(ctx, err)
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
