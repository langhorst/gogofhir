package rest

import (
	"fmt"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/storage"
	"github.com/langhorst/gogofhir/internal/storage/index"
)

// _filter, the one FHIR search parameter that carries its own query language.
//
// Everything else in a query string is a conjunction of parameters, each with
// its own value list; _filter adds "or", "not", and grouping, so it needs a
// grammar and a tree. The leaves are ordinary parameter matches -- chains and
// _has included -- which is why this file only builds the values and hands the
// name resolution to the same code the query string uses.
//
// Grammar, following the specification:
//
//	filter   := orExpr
//	orExpr   := andExpr ("or" andExpr)*
//	andExpr  := unary ("and" unary)*
//	unary    := "not" "(" filter ")" | "(" filter ")" | paramExp
//	paramExp := path operator [value]

// parseFilter parses a _filter expression against a resource type.
func parseFilter(idx *conformance.Index, resourceType, raw string) (*storage.FilterExpr, error) {
	tokens, err := lexFilter(raw)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, nil
	}
	p := &filterParser{tokens: tokens, idx: idx, resourceType: resourceType}
	expr, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.tokens) {
		return nil, &searchError{fmt.Sprintf(
			"_filter has trailing input starting at %q", p.tokens[p.pos].text)}
	}
	return expr, nil
}

// ---- lexing ----

type filterTokenKind int

const (
	filterWord filterTokenKind = iota
	filterString
	filterLParen
	filterRParen
)

type filterToken struct {
	kind filterTokenKind
	text string
}

// lexFilter splits an expression into words, quoted strings, and parentheses.
//
// Operators are ordinary words, so "not(x eq 1)" needs no space before the
// parenthesis: a bracket always ends the word it follows.
func lexFilter(raw string) ([]filterToken, error) {
	var tokens []filterToken
	for i := 0; i < len(raw); {
		switch c := raw[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			tokens = append(tokens, filterToken{filterLParen, "("})
			i++
		case c == ')':
			tokens = append(tokens, filterToken{filterRParen, ")"})
			i++
		case c == '"':
			var b strings.Builder
			i++
			for i < len(raw) && raw[i] != '"' {
				if raw[i] == '\\' && i+1 < len(raw) {
					i++
				}
				b.WriteByte(raw[i])
				i++
			}
			if i == len(raw) {
				return nil, &searchError{"_filter has an unterminated quoted string"}
			}
			i++ // closing quote
			tokens = append(tokens, filterToken{filterString, b.String()})
		default:
			start := i
			for i < len(raw) && !isFilterBreak(raw[i]) {
				if raw[i] == '\\' && i+1 < len(raw) {
					i++
				}
				i++
			}
			tokens = append(tokens, filterToken{filterWord, unescapeValue(raw[start:i])})
		}
	}
	return tokens, nil
}

func isFilterBreak(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '(' || c == ')'
}

// ---- parsing ----

type filterParser struct {
	tokens       []filterToken
	pos          int
	idx          *conformance.Index
	resourceType string
}

func (p *filterParser) peek() (filterToken, bool) {
	if p.pos >= len(p.tokens) {
		return filterToken{}, false
	}
	return p.tokens[p.pos], true
}

func (p *filterParser) next() (filterToken, bool) {
	token, ok := p.peek()
	if ok {
		p.pos++
	}
	return token, ok
}

// takeWord consumes the next token if it is the given keyword, case-insensitive
// as the specification's operators are.
func (p *filterParser) takeWord(word string) bool {
	token, ok := p.peek()
	if !ok || token.kind != filterWord || !strings.EqualFold(token.text, word) {
		return false
	}
	p.pos++
	return true
}

func (p *filterParser) parseOr() (*storage.FilterExpr, error) {
	return p.parseBinary("or", storage.FilterOr, p.parseAnd)
}

func (p *filterParser) parseAnd() (*storage.FilterExpr, error) {
	return p.parseBinary("and", storage.FilterAnd, p.parseUnary)
}

