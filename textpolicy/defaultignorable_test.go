package textpolicy

import (
	"testing"
	"unicode"
)

// independentDefaultIgnorable is a second, hand-transcribed copy of
// Default_Ignorable_Code_Point (Unicode 18.0.0), written directly from
// DerivedCoreProperties.txt's own line breaks rather than from
// defaultIgnorable's already-merged ranges in defaultignorable.go. The two
// tables encode the same set of code points but were not derived from one
// another, so a transcription error, an off-by-one range boundary, or a
// dropped entry in either one shows up as a mismatch here rather than
// passing because the code checked itself against itself.
var independentDefaultIgnorable = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00AD, Hi: 0x00AD, Stride: 1},
		{Lo: 0x034F, Hi: 0x034F, Stride: 1},
		{Lo: 0x061C, Hi: 0x061C, Stride: 1},
		{Lo: 0x115F, Hi: 0x1160, Stride: 1},
		{Lo: 0x17B4, Hi: 0x17B5, Stride: 1},
		{Lo: 0x180B, Hi: 0x180D, Stride: 1},
		{Lo: 0x180E, Hi: 0x180E, Stride: 1},
		{Lo: 0x180F, Hi: 0x180F, Stride: 1},
		{Lo: 0x200B, Hi: 0x200F, Stride: 1},
		{Lo: 0x202A, Hi: 0x202E, Stride: 1},
		{Lo: 0x2060, Hi: 0x2064, Stride: 1},
		{Lo: 0x2065, Hi: 0x2065, Stride: 1}, // Cn — reserved, no assigned category
		{Lo: 0x2066, Hi: 0x206F, Stride: 1},
		{Lo: 0x3164, Hi: 0x3164, Stride: 1},
		{Lo: 0xFE00, Hi: 0xFE0F, Stride: 1},
		{Lo: 0xFEFF, Hi: 0xFEFF, Stride: 1},
		{Lo: 0xFFA0, Hi: 0xFFA0, Stride: 1},
		{Lo: 0xFFF0, Hi: 0xFFF8, Stride: 1}, // Cn — reserved
	},
	R32: []unicode.Range32{
		{Lo: 0x1BCA0, Hi: 0x1BCA3, Stride: 1},
		{Lo: 0x1D173, Hi: 0x1D17A, Stride: 1},
		{Lo: 0xE0000, Hi: 0xE0000, Stride: 1}, // Cn — reserved
		{Lo: 0xE0001, Hi: 0xE0001, Stride: 1},
		{Lo: 0xE0002, Hi: 0xE001F, Stride: 1}, // Cn — reserved
		{Lo: 0xE0020, Hi: 0xE007F, Stride: 1},
		{Lo: 0xE0080, Hi: 0xE00FF, Stride: 1}, // Cn — reserved
		{Lo: 0xE0100, Hi: 0xE01EF, Stride: 1},
		{Lo: 0xE01F0, Hi: 0xE0FFF, Stride: 1}, // Cn — reserved
	},
}

func forEachRune(t *testing.T, table *unicode.RangeTable, f func(r rune)) int {
	t.Helper()
	n := 0
	for _, rng := range table.R16 {
		for c := rune(rng.Lo); c <= rune(rng.Hi); c += rune(rng.Stride) {
			f(c)
			n++
			if rng.Stride == 0 {
				break
			}
		}
	}
	for _, rng := range table.R32 {
		for c := rune(rng.Lo); c <= rune(rng.Hi); c += rune(rng.Stride) {
			f(c)
			n++
			if rng.Stride == 0 {
				break
			}
		}
	}
	return n
}

