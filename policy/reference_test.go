package policy_test

// This file mirrors the TypeScript reference this package must agree with, in
// the hanaML repository: web/test/demos/permissions.test.ts and
// web/test/demos/permissions-data.test.ts, which test
// web/src/demos/permissions/logic.ts and data.ts. Every decide and sortActions
// case in them is here, in the same order, and each case is named with the
// file and line of the expectation it ports. The expected effects and rule
// names are the reference's, with its "approve" written Ask. A change to
// either implementation needs the same change to the other and to this file.
//
// Two things are not ported. clampLimit clamps a number typed into the
// demonstration's form and is not policy (DESIGN.md 5.2).
// permissions-view.test.ts tests the DOM of the running tally; the three
// counts it shows, 6 allowed, 8 waiting and 0 blocked, are asserted with the
// data cases below. Where the reference asserts only a decision, the rule name
// here is the one its decide returns for that case.

import (
	"maps"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
)

// refKinds are the reference's action kinds in the order its policies list
// them.
var refKinds = []string{"read", "write", "send", "pay", "delete", "run"}

// refPolicy is the reference's fixed-shape policy.
type refPolicy struct {
	kinds     map[string]policy.Effect
	payLimit  int
	external  policy.Effect
	sensitive policy.Effect
}

// with returns r with the effect for one kind replaced.
func (r refPolicy) with(kind string, e policy.Effect) refPolicy {
	r.kinds = maps.Clone(r.kinds)
	r.kinds[kind] = e
	return r
}

// build is the rule list of DESIGN.md 5.2, in the order the reference builds
// its matches: a default for each kind, the payment limit, external, sensitive.
func (r refPolicy) build() policy.Policy {
	var rules []policy.Rule
	for _, k := range refKinds {
		rules = append(rules, policy.Rule{
			Name:   "Default for " + k + " actions",
			Effect: r.kinds[k],
			When:   policy.Match{Kinds: []string{k}},
		})
	}
	return policy.Policy{Rules: append(rules,
		policy.Rule{
			Name:   "Payment above the $" + thousands(r.payLimit) + " limit",
			Effect: policy.Ask,
			When: policy.Match{
				Kinds: []string{"pay"},
				Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpGt, Value: r.payLimit}},
			},
		},
		policy.Rule{
			Name:   "Reaches outside the company",
			Effect: r.external,
			When:   policy.Match{Attrs: []policy.Cond{{Attr: "external", Op: policy.OpEq, Value: true}}},
		},
		policy.Rule{
			Name:   "Touches sensitive data",
			Effect: r.sensitive,
			When:   policy.Match{Attrs: []policy.Cond{{Attr: "sensitive", Op: policy.OpEq, Value: true}}},
		},
	)}
}

// thousands writes n with a comma between thousands, as the reference's
// toLocaleString('en-US') does.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// The policy at the top of permissions.test.ts.
var refTestPolicy = refPolicy{
	kinds: map[string]policy.Effect{
		"read": policy.Allow, "write": policy.Allow, "send": policy.Allow, "pay": policy.Allow,
		"delete": policy.Ask, "run": policy.Ask,
	},
	payLimit:  100,
	external:  policy.Ask,
	sensitive: policy.Block,
}

// The policy the demonstration starts with, in data.ts.
var refDefaultPolicy = refPolicy{
	kinds: map[string]policy.Effect{
		"read": policy.Allow, "write": policy.Allow, "send": policy.Allow, "pay": policy.Allow,
		"delete": policy.Ask, "run": policy.Ask,
	},
	payLimit:  200,
	external:  policy.Ask,
	sensitive: policy.Ask,
}

// refAction is an action as the reference models it: the id is the target and
// the rest are attributes, set only when the reference's action sets them.
func refAction(id, agent, kind, summary string, attrs map[string]any) policy.Action {
	a := map[string]any{"agent": agent, "summary": summary}
	maps.Copy(a, attrs)
	return policy.Action{Kind: kind, Target: id, Attrs: a}
}

