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
		orders, ok := compare(v, c.Value)
		if !ok {
			return unclear
		}
		return agree(orders, func(order int) bool { return (order == 0) == (c.Op == OpEq) })
	case OpIn:
		return in(v, c.Value)
	default: // gt, gte, lt, lte: check has refused every other operator
		return ordered(c.Op, v, c.Value)
	}
}

// compare orders x against y, and says whether the two can be compared at all:
// two numbers, two strings or two booleans. A value of any other type, or of
// another type than its partner, cannot be. For numbers the result is the
// ordering, -1, 0 or 1, under each reading of the two (one, unless a number is a
// float with an integer value too large to be told from its shortest decimal);
// for strings and booleans it is 0 when they are equal and 1 when they are not.
func compare(x, y any) (orders []int, comparable bool) {
	if nx, ok := numberOf(x); ok {
		ny, ok := numberOf(y)
		if !ok {
			return nil, false
		}
		return nx.orders(ny), true
	}
	if sx, ok := stringOf(x); ok {
		sy, ok := stringOf(y)
		if !ok {
			return nil, false
		}
		return []int{boolOrder(sx == sy)}, true
	}
	if bx, ok := boolOf(x); ok {
		by, ok := boolOf(y)
		if !ok {
			return nil, false
		}
		return []int{boolOrder(bx == by)}, true
	}
	return nil, false
}

func boolOrder(equal bool) int {
	if equal {
		return 0
	}
	return 1
}

// agree is what a condition comes to over the orderings compare gave: the one
// outcome when every reading gives it, and otherwise cannot be told.
func agree(orders []int, holds func(order int) bool) truth {
	t := fromBool(holds(orders[0]))
	for _, order := range orders[1:] {
		if fromBool(holds(order)) != t {
			return unclear
		}
	}
	return t
}

// ordered compares x with y under one of the four ordering operators. It can
// be told only between two numbers, and only if every reading of them agrees.
func ordered(op Op, x, y any) truth {
	nx, ok := numberOf(x)
	if !ok {
		return unclear
	}
	ny, ok := numberOf(y)
	if !ok {
		return unclear
	}
	return agree(nx.orders(ny), func(order int) bool {
		switch op {
		case OpGt:
			return order > 0
		case OpGte:
			return order >= 0
		case OpLt:
			return order < 0
		default:
			return order <= 0
		}
	})
}

// in is eq on each element of list, joined by "or": it holds if any element
// equals v, and does not hold only if every element could be compared with v
// and none equalled it. An element that could not be compared, or could not be
// told, leaves the rest unable to say, since it might have been the one: a list
// of 22 and "ssh" does not rule out the string "22".
func in(v, list any) truth {
	rl := reflect.ValueOf(list)
	allTold := true
	for i := range rl.Len() {
		orders, ok := compare(v, rl.Index(i).Interface())
		if !ok {
			allTold = false
			continue
		}
		switch agree(orders, func(order int) bool { return order == 0 }) {
		case met:
			return met
		case unclear:
			allTold = false
		}
	}
	if allTold {
		return unmet
	}
	return unclear
}

// isList reports whether v is a slice or an array.
func isList(v any) bool {
	k := reflect.ValueOf(v).Kind()
	return k == reflect.Slice || k == reflect.Array
}

// number is an exact number, or an infinity. A float with an integer value too
// large to be told from its shortest decimal has two readings: r is the exact
// integer, and alt is the shortest decimal that gives the float back. Every
// other number has one, and alt is nil.
type number struct {
	inf int      // 1 or -1 for an infinity, else 0
	r   *big.Rat // the value, when finite
	alt *big.Rat // the second reading, when there is one
}

// order is the ordering of the reading xr of x against the reading yr of y.
func order(x number, xr *big.Rat, y number, yr *big.Rat) int {
	if x.inf != 0 || y.inf != 0 {
		return cmp.Compare(x.inf, y.inf)
	}
	return xr.Cmp(yr)
}

// orders is the ordering of n against o under each reading: one, or two when
// either has a second. When both have, their readings are paired, exact with
// exact and shortest with shortest, so that a float is the same number as
// itself; when one has, each of its readings meets the other's one.
func (n number) orders(o number) []int {
	out := []int{order(n, n.r, o, o.r)}
	switch {
	case n.alt != nil && o.alt != nil:
		out = append(out, order(n, n.alt, o, o.alt))
	case n.alt != nil:
		out = append(out, order(n, n.alt, o, o.r))
	case o.alt != nil:
		out = append(out, order(n, n.r, o, o.alt))
	}
	return out
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
// integer it is. A float without an integer value is the shortest decimal that
// gives it back, so a float64 0.1 is the number 0.1 and a threshold written 0.1
// meets it. A float with an integer value has two readings, that integer and
// its shortest decimal, which are one number below 2^53 (below 2^24 for a
// float32) and two numbers above: a float64 2^70 is 1180591620717411303424 and
// is also 1180591620717411300000, and neither is the better answer, so a
// comparison they disagree on cannot be told. Text is the decimal it spells, of
// any size or precision up to the bounds above, and must be a JSON number. A NaN
// is not a number, nor is text that is not one.
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
	shortest, ok := new(big.Rat).SetString(strconv.FormatFloat(f, 'e', -1, bits))
	if !ok {
		return number{}, false
	}
	if f != math.Trunc(f) {
		return number{r: shortest}, true
	}
	exact := new(big.Rat).SetFloat64(f)
	if exact.Cmp(shortest) == 0 {
		return number{r: exact}, true
	}
	return number{r: exact, alt: shortest}, true
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
