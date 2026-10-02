package agent

import "errors"

// Errors a caller has a decision to make about.
var (
	ErrNotFound       = errors.New("agent: not found")
	ErrUnknownAgent   = errors.New("agent: no such agent registered")
	ErrLeaseLost      = errors.New("agent: lease lost")
	ErrNotClaimable   = errors.New("agent: run cannot be claimed")
	ErrConflict       = errors.New("agent: step is not in the expected status")
	ErrAlreadyDecided = errors.New("agent: approval already decided")
	ErrFinished       = errors.New("agent: run has ended")

	// ErrTransient marks a tool error as worth trying again: the call stays
	// started and is executed again later with the same Key.
	ErrTransient = errors.New("agent: transient failure")
	// ErrPermanent marks a Model error as one no retry will fix: the run
	// fails at once.
	ErrPermanent = errors.New("agent: permanent failure")
)
