// Package events publishes and subscribes to messages, between processes
// or between goroutines in one process, behind two interfaces.
//
// # Why publish and subscribe are separate interfaces
//
// Publishing is a single call that either succeeds or fails. Subscribing is
// a long-running loop that must survive redeliveries and acknowledge or
// reject each message individually. Combining both into one interface would
// require a caller that only ever publishes to implement subscribe methods
// it never calls.
//
// # Why a message is `[]byte` on the subscribe side
//
// [Publisher.Publish] takes `any` and marshals it with [Marshal].
// [Subscriber.Subscribe] hands the handler raw bytes rather than decoding
// into a caller's type, because this package does not know what type any
// given caller publishes. The handler decodes the payload itself. Every
// Publisher implementation in this module, and in events/pubsub, marshals
// with the same [Marshal] function, so a given event value encodes
// identically regardless of which implementation publishes it.
//
// # In-process by default
//
// [InMemoryBus] implements both interfaces without an external broker and
// is the default choice for a single process, including tests. The
// events/pubsub subpackage wraps Google Cloud Pub/Sub for cross-process
// delivery, and is optional: it is a separate package specifically so that
// importing events alone does not pull in Pub/Sub's dependency tree. A
// caller written against the Publisher and Subscriber interfaces can
// switch between InMemoryBus and events/pubsub without changing its own
// calling code — but the two are not interchangeable in what they
// guarantee about delivery. InMemoryBus is at-most-once and can drop a
// message under load (loudly: logged and counted, never silently);
// events/pubsub is at-least-once and can redeliver a message, possibly out
// of order. See [Subscriber] and [InMemoryBus] for the detail a Handler
// needs to account for.
package events
