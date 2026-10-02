package anthropic

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ManavA/keel/llm"
)

// Name is what this provider writes in llm.Opaque.Provider and llm.Error.Provider.
const Name = "anthropic"

// DefaultModel is used when neither the request nor Options names a model.
const DefaultModel = "claude-opus-5-5"

// DefaultBaseURL is Anthropic's own endpoint.
const DefaultBaseURL = "https://api.anthropic.com"

// Version is the anthropic-version header this package is written against.
const Version = "2023-06-01"

const (
	// A reply that is not streamed has to arrive within the request timeout,
	// so its default bound is the smaller one.
	defaultMaxTokens       = 16000
	defaultStreamMaxTokens = 64000

	// defaultTimeout is the time an unstreamed call is given, start to end.
	defaultTimeout = 10 * time.Minute
	// defaultIdleTimeout is how long a stream may go without a byte.
	defaultIdleTimeout = 2 * time.Minute

	// maxBodyBytes bounds a response body that is read whole, and what the
	// content of a streamed reply may add up to.
	maxBodyBytes = 32 << 20

	// refusalFallbackDefault is the one value Options.RefusalFallback takes,
	// and what the fallbacks field is sent as.
	refusalFallbackDefault = "default"
	// fallbackBeta is the beta the fallbacks field needs. The API accepts
	// "default" under this date and no other.
	fallbackBeta = "server-side-fallback-2026-07-01"
)

// Options configures a Client. Only APIKey is required.
type Options struct {
	// APIKey is sent as x-api-key. Required.
	APIKey string
	// BaseURL defaults to DefaultBaseURL. Tests point it at httptest.
	BaseURL string
	// Model is used for a request that names none. Default DefaultModel.
	Model string
	// MaxTokens is used for a request that sets none. Default 16000 for
	// Generate and 64000 for Stream.
	MaxTokens int
	// HTTPClient defaults to one that gives Generate 10 minutes, puts no
	// limit of its own on a stream, and follows no redirect. A client given
	// here is used for every call and keeps its own policies. Its Timeout,
	// if it has one, cuts a stream short however healthy the stream is. If
	// it follows redirects, a 307 or 308 sends the request body again, and
	// the body holds the prompt: where that goes is the given client's
	// policy. The key and the anthropic headers are withheld from any
	// origin but the configured one whatever the policy.
	HTTPClient *http.Client
	// IdleTimeout is how long a stream may go without a byte from the
	// server before Stream gives it up as stalled. Zero means two minutes,
	// and a negative value no limit.
	IdleTimeout time.Duration
	// Betas are extra anthropic-beta values to send.
	Betas []string
	// RefusalFallback, when "default", asks the API to retry a refused
	// request on the model it recommends. Empty leaves a refusal as the
	// reply.
	RefusalFallback string
	// Extra is merged into every request body, for a field this package
	// does not model. A key this package also writes replaces what it wrote.
	Extra map[string]any
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Client calls the Messages API over net/http.
type Client struct {
	apiKey string
	// base is the address of the API. Its scheme and host are the only place
	// the key is ever sent.
	base      *url.URL
	url       string
	model     string
	maxTokens int
	// http makes the calls that are not streamed, and streamHTTP the ones
	// that are. They differ only when the caller gave no client: a stream
	// then has no whole-request timeout.
	http       *http.Client
	streamHTTP *http.Client
	// idle is how long a stream may go without a byte, or zero for ever.
	idle time.Duration
	// betas is the anthropic-beta header, "" when there is none to send.
	betas     string
	fallbacks bool
	// extra is Options.Extra, encoded once and in key order, so that the same
	// request is the same bytes on every call.
	extra  []field
	logger *slog.Logger
}

// New builds a Client. It returns an error when APIKey is empty, and when
// BaseURL, RefusalFallback or Extra holds something that cannot be used.
func New(opts Options) (*Client, error) {
	return newClient(opts, defaultTimeout)
}

// newClient is New with the time an unstreamed call is given.
func newClient(opts Options, timeout time.Duration) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("anthropic: APIKey is required")
	}
	if opts.RefusalFallback != "" && opts.RefusalFallback != refusalFallbackDefault {
		return nil, fmt.Errorf("anthropic: RefusalFallback is %q: the only value is %q",
			opts.RefusalFallback, refusalFallbackDefault)
	}
	base, err := url.Parse(cmp.Or(opts.BaseURL, DefaultBaseURL))
	if err != nil {
		return nil, fmt.Errorf("anthropic: BaseURL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("anthropic: BaseURL %q has no scheme or no host", opts.BaseURL)
	}

	c := &Client{
		apiKey:    opts.APIKey,
		base:      base,
		url:       base.JoinPath("v1", "messages").String(),
		model:     cmp.Or(opts.Model, DefaultModel),
		maxTokens: opts.MaxTokens,
		fallbacks: opts.RefusalFallback == refusalFallbackDefault,
		logger:    cmp.Or(opts.Logger, slog.Default()),
	}
	if opts.HTTPClient == nil {
		c.http = &http.Client{Timeout: timeout, CheckRedirect: noRedirect}
		c.streamHTTP = &http.Client{CheckRedirect: noRedirect}
	} else {
		c.http = c.guarded(opts.HTTPClient)
		c.streamHTTP = c.http
	}
	switch {
	case opts.IdleTimeout == 0:
		c.idle = defaultIdleTimeout
	case opts.IdleTimeout > 0:
		c.idle = opts.IdleTimeout
	}

	betas := slices.Clone(opts.Betas)
	if c.fallbacks && !slices.Contains(betas, fallbackBeta) {
		betas = append(betas, fallbackBeta)
	}
	c.betas = strings.Join(betas, ",")

	for _, name := range slices.Sorted(maps.Keys(opts.Extra)) {
		value, err := encode(opts.Extra[name])
		if err != nil {
			return nil, fmt.Errorf("anthropic: Extra[%q]: %w", name, err)
		}
		c.extra = append(c.extra, field{name: name, value: value})
	}
	return c, nil
}

