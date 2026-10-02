package policy_test

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/policy"
)

// holds reports whether m matches a, through the exported surface: a policy of
// one rule that allows what m matches, under the default that blocks the rest.
func holds(m policy.Match, a policy.Action) bool {
	d := policy.Policy{Rules: []policy.Rule{{Name: "r", Effect: policy.Allow, When: m}}}.Decide(a)
	return d.Index == 0
}

// cents is a named number type, as a service's own money type would be.
type cents int64

func attrs(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func cond(attr string, op policy.Op, value any) policy.Match {
	return policy.Match{Attrs: []policy.Cond{{Attr: attr, Op: op, Value: value}}}
}

func TestMatch_Kinds(t *testing.T) {
	tests := []struct {
		name string
		m    policy.Match
		a    policy.Action
		want bool
	}{
		{name: "the zero Match applies to every action", m: policy.Match{}, a: policy.Action{Kind: "read"}, want: true},
		{name: "the zero Match applies to an action with nothing set", m: policy.Match{}, a: policy.Action{}, want: true},
		{name: "a kind in the list", m: policy.Match{Kinds: []string{"read", "write"}}, a: policy.Action{Kind: "write"}, want: true},
		{name: "a kind not in the list", m: policy.Match{Kinds: []string{"read", "write"}}, a: policy.Action{Kind: "send"}, want: false},
		{name: "kinds are case-sensitive", m: policy.Match{Kinds: []string{"read"}}, a: policy.Action{Kind: "Read"}, want: false},
		{name: "an action with no kind matches no listed kind", m: policy.Match{Kinds: []string{"read"}}, a: policy.Action{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, holds(tt.m, tt.a))
		})
	}
}

func TestMatch_Target(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		target  string
		want    bool
	}{
		{name: "an exact target", pattern: "email:ap@example.com", target: "email:ap@example.com", want: true},
		{name: "another target", pattern: "email:ap@example.com", target: "email:hr@example.com", want: false},
		{name: "star within a segment", pattern: "email:*@example.com", target: "email:ap@example.com", want: true},
		{name: "star matches nothing at all", pattern: "email:*", target: "email:", want: true},
		{name: "star does not cross a slash", pattern: "file:/data/*", target: "file:/data/a/b", want: false},
		{name: "star matches one segment", pattern: "file:/data/*", target: "file:/data/a", want: true},
		{name: "question mark is one character", pattern: "doc-?", target: "doc-1", want: true},
		{name: "question mark is not two", pattern: "doc-?", target: "doc-12", want: false},
		{name: "a character class", pattern: "doc-[0-9]", target: "doc-7", want: true},
		{name: "a character class that does not hold", pattern: "doc-[0-9]", target: "doc-x", want: false},
		{name: "a negated class", pattern: "doc-[^0-9]", target: "doc-x", want: true},
		{name: "an escaped metacharacter", pattern: `a\*b`, target: "a*b", want: true},
		{name: "an escaped metacharacter is not a wildcard", pattern: `a\*b`, target: "axb", want: false},
		{name: "a pattern that matches nothing", pattern: "no-such-target-*", target: "email:ap@example.com", want: false},
		{name: "a literal pattern against no target", pattern: "email:x", target: "", want: false},
		{name: "star matches an action with no target", pattern: "*", target: "", want: true},
		{name: "a pattern that does not compile matches nothing", pattern: "[", target: "[", want: false},
		{name: "a pattern that does not compile matches nothing, though an earlier part mismatches", pattern: "x[", target: "y", want: false},
		{name: "an empty pattern is every target", pattern: "", target: "anything", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := policy.Match{Target: tt.pattern}
			assert.Equal(t, tt.want, holds(m, policy.Action{Kind: "read", Target: tt.target}))
		})
	}
}

