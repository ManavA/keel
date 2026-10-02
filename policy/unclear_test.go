package policy_test

// A condition holds, does not hold, or cannot be told. A rule matches for
// certain when every condition holds and does not match when any does not
// hold; when none fails but one cannot be told, an ask or block rule matches
// and an allow rule does not, and the decision names what could not be told.

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
)

// truthOf says what one condition comes to for an action's attributes, through
// the exported surface: a block rule on the condition, under a default that
// allows. Decided as block for certain it holds; decided as block with
// something uncertain it cannot be told; falling to the default it does not
// hold.
func truthOf(c policy.Cond, attrs map[string]any) string {
	p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{
		{Name: "r", Effect: policy.Block, When: policy.Match{Attrs: []policy.Cond{c}}},
	}}
	d := p.Decide(policy.Action{Kind: "k", Attrs: attrs})
	switch {
	case d.Index < 0:
		return "does not hold"
	case len(d.Uncertain) > 0:
		return "cannot tell"
	default:
		return "holds"
	}
}

const (
	holdsT   = "holds"
	unmetT   = "does not hold"
	unclearT = "cannot tell"
)

func TestCond_ThreeValues(t *testing.T) {
	five := 5
	truth := true
	tests := []struct {
		name  string
		cond  policy.Cond
		attrs map[string]any
		want  string
	}{
		// eq, a number
		{name: "eq number: the same number", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", 5), want: holdsT},
		{name: "eq number: another number", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", 6), want: unmetT},
		{name: "eq number: a string that spells it", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", "5"), want: unclearT},
		{name: "eq number: a boolean", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", true), want: unclearT},
		{name: "eq number: null", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", nil), want: unclearT},
		{name: "eq number: a list of one", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", []any{5}), want: unclearT},
		{name: "eq number: a pointer to it", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", &five), want: unclearT},
		{name: "eq number: NaN", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", math.NaN()), want: unclearT},
		{name: "eq number: an object", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("x", map[string]any{"a": 1}), want: unclearT},
		{name: "eq number: absent", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: 5}, attrs: attrs("y", 5), want: unmetT},
		// eq, a string and a boolean
		{name: "eq string: the same", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: "a"}, attrs: attrs("x", "a"), want: holdsT},
		{name: "eq string: another", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: "a"}, attrs: attrs("x", "b"), want: unmetT},
		{name: "eq string: a number", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: "a"}, attrs: attrs("x", 1), want: unclearT},
		{name: "eq boolean: true", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: true}, attrs: attrs("x", true), want: holdsT},
		{name: "eq boolean: false", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: true}, attrs: attrs("x", false), want: unmetT},
		{name: "eq boolean: the string true", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: true}, attrs: attrs("x", "true"), want: unclearT},
		{name: "eq boolean: the number one", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: true}, attrs: attrs("x", 1), want: unclearT},
		{name: "eq boolean: a pointer to true", cond: policy.Cond{Attr: "x", Op: policy.OpEq, Value: true}, attrs: attrs("x", &truth), want: unclearT},
		// ne
		{name: "ne: a different comparable value", cond: policy.Cond{Attr: "x", Op: policy.OpNe, Value: "a"}, attrs: attrs("x", "b"), want: holdsT},
		{name: "ne: the same value", cond: policy.Cond{Attr: "x", Op: policy.OpNe, Value: "a"}, attrs: attrs("x", "a"), want: unmetT},
		{name: "ne: a value of another type", cond: policy.Cond{Attr: "x", Op: policy.OpNe, Value: "a"}, attrs: attrs("x", 1), want: unclearT},
		{name: "ne: null", cond: policy.Cond{Attr: "x", Op: policy.OpNe, Value: "a"}, attrs: attrs("x", nil), want: unclearT},
		{name: "ne: absent does not hold, as for every operator", cond: policy.Cond{Attr: "x", Op: policy.OpNe, Value: "a"}, attrs: attrs("y", "b"), want: unmetT},
		// ordering
		{name: "gt: above", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", 6), want: holdsT},
		{name: "gt: below", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", 4), want: unmetT},
		{name: "gt: a string that spells a number above", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", "9"), want: unclearT},
		{name: "gt: a string that spells a number below", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", "1"), want: unclearT},
		{name: "gt: a boolean", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", true), want: unclearT},
		{name: "gt: null", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", nil), want: unclearT},
		{name: "gt: a list of one", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", []any{9}), want: unclearT},
		{name: "gt: a pointer", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", &five), want: unclearT},
		{name: "gt: NaN", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", math.NaN()), want: unclearT},
		{name: "gt: a json.Number NaN", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", json.Number("NaN")), want: unclearT},
		{name: "gt: a json.Number that is no number", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", json.Number("abc")), want: unclearT},
		{name: "gt: positive infinity is above", cond: policy.Cond{Attr: "x", Op: policy.OpGt, Value: 5}, attrs: attrs("x", math.Inf(1)), want: holdsT},
		{name: "lt: negative infinity is below", cond: policy.Cond{Attr: "x", Op: policy.OpLt, Value: 5}, attrs: attrs("x", math.Inf(-1)), want: holdsT},
		{name: "lte: absent", cond: policy.Cond{Attr: "x", Op: policy.OpLte, Value: 5}, attrs: nil, want: unmetT},
		// in
		{name: "in: a member", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []any{1, 2}}, attrs: attrs("x", 2), want: holdsT},
		{name: "in: not a member", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []any{1, 2}}, attrs: attrs("x", 3), want: unmetT},
		{name: "in: a string where the list holds numbers", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []any{1, 2}}, attrs: attrs("x", "2"), want: unclearT},
		{name: "in: a number where the list holds strings", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []string{"a", "b"}}, attrs: attrs("x", 1), want: unclearT},
		{name: "in: not a member, with an element of its own type", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []any{1, "a"}}, attrs: attrs("x", "b"), want: unmetT},
		{name: "in: a boolean where the list holds none", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []any{1, "a"}}, attrs: attrs("x", true), want: unclearT},
		{name: "in: null", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []string{"a"}}, attrs: attrs("x", nil), want: unclearT},
		{name: "in: a list", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []string{"a"}}, attrs: attrs("x", []string{"a"}), want: unclearT},
		{name: "in: absent", cond: policy.Cond{Attr: "x", Op: policy.OpIn, Value: []string{"a"}}, attrs: nil, want: unmetT},
		// exists
		{name: "exists true: present, whatever it holds", cond: policy.Cond{Attr: "x", Op: policy.OpExists, Value: true}, attrs: attrs("x", []any{}), want: holdsT},
		{name: "exists true: absent", cond: policy.Cond{Attr: "x", Op: policy.OpExists, Value: true}, attrs: nil, want: unmetT},
		{name: "exists false: absent", cond: policy.Cond{Attr: "x", Op: policy.OpExists, Value: false}, attrs: nil, want: holdsT},
		{name: "exists false: present", cond: policy.Cond{Attr: "x", Op: policy.OpExists, Value: false}, attrs: attrs("x", nil), want: unmetT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, truthOf(tt.cond, tt.attrs))
		})
	}
}

// payPolicy is a payment limit as a reviewer would write it: payments are
// allowed by kind, and a payment above 200 asks.
func payPolicy() policy.Policy {
	return policy.Policy{Rules: []policy.Rule{
		{Name: "Payments are allowed", Effect: policy.Allow, When: policy.Match{Kinds: []string{"pay"}}},
		{Name: "Payment above 200", Effect: policy.Ask, When: policy.Match{
			Kinds: []string{"pay"},
			Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpGt, Value: json.Number("200")}},
		}},
	}}
}

