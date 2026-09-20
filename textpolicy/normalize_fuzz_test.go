package textpolicy

import (
	"testing"
	"unicode/utf8"
)

// FuzzNormalize checks the two properties Normalize promises in its doc
// comment — idempotency and bounded output — over arbitrary input instead of
// hand-picked cases. The table tests cover the tricky inputs someone already
// thought of; this covers the rest of Unicode: expansion blowup (one code
// point NFKC-folding into many), boundary creation (stripping joining two
// sides into a new word), and whatever a future Unicode version adds.
//
// The seed corpus is the existing TestNormalize table inputs, so the fuzzer
// starts from every case a human already thought of, plus the idempotency
// regression (an invisible between a base letter and a combining mark) and
// the largest single-character NFKC expansion known (U+FDFA).
func FuzzNormalize(f *testing.F) {
	seeds := []string{
		"  hello world  ",
		"hello    world\t\tagain",
		"bad" + string(rune(0x200B)) + "word",
		"a" + string(rune(0x200C)) + "b" + string(rune(0x200D)) + "c",
		"a" + string(rune(0x2060)) + "b",
		string(rune(0xFEFF)) + "hello",
		"hy" + string(rune(0x00AD)) + "phen",
		"text" + string(rune(0xFE0F)) + "more",
		"Ｂａｄｗｏｒｄ", // fullwidth "Badword"
		"",
		string(rune(0x200B)) + string(rune(0x200C)) + string(rune(0x200D)),
		"cafe" + string(rune(0x200B)) + string(rune(0x0301)),
		"a" + string(rune(0xFDFA)) + "b",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := Normalize(s)
		if twice := Normalize(once); twice != once {
			t.Errorf("Normalize(Normalize(%q)) = %q, want %q", s, twice, once)
		}
		if got, want := utf8.RuneCountInString(once), maxExpansionRunes*utf8.RuneCountInString(s); got > want {
			t.Errorf("Normalize(%q) expanded to %d runes, over the %dx bound (%d)", s, got, maxExpansionRunes, want)
		}
	})
}

// maxExpansionRunes bounds Normalize output in runes per input rune. NFKC
// compatibility decomposition is the only step that grows the string, and it
// works character by character: sweeping every code point shows the largest
// single-character expansion is U+FDFA (ARABIC LIGATURE SALLALLAHOU ALAYHE
// WASALLAM) folding to an 18-rune phrase. Every other step — stripping,
// trimming, whitespace collapsing, composition — only shrinks. 32 leaves
// headroom for a future Unicode version adding a larger fold while still
// catching a real blowup (quadratic growth, say) immediately.
const maxExpansionRunes = 32
