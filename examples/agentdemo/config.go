package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ManavA/keel/app"
)

// The providers LLM_PROVIDER names.
const (
	providerScripted  = "scripted"
	providerAnthropic = "anthropic"
	providerOpenAI    = "openai"
)

// Config is everything this service reads from its environment.
//
// The lifecycle-owned settings, PORT, DATABASE_URL and the rest, come from the
// embedded app.Config. DATABASE_URL and OPERATOR_TOKEN are required; with
// nothing else set the model is scripted and the service needs only Postgres.
type Config struct {
	app.Config

	// Convenient for one instance and wrong for several: there is no advisory
	// lock, so two instances starting at once can both reach the same pending
	// file and one will fail. Run migrations as their own step beyond one
	// replica.
	MigrateOnStart bool `envconfig:"MIGRATE_ON_START" default:"true"`

	// The bearer token every route but the health checks asks for. It stands
	// in for an operator login, which is what a real service mounts instead.
	OperatorToken string `envconfig:"OPERATOR_TOKEN"`

	// scripted, anthropic or openai. Empty picks anthropic when
	// ANTHROPIC_API_KEY is set and scripted otherwise, so a laptop with no
	// key runs the whole demo.
	LLMProvider string `envconfig:"LLM_PROVIDER"`

	AnthropicAPIKey  string `envconfig:"ANTHROPIC_API_KEY"`
	CoordinatorModel string `envconfig:"COORDINATOR_MODEL" default:"claude-opus-5-5"`
	ReviewerModel    string `envconfig:"REVIEWER_MODEL" default:"claude-haiku-4-5-20251001"`

	// An OpenAI-compatible server: OpenAI itself, or a local runtime, which
	// wants a base URL and a model and no key.
	OpenAIBaseURL string `envconfig:"OPENAI_BASE_URL"`
	OpenAIAPIKey  string `envconfig:"OPENAI_API_KEY"`
	OpenAIModel   string `envconfig:"OPENAI_MODEL"`

	// How long a run whose process died waits to be taken over. Short here,
	// so a killed run resumes within the time it takes to say so; a real
	// service sizes it as docs/agents.md says.
	LeaseTTL time.Duration `envconfig:"LEASE_TTL" default:"5s"`

	// How often the worker looks for a run when it found none.
	PollInterval time.Duration `envconfig:"POLL_INTERVAL" default:"1s"`

	// Slept by every tool, so there is a run in flight to kill.
	StepDelay time.Duration `envconfig:"STEP_DELAY" default:"750ms"`

	// The cost limit of one run, in millionths of a US dollar. Zero is none.
	RunMaxCostMicros int64 `envconfig:"RUN_MAX_COST_MICROS" default:"0"`
}

// Provider is the provider this configuration selects, after the default.
func (c Config) Provider() string {
	p := strings.ToLower(strings.TrimSpace(c.LLMProvider))
	if p != "" {
		return p
	}
	if strings.TrimSpace(c.AnthropicAPIKey) != "" {
		return providerAnthropic
	}
	return providerScripted
}

// Validate is called by config.Load, so a configuration this build cannot
// honour stops the process at startup rather than at the first run.
func (c *Config) Validate() error {
	if err := c.Config.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.OperatorToken) == "" {
		return errors.New("OPERATOR_TOKEN must not be empty: it guards the runs and the approvals")
	}
	switch c.Provider() {
	case providerScripted:
	case providerAnthropic:
		if strings.TrimSpace(c.AnthropicAPIKey) == "" {
			return errors.New("LLM_PROVIDER=anthropic needs ANTHROPIC_API_KEY")
		}
		if strings.TrimSpace(c.CoordinatorModel) == "" || strings.TrimSpace(c.ReviewerModel) == "" {
			return errors.New("COORDINATOR_MODEL and REVIEWER_MODEL must not be empty")
		}
	case providerOpenAI:
		if strings.TrimSpace(c.OpenAIModel) == "" {
			return errors.New("LLM_PROVIDER=openai needs OPENAI_MODEL: the protocol has no model that is right for every server")
		}
	default:
		return fmt.Errorf("LLM_PROVIDER must be scripted, anthropic or openai, got %q", c.LLMProvider)
	}
	if c.LeaseTTL <= 0 {
		return fmt.Errorf("LEASE_TTL must be positive, got %s", c.LeaseTTL)
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("POLL_INTERVAL must be positive, got %s", c.PollInterval)
	}
	if c.StepDelay < 0 {
		return fmt.Errorf("STEP_DELAY must not be negative, got %s", c.StepDelay)
	}
	if c.RunMaxCostMicros < 0 {
		return fmt.Errorf("RUN_MAX_COST_MICROS must not be negative, got %d", c.RunMaxCostMicros)
	}
	return nil
}
