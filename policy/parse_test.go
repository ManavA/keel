package policy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
)

// designJSON is the JSON form of a policy in DESIGN.md 5.3.
const designJSON = `{
  "version": "2026-10-02",
  "default": "block",
  "rules": [
    {"name": "Reading is allowed", "effect": "allow", "when": {"kinds": ["read"]}},
    {"name": "Sending needs a person", "effect": "ask", "when": {"kinds": ["send"]}},
    {"name": "Payment above the $200 limit", "effect": "ask",
     "when": {"kinds": ["pay"], "attrs": [{"attr": "amount", "op": "gt", "value": 200}]}},
    {"name": "Deleting documents is never allowed", "effect": "block", "when": {"kinds": ["delete"]}}
  ]
}`

// designPolicy is that JSON as a Go value, with numbers as encoding/json
// reads them.
func designPolicy() policy.Policy {
	return policy.Policy{
		Version: "2026-10-02",
		Default: policy.Block,
		Rules: []policy.Rule{
			{Name: "Reading is allowed", Effect: policy.Allow, When: policy.Match{Kinds: []string{"read"}}},
			{Name: "Sending needs a person", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}}},
			{
				Name: "Payment above the $200 limit", Effect: policy.Ask,
				When: policy.Match{Kinds: []string{"pay"}, Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpGt, Value: 200.0}}},
			},
			{Name: "Deleting documents is never allowed", Effect: policy.Block, When: policy.Match{Kinds: []string{"delete"}}},
		},
	}
}

// everyPart is a policy that uses every field and every operator.
func everyPart() policy.Policy {
	return policy.Policy{
		Version: "v2",
		Default: policy.Ask,
		Rules: []policy.Rule{
			{
				Name: "every operator", Effect: policy.Allow,
				When: policy.Match{
					Kinds:  []string{"read", "write"},
					Target: "doc:*",
					Attrs: []policy.Cond{
						{Attr: "a", Op: policy.OpEq, Value: "x"},
						{Attr: "b", Op: policy.OpNe, Value: true},
						{Attr: "c", Op: policy.OpGt, Value: 1.5},
						{Attr: "d", Op: policy.OpGte, Value: 2.0},
						{Attr: "e", Op: policy.OpLt, Value: 3.0},
						{Attr: "f", Op: policy.OpLte, Value: 4.0},
						{Attr: "g", Op: policy.OpIn, Value: []any{"p", "q", 1.0}},
						{Attr: "h", Op: policy.OpExists, Value: false},
						{Attr: "i", Op: policy.OpExists, Value: true},
					},
				},
			},
			{Name: "nothing set", Effect: policy.Block},
			{Name: "asks", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}}},
		},
	}
}

func TestParse_TheDesignsJSON(t *testing.T) {
	got, err := policy.Parse([]byte(designJSON))
	require.NoError(t, err)
	assert.Equal(t, designPolicy(), got)

	t.Run("and decides as the design says", func(t *testing.T) {
		tests := []struct {
			name     string
			action   policy.Action
			want     policy.Effect
			wantRule string
		}{
			{name: "a read", action: policy.Action{Kind: "read"}, want: policy.Allow, wantRule: "Reading is allowed"},
			{name: "a send", action: policy.Action{Kind: "send"}, want: policy.Ask, wantRule: "Sending needs a person"},
			{name: "a payment above the limit", action: policy.Action{Kind: "pay", Attrs: attrs("amount", 250)}, want: policy.Ask, wantRule: "Payment above the $200 limit"},
			{name: "a payment at the limit is no rule's", action: policy.Action{Kind: "pay", Attrs: attrs("amount", 200)}, want: policy.Block, wantRule: policy.RuleDefault},
			{name: "a delete", action: policy.Action{Kind: "delete"}, want: policy.Block, wantRule: "Deleting documents is never allowed"},
			{name: "an action nobody wrote a rule for", action: policy.Action{Kind: "launch"}, want: policy.Block, wantRule: policy.RuleDefault},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				d := got.Decide(tt.action)
				assert.Equal(t, tt.want, d.Effect)
				assert.Equal(t, tt.wantRule, d.Rule)
			})
		}
	})
}

