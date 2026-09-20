package otel

import (
	"go.opentelemetry.io/otel/attribute"

	"github.com/ManavA/keel/metrics"
)

// toKeyValues maps keel attributes to OTel key-values one to one. The
// keys are the documented attribute names, so a series recorded
// in-process and one exported through OTel carry identical dimensions.
func toKeyValues(attrs []metrics.Attr) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, attribute.String(a.Key, a.Value))
	}
	return out
}
