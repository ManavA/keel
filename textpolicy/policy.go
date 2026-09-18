package textpolicy

import (
	"errors"
	"fmt"
	"regexp"
)

// Rule is one named pattern a Policy checks text against. The name is what
// gets reported on a match — never the text — so it must be meaningful on
// its own in a log line.
//
// Matching is case-sensitive unless Pattern itself says otherwise: this
// package does not fold case before matching, so a rule intended to catch
// both "Banned" and "banned" needs its own `(?i)` flag (see ExamplePolicy).
type Rule struct {
	Name    string
	Pattern *regexp.Regexp
}

// Policy is an ordered list of rules a piece of text must not match. A nil
// *Policy is valid and behaves as an empty one — Check and CheckStored
// never panic on it — so a caller that makes a Policy optional does not
// need a separate "is this configured" branch before using it.
type Policy struct {
	rules []Rule
}

// New builds a Policy from the given rules, checked in order.
func New(rules ...Rule) *Policy {
	return &Policy{rules: append([]Rule(nil), rules...)}
}

// ViolationError reports which rule matched. It never carries the text that
// matched it: the caller already has the text, and a log line built from this
// error must be safe to paste into an incident channel without republishing
// whatever tripped the guard in the first place.
type ViolationError struct {
	Rule string
}

func (e *ViolationError) Error() string {
	return fmt.Sprintf("textpolicy: rule %q matched", e.Rule)
}

// MaxInputBytes bounds how much raw text Check and CheckStored will
// normalize, checked against the input's UTF-8 byte length before any
// normalization work begins. NFKC compatibility decomposition can expand a
// single code point into a much longer sequence — repeating U+FDFA ARABIC
// LIGATURE SALLALLAHOU ALAYHE WASALLAM, which decomposes to an
// 18-character phrase, costs tens of milliseconds and tens of megabytes per
// 100 KB of input — so an unbounded caller-supplied string is a cheap way
// to cost this package a disproportionate amount of work. Lower it for a
// use case where even the default is too generous.
var MaxInputBytes = 256 * 1024 // 256 KiB

// ErrInputTooLarge is returned by Check and CheckStored when text exceeds
// MaxInputBytes. It is checked before normalization, so raising it is not a
// substitute for CheckStored's own maxLen — that one bounds the NORMALIZED
// length a caller intends to store, which can differ from the raw input's.
var ErrInputTooLarge = errors.New("textpolicy: input exceeds the maximum size checked")

// Check normalizes text (see the package doc for what that means and why)
// and reports the first rule it matches, or nil if none do. The rule order
// on the Policy is the report order: put the rule you most want surfaced
// first if more than one could match the same input.
func (p *Policy) Check(text string) error {
	if len(text) > MaxInputBytes {
		return ErrInputTooLarge
	}
	return p.checkNormalized(normalize(text))
}

// checkNormalized is Check's rule loop, taking already-normalized text so
// CheckStored can normalize once and reuse the result instead of paying for
// normalization twice.
func (p *Policy) checkNormalized(normalized string) error {
	if p == nil {
		return nil
	}
	for _, r := range p.rules {
		if r.Pattern.MatchString(normalized) {
			return &ViolationError{Rule: r.Name}
		}
	}
	return nil
}

// Rules returns a copy of the policy's rule list, in check order.
func (p *Policy) Rules() []Rule {
	if p == nil {
		return nil
	}
	return append([]Rule(nil), p.rules...)
}
