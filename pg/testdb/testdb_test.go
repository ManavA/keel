package testdb_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ManavA/keel/pg/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartGivesAUsableDatabase(t *testing.T) {
	db := testdb.New(t, testdb.Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, db.URL)
	require.NoError(t, err, "the URL Start returned must be the one it proved ready")
	defer func() { _ = conn.Close(ctx) }()

	var answer int
	require.NoError(t, conn.QueryRow(ctx, "select 42").Scan(&answer))
	assert.Equal(t, 42, answer)

	assert.Contains(t, db.URL, "127.0.0.1",
		"the URL is pinned to a literal address so no resolver can send the probe and the tests to different sockets")
}

func TestStartPicksAFreePortPerContainer(t *testing.T) {
	first := testdb.New(t, testdb.Options{})
	second := testdb.New(t, testdb.Options{})

	assert.NotEqual(t, first.Port, second.Port)
	assert.NotEqual(t, first.Name, second.Name,
		"two suites running at once must not reach for the same container name")
}

func TestCloseRemovesTheContainer(t *testing.T) {
	if err := testdb.Available(); err != nil {
		t.Skipf("skipping: %v", err)
	}

	db, err := testdb.Start(context.Background(), testdb.Options{Logf: t.Logf})
	require.NoError(t, err)

	db.Close()
	db.Close() // must not panic or report a second removal

	assert.False(t, containerExists(t, db.Name))
}

func TestReadyRequiresHostTCPNotDockerExec(t *testing.T) {
	// This container publishes no port, so `docker exec psql` inside it reports
	// a working database while the connection string the tests use reaches
	// nothing. The probe must time out and name the query as what failed.
	if err := testdb.Available(); err != nil {
		t.Skipf("skipping: %v", err)
	}

	name := "keel-testdb-unpublished-" + strings.ReplaceAll(t.Name(), "/", "-")
	_ = exec.Command("docker", "rm", "--force", name).Run()
	out, err := exec.Command("docker", "run", "--detach", "--rm", "--name", name,
		"--env", "POSTGRES_USER=keel",
		"--env", "POSTGRES_PASSWORD=keel",
		"--env", "POSTGRES_DB=keel",
		testdb.DefaultImage).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", name).Run() })

	// Wait until the database really is up inside the container, so that the
	// failure below is about reachability and not about timing.
	require.Eventually(t, func() bool {
		return exec.Command("docker", "exec", name, "pg_isready", "-U", "keel").Run() == nil
	}, 90*time.Second, time.Second, "the container never became ready even from the inside")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	err = testdb.Ready(ctx, unpublished{name: name}, time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, testdb.ErrNotAccepting)
}

// unpublished probes a port nothing is listening on, which is what a container
// started without --publish looks like from the host.
type unpublished struct{ name string }

func (u unpublished) Accepting(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, "postgres://keel:keel@127.0.0.1:1/keel?sslmode=disable")
	if err != nil {
		return err
	}
	return conn.Close(ctx)
}

func (u unpublished) StartTime(context.Context) (time.Time, error) {
	return time.Time{}, assertUnreachable
}

var assertUnreachable = errUnreachable{}

type errUnreachable struct{}

func (errUnreachable) Error() string { return "unreachable" }

func containerExists(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "name=^"+name+"$").Output()
	require.NoError(t, err)
	return len(strings.TrimSpace(string(out))) > 0
}

func TestRequired(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"1", true},
		{"yes", true},
	}
	for _, tt := range tests {
		t.Run("KEEL_REQUIRE_DB="+tt.value, func(t *testing.T) {
			t.Setenv(testdb.EnvRequire, tt.value)
			assert.Equal(t, tt.want, testdb.Required())
		})
	}
}

func TestStartPublishesOnLoopbackOnly(t *testing.T) {
	// The package doc's claim is about the container's binding, and the URL
	// string says nothing about it: publishing on 0.0.0.0 would leave db.URL
	// reading 127.0.0.1 while the database is reachable from the network.
	db := testdb.New(t, testdb.Options{})

	out, err := exec.Command("docker", "port", db.Name, "5432").Output()
	require.NoError(t, err)

	bindings := strings.Fields(strings.TrimSpace(string(out)))
	require.NotEmpty(t, bindings, "the container publishes nothing on 5432")
	for _, binding := range bindings {
		assert.True(t, strings.HasPrefix(binding, "127.0.0.1:"),
			"published on %q, which is reachable from outside this machine", binding)
	}
}