func TestParse_RoundTrips(t *testing.T) {
	tests := []struct {
		name string
		p    policy.Policy
	}{
		{name: "the design's policy", p: designPolicy()},
		{name: "every field and operator", p: everyPart()},
		{name: "the empty policy", p: policy.Policy{}},
		{name: "a policy of no rules with a default", p: policy.Policy{Default: policy.Allow, Rules: []policy.Rule{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.p)
			require.NoError(t, err)
			got, err := policy.Parse(data)
			require.NoError(t, err)
			assert.Equal(t, tt.p, got)

			indented, err := json.MarshalIndent(tt.p, "", "  ")
			require.NoError(t, err)
			again, err := policy.Parse(indented)
			require.NoError(t, err)
			assert.Equal(t, tt.p, again)
		})
	}

	t.Run("a policy written with Go numbers decides the same after the trip", func(t *testing.T) {
		p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{
			{Name: "big", Effect: policy.Block, When: policy.Match{Attrs: []policy.Cond{
				{Attr: "n", Op: policy.OpGte, Value: 1000},
				{Attr: "kind", Op: policy.OpIn, Value: []string{"a", "b"}},
			}}},
		}}
		data, err := json.Marshal(p)
		require.NoError(t, err)
		got, err := policy.Parse(data)
		require.NoError(t, err)
		for _, a := range []policy.Action{
			{Kind: "x", Attrs: attrs("n", 1000, "kind", "a")},
			{Kind: "x", Attrs: attrs("n", 999, "kind", "a")},
			{Kind: "x", Attrs: attrs("n", 1000, "kind", "c")},
		} {
			assert.Equal(t, p.Decide(a), got.Decide(a))
		}
	})
}

func TestParse_Refuses(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "an unknown field at the top", data: `{"rules": [], "extra": 1}`, want: `unknown field "extra"`},
		{name: "an unknown field in a rule", data: `{"rules": [{"name": "a", "effect": "allow", "when": {}, "extra": 1}]}`, want: `unknown field "extra"`},
		{name: "an unknown field in a match", data: `{"rules": [{"name": "a", "effect": "allow", "when": {"kind": ["read"]}}]}`, want: `unknown field "kind"`},
		{name: "an unknown field in a condition", data: `{"rules": [{"name": "a", "effect": "allow", "when": {"attrs": [{"attr": "x", "op": "eq", "val": 1}]}}]}`, want: `unknown field "val"`},
		{name: "an effect that is not one of the three", data: `{"rules": [{"name": "a", "effect": "deny", "when": {}}]}`, want: `deny`},
		{name: "an effect of the wrong type", data: `{"rules": [{"name": "a", "effect": 1, "when": {}}]}`, want: `effect`},
		{name: "a default that is not one of the three", data: `{"default": "deny", "rules": []}`, want: `deny`},
		{name: "a rule that fails validation", data: `{"rules": [{"effect": "allow", "when": {}}]}`, want: "no name"},
		{name: "an empty rule effect", data: `{"rules": [{"name": "a", "effect": "", "when": {}}]}`, want: "effect"},
		{name: "no effect at all", data: `{"rules": [{"name": "a", "when": {}}]}`, want: "effect"},
		{name: "text that is not JSON", data: `not json`, want: "policy: parse"},
		{name: "no input", data: ``, want: "policy: parse"},
		{name: "rules of the wrong type", data: `{"rules": 5}`, want: "rules"},
		{name: "a second value after the policy", data: `{"rules": []} {"rules": []}`, want: "after the policy"},
		{name: "text after the policy", data: `{"rules": []} x`, want: "after the policy"},
		{name: "a policy cut short", data: `{"rules": [`, want: "policy: parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := policy.Parse([]byte(tt.data))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), "policy: parse")
			assert.Equal(t, policy.Policy{}, got, "a policy that does not parse is not partly returned")
		})
	}

	t.Run("a bad target is refused by Parse, not left for Decide", func(t *testing.T) {
		_, err := policy.Parse([]byte(`{"rules": [{"name": "a", "effect": "allow", "when": {"target": "["}}]}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "target")
	})
}

func TestParse_Approve(t *testing.T) {
	t.Run("approve decodes as ask in a rule and as the default", func(t *testing.T) {
		got, err := policy.Parse([]byte(`{"default": "approve", "rules": [{"name": "a", "effect": "approve", "when": {}}]}`))
		require.NoError(t, err)
		assert.Equal(t, policy.Ask, got.Default)
		require.Len(t, got.Rules, 1)
		assert.Equal(t, policy.Ask, got.Rules[0].Effect)
	})

	t.Run("and encodes as ask", func(t *testing.T) {
		got, err := policy.Parse([]byte(`{"default": "approve", "rules": [{"name": "a", "effect": "approve", "when": {}}]}`))
		require.NoError(t, err)
		out, err := json.Marshal(got)
		require.NoError(t, err)
		assert.JSONEq(t, `{"default": "ask", "rules": [{"name": "a", "effect": "ask", "when": {}}]}`, string(out))
		assert.NotContains(t, string(out), "approve")
	})

	t.Run("a policy the reference's fixtures describe decodes unchanged and decides as it does", func(t *testing.T) {
		for name, ref := range map[string]refPolicy{
			"the test policy":    refTestPolicy,
			"the demo's default": refDefaultPolicy,
			"send blocked":       refTestPolicy.with("send", policy.Block),
		} {
			built := ref.build()
			data, err := json.Marshal(built)
			require.NoError(t, err)
			// The reference writes ask as approve.
			fixture := strings.ReplaceAll(string(data), `"effect":"ask"`, `"effect":"approve"`)
			require.Contains(t, fixture, `"effect":"approve"`, name)

			parsed, err := policy.Parse([]byte(fixture))
			require.NoError(t, err, name)
			for _, a := range refSample {
				assert.Equal(t, built.Decide(a), parsed.Decide(a), "%s: %s", name, a.Target)
			}
		}
	})
}

func TestEffect_UnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    policy.Effect
		wantErr bool
	}{
		{name: "allow", text: "allow", want: policy.Allow},
		{name: "ask", text: "ask", want: policy.Ask},
		{name: "block", text: "block", want: policy.Block},
		{name: "approve is ask", text: "approve", want: policy.Ask},
		{name: "empty is the zero effect", text: "", want: ""},
		{name: "another case is refused", text: "Approve", wantErr: true},
		{name: "another word is refused", text: "deny", wantErr: true},
		{name: "space is refused", text: " allow", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := policy.Effect("untouched")
			err := e.UnmarshalText([]byte(tt.text))
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "policy")
				assert.Equal(t, policy.Effect("untouched"), e, "an effect that is refused leaves the value alone")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, e)
		})
	}
}

func TestDecision_JSON(t *testing.T) {
	tests := []struct {
		name string
		d    policy.Decision
		want string
	}{
		{
			name: "a rule decided",
			d:    policy.Decision{Effect: policy.Ask, Rule: "Sending needs a person", Index: 1, Matched: []string{"Sending needs a person", "Anything"}},
			want: `{"decision":"ask","rule":"Sending needs a person","index":1,"matched":["Sending needs a person","Anything"]}`,
		},
		{
			name: "no rule matched",
			d:    policy.Decision{Effect: policy.Block, Rule: policy.RuleDefault, Index: -1},
			want: `{"decision":"block","rule":"no rule matched","index":-1}`,
		},
		{
			name: "the zero decision",
			d:    policy.Decision{},
			want: `{"decision":"","rule":"","index":0}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.d)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))

			var back policy.Decision
			require.NoError(t, json.Unmarshal(got, &back))
			assert.Equal(t, tt.d, back)
		})
	}

	t.Run("the reference's verdict reads as a decision", func(t *testing.T) {
		var d policy.Decision
		require.NoError(t, json.Unmarshal([]byte(`{"decision":"approve","rule":"Reaches outside the company"}`), &d))
		assert.Equal(t, policy.Ask, d.Effect)
		assert.Equal(t, "Reaches outside the company", d.Rule)
	})
}