// Attributes come from tool input a model wrote, so an amount that arrives as
// something the operator cannot compare must not be a way round the limit.
func TestDecide_AnAmountThatCannotBeComparedAsksInsteadOfPassing(t *testing.T) {
	amount := 1250.0
	tests := []struct {
		name          string
		attrs         map[string]any
		want          policy.Effect
		wantRule      string
		wantMatched   []string
		wantUncertain []string
	}{
		{name: "a number above the limit", attrs: attrs("amount", 1250.0), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}},
		{name: "a number at the limit", attrs: attrs("amount", 200.0), want: policy.Allow, wantRule: "Payments are allowed", wantMatched: []string{"Payments are allowed"}},
		{name: "a number below the limit", attrs: attrs("amount", 150), want: policy.Allow, wantRule: "Payments are allowed", wantMatched: []string{"Payments are allowed"}},
		{name: "no amount", attrs: nil, want: policy.Allow, wantRule: "Payments are allowed", wantMatched: []string{"Payments are allowed"}},
		{name: "a string above the limit", attrs: attrs("amount", "1250"), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a string below the limit", attrs: attrs("amount", "50"), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "an empty string", attrs: attrs("amount", ""), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a pointer to a number", attrs: attrs("amount", &amount), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a list of one number", attrs: attrs("amount", []any{1250.0}), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "NaN", attrs: attrs("amount", math.NaN()), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a json.Number NaN", attrs: attrs("amount", json.Number("NaN")), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a json.Number that is no number", attrs: attrs("amount", json.Number("12,50")), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "null", attrs: attrs("amount", nil), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a boolean", attrs: attrs("amount", true), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "an object", attrs: attrs("amount", map[string]any{"value": 1250}), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a number too large for a float64", attrs: attrs("amount", json.Number("1e999")), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}},
		{name: "a number with thousands of digits", attrs: attrs("amount", json.Number("1"+strings.Repeat("0", 3000))), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}},
		{name: "a number written beyond what is compared", attrs: attrs("amount", json.Number("1e99999")), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
		{name: "a number text too long to compare", attrs: attrs("amount", json.Number(strings.Repeat("9", 5000))), want: policy.Ask, wantRule: "Payment above 200", wantMatched: []string{"Payments are allowed", "Payment above 200"}, wantUncertain: []string{"amount"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := payPolicy().Decide(policy.Action{Kind: "pay", Attrs: tt.attrs})
			assert.Equal(t, tt.want, got.Effect)
			assert.Equal(t, tt.wantRule, got.Rule)
			assert.Equal(t, tt.wantMatched, got.Matched)
			assert.Equal(t, tt.wantUncertain, got.Uncertain)
		})
	}
}