func TestMatch_Conditions(t *testing.T) {
	tests := []struct {
		name  string
		m     policy.Match
		attrs map[string]any
		want  bool
	}{
		// eq
		{name: "eq: equal strings", m: cond("s", policy.OpEq, "done"), attrs: attrs("s", "done"), want: true},
		{name: "eq: different strings", m: cond("s", policy.OpEq, "done"), attrs: attrs("s", "open"), want: false},
		{name: "eq: equal booleans", m: cond("b", policy.OpEq, true), attrs: attrs("b", true), want: true},
		{name: "eq: different booleans", m: cond("b", policy.OpEq, true), attrs: attrs("b", false), want: false},
		{name: "eq: equal numbers", m: cond("n", policy.OpEq, 5), attrs: attrs("n", 5), want: true},
		{name: "eq: different numbers", m: cond("n", policy.OpEq, 5), attrs: attrs("n", 6), want: false},
		{name: "eq: a string is not the number it spells", m: cond("n", policy.OpEq, 5), attrs: attrs("n", "5"), want: false},
		{name: "eq: a number is not the string it spells", m: cond("n", policy.OpEq, "5"), attrs: attrs("n", 5), want: false},
		{name: "eq: a string is not the boolean it spells", m: cond("b", policy.OpEq, true), attrs: attrs("b", "true"), want: false},
		{name: "eq: a null attribute equals nothing", m: cond("n", policy.OpEq, nil), attrs: attrs("n", nil), want: false},
		{name: "eq: a list equals nothing", m: cond("l", policy.OpEq, []any{"a"}), attrs: attrs("l", []any{"a"}), want: false},
		{name: "eq: a named string type is the string", m: cond("e", policy.OpEq, "block"), attrs: attrs("e", policy.Block), want: true},
		{name: "eq: a named number type is the number", m: cond("n", policy.OpEq, 5), attrs: attrs("n", cents(5)), want: true},
		{name: "eq: a json.Number that is not a number is not a string", m: cond("n", policy.OpEq, "abc"), attrs: attrs("n", json.Number("abc")), want: false},

		// ne
		{name: "ne: different strings", m: cond("s", policy.OpNe, "done"), attrs: attrs("s", "open"), want: true},
		{name: "ne: equal strings", m: cond("s", policy.OpNe, "done"), attrs: attrs("s", "done"), want: false},
		{name: "ne: different booleans", m: cond("b", policy.OpNe, true), attrs: attrs("b", false), want: true},
		{name: "ne: equal numbers", m: cond("n", policy.OpNe, 5), attrs: attrs("n", 5.0), want: false},
		{name: "ne: different numbers", m: cond("n", policy.OpNe, 5), attrs: attrs("n", 6), want: true},
		{name: "ne: a present attribute of another type is not equal", m: cond("n", policy.OpNe, 5), attrs: attrs("n", "five"), want: true},

		// gt, gte, lt, lte
		{name: "gt: above", m: cond("n", policy.OpGt, 5), attrs: attrs("n", 6), want: true},
		{name: "gt: at", m: cond("n", policy.OpGt, 5), attrs: attrs("n", 5), want: false},
		{name: "gt: below", m: cond("n", policy.OpGt, 5), attrs: attrs("n", 4), want: false},
		{name: "gte: above", m: cond("n", policy.OpGte, 5), attrs: attrs("n", 6), want: true},
		{name: "gte: at", m: cond("n", policy.OpGte, 5), attrs: attrs("n", 5), want: true},
		{name: "gte: below", m: cond("n", policy.OpGte, 5), attrs: attrs("n", 4), want: false},
		{name: "lt: below", m: cond("n", policy.OpLt, 5), attrs: attrs("n", 4), want: true},
		{name: "lt: at", m: cond("n", policy.OpLt, 5), attrs: attrs("n", 5), want: false},
		{name: "lt: above", m: cond("n", policy.OpLt, 5), attrs: attrs("n", 6), want: false},
		{name: "lte: below", m: cond("n", policy.OpLte, 5), attrs: attrs("n", 4), want: true},
		{name: "lte: at", m: cond("n", policy.OpLte, 5), attrs: attrs("n", 5), want: true},
		{name: "lte: above", m: cond("n", policy.OpLte, 5), attrs: attrs("n", 6), want: false},
		{name: "gt: negative numbers", m: cond("n", policy.OpGt, -2), attrs: attrs("n", -1), want: true},
		{name: "gt: fractions", m: cond("n", policy.OpGt, 0.25), attrs: attrs("n", 0.5), want: true},
		{name: "gt: a string never holds, though it is above as text", m: cond("n", policy.OpGt, 5), attrs: attrs("n", "9"), want: false},
		{name: "gt: a boolean never holds", m: cond("n", policy.OpGt, 0), attrs: attrs("n", true), want: false},
		{name: "gt: a null never holds", m: cond("n", policy.OpGt, 0), attrs: attrs("n", nil), want: false},
		{name: "gt: a list never holds", m: cond("n", policy.OpGt, 0), attrs: attrs("n", []int{9}), want: false},
		{name: "gt: a value that is not a number never holds", m: cond("n", policy.OpGt, "5"), attrs: attrs("n", 9), want: false},
		{name: "lt: a value that is not a number never holds", m: cond("n", policy.OpLt, nil), attrs: attrs("n", 1), want: false},
		{name: "gt: an int64 above a float64 that cannot tell them apart", m: cond("n", policy.OpGt, float64(1<<53)), attrs: attrs("n", int64(1<<53+1)), want: true},
		{name: "eq: that int64 is not that float64", m: cond("n", policy.OpEq, float64(1<<53)), attrs: attrs("n", int64(1<<53+1)), want: false},
		{name: "lt: the largest int64 is below the next integer, held as unsigned", m: cond("n", policy.OpLt, uint64(math.MaxInt64)+1), attrs: attrs("n", int64(math.MaxInt64)), want: true},
		{name: "lt: a negative int is below an unsigned zero", m: cond("n", policy.OpLt, uint(0)), attrs: attrs("n", -1), want: true},
		{name: "gt: infinity is above any number", m: cond("n", policy.OpGt, 1e300), attrs: attrs("n", math.Inf(1)), want: true},
		{name: "lt: negative infinity is below any number", m: cond("n", policy.OpLt, -1e300), attrs: attrs("n", math.Inf(-1)), want: true},
		{name: "eq: NaN equals nothing", m: cond("n", policy.OpEq, 5), attrs: attrs("n", math.NaN()), want: false},
		{name: "gt: NaN is above nothing", m: cond("n", policy.OpGt, 5), attrs: attrs("n", math.NaN()), want: false},
		{name: "gte: NaN is above nothing", m: cond("n", policy.OpGte, 5), attrs: attrs("n", math.NaN()), want: false},
		{name: "lt: NaN is below nothing", m: cond("n", policy.OpLt, 5), attrs: attrs("n", math.NaN()), want: false},
		{name: "lte: NaN is below nothing", m: cond("n", policy.OpLte, 5), attrs: attrs("n", math.NaN()), want: false},

		// in
		{name: "in: a member", m: cond("s", policy.OpIn, []any{"a", "b"}), attrs: attrs("s", "b"), want: true},
		{name: "in: not a member", m: cond("s", policy.OpIn, []any{"a", "b"}), attrs: attrs("s", "c"), want: false},
		{name: "in: a string list", m: cond("s", policy.OpIn, []string{"a", "b"}), attrs: attrs("s", "a"), want: true},
		{name: "in: an array", m: cond("s", policy.OpIn, [2]string{"a", "b"}), attrs: attrs("s", "b"), want: true},
		{name: "in: numbers of other Go types", m: cond("n", policy.OpIn, []any{1.0, 2.0, 3.0}), attrs: attrs("n", 3), want: true},
		{name: "in: a json.Number against an int list", m: cond("n", policy.OpIn, []int{1, 2, 3}), attrs: attrs("n", json.Number("3")), want: true},
		{name: "in: a number that is not there", m: cond("n", policy.OpIn, []any{1.0, 2.0}), attrs: attrs("n", 2.5), want: false},
		{name: "in: an empty list holds for nothing", m: cond("s", policy.OpIn, []any{}), attrs: attrs("s", "a"), want: false},
		{name: "in: a list of mixed kinds", m: cond("b", policy.OpIn, []any{1, "a", true}), attrs: attrs("b", true), want: true},
		{name: "in: a string is not the number it spells", m: cond("n", policy.OpIn, []any{1, 2}), attrs: attrs("n", "1"), want: false},
		{name: "in: a value that is not a list never holds", m: cond("s", policy.OpIn, "a"), attrs: attrs("s", "a"), want: false},
		{name: "in: a nil value never holds", m: cond("s", policy.OpIn, nil), attrs: attrs("s", "a"), want: false},

		// exists
		{name: "exists true: present", m: cond("x", policy.OpExists, true), attrs: attrs("x", 1), want: true},
		{name: "exists true: absent", m: cond("x", policy.OpExists, true), attrs: attrs("y", 1), want: false},
		{name: "exists false: absent", m: cond("x", policy.OpExists, false), attrs: attrs("y", 1), want: true},
		{name: "exists false: present", m: cond("x", policy.OpExists, false), attrs: attrs("x", 1), want: false},
		{name: "exists true: a null attribute is present", m: cond("x", policy.OpExists, true), attrs: attrs("x", nil), want: true},
		{name: "exists false: a false attribute is present", m: cond("x", policy.OpExists, false), attrs: attrs("x", false), want: false},
		{name: "exists: a value that is not a boolean never holds when absent", m: cond("x", policy.OpExists, "no"), attrs: attrs("y", 1), want: false},
		{name: "exists: a value that is not a boolean never holds when present", m: cond("x", policy.OpExists, "yes"), attrs: attrs("x", 1), want: false},
		{name: "exists: a nil value never holds", m: cond("x", policy.OpExists, nil), attrs: attrs("y", 1), want: false},

		// the operator
		{name: "an operator that is not listed never holds", m: cond("x", "contains", "a"), attrs: attrs("x", "a"), want: false},
		{name: "no operator never holds", m: cond("x", "", "a"), attrs: attrs("x", "a"), want: false},

		// every condition
		{
			name: "two conditions that both hold",
			m: policy.Match{Attrs: []policy.Cond{
				{Attr: "a", Op: policy.OpEq, Value: "x"}, {Attr: "n", Op: policy.OpGt, Value: 1},
			}},
			attrs: attrs("a", "x", "n", 2), want: true,
		},
		{
			name: "two conditions, the second failing",
			m: policy.Match{Attrs: []policy.Cond{
				{Attr: "a", Op: policy.OpEq, Value: "x"}, {Attr: "n", Op: policy.OpGt, Value: 1},
			}},
			attrs: attrs("a", "x", "n", 1), want: false,
		},
		{
			name: "two conditions, the first failing",
			m: policy.Match{Attrs: []policy.Cond{
				{Attr: "a", Op: policy.OpEq, Value: "x"}, {Attr: "n", Op: policy.OpGt, Value: 1},
			}},
			attrs: attrs("a", "y", "n", 2), want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, holds(tt.m, policy.Action{Kind: "k", Attrs: tt.attrs}))
		})
	}
}

