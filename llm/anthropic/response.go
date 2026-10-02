package anthropic

import (
	"cmp"
	"encoding/json"
	"errors"

	"github.com/ManavA/keel/llm"
)

// wireMessage is the part of a Message object this package reads. A stream
// fills one in as its events arrive.
type wireMessage struct {
	ID          string           `json:"id"`
	Model       string           `json:"model"`
	Content     json.RawMessage  `json:"content"`
	StopReason  string           `json:"stop_reason"`
	StopDetails *wireStopDetails `json:"stop_details"`
	Usage       wireUsage        `json:"usage"`
}

// wireStopDetails explains a refusal. Both fields are null when the refusal
// maps to no named category.
type wireStopDetails struct {
	Category    string `json:"category"`
	Explanation string `json:"explanation"`
}

// wireCounts is the token counts of one usage object. They are pointers
// because a stream's message_delta sends some as null or not at all, and
// those must not overwrite what message_start gave.
type wireCounts struct {
	InputTokens   *int64 `json:"input_tokens"`
	OutputTokens  *int64 `json:"output_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
	Details       *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type wireUsage struct {
	wireCounts
	// Iterations has one entry per model that worked on the reply. The
	// counts above are those of the one that answered.
	Iterations []wireIteration `json:"iterations"`
}

type wireIteration struct {
	Model string `json:"model"`
	wireCounts
}

func (c wireCounts) usage() llm.Usage {
	u := llm.Usage{
		InputTokens:      deref(c.InputTokens),
		OutputTokens:     deref(c.OutputTokens),
		CacheReadTokens:  deref(c.CacheRead),
		CacheWriteTokens: deref(c.CacheCreation),
	}
	if c.Details != nil {
		u.ReasoningTokens = c.Details.ThinkingTokens
	}
	return u
}

func deref(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// merge takes from later every count it carries. A stream's counts are
// cumulative, so a later one replaces an earlier one and is never added to
// it.
func (u *wireUsage) merge(later wireUsage) {
	if later.InputTokens != nil {
		u.InputTokens = later.InputTokens
	}
	if later.OutputTokens != nil {
		u.OutputTokens = later.OutputTokens
	}
	if later.CacheRead != nil {
		u.CacheRead = later.CacheRead
	}
	if later.CacheCreation != nil {
		u.CacheCreation = later.CacheCreation
	}
	if later.Details != nil {
		u.Details = later.Details
	}
	if later.Iterations != nil {
		u.Iterations = later.Iterations
	}
}

// block is one content block of a reply: the block itself, and the few
// fields of it this package reads.
type block struct {
	// raw is the block as received, or as rebuilt from a stream.
	raw json.RawMessage
	typ string
	// text is a text block's text.
	text string
	// id is the id of a tool_use or server_tool_use block, and name and
	// input the tool and arguments of a tool_use block.
	id    string
	name  string
	input json.RawMessage
	// malformed reports that a streamed tool call's arguments did not add up
	// to JSON. input then holds them as one JSON string.
	malformed bool
	// resultFor is the tool_use_id of a block that is a server tool's
	// result.
	resultFor string
	// toModel is the model a fallback block hands over to.
	toModel string
}

// decodeContent splits a content array into its blocks.
func decodeContent(content json.RawMessage) ([]block, error) {
	if absent(content) {
		return nil, errors.New("it has no content array")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(content, &raws); err != nil {
		return nil, err
	}
	blocks := make([]block, len(raws))
	for i, raw := range raws {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		blocks[i] = readBlock(raw, fields)
	}
	return blocks, nil
}

// readBlock picks out of a block's fields the ones this package reads. It
// reads only those its type is documented to have, so a block of a type
// added later cannot fail to decode.
func readBlock(raw json.RawMessage, fields map[string]json.RawMessage) block {
	b := block{raw: raw, typ: str(fields["type"]), resultFor: str(fields["tool_use_id"])}
	switch b.typ {
	case blockText:
		b.text = str(fields["text"])
	case blockToolUse:
		b.id, b.name, b.input = str(fields["id"]), str(fields["name"]), fields["input"]
		if absent(b.input) {
			b.input = emptyObject
		}
	case blockServerToolUse:
		b.id = str(fields["id"])
	case blockFallback:
		var to struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(fields["to"], &to) == nil {
			b.toModel = to.Model
		}
	}
	return b
}

// str reads raw as a JSON string, and is "" for anything else.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// echo returns the blocks of a turn that the API takes back on the next
// request. Only a turn holding a fallback block loses any: of what came
// before the final fallback block, the reference's table drops thinking,
// redacted thinking, connector text, client tool calls, and server tool
// calls that have no result. Everything else stays where it is, the
// fallback blocks included, since the API validates the thinking around a
// fallback block by its position.
func echo(blocks []block) (kept []block, dropped bool) {
	last := -1
	for i, b := range blocks {
		if b.typ == blockFallback {
			last = i
		}
	}
	if last < 0 {
		return blocks, false
	}

	answered := make(map[string]bool)
	for _, b := range blocks {
		if b.resultFor != "" {
			answered[b.resultFor] = true
		}
	}
	kept = make([]block, 0, len(blocks))
	for i, b := range blocks {
		if i < last {
			switch b.typ {
			case blockThinking, blockRedactedThinking, blockConnectorText, blockToolUse:
				continue
			case blockServerToolUse:
				if !answered[b.id] {
					continue
				}
			}
		}
		kept = append(kept, b)
	}
	return kept, len(kept) < len(blocks)
}

// response builds the reply from the message's fields and its content
// blocks. received is the content array as the bytes the API sent, or nil
// for a reply rebuilt from a stream.
func (m wireMessage) response(blocks []block, received json.RawMessage) *llm.Response {
	// The echo rule runs first, so that the text, the tool calls and the
	// provider's form of the turn all describe the same blocks. A tool call
	// the rule drops must not be reported: its result would answer a block
	// the next request no longer holds.
	kept, dropped := echo(blocks)
	data := received
	if dropped || received == nil {
		raws := make([]json.RawMessage, len(kept))
		for i, b := range kept {
			raws[i] = b.raw
		}
		data = array(raws)
	}

	msg := llm.Message{Role: llm.RoleAssistant, Opaque: &llm.Opaque{Provider: Name, Data: data}}
	for _, b := range kept {
		switch b.typ {
		case blockText:
			msg.Text += b.text
		case blockToolUse:
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: b.id, Name: b.name, Input: b.input, Malformed: b.malformed})
		}
	}

	resp := &llm.Response{
		ID:      m.ID,
		Model:   m.Model,
		Message: msg,
		Stop:    stopReason(m.StopReason),
		Usage:   m.Usage.usage(),
	}
	if resp.Stop == llm.StopRefusal {
		resp.Refusal = &llm.Refusal{}
		if m.StopDetails != nil {
			resp.Refusal.Category = m.StopDetails.Category
			resp.Refusal.Explanation = m.StopDetails.Explanation
		}
	}
	for _, it := range m.Usage.Iterations {
		// Every entry the reference shows names its model. One that did not
		// would be work the answering model was billed for, and an attempt
		// with no model cannot be priced at all.
		resp.Attempts = append(resp.Attempts, llm.Attempt{Model: cmp.Or(it.Model, m.Model), Usage: it.usage()})
	}
	return resp
}

// stopReason maps the API's stop_reason. A value added after this package
// was written is passed through as it is, so that it is not mistaken for a
// reply that ended normally.
func stopReason(wire string) llm.StopReason {
	switch wire {
	case "end_turn":
		return llm.StopEnd
	case "tool_use":
		return llm.StopToolUse
	case "max_tokens":
		return llm.StopMaxTokens
	case "stop_sequence":
		return llm.StopSequence
	case "pause_turn":
		return llm.StopPause
	case "refusal":
		return llm.StopRefusal
	case "model_context_window_exceeded":
		return llm.StopContextWindow
	default:
		return llm.StopReason(wire)
	}
}