func TestDecide_ABlockRuleCountsAnAttributeItCannotCompareAgainstTheAction(t *testing.T) {
	truth := true
	p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{
		{Name: "Outside is blocked", Effect: policy.Block, When: policy.Match{Kinds: []string{"send"}, Attrs: []policy.Cond{{Attr: "external", Op: policy.OpEq, Value: true}}}},
		{Name: "Away is blocked", Effect: policy.Block, When: policy.Match{Kinds: []string{"visit"}, Attrs: []policy.Cond{{Attr: "region", Op: policy.OpNe, Value: "home"}}}},
	}}
	tests := []struct {
		name          string
		action        policy.Action
		want          policy.Effect
		wantRule      string
		wantUncertain []string
	}{
		{name: "external true", action: policy.Action{Kind: "send", Attrs: attrs("external", true)}, want: policy.Block, wantRule: "Outside is blocked"},
		{name: "external false", action: policy.Action{Kind: "send", Attrs: attrs("external", false)}, want: policy.Allow, wantRule: policy.RuleDefault},
		{name: "external as the string true", action: policy.Action{Kind: "send", Attrs: attrs("external", "true")}, want: policy.Block, wantRule: "Outside is blocked", wantUncertain: []string{"external"}},
		{name: "external as the number one", action: policy.Action{Kind: "send", Attrs: attrs("external", 1)}, want: policy.Block, wantRule: "Outside is blocked", wantUncertain: []string{"external"}},
		{name: "external as null", action: policy.Action{Kind: "send", Attrs: attrs("external", nil)}, want: policy.Block, wantRule: "Outside is blocked", wantUncertain: []string{"external"}},
		{name: "external as a list", action: policy.Action{Kind: "send", Attrs: attrs("external", []any{true})}, want: policy.Block, wantRule: "Outside is blocked", wantUncertain: []string{"external"}},
		{name: "external as a pointer", action: policy.Action{Kind: "send", Attrs: attrs("external", &truth)}, want: policy.Block, wantRule: "Outside is blocked", wantUncertain: []string{"external"}},
		{name: "external absent is not blocked: an absent attribute does not hold", action: policy.Action{Kind: "send"}, want: policy.Allow, wantRule: policy.RuleDefault},
		{name: "region away", action: policy.Action{Kind: "visit", Attrs: attrs("region", "away")}, want: policy.Block, wantRule: "Away is blocked"},
		{name: "region home", action: policy.Action{Kind: "visit", Attrs: attrs("region", "home")}, want: policy.Allow, wantRule: policy.RuleDefault},
		{name: "region as a number", action: policy.Action{Kind: "visit", Attrs: attrs("region", 7)}, want: policy.Block, wantRule: "Away is blocked", wantUncertain: []string{"region"}},
		{name: "region absent is not blocked by ne either", action: policy.Action{Kind: "visit"}, want: policy.Allow, wantRule: policy.RuleDefault},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Decide(tt.action)
			assert.Equal(t, tt.want, got.Effect)
			assert.Equal(t, tt.wantRule, got.Rule)
			assert.Equal(t, tt.wantUncertain, got.Uncertain)
		})
	}

	t.Run("a rule with exists blocks what lacks the attribute, which ne alone does not", func(t *testing.T) {
		p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{
			{Name: "Away is blocked", Effect: policy.Block, When: policy.Match{Kinds: []string{"visit"}, Attrs: []policy.Cond{{Attr: "region", Op: policy.OpNe, Value: "home"}}}},
			{Name: "No region is blocked", Effect: policy.Block, When: policy.Match{Kinds: []string{"visit"}, Attrs: []policy.Cond{{Attr: "region", Op: policy.OpExists, Value: false}}}},
		}}
		got := p.Decide(policy.Action{Kind: "visit"})
		assert.Equal(t, policy.Block, got.Effect)
		assert.Equal(t, "No region is blocked", got.Rule)
		assert.Equal(t, policy.Allow, p.Decide(policy.Action{Kind: "visit", Attrs: attrs("region", "home")}).Effect)
	})
}