// An attribute the action does not carry never holds, except exists false.
func TestMatch_AbsentAttribute(t *testing.T) {
	tests := []struct {
		op    policy.Op
		value any
		want  bool
	}{
		{op: policy.OpEq, value: "a", want: false},
		{op: policy.OpEq, value: nil, want: false},
		{op: policy.OpNe, value: "a", want: false},
		{op: policy.OpGt, value: 0, want: false},
		{op: policy.OpGte, value: 0, want: false},
		{op: policy.OpLt, value: 1, want: false},
		{op: policy.OpLte, value: 1, want: false},
		{op: policy.OpIn, value: []any{"a"}, want: false},
		{op: policy.OpExists, value: true, want: false},
		{op: policy.OpExists, value: false, want: true},
	}
	carriers := map[string]map[string]any{
		"an action with no attributes":                      nil,
		"an action with other attributes":                   attrs("other", "a"),
		"an action with an attribute named almost the same": attrs("x ", "a"),
	}
	for _, tt := range tests {
		for carrier, as := range carriers {
			t.Run(string(tt.op)+" "+strconv.FormatBool(tt.want)+" on "+carrier, func(t *testing.T) {
				assert.Equal(t, tt.want, holds(cond("x", tt.op, tt.value), policy.Action{Kind: "k", Attrs: as}))
			})
		}
	}
}

