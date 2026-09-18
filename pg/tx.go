package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Beginner is anything that can start a transaction: a *pgxpool.Pool, a
// *pgx.Conn, or a pgx.Tx for a nested one. Small, so a repository can take it
// instead of a concrete pool.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// InTx runs fn inside a transaction, committing when it returns nil and rolling
// back otherwise. It handles three things that go wrong when this is written by
// hand at each call site.
//
// A deferred Rollback after a successful Commit returns pgx.ErrTxClosed, so a
// call site that checks it reports a failure on a transaction that committed.
//
// A panic between Begin and Commit leaves the transaction open until the
// connection is returned to the pool and reset, possibly still holding a lock.
// The panic is re-raised, so behaviour above InTx is unchanged.
//
// A rollback that fails after fn already failed is joined to fn's error rather
// than replacing it.
func InTx(ctx context.Context, db Beginner, fn func(pgx.Tx) error) error {
	return InTxOptions(ctx, db, pgx.TxOptions{}, fn)
}

// InTxOptions is InTx with explicit transaction options — a different isolation
// level, or a read-only transaction.
func InTxOptions(ctx context.Context, db Beginner, opts pgx.TxOptions, fn func(pgx.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("pg: begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if p := recover(); p != nil {
			// The panic is what the caller will see; a rollback error on top of
			// it has nowhere useful to go.
			_ = rollback(ctx, tx)
			panic(p)
		}
		if committed {
			return
		}
		if rbErr := rollback(ctx, tx); rbErr != nil {
			err = errors.Join(err, rbErr)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: commit: %w", err)
	}
	committed = true
	return nil
}

// rollback reports a real failure and ignores the errors that mean the
// transaction is already over.
func rollback(ctx context.Context, tx pgx.Tx) error {
	// Without the cancellation: the usual reason fn failed is that ctx was
	// cancelled, and a rollback on a cancelled context does nothing.
	err := tx.Rollback(context.WithoutCancel(ctx))
	switch {
	case err == nil, errors.Is(err, pgx.ErrTxClosed):
		return nil
	default:
		return fmt.Errorf("pg: rollback: %w", err)
	}
}
