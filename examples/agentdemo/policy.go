package main

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ManavA/keel/policy"
	"github.com/ManavA/keel/textpolicy"
)

// policyJSON is the rules, shipped with the binary and parsed at startup:
// data, which a reviewer reads without reading Go.
//
//go:embed policy.json
var policyJSON []byte

func loadPolicy() (policy.Policy, error) {
	p, err := policy.Parse(policyJSON)
	if err != nil {
		return policy.Policy{}, fmt.Errorf("policy.json: %w", err)
	}
	return p, nil
}

// policy matches kinds, targets and attribute names exactly as written, so
// each is spelled once, here, and the tools build their actions from these.
// policy.json uses the same spellings.
const (
	kindRead   = "read"
	kindWrite  = "write"
	kindSend   = "send"
	kindDelete = "delete"

	attrExternal  = "external"
	attrDocuments = "documents"
	attrTextRule  = "text_rule"

	targetBatch = "documents"
)

var (
	documentIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	addressPattern    = regexp.MustCompile(`^[a-z0-9._+-]+@[a-z0-9.-]+$`)
)

// validDocumentID reports whether id has the one form a document id takes. A
// target is built only from an id that has it, so no spelling of an id
// reaches a rule that another spelling of the same id would not.
func validDocumentID(id string) bool { return documentIDPattern.MatchString(id) }

func documentTarget(id string) string {
	if !validDocumentID(id) {
		return "document:invalid"
	}
	return "document:" + id
}

func summaryTarget(id string) string {
	if !validDocumentID(id) {
		return "summary:invalid"
	}
	return "summary:" + id
}

// canonicalAddress is the one spelling of an address: trimmed and lower-case.
// It reports false for anything that is not plainly an address.
func canonicalAddress(addr string) (string, bool) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	return addr, addressPattern.MatchString(addr)
}

func emailTarget(addr string) string {
	canonical, ok := canonicalAddress(addr)
	if !ok {
		return "email:invalid"
	}
	return "email:" + canonical
}

// outgoingText is the content check on a digest. Its one rule is a
// placeholder, as textpolicy's own example is: a real service writes its own.
var outgoingText = textpolicy.New(textpolicy.Rule{
	Name:    "marked-internal-only",
	Pattern: regexp.MustCompile(`(?i)\binternal[\s-]+only\b`),
})

// textRule names the textpolicy rule that parts, read together, match. The
// two packages meet through this name: it goes into the action as text_rule,
// and a rule in policy.json blocks on it. Text that could not be checked is
// reported as a match, so that it is not sent unchecked.
func textRule(parts ...string) (string, bool) {
	err := outgoingText.Check(textpolicy.Joined(parts...))
	if err == nil {
		return "", false
	}
	var violation *textpolicy.ViolationError
	if errors.As(err, &violation) {
		return violation.Rule, true
	}
	return "could-not-be-checked", true
}
