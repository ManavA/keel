package pg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Page is an offset page: LIMIT and OFFSET.
type Page struct {
	Limit  int
	Offset int
}

// PageOptions bounds what ParsePage will accept.
type PageOptions struct {
	// DefaultLimit is used when the caller sends no limit. Default 20, and
	// clamped to MaxLimit: serving 500 rows to a caller who sent nothing while
	// refusing limit=500 would be two answers to the same question.
	DefaultLimit int

	// MaxLimit is the largest page this endpoint will serve. Default 200.
	MaxLimit int

	// MaxOffset bounds how deep a caller may page. Default 1,000,000. Besides
	// protecting the database it prevents an overflow:
	// `?page=9223372036854775807` parses and multiplies into a negative offset,
	// which Postgres rejects and the handler reports as a 500.
	MaxOffset int
}

// ErrPaging is returned for every rejection from ParsePage, so a handler can
// answer 400 with one check. The message names the parameter and says what the
// endpoint does read; it is written to be shown to whoever sent the request.
var ErrPaging = errors.New("invalid pagination")

// pagingAliases are spellings this package does not read, mapped to the ones it
// does. They are rejected rather than translated: a client paging with a
// parameter the endpoint ignores re-reads the first page and gets a plausible
// 200 every time.
// The pagination parameters this package reads.
const (
	paramLimit  = "limit"
	paramOffset = "offset"
	paramPage   = "page"
)

var pagingAliases = map[string]string{
	"per_page":  paramLimit,
	"perPage":   paramLimit,
	"per-page":  paramLimit,
	"page_size": paramLimit,
	"pageSize":  paramLimit,
	"pagesize":  paramLimit,
	"count":     paramLimit,
	"start":     paramOffset,
	"skip":      paramOffset,
}

// ParsePage reads limit, offset and page from a query string.
//
// `page` is 1-based: page=1 is the first page, and page=0 is an error. The
// alternative reads as an off-by-one to every caller, and a client asking for
// the first page and silently getting the second is the failure this function
// exists to prevent. `offset` is 0-based, as an offset is.
//
// A pagination parameter that cannot be acted on is an error, never a silent
// default. An absent parameter still takes the default, since asking for no
// particular page is a legitimate request.
//
// So `limit=1000000` is an error naming the ceiling rather than a clamp to it.
// 200 rows back from a request for a million is indistinguishable from the end
// of the data, and a client paging by limit stops early.
func ParsePage(values url.Values, opts PageOptions) (Page, error) {
	defaultLimit := opts.DefaultLimit
	if defaultLimit <= 0 {
		defaultLimit = 20
	}
	maxLimit := opts.MaxLimit
	if maxLimit <= 0 {
		maxLimit = 200
	}
	maxOffset := opts.MaxOffset
	if maxOffset <= 0 {
		maxOffset = 1_000_000
	}
	if defaultLimit > maxLimit {
		defaultLimit = maxLimit
	}

	for _, alias := range sortedAliases() {
		if _, sent := values[alias]; sent {
			return Page{}, fmt.Errorf("%w: %q is not a parameter of this endpoint, which paginates with %q",
				ErrPaging, alias, pagingAliases[alias])
		}
	}

	limit := defaultLimit
	if v, sent, err := intParam(values, paramLimit, 1, maxLimit); err != nil {
		return Page{}, err
	} else if sent {
		limit = v
	}

	_, hasOffset := values[paramOffset]
	_, hasPage := values[paramPage]
	if hasOffset && hasPage {
		return Page{}, fmt.Errorf("%w: send either \"offset\" or \"page\", not both — "+
			"they are two spellings of one position (offset = (page-1) * limit), "+
			"and honouring one silently discards the other", ErrPaging)
	}

	page := Page{Limit: limit}
	switch {
	case hasOffset:
		v, _, err := intParam(values, paramOffset, 0, maxOffset)
		if err != nil {
			return Page{}, err
		}
		page.Offset = v
	case hasPage:
		v, _, err := intParam(values, paramPage, 1, maxOffset/limit+1)
		if err != nil {
			return Page{}, err
		}
		page.Offset = (v - 1) * limit
	}
	return page, nil
}

// intParam validates one parameter that was sent and reports whether it was
// sent at all.
//
// A repeated parameter is refused rather than resolved: `?limit=10&limit=50`
// asks two things at once, and url.Values.Get answers the first and drops the
// second.
func intParam(values url.Values, name string, minValue, maxValue int) (int, bool, error) {
	raw, sent := values[name]
	if !sent {
		return 0, false, nil
	}
	if len(raw) > 1 {
		return 0, true, fmt.Errorf("%w: %q was sent %d times; send it once — the others would be discarded",
			ErrPaging, name, len(raw))
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw[0]))
	if err != nil || v < minValue || v > maxValue {
		return 0, true, fmt.Errorf("%w: %q must be a whole number between %d and %d, got %q",
			ErrPaging, name, minValue, maxValue, raw[0])
	}
	return v, true, nil
}