func TestDecide_ConditionsAreCombinedInThreeValues(t *testing.T) {
	two := func(effect policy.Effect) policy.Policy {
		return policy.Policy{Default: policy.Allow, Rules: []policy.Rule{{Name: "r", Effect: effect, When: policy.Match{Attrs: []policy.Cond{
			{Attr: "a", Op: policy.OpGt, Value: 100},
			{Attr: "b", Op: policy.OpEq, Value: "x"},
		}}}}}
	}
	tests := []struct {
		name          string
		attrs         map[string]any
		wantBlock     bool
		wantUncertain []string
	}{
		{name: "both hold", attrs: attrs("a", 200, "b", "x"), wantBlock: true},
		{name: "the first does not hold, the second cannot be told", attrs: attrs("a", 50, "b", 7), wantBlock: false},
		{name: "the first cannot be told, the second does not hold", attrs: attrs("a", "200", "b", "y"), wantBlock: false},
		{name: "the first holds, the second cannot be told", attrs: attrs("a", 200, "b", 7), wantBlock: true, wantUncertain: []string{"b"}},
		{name: "the first cannot be told, the second holds", attrs: attrs("a", "200", "b", "x"), wantBlock: true, wantUncertain: []string{"a"}},
		{name: "neither can be told, named in the order of the conditions", attrs: attrs("a", "200", "b", 7), wantBlock: true, wantUncertain: []string{"a", "b"}},
		{name: "the first is absent, the second cannot be told", attrs: attrs("b", 7), wantBlock: false},
		{name: "the second is absent, the first cannot be told", attrs: attrs("a", "200"), wantBlock: false},
		{name: "one does not hold and one holds", attrs: attrs("a", 50, "b", "x"), wantBlock: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			block := two(policy.Block).Decide(policy.Action{Kind: "k", Attrs: tt.attrs})
			ask := two(policy.Ask).Decide(policy.Action{Kind: "k", Attrs: tt.attrs})
			allow := two(policy.Allow).Decide(policy.Action{Kind: "k", Attrs: tt.attrs})
			if tt.wantBlock {
				assert.Equal(t, policy.Block, block.Effect)
				assert.Equal(t, tt.wantUncertain, block.Uncertain)
				assert.Equal(t, policy.Ask, ask.Effect)
				assert.Equal(t, tt.wantUncertain, ask.Uncertain)
			} else {
				assert.Equal(t, policy.RuleDefault, block.Rule)
				assert.Equal(t, policy.RuleDefault, ask.Rule)
			}
			// An allow rule matches only for certain.
			if tt.wantBlock && len(tt.wantUncertain) == 0 {
				assert.Equal(t, "r", allow.Rule)
			} else {
				assert.Equal(t, policy.RuleDefault, allow.Rule)
				assert.Empty(t, allow.Matched)
			}
		})
	}

	t.Run("the same attribute in two conditions is named once", func(t *testing.T) {
		p := policy.Policy{Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{Attrs: []policy.Cond{
			{Attr: "a", Op: policy.OpGt, Value: 1}, {Attr: "a", Op: policy.OpLt, Value: 9},
		}}}}}
		assert.Equal(t, []string{"a"}, p.Decide(policy.Action{Kind: "k", Attrs: attrs("a", "five")}).Uncertain)
	})

	t.Run("a kind that is not listed does not match, whatever cannot be told", func(t *testing.T) {
		p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{
			Kinds: []string{"pay"}, Attrs: []policy.Cond{{Attr: "a", Op: policy.OpGt, Value: 1}},
		}}}}
		assert.Equal(t, policy.RuleDefault, p.Decide(policy.Action{Kind: "read", Attrs: attrs("a", "x")}).Rule)
	})

	t.Run("a target that does not match does not match, whatever cannot be told", func(t *testing.T) {
		p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{
			Target: "doc:*", Attrs: []policy.Cond{{Attr: "a", Op: policy.OpGt, Value: 1}},
		}}}}
		assert.Equal(t, policy.RuleDefault, p.Decide(policy.Action{Kind: "k", Target: "mail:1", Attrs: attrs("a", "x")}).Rule)
		assert.Equal(t, "r", p.Decide(policy.Action{Kind: "k", Target: "doc:1", Attrs: attrs("a", "x")}).Rule)
	})
}