// TestIndependentTableMatchesProductionTable guards the guard: if the two
// hand-transcribed copies of Default_Ignorable_Code_Point ever disagree, every
// other test in this file that trusts independentDefaultIgnorable as ground
// truth needs to know immediately, with the exact code point named, rather
// than surfacing as a confusing failure somewhere else.
func TestIndependentTableMatchesProductionTable(t *testing.T) {
	n := forEachRune(t, independentDefaultIgnorable, func(r rune) {
		if !unicode.Is(defaultIgnorable, r) {
			t.Errorf("U+%04X is in the independent transcription but not in defaultIgnorable", r)
		}
	})
	m := forEachRune(t, defaultIgnorable, func(r rune) {
		if !unicode.Is(independentDefaultIgnorable, r) {
			t.Errorf("U+%04X is in defaultIgnorable but not in the independent transcription", r)
		}
	})
	if n != m {
		t.Errorf("the two tables cover different numbers of code points: %d vs %d", n, m)
	}
	if n != 4174 {
		t.Errorf("Default_Ignorable_Code_Point (Unicode 18.0.0) is 4,174 code points; got %d", n)
	}
}

// TestShouldStripIsSupersetOfDefaultIgnorable is the blocking check: every
// code point in Default_Ignorable_Code_Point — verified here against the
// independent transcription above, not against defaultignorable.go's own
// table — must be stripped. shouldStrip is allowed to strip MORE than this
// (it also strips every Cc and Cf character, some of which are not DICP), but
// never less. A predicate assembled only from named, assigned categories
// (Cc, Cf, Mn, Lo) rather than the property itself silently drops every
// DICP entry categorized Cn — reserved, unassigned code points with no
// category of their own — which is exactly the regression this test exists
// to catch.
func TestShouldStripIsSupersetOfDefaultIgnorable(t *testing.T) {
	n := forEachRune(t, independentDefaultIgnorable, func(r rune) {
		if !shouldStrip(r) {
			t.Errorf("U+%04X is Default_Ignorable_Code_Point but shouldStrip does not strip it", r)
		}
	})
	if n == 0 {
		t.Fatal("independentDefaultIgnorable was empty — this test verified nothing")
	}
	t.Logf("verified %d Default_Ignorable_Code_Point code points are stripped", n)
}

// TestNormalizeStripsAllOfUnicodeCf iterates the Go standard library's own
// Cf table directly — an independent source this package does not
// maintain — and checks that normalize strips every code point in it. This
// is what an entry hand-copied incorrectly, or a range boundary off by one,
// would actually be caught by: an earlier version of this table copied a
// curated SUBSET of Cf by hand and tested itself against that same subset,
// so a wrong or missing range never failed anything.
func TestNormalizeStripsAllOfUnicodeCf(t *testing.T) {
	tested := forEachRune(t, unicode.Cf, func(c rune) {
		got := normalize("a" + string(c) + "b")
		if got != "ab" {
			t.Errorf("normalize did not strip Cf code point U+%04X: got %q", c, got)
		}
	})
	if tested == 0 {
		t.Fatal("unicode.Cf was empty — this test verified nothing")
	}
	t.Logf("verified %d Cf code points", tested)
}

// TestNormalizeStripsAllControlCharacters checks every C0 and C1 control
// character (U+0000-001F, U+007F-009F) against unicode.IsControl, the
// standard library's own predicate — not a copy of it. A control character
// that is ALSO whitespace (tab, CR, NEL) folds to a space rather than
// vanishing — see Normalize's doc comment for why deleting it instead would
// join two words together.
func TestNormalizeStripsAllControlCharacters(t *testing.T) {
	for c := rune(0x0000); c <= 0x009F; c++ {
		if !unicode.IsControl(c) {
			continue
		}
		got := normalize("a" + string(c) + "b")
		want := "ab"
		if unicode.IsSpace(c) {
			want = "a b"
		}
		if got != want {
			t.Errorf("normalize did not handle control character U+%04X as expected: got %q, want %q", c, got, want)
		}
	}
}

// TestNormalizeStripsDefaultIgnorable mirrors the two tests above for the
// full property table: every code point defaultIgnorable claims must
// actually be stripped by normalize, not just by the lower-level shouldStrip
// predicate TestShouldStripIsSupersetOfDefaultIgnorable already checks.
func TestNormalizeStripsDefaultIgnorable(t *testing.T) {
	n := forEachRune(t, defaultIgnorable, func(r rune) {
		got := normalize("a" + string(r) + "b")
		if got != "ab" {
			t.Errorf("normalize did not strip U+%04X: got %q", r, got)
		}
	})
	if n != 4174 {
		t.Errorf("defaultIgnorable should cover 4,174 code points; got %d", n)
	}
}

