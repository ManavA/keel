package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/app"
	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/policy"
)

// stubModel is an llm.Model that answers from a function and keeps the
// requests it was handed, as handed: not copied, so a test can see whether the
// adapter shares memory with the caller. llm.Scripted serves the tests of the
// request. It cannot return an opaque form, a list of attempts or an error of
// the test's choosing, so the tests of the reply use this.
type stubModel struct {
	mu    sync.Mutex
	reqs  []llm.Request
	reply func(req llm.Request) (*llm.Response, error)
}

var _ llm.Model = (*stubModel)(nil)

func (s *stubModel) Generate(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.reply(req)
}

func (s *stubModel) Stream(context.Context, llm.Request, func(llm.Delta) error) (*llm.Response, error) {
	return nil, errors.New("stub: the adapter has no use for a stream")
}

func (s *stubModel) requests() []llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llm.Request(nil), s.reqs...)
}

// answering is a stub that always returns resp.
func answering(resp *llm.Response) *stubModel {
	return &stubModel{reply: func(llm.Request) (*llm.Response, error) { return resp, nil }}
}

// failing is a stub that always returns err.
func failing(err error) *stubModel {
	return &stubModel{reply: func(llm.Request) (*llm.Response, error) { return nil, err }}
}

// plainReply is a reply with nothing in it but an end of turn.
func plainReply() *llm.Response {
	return &llm.Response{
		Model:   "model-a",
		Message: llm.Message{Role: llm.RoleAssistant, Text: "done"},
		Stop:    llm.StopEnd,
	}
}

// asUser is the smallest request a model can be asked.
func asUser(model, text string) agent.Request {
	return agent.Request{
		Model:    model,
		Messages: []agent.Message{{Role: agent.RoleUser, Text: text}},
	}
}

// echoScript answers every request with the same text, whatever turn it is.
func echoScript(llm.Request, int) (llm.Reply, error) { return llm.Reply{Text: "ok"}, nil }

// isTheError reports whether got is want itself and not an error that wraps it.
func isTheError(got, want error) bool {
	return got == want //nolint:errorlint // identity is what is asserted
}

// generate calls m and fails the test on an error.
func generate(t *testing.T, m agent.Model, req agent.Request) agent.Response {
	t.Helper()
	resp, err := m.Generate(context.Background(), req)
	require.NoError(t, err)
	return resp
}

// twoPrices is a made-up table with a different price for each kind of token,
// so that a cost computed from the wrong field cannot match.
func twoPrices() llm.Prices {
	return llm.Prices{
		"model-a": {Input: 3_000_000, Output: 15_000_000, CacheRead: 300_000, CacheWrite: 3_750_000},
		"model-b": {Input: 250_000, Output: 1_250_000},
	}
}

// usageA costs, at model-a's prices, 3000 + 60 + 60 + 113 (rounded up from
// 112.5) = 3233 millionths of a dollar: 1000 input tokens, 4 output, 200 read
// from the cache and 30 written to it.
var usageA = llm.Usage{InputTokens: 1000, OutputTokens: 4, CacheReadTokens: 200, CacheWriteTokens: 30, ReasoningTokens: 2}

const costA = 3233

// usageB costs, at model-b's prices, 1000 + 1250 = 2250.
var usageB = llm.Usage{InputTokens: 4000, OutputTokens: 1000}

const costB = 2250

// oddJSON holds forms of JSON that a decode and encode would change and that a
// provider's replay of an assistant turn cannot survive being changed: keys in
// no order, odd spacing, an escaped character, a number written with a
// trailing zero or an exponent or past a float's range, a repeated key.
var oddJSON = []struct {
	name string
	data string
}{
	{"keys out of order", `{"z":1,"a":2,"m":{"y":true,"b":null}}`},
	{"odd spacing", "{ \"type\" :\"thinking\",\n\t\"signature\":  \"c2ln\" , \"a\"\t:[1 ,2,  3]  }\n"},
	{"escapes", `{"text":"caf\u00e9 \ud83d\ude00 \"quoted\" \/ \n"}`},
	{"numbers as written", `{"a":1.0,"b":1E2,"c":-0,"d":12345678901234567890123}`},
	{"a repeated key", `{"k":1,"k":2}`},
	{"an array of objects", `[{"b":1},{"a":2}]`},
	{"a bare string with spaces round it", `   "just text"  `},
}

func rawOf(s string) json.RawMessage { return json.RawMessage(s) }

// What an llm.Scripted received, field by field.
func TestAgentModel_Request(t *testing.T) {
	req := agent.Request{
		RunID:  "run-1",
		Agent:  "support",
		Model:  "model-a",
		System: "Be brief.",
		Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "What is 2+2, and who is the mayor?"},
			{
				Role: agent.RoleAssistant,
				Text: "Let me look.",
				Calls: []agent.Call{
					{ID: "call-1", Name: "add", Input: rawOf(`{ "b": 2,"a":2 }`)},
					{ID: "call-2", Name: "lookup", Input: rawOf(`"not json {"`), Malformed: true},
				},
				Opaque: &agent.Opaque{Provider: "anthropic", Data: rawOf(oddJSON[1].data)},
			},
			{
				Role: agent.RoleTool,
				Results: []agent.Result{
					{CallID: "call-1", Content: "4"},
					{CallID: "call-2", Content: "no such tool", IsError: true},
				},
			},
		},
		Tools: []agent.ToolSpec{
			{Name: "add", Description: "Adds two numbers.", Schema: rawOf(`{"type":"object","properties":{"a":{"type":"number"}}}`)},
			{Name: "lookup"},
		},
		Output:    rawOf(`{"type": "object" , "properties": {"z": {}, "a": {}}}`),
		MaxTokens: 777,
	}
	scripted := llm.NewScripted(echoScript, llm.ScriptedOptions{})
	m := app.AgentModel(scripted, app.AgentModelOptions{Effort: llm.EffortHigh, StrictTools: true})

	generate(t, m, req)

	got := scripted.Requests()
	require.Len(t, got, 1)
	r := got[0]

	t.Run("Model, System and MaxTokens are the same fields", func(t *testing.T) {
		assert.Equal(t, "model-a", r.Model)
		assert.Equal(t, "Be brief.", r.System)
		assert.Equal(t, 777, r.MaxTokens)
	})
	t.Run("Messages: Role, Text, Calls, Results and Opaque, field for field", func(t *testing.T) {
		require.Len(t, r.Messages, 3)

		assert.Equal(t, llm.Message{Role: llm.RoleUser, Text: "What is 2+2, and who is the mayor?"}, r.Messages[0])

		assert.Equal(t, llm.RoleAssistant, r.Messages[1].Role)
		assert.Equal(t, "Let me look.", r.Messages[1].Text)
		assert.Equal(t, []llm.ToolCall{
			{ID: "call-1", Name: "add", Input: rawOf(`{ "b": 2,"a":2 }`)},
			{ID: "call-2", Name: "lookup", Input: rawOf(`"not json {"`), Malformed: true},
		}, r.Messages[1].ToolCalls)
		assert.Empty(t, r.Messages[1].ToolResults)
		assert.Equal(t, &llm.Opaque{Provider: "anthropic", Data: rawOf(oddJSON[1].data)}, r.Messages[1].Opaque)

		assert.Equal(t, llm.RoleTool, r.Messages[2].Role)
		assert.Empty(t, r.Messages[2].Text)
		assert.Empty(t, r.Messages[2].ToolCalls)
		assert.Equal(t, []llm.ToolResult{
			{CallID: "call-1", Content: "4"},
			{CallID: "call-2", Content: "no such tool", IsError: true},
		}, r.Messages[2].ToolResults)
		assert.Nil(t, r.Messages[2].Opaque)
	})
	t.Run("Tools carry Strict from StrictTools", func(t *testing.T) {
		assert.Equal(t, []llm.Tool{
			{Name: "add", Description: "Adds two numbers.", Schema: rawOf(`{"type":"object","properties":{"a":{"type":"number"}}}`), Strict: true},
			{Name: "lookup", Strict: true},
		}, r.Tools)
	})
	t.Run("Output is wrapped in a schema named answer", func(t *testing.T) {
		assert.Equal(t, &llm.Schema{Name: "answer", JSON: rawOf(`{"type": "object" , "properties": {"z": {}, "a": {}}}`)}, r.Output)
	})
	t.Run("Effort comes from the options", func(t *testing.T) {
		assert.Equal(t, llm.EffortHigh, r.Effort)
	})
	t.Run("nothing else is set", func(t *testing.T) {
		assert.Empty(t, r.ToolChoice)
		assert.Nil(t, r.Temperature)
		assert.Empty(t, r.Stop)
	})
	t.Run("the whole request is the one the table describes", func(t *testing.T) {
		assert.Equal(t, llm.Request{
			Model:  "model-a",
			System: "Be brief.",
			Messages: []llm.Message{
				{Role: llm.RoleUser, Text: "What is 2+2, and who is the mayor?"},
				{
					Role: llm.RoleAssistant,
					Text: "Let me look.",
					ToolCalls: []llm.ToolCall{
						{ID: "call-1", Name: "add", Input: rawOf(`{ "b": 2,"a":2 }`)},
						{ID: "call-2", Name: "lookup", Input: rawOf(`"not json {"`), Malformed: true},
					},
					Opaque: &llm.Opaque{Provider: "anthropic", Data: rawOf(oddJSON[1].data)},
				},
				{
					Role: llm.RoleTool,
					ToolResults: []llm.ToolResult{
						{CallID: "call-1", Content: "4"},
						{CallID: "call-2", Content: "no such tool", IsError: true},
					},
				},
			},
			Tools: []llm.Tool{
				{Name: "add", Description: "Adds two numbers.", Schema: rawOf(`{"type":"object","properties":{"a":{"type":"number"}}}`), Strict: true},
				{Name: "lookup", Strict: true},
			},
			Output:    &llm.Schema{Name: "answer", JSON: rawOf(`{"type": "object" , "properties": {"z": {}, "a": {}}}`)},
			MaxTokens: 777,
			Effort:    llm.EffortHigh,
		}, r)
	})
}