// Generate implements llm.Model.
func (c *Client) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body, err := c.body(req, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, c.http, body)
	if err != nil {
		return nil, sendError(ctx, resp, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(&boundedReader{r: resp.Body, left: maxBodyBytes})
	if !succeeded(resp) {
		// The status is the answer. A body that could not be read whole
		// only costs the error its message.
		return nil, apiError(resp, data)
	}
	if err != nil {
		return nil, readError(ctx, resp, err)
	}
	if e := errorObject(resp, data); e != nil {
		return nil, e
	}

	var msg wireMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, replyError(resp, "the reply is not a message: %v", err)
	}
	blocks, err := decodeContent(msg.Content)
	if err != nil {
		return nil, replyError(resp, "the reply is not a message: %v", err)
	}
	return msg.response(blocks, msg.Content), nil
}

// Stream implements llm.Model.
//
// A delta is delivered as it arrives and cannot be taken back. When a reply
// is refused part way, fn has already been given the text and tool calls
// that came before the refusal; the Response carries none of them.
//
// A stream is as long as its reply: it is ended by ctx, or by the server
// sending nothing for Options.IdleTimeout, and not by the time an unstreamed
// call is given. The time fn takes is the caller's and is not counted.
func (c *Client) Stream(ctx context.Context, req llm.Request, fn func(llm.Delta) error) (*llm.Response, error) {
	body, err := c.body(req, true)
	if err != nil {
		return nil, err
	}

	// The stream runs under a context of its own, which the idle watch ends
	// when the stream stalls. Ending the caller's ends it too. Both are gone
	// before Stream returns.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watch := watchIdle(c.idle, cancel)
	defer watch.close()

	// The wait for the response to begin is the first wait on the server.
	watch.begin(c.idle)
	resp, err := c.post(streamCtx, c.streamHTTP, body)
	stalled := watch.end()
	if err != nil {
		if stalled && ctx.Err() == nil {
			return nil, c.streamFailure(ctx, "", err, stalled)
		}
		return nil, sendError(ctx, resp, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !succeeded(resp) || !isEventStream(resp) {
		watch.begin(c.idle)
		data, err := io.ReadAll(&boundedReader{r: streamBody{r: resp.Body, watch: watch}, left: maxBodyBytes})
		stalled := watch.end()
		switch {
		case !succeeded(resp):
			// The status is the answer, as it is for Generate.
			return nil, apiError(resp, data)
		case err != nil || stalled:
			return nil, c.streamFailure(ctx, requestID(resp), err, stalled)
		}
		if e := errorObject(resp, data); e != nil {
			return nil, e
		}
		return nil, replyError(resp, "the reply to a request for a stream is %s, and not an event stream", contentType(resp))
	}
	if fn == nil {
		fn = func(llm.Delta) error { return nil }
	}
	return c.readStream(ctx, resp, watch, fn)
}

var _ llm.Model = (*Client)(nil)

// post sends one request body with hc. The caller closes the response.
func (c *Client) post(ctx context.Context, hc *http.Client, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set(headerKey, c.apiKey)
	req.Header.Set(headerVersion, Version)
	req.Header.Set("content-type", "application/json")
	if c.betas != "" {
		req.Header.Set(headerBeta, c.betas)
	}
	return hc.Do(req)
}

func succeeded(resp *http.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