// numberForms is n as every Go number type a caller is likely to hold, so that
// a comparison can be run between each pair. n must fit an int8.
func numberForms(n int) []any {
	return []any{
		n, int8(n), int16(n), int32(n), int64(n),
		uint(n), uint8(n), uint16(n), uint32(n), uint64(n),
		float32(n), float64(n),
		json.Number(strconv.Itoa(n)), json.Number(strconv.Itoa(n) + ".0"),
		cents(n),
	}
}

func TestMatch_NumbersCompareAsNumbersWhateverTheirType(t *testing.T) {
	tests := []struct {
		name        string
		op          policy.Op
		attr, value int
		want        bool
	}{
		{name: "eq equal", op: policy.OpEq, attr: 5, value: 5, want: true},
		{name: "eq unequal", op: policy.OpEq, attr: 4, value: 5, want: false},
		{name: "ne equal", op: policy.OpNe, attr: 5, value: 5, want: false},
		{name: "ne unequal", op: policy.OpNe, attr: 4, value: 5, want: true},
		{name: "gt above", op: policy.OpGt, attr: 6, value: 5, want: true},
		{name: "gt at", op: policy.OpGt, attr: 5, value: 5, want: false},
		{name: "gt below", op: policy.OpGt, attr: 4, value: 5, want: false},
		{name: "gte above", op: policy.OpGte, attr: 6, value: 5, want: true},
		{name: "gte at", op: policy.OpGte, attr: 5, value: 5, want: true},
		{name: "gte below", op: policy.OpGte, attr: 4, value: 5, want: false},
		{name: "lt below", op: policy.OpLt, attr: 4, value: 5, want: true},
		{name: "lt at", op: policy.OpLt, attr: 5, value: 5, want: false},
		{name: "lt above", op: policy.OpLt, attr: 6, value: 5, want: false},
		{name: "lte below", op: policy.OpLte, attr: 4, value: 5, want: true},
		{name: "lte at", op: policy.OpLte, attr: 5, value: 5, want: true},
		{name: "lte above", op: policy.OpLte, attr: 6, value: 5, want: false},
		{name: "in present", op: policy.OpIn, attr: 5, value: 5, want: true},
		{name: "in absent", op: policy.OpIn, attr: 4, value: 5, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, got := range numberForms(tt.attr) {
				for _, want := range numberForms(tt.value) {
					v := want
					if tt.op == policy.OpIn {
						v = []any{want}
					}
					assert.Equalf(t, tt.want, holds(cond("n", tt.op, v), policy.Action{Kind: "k", Attrs: attrs("n", got)}),
						"attribute %T(%v) %s value %T(%v)", got, got, tt.op, want, want)
				}
			}
		})
	}
}

