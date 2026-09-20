// Package notifyprefs stores per-user notification preferences — which
// categories of message a user still wants on which channels — and enforces
// them on the send path.
//
// The model is opt-out with safe defaults: everything sends until the user
// says otherwise, except [CategorySecurity] and [CategoryTransactional],
// which always send. A stored opt-out naming either of those is dropped by
// [Store] implementations on write and ignored by [AllowedBy] on read, so a
// caller can never suppress security or transactional mail by accident. A
// user with no stored row gets the defaults, which likewise send everything.
//
// [MemoryStore] is the in-process default. notifyprefs/pg backs the same
// [Store] interface with Postgres for deployments that run more than one
// instance. [GuardedSender] wraps a [mail.Sender] so a service's existing
// mail send path reports [ErrSuppressed] instead of delivering a message
// the recipient opted out of.
package notifyprefs
