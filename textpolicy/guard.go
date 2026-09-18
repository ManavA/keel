package textpolicy

import (
	"errors"
	"unicode/utf8"
)

// ErrTooLong is returned by CheckStored when the NORMALIZED text exceeds
// the caller's maximum length. It is never truncated to fit — see the
// package doc for why.
var ErrTooLong = errors.New("textpolicy: input exceeds the maximum length checked")

// CheckStored is the function a caller should reach for immediately before
// persisting policy-checked text. It normalizes text once, rejects the
// result if it is longer than maxLen runes (maxLen zero or negative means
// no length limit; ErrInputTooLarge is still enforced on the raw input
// regardless of maxLen) or matches a rule, and otherwise returns the
// NORMALIZED text.
//
// Store the string CheckStored returns, not the one passed in. Those two
// can differ — Normalize strips control and format characters, including
// ones with no visible effect of their own but a real one on how software
// downstream interprets the string, such as a right-to-left override that
// makes stored text render in an order different from the order its
// characters are stored in. A caller that checks the normalized text but
// stores the original defeats that stripping entirely: the dangerous
// characters are exactly the ones Check never sees, because they are gone
// by the time Check runs, but they would still reach storage if the
// original string were what got written.
//
// The length limit is in runes, not bytes: a byte limit can cut a
// multi-byte UTF-8 character in half, and a truncation landing
// mid-character is exactly the kind of malformed value this function
// exists to refuse rather than produce. Truncating an over-long value to
// make it fit would also produce a DIFFERENT string from the one the
// policy evaluated — truncation can itself create a word boundary a rule
// was relying on not existing — so CheckStored refuses instead of
// truncating: whatever a caller writes to storage is exactly the value
// this call checked.
func (p *Policy) CheckStored(text string, maxLen int) (string, error) {
	if len(text) > MaxInputBytes {
		return "", ErrInputTooLarge
	}
	normalized := normalize(text)
	if maxLen > 0 && utf8.RuneCountInString(normalized) > maxLen {
		return "", ErrTooLong
	}
	if err := p.checkNormalized(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}
