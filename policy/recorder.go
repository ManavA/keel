package policy

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"time"
)

// Record is one decision as it is logged.
type Record struct {
	At       time.Time `json:"at"`
	Action   Action    `json:"action"`
	Decision Decision  `json:"decision"`
	// Version is the Policy.Version that decided.
	Version string `json:"version,omitempty"`
}

// Recorder keeps the decision log.
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
// is safe for concurrent use.
type MemoryRecorder struct {
	mu   sync.Mutex
	keep int
	recs []Record // at most keep; once full, a ring whose oldest is at next
	next int
}

func newMemoryRecorder(keep int) *MemoryRecorder {
	return &MemoryRecorder{keep: keep}
}

// NewMemoryRecorder builds an empty MemoryRecorder.
func NewMemoryRecorder() *MemoryRecorder {
	return newMemoryRecorder(defaultMemoryRecords)
}

// Record implements Recorder. It keeps a copy of rec, and cannot fail.
func (m *MemoryRecorder) Record(_ context.Context, rec Record) error {
	rec = rec.clone()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.recs) < m.keep {
		m.recs = append(m.recs, rec)
		return nil
	}
	m.recs[m.next] = rec
	m.next = (m.next + 1) % m.keep
	return nil
}

// Records returns a copy of the log, oldest first.
func (m *MemoryRecorder) Records() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Record, len(m.recs))
	for i := range m.recs {
		out[i] = m.recs[(m.next+i)%len(m.recs)].clone()
	}
	return out
}

// clone copies everything in r that a caller could change in place.
func (r Record) clone() Record {
	r.Action = r.Action.clone()
	r.Decision = r.Decision.clone()
	return r
}

// clone copies the attributes, down to every list and object inside them.
func (a Action) clone() Action {
	if attrs, ok := copyValue(a.Attrs).(map[string]any); ok {
		a.Attrs = attrs
	}
	return a
}

// clone copies the lists in d.
func (d Decision) clone() Decision {
	d.Matched = slices.Clone(d.Matched)
	d.Uncertain = slices.Clone(d.Uncertain)
	return d
}

// maxCopyDepth bounds how far copyValue follows a value, so that one that
// refers to itself cannot loop. Past it a value is shared, not copied.
const maxCopyDepth = 64

// copyValue returns a copy of v that shares no list, object, pointer target or
// array with it, however deeply they nest. What Go cannot copy is shared: a
// function, a channel, and the unexported fields of a struct.
func copyValue(v any) any {
	if v == nil {
		return nil
	}
	return copyReflect(reflect.ValueOf(v), 0).Interface()
}

func copyReflect(v reflect.Value, depth int) reflect.Value {
	if depth > maxCopyDepth {
		return v
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(copyReflect(v.Elem(), depth+1))
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(copyReflect(v.Index(i), depth+1))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(copyReflect(v.Index(i), depth+1))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			out.SetMapIndex(iter.Key(), copyReflect(iter.Value(), depth+1))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(copyReflect(v.Elem(), depth+1))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(copyReflect(v.Field(i), depth+1))
			}
		}
		return out
	default:
		return v
	}
}

var _ Recorder = (*MemoryRecorder)(nil)
