package anthropic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/llm"
	"github.com/ManavA/keel/llm/internal/sse"
)

// An event past its bound is told by the reader's own error and not by what
// the error is not: any other error from the reader is a failure of the call
// that may be asked again.
func TestEventFailure_OversizeIsToldByTheReadersError(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	err := c.eventFailure(ctx, "req", fmt.Errorf("reading: %w", sse.ErrEventTooLarge), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "larger than")
	assert.False(t, llm.Retryable(err))
	var e *llm.Error
	assert.NotErrorAs(t, err, &e)

	err = c.eventFailure(ctx, "req", errors.New("something else"), false)
	require.ErrorAs(t, err, &e)
	assert.True(t, e.Retryable)
	assert.NotContains(t, err.Error(), "larger than")
}
