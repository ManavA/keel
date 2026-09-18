// Package textpolicy checks generated and forwarded text against a named set
// of patterns before it reaches storage or an audience. Check reports which
// rule fired; it never echoes the text that tripped it.
//
// # Why this exists
//
// Software that republishes text it did not write — a third-party review, a
// description forwarded from an upstream feed, a message one user composed
// for another — carries responsibility for what that text says regardless of
// who wrote it first. This package is the one mechanism for that check, used
// everywhere text crosses that boundary, rather than an ad hoc regexp at each
// call site. A [Policy] is a list of named patterns; callers supply their own
// rules for their own domain, and matching is case-sensitive unless a rule's
// own pattern says otherwise. [ExamplePolicy] shows the shape of a rule list,
// not its content — its rules are placeholders. A nil *Policy is valid and
// behaves as an empty one.
//
// # Normalization
//
// [Normalize] (which Check and CheckStored both apply before doing anything
// else) trims surrounding space, folds every whitespace variant to a plain
// space, strips every Unicode control character, every format character, and
// a small set of additional invisible marks (see defaultignorable.go),
// NFKC-folds, and collapses whitespace — in that order. The order matters:
// see Normalize's own doc comment for why stripping before folding is what
// makes the function idempotent and correct at the same time.
//
// The stripping step exists because a `\b`-anchored regular expression
// treats an invisible character as a word boundary: inserting one in the
// middle of a banned word defeats the pattern without changing how the text
// reads. Folding compatibility characters (full-width letters, certain
// ligatures) closes the same kind of gap for a different encoding trick.
// Homoglyphs — a Cyrillic letter standing in for a visually identical Latin
// one — are out of scope: NFKC folds compatibility variants of the same
// character, not visually similar characters from different scripts, and
// catching those needs a confusable-character table this package does not
// carry.
//
// Check and CheckStored both reject input over MaxInputBytes, checked
// against the RAW input before normalization runs: NFKC compatibility
// decomposition can expand some single code points into much longer
// sequences, so an unbounded caller-supplied string is a cheap way to cost
// this package a disproportionate amount of CPU and memory.
//
// # Store what you checked
//
// [Policy.CheckStored] is the function that should hand a policy-checked
// value to a caller about to persist it, and the value it returns — the
// NORMALIZED text, not the original — is the one to store. Those two can
// differ in ways that matter: normalization strips characters that have no
// visible effect of their own but a real one on how software downstream
// interprets the string, such as a right-to-left override that makes stored
// text render in a different order than the order its characters are
// stored in. Storing the original instead of what CheckStored returns would
// keep that character even though it was checked against a value that no
// longer contained it.
//
// CheckStored also rejects a normalized value over a caller-supplied maximum
// length, in runes, rather than truncating it: truncation can create a word
// boundary that was not there before, so a truncated value can pass a check
// the original text failed, which makes it a different string from the one
// actually checked.
//
// The same principle applies to composition: checking two fields separately
// is not equivalent to checking what a reader sees when a caller displays
// them together. Call [Joined] (or an equivalent) and pass the result to
// Check, not the parts individually.
package textpolicy
