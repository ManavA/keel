// Package openai is the llm.Model and llm.Embedder for the OpenAI chat
// completions protocol, which local runtimes also speak. It is written over
// net/http and encoding/json with no vendor SDK: one POST to
// {BaseURL}/chat/completions for Generate and for Stream, and one to
// {BaseURL}/embeddings for Embed.
//
// Use it for OpenAI itself, or for any server that speaks the protocol by
// pointing [Options.BaseURL] at it. A local runtime wants no [Options.APIKey]
// and its own [Options.Model], since this protocol has no model that is right
// for every server.
//
// # What it sends and what it reads
//
// It sends only what the reference documents, leaving out a field that has
// nothing to say instead of sending its zero value. It reads what a server
// might leave out: a missing id or model, a usage block that arrives only in
// the last chunk of a stream or not at all, a finish reason a runtime does
// not send, and fields it has no use for.
//
// An assistant turn is fully described by its text and tool calls, so
// [llm.Message.Opaque] is never sent and never set. The protocol has no error
// flag on a tool result, so a result with IsError goes back with its content
// prefixed "ERROR: ".
//
// The reference warns that a tool call's arguments are not always valid
// JSON. When they are not, the call is [llm.ToolCall.Malformed] and its Input
// holds the text as one JSON string; sending the call back sends that text.
// Arguments that are empty are an empty object.
//
// # Streams
//
// A stream is complete once a chunk has carried a finish reason. The usage
// arrives after it, on a chunk with no choices, so the reply waits for that
// chunk and for the end of the stream. If the connection is cut after the
// finish reason the reply is returned as it stands, with zero usage when the
// usage chunk was lost, whether or not the closing data: [DONE] arrived. A
// cut before it is a failure of the connection: an [*llm.Error] with Err set
// and Retryable true, unless the context ended. Whatever was delivered to the
// callback before the cut stays delivered.
//
// # Errors
//
// A response that is not 2xx is an [*llm.Error] with the status, the body's
// error type and message, the x-request-id header and the Retry-After header,
// which may be a number of seconds or an HTTP date. It is retryable for 408
// and every 5xx, and for 429 unless the body says that no wait helps: the
// account is out of credit or has reached a spend or usage limit. The
// reference puts those in error.code, and says error.type can be
// insufficient_quota, so both are read. One call is one HTTP request; wrap
// the client in [llm.Retrying] for retries.
//
// # Sources
//
// The request and response shapes are from OpenAI's references, read on
// 2026-10-02:
//
//   - https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create
//   - https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events
//   - https://developers.openai.com/api/reference/resources/embeddings/methods/create
//   - https://developers.openai.com/api/docs/guides/error-codes
//   - https://developers.openai.com/api/reference/overview
//   - https://raw.githubusercontent.com/openai/openai-openapi/manual_spec/openapi.yaml
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/textproto"
	"slices"
	"strings"
	"time"

	"github.com/ManavA/keel/llm"
)

// Name is what this provider writes in llm.Error.Provider.
const Name = "openai"

// DefaultBaseURL is OpenAI's own endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

const (
	// defaultTimeout bounds a whole call, a stream's body included.
	defaultTimeout = 10 * time.Minute
	// maxBodyBytes is the most of a response body this package reads.
	maxBodyBytes = 32 << 20
)

// Options configures a Client. Only Model is required.
type Options struct {
	// APIKey is sent as a bearer token. Empty sends none, which is what a
	// local runtime expects.
	APIKey string
	// BaseURL defaults to DefaultBaseURL. A local runtime is usually
	// http://localhost:11434/v1 or similar.
	BaseURL string
	// Model is used for a request that names none. Required: this protocol
	// has no model that is right for every server.
	Model string
	// EmbeddingModel is used for an EmbedRequest that names none.
	EmbeddingModel string
	// MaxTokens is used for a request that sets none. Zero sends no bound.
	MaxTokens int
	// LegacyMaxTokens sends the bound as max_tokens rather than
	// max_completion_tokens, for servers that only know the older name.
	LegacyMaxTokens bool
	// SystemRole is the role the system prompt is sent under. Default
	// "system"; OpenAI's newer models also accept "developer".
	SystemRole string
	// Header is added to every request, for a gateway that needs one. It
	// cannot replace Content-Type, nor the Authorization header an APIKey
	// sets.
	Header http.Header
	// HTTPClient defaults to a client with a 10 minute timeout.
	HTTPClient *http.Client
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Client calls a chat completions endpoint over net/http.
type Client struct {
	apiKey          string
	baseURL         string
	model           string
	embeddingModel  string
	maxTokens       int
	legacyMaxTokens bool
	systemRole      string
	header          http.Header
	http            *http.Client
	log             *slog.Logger
}

// New builds a Client. It returns an error when Model is empty.
func New(opts Options) (*Client, error) {
	if opts.Model == "" {
		return nil, errors.New("openai: Options.Model is required: this protocol has no model that is right for every server")
	}
	c := &Client{
		apiKey:          opts.APIKey,
		baseURL:         strings.TrimRight(opts.BaseURL, "/"),
		model:           opts.Model,
		embeddingModel:  opts.EmbeddingModel,
		maxTokens:       opts.MaxTokens,
		legacyMaxTokens: opts.LegacyMaxTokens,
		systemRole:      opts.SystemRole,
		header:          opts.Header.Clone(),
		http:            opts.HTTPClient,
		log:             opts.Logger,
	}
	if opts.BaseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.systemRole == "" {
		c.systemRole = "system"
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: defaultTimeout}
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

// Generate implements llm.Model.
func (c *Client) Generate(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body, err := c.chatBody(req, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.post(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var reply wireCompletion
	if err := c.readJSON(ctx, resp.Body, &reply); err != nil {
		return nil, err
	}
	return c.completionResponse(resp, reply, c.modelFor(req))
}

// post sends body to path. A response that is not 2xx is read and returned as
// an *llm.Error; for a 2xx one the caller closes the body.
func (c *Client) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
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

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer func() { _ = resp.Body.Close() }()
		// Whatever of the body can be read is read: the status is the
		// answer, and a body cut short only costs the message.
		raw, _ := readBounded(resp.Body)
		return nil, httpError(resp, raw)
	}
	return resp, nil
}

// transportError is a call that got no usable answer: the connection failed
// or was cut. The same request may get one, unless its context ended.
func transportError(ctx context.Context, err error) *llm.Error {
	return &llm.Error{Provider: Name, Err: err, Retryable: ctx.Err() == nil}
}

// readJSON reads a whole response body into v.
func (c *Client) readJSON(ctx context.Context, body io.Reader, v any) error {
	raw, err := readBounded(body)
	switch {
	case errors.Is(err, errBodyTooLarge):
		return err
	case err != nil:
		return transportError(ctx, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("openai: decode response: %w", err)
	}
	return nil
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

var (
	_ llm.Model    = (*Client)(nil)
	_ llm.Embedder = (*Client)(nil)
)
