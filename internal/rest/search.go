package rest

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
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
}

// parseSearch turns a query string into a storage query.
func parseSearch(idx *conformance.Index, resourceType string, values url.Values) (storage.SearchQuery, error) {
	q := storage.SearchQuery{Type: resourceType}

	for rawName, rawValues := range values {
		name, modifier, _ := strings.Cut(rawName, ":")
		if controlParams[name] {
			continue
		}
		sp, kind, err := resolveParam(idx, resourceType, name)
		if err != nil {
			return q, err
		}
		for _, raw := range rawValues {
			match, err := parseParam(sp, kind, modifier, raw)
			if err != nil {
				return q, err
			}
			q.Params = append(q.Params, match)
		}
	}

	if err := parseControls(idx, resourceType, values, &q); err != nil {
		return q, err
	}
	return q, nil
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
		missing := raw == "true"
		match.Values = []storage.MatchValue{{Missing: &missing}}
		return match, nil
	}

	for _, part := range splitAlternatives(raw) {
		value, err := parseValue(kind, modifier, part)
		if err != nil {
			return match, err
		}
		match.Values = append(match.Values, value)
	}
	return match, nil
}

// splitAlternatives splits on commas, honouring the backslash escape the
// specification defines so a value containing a literal comma survives.
func splitAlternatives(raw string) []string {
	var out []string
	var current strings.Builder
	for i := 0; i < len(raw); i++ {
		switch {
		case raw[i] == '\\' && i+1 < len(raw):
			current.WriteByte(raw[i+1])
			i++
		case raw[i] == ',':
			out = append(out, current.String())
			current.Reset()
		default:
			current.WriteByte(raw[i])
		}
	}
	out = append(out, current.String())
	return out
}

func parseValue(kind storage.IndexKind, modifier, raw string) (storage.MatchValue, error) {
	switch kind {
	case storage.IndexToken:
		// "system|code", "system|", "|code", or a bare code.
		if system, code, hasPipe := strings.Cut(raw, "|"); hasPipe {
			return storage.MatchValue{System: system, Code: code}, nil
		}
		return storage.MatchValue{Code: raw}, nil

	case storage.IndexString:
		return storage.MatchValue{
			Text:  pick(modifier == "exact", raw, storage.Normalize(raw)),
			Exact: modifier == "exact",
		}, nil

	case storage.IndexReference:
		// "Patient/123", a bare id, or an absolute URL.
		if strings.Contains(raw, "://") {
			return storage.MatchValue{RefURL: raw}, nil
		}
		if typeName, id, ok := strings.Cut(raw, "/"); ok {
			return storage.MatchValue{RefType: typeName, RefID: id}, nil
		}
		return storage.MatchValue{RefID: raw}, nil

	case storage.IndexURI:
		return storage.MatchValue{URI: raw}, nil

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

func pick(cond bool, whenTrue, whenFalse string) string {
	if cond {
		return whenTrue
	}
	return whenFalse
}

// parseControls reads the parameters that shape the response.
func parseControls(idx *conformance.Index, resourceType string, values url.Values, q *storage.SearchQuery) error {
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