func TestAgentModel_RequestOptions(t *testing.T) {
	tests := []struct {
		name       string
		opts       app.AgentModelOptions
		wantEffort llm.Effort
		wantStrict bool
	}{
		{name: "the zero options leave both off", opts: app.AgentModelOptions{}},
		{name: "StrictTools alone", opts: app.AgentModelOptions{StrictTools: true}, wantStrict: true},
		{name: "Effort alone", opts: app.AgentModelOptions{Effort: llm.EffortXHigh}, wantEffort: llm.EffortXHigh},
		{name: "both", opts: app.AgentModelOptions{Effort: llm.EffortLow, StrictTools: true}, wantEffort: llm.EffortLow, wantStrict: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scripted := llm.NewScripted(echoScript, llm.ScriptedOptions{})
			req := asUser("model-a", "hi")
			req.Tools = []agent.ToolSpec{{Name: "one"}, {Name: "two"}}

			generate(t, app.AgentModel(scripted, tt.opts), req)

			got := scripted.Requests()[0]
			assert.Equal(t, tt.wantEffort, got.Effort)
			require.Len(t, got.Tools, 2)
			for _, tool := range got.Tools {
				assert.Equal(t, tt.wantStrict, tool.Strict, "tool %s", tool.Name)
			}
		})
	}
}

