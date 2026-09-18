package textpolicy

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Normalize trims, folds every whitespace variant (including U+00A0
// NO-BREAK SPACE and the other Unicode space separators, not just ASCII
// space) to a plain space, strips every character shouldStrip reports
// (control characters that are not whitespace, format characters, and the
// small explicit additions in extraInvisible — see defaultignorable.go),
// NFKC-folds, and collapses whitespace, in that order. [Policy.Check]
// applies it before every pattern match so a rule cannot be defeated by an
// encoding trick rather than a change in what the text says; it is exported
// so a caller with its own text to clean before storage — a display name
// pulled from an identity provider, say — can apply the same normalization
// without writing a rule to check it against.
//
// Folding whitespace to a space, rather than stripping it, matters because
// a whitespace character (a tab, a no-break space) is itself a control or
// format character by general category and would otherwise be DELETED by
// the same pass that removes a zero-width space — joining "hello<TAB>world"
// into "helloworld" instead of the "hello world" a reader sees.
//
// Stripping (and space-folding) BEFORE NFKC folding matters too, and is not
// just an arbitrary ordering choice: Unicode's normalization algorithm
// blocks composition of a base character with a following combining mark
// when a character of combining class 0 sits between them, and every
// character this function strips or folds has combining class 0. Folding
// first would leave "e" + ZERO WIDTH SPACE + COMBINING ACUTE ACCENT unfused
// into "é", so a rule written against the precomposed form would miss it —
// even though the zero-width space is gone by the time NFKC-folding
// finishes. Removing it first clears the blocker before NFKC needs to cross
// it. The result is idempotent: Normalize(Normalize(x)) == Normalize(x) for
// any x, because the second pass has nothing left to strip or fold and
// NFKC-folding an already-folded string is a no-op.
//
// Homoglyphs (a Cyrillic "а" standing in for a Latin "a", for instance) are
// explicitly out of scope: NFKC folds compatibility variants of the SAME
// character, not visually similar characters from different scripts, and
// detecting those needs a confusable-character table this package does not
// carry.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case shouldStrip(r):
			return -1
		default:
			return r
		}
	}, s)
	s = norm.NFKC.String(s)
	return strings.Join(strings.Fields(s), " ")
}

// normalize is Check's own entry point; kept as a thin, unexported alias so
// the rest of this package reads as "normalize, then match" without a
// capital letter suggesting it is meant for outside use in that spot.
func normalize(s string) string { return Normalize(s) }

// Joined concatenates parts the way a consumer will actually display them,
// with a single space between non-empty parts. Check the RESULT of Joined —
// never the parts individually — whenever a caller renders more than one
// field together, so what gets checked is what a reader will see.
func Joined(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		nonEmpty = append(nonEmpty, p)
	}
	return strings.Join(nonEmpty, " ")
}
