package agent

// The tests of the keeper and of the executor are in package agent_test,
// because they use agenttest, which imports this package. These names are how
// they reach what they test.

// Keep is keep.
var Keep = keep

// ErrCancelRequested is errCancelRequested.
var ErrCancelRequested = errCancelRequested

// KeepOptions is keepOptions.
type KeepOptions = keepOptions

// ErrDrained is errDrained.
var ErrDrained = errDrained

// LastWriteTimeout is lastWriteTimeout.
const LastWriteTimeout = lastWriteTimeout

// ErrTimeBudget is errTimeBudget.
var ErrTimeBudget = errTimeBudget
