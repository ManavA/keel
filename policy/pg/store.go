package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ManavA/keel/policy"
)

// Table is the name of the table policy/pg/migrations creates.
const Table = "policy_decisions"

// List returns this many decisions unless the filter says otherwise, and never
// more than maxLimit, so a caller cannot read the whole log into memory by
// asking for a number it did not think about.
const (
	defaultLimit = 100
	maxLimit     = 1000
)

// conn is the subset of *pgxpool.Pool this package needs, so a test can fake
// it without a live database. A pgx.Tx satisfies it too.
type conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store is a [policy.Recorder] backed by Postgres. It is safe for concurrent
// use when its connection is. The zero value is not usable; build one with
// [New].
type Store struct {
	db conn
}

// New builds a Store over db, which must already have the schema from
// policy/pg/migrations applied.
func New(db conn) *Store {
	return &Store{db: db}
}

const insertSQL = `
insert into ` + Table + ` (decided_at, kind, target, attrs, effect, rule, rule_index, matched, uncertain, policy_version)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// Record implements [policy.Recorder]. It returns nil only when one row was
// written; an error means the decision is not on the record, and a
// [policy.Decider] then gives no decision. An error for a record that can never
// be stored wraps [policy.ErrUnrecordable]: what the store refuses before it
// asks the database (see the package comment) and what the server refuses of
// the record's own content or size. An error that is the database's, or the
// moment's, does not.
func (s *Store) Record(ctx context.Context, rec policy.Record) error {
	args, err := insertArgs(rec)
	if err != nil {
		return fmt.Errorf("policy/pg: record %q action: %w", rec.Action.Kind, unrecordable(err))
	}
	tag, err := s.db.Exec(ctx, insertSQL, args...)
	if err != nil {
		return fmt.Errorf("policy/pg: record %q action: %w", rec.Action.Kind, asUnrecordable(err))
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("policy/pg: record %q action: the insert wrote %d rows, not 1", rec.Action.Kind, n)
	}
	return nil
}

// insertArgs is the parameters of insertSQL for rec, or the reason Postgres
// could not hold it. The record it is handed is already a copy, so nothing
// here copies again.
func insertArgs(rec policy.Record) ([]any, error) {
	d := rec.Decision
	if !d.Effect.Valid() {
		return nil, fmt.Errorf("the decision's effect is %q, not allow, ask or block", d.Effect)
	}
	for _, t := range []struct{ what, text string }{
		{"the action's kind", rec.Action.Kind},
		{"the action's target", rec.Action.Target},
		{"the deciding rule's name", d.Rule},
		{"the policy version", rec.Version},
	} {
		if strings.IndexByte(t.text, 0) >= 0 {
			return nil, errNUL(t.what)
		}
	}
	matched, err := encodeNames("a matched rule's name", d.Matched)
	if err != nil {
		return nil, err
	}
	uncertain, err := encodeNames("an uncertain attribute's name", d.Uncertain)
	if err != nil {
		return nil, err
	}
	attrs, err := encodeAttrs(rec.Action.Attrs)
	if err != nil {
		return nil, err
	}
	return []any{
		// The column keeps microseconds: the rest is dropped here, so that which
		// way it goes is this package's rule and not the driver's.
		rec.At.UTC().Truncate(time.Microsecond),
		rec.Action.Kind, rec.Action.Target, attrs,
		string(d.Effect), d.Rule, d.Index, matched, uncertain,
		rec.Version,
	}, nil
}

func errNUL(what string) error {
	return fmt.Errorf("%s holds a NUL character, which Postgres cannot store", what)
}

// encodeNames is a list of names as the JSON array to store, [] when there are
// none.
func encodeNames(what string, names []string) ([]byte, error) {
	for _, n := range names {
		if strings.IndexByte(n, 0) >= 0 {
			return nil, errNUL(what)
		}
	}
	if len(names) == 0 {
		return []byte("[]"), nil
	}
	b, err := json.Marshal(names)
	if err != nil {
		return nil, fmt.Errorf("%s cannot be written as JSON: %w", what, err)
	}
	return b, nil
}

// Cursor is a position in the log, as agent.Cursor is in a run listing: the
// time a decision was decided and its ID, which orders those decided at one
// time. A caller builds one from a record it was given, as Cursor{At: rec.At,
// ID: rec.ID}.
type Cursor struct {
	At time.Time
	ID int64
}

// Filter narrows List. The zero Filter lists the newest decisions.
type Filter struct {
	// Effect, Rule and Kind each match a decision's own value exactly, with no
	// regard to case. Empty matches any. An Effect that is not allow, ask or
	// block is an error, not an empty result.
	Effect policy.Effect
	Rule   string
	Kind   string
	// Since keeps the decisions decided at this time or later. It is read to the
	// microsecond, as a decision's time is stored. The zero time keeps all.
	Since time.Time
	// Before returns decisions older than this position: those decided earlier,
	// and those decided at the same time and recorded earlier, so a record that
	// shares its time with the last one of a page is on that page or the next
	// and never both. It is read to the microsecond like Since. To read the next
	// page, set it to the position of the last record of the one before; a page
	// that comes back empty is the end of the log. A cursor whose ID is not one
	// the log gives, which is any below 1, is an error.
	Before *Cursor
	// Limit is the size of a page. It defaults to 100, and is at most 1000: a
	// larger number is 1000, and Before reaches the rest.
	Limit int
}

// limit is the number of rows to ask for.
func (f Filter) limit() int {
	switch {
	case f.Limit <= 0:
		return defaultLimit
	case f.Limit > maxLimit:
		return maxLimit
	}
	return f.Limit
}

const selectSQL = `
select id, decided_at, kind, target, attrs, effect, rule, rule_index, matched, uncertain, policy_version
from ` + Table

// query is the select for f and its arguments. The text holds nothing the
// caller supplied: each filter adds a fixed condition and a placeholder.
func (f Filter) query() (string, []any) {
	var (
		sql  strings.Builder
		args []any
		join = "\nwhere "
	)
	sql.WriteString(selectSQL)
	and := func(condition string, arg any) {
		args = append(args, arg)
		fmt.Fprintf(&sql, "%s%s $%d", join, condition, len(args))
		join = "\n  and "
	}
	if f.Effect != "" {
		and("effect =", string(f.Effect))
	}
	if f.Rule != "" {
		and("rule =", f.Rule)
	}
	if f.Kind != "" {
		and("kind =", f.Kind)
	}
	if !f.Since.IsZero() {
		and("decided_at >=", f.Since.UTC().Truncate(time.Microsecond))
	}
	if f.Before != nil {
		// The order of the log, as a row comparison: it is also the order of
		// policy_decisions_decided_idx.
		args = append(args, f.Before.At.UTC().Truncate(time.Microsecond), f.Before.ID)
		fmt.Fprintf(&sql, "%s(decided_at, id) < ($%d, $%d)", join, len(args)-1, len(args))
		join = "\n  and "
	}
	// The id breaks a tie between decisions of one instant, so the order, and
	// which of them a limit cuts, is the same every time.
	args = append(args, f.limit())
	fmt.Fprintf(&sql, "\norder by decided_at desc, id desc\nlimit $%d", len(args))
	return sql.String(), args
}

// List returns a page of decisions newest first, as the package comment
// describes: at most 1000, an empty list and not nil when none match, and every
// record read back with its ID, in UTC, with its attributes, matched rules and
// uncertain attributes non-nil and its numbers json.Number. Filter.Before pages
// back through the log.
func (s *Store) List(ctx context.Context, f Filter) ([]policy.Record, error) {
	if f.Effect != "" && !f.Effect.Valid() {
		return nil, fmt.Errorf("policy/pg: list: the effect %q is not allow, ask or block", f.Effect)
	}
	if f.Before != nil && f.Before.ID < 1 {
		return nil, fmt.Errorf("policy/pg: list: the cursor's id %d is not an id the log gives, which starts at 1", f.Before.ID)
	}
	sql, args := f.query()
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("policy/pg: list: %w", err)
	}
	defer rows.Close()

	out := []policy.Record{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("policy/pg: list: %w", err)
	}
	return out, nil
}

// scanRecord reads the row rows is on.
func scanRecord(rows pgx.Rows) (policy.Record, error) {
	var (
		id                        int64
		rec                       policy.Record
		effect                    string
		attrs, matched, uncertain []byte
	)
	if err := rows.Scan(&id, &rec.At, &rec.Action.Kind, &rec.Action.Target, &attrs,
		&effect, &rec.Decision.Rule, &rec.Decision.Index, &matched, &uncertain, &rec.Version,
	); err != nil {
		return policy.Record{}, fmt.Errorf("policy/pg: list: scan: %w", err)
	}
	rec.ID = id
	rec.At = rec.At.UTC()
	rec.Decision.Effect = policy.Effect(effect)

	var err error
	if rec.Action.Attrs, err = decodeAttrs(attrs); err != nil {
		return policy.Record{}, fmt.Errorf("policy/pg: list: row %d: attrs: %w", id, err)
	}
	if rec.Decision.Matched, err = decodeNames(matched); err != nil {
		return policy.Record{}, fmt.Errorf("policy/pg: list: row %d: matched: %w", id, err)
	}
	if rec.Decision.Uncertain, err = decodeNames(uncertain); err != nil {
		return policy.Record{}, fmt.Errorf("policy/pg: list: row %d: uncertain: %w", id, err)
	}
	return rec, nil
}

// decodeAttrs reads the stored attributes with every number kept as the
// json.Number it was written as, never a float64. JSON null reads as empty.
func decodeAttrs(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var attrs map[string]any
	if err := dec.Decode(&attrs); err != nil {
		return nil, err
	}
	if attrs == nil {
		attrs = map[string]any{}
	}
	return attrs, nil
}

// decodeNames reads a stored list of names. JSON null reads as empty.
func decodeNames(raw []byte) ([]string, error) {
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}

var _ policy.Recorder = (*Store)(nil)
