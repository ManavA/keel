package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	agentpg "github.com/ManavA/keel/agent/pg"
	keelpg "github.com/ManavA/keel/pg"
)

// Document is one item of the batch. Body is left out of a listing.
type Document struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

var errNoDocument = errors.New("no such document")

// Documents is the example's own tables. The three writes each begin with
// agentpg.Once on the tool call's key, in the transaction that makes the
// write, so a call that was interrupted and is made again writes nothing the
// second time: the key and the row commit together or not at all.
type Documents struct {
	pool *pgxpool.Pool
}

func NewDocuments(pool *pgxpool.Pool) *Documents { return &Documents{pool: pool} }

func (d *Documents) List(ctx context.Context) ([]Document, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, title FROM documents ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	docs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Document, error) {
		var doc Document
		err := row.Scan(&doc.ID, &doc.Title)
		return doc, err
	})
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	return docs, nil
}

func (d *Documents) Get(ctx context.Context, id string) (Document, error) {
	doc := Document{ID: id}
	err := d.pool.QueryRow(ctx, `SELECT title, body FROM documents WHERE id = $1`, id).Scan(&doc.Title, &doc.Body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, errNoDocument
	}
	if err != nil {
		return Document{}, fmt.Errorf("read document: %w", err)
	}
	return doc, nil
}

// once runs write in a transaction that first records key, and skips it when
// the key is already there.
func (d *Documents) once(ctx context.Context, key string, write func(tx pgx.Tx) error) error {
	return keelpg.InTx(ctx, d.pool, func(tx pgx.Tx) error {
		first, err := agentpg.Once(ctx, tx, key)
		if err != nil || !first {
			return err
		}
		return write(tx)
	})
}

func (d *Documents) SaveSummary(ctx context.Context, key, runID, documentID, summary string) error {
	err := d.once(ctx, key, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO summaries (run_id, document_id, summary) VALUES ($1, $2, $3)`,
			runID, documentID, summary)
		return err
	})
	if err != nil {
		return fmt.Errorf("save summary: %w", err)
	}
	return nil
}

func (d *Documents) RecordDigest(ctx context.Context, key, runID, recipient, subject, body string, documentIDs []string) error {
	err := d.once(ctx, key, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO digests (run_id, recipient, subject, body, document_ids) VALUES ($1, $2, $3, $4, $5)`,
			runID, recipient, subject, body, documentIDs)
		return err
	})
	if err != nil {
		return fmt.Errorf("record digest: %w", err)
	}
	return nil
}

// Delete is what delete_document would do. Under the example's rules it is
// never reached: the call is blocked before the tool runs.
func (d *Documents) Delete(ctx context.Context, key, id string) error {
	err := d.once(ctx, key, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	return nil
}
