# Test bodies

Which of these follow the vendor reference's documented shape, and which were
composed from its schema. The references were read on 2026-10-02; their
addresses are in the package comment of `openai.go`.

| File | Source |
|---|---|
| `chat_completion.json` | The reference's own example response for Create chat completion, as printed. |
| `chat_completion_tool_calls.json` | The reference's own example for a call with tools, as printed, including the whitespace in `arguments`. |
| `embeddings.json` | The reference's own example response for Create embeddings, as printed. |
| `chat_completion_stream.sse` | **Partly composed.** The reference's streaming sample has three chunks (the role chunk, a `Hello` chunk and the `stop` chunk) with a comment line, "Intermediate chat completion chunks omitted", between them, and shows no usage chunk. The first, second and fourth events here are those three chunks as printed. The third event (a further content chunk) and the fifth (the final usage chunk, whose `choices` is empty, as the `include_usage` option is documented to send it) are composed to the documented chunk shape. The comment line is left out, since it is the page's and not the wire's. |

The bodies of the errors that the tests send are not files. The error guide
has no JSON example, so they are composed in the tests from the `Error` and
`ErrorResponse` schemas in the OpenAPI specification
(`{"error":{"message","type","param","code"}}`, with `param` and `code`
nullable), and from the codes and types the guide names.

Every other body in the tests, completions, chunks and embeddings replies with
a field missing, a server's own variation or an error inside a 200, is written
in the test that sends it and says in its name what it is.
