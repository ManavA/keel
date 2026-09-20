// Package retry runs a function with exponential backoff, stopping on the
// first success, the first error [Options.Retryable] rejects, the attempt
// limit, or context cancellation, whichever comes first. [Breaker] counts
// consecutive call failures and short-circuits callers with [ErrOpen] while
// the downstream is out, letting one probe through after its reset timeout.
//
// # Full jitter
//
// The delay before each retry follows the "Full Jitter" algorithm from the
// AWS Architecture Blog post "Exponential Backoff And Jitter" (Marc Brooker,
// 2015): a uniform random duration in [0, cap), where cap doubles with each
// attempt up to [Options.MaxDelay]. AWS's own simulations in that post found
// full jitter completes a batch of retries in less total time and with
// fewer calls than "equal jitter" (backoff plus a smaller random offset) or
// no jitter at all, because spreading retries across the whole window
// avoids the synchronized retry spikes that a fixed or half-fixed backoff
// produces when many callers fail at once. The tradeoff is that a given
// retry can land anywhere from zero to the full window, including near
// zero, rather than close to the intended backoff.
//
// # Cancellation
//
// A cancelled context stops [Do] before it starts the next attempt, whether
// it is cancelled between attempts or during the backoff sleep. It never
// stops a call to fn already in progress; fn must watch ctx itself if it
// needs to return early.
package retry
