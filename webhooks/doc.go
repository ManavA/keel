// Package webhooks delivers events to external URLs over HTTP, with each
// delivery signed so the receiver can verify it came from a holder of the
// endpoint's secret.
//
// A [Dispatcher] implements [events.Publisher], so it plugs directly into an
// [outbox.Relay] as its publisher: the outbox stays the durability layer —
// an event is only marked published once every subscribed endpoint accepted
// it — and webhooks is the delivery layer over it. A delivery that fails
// every retry returns an error from Publish, which leaves the outbox row
// unpublished for the relay to attempt again, so a failing endpoint is
// retried, not dropped. One endpoint failing does not stop the others from
// receiving the event.
//
// # Signing
//
// The request body is the event marshaled with [events.Marshal] — for an
// outbox relay that is the JSON envelope carrying the outbox id and payload,
// so the bytes on the wire are exactly the bytes the signature covers. The
// signature is HMAC-SHA256 over those bytes, sent as
// `X-Keel-Signature: sha256=<hex>`, alongside `X-Keel-Topic`. [Sign] produces
// it and [Verify] checks it in constant time.
//
// # Failure counts
//
// The dispatcher counts consecutive Publish failures per endpoint, reset by
// the next success, reported by [Dispatcher.Failures]. It is an in-memory
// operator signal, not durable state: restarting the process zeroes it.
package webhooks
