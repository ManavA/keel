package testdb

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
)

// EnvRequire names the environment variable that turns "Docker is unavailable"
// from a skip into a failure. CI sets it; a laptop does not.
//
// A suite that skips itself looks the same as a suite that passes in most CI
// summaries, so a database-backed assertion can stop running unnoticed.
const EnvRequire = "KEEL_REQUIRE_DB"

// Required reports whether a missing harness must fail rather than skip.
func Required() bool {
	v := os.Getenv(EnvRequire)
	return v != "" && v != "0" && v != "false"
}

// New starts a database for one test and removes it when the test ends. It
// skips when Docker is unavailable, or fails if KEEL_REQUIRE_DB is set.
//
// Starting a container takes a couple of seconds, so prefer RunMain and Shared
// for a package with more than a handful of tests.
func New(t testing.TB, opts Options) *DB {
	t.Helper()

	if err := Available(); err != nil {
		if Required() {
			t.Fatalf("%s is set and the harness cannot run: %v", EnvRequire, err)
		}
		t.Skipf("skipping: %v (set %s=1 to make this a failure)", err, EnvRequire)
	}

	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	db, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("start test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

var shared struct {
	db   *DB
	uses atomic.Int64
}

// RunMain starts one database for a whole package, runs its tests against it,
// and removes the container afterwards. Use it from TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testdb.RunMain(m, testdb.Options{})) }
//
// It fails a run in which every test passed and none asked for the database,
// which is what a package looks like after a build tag, a changed skip
// condition or a renamed environment variable stops the tests running.
//
// The testing package does not expose a test count, so what this counts is
// calls to Shared. A test that starts its own database with New, or needs none,
// is invisible to it.
func RunMain(m *testing.M, opts Options) int {
	if err := Available(); err != nil {
		if Required() {
			fmt.Fprintf(os.Stderr, "%s is set and the harness cannot run: %v\n", EnvRequire, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "testdb: %v — database-backed tests will skip\n", err)
		return m.Run()
	}

	db, err := Start(context.Background(), opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: %v\n", err)
		return 1
	}
	shared.db = db

	code := m.Run()
	db.Close()

	if code == 0 && shared.uses.Load() == 0 {
		fmt.Fprintf(os.Stderr,
			"testdb: FAILED — this package started a database and no test used "+
				"it, so nothing that needs one was verified.\n")
		return 1
	}
	return code
}

// Shared returns the database RunMain started and records that this test used
// it. It skips, or fails under KEEL_REQUIRE_DB, when there is none.
//
// Every caller gets the same database, so tests that write need their own
// schema, their own table names, or a transaction rolled back at the end.
func Shared(t testing.TB) *DB {
	t.Helper()

	if shared.db == nil {
		if Required() {
			t.Fatalf("%s is set but no database is running; call testdb.RunMain from TestMain", EnvRequire)
		}
		t.Skipf("skipping: no test database (set %s=1 to make this a failure)", EnvRequire)
	}
	shared.uses.Add(1)
	return shared.db
}