func TestMatch_PartsMustAllHold(t *testing.T) {
	m := policy.Match{
		Kinds:  []string{"send"},
		Target: "email:*@example.com",
		Attrs:  []policy.Cond{{Attr: "external", Op: policy.OpEq, Value: true}},
	}
	tests := []struct {
		name string
		a    policy.Action
		want bool
	}{
		{name: "all three hold", a: policy.Action{Kind: "send", Target: "email:ap@example.com", Attrs: attrs("external", true)}, want: true},
		{name: "the kind fails", a: policy.Action{Kind: "read", Target: "email:ap@example.com", Attrs: attrs("external", true)}, want: false},
		{name: "the target fails", a: policy.Action{Kind: "send", Target: "email:ap@example.org", Attrs: attrs("external", true)}, want: false},
		{name: "the attribute fails", a: policy.Action{Kind: "send", Target: "email:ap@example.com", Attrs: attrs("external", false)}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, holds(m, tt.a))
		})
	}
}

func TestDecide_NoRuleMatched(t *testing.T) {
	none := policy.Match{Kinds: []string{"never"}}
	tests := []struct {
		name string
		p    policy.Policy
		want policy.Effect
	}{
		{name: "an empty policy blocks", p: policy.Policy{}, want: policy.Block},
		{name: "rules that match nothing block", p: policy.Policy{Rules: []policy.Rule{{Name: "n", Effect: policy.Allow, When: none}}}, want: policy.Block},
		{name: "an empty default is block", p: policy.Policy{Default: "", Rules: []policy.Rule{{Name: "n", Effect: policy.Allow, When: none}}}, want: policy.Block},
		{name: "a default of allow", p: policy.Policy{Default: policy.Allow, Rules: []policy.Rule{{Name: "n", Effect: policy.Block, When: none}}}, want: policy.Allow},
		{name: "a default of ask", p: policy.Policy{Default: policy.Ask}, want: policy.Ask},
		{name: "a default of block", p: policy.Policy{Default: policy.Block}, want: policy.Block},
		{name: "a default that is none of the three blocks", p: policy.Policy{Default: "bogus"}, want: policy.Block},
		{name: "the reference's word is not a Go default", p: policy.Policy{Default: "approve"}, want: policy.Block},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.Decide(policy.Action{Kind: "read"})
			assert.Equal(t, policy.Decision{Effect: tt.want, Rule: policy.RuleDefault, Index: -1}, got)
			assert.Empty(t, got.Matched)
		})
	}
}

func TestDecide_TheDefaultOnlyAnswersWhenNothingMatched(t *testing.T) {
	p := policy.Policy{
		Default: policy.Allow,
		Rules:   []policy.Rule{{Name: "asks", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}}}},
	}
	assert.Equal(t, policy.Decision{Effect: policy.Ask, Rule: "asks", Index: 0, Matched: []string{"asks"}},
		p.Decide(policy.Action{Kind: "send"}))
	assert.Equal(t, policy.Decision{Effect: policy.Allow, Rule: policy.RuleDefault, Index: -1},
		p.Decide(policy.Action{Kind: "read"}))
}

