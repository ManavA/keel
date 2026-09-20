package outbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ManavA/keel/events"
)

// Table is the name of the table outbox/pg/migrations creates.
const Table = "outbox_events"

// Event is what Enqueue writes to the outbox.
type Event struct {
	// Topic is passed to events.Publisher.Publish when Relay delivers this
	// event.
	Topic string

	// Payload is marshaled with events.Marshal: a []byte value is stored
	// as-is, anything else is JSON-encoded.
	Payload any

	// PartitionKey optionally groups rows whose delivery order matters,
	// typically one aggregate's id. Empty means no ordering: the row
	// publishes independently of every other row, as before. A non-empty
	// key takes effect only when the Relay runs with
	// Options.OrderedPartitions; otherwise it is stored and ignored.
	PartitionKey string
}

// Enqueue writes event as a new outbox row inside tx, so it commits or
// rolls back with whatever domain change tx also makes. It does not
// publish anything; see [Relay].
func Enqueue(ctx context.Context, tx pgx.Tx, event Event) error {
	if event.Topic == "" {
		return errors.New("outbox: Event.Topic is empty")
	}

	payload, err := events.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("outbox: marshal payload for topic %s: %w", event.Topic, err)
	}

	_, err = tx.Exec(ctx,
		"insert into "+Table+" (topic, payload, partition_key) values ($1, $2, $3)",
		event.Topic, payload, event.PartitionKey)
	if err != nil {
		return fmt.Errorf("outbox: enqueue for topic %s: %w", event.Topic, err)
	}
	return nil
}
