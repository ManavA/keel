package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/outbox"
	keelpg "github.com/ManavA/keel/pg"
)

// Note is one row of the notes table.
type Note struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrNoteNotFound is returned for a note that does not exist. The HTTP layer
// answers it with 404, and answers a note belonging to somebody else the same
// way — a 403 would confirm the row exists, which is the fact the request was
// trying to establish.
var ErrNoteNotFound = errors.New("note not found")

// Notes is the notes table.
type Notes struct {
	pool *pgxpool.Pool
}

func NewNotes(pool *pgxpool.Pool) *Notes { return &Notes{pool: pool} }

// CreateWithEvent inserts a note owned by userID and enqueues its
// note.created event in the same transaction, so the row and the event
// commit or roll back together. The relay publishes the event afterward;
// nothing about the request waits for that.
func (n *Notes) CreateWithEvent(ctx context.Context, userID, title, body string) (Note, error) {
	var note Note
	err := keelpg.InTx(ctx, n.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO notes (user_id, title, body)
			VALUES ($1, $2, $3)
			RETURNING id, user_id, title, body, created_at
		`, userID, title, body).Scan(&note.ID, &note.UserID, &note.Title, &note.Body, &note.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert note: %w", err)
		}
		return outbox.Enqueue(ctx, tx, outbox.Event{
			Topic: topicNoteCreated,
			Payload: map[string]any{
				"id":      note.ID,
				"user_id": note.UserID,
				"title":   note.Title,
			},
		})
	})
	if err != nil {
		return Note{}, err
	}
	return note, nil
}

// Get reads one note of userID's own.
func (n *Notes) Get(ctx context.Context, userID, id string) (Note, error) {
	var note Note
	err := n.pool.QueryRow(ctx, `
		SELECT id, user_id, title, body, created_at FROM notes WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(&note.ID, &note.UserID, &note.Title, &note.Body, &note.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Note{}, ErrNoteNotFound
	case err != nil:
		return Note{}, fmt.Errorf("select note: %w", err)
	}
	return note, nil
}