// TestNormalizeStripsNamedBypassCharacters pins the specific characters found
// to bypass two earlier, narrower versions of this stripping: a 262-entry
// hand-picked list, and later a category-only predicate (Cc/Cf/Mn/Lo) that
// silently excluded every Cn (reserved, unassigned) DICP entry — including
// U+2065 and the four large reserved blocks under U+E0000. Each of these
// must already be caught by the tests above; this test names them
// individually so a future regression fails with a code point in the test
// name, not just a count.
func TestNormalizeStripsNamedBypassCharacters(t *testing.T) {
	named := map[string]rune{
		"left-to-right mark":            0x200E,
		"right-to-left mark":            0x200F,
		"right-to-left override":        0x202E,
		"left-to-right isolate":         0x2066,
		"combining grapheme joiner":     0x034F,
		"mongolian vowel separator":     0x180E,
		"hangul filler":                 0x3164,
		"function application":          0x2061,
		"start of heading (C0)":         0x0001,
		"escape (C0)":                   0x001B,
		"delete (C0)":                   0x007F,
		"padding character (C1)":        0x0080,
		"interlinear annotation anchor": 0xFFF9,
		"arabic number sign":            0x0600,
		"reserved-2065 (Cn)":            0x2065,
		"reserved-FFF0 (Cn)":            0xFFF0,
		"reserved-FFF8 (Cn)":            0xFFF8,
		"reserved-E0000 (Cn)":           0xE0000,
		"reserved-E0002 (Cn)":           0xE0002,
		"reserved-E001F (Cn)":           0xE001F,
		"reserved-E0080 (Cn)":           0xE0080,
		"reserved-E00FF (Cn)":           0xE00FF,
		"reserved-E01F0 (Cn)":           0xE01F0,
		"reserved-E0FFF (Cn)":           0xE0FFF,
	}
	for name, r := range named {
		t.Run(name, func(t *testing.T) {
			got := normalize("bad" + string(r) + "word")
			if got != "badword" {
				t.Errorf("normalize(%q) = %q, want %q", name, got, "badword")
			}
		})
	}
}

// TestNormalizeFoldsNoBreakSpaceToASpace confirms U+00A0 NO-BREAK SPACE —
// category Zs, not stripped by shouldStrip — still behaves as a word
// separator rather than surviving as a literal non-breaking character or
// vanishing and joining two words together. unicode.IsSpace (which
// strings.Fields uses) already covers it; this pins that behavior rather
// than assuming it.
func TestNormalizeFoldsNoBreakSpaceToASpace(t *testing.T) {
	got := normalize("bad word")
	if got != "bad word" {
		t.Errorf("normalize(%q) = %q, want %q", "bad word", got, "bad word")
	}
}

// TestNormalizeDoesNotStripOrdinaryCombiningMarks guards the other
// direction: a legitimate combining mark (used to compose an accented
// letter) must survive, so stripping does not corrupt ordinary text in
// scripts that use combining marks for spelling rather than as format
// artifacts.
func TestNormalizeDoesNotStripOrdinaryCombiningMarks(t *testing.T) {
	// U+0301 COMBINING ACUTE ACCENT: category Mn, but not stripped — it is
	// how "é" is spelled when not using the precomposed U+00E9.
	const combiningAcute = '́'
	if shouldStrip(combiningAcute) {
		t.Fatal("U+0301 (combining acute accent) must not be stripped")
	}
	// NFKC composes "e" + U+0301 into the precomposed "é" (U+00E9) — that is
	// folding working as intended, not this package stripping the mark. The
	// property under test is that the accent survives in SOME form.
	const precomposedE = "é"
	got := normalize("e" + string(combiningAcute))
	if got != precomposedE {
		t.Errorf("normalize(%q) = %q, want %q (a legitimate combining mark must survive, NFKC-composed)",
			"e"+string(combiningAcute), got, precomposedE)
	}
}