// testAction is the helper at the top of permissions.test.ts: id "a", agent
// "Agent", summary "x".
func testAction(kind string, attrs map[string]any) policy.Action {
	return refAction("a", "Agent", kind, "x", attrs)
}

func TestReference_Decide(t *testing.T) {
	// {...policy, kinds: {...policy.kinds, send: 'block'}, external: 'allow'}
	strict := refTestPolicy.with("send", policy.Block)
	strict.external = policy.Allow
	limit2500 := refTestPolicy
	limit2500.payLimit = 2500

	tests := []struct {
		name     string
		policy   refPolicy
		action   policy.Action
		want     policy.Effect
		wantRule string
	}{
		{
			name:   "permissions.test.ts:14 uses the default for the kind when nothing else matches (read)",
			policy: refTestPolicy, action: testAction("read", nil),
			want: policy.Allow, wantRule: "Default for read actions",
		},
		{
			name:   "permissions.test.ts:15 uses the default for the kind when nothing else matches (delete)",
			policy: refTestPolicy, action: testAction("delete", nil),
			want: policy.Ask, wantRule: "Default for delete actions",
		},
		{
			name:   "permissions.test.ts:19 asks for approval on a payment above the limit",
			policy: refTestPolicy, action: testAction("pay", map[string]any{"amount": 101.0}),
			want: policy.Ask, wantRule: "Payment above the $100 limit",
		},
		{
			name:   "permissions.test.ts:23 allows a payment at the limit",
			policy: refTestPolicy, action: testAction("pay", map[string]any{"amount": 100.0}),
			want: policy.Allow, wantRule: "Default for pay actions",
		},
		{
			name:   "permissions.test.ts:24 allows a payment with no amount",
			policy: refTestPolicy, action: testAction("pay", nil),
			want: policy.Allow, wantRule: "Default for pay actions",
		},
		{
			name:   "permissions.test.ts:29 lets the strictest rule win",
			policy: refTestPolicy, action: testAction("send", map[string]any{"external": true, "sensitive": true}),
			want: policy.Block, wantRule: "Touches sensitive data",
		},
		{
			name:   "permissions.test.ts:34 never loosens a stricter default",
			policy: strict, action: testAction("send", map[string]any{"external": true}),
			want: policy.Block, wantRule: "Default for send actions",
		},
		{
			name:   "permissions.test.ts:42 reports the earlier rule on a tie",
			policy: refTestPolicy, action: testAction("delete", map[string]any{"external": true}),
			want: policy.Ask, wantRule: "Default for delete actions",
		},
		{
			name:   "permissions.test.ts:46 writes a limit in the thousands with a separator",
			policy: limit2500, action: testAction("pay", map[string]any{"amount": 3000.0}),
			want: policy.Ask, wantRule: "Payment above the $2,500 limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.build().Decide(tt.action)
			assert.Equal(t, tt.want, got.Effect)
			assert.Equal(t, tt.wantRule, got.Rule)
		})
	}
}

// ids are the targets of a group, which hold the reference's action ids.
func ids(group []policy.Judged) []string {
	out := make([]string, 0, len(group))
	for _, j := range group {
		out = append(out, j.Action.Target)
	}
	return out
}

