package llm_test

import (
	"strings"
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

func TestPrices_CostFor(t *testing.T) {
	prices := contractPrices()
	used := llm.Usage{InputTokens: 1_000, OutputTokens: 100}
	costAt := func(model string, u llm.Usage) int64 {
		c, err := prices.Cost(model, u)
		require.NoError(t, err)
		return c
	}

	tests := []struct {
		name  string
		resp  *llm.Response
		asked string
		want  int64
	}{
		{
			name:  "the model the reply names, when the table lists it",
			resp:  &llm.Response{Model: "model-b", Usage: used},
			asked: "model-a",
			want:  costAt("model-b", used),
		},
		{
			name:  "the model asked for, when a dated id is not in the table",
			resp:  &llm.Response{Model: "model-a-20251001", Usage: used},
			asked: "model-a",
			want:  costAt("model-a", used),
		},
		{
			name:  "the model asked for, when the reply names none",
			resp:  &llm.Response{Usage: used},
			asked: "model-b",
			want:  costAt("model-b", used),
		},
		{
			name: "each attempt at its own model, or at the model asked for where its own is not listed",
			resp: &llm.Response{
				Model: "model-b",
				Usage: used,
				Attempts: []llm.Attempt{
					{Model: "model-b", Usage: used},
					{Model: "model-a-20251001", Usage: used},
				},
			},
			asked: "model-a",
			want:  costAt("model-b", used) + costAt("model-a", used),
		},
		{
			name:  "a reply with no usage costs nothing",
			resp:  &llm.Response{Model: "model-a"},
			asked: "model-a",
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := prices.CostFor(tt.resp, tt.asked)

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("it agrees with CostOf where every name is listed", func(t *testing.T) {
		resp := &llm.Response{
			Model:    "model-b",
			Attempts: []llm.Attempt{{Model: "model-a", Usage: used}, {Model: "model-b", Usage: used}},
		}
		want, err := prices.CostOf(resp)
		require.NoError(t, err)

		got, err := prices.CostFor(resp, "model-z")

		require.NoError(t, err)
		assert.Equal(t, want, got)
	})
}

func TestPrices_CostForAReplyNothingCanPrice(t *testing.T) {
	used := llm.Usage{InputTokens: 1_000}
	tests := []struct {
		name   string
		prices llm.Prices
		resp   *llm.Response
		asked  string
		named  []string
	}{
		{
			name:   "neither name is listed",
			prices: contractPrices(),
			resp:   &llm.Response{Model: "model-z", Usage: used},
			asked:  "model-y",
			named:  []string{"model-z", "model-y"},
		},
		{
			name:   "the reply names none and none was asked for",
			prices: contractPrices(),
			resp:   &llm.Response{Usage: used},
			named:  []string{`""`},
		},
		{
			name:   "one attempt that neither name prices leaves the reply unpriced, not partly priced",
			prices: contractPrices(),
			resp: &llm.Response{
				Model: "model-a",
				Attempts: []llm.Attempt{
					{Model: "model-a", Usage: used},
					{Model: "model-z", Usage: used},
				},
			},
			asked: "model-y",
			named: []string{"model-a", "model-z", "model-y"},
		},
		{
			name:   "a nil table prices nothing",
			prices: nil,
			resp:   &llm.Response{Model: "model-a", Usage: used},
			asked:  "model-a",
			named:  []string{"model-a"},
		},
		{
			name:   "an empty table prices nothing",
			prices: llm.Prices{},
			resp:   &llm.Response{Model: "model-a", Usage: used},
			asked:  "model-a",
			named:  []string{"model-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.prices.CostFor(tt.resp, tt.asked)

			require.ErrorIs(t, err, llm.ErrNoPrice)
			assert.Zero(t, got, "a reply that cannot be priced is not one that costs nothing")
			for _, name := range tt.named {
				assert.Contains(t, err.Error(), name, "the error names what it tried")
			}
			assert.Equal(t, 1, strings.Count(err.Error(), "llm:"), "got %q", err.Error())
		})
	}

	t.Run("a model that several attempts share is named once", func(t *testing.T) {
		_, err := contractPrices().CostFor(&llm.Response{
			Model:    "model-z",
			Attempts: []llm.Attempt{{Model: "model-z", Usage: used}, {Model: "model-z", Usage: used}},
		}, "model-y")

		require.ErrorIs(t, err, llm.ErrNoPrice)
		assert.Equal(t, 1, strings.Count(err.Error(), "model-z"), "got %q", err.Error())
	})

	t.Run("a nil reply is an error and not a panic", func(t *testing.T) {
		got, err := contractPrices().CostFor(nil, "model-a")

		require.Error(t, err)
		assert.Zero(t, got)
	})
}
