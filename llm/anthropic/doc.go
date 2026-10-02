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
// # A refusal is a reply
//
// A declined request is HTTP 200 with llm.StopRefusal and a nil error. With
// [Options.RefusalFallback] the API retries a refusal on another model
// inside the same call. That is a beta, so it is off unless asked for. The
// reply then names the model that answered, lists each billed attempt in
// llm.Response.Attempts, and holds a fallback block where one model handed
// over to the next. Before such a turn is stored, the blocks the API will
// not take back are removed from it, and a tool call among them is not
// reported as a call to run.
//
// # Streaming
//
// Stream returns the Response Generate would have. The content array is
// rebuilt from the events, so the bytes are this package's and the values
// the server's. A stream may carry a delta type newer than this package, and
// then the array cannot be rebuilt as the server holds it: Opaque is left
// nil for that reply, a line is logged at Warn, and the turn is replayed
// from Text and ToolCalls, losing its thinking. A stream that stops before
// its message_stop event is a failed call, retryable like any transport
// failure; one that is cut after it is complete.
//
// # Errors
//
// A response that is not 2xx is an *llm.Error carrying the status, the API's
// own error type, the request id and the wait it asked for. 408, 409 and
// every 5xx are retryable. A 429 is retryable only when it carries
// retry-after: without it the organization has reached a spend cap, and the
// same request keeps failing until the cap is lifted.
//
// # Sources
//
// The wire format is from Anthropic's reference as read on 2026-10-02:
//
//	https://platform.claude.com/docs/en/api/messages
//	https://platform.claude.com/docs/en/build-with-claude/streaming
//	https://platform.claude.com/docs/en/build-with-claude/structured-outputs
//	https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls
//	https://platform.claude.com/docs/en/api/errors
//	https://platform.claude.com/docs/en/api/rate-limits
//	https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback
//	https://platform.claude.com/docs/en/build-with-claude/preserved-thinking
package anthropic
