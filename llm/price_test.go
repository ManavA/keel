package llm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
)

// contractPrices is a made-up table: $3 and $15 per million tokens for one
// model, a quarter of a dollar per million for another, so a single token of
// the second costs a fraction of a millionth of a dollar and must round up.
func contractPrices() llm.Prices {
	return llm.Prices{
		"model-a": {Input: 3_000_000, Output: 15_000_000, CacheRead: 300_000, CacheWrite: 3_750_000},
		"model-b": {Input: 250_000, Output: 1_250_000},
	}
}

func TestPrices_Cost(t *testing.T) {
	tests := []struct {
		name  string
		model string
		usage llm.Usage
		want  int64
	}{
		{
			name:  "a round figure: a million tokens each way is $18",
			model: "model-a",
			usage: llm.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			want:  18_000_000,
		},
		{
			name:  "a thousand input tokens",
			model: "model-a",
			usage: llm.Usage{InputTokens: 1_000},
			want:  3_000,
		},
		{
			name:  "cache reads and writes at their own prices",
			model: "model-a",
			usage: llm.Usage{CacheReadTokens: 10_000, CacheWriteTokens: 1_000},
			want:  3_000 + 3_750,
		},
		{
			name:  "reasoning tokens are inside output and are not priced again",
			model: "model-a",
			usage: llm.Usage{OutputTokens: 100, ReasoningTokens: 80},
			want:  1_500,
		},
		{
			name:  "a quarter of a millionth of a dollar rounds up to one",
			model: "model-b",
			usage: llm.Usage{InputTokens: 1},
			want:  1,
		},
		{
			name:  "exactly one millionth is not rounded past it",
			model: "model-b",
			usage: llm.Usage{InputTokens: 4},
			want:  1,
		},
		{
			name:  "one and a quarter rounds up to two",
			model: "model-b",
			usage: llm.Usage{InputTokens: 5},
			want:  2,
		},
		{
			name:  "no tokens cost nothing",
			model: "model-b",
			usage: llm.Usage{},
			want:  0,
		},
	}

	prices := contractPrices()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := prices.Cost(tt.model, tt.usage)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestPrices_CostOfAnUnlistedModel(t *testing.T) {
	tests := []struct {
		name   string
		prices llm.Prices
	}{
		{name: "a table that lacks the model", prices: contractPrices()},
		{name: "a nil table", prices: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.prices.Cost("model-z", llm.Usage{InputTokens: 1_000})

			require.ErrorIs(t, err, llm.ErrNoPrice)
			assert.Contains(t, err.Error(), "model-z", "the error names the model that has no price")
			assert.Zero(t, got)
		})
	}
}

func TestPrices_CostOf(t *testing.T) {
	prices := contractPrices()

	t.Run("with no attempts it is the cost of the one model and usage", func(t *testing.T) {
		got, err := prices.CostOf(&llm.Response{
			Model: "model-a",
			Usage: llm.Usage{InputTokens: 1_000, OutputTokens: 100},
		})

		require.NoError(t, err)
		assert.Equal(t, int64(3_000+1_500), got)
	})

	t.Run("with two attempts it is their sum, and Model and Usage are not added again", func(t *testing.T) {
		got, err := prices.CostOf(&llm.Response{
			Model: "model-b",
			Usage: llm.Usage{InputTokens: 4_000},
			Attempts: []llm.Attempt{
				{Model: "model-a", Usage: llm.Usage{InputTokens: 1_000}},
				{Model: "model-b", Usage: llm.Usage{InputTokens: 4_000}},
			},
		})

		require.NoError(t, err)
		assert.Equal(t, int64(3_000+1_000), got)
	})

	t.Run("each attempt is rounded up on its own", func(t *testing.T) {
		got, err := prices.CostOf(&llm.Response{
			Model: "model-b",
			Attempts: []llm.Attempt{
				{Model: "model-b", Usage: llm.Usage{InputTokens: 1}},
				{Model: "model-b", Usage: llm.Usage{InputTokens: 1}},
			},
		})

		require.NoError(t, err)
		assert.Equal(t, int64(2), got)
	})

	t.Run("an attempt by an unlisted model is ErrNoPrice, not a partial sum", func(t *testing.T) {
		got, err := prices.CostOf(&llm.Response{
			Model: "model-a",
			Attempts: []llm.Attempt{
				{Model: "model-a", Usage: llm.Usage{InputTokens: 1_000}},
				{Model: "model-z", Usage: llm.Usage{InputTokens: 1_000}},
			},
		})

		require.ErrorIs(t, err, llm.ErrNoPrice)
		assert.Zero(t, got)
	})

	t.Run("no attempts and an unlisted model is ErrNoPrice", func(t *testing.T) {
		_, err := prices.CostOf(&llm.Response{Model: "model-z", Usage: llm.Usage{InputTokens: 1}})

		require.ErrorIs(t, err, llm.ErrNoPrice)
	})
}
