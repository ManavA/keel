package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	keellog "github.com/ManavA/keel/log"
	"github.com/ManavA/keel/pg"
	"github.com/ManavA/keel/pg/migrate"
	"github.com/ManavA/keel/pg/testdb"
)

// An example nobody runs stops working quietly. These tests exercise the job
// the way run() does, against a real database, so a signature drifting in
// one of the packages it wires shows up as a failure rather than a stale file.

func TestMain(m *testing.M) {
	slog.SetDefault(keellog.New(keellog.Options{Output: io.Discard}))
	os.Exit(testdb.RunMain(m, testdb.Options{}))
}

// openTestPool migrates an isolated schema and returns a pool on it, so tests
// do not collide. It mirrors what run() applies: the same embedded files.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	db := testdb.Shared(t)
	ctx := context.Background()

	admin, err := pg.Open(ctx, pg.Options{URL: db.URL})
	require.NoError(t, err)
	schema := "worker_example_" + randomSuffix(t)
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	admin.Close()

	pool, err := pg.Open(ctx, pg.Options{URL: db.URL + "&search_path=" + schema})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = migrate.Run(ctx, pool, migrate.Options{FS: migrationFiles, Dir: "migrations"})
	require.NoError(t, err)
	return pool
}

// TestHeartbeatWritesARow proves the job's liveness claim: one run leaves one
// row carrying the job's name.
func TestHeartbeatWritesARow(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	outcome, err := runHeartbeat(ctx, pool, "heartbeat-test")
	require.NoError(t, err)
	assert.Equal(t, 1, outcome.Attempted)
	assert.Equal(t, 1, outcome.Succeeded)
	assert.Equal(t, 0, outcome.Failed)

	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM worker_heartbeats WHERE job = $1`, "heartbeat-test").Scan(&count))
	assert.Equal(t, 1, count)
}

// TestHeartbeatFailureCountsTheAttempt proves a failed write is reported as
// failed work, not as a bare error with nothing counted: the caller can see
// the attempt happened and did not land.
func TestHeartbeatFailureCountsTheAttempt(t *testing.T) {
	pool := openTestPool(t)

	// Against a table that does not exist, the write fails and the outcome
	// says one attempt, one failure.
	_, err := pool.Exec(context.Background(), `DROP TABLE worker_heartbeats`)
	require.NoError(t, err)

	outcome, err := runHeartbeat(context.Background(), pool, "heartbeat-test")
	require.Error(t, err)
	assert.Equal(t, 1, outcome.Attempted)
	assert.Equal(t, 1, outcome.Failed)
	assert.Equal(t, 0, outcome.Succeeded)
}

func TestConfigValidation(t *testing.T) {
	good := Config{DatabaseURL: "postgres://example/db", HeartbeatInterval: time.Minute, HeartbeatJob: "heartbeat"}
	assert.NoError(t, good.Validate())

	emptyURL := good
	emptyURL.DatabaseURL = "  "
	assert.ErrorContains(t, emptyURL.Validate(), "DATABASE_URL")

	noInterval := good
	noInterval.HeartbeatInterval = 0
	assert.ErrorContains(t, noInterval.Validate(), "HEARTBEAT_INTERVAL")

	emptyJob := good
	emptyJob.HeartbeatJob = ""
	assert.ErrorContains(t, emptyJob.Validate(), "HEARTBEAT_JOB")
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [6]byte
	_, err := rand.Read(b[:])
	require.NoError(t, err)
	return hex.EncodeToString(b[:])
}