func TestAgentModel_OutputIsSentOnlyWhenSet(t *testing.T) {
	tests := []struct {
		name   string
		output json.RawMessage
		want   *llm.Schema
	}{
		{name: "nil", output: nil, want: nil},
		{name: "empty but not nil", output: json.RawMessage{}, want: nil},
		{name: "a schema", output: rawOf(`{"type":"string"}`), want: &llm.Schema{Name: "answer", JSON: rawOf(`{"type":"string"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scripted := llm.NewScripted(echoScript, llm.ScriptedOptions{})
			req := asUser("model-a", "hi")
			req.Output = tt.output

			generate(t, app.AgentModel(scripted, app.AgentModelOptions{}), req)

			assert.Equal(t, tt.want, scripted.Requests()[0].Output)
		})
	}
}

// A request always bounds the reply, so that a budget's hold is a real upper
// bound. The budget assumes 16000 for a request that sets none.
func TestAgentModel_MaxTokensIsAlwaysSet(t *testing.T) {
	tests := []struct {
		name string
		opts app.AgentModelOptions
		req  int
		want int
	}{
		{name: "the request's own wins", opts: app.AgentModelOptions{DefaultMaxTokens: 500}, req: 1234, want: 1234},
		{name: "the request's own wins, with no option", req: 1234, want: 1234},
		{name: "none set, so the option", opts: app.AgentModelOptions{DefaultMaxTokens: 500}, want: 500},
		{name: "none set and no option, so the budget's own default", want: 16000},
		{name: "a negative request is none set", opts: app.AgentModelOptions{DefaultMaxTokens: 500}, req: -1, want: 500},
		{name: "a negative option is none set", opts: app.AgentModelOptions{DefaultMaxTokens: -5}, want: 16000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scripted := llm.NewScripted(echoScript, llm.ScriptedOptions{})
			req := asUser("model-a", "hi")
			req.MaxTokens = tt.req

			generate(t, app.AgentModel(scripted, tt.opts), req)

			assert.Equal(t, tt.want, scripted.Requests()[0].MaxTokens)
		})
	}
}

func TestAgentModel_NilAndEmptyStayAsTheyAre(t *testing.T) {
	t.Run("nil lists stay nil", func(t *testing.T) {
		model := answering(plainReply())

		generate(t, app.AgentModel(model, app.AgentModelOptions{}), agent.Request{Model: "model-a"})

		got := model.requests()[0]
		assert.Nil(t, got.Messages)
		assert.Nil(t, got.Tools)
		assert.Nil(t, got.Output)
	})
	t.Run("empty lists stay empty, and a bare message has no lists", func(t *testing.T) {
		model := answering(plainReply())
		req := agent.Request{
			Model:    "model-a",
			Messages: []agent.Message{{Role: agent.RoleUser, Calls: []agent.Call{}, Results: []agent.Result{}}, {Role: agent.RoleUser}},
			Tools:    []agent.ToolSpec{},
		}

		generate(t, app.AgentModel(model, app.AgentModelOptions{}), req)

		got := model.requests()[0]
		require.NotNil(t, got.Tools)
		assert.Empty(t, got.Tools)
		require.Len(t, got.Messages, 2)
		assert.NotNil(t, got.Messages[0].ToolCalls)
		assert.NotNil(t, got.Messages[0].ToolResults)
		assert.Nil(t, got.Messages[1].ToolCalls)
		assert.Nil(t, got.Messages[1].ToolResults)
	})
	t.Run("a call's input and an opaque form's data keep nil and empty apart", func(t *testing.T) {
		model := answering(plainReply())
		req := agent.Request{Model: "model-a", Messages: []agent.Message{{
			Role: agent.RoleAssistant,
			Calls: []agent.Call{
				{ID: "a", Input: nil},
				{ID: "b", Input: json.RawMessage{}},
			},
			Opaque: &agent.Opaque{Provider: "p", Data: nil},
		}, {
			Role:   agent.RoleAssistant,
			Opaque: &agent.Opaque{Provider: "p", Data: json.RawMessage{}},
		}}}

		generate(t, app.AgentModel(model, app.AgentModelOptions{}), req)

		got := model.requests()[0].Messages
		assert.Nil(t, got[0].ToolCalls[0].Input)
		assert.NotNil(t, got[0].ToolCalls[1].Input)
		assert.Nil(t, got[0].Opaque.Data)
		assert.NotNil(t, got[1].Opaque.Data)
	})
}

// Each side's bytes are its own. A provider that edited a request in place
// must not change the engine's journal, nor an engine that edited a reply the
// model's own record.
func TestAgentModel_NothingIsSharedWithTheModel(t *testing.T) {
	t.Run("the request", func(t *testing.T) {
		model := answering(plainReply())
		req := agent.Request{
			Model: "model-a",
			Messages: []agent.Message{{
				Role:   agent.RoleAssistant,
				Calls:  []agent.Call{{ID: "c", Name: "n", Input: rawOf(`{"a":1}`)}},
				Opaque: &agent.Opaque{Provider: "p", Data: rawOf(`{"b":2}`)},
			}},
			Tools:  []agent.ToolSpec{{Name: "n", Schema: rawOf(`{"type":"object"}`)}},
			Output: rawOf(`{"type":"string"}`),
		}
		generate(t, app.AgentModel(model, app.AgentModelOptions{}), req)
		sent := model.requests()[0]

		// The caller reuses its buffers after the call.
		req.Messages[0].Calls[0].Input[2] = 'X'
		req.Messages[0].Opaque.Data[2] = 'X'
		req.Tools[0].Schema[2] = 'X'
		req.Output[2] = 'X'

		assert.Equal(t, `{"a":1}`, string(sent.Messages[0].ToolCalls[0].Input))
		assert.Equal(t, `{"b":2}`, string(sent.Messages[0].Opaque.Data))
		assert.Equal(t, `{"type":"object"}`, string(sent.Tools[0].Schema))
		assert.Equal(t, `{"type":"string"}`, string(sent.Output.JSON))

		// And the model edits what it was handed in place.
		sent.Messages[0].ToolCalls[0].Input[3] = 'Y'

		assert.Equal(t, `{"X":1}`, string(req.Messages[0].Calls[0].Input), "only the caller's own edit")
	})
	t.Run("the reply", func(t *testing.T) {
		reply := &llm.Response{
			Model: "model-a",
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "c", Name: "n", Input: rawOf(`{"a":1}`)}},
				Opaque:    &llm.Opaque{Provider: "p", Data: rawOf(`{"b":2}`)},
			},
			Stop: llm.StopToolUse,
		}
		resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

		resp.Message.Calls[0].Input[2] = 'X'
		resp.Message.Opaque.Data[2] = 'X'

		assert.Equal(t, `{"a":1}`, string(reply.Message.ToolCalls[0].Input))
		assert.Equal(t, `{"b":2}`, string(reply.Message.Opaque.Data))
	})
}

func TestAgentModel_OpaquePassesThroughByteForByte(t *testing.T) {
	for _, tt := range oddJSON {
		t.Run("to the model: "+tt.name, func(t *testing.T) {
			scripted := llm.NewScripted(echoScript, llm.ScriptedOptions{})
			req := agent.Request{Model: "model-a", Messages: []agent.Message{
				{Role: agent.RoleUser, Text: "hi"},
				{Role: agent.RoleAssistant, Opaque: &agent.Opaque{Provider: "anthropic", Data: rawOf(tt.data)}},
			}}

			generate(t, app.AgentModel(scripted, app.AgentModelOptions{}), req)

			got := scripted.Requests()[0].Messages[1].Opaque
			require.NotNil(t, got)
			assert.Equal(t, "anthropic", got.Provider)
			assert.Equal(t, []byte(tt.data), []byte(got.Data))
		})
		t.Run("from the model: "+tt.name, func(t *testing.T) {
			reply := plainReply()
			reply.Message.Opaque = &llm.Opaque{Provider: "openai", Data: rawOf(tt.data)}

			resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

			require.NotNil(t, resp.Message.Opaque)
			assert.Equal(t, "openai", resp.Message.Opaque.Provider)
			assert.Equal(t, []byte(tt.data), []byte(resp.Message.Opaque.Data))
		})
	}

	t.Run("a turn with no opaque form has none", func(t *testing.T) {
		resp := generate(t, app.AgentModel(answering(plainReply()), app.AgentModelOptions{}), asUser("model-a", "hi"))

		assert.Nil(t, resp.Message.Opaque)
	})
}

// An assistant turn that went out as a reply and comes back in a request must
// reach the model as the model wrote it.
func TestAgentModel_AnAssistantTurnRoundTrips(t *testing.T) {
	roundTrip := func(t *testing.T, produced llm.Message) llm.Message {
		t.Helper()
		call := 0
		model := &stubModel{reply: func(llm.Request) (*llm.Response, error) {
			call++
			if call == 1 {
				return &llm.Response{Model: "model-a", Message: produced, Stop: llm.StopToolUse}, nil
			}
			return plainReply(), nil
		}}
		m := app.AgentModel(model, app.AgentModelOptions{})

		first := generate(t, m, asUser("model-a", "go"))

		next := agent.Request{Model: "model-a", Messages: []agent.Message{
			{Role: agent.RoleUser, Text: "go"},
			first.Message,
		}}
		var results []agent.Result
		for _, c := range first.Message.Calls {
			results = append(results, agent.Result{CallID: c.ID, Content: "ok"})
		}
		if results != nil {
			next.Messages = append(next.Messages, agent.Message{Role: agent.RoleTool, Results: results})
		}
		generate(t, m, next)

		sent := model.requests()
		require.Len(t, sent, 2)
		require.GreaterOrEqual(t, len(sent[1].Messages), 2)
		return sent[1].Messages[1]
	}

	t.Run("text, two tool calls and an opaque form", func(t *testing.T) {
		produced := llm.Message{
			Role: llm.RoleAssistant,
			Text: "I will check both.",
			ToolCalls: []llm.ToolCall{
				{ID: "toolu_01", Name: "add", Input: rawOf(`{"b": 2,  "a":1}`)},
				{ID: "toolu_02", Name: "lookup", Input: rawOf("{\n\"z\":0,\"a\":[1,2]}\n")},
			},
			Opaque: &llm.Opaque{Provider: "anthropic", Data: rawOf(oddJSON[1].data)},
		}

		got := roundTrip(t, produced)

		assert.Equal(t, produced, got)
		assert.Equal(t, []byte(produced.Opaque.Data), []byte(got.Opaque.Data))
	})

	t.Run("whatever the model wrote", func(t *testing.T) {
		for seed := range uint64(400) {
			r := rand.New(rand.NewPCG(seed, 7))
			produced := genAssistantTurn(r)

			got := roundTrip(t, produced)

			require.Equal(t, produced, got, "seed %d", seed)
		}
	})
}

func genAssistantTurn(r *rand.Rand) llm.Message {
	m := llm.Message{Role: llm.RoleAssistant}
	if r.IntN(4) > 0 {
		m.Text = genText(r)
	}
	switch r.IntN(3) {
	case 0:
		m.ToolCalls = nil
	case 1:
		m.ToolCalls = []llm.ToolCall{}
	default:
		for i := range 1 + r.IntN(4) {
			c := llm.ToolCall{ID: fmt.Sprintf("call_%d_%d", r.IntN(1000), i), Name: genName(r)}
			switch r.IntN(6) {
			case 0:
				c.Input = nil
			case 1:
				c.Input = json.RawMessage{}
			case 2:
				c.Malformed = true
				c.Input = rawOf(fmt.Sprintf("%q", genText(r)))
			default:
				c.Input = rawOf(genJSON(r, 0))
			}
			m.ToolCalls = append(m.ToolCalls, c)
		}
	}
	if r.IntN(3) > 0 {
		o := &llm.Opaque{Provider: []string{"anthropic", "openai", "other"}[r.IntN(3)]}
		switch r.IntN(5) {
		case 0:
			o.Data = nil
		case 1:
			o.Data = json.RawMessage{}
		default:
			o.Data = rawOf(genJSON(r, 0))
		}
		m.Opaque = o
	}
	return m
}

func genName(r *rand.Rand) string {
	return []string{"add", "lookup", "send_email", "get-weather", "x"}[r.IntN(5)]
}

func genText(r *rand.Rand) string {
	parts := []string{"hello", " ", "café", "\n", "\t", "☃", "a\"b", "{", "\\", "", "\U0001F600", "  "}
	var b strings.Builder
	for range r.IntN(8) {
		b.WriteString(parts[r.IntN(len(parts))])
	}
	return b.String()
}

// genJSON writes a JSON value as text, with keys in no order, a key sometimes
// repeated, and white space wherever JSON allows it.
func genJSON(r *rand.Rand, depth int) string {
	ws := func() string { return []string{"", "", " ", "  ", "\n", "\t", " \n "}[r.IntN(7)] }
	scalars := []string{`"x"`, `"caf\u00e9"`, `"a\"b"`, `""`, `1.0`, `1E2`, `-0`, `12345678901234567890123`, `0.10`, `true`, `false`, `null`}
	if depth >= 3 || r.IntN(3) == 0 {
		return scalars[r.IntN(len(scalars))]
	}
	if r.IntN(2) == 0 {
		var items []string
		for range r.IntN(4) {
			items = append(items, ws()+genJSON(r, depth+1)+ws())
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	keys := []string{"zeta", "alpha", "mid", "b", "a", "café", "caf\\u00e9", `q\"q`}
	r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	var members []string
	for _, k := range keys[:r.IntN(len(keys)+1)] {
		members = append(members, ws()+`"`+k+`"`+ws()+":"+ws()+genJSON(r, depth+1)+ws())
	}
	if len(members) > 0 && r.IntN(5) == 0 {
		members = append(members, members[0])
	}
	return "{" + strings.Join(members, ",") + "}"
}

func TestAgentModel_Response(t *testing.T) {
	t.Run("Message comes back field for field, and Model is the model that answered", func(t *testing.T) {
		reply := &llm.Response{
			ID:    "msg_01",
			Model: "model-a-20251001",
			Message: llm.Message{
				Role: llm.RoleAssistant,
				Text: "Checking.",
				ToolCalls: []llm.ToolCall{
					{ID: "call-1", Name: "add", Input: rawOf(`{"b":2,"a":1}`)},
					{ID: "call-2", Name: "lookup", Input: rawOf(`"unterminated {"`), Malformed: true},
				},
				Opaque: &llm.Opaque{Provider: "anthropic", Data: rawOf(oddJSON[0].data)},
			},
			Stop: llm.StopToolUse,
		}

		resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

		assert.Equal(t, agent.Message{
			Role: agent.RoleAssistant,
			Text: "Checking.",
			Calls: []agent.Call{
				{ID: "call-1", Name: "add", Input: rawOf(`{"b":2,"a":1}`)},
				{ID: "call-2", Name: "lookup", Input: rawOf(`"unterminated {"`), Malformed: true},
			},
			Opaque: &agent.Opaque{Provider: "anthropic", Data: rawOf(oddJSON[0].data)},
		}, resp.Message)
		assert.Equal(t, "model-a-20251001", resp.Model)
		assert.Equal(t, agent.StopToolUse, resp.Stop)
	})

	// A model is trusted to return an assistant turn, but the adapter passes
	// what it is given: it does not rewrite a role or drop a result.
	for _, role := range []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, ""} {
		t.Run(fmt.Sprintf("a message with role %q and results is passed as it is", role), func(t *testing.T) {
			reply := plainReply()
			reply.Message = llm.Message{
				Role: role,
				Text: "text",
				ToolResults: []llm.ToolResult{
					{CallID: "call-1", Content: "4"},
					{CallID: "call-2", Content: "no such tool", IsError: true},
				},
			}

			resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

			assert.Equal(t, agent.Message{
				Role: agent.Role(role),
				Text: "text",
				Results: []agent.Result{
					{CallID: "call-1", Content: "4"},
					{CallID: "call-2", Content: "no such tool", IsError: true},
				},
			}, resp.Message)
		})
	}
}

func TestAgentModel_Stop(t *testing.T) {
	tests := []struct {
		stop llm.StopReason
		want agent.Stop
	}{
		{llm.StopEnd, agent.StopEnd},
		{llm.StopToolUse, agent.StopToolUse},
		{llm.StopMaxTokens, agent.StopMaxTokens},
		{llm.StopSequence, agent.StopEnd},
		{llm.StopRefusal, agent.StopRefusal},
		{llm.StopPause, agent.StopPause},
		{llm.StopContextWindow, agent.StopContextWindow},
	}
	for _, tt := range tests {
		t.Run(string(tt.stop), func(t *testing.T) {
			reply := plainReply()
			reply.Stop = tt.stop

			resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

			assert.Equal(t, tt.want, resp.Stop)
		})
	}

	// agent has no name for these, and the planner reads a reply by its stop: a
	// guess would end a run as complete or send it round again.
	for _, stop := range []llm.StopReason{"", "something_new"} {
		t.Run(fmt.Sprintf("a stop agent has no name for: %q", stop), func(t *testing.T) {
			reply := plainReply()
			reply.Stop = stop

			resp, err := app.AgentModel(answering(reply), app.AgentModelOptions{}).Generate(context.Background(), asUser("model-a", "hi"))

			require.ErrorIs(t, err, agent.ErrPermanent)
			assert.Equal(t, agent.Response{}, resp)
		})
	}
}

func TestAgentModel_Usage(t *testing.T) {
	t.Run("InputTokens sums input, cache reads and cache writes; reasoning is already in the output", func(t *testing.T) {
		reply := plainReply()
		reply.Usage = usageA

		resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

		assert.Equal(t, int64(1000+200+30), resp.Usage.InputTokens)
		assert.Equal(t, int64(4), resp.Usage.OutputTokens)
	})

	t.Run("each count has its own place", func(t *testing.T) {
		reply := plainReply()
		reply.Usage = llm.Usage{InputTokens: 1, OutputTokens: 20, CacheReadTokens: 300, CacheWriteTokens: 4000}

		resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

		assert.Equal(t, int64(4301), resp.Usage.InputTokens)
		assert.Equal(t, int64(20), resp.Usage.OutputTokens)
	})

	// The run's token limit has to see what the cost sees: a reply that two
	// models worked on was billed for both.
	t.Run("a reply with attempts counts every attempt, as the cost does", func(t *testing.T) {
		reply := plainReply()
		reply.Usage = usageA
		reply.Attempts = []llm.Attempt{{Model: "model-b", Usage: usageB}, {Model: "model-a", Usage: usageA}}

		resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

		assert.Equal(t, int64(4000+1230), resp.Usage.InputTokens)
		assert.Equal(t, int64(1000+4), resp.Usage.OutputTokens)
	})
}

func TestAgentModel_Cost(t *testing.T) {
	tests := []struct {
		name   string
		prices llm.Prices
		asked  string
		reply  *llm.Response
		want   int64
	}{
		{
			name:  "with a nil table nothing is priced, whatever model answers",
			asked: "model-a",
			reply: &llm.Response{Model: "model-zzz", Usage: usageA, Stop: llm.StopEnd},
			want:  0,
		},
		{
			name:   "from the table, at each kind of token's own price",
			prices: twoPrices(),
			asked:  "model-a",
			reply:  &llm.Response{Model: "model-a", Usage: usageA, Stop: llm.StopEnd},
			want:   costA,
		},
		{
			name:   "the model the reply names, not the one asked for",
			prices: twoPrices(),
			asked:  "model-a",
			reply:  &llm.Response{Model: "model-b", Usage: usageB, Stop: llm.StopEnd},
			want:   costB,
		},
		{
			name:   "a dated id is priced as the alias the request asked for",
			prices: twoPrices(),
			asked:  "model-a",
			reply:  &llm.Response{Model: "model-a-20251001", Usage: usageA, Stop: llm.StopEnd},
			want:   costA,
		},
		{
			name:   "two attempts on different models are summed",
			prices: twoPrices(),
			asked:  "model-b",
			reply: &llm.Response{
				Model: "model-a", Usage: usageA, Stop: llm.StopEnd,
				Attempts: []llm.Attempt{{Model: "model-b", Usage: usageB}, {Model: "model-a", Usage: usageA}},
			},
			want: costA + costB,
		},
		{
			name:   "Model and Usage are not added again to the attempts",
			prices: twoPrices(),
			asked:  "model-a",
			reply: &llm.Response{
				Model: "model-a", Usage: usageA, Stop: llm.StopEnd,
				Attempts: []llm.Attempt{{Model: "model-b", Usage: usageB}},
			},
			want: costB,
		},
		{
			name:   "an attempt on a dated id is priced as the model asked for",
			prices: twoPrices(),
			asked:  "model-a",
			reply: &llm.Response{
				Model: "model-b", Usage: usageB, Stop: llm.StopEnd,
				Attempts: []llm.Attempt{{Model: "model-a-20251001", Usage: usageA}, {Model: "model-b", Usage: usageB}},
			},
			want: costA + costB,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := app.AgentModel(answering(tt.reply), app.AgentModelOptions{Prices: tt.prices})

			resp := generate(t, m, asUser(tt.asked, "hi"))

			assert.Equal(t, tt.want, resp.Usage.CostMicros)
		})
	}
}

// The case that must fail: a run with a cost budget must not count a call it
// cannot price as free.
func TestAgentModel_AReplyNoPriceCanBeFoundForFails(t *testing.T) {
	tests := []struct {
		name  string
		asked string
		reply *llm.Response
	}{
		{
			name:  "neither the reply's model nor the request's is in the table",
			asked: "model-y",
			reply: &llm.Response{Model: "model-z", Usage: usageA, Stop: llm.StopEnd},
		},
		{
			name:  "the request names no model and the reply's is unlisted",
			asked: "",
			reply: &llm.Response{Model: "model-z", Usage: usageA, Stop: llm.StopEnd},
		},
		{
			name:  "the reply names none and the request's is unlisted",
			asked: "model-y",
			reply: &llm.Response{Usage: usageA, Stop: llm.StopEnd},
		},
		{
			name:  "one attempt of two that neither name prices",
			asked: "model-y",
			reply: &llm.Response{
				Model: "model-a", Usage: usageA, Stop: llm.StopEnd,
				Attempts: []llm.Attempt{{Model: "model-a", Usage: usageA}, {Model: "model-z", Usage: usageB}},
			},
		},
		{
			name:  "a refusal is billed too",
			asked: "model-y",
			reply: &llm.Response{Model: "model-z", Usage: usageA, Stop: llm.StopRefusal},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := app.AgentModel(answering(tt.reply), app.AgentModelOptions{Prices: twoPrices()})

			resp, err := m.Generate(context.Background(), asUser(tt.asked, "hi"))

			require.Error(t, err)
			assert.ErrorIs(t, err, agent.ErrPermanent)
			assert.ErrorIs(t, err, llm.ErrNoPrice)
			assert.Equal(t, agent.Response{}, resp, "an unpriced reply is not a reply that cost nothing")
		})
	}
}

func TestAgentModel_PricesAreCopiedWhenTheModelIsBuilt(t *testing.T) {
	prices := twoPrices()
	m := app.AgentModel(answering(&llm.Response{Model: "model-a", Usage: usageA, Stop: llm.StopEnd}), app.AgentModelOptions{Prices: prices})

	prices["model-a"] = llm.Price{Input: 1, Output: 1}
	delete(prices, "model-b")

	assert.Equal(t, int64(costA), generate(t, m, asUser("model-a", "hi")).Usage.CostMicros)
}

// nil prices nothing. A table that is there and empty, such as one that failed
// to load, is not "no prices": it lists no model, so no reply can be priced and a
// run's cost limit is not silently off.
func TestAgentModel_NilAndEmptyPrices(t *testing.T) {
	t.Run("nil prices nothing, and every reply costs zero", func(t *testing.T) {
		m := app.AgentModel(answering(&llm.Response{Model: "model-zzz", Usage: usageA, Stop: llm.StopEnd}), app.AgentModelOptions{Prices: nil})

		resp, err := m.Generate(context.Background(), asUser("model-zzz", "hi"))

		require.NoError(t, err)
		assert.Zero(t, resp.Usage.CostMicros)
		assert.Equal(t, int64(1230), resp.Usage.InputTokens)
	})

	for name, prices := range map[string]llm.Prices{"an empty table": {}, "a table made empty": make(llm.Prices, 4)} {
		t.Run(name+" lists no model, so every reply is unpriceable and permanent", func(t *testing.T) {
			m := app.AgentModel(answering(&llm.Response{Model: "model-a", Usage: usageA, Stop: llm.StopEnd}), app.AgentModelOptions{Prices: prices})

			resp, err := m.Generate(context.Background(), asUser("model-a", "hi"))

			require.ErrorIs(t, err, agent.ErrPermanent)
			require.ErrorIs(t, err, llm.ErrNoPrice)
			assert.Equal(t, agent.Response{}, resp, "not a reply that cost nothing")
		})
	}
}

// The budget prices a request that names no model at its own Options.Model. The
// adapter has the same option, sends it when the request names none, and prices
// at it, so the two settle the same price.
func TestAgentModel_ARequestThatNamesNoModel(t *testing.T) {
	t.Run("the option is sent when the request names none, and the request's own wins", func(t *testing.T) {
		tests := []struct {
			name    string
			option  string
			request string
			want    string
		}{
			{name: "none named anywhere", want: ""},
			{name: "the option", option: "model-a", want: "model-a"},
			{name: "the request's own over the option", option: "model-a", request: "model-b", want: "model-b"},
			{name: "the request's own, no option", request: "model-b", want: "model-b"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				model := answering(plainReply())

				generate(t, app.AgentModel(model, app.AgentModelOptions{Model: tt.option}), asUser(tt.request, "hi"))

				assert.Equal(t, tt.want, model.requests()[0].Model)
			})
		}
	})

	t.Run("a dated id is priced as the option's model", func(t *testing.T) {
		m := app.AgentModel(answering(&llm.Response{Model: "model-a-20251001", Usage: usageA, Stop: llm.StopEnd}),
			app.AgentModelOptions{Model: "model-a", Prices: twoPrices()})

		resp := generate(t, m, asUser("", "hi"))

		assert.Equal(t, int64(costA), resp.Usage.CostMicros)
	})

	t.Run("with no option, a request that names no model cannot price a dated id", func(t *testing.T) {
		m := app.AgentModel(answering(&llm.Response{Model: "model-a-20251001", Usage: usageA, Stop: llm.StopEnd}),
			app.AgentModelOptions{Prices: twoPrices()})

		_, err := m.Generate(context.Background(), asUser("", "hi"))

		require.ErrorIs(t, err, agent.ErrPermanent)
		require.ErrorIs(t, err, llm.ErrNoPrice)
	})

	// Parity: for each reply, a Budgeted given the model as its Options.Model
	// settles the cost the adapter reports.
	replies := []struct {
		name  string
		reply *llm.Response
	}{
		{"a listed model", &llm.Response{Model: "model-b", Usage: usageB, Stop: llm.StopEnd}},
		{"the alias itself", &llm.Response{Model: "model-a", Usage: usageA, Stop: llm.StopEnd}},
		{"a dated id", &llm.Response{Model: "model-a-20251001", Usage: usageA, Stop: llm.StopEnd}},
		{"a reply that names no model", &llm.Response{Usage: usageA, Stop: llm.StopEnd}},
		{"two attempts, one on a dated id", &llm.Response{
			Model: "model-b", Usage: usageB, Stop: llm.StopEnd,
			Attempts: []llm.Attempt{{Model: "model-a-20251001", Usage: usageA}, {Model: "model-b", Usage: usageB}},
		}},
		{"a refusal on a dated id", &llm.Response{Model: "model-a-20251001", Usage: usageA, Stop: llm.StopRefusal}},
	}
	for _, tt := range replies {
		t.Run("parity with Budgeted: "+tt.name, func(t *testing.T) {
			budget, err := llm.NewBudgeted(answering(tt.reply), llm.BudgetOptions{
				MaxCostMicros: 1 << 40, Prices: twoPrices(), Model: "model-a",
			})
			require.NoError(t, err)
			m := app.AgentModel(budget, app.AgentModelOptions{Model: "model-a", Prices: twoPrices()})

			resp := generate(t, m, asUser("", "hi"))

			assert.Equal(t, int64(1), budget.Spent().Calls)
			assert.Equal(t, budget.Spent().CostMicros, resp.Usage.CostMicros, "the budget and the adapter charge the same")
			assert.NotZero(t, resp.Usage.CostMicros)
		})
	}
}

