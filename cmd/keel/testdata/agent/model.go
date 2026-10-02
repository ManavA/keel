package main

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/ManavA/keel/agent"
	"github.com/ManavA/keel/app"
	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/anthropic"
	"github.com/ManavA/keel/llm/openai"
)

const (
	// maxReplyTokens bounds every reply. The budget holds a call's worst case
	// before it starts, and that is only a worst case if the request carries
	// the same bound, so the adapter and the budget are given this one number.
	// It is within the output cap of every model the example names.
	maxReplyTokens = 4096

	// processBudgetMicros is what every run this process makes may cost
	// between them: $50. A run's own limit is RUN_MAX_COST_MICROS.
	processBudgetMicros = 50_000_000
)

// modelNames are the models the two agents are defined with.
type modelNames struct {
	coordinator string
	reviewer    string
}

func namesFor(cfg Config) modelNames {
	switch cfg.Provider() {
	case providerAnthropic:
		return modelNames{coordinator: strings.TrimSpace(cfg.CoordinatorModel), reviewer: strings.TrimSpace(cfg.ReviewerModel)}
	case providerOpenAI:
		m := strings.TrimSpace(cfg.OpenAIModel)
		return modelNames{coordinator: m, reviewer: m}
	default:
		return modelNames{coordinator: scriptedCoordinator, reviewer: scriptedReviewer}
	}
}

// buildProvider is the one model that talks to something: the script, or a
// provider's API. No key is logged here or anywhere else; the provider
// packages send it and nothing more.
func buildProvider(cfg Config, logger *slog.Logger) (llm.Model, error) {
	switch cfg.Provider() {
	case providerAnthropic:
		// The refusal fallback is opt-in in the library and on here: a
		// refused request is retried by the API on the model it recommends,
		// and the reply names the model that answered.
		client, err := anthropic.New(anthropic.Options{
			APIKey:          strings.TrimSpace(cfg.AnthropicAPIKey),
			RefusalFallback: "default",
			Logger:          logger,
		})
		if err != nil {
			return nil, fmt.Errorf("anthropic client: %w", err)
		}
		return client, nil
	case providerOpenAI:
		client, err := openai.New(openai.Options{
			APIKey:  strings.TrimSpace(cfg.OpenAIAPIKey),
			BaseURL: strings.TrimSpace(cfg.OpenAIBaseURL),
			Model:   strings.TrimSpace(cfg.OpenAIModel),
			Logger:  logger,
		})
		if err != nil {
			return nil, fmt.Errorf("openai client: %w", err)
		}
		return client, nil
	default:
		return llm.NewScripted(demoScript(), llm.ScriptedOptions{}), nil
	}
}

// buildModel composes the wrappers around provider and adapts the result for
// the engine. Outermost first: the budget, then the meter, so the meter
// records only the calls the budget let through; then the fallback chain; then
// one retrying wrapper per provider, so each retries its own transient
// failures before the chain moves on.
//
// The chain here has one provider in it. A second goes in the same call, with
// its own Retrying, and a request must then name a model both understand.
func buildModel(provider llm.Model, cfg Config, logger *slog.Logger) (agent.Model, *llm.Metered, error) {
	names := namesFor(cfg)
	prices := priceTable(cfg)

	chain, err := llm.NewFallback(llm.FallbackOptions{Logger: logger},
		llm.NewRetrying(provider, llm.RetryOptions{}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("fallback chain: %w", err)
	}
	metered := llm.NewMetered(chain, llm.MeterOptions{Prices: prices})
	budgeted, err := llm.NewBudgeted(metered, llm.BudgetOptions{
		MaxCostMicros:    processBudgetMicros,
		Prices:           prices,
		Model:            names.coordinator,
		DefaultMaxTokens: maxReplyTokens,
		Logger:           logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("budget: %w", err)
	}
	return app.AgentModel(budgeted, app.AgentModelOptions{
		Prices:           prices,
		DefaultMaxTokens: maxReplyTokens,
		Model:            names.coordinator,
		Logger:           logger,
	}), metered, nil
}
