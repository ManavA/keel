package metrics

import (
	"context"
	"sync"
)

// Call is one captured measurement: the instrument name, the value
// added or recorded, and the attributes it carried.
type Call struct {
	Value float64
	Attrs []Attr
}

// Attr returns the value recorded for key, and whether it was present.
func (c Call) Attr(key string) (string, bool) {
	for _, a := range c.Attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return "", false
}

// InMemory captures every measurement in process memory, for tests that
// assert on what the code recorded rather than parsing logs. It is safe
// for concurrent use.
type InMemory struct {
	mu     sync.Mutex
	counts map[string][]Call
	hists  map[string][]Call
}

// NewInMemory builds an empty InMemory.
func NewInMemory() *InMemory {
	return &InMemory{counts: map[string][]Call{}, hists: map[string][]Call{}}
}

type memCounter struct {
	mem  *InMemory
	name string
}

func (c memCounter) Add(_ context.Context, delta int64, attrs ...Attr) {
	c.mem.mu.Lock()
	defer c.mem.mu.Unlock()
	c.mem.counts[c.name] = append(c.mem.counts[c.name], Call{Value: float64(delta), Attrs: attrs})
}

type memHistogram struct {
	mem  *InMemory
	name string
}

func (h memHistogram) Record(_ context.Context, value float64, attrs ...Attr) {
	h.mem.mu.Lock()
	defer h.mem.mu.Unlock()
	h.mem.hists[h.name] = append(h.mem.hists[h.name], Call{Value: value, Attrs: attrs})
}

// Metrics returns a *Metrics recording every instrument into m.
func (m *InMemory) Metrics() *Metrics {
	return New(Instruments{
		HTTPRequests:    memCounter{m, NameHTTPRequests},
		HTTPRequestDur:  memHistogram{m, NameHTTPRequestDur},
		PoolAcquires:    memCounter{m, NamePoolAcquires},
		PoolAcquireDur:  memHistogram{m, NamePoolAcquireDur},
		JobRuns:         memCounter{m, NameJobRuns},
		OutboxPublished: memCounter{m, NameOutboxPublished},
		OutboxFailed:    memCounter{m, NameOutboxFailed},
		OutboxLag:       memHistogram{m, NameOutboxLag},
		OutboxAttempts:  memHistogram{m, NameOutboxAttempts},
		EventsPublished: memCounter{m, NameEventsPublished},
		EventsDropped:   memCounter{m, NameEventsDropped},
		GeocodeLookups:  memCounter{m, NameGeocodeLookups},
	})
}

func matchAttrs(call Call, attrs []Attr) bool {
	for _, want := range attrs {
		got, ok := call.Attr(want.Key)
		if !ok || got != want.Value {
			return false
		}
	}
	return true
}

// CounterTotal sums the deltas added to the named counter whose calls
// carry all of attrs. An empty attrs matches every call to that
// instrument.
func (m *InMemory) CounterTotal(name string, attrs ...Attr) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for _, c := range m.counts[name] {
		if matchAttrs(c, attrs) {
			total += int64(c.Value)
		}
	}
	return total
}

// HistogramValues returns the values recorded on the named histogram
// whose calls carry all of attrs, in record order.
func (m *InMemory) HistogramValues(name string, attrs ...Attr) []float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []float64
	for _, c := range m.hists[name] {
		if matchAttrs(c, attrs) {
			out = append(out, c.Value)
		}
	}
	return out
}
