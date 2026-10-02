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
	// MemoryRecords is how many of the most recent decisions the default
	// in-memory recorder keeps. Zero or less keeps 1000. It has no effect when
	// Recorder is set.
	MemoryRecords int
}

// Decider decides actions under one Policy and records every decision.
type Decider struct {
	policy Policy
	rec    Recorder
	now    func() time.Time
	log    *slog.Logger
}

// NewDecider builds a Decider. It returns an error when p does not validate.
// The Decider takes a deep copy of the rules, every list and every value in
// them, so nothing the caller does to p afterwards, from any goroutine, changes
// the Decider; to change the rules, build another one.
func NewDecider(p Policy, opts Options) (*Decider, error) {
	if err := p.check(); err != nil {
		return nil, fmt.Errorf("policy: new decider: %w", err)
	}
	d := &Decider{policy: p.clone(), rec: opts.Recorder, now: opts.Now, log: opts.Logger}
	if d.rec == nil {
		keep := opts.MemoryRecords
		if keep <= 0 {
			keep = defaultMemoryRecords
		}
		d.rec = newMemoryRecorder(keep)
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
// caller may read as Allow. The Recorder is handed a copy of the action and the
// decision, so neither it nor the caller can change what the other holds.
//
// A Recorder that panics takes Decide with it: no Decision is returned.
func (d *Decider) Decide(ctx context.Context, a Action) (Decision, error) {
	dec := d.policy.Decide(a)
	rec := Record{At: d.now(), Action: a.clone(), Decision: dec.clone(), Version: d.policy.Version}
	if err := d.rec.Record(ctx, rec); err != nil {
		// The action's own facts are left out: they may be what made it sensitive.
		d.log.ErrorContext(ctx, "record policy decision",
			"kind", a.Kind, "rule", dec.Rule, "effect", dec.Effect, "error", err)
		return Decision{}, fmt.Errorf("policy: record decision: %w", err)
	}
	return dec, nil
}

// Policy returns the rules this Decider decides under, as a deep copy: changing
// it changes nothing in the Decider.
func (d *Decider) Policy() Policy {
	return d.policy.clone()
}

// clone copies the rule list, the lists inside each rule, and every value in a
// condition, however deeply it nests.
func (p Policy) clone() Policy {
	p.Rules = slices.Clone(p.Rules)
	for i := range p.Rules {
		w := &p.Rules[i].When
		w.Kinds = slices.Clone(w.Kinds)
		w.Attrs = slices.Clone(w.Attrs)
		for j := range w.Attrs {
			w.Attrs[j].Value = copyValue(w.Attrs[j].Value)
		}
	}
	return p
}
