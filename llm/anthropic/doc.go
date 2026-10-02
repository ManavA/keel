// Package anthropic is the llm.Model for Anthropic's Messages API.
//
// It is a provider for the llm package: build a [Client] with [New] and hand
// it to anything that takes an llm.Model, alone or under llm's wrappers. It
// speaks to the API over net/http and encoding/json, with no vendor SDK, so
// importing it adds nothing to a binary but this package. A Client makes one
// HTTP request per call and never retries; wrap it in llm.Retrying for that.
// There is no Embedder here: Anthropic has no embeddings endpoint.
//
// # An assistant turn goes back as it came
//
// A reply holds more than its text and tool calls. Its thinking blocks are
// signed, and the API accepts a signature only while the system prompt, the
// tools and every earlier message are what they were when the block was
// made. So a reply's whole content array is kept in llm.Message.Opaque, every
// block type included, a thinking block with no text and a type this package
// has never heard of among them. Send the message back unchanged and the
// array is written into the next request byte for byte. The request body is
// assembled by hand for that reason: json.Marshal would compact the array
// and rewrite <, > and & inside it. Tools are always sent, in the order
// given, and turning them off for one call is tool_choice none, never an
// empty list, since the tools are part of what a signature covers.
//
// A turn that came from another provider, or has no provider form, is built
// from its Text and ToolCalls, which the API accepts as an appended turn.
//
// # What a request cannot say
//
// The models this package is written for answer a forced tool choice with a
// 400 and reject any temperature but their default, so there is no forced
// choice and Temperature is sent only when set. Thinking is left to the
// model: no thinking field is sent, and llm.Request.Effort is the control.
// [Options.Extra] reaches any field this package does not model.
//
// Citations are not mapped. A reply that carries them unstreamed keeps them
// in Opaque with the rest of its content; a streamed one delivers them in a
// delta this package does not fold in, and is treated as the section on
// streaming says of any delta it does not know.
//
// # A refusal is a reply, and an empty one
//
// A declined request is HTTP 200 with llm.StopRefusal and a nil error. The
// Response says why in Refusal and carries nothing else: no text, no tool
// calls and no Opaque, whatever the model wrote before it was stopped. The
// reference says to discard that partial output, a tool call from a refused
// turn must never be run, and a refused request is sent again to another
// model as it was, not continued, so there is nothing to replay.
//
// With [Options.RefusalFallback] the API retries a refusal on another model
// inside the same call. That is a beta, so it is off unless asked for. The
// reply then names the model that answered and holds a fallback block where
// one model handed over to the next. Before such a turn is stored, the
// blocks the API will not take back are removed from it, and a tool call
// among them is not reported as a call to run.
//
// # Usage is what was billed
//
// The API reports the tokens of every attempt and does not bill all of them.
// A model that declines before producing any output is billed only when its
// refusal is in one of three categories (bio, frontier_llm and
// reasoning_extraction, as the reference stood in September 2026). So a
// reply refused that way in any other category has a zero Usage, and after a
// fallback llm.Response.Attempts lists every attempt, in order, with no
// usage against one that was not billed. The category of a declined attempt
// comes from the trigger of its fallback block. Where the API does not give
// one, the attempt is counted as billed: a cost may then be too high, and is
// never too low.
//
// # Streaming
//
// Stream returns the Response Generate would have. The content array is
// rebuilt from the events, so the bytes are this package's and the values
// the server's. A stream may carry a delta type newer than this package, and
// then the array cannot be rebuilt as the server holds it: Opaque is left
// nil for that reply, a line is logged at Warn, and the turn is replayed
// from Text and ToolCalls, losing its thinking.
//
// A stream is as long as its reply. It is not held to the ten minutes an
// unstreamed call is given. What ends it early is the caller's context, or
// the server sending nothing for [Options.IdleTimeout] while Stream waits on
// it: no event, no keep-alive ping, no comment line. That is a stalled
// connection and a retryable failure, with no context error in its chain,
// so it is not mistaken for the caller's own cancellation. The time the
// caller's fn takes over a delta is not counted. One event may be 16 MiB,
// and all that a reply keeps 32 MiB, the bound on an unstreamed body; past
// either the call fails with a plain error, since the same request would
// pass the bound again. A stream that stops before its message_stop event is
// a failed call, retryable like any transport failure; one that is cut after
// it is complete. Nothing Stream starts is still running when it returns.
//
// # Errors
//
// A response that is not 2xx is an *llm.Error carrying the status, the API's
// own error type, the request id and the wait it asked for. 408, 409 and
// every 5xx are retryable. A 429 is retryable only when it carries
// retry-after: without it the organization has reached a spend cap, and the
// same request keeps failing until the cap is lifted. An error object under
// a 200, as a body or as a stream's error event, is an *llm.Error too, and a
// 200 that answers a request for a stream with anything but an event stream
// is an error that names what it sent.
//
// When a call fails and the caller's context has ended, the error is the
// context's and not an *llm.Error: the caller gave up, and there is nothing
// to retry. With the context live, a connection that failed and the HTTP
// client's own timeout are *llm.Error values marked retryable.
//
// # Redirects and the key
//
// The client this package builds follows no redirect: a 3xx comes back as an
// *llm.Error. net/http would repeat a 307 with its body and with x-api-key,
// which is not among the headers it withholds from another host. A client
// given in [Options.HTTPClient] keeps its own redirect policy, and whatever
// it follows, the key and the anthropic headers go to no origin but the
// configured one: not to another scheme, host or port. The request body,
// which holds the prompt, goes where that client's policy takes it.
//
// # Sources
//
// The wire format is from Anthropic's reference as read on 2026-10-02:
//
//	https://platform.claude.com/docs/en/api/messages
//	https://platform.claude.com/docs/en/api/beta/messages/create
//	https://platform.claude.com/docs/en/build-with-claude/streaming
//	https://platform.claude.com/docs/en/build-with-claude/structured-outputs
//	https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls
//	https://platform.claude.com/docs/en/api/errors
//	https://platform.claude.com/docs/en/api/rate-limits
//	https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback
//	https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons
//	https://platform.claude.com/docs/en/build-with-claude/preserved-thinking
//
// No test here reaches the network or needs a key. One file of tests does,
// and is built only with the live tag. It skips unless ANTHROPIC_API_KEY is
// set, and never runs in CI:
//
//	go test -tags=live ./llm/anthropic/ -run Live
package anthropic
