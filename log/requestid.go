package log

import "context"

// RequestIDKey is the attribute key a request id is logged under, and the name
// this package looks for. It is exported so a service that already emits
// request ids under a different name can line the two up rather than emitting
// both.
const RequestIDKey = "request_id"

type requestIDContextKey struct{}

// WithRequestID returns a context carrying id. The request-id middleware in
// httpx/middleware calls this; so can anything else with an id worth
// propagating — a queue consumer handling a message, a job run.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestID returns the id carried by ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}