// logSink is a slog.Handler that keeps what it is given.
type logSink struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (*logSink) Enabled(context.Context, slog.Level) bool { return true }
func (s *logSink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, r.Clone())
	return nil
}
func (s *logSink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *logSink) WithGroup(string) slog.Handler      { return s }

func (s *logSink) records() []slog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slog.Record(nil), s.recs...)
}

// logText is everything a record says: its message and every attribute.
func logText(r slog.Record) string {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	return b.String()
}

func logAttrs(r slog.Record) map[string]string {
	out := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value.String()
		return true
	})
	return out
}

// A refusal has nowhere to go in agent.Response, so its reason is logged. The
// prompt and the reply's text are never logged.
func TestAgentModel_ALongRefusalExplanationIsCutInTheLog(t *testing.T) {
	// A provider's explanation is its own text, and could quote the prompt back.
	long := strings.Repeat("é", 400) // 800 bytes
	var buf bytes.Buffer
	resp := &llm.Response{
		ID:      "msg_01XYZ",
		Model:   "model-a-20251001",
		Message: llm.Message{Role: llm.RoleAssistant},
		Stop:    llm.StopRefusal,
		Refusal: &llm.Refusal{Category: "policy", Explanation: long},
	}
	m := app.AgentModel(answering(resp), app.AgentModelOptions{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})

	generate(t, m, asUser("model-a", "prompt"))

	var line map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line))
	got, _ := line["explanation"].(string)
	assert.LessOrEqual(t, len(got), 512+len("…"), "the log keeps the start of it")
	assert.True(t, utf8.ValidString(got), "cut between characters, not inside one")
	assert.True(t, strings.HasSuffix(got, "…"), "and says it was cut")
	assert.True(t, strings.HasPrefix(long, strings.TrimSuffix(got, "…")))

	t.Run("one that fits is logged whole", func(t *testing.T) {
		buf.Reset()
		short := *resp
		short.Refusal = &llm.Refusal{Explanation: strings.Repeat("a", 512)}
		m := app.AgentModel(answering(&short), app.AgentModelOptions{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})

		generate(t, m, asUser("model-a", "prompt"))

		var line map[string]any
		require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line))
		assert.Equal(t, strings.Repeat("a", 512), line["explanation"])
	})
}

