package deliverycheck

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func claim(id string, minutesAgo int, to string) Claim {
	return Claim{MessageID: id, Recipient: to, At: now().Add(-time.Duration(minutesAgo) * time.Minute)}
}

func now() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }

func found(to string) ProviderRecord {
	return ProviderRecord{Found: true, Recipients: []string{to}, Status: "Sent"}
}

// TestIdsTheProviderNeverHeardOfAreADivergence is the central case this
// package exists for: a job claims sends that the provider has no record of
// at all.
func TestIdsTheProviderNeverHeardOfAreADivergence(t *testing.T) {
	v := Reconcile(Input{
		Claims: []Claim{
			claim("a", 90, "buyer@example.com"),
			claim("b", 60, "buyer@example.com"),
		},
		Records:        map[string]ProviderRecord{}, // provider has none of them
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})

	assert.Equal(t, Diverged, v.Status)
	assert.Len(t, v.Missing, 2)
	assert.Contains(t, v.Reason, "no record")
}

// TestNoClaimsIsInconclusiveNotOK is the single most important case here. A
// log query that breaks, an auth failure, a changed field name — all of
// them produce zero claims. Reporting OK would make this exactly the kind
// of instrument that measures nothing and reports success.
func TestNoClaimsIsInconclusiveNotOK(t *testing.T) {
	v := Reconcile(Input{
		Claims:         nil,
		Records:        map[string]ProviderRecord{},
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})

	assert.Equal(t, Inconclusive, v.Status,
		"an empty claim set means the log query told us nothing, not that delivery is healthy")
	assert.NotEqual(t, OK, v.Status)
	assert.Contains(t, v.Reason, "no claims")
}

// TestAFailedNegativeControlVoidsTheWholeRun: a lookup that cannot fail
// proves nothing about the lookups that passed. An API that answers "yes"
// to everything would otherwise certify a dead pipeline as healthy.
func TestAFailedNegativeControlVoidsTheWholeRun(t *testing.T) {
	v := Reconcile(Input{
		Claims:         []Claim{claim("a", 90, "buyer@example.com")},
		Records:        map[string]ProviderRecord{"a": found("buyer@example.com")},
		ControlPassed:  false,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})

	assert.Equal(t, Inconclusive, v.Status,
		"every id resolved, but the lookup also resolved one that cannot exist — so "+
			"resolving proves nothing")
	assert.Contains(t, v.Reason, "control")
}

// TestRecentClaimsAreNotYetEvidence: the provider's archive lags acceptance
// by a moment, so a just-sent id that is absent is not yet evidence of
// anything.
func TestRecentClaimsAreNotYetEvidence(t *testing.T) {
	v := Reconcile(Input{
		Claims:         []Claim{claim("fresh", 2, "buyer@example.com")},
		Records:        map[string]ProviderRecord{},
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})

	assert.Equal(t, Inconclusive, v.Status)
	assert.Equal(t, 0, v.Checked)
	assert.Contains(t, v.Reason, "settle")
}

// TestOneSettledClaimIsEnoughToJudge: a settled claim beside an unsettled
// one still gets checked — otherwise a steady trickle of new sends would
// keep the window permanently inconclusive.
func TestOneSettledClaimIsEnoughToJudge(t *testing.T) {
	v := Reconcile(Input{
		Claims: []Claim{
			claim("fresh", 2, "buyer@example.com"),
			claim("old", 90, "buyer@example.com"),
		},
		Records:        map[string]ProviderRecord{},
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})

	assert.Equal(t, Diverged, v.Status)
	assert.Equal(t, 1, v.Checked, "only the settled one counts")
	require.Len(t, v.Missing, 1)
	assert.Equal(t, "old", v.Missing[0].MessageID)
}

func TestEverythingPresentIsOK(t *testing.T) {
	v := Reconcile(Input{
		Claims: []Claim{
			claim("a", 90, "buyer@example.com"),
			claim("b", 60, "other@example.com"),
		},
		Records: map[string]ProviderRecord{
			"a": found("buyer@example.com"),
			"b": found("other@example.com"),
		},
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})

	assert.Equal(t, OK, v.Status)
	assert.Equal(t, 2, v.Checked)
	assert.Empty(t, v.Missing)
}

// TestAMisaddressedMessageIsADivergence: an id that exists but was
// addressed to somebody else is worse than a missing one — the message was
// actually delivered, to the wrong person.
func TestAMisaddressedMessageIsADivergence(t *testing.T) {
	v := Reconcile(Input{
		Claims:         []Claim{claim("a", 90, "buyer@example.com")},
		Records:        map[string]ProviderRecord{"a": found("someone.else@example.com")},
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})

	assert.Equal(t, Diverged, v.Status)
	assert.Contains(t, v.Reason, "addressed")
}

// TestRecipientComparisonIsForgivingAboutCaseAndSpace: recipient comparison
// must not turn a case difference or stray whitespace into a false
// divergence. A check that reports false divergences will be disabled, and
// a disabled check verifies nothing.
func TestRecipientComparisonIsForgivingAboutCaseAndSpace(t *testing.T) {
	v := Reconcile(Input{
		Claims:         []Claim{claim("a", 90, " Buyer@Example.com ")},
		Records:        map[string]ProviderRecord{"a": found("buyer@example.com")},
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})
	assert.Equal(t, OK, v.Status)
}

// TestAClaimWithNoMessageIDIsADivergence: a claim the job logged with no id
// at all cannot be reconciled, and must not be silently skipped into an OK.
func TestAClaimWithNoMessageIDIsADivergence(t *testing.T) {
	v := Reconcile(Input{
		Claims:         []Claim{claim("", 90, "buyer@example.com")},
		Records:        map[string]ProviderRecord{},
		ControlPassed:  true,
		Now:            now(),
		SettleDuration: 30 * time.Minute,
	})
	assert.Equal(t, Diverged, v.Status)
	assert.Contains(t, v.Reason, "no message id")
}
