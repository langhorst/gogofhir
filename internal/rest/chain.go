package rest

import (
	"fmt"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/storage"
	"github.com/langhorst/gogofhir/internal/storage/index"
)

// Chained search, reverse chaining, and includes.
//
// These are the parts of FHIR search that reach beyond one resource. All three
// resolve names against the conformance index before they reach storage, so an
// unresolvable chain is a 400 explaining which link failed rather than an empty
// result -- with joins especially, "no matches" and "your query was wrong" look
// identical from the outside, and a client cannot tell them apart.

// leafBuilder resolves the innermost parameter of a chain against the type the
// chain arrived at.
//
// Chains, _has, and plain parameters are shared between ordinary query-string
// search and _filter; the two differ only at the leaf, where one has a modifier
// and a comma-separated value list and the other an operator and a single
// value. Passing the leaf in keeps one implementation of the join logic.
type leafBuilder func(resourceType, name string) (storage.ParamMatch, error)

// parseChained builds a chained parameter from a dotted name such as
// "subject.name", "subject:Patient.name", or
// "general-practitioner.organization.name".
//
// The dot is split before the colon, and the order matters: in
// "subject:Patient.family" the type modifier belongs to the reference and the
// rest is the chained parameter. Splitting on the colon first reads the whole
// of "Patient.family" as a modifier, which then fails as an unknown one.
func parseChained(idx *conformance.Index, resourceType, rawName string, leaf leafBuilder) (storage.ParamMatch, error) {
	head, rest, _ := strings.Cut(rawName, ".")
	headName, modifier, _ := strings.Cut(head, ":")

	sp, ok := idx.SearchParam(resourceType, headName)
	if !ok {
		return storage.ParamMatch{}, &searchError{fmt.Sprintf(
			"%s does not support the search parameter %q, so %q cannot be chained through it",
			resourceType, headName, rawName)}
	}
	if sp.Type != "reference" {
		return storage.ParamMatch{}, &searchError{fmt.Sprintf(
			"%q is a %s parameter, and only reference parameters can be chained",
			headName, sp.Type)}
	}

	targetType, err := chainTarget(idx, sp, modifier, rest)
	if err != nil {
		return storage.ParamMatch{}, err
	}

	match := storage.ParamMatch{
		Code:  sp.Code,
		Kind:  index.Reference,
		Chain: &storage.Chain{TargetType: targetType},
	}
	nested, err := parseNamedParam(idx, targetType, rest, leaf)
	if err != nil {
		return storage.ParamMatch{}, err
	}
	match.Chain.Params = nested
	return match, nil
}

// chainTarget decides which referenced type a chain follows.
//
// A reference may point at many types, and the chained parameter has to be
// resolved against one of them. The type modifier says which; failing that, a
// reference with a single target is unambiguous, and otherwise the one target
// that actually defines the next parameter is taken. Only a genuine ambiguity
// is an error, and it names the candidates rather than guessing -- guessing
// would silently search the wrong type.
func chainTarget(idx *conformance.Index, sp *conformance.SearchParam, modifier, rest string) (string, error) {
	if modifier != "" {
		for _, target := range sp.Targets {
			if target == modifier {
				return target, nil
			}
		}
		return "", &searchError{fmt.Sprintf(
			"%q does not reference %s; it references %s",
			sp.Code, modifier, strings.Join(sp.Targets, ", "))}
	}
	if len(sp.Targets) == 1 {
		return sp.Targets[0], nil
	}

	next, _, _ := strings.Cut(rest, ".")
	var candidates []string
	for _, target := range sp.Targets {
		if _, ok := idx.SearchParam(target, next); ok {
			candidates = append(candidates, target)
		}
	}
	switch len(candidates) {
	case 0:
		return "", &searchError{fmt.Sprintf(
			"none of the types %q references define the search parameter %q",
			sp.Code, next)}
	case 1:
		return candidates[0], nil
	default:
		return "", &searchError{fmt.Sprintf(
			"%q is ambiguous: %s all define %q, so name one with %s:Type.%s",
			sp.Code, strings.Join(candidates, ", "), next, sp.Code, next)}
	}
}