func TestDecide_ADecisionFromAnUncertainRuleSaysSoOnlyWhileItIsTheDecider(t *testing.T) {
	cannot := policy.Cond{Attr: "a", Op: policy.OpGt, Value: 1}
	p := policy.Policy{Rules: []policy.Rule{
		{Name: "asks, cannot be told", Effect: policy.Ask, When: policy.Match{Attrs: []policy.Cond{cannot}}},
		{Name: "asks, for certain", Effect: policy.Ask},
		{Name: "blocks, cannot be told", Effect: policy.Block, When: policy.Match{Attrs: []policy.Cond{cannot}}},
		{Name: "allows, cannot be told", Effect: policy.Allow, When: policy.Match{Attrs: []policy.Cond{cannot}}},
	}}

	t.Run("an uncertain rule that is first of its strictness is the one reported, and says why", func(t *testing.T) {
		got := policy.Policy{Rules: p.Rules[:1]}.Decide(policy.Action{Kind: "k", Attrs: attrs("a", "x")})
		assert.Equal(t, policy.Decision{Effect: policy.Ask, Rule: "asks, cannot be told", Index: 0, Matched: []string{"asks, cannot be told"}, Uncertain: []string{"a"}}, got)
	})

	t.Run("a later certain rule of the same strictness does not replace it", func(t *testing.T) {
		got := policy.Policy{Rules: p.Rules[:2]}.Decide(policy.Action{Kind: "k", Attrs: attrs("a", "x")})
		assert.Equal(t, "asks, cannot be told", got.Rule)
		assert.Equal(t, []string{"a"}, got.Uncertain)
	})

	t.Run("a stricter rule replaces it, and the reasons go with the rule that decided", func(t *testing.T) {
		got := p.Decide(policy.Action{Kind: "k", Attrs: attrs("a", "x")})
		assert.Equal(t, policy.Block, got.Effect)
		assert.Equal(t, "blocks, cannot be told", got.Rule)
		assert.Equal(t, []string{"a"}, got.Uncertain)
		assert.Equal(t, []string{"asks, cannot be told", "asks, for certain", "blocks, cannot be told"}, got.Matched, "the allow rule that cannot be told did not match")
	})

	t.Run("a stricter rule that matches for certain leaves nothing uncertain", func(t *testing.T) {
		certain := policy.Policy{Rules: []policy.Rule{p.Rules[0], {Name: "blocks, for certain", Effect: policy.Block}}}
		got := certain.Decide(policy.Action{Kind: "k", Attrs: attrs("a", "x")})
		assert.Equal(t, "blocks, for certain", got.Rule)
		assert.Empty(t, got.Uncertain)
	})

	t.Run("when every condition holds nothing is uncertain", func(t *testing.T) {
		got := p.Decide(policy.Action{Kind: "k", Attrs: attrs("a", 5)})
		assert.Equal(t, "blocks, cannot be told", got.Rule)
		assert.Empty(t, got.Uncertain)
	})

	t.Run("the decision is written with what could not be told", func(t *testing.T) {
		got := policy.Policy{Rules: p.Rules[:1]}.Decide(policy.Action{Kind: "k", Attrs: attrs("a", "x")})
		out, err := json.Marshal(got)
		require.NoError(t, err)
		assert.Equal(t, `{"decision":"ask","rule":"asks, cannot be told","index":0,"matched":["asks, cannot be told"],"uncertain":["a"]}`, string(out))
		var back policy.Decision
		require.NoError(t, json.Unmarshal(out, &back))
		assert.Equal(t, got, back)
	})

	t.Run("Group carries it", func(t *testing.T) {
		g := policy.Policy{Rules: p.Rules[:1]}.Group([]policy.Action{{Kind: "k", Target: "1", Attrs: attrs("a", "x")}, {Kind: "k", Target: "2", Attrs: attrs("a", 0)}})
		require.Len(t, g.Ask, 1)
		assert.Equal(t, []string{"a"}, g.Ask[0].Decision.Uncertain)
		require.Len(t, g.Block, 1)
		assert.Empty(t, g.Block[0].Decision.Uncertain)
	})
}

