package rest

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

// Parsing the FHIR search syntax into a storage query plan.
//
// The layering matters: this file understands query strings and nothing about
// SQL, and the backend understands the plan and nothing about query strings.
// M2 covers the value syntax -- token systems, prefixes, alternatives, :exact
// and :missing -- while chaining, _has, _include, and composite parameters
// arrive with the rest of search.

// controlParams are the query parameters that shape the response rather than
// filter it.
var controlParams = map[string]bool{
	"_format": true, "_count": true, "_offset": true, "_sort": true,
	"_summary": true, "_total": true, "_pretty": true, "_elements": true,
	"_cursor": true, "_include": true, "_revinclude": true, "_filter": true,
}

// searchOptions are the response-shaping choices a query carries.
type searchOptions struct {
	// summary is the _summary mode: "", "true", "text", "data", "count", or
	// "false".
	summary string
	// elements is the _elements list, empty when absent.
	elements []string
	// countOnly is _summary=count: the total without any entries.
	countOnly bool
	// includes are the _include and _revinclude specifications to resolve
	// after the matches are known.
	includes []storage.IncludeSpec
}

// subset applies _summary and _elements to a resource on its way out.
func (o searchOptions) subset(node *resource.Node) *resource.Node {
	if len(o.elements) > 0 {
		return node.Elements(o.elements)
	}
	switch o.summary {
	case "true":
		return node.Summary()
	case "text":
		return node.SummaryText()
	case "data":
		return node.SummaryData()
	default:
		return node
	}
}

// parseSearch turns a query string into a storage query plus the options that
// shape the response.
func parseSearch(idx *conformance.Index, resourceType string, values url.Values) (storage.SearchQuery, searchOptions, error) {
	q := storage.SearchQuery{Type: resourceType}
	var opts searchOptions

	for rawName, rawValues := range values {
		name, _, _ := strings.Cut(rawName, ":")
		if controlParams[name] {
			continue
		}
		for _, raw := range rawValues {
			matches, err := parseOneParam(idx, resourceType, rawName, raw)
			if err != nil {
				return q, opts, err
			}
			q.Params = append(q.Params, matches...)
		}
	}

	if err := parseControls(idx, resourceType, values, &q, &opts); err != nil {
		return q, opts, err
	}
	return q, opts, nil
}

// parseOneParam dispatches on the shape of a parameter name: a reverse chain,
// a forward chain, or a plain parameter.
//
// The dot binds before the colon: in "subject:Patient.family" the modifier
// belongs to the reference, not to the chained parameter.
func parseOneParam(idx *conformance.Index, resourceType, rawName, raw string) ([]storage.ParamMatch, error) {
	return parseNamedParam(idx, resourceType, rawName, queryLeaf(idx, raw))
}

// queryLeaf builds the innermost parameter of a query-string search, where the
// name may carry a modifier and the value is a comma-separated alternative
// list.
func queryLeaf(idx *conformance.Index, raw string) leafBuilder {
	return func(resourceType, name string) (storage.ParamMatch, error) {
		base, modifier, _ := strings.Cut(name, ":")
		if sp, ok := idx.SearchParam(resourceType, base); ok && sp.Type == "composite" {
			return parseComposite(idx, sp, modifier, raw)
		}
		sp, kind, err := resolveParam(idx, resourceType, base)
		if err != nil {
			return storage.ParamMatch{}, err
		}
		return parseParam(sp, kind, modifier, raw)
	}
}

// resolveParam finds a search parameter and the index it uses. _id and
// _lastUpdated are properties of the resource itself rather than indexed
// values, so they resolve without consulting the index.
func resolveParam(idx *conformance.Index, resourceType, name string) (*conformance.SearchParam, storage.IndexKind, error) {
	switch name {
	case "_id":
		return &conformance.SearchParam{Code: "_id", Type: "token"}, storage.IndexToken, nil
	case "_lastUpdated":
		return &conformance.SearchParam{Code: "_lastUpdated", Type: "date"}, storage.IndexDate, nil
	case "_text", "_content":
		// Full-text over the narrative and over the whole resource. Both are
		// "special" parameters: the specification gives them no FHIRPath
		// expression because they are not extracted from one element.
		return &conformance.SearchParam{Code: name, Type: "special"}, storage.IndexFullText, nil
	}
	sp, ok := idx.SearchParam(resourceType, name)
	if !ok {
		return nil, "", &searchError{fmt.Sprintf("%s does not support the search parameter %q", resourceType, name)}
	}
	kind, ok := indexKindFor(sp.Type)
	if !ok {
		return nil, "", &searchError{fmt.Sprintf("search parameter %q is of type %q, which is not supported yet", name, sp.Type)}
	}
	return sp, kind, nil
}