func sortedAliases() []string {
	names := make([]string, 0, len(pagingAliases))
	for alias := range pagingAliases {
		names = append(names, alias)
	}
	// Sorted so a request carrying two aliases always names the same one.
	slices.Sort(names)
	return names
}

// RejectPaging returns an error if any pagination parameter was sent to an
// endpoint that returns its result whole. Ignoring `?limit=10` there leaves the
// caller with every row believing they have the first ten. whole names what
// does come back, so the message can explain why there is nothing to page.
func RejectPaging(values url.Values, whole string) error {
	names := append([]string{paramLimit, paramOffset, paramPage}, sortedAliases()...)
	slices.Sort(names)
	for _, name := range names {
		if _, sent := values[name]; sent {
			return fmt.Errorf("%w: %q is not a parameter of this endpoint: it does not paginate and returns %s",
				ErrPaging, name, whole)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Keyset paging
// ---------------------------------------------------------------------------

// SortKey is one column of an ordering.
type SortKey struct {
	Column string
	Desc   bool
}

// Keyset describes a seek page: the rows after a known position, in a known
// order.
//
// Offset paging re-reads and discards every row it skips, and a row inserted
// mid-walk shifts everything down so the client sees one row twice and misses
// another. Keyset paging has neither problem but cannot jump to an arbitrary
// page. Use offsets for a table a person clicks through, and a keyset for
// anything a machine walks end to end.
//
// The last sort key must be unique, so end the sort with a primary key: an
// ordering on a timestamp alone drops rows whenever two share a value.
type Keyset struct {
	Sort  []SortKey
	After []any
	Limit int
}

// identifier is what a column name may look like, optionally table-qualified.
// Postgres has no placeholder for an identifier, so column names are
// concatenated into SQL and must be checked rather than trusted.
//
// It is a syntax check, not an authorisation one: password_hash matches. A
// handler that forwards a caller's ?sort= needs its own allowlist of columns
// that may be ordered on.
var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)?$`)

// OrderBy renders the ORDER BY clause, without the keywords.
func (k Keyset) OrderBy() (string, error) {
	if len(k.Sort) == 0 {
		return "", errors.New("pg: keyset needs at least one sort key")
	}
	parts := make([]string, len(k.Sort))
	for i, s := range k.Sort {
		if !identifier.MatchString(s.Column) {
			return "", fmt.Errorf("pg: %q is not a valid column name", s.Column)
		}
		parts[i] = s.Column
		if s.Desc {
			parts[i] += " desc"
		}
	}
	return strings.Join(parts, ", "), nil
}

// Where renders the seek predicate and its arguments, numbering placeholders
// from firstArg. The first page has no position to seek past, so the clause is
// empty.
//
// The predicate is a row comparison, (a, b) > ($1, $2), rather than a chain of
// ORs: a multi-column index can satisfy it with one seek.
//
// Every sort key must point the same way, since a row comparison has one
// direction for the whole tuple. A mixed ordering is refused rather than
// rendered wrong.
func (k Keyset) Where(firstArg int) (string, []any, error) {
	if len(k.After) == 0 {
		return "", nil, nil
	}
	if len(k.Sort) == 0 {
		return "", nil, errors.New("pg: keyset needs at least one sort key")
	}
	if len(k.After) != len(k.Sort) {
		return "", nil, fmt.Errorf("pg: keyset has %d sort key(s) but %d cursor value(s)",
			len(k.Sort), len(k.After))
	}

	desc := k.Sort[0].Desc
	columns := make([]string, len(k.Sort))
	for i, s := range k.Sort {
		if s.Desc != desc {
			return "", nil, errors.New("pg: every keyset sort key must point the same way; " +
				"a mixed ordering cannot be expressed as a row comparison")
		}
		if !identifier.MatchString(s.Column) {
			return "", nil, fmt.Errorf("pg: %q is not a valid column name", s.Column)
		}
		columns[i] = s.Column
	}

	placeholders := make([]string, len(k.After))
	for i := range k.After {
		placeholders[i] = "$" + strconv.Itoa(firstArg+i)
	}

	op := ">"
	if desc {
		op = "<"
	}
	clause := fmt.Sprintf("(%s) %s (%s)",
		strings.Join(columns, ", "), op, strings.Join(placeholders, ", "))
	return clause, k.After, nil
}

// EncodeCursor packs the sort values of the last row on a page into an opaque
// string for the client to send back. Opaque so that its shape can change
// without breaking clients that learned to parse it.
//
// base64 is not encryption. Never put anything in a cursor that the caller may
// not see.
func EncodeCursor(values []any) (string, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("pg: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor unpacks a cursor produced by EncodeCursor. A cursor that does
// not decode is an error, not an empty first page: a client that sent a cursor
// is asking for the next page, and answering with the first loops it over the
// same rows forever.
func DecodeCursor(cursor string) ([]any, error) {
	if cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("pg: decode cursor: %w", err)
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("pg: decode cursor: %w", err)
	}
	return values, nil
}
