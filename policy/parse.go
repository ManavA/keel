package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
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
		return fmt.Errorf("policy: unknown effect %q", text)
	}
	return nil
}

// Parse reads a policy from its JSON form and validates it. An unknown field
// is an error.
func Parse(data []byte) (Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy: parse: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Policy{}, errors.New("policy: parse: unexpected data after the policy")
	}
	if err := p.check(); err != nil {
		return Policy{}, fmt.Errorf("policy: parse: %w", err)
	}
	return p, nil
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
// pattern that does not compile, or a condition that can never hold or holds
// whatever the action is. The design says an empty Kinds, an empty Target and
// an empty Attrs each mean "any", so those are accepted; anything else that
// would leave a part of a rule silently unmatched is refused, since on a block
// rule that fails open.
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
	for i, c := range m.Attrs {
		if c.Attr == "" {
			return fmt.Errorf("condition %d has no attribute", i)
		}
		if err := c.check(); err != nil {
			return fmt.Errorf("condition %d on %q: %w", i, c.Attr, err)
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
			return fmt.Errorf("%s needs a number, a string or a boolean, not %T", c.Op, c.Value)
		}
		if infinite {
			return fmt.Errorf("%s needs a finite number, not %v", c.Op, c.Value)
		}
	case OpGt, OpGte, OpLt, OpLte:
		n, ok := numberOf(c.Value)
		if !ok {
			return fmt.Errorf("%s needs a number, not %T", c.Op, c.Value)
		}
		if n.IsInf() {
			return fmt.Errorf("%s needs a finite number, not %v", c.Op, c.Value)
		}
	case OpIn:
		if !isList(c.Value) {
			return fmt.Errorf("%s needs a list, not %T", c.Op, c.Value)
		}
		list := reflect.ValueOf(c.Value)
		if list.Len() == 0 {
			return fmt.Errorf("%s needs a list with something in it", c.Op)
		}
		for i := range list.Len() {
			v := list.Index(i).Interface()
			ok, infinite := scalar(v)
			if !ok {
				return fmt.Errorf("%s list element %d is not a number, a string or a boolean, but %T", c.Op, i, v)
			}
			if infinite {
				return fmt.Errorf("%s list element %d is not a finite number, but %v", c.Op, i, v)
			}
		}
	case OpExists:
		if _, ok := boolOf(c.Value); !ok {
			return fmt.Errorf("%s needs a boolean, not %T", c.Op, c.Value)
		}
	default:
		return fmt.Errorf("unknown operator %q", c.Op)
	}
	return nil
}

// scalar classifies v as eq, ne and in see it: ok when it is a number, a string
// or a boolean, and infinite when it is a number that is not finite.
func scalar(v any) (ok, infinite bool) {
	if n, isNumber := numberOf(v); isNumber {
		return true, n.IsInf()
	}
	if _, isString := stringOf(v); isString {
		return true, false
	}
	_, isBool := boolOf(v)
	return isBool, false
}