func indexKindFor(paramType string) (storage.IndexKind, bool) {
	switch paramType {
	case "string":
		return storage.IndexString, true
	case "token":
		return storage.IndexToken, true
	case "reference":
		return storage.IndexReference, true
	case "date":
		return storage.IndexDate, true
	case "quantity":
		return storage.IndexQuantity, true
	case "uri":
		return storage.IndexURI, true
	case "number":
		return storage.IndexNumber, true
	}
	return "", false
}

// searchError is a client mistake in a query, reported as 400 rather than 500.
type searchError struct{ msg string }

func (e *searchError) Error() string { return e.msg }

// parseParam builds one parameter match. A comma-separated value list is an
// "or"; that is why Values is a slice.
func parseParam(sp *conformance.SearchParam, kind storage.IndexKind, modifier, raw string) (storage.ParamMatch, error) {
	match := storage.ParamMatch{Code: sp.Code, Kind: kind}

	if modifier == "missing" {
		if raw != "true" && raw != "false" {
			return match, &searchError{fmt.Sprintf(":missing takes true or false, got %q", raw)}
		}
		missing := raw == "true"
		match.Values = []storage.MatchValue{{Missing: &missing}}
		return match, nil
	}

	if err := checkModifier(sp, kind, modifier); err != nil {
		return match, err
	}
	switch modifier {
	case "not":
		match.Negate = true
		modifier = ""
	case "text":
		match.TextSearch = true
	}
	// A reference parameter may be modified by a target type -- subject:Patient
	// -- which restricts rather than transforms the match.
	refType := ""
	if kind == storage.IndexReference && isResourceTypeModifier(sp, modifier) {
		refType = modifier
		modifier = ""
	}

	for _, part := range splitAlternatives(raw) {
		value, err := parseValue(kind, modifier, part)
		if err != nil {
			return match, err
		}
		if refType != "" {
			value.RefType, value.RefID = refType, firstNonEmpty(value.RefID, value.URI, value.Text)
		}
		match.Values = append(match.Values, value)
	}
	return match, nil
}

// isResourceTypeModifier reports whether a modifier names one of the reference
// parameter's declared targets.
func isResourceTypeModifier(sp *conformance.SearchParam, modifier string) bool {
	if modifier == "" {
		return false
	}
	for _, target := range sp.Targets {
		if target == modifier {
			return true
		}
	}
	return false
}

// modifiersByKind lists the modifiers each parameter type accepts here.
//
// The ones deliberately absent are the terminology-dependent ones -- :in,
// :not-in, and :above/:below on a token -- which need a value set expansion or
// a code system hierarchy to answer. Accepting them and returning nothing would
// be worse than refusing: a client would read an empty result as "no matching
// resources" rather than "this server cannot answer that".
var modifiersByKind = map[storage.IndexKind]map[string]bool{
	storage.IndexFullText:  {"not": true},
	storage.IndexString:    {"exact": true, "contains": true},
	storage.IndexToken:     {"not": true, "text": true, "of-type": true},
	storage.IndexReference: {"identifier": true},
	storage.IndexURI:       {"above": true, "below": true},
}

// unsupportedModifiers are recognized by the specification but need
// terminology this server does not have.
var unsupportedModifiers = map[string]string{
	"in":     "a value set expansion",
	"not-in": "a value set expansion",
	"above":  "a code system hierarchy",
	"below":  "a code system hierarchy",
}

func checkModifier(sp *conformance.SearchParam, kind storage.IndexKind, modifier string) error {
	if modifier == "" {
		return nil
	}
	if modifiersByKind[kind][modifier] {
		return nil
	}
	if kind == storage.IndexReference && isResourceTypeModifier(sp, modifier) {
		return nil
	}
	if need, known := unsupportedModifiers[modifier]; known {
		return &searchError{fmt.Sprintf(
			"the :%s modifier needs %s, which this server does not provide", modifier, need)}
	}
	return &searchError{fmt.Sprintf(
		"the :%s modifier is not valid for the %s parameter %q", modifier, sp.Type, sp.Code)}
}

// splitAlternatives splits on commas, honouring the backslash escape the
// specification defines so a value containing a literal comma survives.
func splitAlternatives(raw string) []string {
	parts := splitEscaped(raw, ',')
	for i, part := range parts {
		parts[i] = unescapeValue(part)
	}
	return parts
}