// parseBinary collects a left-associative run of one connective into a single
// n-ary node, so "a or b or c" is one node rather than a lopsided pair.
func (p *filterParser) parseBinary(word string, op storage.FilterOp,
	operand func() (*storage.FilterExpr, error)) (*storage.FilterExpr, error) {
	first, err := operand()
	if err != nil {
		return nil, err
	}
	operands := []*storage.FilterExpr{first}
	for p.takeWord(word) {
		next, err := operand()
		if err != nil {
			return nil, err
		}
		operands = append(operands, next)
	}
	if len(operands) == 1 {
		return first, nil
	}
	return &storage.FilterExpr{Op: op, Operands: operands}, nil
}

func (p *filterParser) parseUnary() (*storage.FilterExpr, error) {
	token, ok := p.peek()
	if !ok {
		return nil, &searchError{"_filter ended where an expression was expected"}
	}
	if token.kind == filterWord && strings.EqualFold(token.text, "not") {
		p.pos++
		inner, err := p.parseGroup()
		if err != nil {
			return nil, err
		}
		return &storage.FilterExpr{Op: storage.FilterNot, Operands: []*storage.FilterExpr{inner}}, nil
	}
	if token.kind == filterLParen {
		return p.parseGroup()
	}
	return p.parseParamExp()
}

func (p *filterParser) parseGroup() (*storage.FilterExpr, error) {
	token, ok := p.next()
	if !ok || token.kind != filterLParen {
		return nil, &searchError{"_filter expects a parenthesized expression here"}
	}
	inner, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if token, ok := p.next(); !ok || token.kind != filterRParen {
		return nil, &searchError{"_filter has an unclosed parenthesis"}
	}
	return inner, nil
}

// parseParamExp reads one "path operator value" comparison.
func (p *filterParser) parseParamExp() (*storage.FilterExpr, error) {
	pathToken, ok := p.next()
	if !ok || pathToken.kind != filterWord {
		return nil, &searchError{"_filter expects a search parameter name here"}
	}
	operatorToken, ok := p.next()
	if !ok || operatorToken.kind != filterWord {
		return nil, &searchError{fmt.Sprintf(
			"_filter expects a comparison operator after %q", pathToken.text)}
	}
	operator := strings.ToLower(operatorToken.text)

	value := ""
	if operator == "pr" {
		// Presence takes true or false; nothing else is a valid operand, so a
		// bare "pr" is read as "pr true".
		if token, ok := p.peek(); ok && token.kind == filterWord &&
			(token.text == "true" || token.text == "false") {
			value = token.text
			p.pos++
		} else {
			value = "true"
		}
	} else {
		token, ok := p.next()
		if !ok || (token.kind != filterWord && token.kind != filterString) {
			return nil, &searchError{fmt.Sprintf(
				"_filter expects a value after %q %s", pathToken.text, operator)}
		}
		value = token.text
	}

	// A filter path names a chained type in brackets -- subject[Patient].name --
	// where a query string uses a colon. Rewriting is enough to reuse the same
	// resolution.
	name := strings.NewReplacer("[", ":", "]", "").Replace(pathToken.text)
	matches, err := parseNamedParam(p.idx, p.resourceType, name, filterLeaf(p.idx, operator, value))
	if err != nil {
		return nil, err
	}
	return &storage.FilterExpr{Match: &matches[0]}, nil
}

// ---- leaves ----

// filterUnsupported are operators the specification defines but which need
// terminology or reference resolution this server does not have. Refusing them
// is deliberate: answering nothing would read as "no matching resources".
var filterUnsupported = map[string]string{
	"in": "a value set expansion",
	"ni": "a value set expansion",
	"ss": "a code system hierarchy",
	"sb": "a code system hierarchy",
	"re": "reference identifier resolution",
}

// filterOrderedOps are the comparisons a date, number, or quantity accepts.
// "po" (overlaps) is the same interval test the default comparison already
// performs, so it maps onto it rather than being refused.
var filterOrderedOps = map[string]string{
	"eq": "eq", "ne": "ne", "gt": "gt", "lt": "lt", "ge": "ge",
	"le": "le", "sa": "sa", "eb": "eb", "ap": "ap", "po": "",
}

