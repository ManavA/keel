// Package outbox writes an event to Postgres in the same transaction as
// the domain change it describes, then relays it to an [events.Publisher]
// afterward, so a crash between the domain write and the publish cannot
// leave one without the other.
//
// # Why publish and mark-published are two operations, not one
//
// [Enqueue] commits with the caller's own transaction: either both the
// domain row and the outbox row exist, or neither does. [Relay] then polls
// for unpublished rows and publishes them outside any transaction, because
// there is no transaction that can span a network call to a message broker
// and the local database at once. That means a crash between a successful
// publish and the update that records it can leave a row published at the
// broker but still marked unpublished here — Relay is at-least-once, the
// same stance events/pubsub documents for Pub/Sub itself, and for the same
// reason: closing that gap needs a two-phase commit with the broker, which
// Postgres and Pub/Sub do not offer each other. A row Relay publishes more
// than once always carries the same id (see [Envelope]), so a consumer
// that dedupes on it is correct regardless of how many times a given row
// is delivered.
//
// # A failing row backs off
//
// A row whose publish fails is hidden from the relay until a backoff
// passes; the backoff doubles with each further failure up to a cap (see
// [Options]). A row that can never publish therefore stops taking a slot
// in every batch, and the rows behind it are fetched instead. Once a failed
// row is due again it is fetched in creation order, ahead of anything
// newer, so a transient failure does not push a row behind later arrivals.
// There is no attempts cap and no dead-letter table: a poisoned row is
// retried at the capped interval until someone deletes or fixes it.
//
// # What is out of scope
//
// Relay does not serialize retries per aggregate: two rows for the same
// aggregate can still be published out of order if the earlier one is
// being retried when the later one succeeds. Ordering delivery strictly
// per aggregate would need a partition key on the outbox table and a
// relay that tracks in-flight rows per partition; nothing here does
// either.
package outbox
