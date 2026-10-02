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
// holds the text as one JSON string; sending the call back sends that text, as
// the model wrote it. A server that parses assistant history strictly may
// refuse the turns that follow such a call. Arguments that are empty are an
// empty object.
//
// # Streams
//
// A reply is complete once a chunk has carried a finish reason. The usage
// arrives after it, on a chunk with no choices, so the reply waits for that
// chunk and for the end of the stream. If the connection is cut after the
// finish reason the reply is returned as it stands, with zero usage when the
// usage chunk was lost, whether or not the closing data: [DONE] arrived.
//
// Some servers never send a finish reason. A stream that ends cleanly with
// data: [DONE] and no finish reason is complete too, and its stop reason is
// read from what the reply holds: [llm.StopToolUse] when it made tool calls,
// [llm.StopRefusal] for a refusal, [llm.StopEnd] otherwise. The Response has no
// field for the missing reason, so it is logged at debug level. A [DONE] that
// follows nothing at all, no chunk with an id or a choice, is not a reply.
//
// A stream that stops with neither a finish reason nor [DONE], whether it is
// cut or simply ends, is a failure of the connection: an [*llm.Error] with Err
// set and Retryable true, unless the caller's context ended. Whatever was
// delivered to the callback before the cut stays delivered.
//
// A stream is bounded by its context, by [Options.IdleTimeout] between bytes,
// by 16 MiB in one event and by 32 MiB in all that the reply assembles, and
// not by a timeout on the whole request, which would cut a long reply that is
// going well. A reply that outgrows a size bound is an error that is not
// retryable, since a retry meets the same size. After a stream ends cleanly a
// small bounded amount of what follows is read, so that the connection is
// used again.
//
// A 200 in answer to a streaming request that is not an event stream, such as
// a page from a proxy or the whole completion from a server that ignored
// stream, is an error that names the content type and is not retryable.
//
// # Errors
//
// A response that is not 2xx is an [*llm.Error] with the status, the body's
// error type and message, the x-request-id header and the Retry-After header,
// which may be a number of seconds or an HTTP date. It is retryable for 408
// and every 5xx, and for 429 unless the body says that no wait helps: the
// account is out of credit or has reached a spend or usage limit. The
// reference puts those in error.code, and says error.type can be
// insufficient_quota, so both are read. An error object inside a 200 response,
// whether in a completion, an embeddings reply or a stream, is an [*llm.Error]
// too, retryable when it is a rate limit, an overload or a server error.
//
// When a call fails and the caller's context is done, the error is the
// context's and is not retryable. When the context is live, a failed or cut
// connection, and the client's own timeout, are an [*llm.Error] marked
// retryable. One call is one HTTP request; wrap the client in [llm.Retrying]
// for retries.
//
// The default client does not follow redirects: a 3xx is an [*llm.Error], and
// nothing is sent to a host other than the configured one. A client given in
// the options keeps its own policy, and still has the key and [Options.Header]
// withheld from a host it is sent on to.
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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ManavA/keel/llm"
)

// Name is what this provider writes in llm.Error.Provider.
const Name = "openai"

// DefaultBaseURL is OpenAI's own endpoint.
const DefaultBaseURL = "https://api.openai.com/v1"

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
	// HTTPClient defaults to a client that follows no redirects and has no
	// timeout of its own: a call that is not a stream is bounded by 10
	// minutes, and a stream by IdleTimeout. A client given here keeps its own
	// timeout, which then bounds a stream too, and its own policy on
	// redirects, which may follow one. The key and Header are still withheld
	// from a host other than the one first asked.
	HTTPClient *http.Client
	// IdleTimeout is how long a stream may go without a byte, from the
	// request being sent to the end of the stream. Zero is two minutes;
	// negative is no limit.
	IdleTimeout time.Duration
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
	// callTimeout bounds a call that is not a stream, for the default client;
	// zero for a client the caller supplied, which has its own.
	callTimeout time.Duration
	// idleTimeout is how long a stream may go quiet; zero is no limit.
	idleTimeout time.Duration
	// maxReplyBytes is the most a streamed reply may add up to.
	maxReplyBytes int
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
		log:             opts.Logger,
		maxReplyBytes:   maxBodyBytes,
	}
	if opts.BaseURL == "" {
		c.baseURL = DefaultBaseURL
	}
	if c.systemRole == "" {
		c.systemRole = "system"
	}
	if opts.HTTPClient == nil {
		c.http = &http.Client{CheckRedirect: noRedirects}
		c.callTimeout = defaultTimeout
	} else {
		c.http = c.guardRedirects(opts.HTTPClient)
	}
	switch {
	case opts.IdleTimeout == 0:
		c.idleTimeout = defaultIdleTimeout
	case opts.IdleTimeout > 0:
		c.idleTimeout = opts.IdleTimeout
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
	ans, err := c.fetch(ctx, "/chat/completions", body)
	if err != nil {
		return nil, err
	}

	var comp wireCompletion
	if err := json.Unmarshal(ans.body, &comp); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	return c.completionResponse(ans, comp, c.modelFor(req))
}

var (
	_ llm.Model    = (*Client)(nil)
	_ llm.Embedder = (*Client)(nil)
)