// A Policy that skipped Validate can hold a rule whose match cannot be
// evaluated. That is "cannot tell", so it matches unless its effect is allow,
// and the answer is the same for a broken pattern as for a malformed condition.
func TestDecide_ARuleThatCannotBeEvaluatedMatchesUnlessItAllows(t *testing.T) {
	two := func(attr string) policy.Cond { return policy.Cond{Attr: attr, Op: policy.OpGt, Value: 1} }
	malformed := []struct {
		name          string
		match         policy.Match
		action        policy.Action
		wantUncertain []string
	}{
		{name: "a pattern that does not compile", match: policy.Match{Target: "email:["}, action: policy.Action{Kind: "send", Target: "email:["}, wantUncertain: []string{"target pattern"}},
		{name: "a pattern that does not compile, though an earlier part mismatches", match: policy.Match{Target: "x["}, action: policy.Action{Kind: "send", Target: "y"}, wantUncertain: []string{"target pattern"}},
		{name: "an operator that is not listed", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: "contains", Value: "x"}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", "x")}, wantUncertain: []string{"a"}},
		{name: "eq with no value", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpEq}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", "x")}, wantUncertain: []string{"a"}},
		{name: "ne with no value", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpNe}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", "x")}, wantUncertain: []string{"a"}},
		{name: "ne with a list", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpNe, Value: []any{"x"}}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", "y")}, wantUncertain: []string{"a"}},
		{name: "in with a value that is not a list", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpIn, Value: "x"}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", "x")}, wantUncertain: []string{"a"}},
		{name: "in with an empty list", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpIn, Value: []any{}}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", "x")}, wantUncertain: []string{"a"}},
		{name: "exists with a value that is not a boolean", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpExists, Value: "yes"}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", "x")}, wantUncertain: []string{"a"}},
		{name: "gt with a string", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpGt, Value: "5"}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", 9)}, wantUncertain: []string{"a"}},
		{name: "a condition with no attribute", match: policy.Match{Attrs: []policy.Cond{{Op: policy.OpEq, Value: 1}}}, action: policy.Action{Kind: "k", Attrs: attrs("a", 1)}, wantUncertain: []string{"condition 0"}},
		{name: "a malformed condition on an attribute the action does not carry", match: policy.Match{Attrs: []policy.Cond{{Attr: "a", Op: policy.OpEq}}}, action: policy.Action{Kind: "k"}, wantUncertain: []string{"a"}},
		{name: "a list of kinds with an empty entry, for a kind it does not list", match: policy.Match{Kinds: []string{"pay", ""}}, action: policy.Action{Kind: "read"}, wantUncertain: []string{"kinds list"}},
		{name: "two things that cannot be told are both named", match: policy.Match{Target: "[", Attrs: []policy.Cond{two("a")}}, action: policy.Action{Kind: "k", Target: "x", Attrs: attrs("a", "s")}, wantUncertain: []string{"target pattern", "a"}},
	}
	for _, tt := range malformed {
		for _, effect := range []policy.Effect{policy.Block, policy.Ask} {
			t.Run(string(effect)+": "+tt.name, func(t *testing.T) {
				p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{{Name: "r", Effect: effect, When: tt.match}}}
				got := p.Decide(tt.action)
				assert.Equal(t, effect, got.Effect)
				assert.Equal(t, "r", got.Rule)
				assert.Equal(t, tt.wantUncertain, got.Uncertain)
			})
		}
		t.Run("allow: "+tt.name, func(t *testing.T) {
			p := policy.Policy{Default: policy.Block, Rules: []policy.Rule{{Name: "r", Effect: policy.Allow, When: tt.match}}}
			got := p.Decide(tt.action)
			assert.Equal(t, policy.Decision{Effect: policy.Block, Rule: policy.RuleDefault, Index: -1}, got)
		})
	}

	// Another part that does not hold settles it.
	settled := []struct {
		name   string
		match  policy.Match
		action policy.Action
	}{
		{name: "the kind does not match", match: policy.Match{Kinds: []string{"pay"}, Target: "["}, action: policy.Action{Kind: "read"}},
		{name: "a valid pattern that does not match, with a malformed condition", match: policy.Match{Target: "doc:*", Attrs: []policy.Cond{{Attr: "a", Op: policy.OpEq}}}, action: policy.Action{Kind: "k", Target: "mail:1", Attrs: attrs("a", 1)}},
		{name: "a condition that does not hold, with a broken pattern", match: policy.Match{Target: "[", Attrs: []policy.Cond{{Attr: "a", Op: policy.OpEq, Value: 1}}}, action: policy.Action{Kind: "k", Target: "x", Attrs: attrs("a", 2)}},
		{name: "an absent attribute, with a broken pattern", match: policy.Match{Target: "[", Attrs: []policy.Cond{{Attr: "a", Op: policy.OpEq, Value: 1}}}, action: policy.Action{Kind: "k", Target: "x"}},
	}
	for _, tt := range settled {
		t.Run("does not match: "+tt.name, func(t *testing.T) {
			p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: tt.match}}}
			assert.Equal(t, policy.RuleDefault, p.Decide(tt.action).Rule)
		})
	}

	t.Run("a list of kinds with an empty entry still matches a kind it lists, for certain", func(t *testing.T) {
		p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{{Name: "r", Effect: policy.Block, When: policy.Match{Kinds: []string{"pay", ""}}}}}
		got := p.Decide(policy.Action{Kind: "pay"})
		assert.Equal(t, "r", got.Rule)
		assert.Empty(t, got.Uncertain)
	})
}