func TestDecide_Ties(t *testing.T) {
	all := policy.Match{}
	tests := []struct {
		name      string
		rules     []policy.Rule
		wantRule  string
		wantIndex int
		want      policy.Effect
	}{
		{
			name:     "two allows report the earlier",
			rules:    []policy.Rule{{Name: "first", Effect: policy.Allow, When: all}, {Name: "second", Effect: policy.Allow, When: all}},
			wantRule: "first", wantIndex: 0, want: policy.Allow,
		},
		{
			name:     "two asks report the earlier",
			rules:    []policy.Rule{{Name: "first", Effect: policy.Ask, When: all}, {Name: "second", Effect: policy.Ask, When: all}},
			wantRule: "first", wantIndex: 0, want: policy.Ask,
		},
		{
			name:     "two blocks report the earlier",
			rules:    []policy.Rule{{Name: "first", Effect: policy.Block, When: all}, {Name: "second", Effect: policy.Block, When: all}},
			wantRule: "first", wantIndex: 0, want: policy.Block,
		},
		{
			name:     "a tie later in the list is still the earlier of the two",
			rules:    []policy.Rule{{Name: "loose", Effect: policy.Allow, When: all}, {Name: "first ask", Effect: policy.Ask, When: all}, {Name: "second ask", Effect: policy.Ask, When: all}},
			wantRule: "first ask", wantIndex: 1, want: policy.Ask,
		},
		{
			name:     "a later stricter rule wins",
			rules:    []policy.Rule{{Name: "loose", Effect: policy.Allow, When: all}, {Name: "strict", Effect: policy.Block, When: all}},
			wantRule: "strict", wantIndex: 1, want: policy.Block,
		},
		{
			name:     "a later looser rule does not",
			rules:    []policy.Rule{{Name: "strict", Effect: policy.Block, When: all}, {Name: "loose", Effect: policy.Allow, When: all}},
			wantRule: "strict", wantIndex: 0, want: policy.Block,
		},
		{
			name:     "a rule that does not match is not a tie",
			rules:    []policy.Rule{{Name: "elsewhere", Effect: policy.Block, When: policy.Match{Kinds: []string{"never"}}}, {Name: "here", Effect: policy.Block, When: all}},
			wantRule: "here", wantIndex: 1, want: policy.Block,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.Policy{Rules: tt.rules}.Decide(policy.Action{Kind: "read"})
			assert.Equal(t, tt.want, got.Effect)
			assert.Equal(t, tt.wantRule, got.Rule)
			assert.Equal(t, tt.wantIndex, got.Index)
		})
	}
}

// permute calls fn with every ordering of rules. fn must not keep its argument.
func permute(rules []policy.Rule, fn func([]policy.Rule)) {
	var rec func(k int)
	rec = func(k int) {
		if k == len(rules) {
			fn(rules)
			return
		}
		for i := k; i < len(rules); i++ {
			rules[k], rules[i] = rules[i], rules[k]
			rec(k + 1)
			rules[k], rules[i] = rules[i], rules[k]
		}
	}
	rec(0)
}

// The strictest matching effect wins whatever the order of the rules, and the
// rule reported is the first of that effect in the order given.
func TestDecide_StrictestWinsWhateverTheOrder(t *testing.T) {
	all := policy.Match{}
	elsewhere := policy.Match{Kinds: []string{"never"}}
	tests := []struct {
		name  string
		rules []policy.Rule
		want  policy.Effect
	}{
		{
			name: "blocks, asks and allows",
			rules: []policy.Rule{
				{Name: "allow a", Effect: policy.Allow, When: all},
				{Name: "ask a", Effect: policy.Ask, When: all},
				{Name: "ask b", Effect: policy.Ask, When: all},
				{Name: "block a", Effect: policy.Block, When: all},
				{Name: "block b", Effect: policy.Block, When: all},
				{Name: "block elsewhere", Effect: policy.Block, When: elsewhere},
			},
			want: policy.Block,
		},
		{
			name: "asks and allows, with a block that does not match",
			rules: []policy.Rule{
				{Name: "allow a", Effect: policy.Allow, When: all},
				{Name: "allow b", Effect: policy.Allow, When: all},
				{Name: "ask a", Effect: policy.Ask, When: all},
				{Name: "ask b", Effect: policy.Ask, When: all},
				{Name: "block elsewhere", Effect: policy.Block, When: elsewhere},
			},
			want: policy.Ask,
		},
		{
			name: "allows only, with a block that does not match",
			rules: []policy.Rule{
				{Name: "allow a", Effect: policy.Allow, When: all},
				{Name: "allow b", Effect: policy.Allow, When: all},
				{Name: "allow c", Effect: policy.Allow, When: all},
				{Name: "block elsewhere", Effect: policy.Block, When: elsewhere},
			},
			want: policy.Allow,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orderings := 0
			permute(slices.Clone(tt.rules), func(order []policy.Rule) {
				orderings++
				var matched []string
				var effects []policy.Effect
				wantIndex := -1
				for i, r := range order {
					if r.When.Kinds != nil { // the fixture's rules that cannot match
						continue
					}
					matched = append(matched, r.Name)
					effects = append(effects, r.Effect)
					if wantIndex < 0 && r.Effect == tt.want {
						wantIndex = i
					}
				}
				require.Equal(t, tt.want, policy.Strictest(effects...), "the fixture's own strictest effect")

				got := policy.Policy{Rules: order}.Decide(policy.Action{Kind: "read"})
				if !assert.Equal(t, tt.want, got.Effect) ||
					!assert.Equal(t, wantIndex, got.Index) ||
					!assert.Equal(t, order[wantIndex].Name, got.Rule) ||
					!assert.Equal(t, matched, got.Matched) {
					t.Fatalf("for the order %v", ruleNames(order))
				}
			})
			assert.Greater(t, orderings, 1)
		})
	}
}