func TestValidate(t *testing.T) {
	// withCond is a rule holding one condition on "x".
	withCond := func(op policy.Op, v any) policy.Rule {
		return policy.Rule{Name: "c", Effect: policy.Allow, When: cond("x", op, v)}
	}
	rule := func(name string, e policy.Effect) policy.Rule { return policy.Rule{Name: name, Effect: e} }

	refused := []struct {
		name string
		p    policy.Policy
		want string
	}{
		{name: "a rule with no name", p: policy.Policy{Rules: []policy.Rule{rule("", policy.Allow)}}, want: "rule 0 has no name"},
		{name: "a later rule with no name", p: policy.Policy{Rules: []policy.Rule{rule("a", policy.Allow), rule("", policy.Allow)}}, want: "rule 1 has no name"},
		{name: "two rules with one name", p: policy.Policy{Rules: []policy.Rule{rule("a", policy.Allow), rule("b", policy.Ask), rule("a", policy.Block)}}, want: `rules 0 and 2 are both named "a"`},
		{name: "a rule with no effect", p: policy.Policy{Rules: []policy.Rule{rule("a", "")}}, want: `rule 0 ("a"): effect ""`},
		{name: "a rule with an effect that is not one of the three", p: policy.Policy{Rules: []policy.Rule{rule("a", "deny")}}, want: `rule 0 ("a"): effect "deny"`},
		{name: "a rule with the reference's word as a Go value", p: policy.Policy{Rules: []policy.Rule{rule("a", "approve")}}, want: `effect "approve"`},
		{name: "a default that is not one of the three", p: policy.Policy{Default: "deny"}, want: `default "deny"`},
		{name: "a default of the reference's word as a Go value", p: policy.Policy{Default: "approve"}, want: `default "approve"`},
		{name: "a target pattern that does not compile", p: policy.Policy{Rules: []policy.Rule{{Name: "a", Effect: policy.Allow, When: policy.Match{Target: "email:["}}}}, want: `rule 0 ("a"): target pattern "email:["`},
		{name: "a target pattern with an unclosed class after a literal", p: policy.Policy{Rules: []policy.Rule{{Name: "a", Effect: policy.Allow, When: policy.Match{Target: "x[a-"}}}}, want: "target pattern"},
		{name: "a target pattern that ends in an escape", p: policy.Policy{Rules: []policy.Rule{{Name: "a", Effect: policy.Allow, When: policy.Match{Target: `a\`}}}}, want: "target pattern"},
		{name: "a target pattern with an empty class", p: policy.Policy{Rules: []policy.Rule{{Name: "a", Effect: policy.Allow, When: policy.Match{Target: "a[]"}}}}, want: "target pattern"},
		{name: "an operator that is not listed", p: policy.Policy{Rules: []policy.Rule{withCond("contains", "a")}}, want: `unknown operator "contains"`},
		{name: "no operator", p: policy.Policy{Rules: []policy.Rule{withCond("", "a")}}, want: `unknown operator ""`},
		{name: "in with a string", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpIn, "a")}}, want: "in needs a list"},
		{name: "in with a number", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpIn, 1)}}, want: "in needs a list"},
		{name: "in with nothing", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpIn, nil)}}, want: "in needs a list"},
		{name: "exists with a string", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpExists, "yes")}}, want: "exists needs a boolean"},
		{name: "exists with a number", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpExists, 1)}}, want: "exists needs a boolean"},
		{name: "exists with nothing", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpExists, nil)}}, want: "exists needs a boolean"},
		{name: "gt with a string", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpGt, "5")}}, want: "gt needs a number"},
		{name: "gte with a boolean", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpGte, true)}}, want: "gte needs a number"},
		{name: "lt with nothing", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpLt, nil)}}, want: "lt needs a number"},
		{name: "lte with a list", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpLte, []any{1})}}, want: "lte needs a number"},
		{name: "a json.Number that is not a number", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpGt, json.Number("abc"))}}, want: "gt needs a number"},
		{
			name: "the second condition of a rule",
			p: policy.Policy{Rules: []policy.Rule{{Name: "a", Effect: policy.Allow, When: policy.Match{Attrs: []policy.Cond{
				{Attr: "x", Op: policy.OpEq, Value: 1}, {Attr: "y", Op: policy.OpIn, Value: "no"},
			}}}}},
			want: `condition 1 on "y"`,
		},
	}
	for _, tt := range refused {
		t.Run("refuses "+tt.name, func(t *testing.T) {
			err := tt.p.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.True(t, strings.HasPrefix(err.Error(), "policy: "), err.Error())
		})
	}

	t.Run("reports the first thing wrong", func(t *testing.T) {
		p := policy.Policy{
			Default: "deny",
			Rules:   []policy.Rule{rule("", policy.Allow), rule("a", "deny")},
		}
		err := p.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default")
		assert.NotContains(t, err.Error(), "no name")

		p.Default = ""
		err = p.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rule 0 has no name")
		assert.NotContains(t, err.Error(), "deny")
	})

	accepted := []struct {
		name string
		p    policy.Policy
	}{
		{name: "the empty policy", p: policy.Policy{}},
		{name: "the design's policy", p: designPolicy()},
		{name: "every field and operator", p: everyPart()},
		{name: "a default of allow", p: policy.Policy{Default: policy.Allow}},
		{name: "a default of ask", p: policy.Policy{Default: policy.Ask}},
		{name: "a default of block", p: policy.Policy{Default: policy.Block}},
		{name: "names that differ in case", p: policy.Policy{Rules: []policy.Rule{rule("a", policy.Allow), rule("A", policy.Allow)}}},
		{name: "in with Go lists", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpIn, []string{"a"}), {Name: "d", Effect: policy.Allow, When: cond("x", policy.OpIn, [1]int{1})}}}},
		{name: "a numeric operator with each kind of number", p: policy.Policy{Rules: []policy.Rule{
			withCond(policy.OpGt, 1), {Name: "d", Effect: policy.Allow, When: cond("x", policy.OpGte, int64(1))},
			{Name: "e", Effect: policy.Allow, When: cond("x", policy.OpLt, 1.5)},
			{Name: "f", Effect: policy.Allow, When: cond("x", policy.OpLte, json.Number("2"))},
		}}},
		{name: "equality with any value", p: policy.Policy{Rules: []policy.Rule{withCond(policy.OpEq, "a"), {Name: "d", Effect: policy.Allow, When: cond("x", policy.OpNe, true)}}}},
		{name: "a pattern with every metacharacter", p: policy.Policy{Rules: []policy.Rule{{Name: "a", Effect: policy.Allow, When: policy.Match{Target: `a*b?[c-d][^e]\*`}}}}},
	}
	for _, tt := range accepted {
		t.Run("accepts "+tt.name, func(t *testing.T) {
			assert.NoError(t, tt.p.Validate())
		})
	}
}
