// Package deliverycheck compares what a send job reported as delivered
// against what the mail provider's own records hold.
//
// # Why this exists
//
// A send job logging a message as sent proves that the job's code ran
// without erroring. It does not prove the provider received the message. A
// client library that discards an API-level failure, an account with
// invalid credentials, or a send path that has been swapped for a no-op
// fallback can all produce a normal-looking success log while no mail was
// sent. A job's own counters cannot detect any of these, because the same
// failure that breaks delivery also produces the counters.
//
// This package compares two independent sources: what the job claimed, and
// what the provider's outbound API reports. It is pure: it contains no
// network calls, only the comparison. Reading the job's claims and querying
// the provider are the caller's responsibility, which keeps this package
// testable without a provider account and usable with any provider whose
// API can answer whether it has a record of a given message id.
//
// The most important case is an empty result. A broken log query, a
// misconfigured filter, or a provider lookup that cannot distinguish found
// from not-found must be reported as inconclusive, never as reconciled and
// clean.
package deliverycheck

import (
	"fmt"
	"strings"
	"time"
)

// Status is the outcome of one reconciliation run.
type Status string

const (
	// OK means every settled claim was found at the provider, addressed
	// correctly.
	OK Status = "ok"
	// Diverged means the job claimed a send the provider cannot account
	// for, or one addressed to somebody else.
	Diverged Status = "diverged"
	// Inconclusive means this run learned nothing — which is NOT good
	// news, and is deliberately distinct from OK. See Reconcile.
	Inconclusive Status = "inconclusive"
)

// Claim is one "message sent" a job logged.
type Claim struct {
	MessageID string
	Recipient string
	At        time.Time
}

// ProviderRecord is what the provider says about one message id.
type ProviderRecord struct {
	Found      bool
	Recipients []string
	Status     string
}

// Input is everything one reconciliation needs. All IO happens before this
// is built.
type Input struct {
	Claims  []Claim
	Records map[string]ProviderRecord

	// ControlPassed reports that the provider lookup can distinguish found
	// from not-found, established by asking it about an id that cannot
	// exist. Without this, a lookup that answers "found" to everything
	// would certify a dead pipeline as healthy, so a failed (or unrun)
	// control voids the whole run rather than passing it.
	ControlPassed bool

	Now time.Time
	// SettleDuration is how long the provider's own archive may lag
	// acceptance. Claims younger than this are not yet evidence of
	// anything, in either direction.
	SettleDuration time.Duration
}

// Verdict is the reconciliation's answer.
type Verdict struct {
	Status  Status
	Checked int
	Missing []Claim
	Reason  string
}

// Reconcile compares claims against provider records.
//
// Emptiness and a failed control are checked before any per-claim work,
// because both mean the per-claim results carry no information. Reporting
// OK in either case would report a run that measured nothing as clean.
func Reconcile(in Input) Verdict {
	if !in.ControlPassed {
		return Verdict{
			Status: Inconclusive,
			Reason: "the negative control failed: the provider lookup reported an id that " +
				"cannot exist as present, so it cannot tell found from not-found and " +
				"nothing it said this run means anything",
		}
	}

	if len(in.Claims) == 0 {
		return Verdict{
			Status: Inconclusive,
			Reason: "no claims were found to check — that is a broken or empty " +
				"log query, not a healthy pipeline, and must never be reported as ok",
		}
	}

	cutoff := in.Now.Add(-in.SettleDuration)
	var settled, missing []Claim
	var misaddressed []string

	for _, c := range in.Claims {
		if c.At.After(cutoff) {
			continue // the provider's archive may not hold it yet
		}
		settled = append(settled, c)

		if c.MessageID == "" {
			missing = append(missing, c)
			continue
		}
		rec, ok := in.Records[c.MessageID]
		if !ok || !rec.Found {
			missing = append(missing, c)
			continue
		}
		if c.Recipient != "" && len(rec.Recipients) > 0 && !addressedTo(rec, c.Recipient) {
			misaddressed = append(misaddressed,
				fmt.Sprintf("%s was addressed to %s, not to %s",
					c.MessageID, strings.Join(rec.Recipients, ", "), c.Recipient))
		}
	}

	if len(settled) == 0 {
		return Verdict{
			Status: Inconclusive,
			Reason: fmt.Sprintf("all %d claims are newer than the %s settle window, so the "+
				"provider's archive may legitimately not hold them yet",
				len(in.Claims), in.SettleDuration),
		}
	}

	// Reported before the missing ones: a message that exists but reached
	// the wrong inbox is worse than one that does not exist, because it has
	// already been read.
	if len(misaddressed) > 0 {
		return Verdict{
			Status:  Diverged,
			Checked: len(settled),
			Missing: missing,
			Reason: fmt.Sprintf("%d of %d checked messages were addressed to somebody else: %s",
				len(misaddressed), len(settled), strings.Join(misaddressed, "; ")),
		}
	}

	if len(missing) > 0 {
		noID := 0
		for _, m := range missing {
			if m.MessageID == "" {
				noID++
			}
		}
		reason := fmt.Sprintf(
			"%d of %d messages the job logged as sent have no record at the provider",
			len(missing), len(settled))
		if noID > 0 {
			reason += fmt.Sprintf("; %d of them were logged with no message id at all", noID)
		}
		return Verdict{Status: Diverged, Checked: len(settled), Missing: missing, Reason: reason}
	}

	return Verdict{
		Status:  OK,
		Checked: len(settled),
		Reason:  fmt.Sprintf("all %d settled claims are present at the provider", len(settled)),
	}
}

// addressedTo compares addresses case-insensitively and ignores surrounding
// whitespace, since mail systems treat those as the same address. A
// comparison that did not would produce false-positive divergences.
func addressedTo(rec ProviderRecord, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	for _, got := range rec.Recipients {
		if strings.ToLower(strings.TrimSpace(got)) == want {
			return true
		}
	}
	return false
}
