package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"reflect"
	"strings"
	"unicode"
)

// UnmarshalText reads an effect. It also reads "approve" as Ask, which is
// what the TypeScript reference calls it. The empty text reads as the zero
// Effect: Validate refuses it as a rule's effect, and as a Default it means
// Block. Any other text is an error.
func (e *Effect) UnmarshalText(text []byte) error {
	switch s := Effect(text); s {
	case Allow, Ask, Block, "":
		*e = s
	case "approve":
		*e = Ask
	default:
		return fmt.Errorf("effect %q is not allow, ask or block", text)
	}
	return nil
}

// Parse reads a policy from its JSON form and validates it. The document must
// be a JSON object. An unknown field is an error, and so is a key repeated in
// any object, compared without regard to case: encoding/json would take the
// last of two spellings, or merge them, and the file a reviewer reads would
// not be the policy that loads. Numbers are kept as written, as json.Number,
// so a threshold too large or too precise for a float64 is compared as it is.
func Parse(data []byte) (Policy, error) {
	if err := scanDocument(data); err != nil {
		return Policy{}, fmt.Errorf("policy: parse: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy: parse: %w", err)
	}
	if err := p.check(); err != nil {
		return Policy{}, fmt.Errorf("policy: parse: %w", err)
	}
	return p, nil
}

// maxDocumentDepth is how deep a policy document may nest. A policy is a few
// levels deep; a document past this is refused rather than followed.
const maxDocumentDepth = 64

// scanDocument reads data once, without building a Policy, to refuse what the
// decoder would accept without a word: a document that is not an object, a key
// repeated within an object, and anything after the document.
func scanDocument(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return errors.New("a policy is a JSON object, and the document is empty")
	}
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("a policy is a JSON object, not %s", describeToken(tok))
	}
	if err := scanObject(dec, "", 1); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the policy")
	}
	return nil
}

// scanObject reads the rest of an object whose opening brace has been read,
// and the values in it. at is where it is, as the path to it, or "" for the
// document itself.
func scanObject(dec *json.Decoder, at string, depth int) error {
	if depth > maxDocumentDepth {
		return fmt.Errorf("nested more than %d levels deep at %s", maxDocumentDepth, where(at))
	}
	seen := map[string]string{} // each key, folded, as it was first written
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string) // within an object, a key
		if !ok {
			return fmt.Errorf("expected a key in %s", where(at))
		}
		folded := fold(key)
		if first, ok := seen[folded]; ok {
			if first == key {
				return fmt.Errorf("key %q appears twice in %s", key, where(at))
			}
			return fmt.Errorf("key %q appears twice in %s, also written %q", key, where(at), first)
		}
		seen[folded] = key
		child := key
		if at != "" {
			child = at + "." + key
		}
		if err := scanValue(dec, child, depth); err != nil {
			return err
		}
	}
	_, err := dec.Token() // the closing brace
	return err
}

// scanValue reads one value, and what is inside it if it is an object or a
// list.
func scanValue(dec *json.Decoder, at string, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if d == '{' {
		return scanObject(dec, at, depth+1)
	}
	if depth+1 > maxDocumentDepth {
		return fmt.Errorf("nested more than %d levels deep at %s", maxDocumentDepth, where(at))
	}
	for i := 0; dec.More(); i++ {
		if err := scanValue(dec, fmt.Sprintf("%s[%d]", at, i), depth+1); err != nil {
			return err
		}
	}
	_, err = dec.Token() // the closing bracket
	return err
}

func where(at string) string {
	if at == "" {
		return "the top level"
	}
	return at
}

// describeToken names what a document opened with, for an error.
func describeToken(tok json.Token) string {
	switch t := tok.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case json.Delim:
		if t == '[' {
			return "a list"
		}
	}
	return fmt.Sprintf("%v", tok)
}