// parseHas builds a reverse-chained parameter from a name such as
// "_has:Observation:subject:code".
//
// The shape is _has:{type}:{reference}:{parameter}, and the final part may be
// another _has, which is how the specification expresses two-level reverse
// chains.
func parseHas(idx *conformance.Index, resourceType, name string, leaf leafBuilder) (storage.ParamMatch, error) {
	parts := strings.SplitN(name, ":", 4)
	if len(parts) < 4 || parts[0] != "_has" {
		return storage.ParamMatch{}, &searchError{
			"_has takes the form _has:Type:reference:parameter"}
	}
	sourceType, referenceCode, target := parts[1], parts[2], parts[3]

	if !idx.IsResource(sourceType) {
		return storage.ParamMatch{}, &searchError{fmt.Sprintf(
			"_has names an unknown resource type %q", sourceType)}
	}
	refParam, ok := idx.SearchParam(sourceType, referenceCode)
	if !ok {
		return storage.ParamMatch{}, &searchError{fmt.Sprintf(
			"%s does not support the search parameter %q", sourceType, referenceCode)}
	}
	if refParam.Type != "reference" {
		return storage.ParamMatch{}, &searchError{fmt.Sprintf(
			"_has needs a reference parameter, but %s.%s is a %s",
			sourceType, referenceCode, refParam.Type)}
	}

	nested, err := parseNamedParam(idx, sourceType, target, leaf)
	if err != nil {
		return storage.ParamMatch{}, err
	}
	return storage.ParamMatch{
		Code: refParam.Code,
		Kind: index.Reference,
		Has: &storage.Has{
			SourceType: sourceType,
			Code:       refParam.Code,
			Params:     nested,
		},
	}, nil
}

// parseNamedParam resolves one parameter name against a type, following further
// chains or _has clauses. It returns a slice because that is what the nested
// forms hold.
func parseNamedParam(idx *conformance.Index, resourceType, name string, leaf leafBuilder) ([]storage.ParamMatch, error) {
	var (
		match storage.ParamMatch
		err   error
	)
	switch {
	case strings.HasPrefix(name, "_has:"):
		match, err = parseHas(idx, resourceType, name, leaf)
	case strings.Contains(name, "."):
		match, err = parseChained(idx, resourceType, name, leaf)
	default:
		match, err = leaf(resourceType, name)
	}
	if err != nil {
		return nil, err
	}
	return []storage.ParamMatch{match}, nil
}

// parseInclude reads an _include or _revinclude value.
//
// The form is Type:parameter[:targetType], or "*" for every reference.
func parseInclude(idx *conformance.Index, searchType, raw string, reverse, iterate bool) (storage.IncludeSpec, error) {
	spec := storage.IncludeSpec{Reverse: reverse, Iterate: iterate}

	if raw == "*" {
		if reverse {
			return spec, &searchError{"_revinclude=* is not supported; name the resource type and parameter"}
		}
		spec.Wildcard, spec.SourceType = true, searchType
		return spec, nil
	}

	parts := strings.Split(raw, ":")
	if len(parts) < 2 {
		return spec, &searchError{fmt.Sprintf(
			"%s takes the form Type:parameter or Type:parameter:targetType, got %q",
			includeName(reverse), raw)}
	}
	spec.SourceType, spec.Code = parts[0], parts[1]
	if len(parts) > 2 {
		spec.TargetType = parts[2]
	}

	if !idx.IsResource(spec.SourceType) {
		return spec, &searchError{fmt.Sprintf(
			"%s names an unknown resource type %q", includeName(reverse), spec.SourceType)}
	}
	sp, ok := idx.SearchParam(spec.SourceType, spec.Code)
	if !ok {
		return spec, &searchError{fmt.Sprintf(
			"%s does not support the search parameter %q", spec.SourceType, spec.Code)}
	}
	if sp.Type != "reference" {
		return spec, &searchError{fmt.Sprintf(
			"%s needs a reference parameter, but %s.%s is a %s",
			includeName(reverse), spec.SourceType, spec.Code, sp.Type)}
	}
	spec.Code = sp.Code

	// A forward include starts from the matches, so its type has to be the one
	// being searched -- unless it iterates, where the point is to follow what
	// an earlier include already pulled in.
	if !reverse && !iterate && spec.SourceType != searchType {
		return spec, &searchError{fmt.Sprintf(
			"_include=%s applies to %s, but the search is over %s; use _include:iterate to follow it",
			raw, spec.SourceType, searchType)}
	}
	return spec, nil
}

func includeName(reverse bool) string {
	if reverse {
		return "_revinclude"
	}
	return "_include"
}
