package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ManavA/keel/pg"
)

// Note is one row of the notes table.
type Note struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrNoteNotFound is returned for a note that does not exist. The HTTP layer
// answers it with 404, and answers a note belonging to somebody else the same
// way — a 403 would confirm the row exists, which is the fact the request was
// trying to establish.
var ErrNoteNotFound = errors.New("note not found")

// Notes is the notes table. It takes a pg.Beginner rather than a concrete pool
// so a caller can hand it a transaction, and so a test can hand it a fake.
type Notes struct {
	pool *pgxpool.Pool
}

func NewNotes(pool *pgxpool.Pool) *Notes { return &Notes{pool: pool} }

// Create inserts a note and returns it as stored, with the id and timestamp
// the database assigned rather than ones guessed here.
func (n *Notes) Create(ctx context.Context, title, body string) (Note, error) {
	var note Note
	err := n.pool.QueryRow(ctx, `
		INSERT INTO notes (title, body)
		VALUES ($1, $2)
		RETURNING id, title, body, created_at
	`, title, body).Scan(&note.ID, &note.Title, &note.Body, &note.CreatedAt)
	if err != nil {
		return Note{}, fmt.Errorf("insert note: %w", err)
	}
	return note, nil
}

// Get reads one note.
func (n *Notes) Get(ctx context.Context, id string) (Note, error) {
	var note Note
	err := n.pool.QueryRow(ctx, `
		SELECT id, title, body, created_at FROM notes WHERE id = $1
	`, id).Scan(&note.ID, &note.Title, &note.Body, &note.CreatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Note{}, ErrNoteNotFound
	case err != nil:
		return Note{}, fmt.Errorf("select note: %w", err)
	}
	return note, nil
}

// Delete removes one note, reporting ErrNoteNotFound when there was nothing to
// remove. A delete that silently succeeds against a missing row cannot be told
// apart from one that worked.
func (n *Notes) Delete(ctx context.Context, id string) error {
	tag, err := n.pool.Exec(ctx, `DELETE FROM notes WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNoteNotFound
	}
	return nil
}

// notesKeyset is the ordering every listing page uses. The id is in it because
// created_at is not unique, and a keyset whose last column can repeat skips
// rows at a page boundary.
var notesKeyset = []pg.SortKey{
	{Column: "created_at", Desc: true},
	{Column: "id", Desc: true},
}

// List returns a page of notes newest first, plus the cursor for the next page.
// An empty cursor means there is no next page.
func (n *Notes) List(ctx context.Context, limit int, cursor string) ([]Note, string, error) {
	after, err := pg.DecodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	keyset := pg.Keyset{Sort: notesKeyset, After: after, Limit: limit}
	order, err := keyset.OrderBy()
	if err != nil {
		return nil, "", err
	}
	where, args, err := keyset.Where(1)
	if err != nil {
		return nil, "", err
	}

	query := `SELECT id, title, body, created_at FROM notes`
	if where != "" {
		query += " WHERE " + where
	}
	// One more row than asked for, so "is there a next page" is answered by
	// looking rather than by guessing from a count.
	query += fmt.Sprintf(" ORDER BY %s LIMIT %d", order, limit+1)

	rows, err := n.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()

	notes := make([]Note, 0, limit)
	for rows.Next() {
		var note Note
		if err := rows.Scan(&note.ID, &note.Title, &note.Body, &note.CreatedAt); err != nil {
			return nil, "", fmt.Errorf("scan note: %w", err)
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("list notes: %w", err)
	}

	if len(notes) <= limit {
		return notes, "", nil
	}
	notes = notes[:limit]
	last := notes[len(notes)-1]
	next, err := pg.EncodeCursor([]any{last.CreatedAt, last.ID})
	if err != nil {
		return nil, "", err
	}
	return notes, next, nil
}

// All reads every note. Used by the reconcile job, which needs the whole table
// to decide what the search index should contain.
func (n *Notes) All(ctx context.Context) ([]Note, error) {
	rows, err := n.pool.Query(ctx, `SELECT id, title, body, created_at FROM notes`)
	if err != nil {
		return nil, fmt.Errorf("read notes: %w", err)
	}
	defer rows.Close()

	var notes []Note
	for rows.Next() {
		var note Note
		if err := rows.Scan(&note.ID, &note.Title, &note.Body, &note.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan note: %w", err)
		}
		notes = append(notes, note)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read notes: %w", err)
	}
	return notes, nil
}