func TestAgentModel_ARefusalIsLogged(t *testing.T) {
	refusal := func() *llm.Response {
		return &llm.Response{
			ID:    "msg_01XYZ",
			Model: "model-a-20251001",
			Message: llm.Message{
				Role:      llm.RoleAssistant,
				Text:      "reply text that must not be logged",
				ToolCalls: []llm.ToolCall{{ID: "c", Name: "n", Input: rawOf(`{"secret":"call input that must not be logged"}`)}},
			},
			Stop:    llm.StopRefusal,
			Refusal: &llm.Refusal{Category: "cyber", Explanation: "the request asked for something the model will not do"},
		}
	}
	request := asUser("model-a", "prompt text that must not be logged")
	request.System = "system text that must not be logged"

	t.Run("one line at warning level with the id, the model, the category and the explanation", func(t *testing.T) {
		sink := &logSink{}
		m := app.AgentModel(answering(refusal()), app.AgentModelOptions{Logger: slog.New(sink)})

		generate(t, m, request)

		recs := sink.records()
		require.Len(t, recs, 1)
		assert.Equal(t, slog.LevelWarn, recs[0].Level)
		assert.Equal(t, map[string]string{
			"id":          "msg_01XYZ",
			"model":       "model-a-20251001",
			"category":    "cyber",
			"explanation": "the request asked for something the model will not do",
		}, logAttrs(recs[0]))
		assert.Contains(t, recs[0].Message, "refus")
	})

	t.Run("never the prompt, the system text, or what the reply said", func(t *testing.T) {
		sink := &logSink{}
		m := app.AgentModel(answering(refusal()), app.AgentModelOptions{Logger: slog.New(sink)})

		generate(t, m, request)

		require.Len(t, sink.records(), 1)
		all := logText(sink.records()[0])
		for _, secret := range []string{"prompt text", "system text", "reply text", "call input", "secret"} {
			assert.NotContains(t, all, secret)
		}
	})

	t.Run("a refusal that gives no reason is logged with what there is", func(t *testing.T) {
		sink := &logSink{}
		reply := refusal()
		reply.Refusal = nil
		m := app.AgentModel(answering(reply), app.AgentModelOptions{Logger: slog.New(sink)})

		generate(t, m, request)

		require.Len(t, sink.records(), 1)
		assert.Equal(t, map[string]string{"id": "msg_01XYZ", "model": "model-a-20251001"}, logAttrs(sink.records()[0]))
	})

	t.Run("an empty part of the reason is not logged as an empty field", func(t *testing.T) {
		tests := []struct {
			name   string
			reason *llm.Refusal
			want   map[string]string
		}{
			{"a category alone", &llm.Refusal{Category: "cyber"}, map[string]string{"id": "msg_01XYZ", "model": "model-a-20251001", "category": "cyber"}},
			{"an explanation alone", &llm.Refusal{Explanation: "not something it does"}, map[string]string{"id": "msg_01XYZ", "model": "model-a-20251001", "explanation": "not something it does"}},
			{"neither", &llm.Refusal{}, map[string]string{"id": "msg_01XYZ", "model": "model-a-20251001"}},
		}
		for _, tt := range tests {
			sink := &logSink{}
			reply := refusal()
			reply.Refusal = tt.reason
			m := app.AgentModel(answering(reply), app.AgentModelOptions{Logger: slog.New(sink)})

			generate(t, m, request)

			require.Len(t, sink.records(), 1, tt.name)
			assert.Equal(t, tt.want, logAttrs(sink.records()[0]), tt.name)
		}
	})

	t.Run("a reply that is not a refusal logs nothing", func(t *testing.T) {
		sink := &logSink{}
		reply := refusal()
		reply.Stop = llm.StopEnd
		m := app.AgentModel(answering(reply), app.AgentModelOptions{Logger: slog.New(sink)})

		generate(t, m, request)

		assert.Empty(t, sink.records())
	})

	t.Run("a failed call logs nothing", func(t *testing.T) {
		sink := &logSink{}
		m := app.AgentModel(failing(errors.New("down")), app.AgentModelOptions{Logger: slog.New(sink)})

		_, _ = m.Generate(context.Background(), request)

		assert.Empty(t, sink.records())
	})

	t.Run("no Logger is slog.Default", func(t *testing.T) {
		sink := &logSink{}
		was := slog.Default()
		slog.SetDefault(slog.New(sink))
		t.Cleanup(func() { slog.SetDefault(was) })
		m := app.AgentModel(answering(refusal()), app.AgentModelOptions{})

		generate(t, m, request)

		require.Len(t, sink.records(), 1)
		assert.Equal(t, "msg_01XYZ", logAttrs(sink.records()[0])["id"])
	})

	t.Run("a refusal an unpriceable table turns into an error is still logged", func(t *testing.T) {
		sink := &logSink{}
		m := app.AgentModel(answering(refusal()), app.AgentModelOptions{Logger: slog.New(sink), Prices: llm.Prices{"other": {}}})

		_, err := m.Generate(context.Background(), request)

		require.ErrorIs(t, err, llm.ErrNoPrice)
		assert.Len(t, sink.records(), 1)
	})
}

// A refused reply carries no text and no tool calls. The adapter passes what
// it is given, and a refusal is a reply, not an error.
func TestAgentModel_ARefusalIsAReply(t *testing.T) {
	t.Run("one that carries nothing", func(t *testing.T) {
		scripted := llm.NewScripted(func(llm.Request, int) (llm.Reply, error) {
			return llm.Reply{Stop: llm.StopRefusal, Usage: usageA}, nil
		}, llm.ScriptedOptions{})
		m := app.AgentModel(scripted, app.AgentModelOptions{Prices: twoPrices()})

		resp, err := m.Generate(context.Background(), asUser("model-a", "hi"))

		require.NoError(t, err)
		assert.Equal(t, agent.StopRefusal, resp.Stop)
		assert.Equal(t, agent.Message{Role: agent.RoleAssistant}, resp.Message)
		assert.Equal(t, "model-a", resp.Model)
		assert.Equal(t, int64(costA), resp.Usage.CostMicros, "what was refused was still billed")
	})

	// A refusal reaches the engine with nothing to act on, whatever the
	// provider put in it: an engine that journals calls before it reads the stop
	// would run them, and the opaque form would replay them.
	reasons := map[string]*llm.Refusal{
		"with a reason":       {Category: "cyber", Explanation: "no"},
		"with an empty field": {},
		"with no reason":      nil,
	}
	for name, reason := range reasons {
		t.Run("one that carries text, two calls and an opaque form reaches the engine with none of them, "+name, func(t *testing.T) {
			reply := &llm.Response{
				ID:    "msg_01",
				Model: "model-a",
				Message: llm.Message{
					Role: llm.RoleAssistant,
					Text: "I can't help with that.",
					ToolCalls: []llm.ToolCall{
						{ID: "call-1", Name: "delete_all", Input: rawOf(`{"everything":true}`)},
						{ID: "call-2", Name: "send", Input: rawOf(`{"to":"x"}`)},
					},
					ToolResults: []llm.ToolResult{{CallID: "call-0", Content: "stale"}},
					Opaque:      &llm.Opaque{Provider: "anthropic", Data: rawOf(oddJSON[1].data)},
				},
				Stop:    llm.StopRefusal,
				Usage:   usageA,
				Refusal: reason,
			}

			resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{Prices: twoPrices()}), asUser("model-a", "hi"))

			assert.Equal(t, agent.StopRefusal, resp.Stop)
			assert.Empty(t, resp.Message.Text)
			assert.Empty(t, resp.Message.Calls)
			assert.Empty(t, resp.Message.Results)
			assert.Nil(t, resp.Message.Opaque)
			assert.Equal(t, agent.Message{Role: agent.RoleAssistant}, resp.Message)
			assert.Equal(t, "model-a", resp.Model)
			assert.Equal(t, int64(costA), resp.Usage.CostMicros, "what was refused was still billed")
			assert.Equal(t, int64(1230), resp.Usage.InputTokens)
		})
	}

	t.Run("only a refusal loses its content: every other stop keeps it", func(t *testing.T) {
		for _, stop := range []llm.StopReason{llm.StopEnd, llm.StopToolUse, llm.StopMaxTokens, llm.StopSequence, llm.StopPause, llm.StopContextWindow} {
			reply := plainReply()
			reply.Stop = stop
			reply.Message = llm.Message{
				Role:      llm.RoleAssistant,
				Text:      "some text",
				ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "n", Input: rawOf(`{}`)}},
				Opaque:    &llm.Opaque{Provider: "p", Data: rawOf(`{"a":1}`)},
			}

			resp := generate(t, app.AgentModel(answering(reply), app.AgentModelOptions{}), asUser("model-a", "hi"))

			assert.Equal(t, "some text", resp.Message.Text, "stop %s", stop)
			assert.Len(t, resp.Message.Calls, 1, "stop %s", stop)
			assert.NotNil(t, resp.Message.Opaque, "stop %s", stop)
		}
	})

	// Fallback moves past a refusal and returns the last one as the reply when
	// the models after it fail: a normal reply, billed for every attempt.
	t.Run("one a fallback chain returns after a later model failed", func(t *testing.T) {
		refusing := func(name string, usage llm.Usage) llm.Model {
			return llm.NewScripted(func(llm.Request, int) (llm.Reply, error) {
				return llm.Reply{Stop: llm.StopRefusal, Usage: usage}, nil
			}, llm.ScriptedOptions{Name: name})
		}
		chain, err := llm.NewFallback(llm.FallbackOptions{OnRefusal: true},
			refusing("model-a", usageA),
			refusing("model-b", usageB),
			failing(errors.New("provider down")),
		)
		require.NoError(t, err)
		m := app.AgentModel(chain, app.AgentModelOptions{Prices: twoPrices()})

		resp, err := m.Generate(context.Background(), asUser("model-a", "hi"))

		require.NoError(t, err)
		assert.Equal(t, agent.StopRefusal, resp.Stop)
		assert.Equal(t, "model-b", resp.Model)
		assert.Equal(t, int64(costA+costB), resp.Usage.CostMicros)
		assert.Equal(t, int64(1230+4000), resp.Usage.InputTokens)
	})
}

