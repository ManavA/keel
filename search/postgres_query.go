package search

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildPostgresWhere translates text and filters into a SQL WHERE clause
// (without the "WHERE" keyword) and appends the corresponding arguments to
// *args in the order their placeholders appear. It returns "" with a nil
// error when there is nothing to filter on.
//
// Every JSON field name is passed as a PARAMETER to the `->>` operator
// (`document->>$1`), never interpolated into the SQL text — Postgres
// accepts a parameter on the right-hand side of `->>` exactly as it does
// for a comparison value, which means a field name never needs identifier
// quoting or escaping here at all.
func buildPostgresWhere(text string, filters []Filter, args *[]any) (string, error) {
	var clauses []string

	if text != "" {
		*args = append(*args, text)
		clauses = append(clauses, fmt.Sprintf("search_text @@ plainto_tsquery('simple', $%d)", len(*args)))
	}

	for _, f := range filters {
		clause, err := buildPostgresFilterClause(f, args)
		if err != nil {
			return "", err
		}
		clauses = append(clauses, clause)
	}

	return strings.Join(clauses, " AND "), nil
}

func buildPostgresFilterClause(f Filter, args *[]any) (string, error) {
	switch f.Op {
	case OpRaw:
		return "", fmt.Errorf("search: PostgresIndex does not support raw filter expressions (field %q) — "+
			"this is a Meilisearch-only escape hatch; express the filter in Postgres SQL yourself instead", f.Field)
	case OpEq:
		return textFieldClause(f.Field, "=", f.Value, args), nil
	case OpNeq:
		return textFieldClause(f.Field, "!=", f.Value, args), nil
	case OpGte:
		return numericFieldClause(f.Field, ">=", f.Value, args), nil
	case OpLte:
		return numericFieldClause(f.Field, "<=", f.Value, args), nil
	case OpGt:
		return numericFieldClause(f.Field, ">", f.Value, args), nil
	case OpLt:
		return numericFieldClause(f.Field, "<", f.Value, args), nil
	case OpIn:
		if len(f.Values) == 0 {
			// An empty IN clause means "match nothing" in SQL terms
			// (`= ANY('{}')` is always false), which agrees with what an
			// empty candidate set should mean.
			return "false", nil
		}
		*args = append(*args, f.Field)
		fieldParam := len(*args)
		*args = append(*args, f.Values)
		valuesParam := len(*args)
		return fmt.Sprintf("document->>$%d = ANY($%d)", fieldParam, valuesParam), nil
	default:
		return "", fmt.Errorf("search: unsupported filter op %q", f.Op)
	}
}

func textFieldClause(field, op string, value any, args *[]any) string {
	*args = append(*args, field)
	fieldParam := len(*args)
	*args = append(*args, fmt.Sprintf("%v", value))
	valParam := len(*args)
	return fmt.Sprintf("document->>$%d %s $%d", fieldParam, op, valParam)
}

func numericFieldClause(field, op string, value any, args *[]any) string {
	*args = append(*args, field)
	fieldParam := len(*args)
	*args = append(*args, value)
	valParam := len(*args)
	return fmt.Sprintf("(document->>$%d)::numeric %s $%d::numeric", fieldParam, op, valParam)
}

// buildPostgresOrderBy translates sort into a SQL ORDER BY clause (without
// the "ORDER BY" keywords), appending arguments to *args the same way
// buildPostgresWhere does. It returns "" when sort is empty.
func buildPostgresOrderBy(sort []SortField, args *[]any) string {
	if len(sort) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sort))
	for _, s := range sort {
		*args = append(*args, s.Field)
		idx := len(*args)
		expr := fmt.Sprintf("document->>$%d", idx)
		if s.Numeric {
			expr = fmt.Sprintf("(document->>$%d)::numeric", idx)
		}
		dir := "ASC"
		if s.Dir == Desc {
			dir = "DESC"
		}
		parts = append(parts, expr+" "+dir)
	}
	return strings.Join(parts, ", ")
}

// computeSearchText extracts fields' string values from a marshaled
// document and joins them with a space, for the tsvector column. A field
// that is missing, or whose value is not a JSON string, contributes
// nothing — a numeric or nested field is not text to search over at this
// tier.
func computeSearchText(fields []string, rawDocument []byte) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	var m map[string]any
	if err := json.Unmarshal(rawDocument, &m); err != nil {
		return "", fmt.Errorf("decode document for search text: %w", err)
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if s, ok := m[f].(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " "), nil
}

// quoteIdent quotes a SQL identifier (a table or index name). Config.Table
// is a value the calling developer sets in source, not user input, but this
// still avoids depending on that distinction being remembered correctly
// later.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
