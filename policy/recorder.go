package policy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync"
	"time"
)

// ErrUnrecordable is what a Recorder wraps in the error it returns for a record
// it can never store, however often it is tried again: a value its storage
// cannot represent, or more of them than it will take. A Recorder that cannot
// write because of how things are at the moment (a database that is down, a
// context that is done) does not wrap it. Whoever retries a decision that failed
// to record retries on the second kind and stops at the first, since a retry of
// a record that can never be stored never ends. The error that wraps it is still
// the Recorder's own, which errors.Is and errors.As can find.
var ErrUnrecordable = errors.New("policy: the record cannot be stored")

// unrecordable is err marked as one that no retry will cure. Its text is err's,
// and it wraps both ErrUnrecordable and err.
func unrecordable(err error) error {
	return unrecordableError{err}
}

type unrecordableError struct{ err error }

func (e unrecordableError) Error() string   { return e.err.Error() }
func (e unrecordableError) Unwrap() []error { return []error{ErrUnrecordable, e.err} }

// Record is one decision as it is logged.
type Record struct {
	// ID is the store's own number for a record it lists, which with At is the
	// record's place in the log: a Recorder that keeps an ID sets it on the
	// records it returns and ignores it on the ones it is handed. It is zero for
	// a record that no store has numbered, and the in-memory recorder has none.
	ID       int64     `json:"id,omitempty"`
	At       time.Time `json:"at"`
	Action   Action    `json:"action"`
	Decision Decision  `json:"decision"`
	// Version is the Policy.Version that decided.
	Version string `json:"version,omitempty"`
}

// Recorder keeps the decision log. Record returns nil only when the record is
// kept; a Decider returns no decision for one that is not. An error for a record
// that can never be stored, whatever is retried, wraps ErrUnrecordable. Any
// other error is one a later call may not meet.
type Recorder interface {
	Record(ctx context.Context, rec Record) error
}

// defaultMemoryRecords is how many decisions a MemoryRecorder keeps unless it
// is built with another number.
const defaultMemoryRecords = 1000

// MemoryRecorder is the in-process Recorder. It keeps the most recent 1000
// decisions and drops the oldest as new ones arrive, so a long-running process
// that never reads it does not grow with every decision; a Decider built with
// Options.MemoryRecords keeps another number. What it keeps and what it returns
// are copies, down to the lists and objects inside an action's attributes. It
// is safe for concurrent use, and its zero value is ready to use.
type MemoryRecorder struct {
	mu   sync.Mutex
	keep int      // how many to keep; zero or less is the default
	recs []Record // at most the bound; once full, a ring whose oldest is at next
	next int
}

// bound is how many records m keeps.
func (m *MemoryRecorder) bound() int {
	if m.keep > 0 {
		return m.keep
	}
	return defaultMemoryRecords
}

func newMemoryRecorder(keep int) *MemoryRecorder {
	return &MemoryRecorder{keep: keep}
}

// NewMemoryRecorder builds an empty MemoryRecorder.
func NewMemoryRecorder() *MemoryRecorder {
	return newMemoryRecorder(defaultMemoryRecords)
}

// Record implements Recorder. It keeps a copy of rec. It fails only when rec
// holds more than 10000 values to copy, since a record kept with its inside
// shared with the caller could be changed by the caller, and the error wraps
// ErrUnrecordable.
func (m *MemoryRecorder) Record(_ context.Context, rec Record) error {
	rec, err := rec.clone(maxCopyValues)
	if err != nil {
		return fmt.Errorf("policy: record: %w", unrecordable(err))
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if bound := m.bound(); len(m.recs) >= bound {
		m.recs[m.next] = rec
		m.next = (m.next + 1) % bound
	} else {
		m.recs = append(m.recs, rec)
	}
	return nil
}

// Records returns a copy of the log, oldest first.
func (m *MemoryRecorder) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.recs))
	for i := range m.recs {
		// Kept records were copied within the bound, so copying one out cannot
		// exceed it, and needs none.
		out[i], _ = m.recs[(m.next+i)%len(m.recs)].clone(noCopyLimit)
	}
	return out
}

// clone copies everything in r that a caller could change in place, or fails if
// that is more than budget values.
func (r Record) clone(budget int) (Record, error) {
	action, err := r.Action.clone(budget)
	r.Action = action
	r.Decision = r.Decision.clone()
	return r, err
}

// clone copies the attributes, down to every list and object inside them, or
// fails if that is more than budget values.
func (a Action) clone(budget int) (Action, error) {
	c := newCopier(budget)
	if attrs, ok := c.value(a.Attrs).(map[string]any); ok {
		a.Attrs = attrs
	}
	return a, c.err()
}

// clone copies the lists in d.
func (d Decision) clone() Decision {
	d.Matched = slices.Clone(d.Matched)
	d.Uncertain = slices.Clone(d.Uncertain)
	return d
}

// How far a copy follows a value, and how much of it. The depth bound stops a
// value that refers to itself; the count of values bounds the work, which depth
// alone does not: a value that shares a child twice at every level is a few
// levels deep and doubles with each. A copy past either bound fails, and its
// result is not used. A few thousand values is ample for an action's attributes
// or a policy's conditions.
const (
	maxCopyDepth  = 64
	maxCopyValues = 10000
	noCopyLimit   = math.MaxInt
)

var errCopyTooLarge = fmt.Errorf("more than %d values to copy", maxCopyValues)

// copier copies values, each one a copy that shares no list, object, pointer
// target or array with its original, however deeply they nest. What Go cannot
// copy is shared: a function, a channel, and the unexported fields of a struct.
// Everything one copier copies counts against one budget.
type copier struct {
	left int
	over bool
}

func newCopier(budget int) *copier {
	return &copier{left: budget}
}

// value returns a copy of v. After the budget or the depth bound is passed, what
// it returns is not a copy, and err says so.
func (c *copier) value(v any) any {
	if v == nil {
		return nil
	}
	return c.reflect(reflect.ValueOf(v), 0).Interface()
}

// err reports whether a copy went past its budget.
func (c *copier) err() error {
	if c.over {
		return errCopyTooLarge
	}
	return nil
}

func (c *copier) reflect(v reflect.Value, depth int) reflect.Value {
	if c.over || depth > maxCopyDepth {
		return v
	}
	// An interface is the value inside it, and is not counted twice.
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(c.reflect(v.Elem(), depth+1))
		return out
	}
	if c.left <= 0 {
		c.over = true
		return v
	}
	c.left--
	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(c.reflect(v.Index(i), depth+1))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(c.reflect(v.Index(i), depth+1))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			out.SetMapIndex(iter.Key(), c.reflect(iter.Value(), depth+1))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(c.reflect(v.Elem(), depth+1))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(c.reflect(v.Field(i), depth+1))
			}
		}
		return out
	default:
		return v
	}
}

var _ Recorder = (*MemoryRecorder)(nil)
