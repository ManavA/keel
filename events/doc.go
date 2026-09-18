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
// [Publisher.Publish] takes `any` and marshals it to JSON. [Subscriber.Subscribe]
// hands the handler raw bytes rather than decoding into a caller's type,
// because this package does not know what type any given caller publishes.
// The handler decodes the payload itself.
//
// # In-process by default
//
// [InMemoryBus] implements both interfaces without an external broker and
// is the default choice for a single process, including tests. [PubSubPublisher]
// and [PubSubSubscriber] wrap Google Cloud Pub/Sub for cross-process
// delivery, and are optional: a caller written against the Publisher and
// Subscriber interfaces can switch between InMemoryBus and Pub/Sub without
// changing its own code.
package events
