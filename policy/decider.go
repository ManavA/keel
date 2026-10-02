package policy

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Options configures a Decider. The zero value records in memory.
type Options struct {
	// Recorder defaults to NewMemoryRecorder.
	Recorder Recorder
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Decider decides actions under one Policy and records every decision.
type Decider struct {
	policy Policy
	rec    Recorder
	now    func() time.Time
	log    *slog.Logger
}

// NewDecider builds a Decider. It returns an error when p does not validate.
// The Decider keeps its own copy of the rules, so changing p afterwards
// changes nothing; to change the rules, build another Decider.
func NewDecider(p Policy, opts Options) (*Decider, error) {
	if err := p.check(); err != nil {
		return nil, fmt.Errorf("policy: new decider: %w", err)
	}
	d := &Decider{policy: p.clone(), rec: opts.Recorder, now: opts.Now, log: opts.Logger}
	if d.rec == nil {
		d.rec = NewMemoryRecorder()
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	return d, nil
}

// Decide decides a and records the decision. When the record cannot be
// written it returns the error and a zero Decision, whose empty Effect no
// caller may read as Allow.
func (d *Decider) Decide(ctx context.Context, a Action) (Decision, error) {
	dec := d.policy.Decide(a)
	rec := Record{At: d.now(), Action: a, Decision: dec, Version: d.policy.Version}
	if err := d.rec.Record(ctx, rec); err != nil {
		// The action's own facts are left out: they may be what made it sensitive.
		d.log.ErrorContext(ctx, "record policy decision",
			"kind", a.Kind, "rule", dec.Rule, "effect", dec.Effect, "error", err)
		return Decision{}, fmt.Errorf("policy: record decision: %w", err)
	}
	return dec, nil
}

// Policy returns the rules this Decider decides under. The result is a copy.
func (d *Decider) Policy() Policy {
	return d.policy.clone()
}

// clone copies the rule list and the lists inside each rule. The values in a
// condition are not copied.
func (p Policy) clone() Policy {
	p.Rules = slices.Clone(p.Rules)
	for i := range p.Rules {
		p.Rules[i].When.Kinds = slices.Clone(p.Rules[i].When.Kinds)
		p.Rules[i].When.Attrs = slices.Clone(p.Rules[i].When.Attrs)
	}
	return p
}