func TestAgentModel_Errors(t *testing.T) {
	notRetryable := &llm.Error{Provider: "anthropic", Status: 401, Type: "authentication_error", Message: "invalid key"}
	retryable := &llm.Error{Provider: "anthropic", Status: 429, Type: "rate_limit_error", Message: "slow down", Retryable: true}
	plain := errors.New("connection reset by peer")

	tests := []struct {
		name      string
		err       error
		permanent bool
		// is, when set, must still be found in the chain.
		is []error
	}{
		{name: "ErrBudgetExceeded", err: llm.ErrBudgetExceeded, permanent: true, is: []error{llm.ErrBudgetExceeded}},
		{
			name:      "ErrBudgetExceeded with context",
			err:       fmt.Errorf("llm: budget: a call of up to 9 tokens: %w", llm.ErrBudgetExceeded),
			permanent: true,
			is:        []error{llm.ErrBudgetExceeded},
		},
		{name: "ErrNoPrice", err: fmt.Errorf("pricing the worst case: %w: %q", llm.ErrNoPrice, "m"), permanent: true, is: []error{llm.ErrNoPrice}},
		{name: "an *llm.Error that is not retryable", err: notRetryable, permanent: true, is: []error{notRetryable}},
		{
			name:      "a wrapped *llm.Error that is not retryable",
			err:       fmt.Errorf("llm: fallback: %w", errors.Join(fmt.Errorf("model 1 of 1: %w", notRetryable))),
			permanent: true,
			is:        []error{notRetryable},
		},
		{
			name:      "a non-retryable *llm.Error that wraps a deadline: the mark is trusted before the context error",
			err:       &llm.Error{Provider: "openai", Err: context.DeadlineExceeded},
			permanent: true,
			is:        []error{context.DeadlineExceeded},
		},
		{name: "an *llm.Error that is retryable", err: retryable},
		{
			name: "a retryable *llm.Error that wraps a deadline, a client's own timeout",
			err:  &llm.Error{Provider: "openai", Retryable: true, Err: context.DeadlineExceeded},
		},
		{name: "a wrapped retryable *llm.Error", err: fmt.Errorf("model 1 of 2: %w", retryable)},
		{name: "a plain error", err: plain},
		{name: "a script that ran out", err: llm.ErrScriptExhausted},
		{name: "a bare deadline error while the caller's context is live", err: context.DeadlineExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := app.AgentModel(failing(tt.err), app.AgentModelOptions{})

			resp, err := m.Generate(context.Background(), asUser("model-a", "hi"))

			require.Error(t, err)
			assert.Equal(t, agent.Response{}, resp)
			assert.Equal(t, tt.permanent, errors.Is(err, agent.ErrPermanent), "permanent: %v", err)
			for _, want := range tt.is {
				assert.ErrorIs(t, err, want, "still unwraps to what it was")
			}
			var le *llm.Error
			if errors.As(tt.err, &le) {
				var got *llm.Error
				require.ErrorAs(t, err, &got)
				assert.Same(t, le, got, "the same *llm.Error")
				assert.Equal(t, le.Retryable, llm.Retryable(err), "its own mark stands")
			}
			if !tt.permanent {
				assert.True(t, isTheError(err, tt.err), "returned as it is, not wrapped")
			}
		})
	}

	t.Run("the permanent errors say what they were", func(t *testing.T) {
		_, err := app.AgentModel(failing(fmt.Errorf("%w: a call of up to 9 tokens", llm.ErrBudgetExceeded)), app.AgentModelOptions{}).
			Generate(context.Background(), asUser("model-a", "hi"))

		assert.Contains(t, err.Error(), "budget exceeded")
		assert.Contains(t, err.Error(), "a call of up to 9 tokens")
	})
}

// The caller's cancellation or deadline is neither retryable nor permanent. It
// is read from the context, as the llm wrappers read it.
func TestAgentModel_TheCallersContext(t *testing.T) {
	assertTheCallers := func(t *testing.T, err error, want error) {
		t.Helper()
		require.Error(t, err)
		assert.ErrorIs(t, err, want)
		assert.NotErrorIs(t, err, agent.ErrPermanent, "a shutdown must not fail the run for good")
		assert.False(t, llm.Retryable(err), "and it is not a provider failure to try again")
	}

	t.Run("cancelled before the call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		m := app.AgentModel(llm.NewScripted(echoScript, llm.ScriptedOptions{}), app.AgentModelOptions{})

		_, err := m.Generate(ctx, asUser("model-a", "hi"))

		assertTheCallers(t, err, context.Canceled)
	})
	t.Run("past its deadline before the call", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		m := app.AgentModel(llm.NewScripted(echoScript, llm.ScriptedOptions{}), app.AgentModelOptions{})

		_, err := m.Generate(ctx, asUser("model-a", "hi"))

		assertTheCallers(t, err, context.DeadlineExceeded)
	})
	t.Run("cancelled during the call", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		blocked := &stubModel{}
		blocked.reply = func(llm.Request) (*llm.Response, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		m := app.AgentModel(blocked, app.AgentModelOptions{})
		errc := make(chan error, 1)
		go func() {
			_, err := m.Generate(ctx, asUser("model-a", "hi"))
			errc <- err
		}()

		select {
		case <-started:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("the model was never called")
		}
		cancel()

		select {
		case err := <-errc:
			assertTheCallers(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("the call did not end when its context was cancelled")
		}
	})
	t.Run("a deadline that passes during the call", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		slow := &stubModel{reply: func(llm.Request) (*llm.Response, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}

		_, err := app.AgentModel(slow, app.AgentModelOptions{}).Generate(ctx, asUser("model-a", "hi"))

		assertTheCallers(t, err, context.DeadlineExceeded)
	})
	// A client that wraps what the transport said, once its caller's context
	// is done, is the caller's cancellation and not a failure for good.
	t.Run("an error that only reports the caller's cancellation, however it is marked", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		wrapped := &llm.Error{Provider: "openai", Err: context.Canceled}

		_, err := app.AgentModel(failing(wrapped), app.AgentModelOptions{}).Generate(ctx, asUser("model-a", "hi"))

		assertTheCallers(t, err, context.Canceled)
		assert.True(t, isTheError(err, wrapped), "returned as it is")
	})
}

// The context decides before anything the error says: a budget refusal, an
// ErrNoPrice or a reply nothing can price, met while the caller's context is
// done, is returned as it is. The retry meets it again, and an error that is
// real then is permanent.
func TestAgentModel_TheCallersContextDecidesBeforeAnyOtherError(t *testing.T) {
	done := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	budgetErr := fmt.Errorf("llm: budget: a call of up to 9 tokens: %w", llm.ErrBudgetExceeded)
	noPriceErr := fmt.Errorf("pricing the worst case: %w: %q", llm.ErrNoPrice, "m")

	tests := []struct {
		name  string
		model llm.Model
		opts  app.AgentModelOptions
		is    error
		same  error
	}{
		{name: "a budget refusal", model: failing(budgetErr), is: llm.ErrBudgetExceeded, same: budgetErr},
		{name: "an ErrNoPrice from the model", model: failing(noPriceErr), is: llm.ErrNoPrice, same: noPriceErr},
		{
			name:  "a reply nothing can price",
			model: answering(&llm.Response{Model: "model-z", Usage: usageA, Stop: llm.StopEnd}),
			opts:  app.AgentModelOptions{Prices: twoPrices()},
			is:    llm.ErrNoPrice,
		},
		{
			name:  "a reply that stopped for a reason agent has no name for",
			model: answering(&llm.Response{Model: "model-a", Usage: usageA, Stop: "something_new"}),
		},
		{
			name:  "a reply no empty table can price",
			model: answering(&llm.Response{Model: "model-a", Usage: usageA, Stop: llm.StopEnd}),
			opts:  app.AgentModelOptions{Prices: llm.Prices{}},
			is:    llm.ErrNoPrice,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := app.AgentModel(tt.model, tt.opts).Generate(done(), asUser("model-y", "hi"))

			require.Error(t, err)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is)
			}
			assert.NotErrorIs(t, err, agent.ErrPermanent, "a shutdown must not fail the run for good")
			assert.Equal(t, agent.Response{}, resp)
			if tt.same != nil {
				assert.True(t, isTheError(err, tt.same), "returned as it is")
			}
		})
	}

	t.Run("a real budget in front of a model, with the caller's context done", func(t *testing.T) {
		budget, err := llm.NewBudgeted(llm.NewScripted(echoScript, llm.ScriptedOptions{}), llm.BudgetOptions{MaxTokens: 10})
		require.NoError(t, err)

		_, err = app.AgentModel(budget, app.AgentModelOptions{}).Generate(done(), asUser("model-y", "hi"))

		require.ErrorIs(t, err, llm.ErrBudgetExceeded)
		assert.NotErrorIs(t, err, agent.ErrPermanent)
	})

	t.Run("the same errors with the context live are permanent", func(t *testing.T) {
		for _, tt := range tests {
			_, err := app.AgentModel(tt.model, tt.opts).Generate(context.Background(), asUser("model-y", "hi"))

			require.ErrorIs(t, err, agent.ErrPermanent, tt.name)
			if tt.is != nil {
				require.ErrorIs(t, err, tt.is, tt.name)
			}
		}
	})
}

