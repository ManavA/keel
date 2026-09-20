package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/httpx"
	"github.com/ManavA/keel/webhooks"
)

// maxBodyBytes caps a delivery body. Without it a client can make the server
// allocate whatever it likes.
const maxBodyBytes = 64 << 10

// Delivery is one stored webhook delivery.
type Delivery struct {
	ID         string    `json:"id"`
	Topic      string    `json:"topic"`
	ReceivedAt time.Time `json:"received_at"`
}

// Receiver verifies deliveries and stores them. The secret signs every
// delivery and never leaves this value: it is compared, never returned.
type Receiver struct {
	pool   *pgxpool.Pool
	secret string
}

func NewReceiver(pool *pgxpool.Pool, secret string) *Receiver {
	return &Receiver{pool: pool, secret: secret}
}

// Routes registers the receiver. One route: a sender POSTs an event, the
// receiver answers 202 once it is stored.
func (rc *Receiver) Routes(r chi.Router) {
	r.Post("/hooks/events", rc.receive)
}

func (rc *Receiver) receive(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		httpx.BadRequest(w, r, err)
		return
	}

	// Verified before parsed: an unsigned body is refused whatever it says,
	// and the topic is required before the signature, so a signed body for
	// no topic cannot be stored under an empty one.
	topic := strings.TrimSpace(r.Header.Get(webhooks.TopicHeader))
	if topic == "" {
		httpx.BadRequest(w, r, fmt.Errorf("missing %s", webhooks.TopicHeader))
		return
	}
	if !webhooks.Verify(rc.secret, body, r.Header.Get(webhooks.SignatureHeader)) {
		httpx.Unauthorized(w, r)
		return
	}
	if !json.Valid(body) {
		httpx.BadRequest(w, r, fmt.Errorf("delivery body is not JSON"))
		return
	}

	delivery, err := rc.store(r.Context(), topic, body)
	if err != nil {
		httpx.InternalError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, delivery)
}

// store inserts the delivery and returns it as stored, with the id and
// timestamp the database assigned rather than ones guessed here.
func (rc *Receiver) store(ctx context.Context, topic string, payload []byte) (Delivery, error) {
	var delivery Delivery
	err := rc.pool.QueryRow(ctx, `
		INSERT INTO webhook_deliveries (topic, payload)
		VALUES ($1, $2::jsonb)
		RETURNING id, topic, received_at
	`, topic, string(payload)).Scan(&delivery.ID, &delivery.Topic, &delivery.ReceivedAt)
	if err != nil {
		return Delivery{}, fmt.Errorf("insert delivery: %w", err)
	}
	return delivery, nil
}
