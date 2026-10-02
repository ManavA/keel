package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
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

// check reports what is wrong with m: a target pattern that does not compile,
// or a condition no action could hold under.
func (m Match) check() error {
	// Matching against the empty string still reads the whole pattern, so a
	// malformed one is reported whatever the target.
	if _, err := path.Match(m.Target, ""); err != nil {
		return fmt.Errorf("target pattern %q does not compile: %w", m.Target, err)
	}
	for i, c := range m.Attrs {
		if err := c.check(); err != nil {
			return fmt.Errorf("condition %d on %q: %w", i, c.Attr, err)
		}
	}
	return nil
}

// check reports whether c has an operator that is listed and a value that
// operator can use.
func (c Cond) check() error {
	switch c.Op {
	case OpEq, OpNe:
		return nil
	case OpGt, OpGte, OpLt, OpLte:
		if _, ok := numberOf(c.Value); !ok {
			return fmt.Errorf("%s needs a number, not %T", c.Op, c.Value)
		}
	case OpIn:
		if !isList(c.Value) {
			return fmt.Errorf("%s needs a list, not %T", c.Op, c.Value)
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
