package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// Search returns matching resources, the total, and a cursor for the next page.
func (s *Store) Search(ctx context.Context, q storage.SearchQuery) ([]*storage.Resource, int, string, error) {
	where := []string{"r.deleted = 0"}
	var args []any
	if q.Type != "" {
		where = append(where, "r.resource_type = ?")
		args = append(args, q.Type)
	}
	for _, p := range q.Params {
		clause, clauseArgs, err := renderParam(p)
		if err != nil {
			return nil, 0, "", err
		}
		if clause == "" {
			continue
		}
		where = append(where, clause)
		args = append(args, clauseArgs...)
	}
	condition := strings.Join(where, " AND ")

	total := -1
	if !q.SkipTotal {
		// Counting is a second evaluation of the predicate, so _total=none
		// skips it. A client paging through results rarely needs it twice.
		if err := s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM resource r WHERE "+condition, args...).Scan(&total); err != nil {
			return nil, 0, "", err
		}
	}

	sorts := sortExpressions(q.SortBy)
	pageArgs := append([]any{}, args...)

	// A cursor resumes exactly where the previous page stopped, by value rather
	// than by position.
	if q.Cursor != "" {
		values, err := decodeCursor(q.Cursor, len(sorts))
		if err != nil {
			return nil, 0, "", err
		}
		clause, cursorArgs := keysetPredicate(sorts, q.SortBy, values)
		condition += " AND " + clause
		pageArgs = append(pageArgs, cursorArgs...)
	}

	limit := pageLimit(q.Count)
	columns := []string{"r.resource_type", "r.fhir_id", "r.version_id", "r.last_updated", "r.content", "r.pid"}
	for _, expr := range sorts {
		columns = append(columns, expr)
	}
	query := "SELECT " + strings.Join(columns, ", ") +
		" FROM resource r WHERE " + condition +
		" ORDER BY " + renderSort(sorts, q.SortBy) +
		" LIMIT ?"
	pageArgs = append(pageArgs, limit)
	if q.Cursor == "" && q.Offset > 0 {
		// Offset paging remains available for clients that ask for it by name,
		// but the links the server hands out use cursors.
		query += " OFFSET ?"
		pageArgs = append(pageArgs, q.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, 0, "", err
	}
	defer rows.Close()

	var (
		out       []*storage.Resource
		lastPID   int64
		lastSorts []any
		rowCount  int
	)
	for rows.Next() {
		var (
			res    storage.Resource
			micros int64
			pid    int64
		)
		scanTargets := []any{&res.Type, &res.ID, &res.VersionID, &micros, &res.Content, &pid}
		sortValues := make([]any, len(sorts))
		for i := range sortValues {
			scanTargets = append(scanTargets, &sortValues[i])
		}
		if err := rows.Scan(scanTargets...); err != nil {
			return nil, 0, "", err
		}
		res.LastUpdated = time.UnixMicro(micros).UTC()
		out = append(out, &res)
		lastPID, lastSorts = pid, sortValues
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, "", err
	}

	next := ""
	if rowCount == limit && limit > 0 {
		// A full page means there may be more. Encoding the last row's sort
		// values plus its surrogate key is what makes the next page resume by
		// value: a resource inserted meanwhile cannot shift what follows.
		next = encodeCursor(lastSorts, lastPID)
	}
	return out, total, next, nil
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

// ---- cursors ----

// encodeCursor packs the last row's ordering values into an opaque token.
//
// Opaque on purpose: clients follow the bundle's next link rather than
// constructing one, so the encoding can change -- as it did when this replaced
// offset paging -- without breaking anyone.
func encodeCursor(sortValues []any, pid int64) string {
	payload := make([]any, 0, len(sortValues)+1)
	for _, v := range sortValues {
		switch x := v.(type) {
		case []byte:
			payload = append(payload, string(x))
		default:
			payload = append(payload, x)
		}
	}
	payload = append(payload, pid)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeCursor(cursor string, sortCount int) ([]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("storage: malformed cursor")
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("storage: malformed cursor")
	}
	if len(values) != sortCount+1 {
		// The cursor was made for a different sort order, so it cannot be
		// resumed against this query.
		return nil, fmt.Errorf("storage: cursor does not match the query's sort order")
	}
	return values, nil
}

// keysetPredicate builds the "everything after this row" condition.
//
// With several sort keys it expands lexicographically: later keys are only
// consulted where the earlier ones tie. The surrogate key is the final term,
// which is what makes the ordering total and the paging exact -- without it,
// rows with equal sort values could be repeated or skipped across pages.
func keysetPredicate(sorts []string, keys []storage.SortKey, values []any) (string, []any) {
	pid := values[len(values)-1]

	var terms []string
	var args []any
	for i := range sorts {
		var equalities []string
		for j := 0; j < i; j++ {
			equalities = append(equalities, sorts[j]+" = ?")
			args = append(args, values[j])
		}
		comparison := ">"
		if keys[i].Descending {
			comparison = "<"
		}
		equalities = append(equalities, sorts[i]+" "+comparison+" ?")
		args = append(args, values[i])
		terms = append(terms, "("+strings.Join(equalities, " AND ")+")")
	}

	// The final term: all sort values equal, so order by the surrogate key.
	var tie []string
	for j := range sorts {
		tie = append(tie, sorts[j]+" = ?")
		args = append(args, values[j])
	}
	tie = append(tie, "r.pid > ?")
	args = append(args, pid)
	terms = append(terms, "("+strings.Join(tie, " AND ")+")")

	return "(" + strings.Join(terms, " OR ") + ")", args
}

// ---- ordering ----

// sortExpressions renders each sort key as an SQL expression.
//
// Every expression is coalesced to a sentinel so it is never NULL. NULL
// compares as unknown, which would silently break the keyset predicate at the
// first resource lacking a value for the sort parameter; the same expression is
// used for ordering and for paging, so the two cannot disagree.
func sortExpressions(keys []storage.SortKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		switch k.Code {
		case "_id":
			out = append(out, "r.fhir_id")
		case "_lastUpdated":
			out = append(out, "r.last_updated")
		default:
			spec, ok := indexTables[k.Kind]
			if !ok {
				continue
			}
			column, sentinel := sortColumn(k.Kind)
			// A resource may hold several values for one parameter; sorting by
			// the lowest is what a client means by "sort by name".
			out = append(out, fmt.Sprintf(
				"COALESCE((SELECT MIN(%s) FROM %s WHERE pid = r.pid AND code = %s), %s)",
				column, spec.table, quote(k.Code), sentinel))
		}
	}
	return out
}

