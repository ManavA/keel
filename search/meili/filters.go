package meili

import (
	"fmt"
	"strings"

	"github.com/ManavA/keel/search"
)

// QuoteFilterValue removes characters that could break out of a quoted
// Meilisearch filter string, and returns the value quoted and ready to
// interpolate into a raw expression (see [Raw]).
//
// A string value containing a space or a hyphen, interpolated without
// quotes, is read by Meilisearch's filter parser as more than one token,
// which can make the query match the wrong documents or error. Quote every
// string value for this reason, not only ones known to contain such
// characters. [buildMeiliFilters] does this for every [Filter]; call this
// directly only when building a [Raw] expression by hand.
func QuoteFilterValue(v string) string {
	cleaned := strings.NewReplacer(`"`, "", `\`, "").Replace(v)
	return fmt.Sprintf("%q", cleaned)
}

// buildMeiliFilters translates a backend-agnostic []search.Filter into
// Meilisearch filter expressions.
func buildMeiliFilters(filters []search.Filter) ([]string, error) {
	out := make([]string, 0, len(filters))
	for _, f := range filters {
		switch f.Op {
		case search.OpRaw:
			expr, _ := f.Value.(string)
			if expr != "" {
				out = append(out, expr)
			}
		case search.OpEq:
			out = append(out, fmt.Sprintf("%s = %s", f.Field, meiliValue(f.Value)))
		case search.OpNeq:
			out = append(out, fmt.Sprintf("%s != %s", f.Field, meiliValue(f.Value)))
		case search.OpGte:
			out = append(out, fmt.Sprintf("%s >= %s", f.Field, meiliValue(f.Value)))
		case search.OpLte:
			out = append(out, fmt.Sprintf("%s <= %s", f.Field, meiliValue(f.Value)))
		case search.OpGt:
			out = append(out, fmt.Sprintf("%s > %s", f.Field, meiliValue(f.Value)))
		case search.OpLt:
			out = append(out, fmt.Sprintf("%s < %s", f.Field, meiliValue(f.Value)))
		case search.OpIn:
			if len(f.Values) == 0 {
				continue
			}
			quoted := make([]string, len(f.Values))
			for i, v := range f.Values {
				quoted[i] = QuoteFilterValue(v)
			}
			out = append(out, fmt.Sprintf("%s IN [%s]", f.Field, strings.Join(quoted, ", ")))
		default:
			return nil, fmt.Errorf("meili: unsupported filter op %q", f.Op)
		}
	}
	return out, nil
}

// meiliValue renders a filter value for interpolation: quoted if it is a
// string, printed bare (numeric or boolean) otherwise.
func meiliValue(v any) string {
	if s, ok := v.(string); ok {
		return QuoteFilterValue(s)
	}
	return fmt.Sprintf("%v", v)
}

// buildMeiliSort translates a backend-agnostic []search.SortField into
// Meilisearch sort expressions.
func buildMeiliSort(sort []search.SortField) []string {
	out := make([]string, 0, len(sort))
	for _, s := range sort {
		dir := "asc"
		if s.Dir == search.Desc {
			dir = "desc"
		}
		out = append(out, fmt.Sprintf("%s:%s", s.Field, dir))
	}
	return out
}