// filterLeaf builds the innermost parameter of a filter expression, where the
// comparison is an operator rather than a modifier and there is exactly one
// value.
func filterLeaf(idx *conformance.Index, operator, value string) leafBuilder {
	return func(resourceType, name string) (storage.ParamMatch, error) {
		base, modifier, _ := strings.Cut(name, ":")
		if sp, ok := idx.SearchParam(resourceType, base); ok && sp.Type == "composite" {
			return storage.ParamMatch{}, &searchError{fmt.Sprintf(
				"the composite parameter %q cannot be used in _filter; name its components instead", base)}
		}
		sp, kind, err := resolveParam(idx, resourceType, base)
		if err != nil {
			return storage.ParamMatch{}, err
		}
		if modifier != "" && !(kind == index.Reference && isResourceTypeModifier(sp, modifier)) {
			return storage.ParamMatch{}, &searchError{fmt.Sprintf(
				"%q does not reference %s", sp.Code, modifier)}
		}
		if need, known := filterUnsupported[operator]; known {
			return storage.ParamMatch{}, &searchError{fmt.Sprintf(
				"the _filter operator %q needs %s, which this server does not provide", operator, need)}
		}

		match := storage.ParamMatch{Code: sp.Code, Kind: kind}
		if operator == "pr" {
			missing := value == "false"
			match.Values = []storage.MatchValue{{Missing: &missing}}
			return match, nil
		}

		matchValue, err := filterValue(kind, sp, operator, value, &match)
		if err != nil {
			return match, err
		}
		if modifier != "" {
			matchValue.RefType = modifier
			matchValue.RefID = firstNonEmpty(matchValue.RefID, matchValue.URI, matchValue.Text)
		}
		match.Values = []storage.MatchValue{matchValue}
		return match, nil
	}
}

// filterValue turns one operator and value into a match value, setting Negate
// on the enclosing match where the operator is a negation.
func filterValue(kind index.Kind, sp *conformance.SearchParam,
	operator, value string, match *storage.ParamMatch) (storage.MatchValue, error) {
	switch kind {
	case index.String:
		switch operator {
		case "eq", "ne":
			// "eq" on a string is equality, not the prefix match a bare query
			// parameter performs -- the operator says what it means.
			match.Negate = operator == "ne"
			return storage.MatchValue{Text: value, Match: storage.MatchExact}, nil
		case "co":
			return storage.MatchValue{Text: index.Normalize(value), Match: storage.MatchContains}, nil
		case "sw":
			return storage.MatchValue{Text: index.Normalize(value), Match: storage.MatchPrefix}, nil
		case "ew":
			return storage.MatchValue{Text: index.Normalize(value), Match: storage.MatchEndsWith}, nil
		}

	case index.FullText:
		switch operator {
		case "eq", "co":
			return storage.MatchValue{Text: value}, nil
		case "ne":
			match.Negate = true
			return storage.MatchValue{Text: value}, nil
		}

	case index.Token, index.Reference, index.URI:
		switch operator {
		case "eq", "ne":
			match.Negate = operator == "ne"
			return parseValue(kind, "", value)
		case "sw":
			if kind == index.URI {
				return storage.MatchValue{URI: value, URIBelow: true}, nil
			}
		}

	case index.Date, index.Number, index.Quantity:
		prefix, ok := filterOrderedOps[operator]
		if !ok {
			break
		}
		matchValue, err := parseValue(kind, "", value)
		if err != nil {
			return matchValue, err
		}
		// The operator is the comparison, so it replaces whatever prefix the
		// value may have carried.
		matchValue.Prefix = prefix
		return matchValue, nil
	}
	return storage.MatchValue{}, &searchError{fmt.Sprintf(
		"the _filter operator %q is not valid for the %s parameter %q", operator, sp.Type, sp.Code)}
}
