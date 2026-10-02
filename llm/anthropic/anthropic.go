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
	// A reply that is not streamed has to arrive within the HTTP timeout, so
	// its default bound is the smaller one.
	defaultMaxTokens       = 16000
	defaultStreamMaxTokens = 64000

	defaultTimeout = 10 * time.Minute

	// maxBodyBytes bounds one response body, streamed or not.
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
	// HTTPClient defaults to a client with a 10 minute timeout.
	HTTPClient *http.Client
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
	apiKey    string
	url       string
	model     string
	maxTokens int
	http      *http.Client
	// betas is the anthropic-beta header, "" when there is none to send.
	betas     string
	fallbacks bool
	// extra is Options.Extra, encoded once and in key order, so that the same
	// request is the same bytes on every call.
	extra  []field
	logger *slog.Logger
}

// New builds a Client. It returns an error when APIKey is empty, and when
// RefusalFallback or Extra holds something that cannot be sent.
func New(opts Options) (*Client, error) {
	if opts.APIKey == "" {
		return nil, errors.New("anthropic: APIKey is required")
	}
	if opts.RefusalFallback != "" && opts.RefusalFallback != refusalFallbackDefault {
		return nil, fmt.Errorf("anthropic: RefusalFallback is %q: the only value is %q",
			opts.RefusalFallback, refusalFallbackDefault)
	}

	c := &Client{
		apiKey:    opts.APIKey,
		url:       strings.TrimRight(cmp.Or(opts.BaseURL, DefaultBaseURL), "/") + "/v1/messages",
		model:     cmp.Or(opts.Model, DefaultModel),
		maxTokens: opts.MaxTokens,
		http:      opts.HTTPClient,
		fallbacks: opts.RefusalFallback == refusalFallbackDefault,
		logger:    opts.Logger,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout}
	}
	if c.logger == nil {
		c.logger = slog.Default()
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
	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(&boundedReader{r: resp.Body, left: maxBodyBytes})
	if err != nil {
		return nil, readError(ctx, resp, err)
	}
	if !succeeded(resp) {
		return nil, apiError(resp, data)
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
func (c *Client) Stream(ctx context.Context, req llm.Request, fn func(llm.Delta) error) (*llm.Response, error) {
	body, err := c.body(req, true)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	bounded := &boundedReader{r: resp.Body, left: maxBodyBytes}
	if !succeeded(resp) {
		data, err := io.ReadAll(bounded)
		if err != nil {
			return nil, readError(ctx, resp, err)
		}
		return nil, apiError(resp, data)
	}
	if fn == nil {
		fn = func(llm.Delta) error { return nil }
	}
	return c.readStream(ctx, resp, bounded, fn)
}

var _ llm.Model = (*Client)(nil)

// post sends one request body. The caller closes the response.
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", Version)
	req.Header.Set("content-type", "application/json")
	if c.betas != "" {
		req.Header.Set("anthropic-beta", c.betas)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, "", err)
	}
	return resp, nil
}

func succeeded(resp *http.Response) bool {
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// errTooLarge is what a boundedReader returns for a body past the bound.
var errTooLarge = fmt.Errorf("the response body is larger than %d bytes", maxBodyBytes)

// boundedReader reads up to left bytes of r and fails if r holds more, so a
// body past the bound is an error and not an allocation without limit.
// io.LimitReader would end such a body quietly, and a reply cut off at the
// bound would then pass for a whole one.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.r.Read(one[:])
		if n > 0 {
			return 0, errTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}
