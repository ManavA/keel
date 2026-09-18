// Package idempotency lets a client safely retry an HTTP request that may
// have already succeeded, by replaying the first response instead of
// running the handler again.
//
// # Relationship to jobs.Guard
//
// jobs.Guard also dedupes by key, but it only remembers whether a key ran;
// it has nothing to hand back to a caller waiting on a result. This package
// stores and replays the whole response, because an HTTP client retrying a
// POST needs the response it would have gotten the first time, not just
// confirmation that something happened.
//
// # Concurrent requests with the same key
//
// The middleware makes the second of two concurrent requests for the same
// key wait for the first to finish, rather than answering it with a
// conflict. A client that retries a slow request is very likely retrying
// because it gave up waiting, not because it wants two outcomes, so handing
// it the eventual real response is more useful than a synthetic error it
// will just retry again. The wait is bounded by [Options.Wait]: if the
// first request is still running when that elapses, the second gets 409,
// the same as it would if it had not waited at all.
//
// # Lease and TTL
//
// Two durations apply to a key. [Options.ClaimLease] is how long a running
// request holds it; [Options.TTL] is how long the finished response is
// replayed. They are separate because they answer different questions: the
// lease has to outlast the slowest handler and should be short, so a key
// whose process died frees up soon, while the TTL is as long as clients may
// keep retrying. A request that outlives its lease loses the key: a retry
// then runs the handler again, and the late request's response goes to its
// own client but is not stored.
package idempotency