func TestAgentModel_ANilReplyIsAnError(t *testing.T) {
	m := app.AgentModel(&stubModel{reply: func(llm.Request) (*llm.Response, error) { return nil, nil }}, app.AgentModelOptions{Prices: twoPrices()})

	resp, err := m.Generate(context.Background(), asUser("model-a", "hi"))

	require.Error(t, err)
	assert.Equal(t, agent.Response{}, resp)
	assert.NotErrorIs(t, err, agent.ErrPermanent, "the model may answer next time")
	assert.False(t, llm.Retryable(err))
}

func TestAgentModel_AnErrorWinsOverAReplyThatComesWithIt(t *testing.T) {
	boom := errors.New("boom")
	m := app.AgentModel(&stubModel{reply: func(llm.Request) (*llm.Response, error) { return plainReply(), boom }}, app.AgentModelOptions{})

	resp, err := m.Generate(context.Background(), asUser("model-a", "hi"))

	assert.True(t, isTheError(err, boom))
	assert.Equal(t, agent.Response{}, resp)
}

func TestAgentModel_NoModel(t *testing.T) {
	resp, err := app.AgentModel(nil, app.AgentModelOptions{}).Generate(context.Background(), asUser("model-a", "hi"))

	require.Error(t, err)
	assert.ErrorIs(t, err, agent.ErrPermanent, "no retry will supply one")
	assert.Equal(t, agent.Response{}, resp)
}

// The wrappers compose with the budget outermost and the meter inside it.
func TestAgentModel_ThroughTheWrappers(t *testing.T) {
	t.Run("a refused call is permanent, still a budget error, and never reaches the model or the meter", func(t *testing.T) {
		inner := llm.NewScripted(echoScript, llm.ScriptedOptions{})
		metered := llm.NewMetered(inner, llm.MeterOptions{})
		budget, err := llm.NewBudgeted(metered, llm.BudgetOptions{MaxTokens: 10})
		require.NoError(t, err)

		resp, err := app.AgentModel(budget, app.AgentModelOptions{}).Generate(context.Background(), asUser("model-a", "hi"))

		require.ErrorIs(t, err, agent.ErrPermanent)
		require.ErrorIs(t, err, llm.ErrBudgetExceeded)
		assert.Equal(t, agent.Response{}, resp)
		assert.Empty(t, inner.Requests())
		assert.Zero(t, metered.Totals().Calls)
	})

	t.Run("a model the cost limit cannot price is permanent and still an ErrNoPrice", func(t *testing.T) {
		budget, err := llm.NewBudgeted(llm.NewScripted(echoScript, llm.ScriptedOptions{}),
			llm.BudgetOptions{MaxCostMicros: 1 << 40, Prices: twoPrices()})
		require.NoError(t, err)

		_, err = app.AgentModel(budget, app.AgentModelOptions{}).Generate(context.Background(), asUser("model-z", "hi"))

		require.ErrorIs(t, err, agent.ErrPermanent)
		require.ErrorIs(t, err, llm.ErrNoPrice)
	})

	// The budget holds the request's MaxTokens, or its own default, as the most
	// the reply can be. The adapter sends one always, and its default is the
	// number the budget assumes, so the hold is the bound the call keeps to.
	t.Run("the default bound is the one the budget holds", func(t *testing.T) {
		sent := llm.Request{
			Model:    "model-a",
			Messages: []llm.Message{{Role: llm.RoleUser, Text: "hi"}},
		}
		est := llm.EstimateInputTokens(sent)
		tests := []struct {
			name    string
			opts    app.AgentModelOptions
			bound   int64
			wantErr bool
		}{
			{name: "16000 fits a limit of exactly that", bound: est + 16000},
			{name: "16000 does not fit a limit of one less", bound: est + 15999, wantErr: true},
			{name: "the option's bound fits a limit of exactly that", opts: app.AgentModelOptions{DefaultMaxTokens: 1000}, bound: est + 1000},
			{name: "the option's bound does not fit a limit of one less", opts: app.AgentModelOptions{DefaultMaxTokens: 1000}, bound: est + 999, wantErr: true},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				budget, err := llm.NewBudgeted(llm.NewScripted(echoScript, llm.ScriptedOptions{}), llm.BudgetOptions{MaxTokens: tt.bound})
				require.NoError(t, err)

				_, err = app.AgentModel(budget, tt.opts).Generate(context.Background(), asUser("model-a", "hi"))

				if tt.wantErr {
					require.ErrorIs(t, err, llm.ErrBudgetExceeded)
					return
				}
				require.NoError(t, err)
			})
		}
	})
}

func TestAgentModel_IsSafeForConcurrentUse(t *testing.T) {
	m := app.AgentModel(answering(&llm.Response{Model: "model-a-1", Usage: usageA, Stop: llm.StopEnd}),
		app.AgentModelOptions{Prices: twoPrices(), Effort: llm.EffortLow, StrictTools: true})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			req := asUser("model-a", "hi")
			req.Tools = []agent.ToolSpec{{Name: "t"}}
			resp, err := m.Generate(context.Background(), req)
			assert.NoError(t, err)
			assert.Equal(t, int64(costA), resp.Usage.CostMicros)
		})
	}
	wg.Wait()
}

// --- AgentGuard ---

// captureRecorder keeps what a Decider hands its recorder, and fails when told.
type captureRecorder struct {
	mu   sync.Mutex
	recs []policy.Record
	ctxs []context.Context
	err  error
}

func (c *captureRecorder) Record(ctx context.Context, rec policy.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, rec)
	c.ctxs = append(c.ctxs, ctx)
	return c.err
}

func (c *captureRecorder) last(t *testing.T) policy.Record {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotEmpty(t, c.recs)
	return c.recs[len(c.recs)-1]
}

func newDecider(t *testing.T, p policy.Policy, rec policy.Recorder) *policy.Decider {
	t.Helper()
	d, err := policy.NewDecider(p, policy.Options{Recorder: rec})
	require.NoError(t, err)
	return d
}

func guardRules() policy.Policy {
	return policy.Policy{
		Version: "2026-10",
		Rules: []policy.Rule{
			{Name: "Reading is allowed", Effect: policy.Allow, When: policy.Match{Kinds: []string{"read"}}},
			{Name: "Sending outside asks a person", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}, Target: "email:*"}},
			{Name: "Deleting is blocked", Effect: policy.Block, When: policy.Match{Kinds: []string{"delete"}}},
		},
	}
}

func TestAgentGuard_EachEffectAndItsRuleComeBack(t *testing.T) {
	tests := []struct {
		name     string
		policy   policy.Policy
		action   agent.Action
		want     agent.Effect
		wantRule string
	}{
		{name: "allow", policy: guardRules(), action: agent.Action{Kind: "read", Target: "file:/a"}, want: agent.Allow, wantRule: "Reading is allowed"},
		{name: "ask", policy: guardRules(), action: agent.Action{Kind: "send", Target: "email:ap@example.com"}, want: agent.Ask, wantRule: "Sending outside asks a person"},
		{name: "block", policy: guardRules(), action: agent.Action{Kind: "delete", Target: "file:/a"}, want: agent.Block, wantRule: "Deleting is blocked"},
		{name: "no rule matched is blocked, under the policy's own name for it", policy: guardRules(), action: agent.Action{Kind: "pay"}, want: agent.Block, wantRule: policy.RuleDefault},
		{
			name:     "the policy's default, when it says allow",
			policy:   policy.Policy{Default: policy.Allow},
			action:   agent.Action{Kind: "pay"},
			want:     agent.Allow,
			wantRule: policy.RuleDefault,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := app.AgentGuard(newDecider(t, tt.policy, nil))

			got, err := g.Decide(context.Background(), tt.action)

			require.NoError(t, err)
			assert.Equal(t, agent.Decision{Effect: tt.want, Rule: tt.wantRule}, got)
		})
	}
}

func TestAgentGuard_TheActionReachesTheDecider(t *testing.T) {
	rec := &captureRecorder{}
	g := app.AgentGuard(newDecider(t, guardRules(), rec))
	attrs := map[string]any{
		"amount":   json.Number("1250.50"),
		"count":    int64(3),
		"ratio":    0.1,
		"outside":  true,
		"note":     "5",
		"nothing":  nil,
		"nested":   map[string]any{"b": []any{"x", json.Number("1")}},
		"products": []string{"a", "b"},
	}

	_, err := g.Decide(context.Background(), agent.Action{Kind: "send", Target: "email:ap@example.com", Attrs: attrs})

	require.NoError(t, err)
	got := rec.last(t)
	assert.Equal(t, "send", got.Action.Kind)
	assert.Equal(t, "email:ap@example.com", got.Action.Target)
	assert.Equal(t, attrs, got.Action.Attrs)
	assert.Equal(t, "Sending outside asks a person", got.Decision.Rule)
	assert.Equal(t, "2026-10", got.Version)
}

func TestAgentGuard_NilAndEmptyAttrsStayAsTheyAre(t *testing.T) {
	rec := &captureRecorder{}
	g := app.AgentGuard(newDecider(t, guardRules(), rec))

	_, err := g.Decide(context.Background(), agent.Action{Kind: "read"})
	require.NoError(t, err)
	_, err = g.Decide(context.Background(), agent.Action{Kind: "read", Attrs: map[string]any{}})
	require.NoError(t, err)

	require.Len(t, rec.recs, 2)
	assert.Nil(t, rec.recs[0].Action.Attrs)
	assert.NotNil(t, rec.recs[1].Action.Attrs)
	assert.Empty(t, rec.recs[1].Action.Attrs)
}

