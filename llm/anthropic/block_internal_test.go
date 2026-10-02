package anthropic

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// manyFields is a text block with n fields this package does not read.
func manyFields(n int) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"type":"text","text":"Hi."`)
	for i := range n {
		b.WriteString(`,"f` + strconv.Itoa(i) + `":1`)
	}
	b.WriteString(`}`)
	return json.RawMessage(b.String())
}

// What a block costs to hold is its bytes, however many fields it has. A
// map of its fields would cost a hundred bytes a field on top, and a body of
// 32 MiB of two-byte fields would then hold gigabytes.
func TestReadBlock_HoldsNothingPerField(t *testing.T) {
	few, many := manyFields(10), manyFields(10000)

	allocs := func(raw json.RawMessage) float64 {
		return testing.AllocsPerRun(20, func() {
			b, err := readBlock(raw)
			if err != nil || b.text != "Hi." {
				t.Fatalf("readBlock: %v %q", err, b.text)
			}
		})
	}
	assert.InDelta(t, allocs(few), allocs(many), 2, "reading a block allocates the same whatever its number of fields")
}

func TestRewrite(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		set  map[string]json.RawMessage
		want string
	}{
		{
			name: "a field is replaced where it stood, and the rest are as they came",
			raw:  `{ "type" : "text" , "text":"", "citations":[{"a":{"b":[1,2,{"c":null}]}}], "z" : -1.5e3 }`,
			set:  map[string]json.RawMessage{"text": json.RawMessage(`"Hello"`)},
			want: `{"type":"text","text":"Hello","citations":[{"a":{"b":[1,2,{"c":null}]}}],"z":-1.5e3}`,
		},
		{
			name: "a field the block lacks is added at the end",
			raw:  `{"type":"thinking","thinking":""}`,
			set:  map[string]json.RawMessage{"signature": json.RawMessage(`"sig"`), "thinking": json.RawMessage(`"t"`)},
			want: `{"type":"thinking","thinking":"t","signature":"sig"}`,
		},
		{
			name: "nothing to set",
			raw:  `{"type":"fallback","from":{"model":"a"},"to":{"model":"b"}}`,
			want: `{"type":"fallback","from":{"model":"a"},"to":{"model":"b"}}`,
		},
		{
			name: "an empty object",
			raw:  `{}`,
			set:  map[string]json.RawMessage{"text": json.RawMessage(`"x"`)},
			want: `{"text":"x"}`,
		},
		{
			name: "a field written twice is replaced once",
			raw:  `{"text":"a","type":"text","text":"b"}`,
			set:  map[string]json.RawMessage{"text": json.RawMessage(`"c"`)},
			want: `{"text":"c","type":"text"}`,
		},
		{
			name: "a value that is a string with colons and spaces in it",
			raw:  `{"type":"text","note": ": a : b ","text":""}`,
			set:  map[string]json.RawMessage{"text": json.RawMessage(`"x"`)},
			want: `{"type":"text","note":": a : b ","text":"x"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := rewrite(json.RawMessage(tt.raw), tt.set)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}

	for _, raw := range []string{`"text"`, `[1]`, `null`, `{"a":`, ``} {
		t.Run("not an object: "+raw, func(t *testing.T) {
			_, err := rewrite(json.RawMessage(raw), nil)
			require.Error(t, err)
		})
	}

	t.Run("it holds nothing per field", func(t *testing.T) {
		set := map[string]json.RawMessage{"text": json.RawMessage(`"x"`)}
		few, many := manyFields(10), manyFields(10000)
		// The decoder allocates a string for each name as it passes. What
		// matters is that none of them is kept: the output is one buffer.
		out, err := rewrite(many, set)
		require.NoError(t, err)
		assert.Len(t, out, len(many)-len(`"Hi."`)+len(`"x"`))
		_, err = rewrite(few, set)
		require.NoError(t, err)
	})
}
