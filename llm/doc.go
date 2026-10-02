// Package llm is how a service asks a language model for the next turn of a
// conversation: text, tool calls, a reply held to a JSON Schema, a streamed
// reply, and, separately, embeddings. It declares the [Model] and [Embedder]
// interfaces and the types that cross them, and knows no provider. The
// providers are the subpackages llm/anthropic and llm/openai, written over
// net/http with no vendor SDK, so importing llm pulls in neither.
//
// One call in, one turn out. This package does not run tools and does not
// loop; the agent package is the loop.
//
// # In-process by default
//
// [Scripted] is a Model that answers from a function of the request, and
// [HashEmbedder] an Embedder that hashes words, so code written against the
// two interfaces runs under go test with no network, no key and no file.
//
// # What a request cannot ask for
//
// There is no tool choice that forces a call. Current Claude models answer
// one with an error, and [Request.Output] gets a structured reply without
// it. [Request.Temperature] is a pointer for a related reason: those models
// reject any value but their default, so it is sent only when set.
//
// # An assistant turn goes back as it came
//
// A provider may return more in a turn than its text and tool calls.
// Anthropic's thinking blocks are signed, and the signature holds only while
// everything before the block is sent back as it was. [Message.Opaque] keeps
// the turn in the provider's own form: store it with the message and send the
// message back unchanged. The provider that produced it replays it, and any
// other builds the turn from Text and ToolCalls, which is how a conversation
// crosses providers.
//
// # A refusal is a reply
//
// A model that declines answers with [StopRefusal] and a nil error, so read
// [Response.Stop] before the text. An error is a call that failed: an
// [*Error] from a provider says whether the same request may succeed later,
// and [Retryable] reads that through any wrapping.
//
// # Money
//
// Cost is an int64 of millionths of a US dollar, because budgets are summed
// and compared over many calls. The package ships no price table: prices
// change, and a library's copy goes stale without anyone noticing. The
// caller supplies [Prices], and a model the table lacks is [ErrNoPrice]
// rather than free.
//
// # Wrappers
//
// [Retrying], [Fallback], [Budgeted] and [Metered] each wrap a Model and are
// one. Compose them, outermost first, as Budgeted, Metered, Fallback, then
// one Retrying per provider: each provider retries its own transient
// failures before the chain moves on, the budget refuses a call before
// anything else sees it, and the budget and the meter each see one call
// however many attempts it took. The meter records only what the budget let
// through.
package llm
