package jobs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNoopTrigger_Run_NeverErrors(t *testing.T) {
	var trig NoopTrigger
	assert.NoError(t, trig.Run(context.Background(), "anything"))
}
