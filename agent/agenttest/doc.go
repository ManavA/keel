// Package agenttest is what tests of agents are built from: a [Clock] a test
// moves by hand, a [Model] that answers from a script, a [FaultStore] that
// stops a process at a chosen store call, and [RunStoreSuite], the contract
// every [agent.Store] passes.
//
// Use it to test an agent, a tool or an engine without a model provider, and
// to hold a new Store to the behaviour of the ones that exist. Nothing here
// needs a network or a database.
//
// # The scripted model
//
// A [Model] picks its reply from the request alone: the [Script] is handed
// the request and the number of assistant turns already in it, and nothing
// about earlier calls decides the answer. So a run that is resumed after a
// crash is sent the reply for the point its journal reached, a call made
// twice gets the same reply twice, and a test needs no coordination between
// the model and the store.
//
// # Killing a store
//
// A crash is anything that stops a process from writing. [FaultStore] makes
// one by failing every call from a chosen one on: [FaultStore.KillBefore]
// for a write that never happened, [FaultStore.KillAfter] for a write that
// landed with the caller never told. Counting an uninterrupted run's calls
// with [FaultStore.Calls] and then killing at each in turn visits every
// point a run can stop at. The process that takes over uses the wrapped
// store directly, or a FaultStore of its own.
//
// [FaultStore.FailBefore] and [FaultStore.FailAfter] are for a failure the
// process outlives: they fail one named method a given number of times and
// leave the store working.
//
// # The Store contract
//
// [RunStoreSuite] is written against the interface and nothing else, so the
// in-process store and the Postgres one are held to the same behaviour and
// cannot drift apart. It takes a constructor and calls it once for every
// case; each call must return an empty store.
package agenttest
