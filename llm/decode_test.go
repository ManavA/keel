package llm_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// verdict is the shape a structured reply is held to in these tests.
type verdict struct {
	Name  string   `json:"name"`
	Count int      `json:"count"`
	Tags  []string `json:"tags,omitempty"`
}

func ended(text string) *llm.Response {
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Text: text}, Stop: llm.StopEnd}
}

func TestDecode(t *testing.T) {
	tests := []struct {
		name string
		text string
		want verdict
	}{
		{name: "an object", text: `{"name":"batch","count":3}`, want: verdict{Name: "batch", Count: 3}},
		{name: "with a list", text: `{"name":"batch","count":3,"tags":["a","b"]}`, want: verdict{Name: "batch", Count: 3, Tags: []string{"a", "b"}}},
		{name: "a field it lacks is zero", text: `{"name":"batch"}`, want: verdict{Name: "batch"}},
		{name: "a field it does not know is ignored", text: `{"name":"batch","count":3,"extra":true}`, want: verdict{Name: "batch", Count: 3}},
		{name: "white space around it", text: "\n  {\"name\": \"batch\",\n \"count\": 3}  \n", want: verdict{Name: "batch", Count: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := llm.Decode[verdict](ended(tt.text))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("into other types", func(t *testing.T) {
		m, err := llm.Decode[map[string]any](ended(`{"a":1,"b":[true]}`))
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"a": float64(1), "b": []any{true}}, m)

		list, err := llm.Decode[[]int](ended(`[1,2,3]`))
		require.NoError(t, err)
		assert.Equal(t, []int{1, 2, 3}, list)

		s, err := llm.Decode[string](ended(`"just this"`))
		require.NoError(t, err)
		assert.Equal(t, "just this", s)

		p, err := llm.Decode[*verdict](ended(`{"name":"batch"}`))
		require.NoError(t, err)
		assert.Equal(t, &verdict{Name: "batch"}, p)
	})
}

// The text here is valid JSON that fits the type in every case, so what fails
// is the stop reason and nothing else: a refused or truncated reply need not
// match the schema, and a reply that happens to parse still did not end.
func TestDecode_RefusesAReplyThatDidNotEnd(t *testing.T) {
	stops := []llm.StopReason{
		llm.StopToolUse, llm.StopMaxTokens, llm.StopSequence, llm.StopRefusal, llm.StopPause, llm.StopContextWindow,
		"", // a response that was never given a stop reason
	}
	for _, stop := range stops {
		t.Run("stop "+string(stop), func(t *testing.T) {
			resp := ended(`{"name":"batch","count":3}`)
			resp.Stop = stop

			got, err := llm.Decode[verdict](resp)
			require.Error(t, err)
			assert.Zero(t, got, "nothing is returned with the error")
			assert.Contains(t, err.Error(), "llm: decode")
			assert.Contains(t, err.Error(), `"`+string(stop)+`"`, "the error names the stop reason")
		})
	}

	t.Run("a refusal that explains itself", func(t *testing.T) {
		resp := ended(`{"name":"batch","count":3}`)
		resp.Stop = llm.StopRefusal
		resp.Refusal = &llm.Refusal{Category: "policy", Explanation: "not something it can do"}
		_, err := llm.Decode[verdict](resp)
		assert.Error(t, err)
	})
}

func TestDecode_RefusesText(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "prose", text: "I'm sorry, I can't help with that."},
		{name: "empty", text: ""},
		{name: "only white space", text: " \n"},
		{name: "cut off", text: `{"name":"batch","count":`},
		{name: "wrapped in a code fence", text: "```json\n{\"name\":\"batch\"}\n```"},
		{name: "followed by more text", text: `{"name":"batch"} and that is all`},
		{name: "two objects", text: `{"name":"batch"}{"name":"other"}`},
		{name: "the wrong type for a field", text: `{"name":"batch","count":"three"}`},
		{name: "the wrong type for the whole", text: `["batch"]`},
		{name: "a bare word where a value goes", text: `{"name":batch}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := llm.Decode[verdict](ended(tt.text))
			require.Error(t, err)
			assert.Zero(t, got, "a partly read value is not handed back")
			assert.Contains(t, err.Error(), "llm: decode")
		})
	}

	t.Run("the cause is still there to find", func(t *testing.T) {
		_, err := llm.Decode[verdict](ended(`{"name":batch}`))
		var syntax *json.SyntaxError
		assert.True(t, errors.As(err, &syntax))

		_, err = llm.Decode[verdict](ended(`{"count":"three"}`))
		var typ *json.UnmarshalTypeError
		assert.True(t, errors.As(err, &typ))
	})
}

func TestDecode_NoResponse(t *testing.T) {
	got, err := llm.Decode[verdict](nil)
	require.Error(t, err)
	assert.Zero(t, got)
}

func TestDecode_OfAScriptedReply(t *testing.T) {
	t.Run("a reply that ended", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: `{"name":"batch","count":3}`}), llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{Output: &llm.Schema{Name: "answer", JSON: json.RawMessage(`{"type":"object"}`)}})
		require.NoError(t, err)

		got, err := llm.Decode[verdict](resp)
		require.NoError(t, err)
		assert.Equal(t, verdict{Name: "batch", Count: 3}, got)
	})

	t.Run("a reply cut short", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Text: `{"name":"batch","count":3}`, Stop: llm.StopMaxTokens}), llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)

		_, err = llm.Decode[verdict](resp)
		assert.Error(t, err)
	})

	t.Run("a refusal", func(t *testing.T) {
		s := llm.NewScripted(llm.Replies(llm.Reply{Stop: llm.StopRefusal}), llm.ScriptedOptions{})
		resp, err := s.Generate(t.Context(), llm.Request{})
		require.NoError(t, err)

		_, err = llm.Decode[verdict](resp)
		assert.Error(t, err)
	})
}
