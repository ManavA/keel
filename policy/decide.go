package policy

import (
	"encoding/json"
	"math"
	"math/big"
	"path"
	"reflect"
	"slices"
	"strconv"
)

// Decide gives the decision for a. It is pure: it reads nothing but p and a,
// and records nothing.
//
// The decision is the first match of the greatest strictness: a later match
// replaces the one held only when it is strictly stricter, so of two equally
// strict rules the earlier is reported. Called on a Policy that did not come
// through Validate, a rule with an effect that is none of the three counts as
// Block, and a target pattern that does not compile, or a condition with an
// operator or a value its operator cannot use, matches nothing. The other forms
// Validate refuses, such as an empty list of values, are decided as written.
func (p Policy) Decide(a Action) Decision {
	d := Decision{Index: -1}
	for i, r := range p.Rules {
		if !r.When.holds(a) {
			continue
		}
		d.Matched = append(d.Matched, r.Name)
		if e := r.Effect.effective(); d.Index < 0 || e.rank() > d.Effect.rank() {
			d.Effect, d.Rule, d.Index = e, r.Name, i
		}
	}
	if d.Index < 0 {
		d.Effect, d.Rule = p.Default.effective(), RuleDefault
	}
	return d
}

// Group decides every action and sorts them by effect.
func (p Policy) Group(actions []Action) Grouped {
	g := Grouped{Allow: []Judged{}, Ask: []Judged{}, Block: []Judged{}}
	for _, a := range actions {
		j := Judged{Action: a, Decision: p.Decide(a)}
		switch j.Decision.Effect {
		case Allow:
			g.Allow = append(g.Allow, j)
		case Ask:
			g.Ask = append(g.Ask, j)
		default:
			g.Block = append(g.Block, j)
		}
	}
	return g
}

// holds reports whether every part of m that is set holds for a.
func (m Match) holds(a Action) bool {
	if len(m.Kinds) > 0 && !slices.Contains(m.Kinds, a.Kind) {
		return false
	}
	if m.Target != "" {
		if ok, err := path.Match(m.Target, a.Target); err != nil || !ok {
			return false
		}
	}
	for _, c := range m.Attrs {
		if !c.holds(a) {
			return false
		}
	}
	return true
}

// holds reports whether c holds for a. A condition on an attribute the action
// does not carry never holds, except that exists is false. A condition whose
// operator is not listed, or whose value the operator cannot compare with,
// never holds, ne no less than eq.
func (c Cond) holds(a Action) bool {
	v, present := a.Attrs[c.Attr]
	if c.Op == OpExists {
		want, ok := boolOf(c.Value)
		return ok && present == want
	}
	if !present {
		return false
	}
	switch c.Op {
	case OpEq, OpNe:
		if ok, _ := scalar(c.Value); !ok {
			return false
		}
		return equal(v, c.Value) == (c.Op == OpEq)
	case OpIn:
		return in(v, c.Value)
	case OpGt, OpGte, OpLt, OpLte:
		return ordered(c.Op, v, c.Value)
	default:
		return false
	}
}

// equal reports whether x and y are the same number, the same string or the
// same boolean. Values of any other kind, nil and lists among them, are equal
// to nothing.
func equal(x, y any) bool {
	if nx, ok := numberOf(x); ok {
		ny, ok := numberOf(y)
		return ok && nx.Cmp(ny) == 0
	}
	if sx, ok := stringOf(x); ok {
		sy, ok := stringOf(y)
		return ok && sx == sy
	}
	if bx, ok := boolOf(x); ok {
		by, ok := boolOf(y)
		return ok && bx == by
	}
	return false
}

// ordered compares x with y under one of the four ordering operators. It
// holds only between two numbers.
func ordered(op Op, x, y any) bool {
	nx, ok := numberOf(x)
	if !ok {
		return false
	}
	ny, ok := numberOf(y)
	if !ok {
		return false
	}
	switch c := nx.Cmp(ny); op {
	case OpGt:
		return c > 0
	case OpGte:
		return c >= 0
	case OpLt:
		return c < 0
	default:
		return c <= 0
	}
}

// in reports whether v equals an element of list.
func in(v, list any) bool {
	rl := reflect.ValueOf(list)
	if rl.Kind() != reflect.Slice && rl.Kind() != reflect.Array {
		return false
	}
	for i := range rl.Len() {
		if equal(v, rl.Index(i).Interface()) {
			return true
		}
	}
	return false
}

// isList reports whether v is a slice or an array.
func isList(v any) bool {
	k := reflect.ValueOf(v).Kind()
	return k == reflect.Slice || k == reflect.Array
}

// numberOf reads v as a number, whatever Go type holds it: any integer or
// float type, a named type over one, or a json.Number. The result is exact, so
// an int64 and a float64 that differ in the last place compare as different. A
// NaN is not a number to compare, and nor is a json.Number that is not finite.
func numberOf(v any) (*big.Float, bool) {
	if n, ok := v.(json.Number); ok {
		if i, err := strconv.ParseInt(string(n), 10, 64); err == nil {
			return new(big.Float).SetInt64(i), true
		}
		f, err := strconv.ParseFloat(string(n), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return new(big.Float).SetFloat64(f), true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return new(big.Float).SetInt64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return new(big.Float).SetUint64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		if math.IsNaN(f) {
			return nil, false
		}
		return new(big.Float).SetFloat64(f), true
	default:
		return nil, false
	}
}

// stringOf reads v as a string, or a named type over one. A json.Number is
// not a string, whether or not it holds a number.
func stringOf(v any) (string, bool) {
	if _, ok := v.(json.Number); ok {
		return "", false
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.String {
		return "", false
	}
	return rv.String(), true
}

// boolOf reads v as a boolean, or a named type over one.
func boolOf(v any) (bool, bool) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Bool {
		return false, false
	}
	return rv.Bool(), true
}