func TestCond_NumbersAreExact(t *testing.T) {
	big := "123456789012345678901234567890.123456789"
	bigPlus := "123456789012345678901234567890.123456790"
	// Not a constant, which Go would add exactly: this is the float64 sum.
	a, b := 0.1, 0.2
	sum := a + b
	tests := []struct {
		name  string
		op    policy.Op
		value any
		attr  any
		want  string
	}{
		// A float is the number it was written as, so 0.1 is 0.1.
		{name: "float64 0.1 equals the threshold 0.1", op: policy.OpEq, value: json.Number("0.1"), attr: 0.1, want: holdsT},
		{name: "float64 0.1 meets the threshold 0.1", op: policy.OpGte, value: json.Number("0.1"), attr: 0.1, want: holdsT},
		{name: "float64 0.1 is not above the threshold 0.1", op: policy.OpGt, value: json.Number("0.1"), attr: 0.1, want: unmetT},
		{name: "float64 0.1 is not below the threshold 0.1", op: policy.OpLt, value: json.Number("0.1"), attr: 0.1, want: unmetT},
		{name: "float64 0.3 meets the threshold 0.3", op: policy.OpGte, value: json.Number("0.3"), attr: 0.3, want: holdsT},
		{name: "float32 0.1 equals the threshold 0.1", op: policy.OpEq, value: json.Number("0.1"), attr: float32(0.1), want: holdsT},
		{name: "float64 0.1 plus 0.2 is above 0.3, as it is", op: policy.OpGt, value: json.Number("0.3"), attr: sum, want: holdsT},
		{name: "a float threshold and a float attribute agree", op: policy.OpLte, value: 0.7, attr: 0.7, want: holdsT},
		// Integers against floats and against text, without rounding.
		{name: "an int64 one above 2^53 is above the float64 2^53", op: policy.OpGt, value: float64(1 << 53), attr: int64(1<<53 + 1), want: holdsT},
		{name: "the same int64 is not equal to it", op: policy.OpEq, value: float64(1 << 53), attr: int64(1<<53 + 1), want: unmetT},
		{name: "the float64 2^53 is below the int64 one above it", op: policy.OpLt, value: int64(1<<53 + 1), attr: float64(1 << 53), want: holdsT},
		{name: "an int64 meets the same integer written as text", op: policy.OpEq, value: json.Number("9007199254740993"), attr: int64(9007199254740993), want: holdsT},
		{name: "a float64 2^53 is below that text", op: policy.OpLt, value: json.Number("9007199254740993"), attr: float64(1 << 53), want: holdsT},
		{name: "the largest uint64 meets its text", op: policy.OpEq, value: json.Number("18446744073709551615"), attr: uint64(math.MaxUint64), want: holdsT},
		{name: "the largest uint64 is below the next integer", op: policy.OpLt, value: json.Number("18446744073709551616"), attr: uint64(math.MaxUint64), want: holdsT},
		{name: "a negative int64 is below an unsigned zero", op: policy.OpLt, value: uint(0), attr: int64(-1), want: holdsT},
		// Text of any size or precision.
		{name: "a long decimal equals itself", op: policy.OpEq, value: json.Number(big), attr: json.Number(big), want: holdsT},
		{name: "a long decimal one last digit above is above", op: policy.OpGt, value: json.Number(big), attr: json.Number(bigPlus), want: holdsT},
		{name: "and the one below is below", op: policy.OpLt, value: json.Number(bigPlus), attr: json.Number(big), want: holdsT},
		{name: "above the largest float64", op: policy.OpGt, value: math.MaxFloat64, attr: json.Number("1e999"), want: holdsT},
		{name: "the largest float64 is below text that is larger", op: policy.OpLt, value: json.Number("1e999"), attr: math.MaxFloat64, want: holdsT},
		{name: "text one in the last place above the largest float64", op: policy.OpGt, value: json.Number("1.7976931348623157e308"), attr: json.Number("1.7976931348623158e308"), want: holdsT},
		{name: "text below the smallest float64", op: policy.OpGt, value: json.Number("0"), attr: json.Number("1e-400"), want: holdsT},
		{name: "the smallest float64 is below text just above it", op: policy.OpLt, value: json.Number("1e-323"), attr: 5e-324, want: holdsT},
		{name: "exponent forms are the numbers they spell", op: policy.OpEq, value: json.Number("100"), attr: json.Number("1E2"), want: holdsT},
		{name: "a plus in the exponent", op: policy.OpEq, value: json.Number("100"), attr: json.Number("1e+2"), want: holdsT},
		{name: "a decimal point and an exponent", op: policy.OpEq, value: json.Number("1"), attr: json.Number("0.1e1"), want: holdsT},
		{name: "minus zero is zero", op: policy.OpEq, value: json.Number("0"), attr: json.Number("-0"), want: holdsT},
		{name: "trailing zeros are the same number", op: policy.OpEq, value: json.Number("200"), attr: json.Number("200.000"), want: holdsT},
		// Bounds on what is compared.
		{name: "the largest exponent compared", op: policy.OpGt, value: json.Number("1"), attr: json.Number("1e4096"), want: holdsT},
		{name: "an exponent beyond it cannot be told", op: policy.OpGt, value: json.Number("1"), attr: json.Number("1e4097"), want: unclearT},
		{name: "a negative exponent beyond it cannot be told", op: policy.OpLt, value: json.Number("1"), attr: json.Number("1e-4097"), want: unclearT},
		{name: "text as long as is compared", op: policy.OpGt, value: json.Number("1"), attr: json.Number(strings.Repeat("9", 4096)), want: holdsT},
		{name: "text longer cannot be told", op: policy.OpGt, value: json.Number("1"), attr: json.Number(strings.Repeat("9", 4097)), want: unclearT},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := policy.Cond{Attr: "x", Op: tt.op, Value: tt.value}
			assert.Equal(t, tt.want, truthOf(c, attrs("x", tt.attr)))
		})
	}

	t.Run("text that is not a JSON number cannot be told", func(t *testing.T) {
		for _, text := range []string{"+5", "05", ".5", "5.", "1e", "1e+", "0x10", "1_000", " 5", "5 ", "Infinity", "-Infinity", "inf", "NaN", "", "-", "--1", "1.2.3", "1/2", "١٢"} {
			c := policy.Cond{Attr: "x", Op: policy.OpGt, Value: 0}
			assert.Equal(t, unclearT, truthOf(c, attrs("x", json.Number(text))), "json.Number(%q)", text)
		}
	})

	t.Run("text that is a JSON number can be told", func(t *testing.T) {
		for _, text := range []string{"0", "-0", "7", "-7", "0.5", "-0.5", "10", "1e5", "1E5", "1e+5", "1e-5", "1.5e3", "123456789012345678901234567890"} {
			c := policy.Cond{Attr: "x", Op: policy.OpGt, Value: json.Number("-1e300")}
			assert.Equal(t, holdsT, truthOf(c, attrs("x", json.Number(text))), "json.Number(%q)", text)
		}
	})
}
