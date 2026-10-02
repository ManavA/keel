package anthropic

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	// Iterations is the record of every sampling pass behind the reply. The
	// counts above are those of the model that answered.
	Iterations wireIterations `json:"iterations"`
}

// maxIterations is the most entries of usage.iterations this package reads.
// The record holds the hops of a fallback, three at most, and the turns of a
// server-side tool loop. An entry costs a struct some fifty times the size
// of the shortest JSON that makes one, so a record without a bound would
// let a small body hold a great deal.
const maxIterations = 4096

// wireIterations is usage.iterations, read one entry at a time so that a
// record past maxIterations is refused before it is held.
type wireIterations []wireIteration

// UnmarshalJSON implements json.Unmarshaler.
func (it *wireIterations) UnmarshalJSON(data []byte) error {
	if absent(data) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return errors.New("usage.iterations is not an array")
	}
	entries := wireIterations{}
	for dec.More() {
		if len(entries) == maxIterations {
			return fmt.Errorf("usage.iterations has more than %d entries", maxIterations)
		}
		var entry wireIteration
		if err := dec.Decode(&entry); err != nil {
			return fmt.Errorf("usage.iterations: %w", err)
		}
		entries = append(entries, entry)
	}
	*it = entries
	return nil
}

// wireIteration is one entry of usage.iterations: one sampling pass, by one
// model. The beta Messages reference gives it a type of message,
// fallback_message, compaction or advisor_message.
type wireIteration struct {
	Type  string `json:"type"`
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
	// unsigned reports a thinking block with no signature, which the API
	// will not take back.
	unsigned bool
	// toModel is the model a fallback block hands over to, and declined why
	// the model before it handed over.
	toModel  string
	declined decline
}

// decline is what is known of why a model declined a request: the trigger
// of a fallback block, or the stop_details of a refused reply.
type decline struct {
	// known is false when the API did not say, which is not the same as its
	// saying the refusal has no category.
	known    bool
	category string
}

// unbilled reports whether an attempt that ended in d and produced output
// tokens is one the API does not bill. The refusals reference: an attempt
// that produced output is billed, and one that declined before any output is
// billed in some categories and not in others. As the page stood in
// September 2026 the ones not billed are cyber, general_harms and a refusal
// with no category. The page says the billed set may change, so a category
// this package does not know is taken as billed, and so is a decline the API
// gave no reason for: a cost may then be too high and is never too low.
func (d decline) unbilled(output int64) bool {
	if output > 0 || !d.known {
		return false
	}
	switch d.category {
	case "", "cyber", "general_harms":
		return true
	}
	return false
}

// errTooManyBlocks is decodeContent's error for a content array whose blocks
// cost more to hold than the bound allows.
var errTooManyBlocks = errors.New("the content has more blocks than this package holds")

// decodeContent splits a content array into its blocks. The blocks are read
// one at a time and each is counted as a stream counts it, its bytes and
// blockOverhead, so that a body of very many very small blocks is refused
// at the bound and not held as so many structs.
func decodeContent(content json.RawMessage, bound int64) ([]block, error) {
	if absent(content) {
		return nil, errors.New("it has no content array")
	}
	dec := json.NewDecoder(bytes.NewReader(content))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, errors.New("its content is not an array")
	}
	var blocks []block
	var size int64
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		if size += blockOverhead + int64(len(raw)); size > bound {
			return nil, errTooManyBlocks
		}
		b, err := readBlock(raw)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return blocks, nil
}