func renderSort(expressions []string, keys []storage.SortKey) string {
	terms := make([]string, 0, len(expressions)+1)
	for i, expr := range expressions {
		direction := "ASC"
		if i < len(keys) && keys[i].Descending {
			direction = "DESC"
		}
		terms = append(terms, expr+" "+direction)
	}
	// Always last, so the ordering is total.
	terms = append(terms, "r.pid ASC")
	return strings.Join(terms, ", ")
}

// sortColumn gives the column to order by and the sentinel that stands in for a
// missing value, chosen so absent values sort before present ones.
func sortColumn(kind storage.IndexKind) (column, sentinel string) {
	switch kind {
	case storage.IndexString:
		return "norm", "''"
	case storage.IndexToken:
		return "value", "''"
	case storage.IndexReference:
		return "target_id", "''"
	case storage.IndexURI:
		return "value", "''"
	case storage.IndexDate:
		return "low", "-9223372036854775808"
	default:
		return "low", "-1e308"
	}
}

// quote renders a string literal for the one place a value cannot be a
// placeholder: a correlated subquery inside ORDER BY. Only parameter codes go
// through it, and they come from the conformance index rather than a request,
// but it escapes regardless.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ---- predicates ----

// renderParam turns one parameter match into an EXISTS clause.
func renderParam(p storage.ParamMatch) (string, []any, error) {
	switch p.Code {
	case "_id":
		return renderResourceColumn("r.fhir_id", p)
	case "_lastUpdated":
		return renderLastUpdated(p)
	}

	if p.Kind == storage.IndexFullText {
		return renderFullText(p)
	}

	kind := p.Kind
	if p.TextSearch {
		// :text looks at the text extraction writes alongside a coded value,
		// which lives in the string index under the same parameter code.
		kind = storage.IndexString
	}
	spec, ok := indexTables[kind]
	if !ok {
		return "", nil, fmt.Errorf("storage: no index for parameter %q", p.Code)
	}

	// :missing asks about presence rather than value.
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
		clause, clauseArgs, err := renderValue(kind, v)
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
	if p.Negate {
		// :not negates the parameter, not each value: "has no value among
		// these", rather than "has some value that is not among these". The
		// difference shows on a resource with several values, and the
		// specification means the former.
		clause = "NOT " + clause
	}
	return clause, append([]any{p.Code}, args...), nil
}