func ruleNames(rules []policy.Rule) []string {
	names := make([]string, len(rules))
	for i, r := range rules {
		names[i] = r.Name
	}
	return names
}

func TestDecide_MatchedListsEveryMatchingRuleInOrder(t *testing.T) {
	p := policy.Policy{Rules: []policy.Rule{
		{Name: "reads", Effect: policy.Allow, When: policy.Match{Kinds: []string{"read"}}},
		{Name: "sends", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}}},
		{Name: "anything", Effect: policy.Allow},
		{Name: "outside", Effect: policy.Block, When: cond("external", policy.OpEq, true)},
		{Name: "also anything", Effect: policy.Ask},
	}}
	tests := []struct {
		name        string
		action      policy.Action
		wantMatched []string
		wantRule    string
	}{
		{name: "a read", action: policy.Action{Kind: "read"}, wantMatched: []string{"reads", "anything", "also anything"}, wantRule: "also anything"},
		{name: "an outside send", action: policy.Action{Kind: "send", Attrs: attrs("external", true)}, wantMatched: []string{"sends", "anything", "outside", "also anything"}, wantRule: "outside"},
		{name: "a delete", action: policy.Action{Kind: "delete"}, wantMatched: []string{"anything", "also anything"}, wantRule: "also anything"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Decide(tt.action)
			assert.Equal(t, tt.wantMatched, got.Matched)
			assert.Equal(t, tt.wantRule, got.Rule)
		})
	}
}

// A policy that did not come through Validate: a rule whose effect is none of
// the three is block.
func TestDecide_AnUnknownEffectIsBlock(t *testing.T) {
	tests := []struct {
		name      string
		rules     []policy.Rule
		wantRule  string
		wantIndex int
	}{
		{name: "an unknown effect alone", rules: []policy.Rule{{Name: "odd", Effect: "bogus"}}, wantRule: "odd", wantIndex: 0},
		{name: "the zero effect", rules: []policy.Rule{{Name: "odd", Effect: ""}}, wantRule: "odd", wantIndex: 0},
		{name: "the reference's word as a Go value", rules: []policy.Rule{{Name: "odd", Effect: "approve"}}, wantRule: "odd", wantIndex: 0},
		{name: "it ties a later block and, being earlier, is the one reported", rules: []policy.Rule{{Name: "odd", Effect: "bogus"}, {Name: "block", Effect: policy.Block}}, wantRule: "odd", wantIndex: 0},
		{name: "it beats an earlier ask", rules: []policy.Rule{{Name: "ask", Effect: policy.Ask}, {Name: "odd", Effect: "bogus"}}, wantRule: "odd", wantIndex: 1},
		{name: "it is not reported when an earlier block ties it", rules: []policy.Rule{{Name: "block", Effect: policy.Block}, {Name: "odd", Effect: "bogus"}}, wantRule: "block", wantIndex: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := policy.Policy{Default: policy.Allow, Rules: tt.rules}.Decide(policy.Action{Kind: "read"})
			assert.Equal(t, policy.Block, got.Effect)
			assert.Equal(t, tt.wantRule, got.Rule)
			assert.Equal(t, tt.wantIndex, got.Index)
		})
	}
}

func TestDecide_APatternThatDoesNotCompileDoesNotMatch(t *testing.T) {
	p := policy.Policy{Default: policy.Allow, Rules: []policy.Rule{
		{Name: "broken", Effect: policy.Block, When: policy.Match{Target: "email:["}},
	}}
	got := p.Decide(policy.Action{Kind: "send", Target: "email:["})
	assert.Equal(t, policy.Decision{Effect: policy.Allow, Rule: policy.RuleDefault, Index: -1}, got)
}

