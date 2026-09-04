package sqlstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/langhorst/gogofhir/internal/storage"
	"github.com/langhorst/gogofhir/internal/storage/index"
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
func (s *Store) Search(ctx context.Context, q storage.SearchQuery) (storage.SearchResult, error) {
	where := []string{"r.deleted = FALSE"}
	var args []any
	if q.Type != "" {
		where = append(where, "r.resource_type = ?")
		args = append(args, q.Type)
	}
	scope := &renderScope{alias: "r", store: s}
	for _, p := range q.Params {
		clause, clauseArgs, err := renderParam(p, scope)
		if err != nil {
			return storage.SearchResult{}, err
		}
		if clause == "" {
			continue
		}
		where = append(where, clause)
		args = append(args, clauseArgs...)
	}
	if q.Filter != nil {
		clause, clauseArgs, err := renderFilter(q.Filter, scope)
		if err != nil {
			return storage.SearchResult{}, err
		}
		if clause != "" {
			where = append(where, clause)
			args = append(args, clauseArgs...)
		}
	}
	condition := strings.Join(where, " AND ")

	total := 0
	if !q.SkipTotal {
		// Counting is a second evaluation of the predicate, so _total=none
		// skips it. A client paging through results rarely needs it twice.
		if err := s.queryRow(ctx,
			"SELECT COUNT(*) FROM resource r WHERE "+condition, args...).Scan(&total); err != nil {
			return storage.SearchResult{}, err
		}
	}

	sorts := sortExpressions(q.SortBy)
	pageArgs := append([]any{}, args...)

	// A cursor resumes exactly where the previous page stopped, by value rather
	// than by position.
	if q.Cursor != "" {
		values, err := decodeCursor(q.Cursor, len(sorts))
		if err != nil {
			return storage.SearchResult{}, err
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

	rows, err := s.query(ctx, query, pageArgs...)
	if err != nil {
		return storage.SearchResult{}, err
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
			return storage.SearchResult{}, err
		}
		res.LastUpdated = time.UnixMicro(micros).UTC()
		out = append(out, &res)
		lastPID, lastSorts = pid, sortValues
		rowCount++
	}
	if err := rows.Err(); err != nil {
		return storage.SearchResult{}, err
	}

	next := ""
	if rowCount == limit && limit > 0 {
		// A full page means there may be more. Encoding the last row's sort
		// values plus its surrogate key is what makes the next page resume by
		// value: a resource inserted meanwhile cannot shift what follows.
		next = encodeCursor(lastSorts, lastPID)
	}
	return storage.SearchResult{Matches: out, Total: total, HasTotal: !q.SkipTotal, Next: next}, nil
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
func sortColumn(kind index.Kind) (column, sentinel string) {
	switch kind {
	case index.String:
		return "norm", "''"
	case index.Token:
		return "value", "''"
	case index.Reference:
		return "target_id", "''"
	case index.URI:
		return "value", "''"
	case index.Date:
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

// renderScope tracks which resource row predicates are being written against,
// and hands out unique table aliases.
//
// Chained and reverse-chained searches join to another resource, and the
// predicates on the far side are the same predicates -- so every renderer takes
// a scope rather than assuming the outer "r".
type renderScope struct {
	// store is carried so a renderer can reach the dialect; full text is the
	// one predicate the engines build differently.
	store *Store
	alias string
	n     int
	// depth bounds how far chains may nest. A chain is a join per level, and a
	// deeply nested one from an untrusted query is a cheap way to make the
	// server do expensive work.
	depth int
}

const maxChainDepth = 4

func (sc *renderScope) nested(alias string) *renderScope {
	return &renderScope{store: sc.store, alias: alias, n: sc.n, depth: sc.depth + 1}
}

func (sc *renderScope) newAlias(prefix string) string {
	sc.n++
	return fmt.Sprintf("%s%d", prefix, sc.n)
}

// renderParam turns one parameter match into an EXISTS clause.
func renderParam(p storage.ParamMatch, sc *renderScope) (string, []any, error) {
	if p.Chain != nil {
		return renderChain(p, sc)
	}
	if p.Has != nil {
		return renderHas(p, sc)
	}
	if len(p.Composite) > 0 {
		return renderComposite(p, sc)
	}

	switch p.Code {
	case "_id":
		return renderResourceColumn(sc.alias+".fhir_id", p)
	case "_lastUpdated":
		return renderLastUpdated(p, sc)
	}

	if p.Kind == index.FullText {
		return renderFullText(p, sc)
	}

	kind := p.Kind
	if p.TextSearch {
		// :text looks at the text extraction writes alongside a coded value,
		// which lives in the string index under the same parameter code.
		kind = index.String
	}
	spec, ok := indexTables[kind]
	if !ok {
		return "", nil, fmt.Errorf("storage: no index for parameter %q", p.Code)
	}

	x := sc.newAlias("x")

	// :missing asks about presence rather than value.
	if len(p.Values) == 1 && p.Values[0].Missing != nil {
		clause := fmt.Sprintf("EXISTS (SELECT 1 FROM %s %s WHERE %s.pid = %s.pid AND %s.code = ?)",
			spec.table, x, x, sc.alias, x)
		if *p.Values[0].Missing {
			clause = "NOT " + clause
		}
		return clause, []any{p.Code}, nil
	}

	inner, args, err := renderAlternatives(kind, p.Values, x)
	if err != nil {
		return "", nil, err
	}
	if inner == "" {
		return "", nil, nil
	}
	clause := fmt.Sprintf("EXISTS (SELECT 1 FROM %s %s WHERE %s.pid = %s.pid AND %s.code = ? AND (%s))",
		spec.table, x, x, sc.alias, x, inner)
	if p.Negate {
		// :not negates the parameter, not each value: "has no value among
		// these", rather than "has some value that is not among these". The
		// difference shows on a resource with several values, and the
		// specification means the former.
		clause = "NOT " + clause
	}
	return clause, append([]any{p.Code}, args...), nil
}

// renderAlternatives renders one parameter's values as a disjunction, for use
// inside an EXISTS over its index table. It returns an empty clause when there
// is nothing to match, which the caller reads as "no constraint".
func renderAlternatives(kind index.Kind, values []storage.MatchValue, alias string) (string, []any, error) {
	var alternatives []string
	var args []any
	for _, v := range values {
		clause, clauseArgs, err := renderValue(kind, v, alias)
		if err != nil {
			return "", nil, err
		}
		alternatives = append(alternatives, clause)
		args = append(args, clauseArgs...)
	}
	if len(alternatives) == 0 {
		return "", nil, nil
	}
	return strings.Join(alternatives, " OR "), args, nil
}

// renderComposite requires a composite's components to be satisfied by one and
// the same occurrence.
//
// code-value-quantity asks about a single measurement -- this code with this
// value -- not about a code somewhere in the resource and a value somewhere
// else in it. Extraction tags every row from one occurrence of the composite's
// base expression with a shared seq, and the join here is on (pid, seq): the
// first component anchors the occurrence and the rest are correlated to it.
// Matching on pid alone would return resources that have no such measurement,
// which is worse than failing because it looks like an answer.
func renderComposite(p storage.ParamMatch, sc *renderScope) (string, []any, error) {
	var clauses []string
	var args []any
	for _, alternative := range p.Composite {
		clause, clauseArgs, err := renderCompositeAlternative(alternative, sc)
		if err != nil {
			return "", nil, err
		}
		if clause == "" {
			continue
		}
		clauses = append(clauses, clause)
		args = append(args, clauseArgs...)
	}
	if len(clauses) == 0 {
		return "", nil, nil
	}
	clause := "(" + strings.Join(clauses, " OR ") + ")"
	if p.Negate {
		clause = "NOT " + clause
	}
	return clause, args, nil
}

func renderCompositeAlternative(alternative storage.CompositeMatch, sc *renderScope) (string, []any, error) {
	type leg struct {
		table, alias, condition string
		args                    []any
	}
	legs := make([]leg, 0, len(alternative.Components))
	for _, component := range alternative.Components {
		spec, ok := indexTables[component.Kind]
		if !ok {
			return "", nil, fmt.Errorf("storage: no index for composite component %q", component.Code)
		}
		alias := sc.newAlias("cp")
		inner, innerArgs, err := renderAlternatives(component.Kind, component.Values, alias)
		if err != nil {
			return "", nil, err
		}
		condition := f("%s.code = ?", alias)
		legArgs := append([]any{component.Code}, innerArgs...)
		if inner != "" {
			condition += " AND (" + inner + ")"
		}
		legs = append(legs, leg{spec.table, alias, condition, legArgs})
	}
	if len(legs) == 0 {
		return "", nil, nil
	}

	head := legs[0]
	conditions := []string{f("%s.pid = %s.pid", head.alias, sc.alias), head.condition}
	args := append([]any{}, head.args...)
	for _, l := range legs[1:] {
		conditions = append(conditions, f(
			"EXISTS (SELECT 1 FROM %s %s WHERE %s.pid = %s.pid AND %s.seq = %s.seq AND %s)",
			l.table, l.alias, l.alias, head.alias, l.alias, head.alias, l.condition))
		args = append(args, l.args...)
	}
	return f("EXISTS (SELECT 1 FROM %s %s WHERE %s)",
		head.table, head.alias, strings.Join(conditions, " AND ")), args, nil
}

// renderFilter renders a _filter expression tree.
//
// The leaves are ordinary parameter matches, so everything the query string can
// express is available inside a filter; the tree only adds the connectives. An
// operand that renders to nothing is dropped rather than treated as false --
// the same reading renderParam gives an unconstrained parameter.
func renderFilter(e *storage.FilterExpr, sc *renderScope) (string, []any, error) {
	if e == nil {
		return "", nil, nil
	}
	if e.Match != nil {
		return renderParam(*e.Match, sc)
	}

	var clauses []string
	var args []any
	for _, operand := range e.Operands {
		clause, clauseArgs, err := renderFilter(operand, sc)
		if err != nil {
			return "", nil, err
		}
		if clause == "" {
			continue
		}
		clauses = append(clauses, clause)
		args = append(args, clauseArgs...)
	}
	if len(clauses) == 0 {
		return "", nil, nil
	}

	switch e.Op {
	case storage.FilterNot:
		return "NOT (" + strings.Join(clauses, " AND ") + ")", args, nil
	case storage.FilterOr:
		return "(" + strings.Join(clauses, " OR ") + ")", args, nil
	default:
		return "(" + strings.Join(clauses, " AND ") + ")", args, nil
	}
}

// renderFullText builds the _text and _content predicates.
//
// This is the one predicate the engines build differently -- FTS5 on one side,
// tsvector on the other -- so it is the one that goes through the dialect. Both
// render "all of these words appear", which is what a client expects from a
// text search, and both treat the client's terms as words rather than as query
// syntax: a value containing NEAR, OR, or a quote must not become an operator.
func renderFullText(p storage.ParamMatch, sc *renderScope) (string, []any, error) {
	column := "content"
	if p.Code == "_text" {
		column = "narrative"
	}
	var alternatives []string
	var args []any
	for _, v := range p.Values {
		clause, clauseArgs, ok := sc.store.dialect.FullTextPredicate(sc.alias, column, v.Text)
		if !ok {
			continue
		}
		alternatives = append(alternatives, clause)
		args = append(args, clauseArgs...)
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

func renderLastUpdated(p storage.ParamMatch, sc *renderScope) (string, []any, error) {
	column := sc.alias + ".last_updated"
	var terms []string
	var args []any
	for _, v := range p.Values {
		term, termArgs := rangeComparison(column, column, v.Prefix, v.DateLow, v.DateHigh)
		terms = append(terms, term)
		args = append(args, termArgs...)
	}
	if len(terms) == 0 {
		return "", nil, nil
	}
	return "(" + strings.Join(terms, " OR ") + ")", args, nil
}

// renderValue builds the condition for one alternative, inside the EXISTS.
func renderValue(kind index.Kind, v storage.MatchValue, x string) (string, []any, error) {
	switch kind {
	case index.Token:
		switch {
		case v.System != "" && v.Code != "":
			return f("(%s.system = ? AND %s.value = ?)", x, x), []any{v.System, v.Code}, nil
		case v.System != "":
			// "system|" matches any code in that system.
			return f("%s.system = ?", x), []any{v.System}, nil
		default:
			return f("%s.value = ?", x), []any{v.Code}, nil
		}

	case index.String:
		// The folded column is only ever compared with folded text. Callers
		// normalize already, but doing it here as well is what makes the
		// comparison independent of the engine: SQLite's LIKE ignores ASCII
		// case and PostgreSQL's does not, so a query that reached the column
		// unfolded would quietly mean different things on the two.
		folded := escapeLike(index.Normalize(v.Text))
		switch v.Match {
		case storage.MatchExact:
			return f("%s.exact = ?", x), []any{v.Text}, nil
		case storage.MatchContains:
			return f(`%s.norm LIKE ? ESCAPE '\'`, x), []any{"%" + folded + "%"}, nil
		case storage.MatchEndsWith:
			return f(`%s.norm LIKE ? ESCAPE '\'`, x), []any{"%" + folded}, nil
		default:
			// FHIR string search matches by prefix, on the folded form.
			return f(`%s.norm LIKE ? ESCAPE '\'`, x), []any{folded + "%"}, nil
		}

	case index.Reference:
		switch {
		case v.RefType != "" && v.RefID != "":
			return f("(%s.target_type = ? AND %s.target_id = ?)", x, x), []any{v.RefType, v.RefID}, nil
		case v.RefID != "":
			// A bare id matches whatever type it turns out to be.
			return f("(%s.target_id = ? OR %s.url = ?)", x, x), []any{v.RefID, v.RefID}, nil
		default:
			return f("%s.url = ?", x), []any{v.RefURL}, nil
		}

	case index.URI:
		switch {
		case v.URIBelow:
			// :below matches the value and anything under it.
			return f(`%s.value LIKE ? ESCAPE '\'`, x), []any{escapeLike(v.URI) + "%"}, nil
		case v.URIAbove:
			// :above matches the value and its ancestors, which is a prefix
			// test with the operands the other way round. SQLite has no
			// "starts-with" operator, so it is expressed as the query value
			// beginning with the stored one.
			return f("? LIKE %s.value || '%%'", x), []any{v.URI}, nil
		default:
			return f("%s.value = ?", x), []any{v.URI}, nil
		}

	case index.Date:
		clause, args := rangeComparison(x+".low", x+".high", v.Prefix, v.DateLow, v.DateHigh)
		return clause, args, nil

	case index.Number, index.Quantity:
		clause, args := rangeComparisonFloat(x+".low", x+".high", v.Prefix, v.NumLow, v.NumHigh)
		if kind == index.Quantity && v.QuantityCode != "" {
			clause = f("(%s AND (%s.unit = ? OR %s.system = ?))", clause, x, x)
			args = append(args, v.QuantityCode, v.QuantitySystem)
		}
		return clause, args, nil
	}
	return "", nil, fmt.Errorf("storage: unsupported index kind %q", kind)
}

// f is fmt.Sprintf, named short because these SQL fragments are mostly alias
// substitution and the formatting call would otherwise dominate the line.
func f(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// renderChain joins through a reference and applies the far side's predicates.
//
// "subject.name=peter" is two questions: which resources does subject point at,
// and do those match name=peter. The join carries the second question into a
// nested scope, where the same renderers run against the joined row.
func renderChain(p storage.ParamMatch, sc *renderScope) (string, []any, error) {
	if sc.depth >= maxChainDepth {
		return "", nil, fmt.Errorf("storage: search chain is deeper than %d levels", maxChainDepth)
	}
	ref := sc.newAlias("ch")
	target := sc.newAlias("ct")

	conditions := []string{
		f("%s.pid = %s.pid", ref, sc.alias),
		f("%s.code = ?", ref),
		f("%s.deleted = FALSE", target),
	}
	args := []any{p.Code}
	if p.Chain.TargetType != "" {
		conditions = append(conditions, f("%s.target_type = ?", ref))
		args = append(args, p.Chain.TargetType)
	}

	nested, nestedArgs, err := renderNested(p.Chain.Params, sc, target)
	if err != nil {
		return "", nil, err
	}
	conditions = append(conditions, nested...)
	args = append(args, nestedArgs...)

	clause := f("EXISTS (SELECT 1 FROM idx_reference %s"+
		" JOIN resource %s ON %s.resource_type = %s.target_type AND %s.fhir_id = %s.target_id"+
		" WHERE %s)",
		ref, target, target, ref, target, ref, strings.Join(conditions, " AND "))
	if p.Negate {
		clause = "NOT " + clause
	}
	return clause, args, nil
}

// renderNested renders the far side's parameters against a joined row.
//
// Chains and reverse chains differ in how they join, not in what they ask of
// the row they reach: the same predicate renderers run against it in a nested
// scope. The alias counter is carried back out so aliases minted inside stay
// unique for whatever the caller renders next.
func renderNested(params []storage.ParamMatch, sc *renderScope, alias string) ([]string, []any, error) {
	inner := sc.nested(alias)
	var clauses []string
	var args []any
	for _, p := range params {
		clause, clauseArgs, err := renderParam(p, inner)
		if err != nil {
			return nil, nil, err
		}
		if clause == "" {
			continue
		}
		clauses = append(clauses, clause)
		args = append(args, clauseArgs...)
	}
	sc.n = inner.n
	return clauses, args, nil
}

// renderHas is the reverse join: some resource points at this one and matches.
func renderHas(p storage.ParamMatch, sc *renderScope) (string, []any, error) {
	if sc.depth >= maxChainDepth {
		return "", nil, fmt.Errorf("storage: search chain is deeper than %d levels", maxChainDepth)
	}
	source := sc.newAlias("hs")
	ref := sc.newAlias("hr")

	conditions := []string{
		f("%s.pid = %s.pid", ref, source),
		f("%s.resource_type = ?", source),
		f("%s.deleted = FALSE", source),
		f("%s.code = ?", ref),
		// The reference must point back at the row in scope. Matching both the
		// type and the id keeps "Patient/1" from also matching "Group/1".
		f("%s.target_type = %s.resource_type", ref, sc.alias),
		f("%s.target_id = %s.fhir_id", ref, sc.alias),
	}
	args := []any{p.Has.SourceType, p.Has.Code}

	nested, nestedArgs, err := renderNested(p.Has.Params, sc, source)
	if err != nil {
		return "", nil, err
	}
	conditions = append(conditions, nested...)
	args = append(args, nestedArgs...)

	clause := f("EXISTS (SELECT 1 FROM resource %s JOIN idx_reference %s ON %s.pid = %s.pid WHERE %s)",
		source, ref, ref, source, strings.Join(conditions, " AND "))
	if p.Negate {
		clause = "NOT " + clause
	}
	return clause, args, nil
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
