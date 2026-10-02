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
	tests := []struct {
		name string
		stop llm.StopReason
	}{
		{name: "tool use", stop: llm.StopToolUse},
		{name: "max tokens", stop: llm.StopMaxTokens},
		{name: "stop sequence", stop: llm.StopSequence},
		{name: "refusal", stop: llm.StopRefusal},
		{name: "pause", stop: llm.StopPause},
		{name: "context window", stop: llm.StopContextWindow},
		{name: "a response that was never given a stop reason", stop: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := ended(`{"name":"batch","count":3}`)
			resp.Stop = tt.stop

			got, err := llm.Decode[verdict](resp)
			require.Error(t, err)
			assert.Zero(t, got, "nothing is returned with the error")
			assert.Contains(t, err.Error(), "llm: decode")
			assert.Contains(t, err.Error(), `"`+string(tt.stop)+`"`, "the error names the stop reason")
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

// encoding/json reads null into any type and reports nothing, which for a
// structured reply is a model that answered with no answer.
func TestDecode_RefusesNull(t *testing.T) {
	texts := []struct {
		name string
		text string
	}{
		{name: "bare", text: "null"},
		{name: "spaces around it", text: "  null  "},
		{name: "newlines and tabs around it", text: "\n\t null\r\n"},
	}
	for _, tt := range texts {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("into a struct", func(t *testing.T) {
				got, err := llm.Decode[verdict](ended(tt.text))
				require.Error(t, err)
				assert.Zero(t, got)
				assert.Contains(t, err.Error(), "llm: decode")
				assert.Contains(t, err.Error(), "null")
			})
			t.Run("into a pointer", func(t *testing.T) {
				got, err := llm.Decode[*verdict](ended(tt.text))
				require.Error(t, err)
				assert.Nil(t, got, "not a nil pointer handed back as if it were an answer")
				assert.Contains(t, err.Error(), "null")
			})
			t.Run("into a map", func(t *testing.T) {
				got, err := llm.Decode[map[string]any](ended(tt.text))
				require.Error(t, err)
				assert.Nil(t, got)
				assert.Contains(t, err.Error(), "null")
			})
			t.Run("into a slice", func(t *testing.T) {
				got, err := llm.Decode[[]int](ended(tt.text))
				require.Error(t, err)
				assert.Nil(t, got)
				assert.Contains(t, err.Error(), "null")
			})
			t.Run("into any", func(t *testing.T) {
				got, err := llm.Decode[any](ended(tt.text))
				require.Error(t, err)
				assert.Nil(t, got)
				assert.Contains(t, err.Error(), "null")
			})
		})
	}

	t.Run("the error says the output was null", func(t *testing.T) {
		_, err := llm.Decode[verdict](ended("null"))
		assert.EqualError(t, err, "llm: decode: the model's output was null")
	})

	// Only a reply that is null as a whole is refused; null inside a value, or
	// as the text of a JSON string, is an answer.
	t.Run("null that is not the whole reply is read", func(t *testing.T) {
		got, err := llm.Decode[verdict](ended(`{"name":null,"count":1,"tags":null}`))
		require.NoError(t, err)
		assert.Equal(t, verdict{Count: 1}, got)

		list, err := llm.Decode[[]*verdict](ended(`[null]`))
		require.NoError(t, err)
		assert.Equal(t, []*verdict{nil}, list)

		s, err := llm.Decode[string](ended(`"null"`))
		require.NoError(t, err)
		assert.Equal(t, "null", s)
	})

	t.Run("a word that only starts with null is not JSON at all", func(t *testing.T) {
		_, err := llm.Decode[verdict](ended("nullable"))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "was null")
	})

	t.Run("a null reply that did not end is refused for that", func(t *testing.T) {
		resp := ended("null")
		resp.Stop = llm.StopMaxTokens
		_, err := llm.Decode[verdict](resp)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_tokens")
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