// renderFullText builds the _text and _content predicates.
//
// FTS5 has its own query language, and a user's search terms are not written in
// it: a value containing NEAR, OR, or a quote would otherwise be read as syntax
// rather than as words. Each term is therefore quoted as a phrase and the terms
// combined with AND, which gives "all of these words appear" -- the semantics a
// client expects from a text search.
func renderFullText(p storage.ParamMatch) (string, []any, error) {
	column := "content"
	if p.Code == "_text" {
		column = "narrative"
	}
	var alternatives []string
	var args []any
	for _, v := range p.Values {
		query := fts5Query(column, v.Text)
		if query == "" {
			continue
		}
		// FTS5 requires the table's own name on the left of MATCH; an alias is
		// not a column and the parser rejects it.
		alternatives = append(alternatives,
			"EXISTS (SELECT 1 FROM idx_fulltext"+
				" WHERE idx_fulltext.rowid = r.pid AND idx_fulltext MATCH ?)")
		args = append(args, query)
	}
	if len(alternatives) == 0 {
		return "", nil, nil
	}
	clause := "(" + strings.Join(alternatives, " OR ") + ")"
	if p.Negate {
		clause = "NOT " + clause
	}
	return clause, args, nil
}

// fts5Query renders search terms as an FTS5 column-filtered conjunction, with
// every term quoted so none of it can be read as operator syntax.
func fts5Query(column, value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		quoted = append(quoted, `"`+strings.ReplaceAll(field, `"`, `""`)+`"`)
	}
	return column + " : (" + strings.Join(quoted, " AND ") + ")"
}

func renderResourceColumn(column string, p storage.ParamMatch) (string, []any, error) {
	var terms []string
	var args []any
	for _, v := range p.Values {
		terms = append(terms, column+" = ?")
		args = append(args, firstNonEmpty(v.Code, v.Text, v.URI))
	}
	if len(terms) == 0 {
		return "", nil, nil
	}
	clause := "(" + strings.Join(terms, " OR ") + ")"
	if p.Negate {
		clause = "NOT " + clause
	}
	return clause, args, nil
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
		switch v.Match {
		case storage.MatchExact:
			return "x.exact = ?", []any{v.Text}, nil
		case storage.MatchContains:
			return `x.norm LIKE ? ESCAPE '\'`, []any{"%" + escapeLike(v.Text) + "%"}, nil
		default:
			// FHIR string search matches by prefix, on the folded form.
			return `x.norm LIKE ? ESCAPE '\'`, []any{escapeLike(v.Text) + "%"}, nil
		}

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
		switch {
		case v.URIBelow:
			// :below matches the value and anything under it.
			return `x.value LIKE ? ESCAPE '\'`, []any{escapeLike(v.URI) + "%"}, nil
		case v.URIAbove:
			// :above matches the value and its ancestors, which is a prefix
			// test with the operands the other way round. SQLite has no
			// "starts-with" operator, so the comparison is expressed as the
			// query value beginning with the stored one.
			return "? LIKE x.value || '%'", []any{v.URI}, nil
		default:
			return "x.value = ?", []any{v.URI}, nil
		}

	case storage.IndexDate:
		clause, args := rangeComparison("x.low", "x.high", v.Prefix, v.DateLow, v.DateHigh)
		return clause, args, nil

	case storage.IndexNumber, storage.IndexQuantity:
		clause, args := rangeComparisonFloat("x.low", "x.high", v.Prefix, v.NumLow, v.NumHigh)
		if kind == storage.IndexQuantity && v.QuantityCode != "" {
			clause = "(" + clause + " AND (x.unit = ? OR x.system = ?))"
			args = append(args, v.QuantityCode, v.QuantitySystem)
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

// rangeComparisonFloat is rangeComparison for numbers, where the interval is
// half-open.
//
// A written 70 denotes [69.5, 70.5) and 71 denotes [70.5, 71.5). With inclusive
// endpoints those two overlap at exactly 70.5, so a search for 71 would match a
// stored 70 -- which is why the equality test uses strict comparison here and
// not for dates, whose bounds are already exclusive by a microsecond.
func rangeComparisonFloat(lowCol, highCol, prefix string, low, high float64) (string, []any) {
	switch prefix {
	case "ne":
		return fmt.Sprintf("(%s >= ? OR %s <= ?)", lowCol, highCol), []any{high, low}
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
		return fmt.Sprintf("(%s < ? AND %s > ?)", lowCol, highCol), []any{high, low}
	}
}

// escapeLike neutralizes the wildcards in a user-supplied value so a search for
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