func TestDecide_IsPure(t *testing.T) {
	p := policy.Policy{
		Version: "v1",
		Default: policy.Ask,
		Rules: []policy.Rule{
			{Name: "reads", Effect: policy.Allow, When: policy.Match{Kinds: []string{"read"}}},
			{Name: "big payments", Effect: policy.Ask, When: policy.Match{Kinds: []string{"pay"}, Attrs: []policy.Cond{{Attr: "amount", Op: policy.OpGt, Value: 200.0}}}},
			{Name: "deletes", Effect: policy.Block, When: policy.Match{Kinds: []string{"delete"}}},
		},
	}
	a := policy.Action{Kind: "pay", Target: "invoice:7", Attrs: attrs("amount", 300.0, "tags", []any{"a", "b"})}
	want := p.Decide(a)
	require.Equal(t, policy.Decision{Effect: policy.Ask, Rule: "big payments", Index: 1, Matched: []string{"big payments"}}, want)

	policyBefore, err := json.Marshal(p)
	require.NoError(t, err)
	actionBefore, err := json.Marshal(a)
	require.NoError(t, err)

	t.Run("the same input gives the same decision every time", func(t *testing.T) {
		for range 100 {
			assert.Equal(t, want, p.Decide(a))
		}
	})

	t.Run("it changes neither the policy nor the action", func(t *testing.T) {
		policyAfter, err := json.Marshal(p)
		require.NoError(t, err)
		actionAfter, err := json.Marshal(a)
		require.NoError(t, err)
		assert.JSONEq(t, string(policyBefore), string(policyAfter))
		assert.JSONEq(t, string(actionBefore), string(actionAfter))
	})

	t.Run("a decision shares nothing with the next one", func(t *testing.T) {
		first := p.Decide(a)
		first.Matched[0] = "changed"
		assert.Equal(t, want, p.Decide(a))
	})

	t.Run("many goroutines reach the same decision", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make([]policy.Decision, 32)
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 50 {
					results[i] = p.Decide(a)
				}
			}()
		}
		wg.Wait()
		for _, got := range results {
			assert.Equal(t, want, got)
		}
	})
}

func TestGroup(t *testing.T) {
	p := policy.Policy{Rules: []policy.Rule{
		{Name: "reads", Effect: policy.Allow, When: policy.Match{Kinds: []string{"read"}}},
		{Name: "sends", Effect: policy.Ask, When: policy.Match{Kinds: []string{"send"}}},
		{Name: "deletes", Effect: policy.Block, When: policy.Match{Kinds: []string{"delete"}}},
	}}
	actions := []policy.Action{
		{Kind: "read", Target: "1"},
		{Kind: "delete", Target: "2"},
		{Kind: "send", Target: "3"},
		{Kind: "read", Target: "4"},
		{Kind: "unknown", Target: "5"},
		{Kind: "send", Target: "6"},
	}

	got := p.Group(actions)

	t.Run("sorts by effect and keeps input order within a group", func(t *testing.T) {
		assert.Equal(t, []string{"1", "4"}, ids(got.Allow))
		assert.Equal(t, []string{"3", "6"}, ids(got.Ask))
		assert.Equal(t, []string{"2", "5"}, ids(got.Block))
	})

	t.Run("an action nothing matched is in the group of the default", func(t *testing.T) {
		require.Len(t, got.Block, 2)
		assert.Equal(t, policy.Judged{
			Action:   policy.Action{Kind: "unknown", Target: "5"},
			Decision: policy.Decision{Effect: policy.Block, Rule: policy.RuleDefault, Index: -1},
		}, got.Block[1])
	})

	t.Run("each action is with the decision Decide gives it", func(t *testing.T) {
		for _, group := range [][]policy.Judged{got.Allow, got.Ask, got.Block} {
			for _, j := range group {
				assert.Equal(t, p.Decide(j.Action), j.Decision)
			}
		}
	})

	t.Run("every action is in exactly one group", func(t *testing.T) {
		assert.Len(t, got.Allow, 2)
		assert.Len(t, got.Ask, 2)
		assert.Len(t, got.Block, 2)
	})

	t.Run("no actions give empty lists, which are written as lists", func(t *testing.T) {
		for _, in := range [][]policy.Action{nil, {}} {
			out, err := json.Marshal(p.Group(in))
			require.NoError(t, err)
			assert.JSONEq(t, `{"allow":[],"ask":[],"block":[]}`, string(out))
		}
	})

	t.Run("a rule with an unknown effect puts its actions in block", func(t *testing.T) {
		odd := policy.Policy{Rules: []policy.Rule{{Name: "odd", Effect: "bogus"}}}
		g := odd.Group([]policy.Action{{Kind: "read", Target: "1"}})
		assert.Equal(t, []string{"1"}, ids(g.Block))
		assert.Empty(t, g.Allow)
		assert.Empty(t, g.Ask)
	})

	t.Run("it is written as the design gives it", func(t *testing.T) {
		out, err := json.Marshal(p.Group([]policy.Action{{Kind: "read", Target: "1"}}))
		require.NoError(t, err)
		assert.JSONEq(t, `{
			"allow": [{"action": {"kind": "read", "target": "1"},
			           "decision": {"decision": "allow", "rule": "reads", "index": 0, "matched": ["reads"]}}],
			"ask": [],
			"block": []
		}`, string(out))
	})
}