// splitEscaped splits on a separator, skipping over backslash-escaped
// characters but leaving the escapes in place.
//
// Escapes survive because a composite value is split twice -- on the comma
// first, then on the "$" -- and unescaping in between would turn a literal
// "\$" into a separator for the second pass.
func splitEscaped(raw string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(raw); i++ {
		switch {
		case raw[i] == '\\' && i+1 < len(raw):
			i++
		case raw[i] == sep:
			out = append(out, raw[start:i])
			start = i + 1
		}
	}
	return append(out, raw[start:])
}

// unescapeValue removes the backslash escapes once a value is no longer going
// to be split.
func unescapeValue(raw string) string {
	if !strings.Contains(raw, "\\") {
		return raw
	}
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			i++
		}
		b.WriteByte(raw[i])
	}
	return b.String()
}

func parseValue(kind storage.IndexKind, modifier, raw string) (storage.MatchValue, error) {
	switch kind {
	case storage.IndexToken:
		if modifier == "text" {
			// :text redirects to the string index, so the value is a string
			// match rather than a coded one.
			return storage.MatchValue{Text: storage.Normalize(raw), Match: storage.MatchPrefix}, nil
		}
		if modifier == "of-type" {
			// "system|code|value": the identifier's type, then its value.
			parts := strings.SplitN(raw, "|", 3)
			if len(parts) != 3 {
				return storage.MatchValue{}, &searchError{
					":of-type takes system|code|value"}
			}
			return storage.MatchValue{Code: parts[2]}, nil
		}
		// "system|code", "system|", "|code", or a bare code.
		if system, code, hasPipe := strings.Cut(raw, "|"); hasPipe {
			return storage.MatchValue{System: system, Code: code}, nil
		}
		return storage.MatchValue{Code: raw}, nil

	case storage.IndexString:
		switch modifier {
		case "exact":
			return storage.MatchValue{Text: raw, Exact: true, Match: storage.MatchExact}, nil
		case "contains":
			return storage.MatchValue{Text: storage.Normalize(raw), Match: storage.MatchContains}, nil
		default:
			return storage.MatchValue{Text: storage.Normalize(raw), Match: storage.MatchPrefix}, nil
		}

	case storage.IndexReference:
		if modifier == "identifier" {
			// :identifier matches the reference's own identifier, which
			// extraction indexes as a token under the same code.
			if system, code, hasPipe := strings.Cut(raw, "|"); hasPipe {
				return storage.MatchValue{System: system, Code: code}, nil
			}
			return storage.MatchValue{Code: raw}, nil
		}
		// "Patient/123", a bare id, or an absolute URL.
		if strings.Contains(raw, "://") {
			return storage.MatchValue{RefURL: raw}, nil
		}
		if typeName, id, ok := strings.Cut(raw, "/"); ok {
			return storage.MatchValue{RefType: typeName, RefID: id}, nil
		}
		return storage.MatchValue{RefID: raw}, nil

	case storage.IndexURI:
		return storage.MatchValue{
			URI:      raw,
			URIAbove: modifier == "above",
			URIBelow: modifier == "below",
		}, nil

	case storage.IndexDate:
		prefix, rest := splitPrefix(raw)
		low, high, err := storage.DateRange(rest)
		if err != nil {
			return storage.MatchValue{}, &searchError{fmt.Sprintf("invalid date %q", rest)}
		}
		return storage.MatchValue{Prefix: prefix, DateLow: low, DateHigh: high}, nil

	case storage.IndexNumber:
		prefix, rest := splitPrefix(raw)
		low, high, err := storage.NumberRange(rest)
		if err != nil {
			return storage.MatchValue{}, &searchError{fmt.Sprintf("invalid number %q", rest)}
		}
		return storage.MatchValue{Prefix: prefix, NumLow: low, NumHigh: high}, nil

	case storage.IndexFullText:
		return storage.MatchValue{Text: raw}, nil

	case storage.IndexQuantity:
		// "[prefix]number|system|code"
		prefix, rest := splitPrefix(raw)
		number, system, code := rest, "", ""
		if n, remainder, ok := strings.Cut(rest, "|"); ok {
			number = n
			system, code, _ = strings.Cut(remainder, "|")
		}
		low, high, err := storage.NumberRange(number)
		if err != nil {
			return storage.MatchValue{}, &searchError{fmt.Sprintf("invalid quantity %q", raw)}
		}
		return storage.MatchValue{
			Prefix: prefix, NumLow: low, NumHigh: high,
			QuantitySystem: system, QuantityCode: code,
		}, nil
	}
	return storage.MatchValue{}, &searchError{fmt.Sprintf("unsupported search type %q", kind)}
}

// searchPrefixes are the two-letter comparators a date, number, or quantity
// value may carry.
var searchPrefixes = []string{"eq", "ne", "gt", "lt", "ge", "le", "sa", "eb", "ap"}

