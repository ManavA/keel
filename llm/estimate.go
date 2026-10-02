package llm

// messageOverhead is what each message is counted as costing beyond its
// content: its role, and the structure a provider wraps around a turn.
const messageOverhead = 8

// EstimateInputTokens is an upper estimate of what req costs to send.
//
// It counts one token per three bytes of the system prompt, message text,
// tool arguments, tool results, tool definitions and output schema, rounded
// up, plus eight per message. English text runs nearer four bytes to a token,
// which is what makes this an upper estimate; it is not a tokenizer, and
// exists so a budget can refuse a call before making it.
func EstimateInputTokens(req Request) int64 {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Text)
		for _, c := range m.ToolCalls {
			n += len(c.Input)
		}
		for _, r := range m.ToolResults {
			n += len(r.Content)
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	if req.Output != nil {
		n += len(req.Output.Name) + len(req.Output.Description) + len(req.Output.JSON)
	}
	return (int64(n)+2)/3 + messageOverhead*int64(len(req.Messages))
}
