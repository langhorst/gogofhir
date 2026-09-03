package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/langhorst/gogofhir/internal/storage"
)

// Query execution.
//
// A SearchQuery arrives as a plan -- typed parameter matches, not a query
// string -- and is rendered to SQL here. That is the whole point of the
// boundary: the search layer never writes SQL, and this file never parses FHIR
// syntax, so the PostgreSQL port rewrites only what is below.
//
// Each parameter becomes an EXISTS subquery against its index table. Values
// within one parameter are alternatives (FHIR's comma-separated "or"); separate
// parameters are conjunctions. EXISTS rather than a join because a resource can
// have many values for one parameter -- three given names, five identifiers --
// and joining would multiply rows and need a DISTINCT to undo.

// Search returns the current versions matching a query, and the total number of
// matches ignoring paging.
func (s *Store) Search(ctx context.Context, q storage.SearchQuery) ([]*storage.Resource, int, error) {
	where := []string{"r.deleted = 0"}
	var args []any
	if q.Type != "" {
		where = append(where, "r.resource_type = ?")
		args = append(args, q.Type)
	}
	for _, p := range q.Params {
		clause, clauseArgs, err := renderParam(p)
		if err != nil {
			return nil, 0, err
		}
		if clause == "" {
			continue
		}
		where = append(where, clause)
		args = append(args, clauseArgs...)
	}
	condition := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM resource r WHERE "+condition, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := "SELECT r.resource_type, r.fhir_id, r.version_id, r.last_updated, r.content" +
		" FROM resource r WHERE " + condition + " ORDER BY " + renderSort(q.SortBy)
	pageArgs := append(append([]any{}, args...), pageLimit(q.Count), q.Offset)
	query += " LIMIT ? OFFSET ?"

	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []*storage.Resource
	for rows.Next() {
		var (
			res    storage.Resource
			micros int64
		)
		if err := rows.Scan(&res.Type, &res.ID, &res.VersionID, &micros, &res.Content); err != nil {
			return nil, 0, err
		}
		res.LastUpdated = time.UnixMicro(micros).UTC()
		out = append(out, &res)
	}
	return out, total, rows.Err()
}

func pageLimit(count int) int {
	switch {
	case count < 0:
		return 0
	case count == 0:
		return 50
	default:
		return count
	}
}

// renderSort builds the ORDER BY. The surrogate key is always the final term so
// the ordering is total: without it, resources with equal sort values come back
// in an arbitrary order that can change between pages.
func renderSort(keys []storage.SortKey) string {
	var terms []string
	for _, k := range keys {
		direction := "ASC"
		if k.Descending {
			direction = "DESC"
		}
		switch k.Code {
		case "_id":
			terms = append(terms, "r.fhir_id "+direction)
		case "_lastUpdated":
			terms = append(terms, "r.last_updated "+direction)
		default:
			// Sorting by an indexed parameter uses its lowest value, which is
			// what a client means by "sort by name" when a resource has several.
			spec, ok := indexTables[k.Kind]
			if !ok {
				continue
			}
			column := sortColumn(k.Kind)
			terms = append(terms, fmt.Sprintf(
				"(SELECT MIN(%s) FROM %s WHERE pid = r.pid AND code = %s) %s",
				column, spec.table, quote(k.Code), direction))
		}
	}
	terms = append(terms, "r.pid ASC")
	return strings.Join(terms, ", ")
}

func sortColumn(kind storage.IndexKind) string {
	switch kind {
	case storage.IndexString:
		return "norm"
	case storage.IndexToken:
		return "value"
	case storage.IndexReference:
		return "target_id"
	case storage.IndexURI:
		return "value"
	default:
		return "low"
	}
}

// quote renders a string literal for the few places a value cannot be a
// placeholder -- a correlated subquery inside ORDER BY. Only parameter codes go
// through it, and they come from the conformance index rather than from a
// request, but it escapes regardless.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// renderParam turns one parameter match into an EXISTS clause.
func renderParam(p storage.ParamMatch) (string, []any, error) {
	// _id and _lastUpdated are properties of the resource row itself, not
	// indexed values.
	switch p.Code {
	case "_id":
		return renderResourceColumn("r.fhir_id", p)
	case "_lastUpdated":
		return renderLastUpdated(p)
	}

	spec, ok := indexTables[p.Kind]
	if !ok {
		return "", nil, fmt.Errorf("storage: no index for parameter %q", p.Code)
	}

	// The :missing modifier asks about presence rather than value.
	if len(p.Values) == 1 && p.Values[0].Missing != nil {
		clause := fmt.Sprintf("EXISTS (SELECT 1 FROM %s x WHERE x.pid = r.pid AND x.code = ?)", spec.table)
		if *p.Values[0].Missing {
			clause = "NOT " + clause
		}
		return clause, []any{p.Code}, nil
	}

	var alternatives []string
	var args []any
	for _, v := range p.Values {
		clause, clauseArgs, err := renderValue(p.Kind, v)
		if err != nil {
			return "", nil, err
		}
		alternatives = append(alternatives, clause)
		args = append(args, clauseArgs...)
	}
	if len(alternatives) == 0 {
		return "", nil, nil
	}
	inner := strings.Join(alternatives, " OR ")
	clause := fmt.Sprintf("EXISTS (SELECT 1 FROM %s x WHERE x.pid = r.pid AND x.code = ? AND (%s))",
		spec.table, inner)
	return clause, append([]any{p.Code}, args...), nil
}