func TestReference_SortActions(t *testing.T) {
	tests := []struct {
		name                          string
		actions                       []policy.Action
		wantAllow, wantAsk, wantBlock []string
	}{
		{
			name: "permissions.test.ts:62 puts every action in exactly one group and keeps input order",
			actions: []policy.Action{
				refAction("1", "Agent", "read", "x", nil),
				refAction("2", "Agent", "pay", "x", map[string]any{"amount": 500.0}),
				refAction("3", "Agent", "write", "x", map[string]any{"sensitive": true}),
				refAction("4", "Agent", "write", "x", nil),
			},
			wantAllow: []string{"1", "4"}, wantAsk: []string{"2"}, wantBlock: []string{"3"},
		},
		{
			name:      "permissions.test.ts:65 gives three empty groups for no actions",
			actions:   []policy.Action{},
			wantAllow: []string{}, wantAsk: []string{}, wantBlock: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := refTestPolicy.build().Group(tt.actions)
			assert.Equal(t, tt.wantAllow, ids(got.Allow))
			assert.Equal(t, tt.wantAsk, ids(got.Ask))
			assert.Equal(t, tt.wantBlock, ids(got.Block))
		})
	}
	t.Run("permissions.test.ts:65 an empty result holds empty lists, as the reference's does", func(t *testing.T) {
		got := refTestPolicy.build().Group(nil)
		assert.Equal(t, policy.Grouped{Allow: []policy.Judged{}, Ask: []policy.Judged{}, Block: []policy.Judged{}}, got)
	})
}

// refSample is data.ts's sample actions, in the order the agents attempted
// them. The sentences keep the amounts they state and leave out the invented
// supplier names.
var refSample = []policy.Action{
	refAction("pay-courier", "Invoice agent", "pay", "Pay the $48 courier invoice.", map[string]any{"amount": 48.0}),
	refAction("read-returns", "Support agent", "read", "Read the returns policy in the help centre.", nil),
	refAction("email-customer", "Support agent", "send", "Email a customer to say their refund has been sent.", map[string]any{"external": true}),
	refAction("pay-print", "Invoice agent", "pay", "Pay the $1,250 invoice for trade show banners.", map[string]any{"amount": 1250.0}),
	refAction("delete-logs", "Ops agent", "delete", "Delete 300 log files that are more than a year old.", nil),
	refAction("post-tickets", "Support agent", "send", "Post a summary of today's open tickets in the support team channel.", nil),
	refAction("read-payroll", "Invoice agent", "read", "Read last month's payroll data to check an expense claim.", map[string]any{"sensitive": true}),
	refAction("write-equipment", "Ops agent", "write", "Update the equipment list in the shared spreadsheet.", nil),
	refAction("run-restart", "Ops agent", "run", "Run the restart script on a production server.", nil),
	refAction("write-summary", "Research agent", "write", "Save a draft market summary to the research folder.", nil),
	refAction("send-survey", "Research agent", "send", "Send survey answers with people's names to an outside research firm.", map[string]any{"external": true, "sensitive": true}),
	refAction("write-answer", "Support agent", "write", "Add a new answer to the list of common questions in the help centre.", nil),
	refAction("run-charts", "Research agent", "run", "Run a script that charts last quarter's survey results.", nil),
	refAction("delete-accounts", "Ops agent", "delete", "Delete the accounts of three people who have left the company.", map[string]any{"sensitive": true}),
}

// found is the helper in permissions-data.test.ts: the sample action with this
// id, as the policy judges it.
func found(t *testing.T, p policy.Policy, id string) policy.Judged {
	t.Helper()
	g := p.Group(refSample)
	for _, group := range [][]policy.Judged{g.Allow, g.Ask, g.Block} {
		for _, j := range group {
			if j.Action.Target == id {
				return j
			}
		}
	}
	require.FailNow(t, "no such sample action", id)
	return policy.Judged{}
}

func TestReference_SampleActions(t *testing.T) {
	t.Run("permissions-data.test.ts:13 has fourteen actions with their own ids, from the four agents", func(t *testing.T) {
		assert.Len(t, refSample, 14)
		targets := map[string]bool{}
		agents := map[string]bool{}
		for _, a := range refSample {
			targets[a.Target] = true
			agents[a.Attrs["agent"].(string)] = true
		}
		assert.Len(t, targets, 14)
		assert.Equal(t,
			map[string]bool{"Invoice agent": true, "Ops agent": true, "Research agent": true, "Support agent": true},
			agents)
	})

	for _, kind := range refKinds {
		t.Run("permissions-data.test.ts:25 covers every kind of action at least twice ("+kind+")", func(t *testing.T) {
			n := 0
			for _, a := range refSample {
				if a.Kind == kind {
					n++
				}
			}
			assert.GreaterOrEqual(t, n, 2)
		})
	}

	for _, a := range refSample {
		if a.Kind != "pay" {
			continue
		}
		t.Run("permissions-data.test.ts:31 gives every payment an amount that its sentence states ("+a.Target+")", func(t *testing.T) {
			amount := int(a.Attrs["amount"].(float64))
			assert.Contains(t, a.Attrs["summary"], "$"+thousands(amount)+" ")
		})
	}
}

