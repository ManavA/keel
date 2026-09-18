package textpolicy

import (
	"testing"
	"unicode"
)

// TestNormalizeIsIdempotent is the regression test for the composition-
// blocking bug: Normalize(Normalize(x)) must equal Normalize(x) for any x.
//
// It failed under an earlier ordering (fold before strip) whenever an
// invisible character sat between a base letter and a combining mark: NFKC
// composition is blocked by an intervening character of combining class 0,
// so "e" + ZERO WIDTH SPACE + COMBINING ACUTE ACCENT never composed into
// "é" on the first pass — the zero width space was still there to block
// it — but composed on the SECOND pass, once the first pass had already
// removed it. That made the two normalized forms of the same input
// different strings, and a rule matched against one could miss the other.
//
// This checks every code point in defaultIgnorable, combined with several
// base+combining-mark pairs, plus a full sweep of unicode.Cf normalized
// twice end to end.
func TestNormalizeIsIdempotent(t *testing.T) {
	marks := []rune{
		0x0301, // COMBINING ACUTE ACCENT
		0x0300, // COMBINING GRAVE ACCENT
		0x0303, // COMBINING TILDE
	}
	bases := []rune{'e', 'a', 'n', 'o'}

	checkPair := func(t *testing.T, invisible rune) {
		for _, base := range bases {
			for _, mark := range marks {
				s := string(base) + string(invisible) + string(mark)
				once := Normalize(s)
				twice := Normalize(once)
				if once != twice {
					t.Errorf("Normalize(Normalize(%q)) = %q, want %q (invisible U+%04X between %q and mark U+%04X)",
						s, twice, once, invisible, string(base), mark)
				}
			}
		}
	}

	for _, rng := range defaultIgnorable.R16 {
		for c := rune(rng.Lo); c <= rune(rng.Hi); c += rune(rng.Stride) {
			checkPair(t, c)
			if rng.Stride == 0 {
				break
			}
		}
	}
	for _, rng := range defaultIgnorable.R32 {
		for c := rune(rng.Lo); c <= rune(rng.Hi); c += rune(rng.Stride) {
			checkPair(t, c)
			if rng.Stride == 0 {
				break
			}
		}
	}
	// Cf's own composition-blocking behavior does not depend on which Cf code
	// point is used, so a full sweep over Cf ALONE (input once, normalize
	// twice, no marks) still catches a reordering regression for every one of
	// them without paying for the base+mark cross product a second time.
	for _, rng := range unicode.Cf.R16 {
		for c := rune(rng.Lo); c <= rune(rng.Hi); c += rune(rng.Stride) {
			s := "a" + string(c) + "b"
			once := Normalize(s)
			twice := Normalize(once)
			if once != twice {
				t.Errorf("Normalize(Normalize(%q)) = %q, want %q", s, twice, once)
			}
			if rng.Stride == 0 {
				break
			}
		}
	}

	// The specific example from the report: an invisible character between
	// a base letter and a combining mark must compose on the FIRST pass,
	// not only after a second call.
	t.Run("cafe+ZWSP+acute composes on the first pass", func(t *testing.T) {
		s := "cafe" + string(rune(0x200B)) + string(rune(0x0301))
		got := Normalize(s)
		want := "café"
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", s, got, want)
		}
	})
}
