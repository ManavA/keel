package pg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/agent"
	agentpg "github.com/ManavA/keel/agent/pg"
)

func TestOnce_TrueTheFirstTimeAndFalseAfter(t *testing.T) {
	pool := newDatabase(t).pool(t)
	ctx := t.Context()
	once := func(key string) bool {
		t.Helper()
		tx := begin(t, pool)
		first, err := agentpg.Once(ctx, tx, key)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
		return first
	}

	assert.True(t, once("run-1:2"), "the first time")
	assert.False(t, once("run-1:2"), "the second")
	assert.False(t, once("run-1:2"), "and every time after")
	assert.True(t, once("run-1:3"), "another key is another effect")

	// In one transaction, too: the second call sees the first.
	tx := begin(t, pool)
	first, err := agentpg.Once(ctx, tx, "run-1:4")
	require.NoError(t, err)
	assert.True(t, first)
	first, err = agentpg.Once(ctx, tx, "run-1:4")
	require.NoError(t, err)
	assert.False(t, first)
	require.NoError(t, tx.Commit(ctx))
}

// Two attempts of one call, each in the transaction that makes its change.
// The second waits for the first, and is told whether the first kept its
// key: so the change is made once when the first commits, and is still made
// when the first is rolled back.
func TestOnce_FromTwoTransactionsAtOnce(t *testing.T) {
	tests := []struct {
		name string
		// commit says how the first transaction ends.
		commit     bool
		wantSecond bool
	}{
		{"the first commits: one true and one false", true, false},
		{"the first is rolled back: its key was never recorded, and the second is the first", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := newDatabase(t).pool(t)
			ctx := t.Context()
			const key = "run-1:2"

			first := begin(t, pool)
			isFirst, err := agentpg.Once(ctx, first, key)
			require.NoError(t, err)
			require.True(t, isFirst)

			type answer struct {
				first bool
				err   error
			}
			second := make(chan answer, 1)
			go func() {
				tx, err := pool.Begin(ctx)
				if err != nil {
					second <- answer{err: err}
					return
				}
				isFirst, err := agentpg.Once(ctx, tx, key)
				if err != nil {
					_ = tx.Rollback(context.Background())
					second <- answer{err: err}
					return
				}
				second <- answer{first: isFirst, err: tx.Commit(ctx)}
			}()

			// The second attempt cannot know yet, and waits.
			waits(t, pool, second)
			if tt.commit {
				require.NoError(t, first.Commit(ctx))
			} else {
				require.NoError(t, first.Rollback(ctx))
			}

			got := result(t, second)
			require.NoError(t, got.err)
			assert.Equal(t, tt.wantSecond, got.first)

			var recorded int
			require.NoError(t, pool.QueryRow(ctx,
				"select count(*) from "+agentpg.EffectsTable+" where key = $1", key).Scan(&recorded))
			assert.Equal(t, 1, recorded, "the key is recorded once")
		})
	}
}

func TestOnce_ARolledBackTransactionLeavesTheKeyUnrecorded(t *testing.T) {
	pool := newDatabase(t).pool(t)
	ctx := t.Context()

	tx := begin(t, pool)
	first, err := agentpg.Once(ctx, tx, "run-1:2")
	require.NoError(t, err)
	require.True(t, first)
	require.NoError(t, tx.Rollback(ctx))

	var recorded int
	require.NoError(t, pool.QueryRow(ctx, "select count(*) from "+agentpg.EffectsTable).Scan(&recorded))
	assert.Zero(t, recorded)

	tx = begin(t, pool)
	first, err = agentpg.Once(ctx, tx, "run-1:2")
	require.NoError(t, err)
	assert.True(t, first, "the effect was undone with its key, so the next attempt makes it")
	require.NoError(t, tx.Commit(ctx))
}

// Once is for a transaction at read committed, which is what a pool gives. A
// caller that asked for repeatable read or above, and whose transaction
// began before another attempt recorded the key, is not told false:
// Postgres ends its statement with a serialization failure, as it does for
// any write that meets one it could not see. The effect is still not made
// twice. The caller's transaction is over, and tried again it is told false.
func TestOnce_UnderAStricterTransactionIsASerializationFailure(t *testing.T) {
	pool := newDatabase(t).pool(t)
	ctx := t.Context()

	for i, level := range []pgx.TxIsoLevel{pgx.RepeatableRead, pgx.Serializable} {
		// A key of its own for each level, that nothing has recorded.
		key := agent.StepKey("run", i+1)

		// The stricter transaction has begun, and looked at the database.
		strict, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
		require.NoError(t, err)
		t.Cleanup(func() { _ = strict.Rollback(context.Background()) })
		var one int
		require.NoError(t, strict.QueryRow(ctx, "select 1").Scan(&one))

		// Another attempt of the same call records the key and commits.
		other := begin(t, pool)
		_, err = agentpg.Once(ctx, other, key)
		require.NoError(t, err)
		require.NoError(t, other.Commit(ctx))

		first, err := agentpg.Once(ctx, strict, key)

		var failure *pgconn.PgError
		require.ErrorAs(t, err, &failure, "isolation %s", level)
		assert.Equal(t, "40001", failure.Code, "isolation %s", level)
		assert.False(t, first)
		require.NoError(t, strict.Rollback(ctx))

		// Tried again, the caller's transaction sees the key.
		again, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
		require.NoError(t, err)
		t.Cleanup(func() { _ = again.Rollback(context.Background()) })
		first, err = agentpg.Once(ctx, again, key)
		require.NoError(t, err, "isolation %s", level)
		assert.False(t, first, "isolation %s", level)
		require.NoError(t, again.Commit(ctx))
	}
}
