package main

import "github.com/ManavA/keel/llm"

// The scripted model's names. They are model names like any other: the two
// agents are defined with them, the script routes on them and the price table
// lists them.
const (
	scriptedCoordinator = "scripted-coordinator"
	scriptedReviewer    = "scripted-reviewer"
)

// priceTable is what a call costs, in millionths of a US dollar per million
// tokens. keel ships no prices, because a library's copy goes stale without
// anyone noticing; this one is the example's, and it goes stale too.
//
// The Anthropic rows were read on 2026-09-25 from
// https://platform.claude.com/docs/en/about-claude/pricing. A cache write is
// the five-minute rate, 1.25 times the input price. Check them before
// trusting a budget to them.
//
// A provider may answer an alias with a dated id. A reply is priced at the
// model it names and, when the table lacks that, at the model the request
// asked for, so listing the names the configuration uses is enough.
func priceTable(cfg Config) llm.Prices {
	opus := llm.Price{Input: 4_000_000, Output: 20_000_000, CacheRead: 200_000, CacheWrite: 5_000_000}
	haiku := llm.Price{Input: 1_000_000, Output: 5_000_000, CacheRead: 100_000, CacheWrite: 1_250_000}
	prices := llm.Prices{
		"claude-opus-5-5":           opus,
		"claude-haiku-4-5":          haiku,
		"claude-haiku-4-5-20251001": haiku,

		// The scripted model costs nothing to run. It is priced so that the
		// demo shows a run's cost adding up, at round numbers.
		scriptedCoordinator: {Input: 4_000_000, Output: 20_000_000},
		scriptedReviewer:    {Input: 1_000_000, Output: 5_000_000},
	}
	if cfg.Provider() == providerOpenAI {
		// The example cannot know what an OpenAI-compatible server charges,
		// and a local runtime charges nothing. The row is zero, which leaves
		// RUN_MAX_COST_MICROS without effect for this provider; a service
		// that pays for these calls writes the real price here.
		if _, listed := prices[cfg.OpenAIModel]; !listed {
			prices[cfg.OpenAIModel] = llm.Price{}
		}
	}
	return prices
}