// blockFields is the fields of a content block that this package reads, each
// as it was written. Every other field of a block is passed over without
// being held, so what a block costs to read is its own bytes and not a map
// of however many fields it has.
type blockFields struct {
	Type      json.RawMessage `json:"type"`
	Text      json.RawMessage `json:"text"`
	Thinking  json.RawMessage `json:"thinking"`
	Signature json.RawMessage `json:"signature"`
	ID        json.RawMessage `json:"id"`
	Name      json.RawMessage `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID json.RawMessage `json:"tool_use_id"`
	To        json.RawMessage `json:"to"`
	Trigger   json.RawMessage `json:"trigger"`
}

// fieldsOf reads the fields of a content block. A block that is not an
// object is an error. The fields are raw so that one of a type added later,
// whatever its fields hold, cannot fail to decode.
func fieldsOf(raw json.RawMessage) (blockFields, error) {
	var fields blockFields
	if absent(raw) {
		return fields, errors.New("a content block is null")
	}
	err := json.Unmarshal(raw, &fields)
	return fields, err
}

// readBlock picks out of a block the fields this package reads. It reads
// only those its type is documented to have.
func readBlock(raw json.RawMessage) (block, error) {
	fields, err := fieldsOf(raw)
	if err != nil {
		return block{}, err
	}
	b := block{raw: raw, typ: str(fields.Type), resultFor: str(fields.ToolUseID)}
	switch b.typ {
	case blockText:
		b.text = str(fields.Text)
	case blockThinking:
		b.unsigned = str(fields.Signature) == ""
	case blockToolUse:
		b.id, b.name, b.input = str(fields.ID), str(fields.Name), fields.Input
		if absent(b.input) {
			b.input = emptyObject()
		}
	case blockServerToolUse:
		b.id = str(fields.ID)
	case blockFallback:
		var to struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(fields.To, &to) == nil {
			b.toModel = to.Model
		}
		var trigger *struct {
			Category string `json:"category"`
		}
		if json.Unmarshal(fields.Trigger, &trigger) == nil && trigger != nil {
			b.declined = decline{known: true, category: trigger.Category}
		}
	}
	return b, nil
}

// str reads raw as a JSON string, and is "" for anything else.
func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// signed returns the blocks of a turn without the thinking the API would
// not take back. A thinking block with no signature fails when it is sent
// back, so it is dropped; and the reference says a gap in the run of
// thinking blocks invalidates the ones after it, so every thinking block
// after it in the turn is dropped too. What is left is a run with its end
// removed, which the reference allows. dropped is how many blocks went.
func signed(blocks []block) (kept []block, dropped int) {
	from := -1
	for i, b := range blocks {
		if b.unsigned {
			from = i
			break
		}
	}
	if from < 0 {
		return blocks, 0
	}
	kept = make([]block, 0, len(blocks))
	for i, b := range blocks {
		if i >= from && (b.typ == blockThinking || b.typ == blockRedactedThinking) {
			dropped++
			continue
		}
		kept = append(kept, b)
	}
	return kept, dropped
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
// for a reply rebuilt from a stream. unsigned is how many thinking blocks
// were left out of the provider's form of the turn for want of a signature.
func (m wireMessage) response(blocks []block, received json.RawMessage) (resp *llm.Response, unsigned int) {
	resp = &llm.Response{
		ID:      m.ID,
		Model:   m.Model,
		Message: llm.Message{Role: llm.RoleAssistant},
		Stop:    stopReason(m.StopReason),
	}
	if resp.Stop == llm.StopRefusal {
		// A refused reply hands nothing on. The reference says to treat any
		// partial output as incomplete and discard it, and a tool call from
		// a turn the model refused must never reach whatever runs tools. The
		// turn has no form to replay either: a refused request is sent again
		// as it was, to another model, and not continued.
		resp.Refusal = &llm.Refusal{}
		if m.StopDetails != nil {
			resp.Refusal.Category = m.StopDetails.Category
			resp.Refusal.Explanation = m.StopDetails.Explanation
		}
	} else {
		resp.Message, unsigned = turn(blocks, received)
	}
	resp.Usage, resp.Attempts = m.billed(blocks, resp.Stop == llm.StopRefusal)
	return resp, unsigned
}

// warnUnsigned says that thinking was left out of a turn's provider form.
func (c *Client) warnUnsigned(ctx context.Context, resp *llm.Response, dropped int) {
	if dropped == 0 {
		return
	}
	c.logger.WarnContext(ctx, "anthropic: a thinking block came with no signature, so it and the thinking after it in the turn are left out of what is replayed",
		"blocks", dropped, "message_id", resp.ID, "model", resp.Model)
}

// turn builds the assistant turn from the content blocks of a reply.
func turn(blocks []block, received json.RawMessage) (msg llm.Message, unsigned int) {
	// The echo rule runs first, so that the text, the tool calls and the
	// provider's form of the turn all describe the same blocks. A tool call
	// the rule drops must not be reported: its result would answer a block
	// the next request no longer holds.
	kept, dropped := echo(blocks)
	kept, unsigned = signed(kept)
	data := received
	if dropped || unsigned > 0 || received == nil {
		raws := make([]json.RawMessage, len(kept))
		for i, b := range kept {
			raws[i] = b.raw
		}
		data = array(raws)
	}

	msg = llm.Message{Role: llm.RoleAssistant, Opaque: &llm.Opaque{Provider: Name, Data: data}}
	for _, b := range kept {
		switch b.typ {
		case blockText:
			msg.Text += b.text
		case blockToolUse:
			msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{ID: b.id, Name: b.name, Input: b.input, Malformed: b.malformed})
		}
	}
	return msg, unsigned
}

// billed returns what the reply was billed for: the usage of the model that
// answered, and one attempt per usage.iterations entry.
//
// The API reports the tokens of every attempt, and does not bill all of
// them: a model that declined before producing any output is billed only in
// some categories. Such an attempt stays in the list, so the record shows it
// ran, with no usage against it. What remains adds up to what the reference
// says is billed.
func (m wireMessage) billed(blocks []block, refused bool) (llm.Usage, []llm.Attempt) {
	usage := m.Usage.usage()
	// last is why the reply itself was refused. A reply that was not refused
	// has no decline, which reads as one that is billed.
	var last decline
	if refused && m.StopDetails != nil {
		last = decline{known: true, category: m.StopDetails.Category}
	}
	if last.unbilled(usage.OutputTokens) {
		usage = llm.Usage{}
	}

	entries := m.Usage.Iterations
	if len(entries) == 0 {
		return usage, nil
	}

	// A hop is one model's run of entries: a model that declined, or the one
	// that answered, each of which may have sampled more than once in a
	// server-side tool loop. Entries that are not a model's turn at the
	// reply, a compaction for one, belong to no hop and are billed as given.
	//
	// Every entry of a model's turn names its model in the reference, though
	// the field may be null, and a compaction entry names none. Either is
	// taken as work of the model that answered: an attempt with no model
	// could not be priced at all.
	attempts := make([]llm.Attempt, len(entries))
	hop := make([]int, len(entries))
	var output []int64
	previous := ""
	for i, entry := range entries {
		model := cmp.Or(entry.Model, m.Model)
		attempts[i].Model = model
		hop[i] = -1
		if entry.Type != "" && entry.Type != iterationMessage && entry.Type != iterationFallback {
			continue
		}
		if len(output) == 0 || model != previous {
			output = append(output, 0)
		}
		previous = model
		hop[i] = len(output) - 1
		output[hop[i]] += deref(entry.OutputTokens)
	}

	// Every hop but the last declined, and the fallback blocks say why, one
	// per hop and in order. When the two do not line up there is no telling
	// which reason is whose, and nothing is taken as free.
	var declines []decline
	for _, b := range blocks {
		if b.typ == blockFallback {
			declines = append(declines, b.declined)
		}
	}
	free := make([]bool, len(output))
	if len(declines) == len(output)-1 {
		for i, d := range declines {
			free[i] = d.unbilled(output[i])
		}
	}
	if len(output) > 0 {
		free[len(output)-1] = last.unbilled(output[len(output)-1])
	}

	for i, entry := range entries {
		if hop[i] < 0 || !free[hop[i]] {
			attempts[i].Usage = entry.usage()
		}
	}
	return usage, attempts
}

// The types of usage.iterations entry that are a model's turn at the reply.
const (
	iterationMessage  = "message"
	iterationFallback = "fallback_message"
)

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
