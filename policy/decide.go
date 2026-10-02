package policy

import (
	"cmp"
	"encoding/json"
	"math"
	"math/big"
	"path"
	"reflect"
	"slices"
	"strconv"
)

// truth is what a condition, or a whole match, comes to for one action. There
// are three values, because an attribute can arrive as something an operator
// cannot compare, and calling that "does not hold" lets a rule be walked round
// by writing the amount as a string.
type truth uint8

const (
	unmet   truth = iota // does not hold
	met                  // holds
	unclear              // cannot be told
)

func fromBool(b bool) truth {
	if b {
		return met
	}
	return unmet
}

// Decide gives the decision for a. It is pure: it reads nothing but p and a,
// and records nothing.
//
// The decision is the first match of the greatest strictness: a later match
// replaces the one held only when it is strictly stricter, so of two equally
// strict rules the earlier is reported.
//
// A rule matches for certain when every condition of its match holds, and does
// not match when any does not hold. When none fails but one cannot be told, an
// ask or block rule matches and an allow rule does not, and Decision.Uncertain
// says what could not be told. A condition cannot be told when the attribute is
// present but of a type its operator cannot compare. The same goes for a rule
// that did not come through Validate and cannot be evaluated: a target pattern
// that does not compile, or a condition with an operator or a value its
// operator cannot use. A rule with an effect that is none of the three counts
// as Block.
func (p Policy) Decide(a Action) Decision {
	d := Decision{Index: -1}
	for i, r := range p.Rules {
		t, why := r.When.eval(a)
		e := r.Effect.effective()
		if t == unmet || (t == unclear && e == Allow) {
			continue
		}
		d.Matched = append(d.Matched, r.Name)
		if d.Index < 0 || e.rank() > d.Effect.rank() {
			d.Effect, d.Rule, d.Index, d.Uncertain = e, r.Name, i, why
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

// eval reports what m comes to for a: met when every part holds, unmet when
// any does not, and otherwise unclear, with the names of what could not be
// told. A part that does not hold settles it whatever else cannot be told.
func (m Match) eval(a Action) (truth, []string) {
	var why []string
	note := func(what string) {
		if !slices.Contains(why, what) {
			why = append(why, what)
		}
	}
	if len(m.Kinds) > 0 && !slices.Contains(m.Kinds, a.Kind) {
		// An empty entry names no kind, so the list is not one that can be read.
		if !slices.Contains(m.Kinds, "") {
			return unmet, nil
		}
		note("kinds list")
	}
	if m.Target != "" {
		switch ok, err := path.Match(m.Target, a.Target); {
		case err != nil:
			note("target pattern")
		case !ok:
			return unmet, nil
		}
	}
	for i, c := range m.Attrs {
		switch c.eval(a) {
		case unmet:
			return unmet, nil
		case unclear:
			name := c.Attr
			if name == "" {
				name = "condition " + strconv.Itoa(i)
			}
			note(name)
		}
	}
	if len(why) > 0 {
		return unclear, why
	}
	return met, nil
}

// eval reports what c comes to for a. A malformed condition cannot be told,
// whether or not the action carries the attribute. Otherwise an attribute the
// action does not carry does not hold, for every operator but exists with the
// value false; and one it carries is compared, or cannot be told.
func (c Cond) eval(a Action) truth {
	if c.Attr == "" || c.check() != nil {
		return unclear
	}
	v, present := a.Attrs[c.Attr]
	if c.Op == OpExists {
		want, _ := boolOf(c.Value)
		return fromBool(present == want)
	}
	if !present {
		return unmet
	}
	switch c.Op {
	case OpEq, OpNe:
		same, ok := compare(v, c.Value)
		if !ok {
			return unclear
		}
		return fromBool(same == (c.Op == OpEq))
	case OpIn:
		return in(v, c.Value)
	default: // gt, gte, lt, lte: check has refused every other operator
		return ordered(c.Op, v, c.Value)
	}
}

// compare reports whether x equals y, and whether the two can be compared at
// all: two numbers, two strings or two booleans. A value of any other type, or
// of another type than its partner, cannot be.
func compare(x, y any) (equal, comparable bool) {
	if nx, ok := numberOf(x); ok {
		ny, ok := numberOf(y)
		return ok && nx.cmp(ny) == 0, ok
	}
	if sx, ok := stringOf(x); ok {
		sy, ok := stringOf(y)
		return ok && sx == sy, ok
	}
	if bx, ok := boolOf(x); ok {
		by, ok := boolOf(y)
		return ok && bx == by, ok
	}
	return false, false
}

// ordered compares x with y under one of the four ordering operators. It can
// be told only between two numbers.
func ordered(op Op, x, y any) truth {
	nx, ok := numberOf(x)
	if !ok {
		return unclear
	}
	ny, ok := numberOf(y)
	if !ok {
		return unclear
	}
	switch c := nx.cmp(ny); op {
	case OpGt:
		return fromBool(c > 0)
	case OpGte:
		return fromBool(c >= 0)
	case OpLt:
		return fromBool(c < 0)
	default:
		return fromBool(c <= 0)
	}
}

// in is eq on each element of list, joined by "or": it holds if any element
// equals v, and does not hold only if every element could be compared with v
// and none equalled it. An element that could not be compared leaves the rest
// unable to say, since it might have been the one: a list of 22 and "ssh" does
// not rule out the string "22".
func in(v, list any) truth {
	rl := reflect.ValueOf(list)
	allCompared := true
	for i := range rl.Len() {
		equal, ok := compare(v, rl.Index(i).Interface())
		switch {
		case !ok:
			allCompared = false
		case equal:
			return met
		}
	}
	if allCompared {
		return unmet
	}
	return unclear
}

// isList reports whether v is a slice or an array.
func isList(v any) bool {
	k := reflect.ValueOf(v).Kind()
	return k == reflect.Slice || k == reflect.Array
}

// number is an exact number, or an infinity.
type number struct {
	inf int      // 1 or -1 for an infinity, else 0
	r   *big.Rat // the value, when finite
}

// cmp orders n and o: -1, 0 or 1.
func (n number) cmp(o number) int {
	if n.inf != 0 || o.inf != 0 {
		return cmp.Compare(n.inf, o.inf)
	}
	return n.r.Cmp(o.r)
}

// What is compared of a number written as text, so that a hostile one cannot
// cost memory or time out of proportion: its length in bytes and the size of
// its exponent. A number past either cannot be told.
const (
	maxNumberText     = 4096
	maxNumberExponent = 4096
)

// numberOf reads v as a number, whatever Go type holds it: any integer or float
// type, a named type over one, or a json.Number. It is exact. An integer is the
// integer it is. A float with an integer value is that integer, so a float64
// 2^70 is 1180591620717411303424 and is above a threshold of
// 1180591620717411303000. Any other float is the shortest decimal that gives it
// back, so a float64 0.1 is the number 0.1 and a threshold written 0.1 meets
// it. Text is the decimal it spells, of any size or precision up to the bounds
// above, and must be a JSON number. A NaN is not a number, nor is text that is
// not one.
func numberOf(v any) (number, bool) {
	if n, ok := v.(json.Number); ok {
		r, fault := parseNumber(string(n))
		return number{r: r}, fault == ""
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return number{r: new(big.Rat).SetInt64(rv.Int())}, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return number{r: new(big.Rat).SetUint64(rv.Uint())}, true
	case reflect.Float32:
		return floatNumber(rv.Float(), 32)
	case reflect.Float64:
		return floatNumber(rv.Float(), 64)
	default:
		return number{}, false
	}
}

func floatNumber(f float64, bits int) (number, bool) {
	switch {
	case math.IsNaN(f):
		return number{}, false
	case math.IsInf(f, 1):
		return number{inf: 1}, true
	case math.IsInf(f, -1):
		return number{inf: -1}, true
	}
	if f == math.Trunc(f) {
		return number{r: new(big.Rat).SetFloat64(f)}, true
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'e', -1, bits))
	return number{r: r}, ok
}

// Why text is not a number that can be compared, as a phrase to follow "a
// json.Number that".
var (
	faultNotNumber = "is not a JSON number"
	faultTooLong   = "has more than " + strconv.Itoa(maxNumberText) + " bytes of text"
	faultExponent  = "has an exponent past " + strconv.Itoa(maxNumberExponent)
)

// parseNumber reads text as a JSON number, and nothing looser: no plus sign, no
// leading zero, no bare point, no "Infinity", no hex, no underscores. When it
// cannot, it says why, in the phrases above; the empty string is success.
func parseNumber(s string) (*big.Rat, string) {
	if len(s) > maxNumberText {
		return nil, faultTooLong
	}
	if s == "" {
		return nil, faultNotNumber
	}
	isDigit := func(i int) bool { return i < len(s) && s[i] >= '0' && s[i] <= '9' }
	i := 0
	if s[i] == '-' {
		i++
	}
	switch {
	case i == len(s):
		return nil, faultNotNumber
	case s[i] == '0':
		i++
	case isDigit(i):
		for isDigit(i) {
			i++
		}
	default:
		return nil, faultNotNumber
	}
	if i < len(s) && s[i] == '.' {
		i++
		digits := i
		for isDigit(i) {
			i++
		}
		if i == digits {
			return nil, faultNotNumber
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
		digits, exp := i, 0
		for isDigit(i) {
			if exp = exp*10 + int(s[i]-'0'); exp > maxNumberExponent {
				return nil, faultExponent
			}
			i++
		}
		if i == digits {
			return nil, faultNotNumber
		}
	}
	if i != len(s) {
		return nil, faultNotNumber
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, faultNotNumber
	}
	return r, ""
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
