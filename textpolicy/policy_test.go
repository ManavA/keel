package textpolicy

import (
	"errors"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testPolicy() *Policy {
	return New(
		Rule{Name: "banned-word", Pattern: regexp.MustCompile(`(?i)\bbadword\b`)},
		Rule{Name: "banned-phrase", Pattern: regexp.MustCompile(`(?i)\bforbidden\s+phrase\b`)},
	)
}

func TestPolicyCheck(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantRule  string
		wantClean bool
	}{
		{name: "clean text passes", text: "a perfectly ordinary sentence", wantClean: true},
		{name: "matches a plain rule", text: "this has a badword in it", wantRule: "banned-word"},
		{name: "matches a phrase rule", text: "this is a forbidden phrase here", wantRule: "banned-phrase"},
		{
			name:     "zero-width character cannot split the match away",
			text:     "this has a bad" + string(rune(0x200B)) + "word in it",
			wantRule: "banned-word",
		},
		{name: "first matching rule wins", text: "badword and a forbidden phrase", wantRule: "banned-word"},
		{name: "case insensitive", text: "BADWORD", wantRule: "banned-word"},
		{name: "substring inside another word does not match", text: "badwording is not a word", wantClean: true},
		// A prior version of normalize stripped only a hand-picked 262 of the
		// 4174 Default_Ignorable_Code_Point characters, so each of these
		// still split a match. All are default-ignorable, and none is a
		// zero-width space (the case above already covers that one).
		{name: "left-to-right mark cannot split the match away", text: "bad" + string(rune(0x200E)) + "word", wantRule: "banned-word"},
		{name: "right-to-left mark cannot split the match away", text: "bad" + string(rune(0x200F)) + "word", wantRule: "banned-word"},
		{name: "right-to-left override cannot split the match away", text: "bad" + string(rune(0x202E)) + "word", wantRule: "banned-word"},
		{name: "left-to-right isolate cannot split the match away", text: "bad" + string(rune(0x2066)) + "word", wantRule: "banned-word"},
		{name: "combining grapheme joiner cannot split the match away", text: "bad" + string(rune(0x034F)) + "word", wantRule: "banned-word"},
		{name: "mongolian vowel separator cannot split the match away", text: "bad" + string(rune(0x180E)) + "word", wantRule: "banned-word"},
		{name: "hangul filler cannot split the match away", text: "bad" + string(rune(0x3164)) + "word", wantRule: "banned-word"},
		{name: "function application cannot split the match away", text: "bad" + string(rune(0x2061)) + "word", wantRule: "banned-word"},
	}

	p := testPolicy()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Check(tt.text)
			if tt.wantClean {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			var violation *ViolationError
			require.True(t, errors.As(err, &violation))
			assert.Equal(t, tt.wantRule, violation.Rule)
			// The error must never carry the checked text.
			assert.NotContains(t, err.Error(), tt.text)
		})
	}
}

func TestNilPolicyIsSafe(t *testing.T) {
	var p *Policy
	assert.NoError(t, p.Check("anything at all"))
	assert.Nil(t, p.Rules())

	got, err := p.CheckStored("anything at all", 0)
	require.NoError(t, err)
	assert.Equal(t, "anything at all", got)
}

func TestCheckRejectsInputOverMaxInputBytes(t *testing.T) {
	original := MaxInputBytes
	MaxInputBytes = 8
	defer func() { MaxInputBytes = original }()

	p := testPolicy()
	err := p.Check("this is much longer than eight bytes")
	assert.ErrorIs(t, err, ErrInputTooLarge)

	_, err = p.CheckStored("this is much longer than eight bytes", 0)
	assert.ErrorIs(t, err, ErrInputTooLarge)
}

func TestPolicyRulesReturnsACopy(t *testing.T) {
	p := testPolicy()
	rules := p.Rules()
	rules[0].Name = "mutated"
	assert.Equal(t, "banned-word", p.Rules()[0].Name, "mutating the returned slice must not affect the policy")
}

func TestExamplePolicyIsObviouslyAPlaceholder(t *testing.T) {
	// The example rules must not read as production content: this is a
	// regression test against someone quietly replacing them with a real list.
	for _, r := range ExamplePolicy.Rules() {
		assert.Contains(t, r.Name, "example-placeholder")
	}
	assert.NoError(t, ExamplePolicy.Check("a totally ordinary piece of text"))
	assert.Error(t, ExamplePolicy.Check("this is a badword-placeholder right here"))
}