func TestReference_StartingPolicy(t *testing.T) {
	start := refDefaultPolicy.build()

	blockSensitive := refDefaultPolicy
	blockSensitive.sensitive = policy.Block
	blockPay := refDefaultPolicy.with("pay", policy.Block)

	t.Run("permissions-data.test.ts:39 allows six actions, holds eight for a person and blocks none", func(t *testing.T) {
		g := start.Group(refSample)
		assert.Equal(t,
			[]string{"pay-courier", "read-returns", "post-tickets", "write-equipment", "write-summary", "write-answer"},
			ids(g.Allow))
		assert.Len(t, g.Ask, 8)
		assert.Equal(t, []string{}, ids(g.Block))
		// The three counts permissions-view.test.ts feeds the running tally.
		assert.Equal(t, [3]int{6, 8, 0}, [3]int{len(g.Allow), len(g.Ask), len(g.Block)})
	})

	tests := []struct {
		name     string
		policy   policy.Policy
		id       string
		want     policy.Effect
		wantRule string
	}{
		{
			name:   "permissions-data.test.ts:45 holds the $1,250 payment because of the limit",
			policy: start, id: "pay-print",
			want: policy.Ask, wantRule: "Payment above the $200 limit",
		},
		{
			name:   "permissions-data.test.ts:46 lets the $48 payment through",
			policy: start, id: "pay-courier",
			want: policy.Allow, wantRule: "Default for pay actions",
		},
		{
			name:   "permissions-data.test.ts:51 blocks the payroll read when sensitive data is set to block",
			policy: blockSensitive.build(), id: "read-payroll",
			want: policy.Block, wantRule: "Touches sensitive data",
		},
		{
			name:   "permissions-data.test.ts:57 blocks both payments when pay is set to block (courier)",
			policy: blockPay.build(), id: "pay-courier",
			want: policy.Block, wantRule: "Default for pay actions",
		},
		{
			name:   "permissions-data.test.ts:57 blocks both payments when pay is set to block (print)",
			policy: blockPay.build(), id: "pay-print",
			want: policy.Block, wantRule: "Default for pay actions",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := found(t, tt.policy, tt.id).Decision
			assert.Equal(t, tt.want, got.Effect)
			assert.Equal(t, tt.wantRule, got.Rule)
		})
	}
}

// The helper has to build the list DESIGN.md 5.2 gives, or the cases above
// would be held to a different policy than the reference's.
func TestReference_BuildsTheRuleListOfDesign52(t *testing.T) {
	p := refTestPolicy.build()
	require.NoError(t, p.Validate())

	names := make([]string, 0, len(p.Rules))
	for _, r := range p.Rules {
		names = append(names, r.Name)
	}
	assert.Equal(t, []string{
		"Default for read actions",
		"Default for write actions",
		"Default for send actions",
		"Default for pay actions",
		"Default for delete actions",
		"Default for run actions",
		"Payment above the $100 limit",
		"Reaches outside the company",
		"Touches sensitive data",
	}, names)
	assert.Equal(t, policy.Ask, p.Rules[6].Effect)
	assert.Equal(t, policy.Ask, p.Rules[7].Effect)
	assert.Equal(t, policy.Block, p.Rules[8].Effect)
}

func TestReference_Thousands(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"}, {100, "100"}, {1250, "1,250"}, {2500, "2,500"}, {100000, "100,000"}, {-1000, "-1,000"}, {-100, "-100"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, thousands(tt.n))
		})
	}
}