func TestAgentGuard_NothingIsNormalisedOrCoerced(t *testing.T) {
	t.Run("kind, target and attribute names and strings are as written", func(t *testing.T) {
		rec := &captureRecorder{}
		g := app.AgentGuard(newDecider(t, guardRules(), rec))
		odd := agent.Action{
			Kind:   " Send ",
			Target: "Email:AP@Example.com/../x//y ",
			Attrs:  map[string]any{"Amount": "Prod ", " ": "x"},
		}

		_, err := g.Decide(context.Background(), odd)

		require.NoError(t, err)
		assert.Equal(t, policy.Action{Kind: " Send ", Target: "Email:AP@Example.com/../x//y ", Attrs: map[string]any{"Amount": "Prod ", " ": "x"}}, rec.last(t).Action)
	})

	t.Run("policy matches a kind exactly as written", func(t *testing.T) {
		g := app.AgentGuard(newDecider(t, guardRules(), nil))

		exact, err := g.Decide(context.Background(), agent.Action{Kind: "read"})
		require.NoError(t, err)
		upper, err := g.Decide(context.Background(), agent.Action{Kind: "Read"})
		require.NoError(t, err)
		padded, err := g.Decide(context.Background(), agent.Action{Kind: "read "})
		require.NoError(t, err)

		assert.Equal(t, agent.Allow, exact.Effect)
		assert.Equal(t, agent.Block, upper.Effect, "no case folding in the adapter")
		assert.Equal(t, agent.Block, padded.Effect, "no trimming in the adapter")
	})

	t.Run("a target is matched exactly as written", func(t *testing.T) {
		p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{
			{Name: "Nothing under etc", Effect: policy.Block, When: policy.Match{Target: "file:/etc/*"}},
		}}
		g := app.AgentGuard(newDecider(t, p, nil))

		blocked, err := g.Decide(context.Background(), agent.Action{Kind: "read", Target: "file:/etc/passwd"})
		require.NoError(t, err)
		doubled, err := g.Decide(context.Background(), agent.Action{Kind: "read", Target: "file:/etc//passwd"})
		require.NoError(t, err)

		assert.Equal(t, agent.Block, blocked.Effect)
		assert.Equal(t, agent.Allow, doubled.Effect, "the caller puts a target in one spelling; the adapter does not clean it")
	})

	t.Run("an attribute of the wrong type is not read as the type its rule wants", func(t *testing.T) {
		g := app.AgentGuard(newDecider(t, payRules(), nil))

		asString, err := g.Decide(context.Background(), agent.Action{Kind: "pay", Attrs: map[string]any{"amount": "1250"}})
		require.NoError(t, err)
		asNumber, err := g.Decide(context.Background(), agent.Action{Kind: "pay", Attrs: map[string]any{"amount": json.Number("1250")}})
		require.NoError(t, err)

		assert.Contains(t, asString.Rule, "could not evaluate", "a string was not coerced to the number it spells")
		assert.Equal(t, "Large payments ask", asNumber.Rule, "a json.Number is compared as it is, and is certain")
	})
}

func payRules() policy.Policy {
	return policy.Policy{Rules: []policy.Rule{
		{Name: "Small payments are allowed", Effect: policy.Allow, When: policy.Match{
			Kinds: []string{"pay"},
			Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpLte, Value: json.Number("200")}},
		}},
		{Name: "Large payments ask", Effect: policy.Ask, When: policy.Match{
			Kinds: []string{"pay"},
			Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpGt, Value: json.Number("200")}},
		}},
	}}
}

// agent.Decision carries only an effect and a rule, so what the policy could
// not evaluate is written into the rule, where the timeline and the approval a
// person is shown will read it.
func TestAgentGuard_ADecisionOnWhatCouldNotBeEvaluatedSaysSo(t *testing.T) {
	oddOrders := policy.Policy{Rules: []policy.Rule{
		{Name: "Odd orders are blocked", Effect: policy.Block, When: policy.Match{
			Kinds: []string{"order"},
			Attrs: []policy.Cond{
				{Attr: "amount", Op: policy.OpGt, Value: json.Number("200")},
				{Attr: "items", Op: policy.OpLt, Value: json.Number("5")},
				{Attr: "region", Op: policy.OpEq, Value: "home"},
			},
		}},
	}}
	tests := []struct {
		name   string
		policy policy.Policy
		action agent.Action
		want   agent.Decision
	}{
		{
			name:   "one attribute",
			policy: payRules(),
			action: agent.Action{Kind: "pay", Attrs: map[string]any{"amount": "1250"}},
			want:   agent.Decision{Effect: agent.Ask, Rule: "Large payments ask (could not evaluate: amount)"},
		},
		{
			name:   "two attributes, in the order the rule lists them, joined by a comma and a space",
			policy: oddOrders,
			action: agent.Action{Kind: "order", Attrs: map[string]any{"amount": "1250", "items": []any{1}, "region": "home"}},
			want:   agent.Decision{Effect: agent.Block, Rule: "Odd orders are blocked (could not evaluate: amount, items)"},
		},
		{
			name:   "a present attribute of no type is still one that could not be evaluated",
			policy: payRules(),
			action: agent.Action{Kind: "pay", Attrs: map[string]any{"amount": nil}},
			want:   agent.Decision{Effect: agent.Ask, Rule: "Large payments ask (could not evaluate: amount)"},
		},
		{
			name:   "a rule that matched for certain is named as it is written",
			policy: payRules(),
			action: agent.Action{Kind: "pay", Attrs: map[string]any{"amount": json.Number("900")}},
			want:   agent.Decision{Effect: agent.Ask, Rule: "Large payments ask"},
		},
		{
			name:   "an allow rule that cannot be evaluated does not match, so the default is not explained as one",
			policy: policy.Policy{Rules: []policy.Rule{payRules().Rules[0]}},
			action: agent.Action{Kind: "pay", Attrs: map[string]any{"amount": "10"}},
			want:   agent.Decision{Effect: agent.Block, Rule: policy.RuleDefault},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &captureRecorder{}
			g := app.AgentGuard(newDecider(t, tt.policy, rec))

			got, err := g.Decide(context.Background(), tt.action)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("the policy's own record keeps the rule's name and the list apart", func(t *testing.T) {
		rec := &captureRecorder{}
		g := app.AgentGuard(newDecider(t, oddOrders, rec))

		_, err := g.Decide(context.Background(), agent.Action{Kind: "order", Attrs: map[string]any{"amount": "1250", "items": []any{1}, "region": "home"}})

		require.NoError(t, err)
		assert.Equal(t, "Odd orders are blocked", rec.last(t).Decision.Rule)
		assert.Equal(t, []string{"amount", "items"}, rec.last(t).Decision.Uncertain)
	})
}

func TestAgentGuard_ARecorderThatFailsIsAnErrorAndNotAnAllow(t *testing.T) {
	disk := errors.New("disk full")
	g := app.AgentGuard(newDecider(t, policy.Policy{Default: policy.Allow}, &captureRecorder{err: disk}))

	got, err := g.Decide(context.Background(), agent.Action{Kind: "read"})

	require.ErrorIs(t, err, disk)
	assert.Equal(t, agent.Decision{}, got)
	assert.NotEqual(t, agent.Allow, got.Effect)
	assert.NotErrorIs(t, err, agent.ErrPermanent)
}

func TestAgentGuard_ADecisionThatCanNeverBeRecordedIsPermanent(t *testing.T) {
	never := fmt.Errorf("pg: the action holds a NUL: %w", policy.ErrUnrecordable)

	t.Run("a record no store can hold is permanent, and still itself", func(t *testing.T) {
		g := app.AgentGuard(newDecider(t, policy.Policy{Default: policy.Allow}, &captureRecorder{err: never}))

		got, err := g.Decide(context.Background(), agent.Action{Kind: "read"})

		require.ErrorIs(t, err, agent.ErrPermanent, "retrying cannot make the record storable")
		require.ErrorIs(t, err, policy.ErrUnrecordable)
		require.ErrorIs(t, err, never)
		assert.Equal(t, agent.Decision{}, got)
	})

	t.Run("an action too large to copy is permanent", func(t *testing.T) {
		big := make([]any, 10001)
		for i := range big {
			big[i] = i
		}
		g := app.AgentGuard(newDecider(t, policy.Policy{Default: policy.Allow}, &captureRecorder{}))

		got, err := g.Decide(context.Background(), agent.Action{Kind: "read", Attrs: map[string]any{"rows": big}})

		require.ErrorIs(t, err, policy.ErrUnrecordable)
		require.ErrorIs(t, err, agent.ErrPermanent)
		assert.Equal(t, agent.Decision{}, got)
	})

	t.Run("with the caller's context done the error comes back as it is", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		g := app.AgentGuard(newDecider(t, policy.Policy{Default: policy.Allow}, &captureRecorder{err: never}))

		_, err := g.Decide(ctx, agent.Action{Kind: "read"})

		require.ErrorIs(t, err, never)
		assert.NotErrorIs(t, err, agent.ErrPermanent, "a run being shut down is not failed for good")
	})
}

func TestAgentGuard_TheContextReachesTheRecorder(t *testing.T) {
	type key struct{}
	rec := &captureRecorder{}
	g := app.AgentGuard(newDecider(t, guardRules(), rec))

	_, err := g.Decide(context.WithValue(context.Background(), key{}, "run-7"), agent.Action{Kind: "read"})

	require.NoError(t, err)
	require.Len(t, rec.ctxs, 1)
	assert.Equal(t, "run-7", rec.ctxs[0].Value(key{}))
}

func TestAgentGuard_TheActionIsNotChanged(t *testing.T) {
	g := app.AgentGuard(newDecider(t, guardRules(), nil))
	attrs := map[string]any{"a": []any{"x"}, "b": map[string]any{"c": json.Number("1")}}
	action := agent.Action{Kind: "read", Target: "file:/a", Attrs: attrs}

	_, err := g.Decide(context.Background(), action)

	require.NoError(t, err)
	assert.Equal(t, agent.Action{Kind: "read", Target: "file:/a", Attrs: map[string]any{"a": []any{"x"}, "b": map[string]any{"c": json.Number("1")}}}, action)
}

// A guard with no decider must not read as one that allows.
func TestAgentGuard_NoDecider(t *testing.T) {
	got, err := app.AgentGuard(nil).Decide(context.Background(), agent.Action{Kind: "read"})

	require.Error(t, err)
	assert.Equal(t, agent.Decision{}, got)
}

func TestAgentGuard_IsSafeForConcurrentUse(t *testing.T) {
	rec := &captureRecorder{}
	g := app.AgentGuard(newDecider(t, guardRules(), rec))
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			got, err := g.Decide(context.Background(), agent.Action{Kind: "read", Attrs: map[string]any{"n": 1}})
			assert.NoError(t, err)
			assert.Equal(t, agent.Allow, got.Effect)
		})
	}
	wg.Wait()
	assert.Len(t, rec.recs, 32)
}