func renderResourceColumn(column string, p storage.ParamMatch) (string, []any, error) {
	var terms []string
	var args []any
	for _, v := range p.Values {
		value := firstNonEmpty(v.Code, v.Text, v.URI)
		terms = append(terms, column+" = ?")
		args = append(args, value)
	}
	if len(terms) == 0 {
		return "", nil, nil
	}
	return "(" + strings.Join(terms, " OR ") + ")", args, nil
}

func renderLastUpdated(p storage.ParamMatch) (string, []any, error) {
	var terms []string
	var args []any
	for _, v := range p.Values {
		term, termArgs := rangeComparison("r.last_updated", "r.last_updated", v.Prefix, v.DateLow, v.DateHigh)
		terms = append(terms, term)
		args = append(args, termArgs...)
	}
	if len(terms) == 0 {
		return "", nil, nil
	}
	return "(" + strings.Join(terms, " OR ") + ")", args, nil
}

// renderValue builds the condition for one alternative, inside the EXISTS.
func renderValue(kind storage.IndexKind, v storage.MatchValue) (string, []any, error) {
	switch kind {
	case storage.IndexToken:
		switch {
		case v.System != "" && v.Code != "":
			return "(x.system = ? AND x.value = ?)", []any{v.System, v.Code}, nil
		case v.System != "":
			// "system|" matches any code in that system.
			return "x.system = ?", []any{v.System}, nil
		default:
			return "x.value = ?", []any{v.Code}, nil
		}

	case storage.IndexString:
		if v.Exact {
			return "x.exact = ?", []any{v.Text}, nil
		}
		// FHIR string search matches by prefix by default, on the folded form.
		return "x.norm LIKE ? ESCAPE '\\'", []any{escapeLike(v.Text) + "%"}, nil

	case storage.IndexReference:
		switch {
		case v.RefType != "" && v.RefID != "":
			return "(x.target_type = ? AND x.target_id = ?)", []any{v.RefType, v.RefID}, nil
		case v.RefID != "":
			// A bare id matches whatever type it turns out to be.
			return "(x.target_id = ? OR x.url = ?)", []any{v.RefID, v.RefID}, nil
		default:
			return "x.url = ?", []any{v.RefURL}, nil
		}

	case storage.IndexURI:
		return "x.value = ?", []any{v.URI}, nil

	case storage.IndexDate:
		clause, args := rangeComparison("x.low", "x.high", v.Prefix, v.DateLow, v.DateHigh)
		return clause, args, nil

	case storage.IndexNumber, storage.IndexQuantity:
		clause, args := rangeComparisonFloat("x.low", "x.high", v.Prefix, v.NumLow, v.NumHigh)
		if kind == storage.IndexQuantity {
			if v.QuantityCode != "" {
				clause = "(" + clause + " AND (x.unit = ? OR x.system = ?))"
				args = append(args, v.QuantityCode, v.QuantitySystem)
			}
		}
		return clause, args, nil
	}
	return "", nil, fmt.Errorf("storage: unsupported index kind %q", kind)
}

// rangeComparison implements the FHIR search prefixes as interval algebra over
// the stored [low, high] range and the query's own [low, high].
//
// Both sides are ranges, which is what makes the prefixes subtle: "eq" means
// the ranges overlap, not that endpoints match, and "sa" (starts after) is
// about the stored range beginning strictly after the query range ends.
func rangeComparison(lowCol, highCol, prefix string, low, high int64) (string, []any) {
	switch prefix {
	case "ne":
		return fmt.Sprintf("(%s > ? OR %s < ?)", lowCol, highCol), []any{high, low}
	case "gt":
		return fmt.Sprintf("%s > ?", highCol), []any{high}
	case "lt":
		return fmt.Sprintf("%s < ?", lowCol), []any{low}
	case "ge":
		return fmt.Sprintf("%s >= ?", highCol), []any{low}
	case "le":
		return fmt.Sprintf("%s <= ?", lowCol), []any{high}
	case "sa":
		return fmt.Sprintf("%s > ?", lowCol), []any{high}
	case "eb":
		return fmt.Sprintf("%s < ?", highCol), []any{low}
	default: // eq and ap
		return fmt.Sprintf("(%s <= ? AND %s >= ?)", lowCol, highCol), []any{high, low}
	}
}

func rangeComparisonFloat(lowCol, highCol, prefix string, low, high float64) (string, []any) {
	switch prefix {
	case "ne":
		return fmt.Sprintf("(%s > ? OR %s < ?)", lowCol, highCol), []any{high, low}
	case "gt":
		return fmt.Sprintf("%s > ?", highCol), []any{high}
	case "lt":
		return fmt.Sprintf("%s < ?", lowCol), []any{low}
	case "ge":
		return fmt.Sprintf("%s >= ?", highCol), []any{low}
	case "le":
		return fmt.Sprintf("%s <= ?", lowCol), []any{high}
	case "sa":
		return fmt.Sprintf("%s > ?", lowCol), []any{high}
	case "eb":
		return fmt.Sprintf("%s < ?", highCol), []any{low}
	default:
		return fmt.Sprintf("(%s <= ? AND %s >= ?)", lowCol, highCol), []any{high, low}
	}
}

// escapeLike neutralizes the wildcards in a user-supplied prefix so a search for
// "100%" does not match everything.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
