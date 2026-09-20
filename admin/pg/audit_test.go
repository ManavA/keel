package pg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ManavA/keel/admin"
	adminpg "github.com/ManavA/keel/admin/pg"
)

// newAuditStore truncates the shared audit table so each test starts from
// an empty trail. Tests in this package do not run in parallel, so no test
// can observe another's rows.
func newAuditStore(t *testing.T) (*pgxpool.Pool, *adminpg.AuditStore) {
	t.Helper()
	pool := newTestPool(t)
	if _, err := pool.Exec(context.Background(), "TRUNCATE admin_audit"); err != nil {
		t.Fatalf("truncate audit table: %v", err)
	}
	return pool, adminpg.NewAuditStore(pool)
}

func TestAuditStoreAppendAndList(t *testing.T) {
	_, store := newAuditStore(t)
	ctx := context.Background()

	first := &admin.AuditEntry{Actor: "admin_1", Action: "user.disable", Target: "user_42", Outcome: admin.AuditOutcomeOK}
	require.NoError(t, store.Append(ctx, first))
	assert.NotZero(t, first.ID)
	assert.False(t, first.CreatedAt.IsZero())

	second := &admin.AuditEntry{Actor: "admin_1", Action: "user.enable", Target: "user_43", Outcome: admin.AuditOutcomeError}
	require.NoError(t, store.Append(ctx, second))

	entries, err := store.List(ctx, 50, 0)
	require.NoError(t, err)
	require.Len(t, entries, 2, "the trail keeps every appended row")
	assert.Equal(t, "user.disable", entries[0].Action)
	assert.Equal(t, "user_42", entries[0].Target)
	assert.Equal(t, admin.AuditOutcomeOK, entries[0].Outcome)
	assert.Equal(t, "admin_1", entries[0].Actor)
	assert.False(t, entries[0].CreatedAt.IsZero())
	assert.Less(t, entries[0].ID, entries[1].ID, "list is oldest first")
	assert.Equal(t, "user.enable", entries[1].Action)
}

func TestAuditStoreListPagination(t *testing.T) {
	_, store := newAuditStore(t)
	ctx := context.Background()

	for _, action := range []string{"a.one", "a.two", "a.three"} {
		require.NoError(t, store.Append(ctx, &admin.AuditEntry{
			Actor: "admin_1", Action: action, Outcome: admin.AuditOutcomeOK,
		}))
	}

	first, err := store.List(ctx, 2, 0)
	require.NoError(t, err)
	require.Len(t, first, 2)
	assert.Equal(t, "a.one", first[0].Action)
	assert.Equal(t, "a.two", first[1].Action)

	second, err := store.List(ctx, 2, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, "a.three", second[0].Action)

	empty, err := store.List(ctx, 2, 3)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestAuditStoreAppendInTransactionJoinsTheDomainChange builds the store
// over a tx: a rolled-back transaction leaves no audit row, a committed one
// does. That is how a handler records its audit entry in the same
// transaction as the domain change it describes.
func TestAuditStoreAppendInTransactionJoinsTheDomainChange(t *testing.T) {
	pool, store := newAuditStore(t)
	ctx := context.Background()

	rolledBack, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, adminpg.NewAuditStore(rolledBack).Append(ctx, &admin.AuditEntry{
		Actor: "admin_1", Action: "user.disable", Target: "user_42", Outcome: admin.AuditOutcomeOK,
	}))
	require.NoError(t, rolledBack.Rollback(ctx))

	entries, err := store.List(ctx, 50, 0)
	require.NoError(t, err)
	assert.Empty(t, entries, "a rolled-back transaction must leave no audit row")

	committed, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, adminpg.NewAuditStore(committed).Append(ctx, &admin.AuditEntry{
		Actor: "admin_1", Action: "user.disable", Target: "user_42", Outcome: admin.AuditOutcomeOK,
	}))
	require.NoError(t, committed.Commit(ctx))

	entries, err = store.List(ctx, 50, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "user_42", entries[0].Target)
}