// splitPrefix separates a comparison prefix from its value. A prefix is only
// recognized when what follows looks like a value, so an identifier beginning
// with "eq" is not mistaken for one.
func splitPrefix(raw string) (prefix, rest string) {
	if len(raw) < 3 {
		return "", raw
	}
	candidate := raw[:2]
	for _, p := range searchPrefixes {
		if candidate != p {
			continue
		}
		next := raw[2]
		if next == '-' || next == '+' || next == '.' || (next >= '0' && next <= '9') {
			return candidate, raw[2:]
		}
	}
	return "", raw
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseControls reads the parameters that shape the response.
func parseControls(idx *conformance.Index, resourceType string, values url.Values,
	q *storage.SearchQuery, opts *searchOptions) error {
	if raw := values.Get("_count"); raw != "" {
		count, err := strconv.Atoi(raw)
		if err != nil || count < 0 {
			return &searchError{fmt.Sprintf("_count must be a non-negative integer, got %q", raw)}
		}
		if count > maxPageSize {
			count = maxPageSize
		}
		if count == 0 {
			// _count=0 asks for the total only, which the bundle reports
			// without entries.
			count = -1
		}
		q.Count = count
	}
	if raw := values.Get("_offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return &searchError{fmt.Sprintf("_offset must be a non-negative integer, got %q", raw)}
		}
		q.Offset = offset
	}
	for _, raw := range values["_sort"] {
		for _, field := range strings.Split(raw, ",") {
			key, err := parseSortKey(idx, resourceType, field)
			if err != nil {
				return err
			}
			q.SortBy = append(q.SortBy, key)
		}
	}
	q.Cursor = values.Get("_cursor")

	switch total := values.Get("_total"); total {
	case "", "accurate":
		// Accurate is the default here. The server is a test target, where a
		// wrong count is more confusing than a slow one.
	case "none":
		q.SkipTotal = true
	case "estimate":
		// No cheaper estimate exists on this schema, so an accurate count is
		// returned. That satisfies the request: an estimate may be exact.
	default:
		return &searchError{fmt.Sprintf("_total must be none, estimate, or accurate, got %q", total)}
	}

	for rawName, rawValues := range values {
		base, modifier, _ := strings.Cut(rawName, ":")
		if base != "_include" && base != "_revinclude" {
			continue
		}
		if modifier != "" && modifier != "iterate" && modifier != "recurse" {
			return &searchError{fmt.Sprintf(
				"the only modifier %s accepts is :iterate, got :%s", base, modifier)}
		}
		for _, raw := range rawValues {
			spec, err := parseInclude(idx, resourceType, raw, base == "_revinclude", modifier != "")
			if err != nil {
				return err
			}
			opts.includes = append(opts.includes, spec)
		}
	}

	// Several _filter parameters are a conjunction, like any other repeated
	// search parameter.
	for _, raw := range values["_filter"] {
		expr, err := parseFilter(idx, resourceType, raw)
		if err != nil {
			return err
		}
		if expr == nil {
			continue
		}
		switch {
		case q.Filter == nil:
			q.Filter = expr
		case q.Filter.Op == storage.FilterAnd:
			q.Filter.Operands = append(q.Filter.Operands, expr)
		default:
			q.Filter = &storage.FilterExpr{
				Op:       storage.FilterAnd,
				Operands: []*storage.FilterExpr{q.Filter, expr},
			}
		}
	}

	if err := parseSubsetOptions(values, opts); err != nil {
		return err
	}
	if opts.countOnly {
		q.Count = -1
	}
	return nil
}

// parseSubsetOptions reads _summary and _elements, which apply to a read as
// much as to a search.
func parseSubsetOptions(values url.Values, opts *searchOptions) error {
	switch summary := values.Get("_summary"); summary {
	case "", "false":
	case "true", "text", "data":
		opts.summary = summary
	case "count":
		opts.summary, opts.countOnly = summary, true
	default:
		return &searchError{fmt.Sprintf(
			"_summary must be true, text, data, count, or false, got %q", summary)}
	}

	if raw := values.Get("_elements"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				opts.elements = append(opts.elements, name)
			}
		}
	}
	return nil
}

func parseSortKey(idx *conformance.Index, resourceType, field string) (storage.SortKey, error) {
	key := storage.SortKey{}
	if rest, descending := strings.CutPrefix(field, "-"); descending {
		key.Descending = true
		field = rest
	}
	key.Code = field
	switch field {
	case "_id", "_lastUpdated":
		return key, nil
	}
	sp, kind, err := resolveParam(idx, resourceType, field)
	if err != nil {
		return key, err
	}
	key.Code, key.Kind = sp.Code, kind
	return key, nil
}