// fold maps a key to what every spelling that differs from it only in case has
// in common: each rune becomes the least of the runes it folds with. Two keys
// fold alike exactly when strings.EqualFold says they are equal, which is how
// encoding/json matches a key to a field.
func fold(s string) string {
	var b strings.Builder
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

// Validate reports the first thing wrong with p.
func (p Policy) Validate() error {
	if err := p.check(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	return nil
}

// check is Validate without its prefix, for the callers that add their own.
// The default is checked first, then each rule in order.
func (p Policy) check() error {
	if p.Default != "" && !p.Default.Valid() {
		return fmt.Errorf("default %q is not allow, ask or block", p.Default)
	}
	seen := make(map[string]int, len(p.Rules))
	for i, r := range p.Rules {
		if r.Name == "" {
			return fmt.Errorf("rule %d has no name", i)
		}
		if r.Name == RuleDefault {
			return fmt.Errorf("rule %d is named %q, which is what a decision records when no rule matched", i, r.Name)
		}
		if j, dup := seen[r.Name]; dup {
			return fmt.Errorf("rules %d and %d are both named %q", j, i, r.Name)
		}
		seen[r.Name] = i
		if !r.Effect.Valid() {
			return fmt.Errorf("rule %d (%q): effect %q is not allow, ask or block", i, r.Name, r.Effect)
		}
		if err := r.When.check(); err != nil {
			return fmt.Errorf("rule %d (%q): %w", i, r.Name, err)
		}
	}
	return nil
}

// check reports what is wrong with m: a kind that names nothing, a target
// pattern that does not compile or is a bare star, a condition that can never
// hold or holds whatever the action is, or conditions that cannot all hold. The
// design says an empty Kinds, an empty Target and an empty Attrs each mean
// "any", so those are accepted; anything else that would leave a part of a rule
// silently unmatched is refused, since on a block rule that fails open.
func (m Match) check() error {
	for i, k := range m.Kinds {
		if k == "" {
			return fmt.Errorf("kind %d is empty, which names no action", i)
		}
	}
	// Matching against the empty string still reads the whole pattern, so a
	// malformed one is reported whatever the target.
	if _, err := path.Match(m.Target, ""); err != nil {
		return fmt.Errorf("target pattern %q does not compile: %w", m.Target, err)
	}
	if m.Target != "" && strings.Trim(m.Target, "*") == "" {
		return fmt.Errorf("target %q matches only a target that holds no slash; leave the target empty for every target", m.Target)
	}
	for i, c := range m.Attrs {
		if c.Attr == "" {
			return fmt.Errorf("condition %d has no attribute", i)
		}
		if err := c.check(); err != nil {
			return fmt.Errorf("condition %d on %q: %w", i, c.Attr, err)
		}
	}
	for i, c := range m.Attrs {
		if absent, ok := boolOf(c.Value); c.Op != OpExists || !ok || absent {
			continue
		}
		for j, other := range m.Attrs {
			if j == i || other.Attr != c.Attr {
				continue
			}
			if otherAbsent, ok := boolOf(other.Value); other.Op == OpExists && ok && !otherAbsent {
				return fmt.Errorf("conditions %d and %d on %q: exists false is written twice", min(i, j), max(i, j), c.Attr)
			}
			return fmt.Errorf("conditions %d and %d on %q: exists false cannot be combined with another condition on the same attribute, which an attribute that is absent never meets", min(i, j), max(i, j), c.Attr)
		}
	}
	return nil
}

// check reports whether c has an operator that is listed and a value that
// operator can use. A value must be one the operator can hold under for some
// action and fail under for another: eq and ne compare against a number, a
// string or a boolean, since anything else is equal to nothing and eq would
// never hold while ne always did; the ordering operators need a finite number,
// since nothing is above positive infinity and every number is below it; and in
// needs a list with something in it, of such values.
func (c Cond) check() error {
	switch c.Op {
	case OpEq, OpNe:
		ok, infinite := scalar(c.Value)
		if !ok {
			return fmt.Errorf("%s needs a number, a string or a boolean, not %s", c.Op, describe(c.Value))
		}
		if infinite {
			return fmt.Errorf("%s needs a finite number, not %v", c.Op, c.Value)
		}
	case OpGt, OpGte, OpLt, OpLte:
		n, ok := numberOf(c.Value)
		if !ok {
			return fmt.Errorf("%s needs a number, not %s", c.Op, describe(c.Value))
		}
		if n.inf != 0 {
			return fmt.Errorf("%s needs a finite number, not %v", c.Op, c.Value)
		}
	case OpIn:
		if !isList(c.Value) {
			return fmt.Errorf("%s needs a list, not %s", c.Op, describe(c.Value))
		}
		list := reflect.ValueOf(c.Value)
		if list.Len() == 0 {
			return fmt.Errorf("%s needs a list with something in it", c.Op)
		}
		for i := range list.Len() {
			v := list.Index(i).Interface()
			ok, infinite := scalar(v)
			if !ok {
				return fmt.Errorf("%s list element %d is not a number, a string or a boolean, but %s", c.Op, i, describe(v))
			}
			if infinite {
				return fmt.Errorf("%s list element %d is not a finite number, but %v", c.Op, i, v)
			}
		}
	case OpExists:
		if _, ok := boolOf(c.Value); !ok {
			return fmt.Errorf("%s needs a boolean, not %s", c.Op, describe(c.Value))
		}
	default:
		return fmt.Errorf("unknown operator %q", c.Op)
	}
	return nil
}

// describe names what v is, for an error: NaN by name, a json.Number that
// cannot be compared by the bound it is past or the rule it breaks, anything
// else by its Go type.
func describe(v any) string {
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Float32 || rv.Kind() == reflect.Float64 {
		if math.IsNaN(rv.Float()) {
			return "NaN"
		}
	}
	if n, ok := v.(json.Number); ok {
		if _, fault := parseNumber(string(n)); fault != "" {
			return "a json.Number that " + fault
		}
	}
	return fmt.Sprintf("%T", v)
}

// scalar classifies v as eq, ne and in see it: ok when it is a number, a string
// or a boolean, and infinite when it is a number that is not finite.
func scalar(v any) (ok, infinite bool) {
	if n, isNumber := numberOf(v); isNumber {
		return true, n.inf != 0
	}
	if _, isString := stringOf(v); isString {
		return true, false
	}
	_, isBool := boolOf(v)
	return isBool, false
}
