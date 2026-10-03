package llm_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ManavA/keel/llm"
)

func TestResponse_BilledUsage(t *testing.T) {
	tests := []struct {
		name string
		resp llm.Response
		want llm.Usage
	}{
		{"no attempts is the one usage", llm.Response{Usage: llm.Usage{InputTokens: 3, OutputTokens: 4}}, llm.Usage{InputTokens: 3, OutputTokens: 4}},
		{"attempts replace the usage", llm.Response{
			Usage: llm.Usage{InputTokens: 100},
			Attempts: []llm.Attempt{
				{Model: "a", Usage: llm.Usage{InputTokens: 1, CacheReadTokens: 5}},
				{Model: "b", Usage: llm.Usage{InputTokens: 2, OutputTokens: 7}},
			},
		}, llm.Usage{InputTokens: 3, OutputTokens: 7, CacheReadTokens: 5}},
		{"nothing at all is zero", llm.Response{}, llm.Usage{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.resp.BilledUsage())
		})
	}
}
