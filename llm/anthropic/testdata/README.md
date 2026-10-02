# Test bodies

Every file here is a Messages API response as Anthropic's reference shows or
defines one. The reference was read on 2026-10-02; the package comment lists
the pages. Model names are `claude-opus-5-5`, `claude-sonnet-5-5` and
`claude-haiku-4-5-20251001` throughout, whatever the reference's example
used.

A `.sse` file must end with a blank line. That line ends its last event, and
a stream without it is one cut short.

## Following an example the reference prints

| File | Example | Changed |
|---|---|---|
| `message.json` | The "Response (200)" example of the Messages reference | Nothing but the model name |
| `tool_use.json` | The reply in the tool-call guide | `type`, `stop_sequence` and `usage` added; the keys of `input` put out of alphabetical order |
| `refusal.json` | The refusal in the refusals guide | Nothing but the model name |
| `fallback.json` | The fallback reply in the refusals guide | The `trigger` the beta reference gives a `fallback` block added; the cache counts of the answering attempt made distinct |
| `stream_text.sse` | The basic stream in the streaming guide, with its `ping` | Nothing |
| `stream_tool_use.sse` | The tool-use stream in the streaming guide | Nothing but the model name |

## Composed from the reference's schema

No example of these is printed. Each is put together from the field
definitions and from what the guides say in prose.

| File | Built from |
|---|---|
| `usage.json` | The `Usage` object, every count given a different value |
| `thinking.json` | `ThinkingBlock` and `RedactedThinkingBlock` beside the tool-call guide's reply |
| `refusal_no_category.json` | The refusals guide: `category` and `explanation` are null when a refusal maps to no category |
| `stream_thinking.sse` | The streaming guide on `display: "omitted"`: an empty `thinking_delta`, one `signature_delta`, then the block closes |
| `stream_refusal.sse` | `refusal.json` as the events the streaming guide defines |
| `stream_fallback.sse` | The refusals guide on a decline part way through a stream: the declining model's blocks, a `fallback` start and stop with no deltas, the next model's blocks, and `usage.iterations` on the last `message_delta` |
| `stream_*.json` | The unstreamed body that holds the same reply as the stream of the same name |
