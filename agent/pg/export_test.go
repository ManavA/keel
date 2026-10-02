package pg

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/agent"
	keelpg "github.com/ManavA/keel/pg"
)

// ParkWithoutLock is Park with one thing taken out: it reads the run's hold
// without locking the run's row. Everything else is Park's own code and
// Park's own statements. It exists for the test that shows the lock is what
// keeps a run from being stranded.
func ParkWithoutLock(ctx context.Context, db keelpg.Beginner, lease agent.Lease, req agent.ParkRequest) (bool, error) {
	var parked bool
	err := New(db).inTx(ctx, "park without lock", readCommitted, func(tx pgx.Tx) error {
		run, err := held(ctx, tx, lease, strings.TrimSuffix(lockRunSQL, " for update"))
		if err != nil {
			return err
		}
		parked, err = park(ctx, tx, lease.RunID, run, req)
		return err
	})
	return parked, err
}

// The encoders, for the tests that hold them to encoding/json.
var (
	EncodeCall     = encodeCall
	EncodeMessage  = encodeMessage
	EncodeSnapshot = encodeSnapshot
)
