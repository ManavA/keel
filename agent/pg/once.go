package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/agent/internal/storerule"
)

const onceSQL = `insert into ` + EffectsTable + ` (key) values ($1) on conflict (key) do nothing`

// Once records key in tx and reports whether this is the first time it has
// been recorded. A tool calls it with Invocation.Key inside the transaction
// that makes its change, and skips the change when it reports false.
//
// The key and the change commit together or not at all, so a call that was
// interrupted and is made again finds the key exactly when its change is
// already there. Two attempts at once are put in order by the key's unique
// index: the second waits for the first to commit, and is then told false,
// or for it to be rolled back, and is then the first.
//
// That is for a transaction at read committed, which is what a pool gives. A
// transaction at repeatable read or above that began before another attempt
// recorded the key is not told false: Postgres ends the statement with a
// serialization failure, SQLSTATE 40001, and Once returns it. The effect is
// still not made twice, since the transaction that would have made it is
// over. Such a caller does what it does for any serialization failure, and
// runs its transaction again, in which Once reports false.
//
// A key that holds a NUL character or a byte that is not UTF-8 is refused: a
// key is compared, and cannot be kept as another.
func Once(ctx context.Context, tx pgx.Tx, key string) (first bool, err error) {
	if !storerule.Comparable(key) {
		return false, fmt.Errorf("agent/pg: once: key %q holds a character no column keeps", key)
	}
	tag, err := tx.Exec(ctx, onceSQL, key)
	if err != nil {
		return false, fmt.Errorf("agent/pg: once: record %q: %w", key, err)
	}
	return tag.RowsAffected() == 1, nil
}
