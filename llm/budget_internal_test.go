package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPriceReply(t *testing.T) {
	prices := Prices{
		"model-a": {Input: 2_000_000, Output: 10_000_000},
		"model-b": {Input: 500_000, Output: 2_500_000},
	}
	used := Usage{InputTokens: 1000, OutputTokens: 100}
	costAt := func(model string, u Usage) int64 {
		c, err := prices.Cost(model, u)
		require.NoError(t, err)
		return c
	}
	tests := []struct {
		name     string
		resp     *Response
		asked    string
		wantCost int64
		wantOK   bool
	}{
		{
			name:     "the model the reply names, when the table lists it",
			resp:     &Response{Model: "model-b", Usage: used},
			asked:    "model-a",
			wantCost: costAt("model-b", used),
			wantOK:   true,
		},
		{
			name:     "the model asked for, when the table does not list the one the reply names",
			resp:     &Response{Model: "model-a-20251001", Usage: used},
			asked:    "model-a",
			wantCost: costAt("model-a", used),
			wantOK:   true,
		},
		{
			name:     "the model asked for, when the reply names none",
			resp:     &Response{Usage: used},
			asked:    "model-b",
			wantCost: costAt("model-b", used),
			wantOK:   true,
		},
		{
			name:   "neither listed",
			resp:   &Response{Model: "model-z", Usage: used},
			asked:  "model-y",
			wantOK: false,
		},
		{
			name:   "the reply names none and none was asked for",
			resp:   &Response{Usage: used},
			wantOK: false,
		},
		{
			name:     "each attempt by its own model, and by the model asked for where its own is not listed",
			resp:     &Response{Model: "model-b", Attempts: []Attempt{{Model: "model-b", Usage: used}, {Model: "model-a-20251001", Usage: used}}},
			asked:    "model-a",
			wantCost: costAt("model-b", used) + costAt("model-a", used),
			wantOK:   true,
		},
		{
			name:   "one attempt that nothing can price leaves the whole reply unpriced",
			resp:   &Response{Model: "model-b", Attempts: []Attempt{{Model: "model-b", Usage: used}, {Model: "model-z", Usage: used}}},
			asked:  "model-y",
			wantOK: false,
		},
		{
			name:     "a reply with no usage costs nothing and is priced",
			resp:     &Response{Model: "model-a"},
			asked:    "model-a",
			wantCost: 0,
			wantOK:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cost, ok := priceReply(prices, tt.resp, tt.asked)

			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantCost, cost)
		})
	}
	t.Run("no table prices nothing", func(t *testing.T) {
		_, ok := priceReply(nil, &Response{Model: "model-a", Usage: used}, "model-a")
		assert.False(t, ok)
	})
}

// With a cost limit, admission has already priced the request's model, so a
// reply that nothing can price is not reachable through the public calls. The
// last step is still defined: the call is charged the cost that was held for it.
func TestBudgeted_settleChargesTheHeldCostWhenNothingCanPriceTheReply(t *testing.T) {
	b, err := NewBudgeted(nil, BudgetOptions{
		MaxCostMicros: 1 << 40,
		Prices:        Prices{"model-a": {Input: 2_000_000, Output: 10_000_000}},
	})
	require.NoError(t, err)
	h := hold{tokens: 50, cost: 7, model: "model-y"}
	b.held = hold{tokens: h.tokens, cost: h.cost}

	b.settle(h, &Response{Model: "model-z", Usage: Usage{InputTokens: 10, OutputTokens: 5}})

	assert.Equal(t, Spend{Calls: 1, Tokens: 15, CostMicros: 7}, b.Spent())
	assert.Equal(t, hold{}, b.held, "and the hold is given back")
}
