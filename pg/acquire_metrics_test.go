package pg_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/metrics"
	"github.com/ManavA/keel/pg"
)

// TestAcquire_RecordsWait pins the metrics hook: one acquisition counts
// once and records how long the caller waited for a connection.
func TestAcquire_RecordsWait(t *testing.T) {
	pool := openPool(t)
	mem := metrics.NewInMemory()

	conn, err := pg.Acquire(context.Background(), pool, mem.Metrics())
	require.NoError(t, err)
	conn.Release()

	assert.Equal(t, int64(1), mem.CounterTotal(metrics.NamePoolAcquires))
	wait := mem.HistogramValues(metrics.NamePoolAcquireDur)
	require.Len(t, wait, 1)
	assert.GreaterOrEqual(t, wait[0], 0.0)
}
