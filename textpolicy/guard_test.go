package textpolicy

import (
	"errors"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckStored(t *testing.T) {
	p := New(Rule{Name: "banned-word", Pattern: regexp.MustCompile(`(?i)\bbadword\b`)})

	t.Run("clean text within the limit is returned unchanged", func(t *testing.T) {
		got, err := p.CheckStored("hello world", 20)
		require.NoError(t, err)
		assert.Equal(t, "hello world", got)
	})

	t.Run("over-long input is rejected, not truncated", func(t *testing.T) {
		got, err := p.CheckStored("this text is much too long", 10)
		require.ErrorIs(t, err, ErrTooLong)
		assert.Empty(t, got, "a rejected value must not be partially returned")
	})

	t.Run("zero max length means unbounded", func(t *testing.T) {
		got, err := p.CheckStored("as long as this needs to be", 0)
		require.NoError(t, err)
		assert.Equal(t, "as long as this needs to be", got)
	})

	t.Run("a rule violation is still reported even under the length limit", func(t *testing.T) {
		_, err := p.CheckStored("a badword", 50)
		var violation *ViolationError
		require.True(t, errors.As(err, &violation))
		assert.Equal(t, "banned-word", violation.Rule)
	})

	t.Run("length is checked before the policy", func(t *testing.T) {
		// Truncating "a badword" to fit 3 bytes would produce "a b", which is
		// clean — proving the value stored would differ from the value the
		// policy actually evaluated. CheckStored must refuse instead.
		_, err := p.CheckStored("a badword", 3)
		require.ErrorIs(t, err, ErrTooLong)
	})

	t.Run("the length limit counts runes, not bytes", func(t *testing.T) {
		// Five precomposed "é" (U+00E9) is 5 runes but 10 bytes. A byte-based
		// limit of 5 would reject this; the rune-based one must not.
		five := "ééééé"
		require.Len(t, five, 10, "test fixture sanity check: 5 runes of é must be 10 UTF-8 bytes")
		got, err := p.CheckStored(five, 5)
		require.NoError(t, err, "a 5-rune, 10-byte string must fit a maxLen of 5 runes")
		assert.Equal(t, five, got)

		// One rune over the limit must still be rejected, proving the check
		// is not simply "unbounded because it's multi-byte" either.
		_, err = p.CheckStored(five+"é", 5)
		require.ErrorIs(t, err, ErrTooLong)
	})

	t.Run("the returned value is the normalized one, not the original", func(t *testing.T) {
		got, err := p.CheckStored("hello"+string(rune(0x200B))+"world", 50)
		require.NoError(t, err)
		assert.Equal(t, "helloworld", got, "CheckStored must return what it actually checked")
	})

	// The regression case: a right-to-left override makes stored text
	// render in a different order than the order its characters are
	// stored in. This policy has no rule about word order or content here,
	// so the check passes either way — the property under test is that the
	// dangerous control character itself never reaches what gets returned
	// and stored, regardless of whether any rule fires on it.
	t.Run("a right-to-left override does not survive into the stored value", func(t *testing.T) {
		rtlOverride := string(rune(0x202E))
		got, err := p.CheckStored(rtlOverride+"dennab", 50)
		require.NoError(t, err)
		assert.NotContains(t, got, rtlOverride, "CheckStored must never return a value containing a directional override")
	})
}
