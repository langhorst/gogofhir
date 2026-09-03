package rest

import (
	"fmt"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/storage"
)

// Composite search parameters.
//
// A composite constrains several components of the *same* occurrence:
// code-value-quantity=http://loinc.org|8480-6$gt60 asks for a measurement whose
// code is that one and whose value exceeds 60, not for a resource that happens
// to contain both somewhere. Extraction records the components under synthetic
// codes tagged with a shared sequence number, and the plan carries them as
// component matches so the backend can join on it.

// parseComposite builds a composite match from a "$"-separated value.
//
// Alternatives separated by commas are still alternatives, so the components
// are grouped: each comma-separated group must be satisfied together, and any
// group satisfies the parameter.
func parseComposite(idx *conformance.Index, sp *conformance.SearchParam, modifier, raw string) (storage.ParamMatch, error) {
	if len(sp.Components) == 0 {
		return storage.ParamMatch{}, &searchError{fmt.Sprintf(
			"the composite parameter %q declares no components", sp.Code)}
	}

	kinds := make([]storage.IndexKind, len(sp.Components))
	for i, component := range sp.Components {
		target, ok := idx.SearchParamByURL(component.Definition)
		if !ok {
			return storage.ParamMatch{}, &searchError{fmt.Sprintf(
				"the composite parameter %q names a component this server does not know: %s",
				sp.Code, component.Definition)}
		}
		kind, ok := indexKindFor(target.Type)
		if !ok {
			return storage.ParamMatch{}, &searchError{fmt.Sprintf(
				"component %d of %q is of type %q, which is not supported yet",
				i+1, sp.Code, target.Type)}
		}
		kinds[i] = kind
	}

	switch modifier {
	case "":
	case "missing":
		// Presence is answered against the first component: extraction writes a
		// row for it whenever the composite's base expression matched at all,
		// so its absence is the composite's absence.
		if raw != "true" && raw != "false" {
			return storage.ParamMatch{}, &searchError{fmt.Sprintf(
				":missing takes true or false, got %q", raw)}
		}
		missing := raw == "true"
		return storage.ParamMatch{
			Code:   storage.CompositeComponentCode(sp.Code, 0),
			Kind:   kinds[0],
			Values: []storage.MatchValue{{Missing: &missing}},
		}, nil
	default:
		return storage.ParamMatch{}, &searchError{fmt.Sprintf(
			"the :%s modifier is not valid for the composite parameter %q", modifier, sp.Code)}
	}

	match := storage.ParamMatch{Code: sp.Code}
	for _, alternative := range splitEscaped(raw, ',') {
		parts := splitEscaped(alternative, '$')
		if len(parts) != len(sp.Components) {
			return match, &searchError{fmt.Sprintf(
				"the composite parameter %q takes %d $-separated components, but %q has %d",
				sp.Code, len(sp.Components), alternative, len(parts))}
		}
		components := make([]storage.ParamMatch, 0, len(parts))
		for i, part := range parts {
			value, err := parseValue(kinds[i], "", unescapeValue(part))
			if err != nil {
				return match, err
			}
			components = append(components, storage.ParamMatch{
				Code:   storage.CompositeComponentCode(sp.Code, i),
				Kind:   kinds[i],
				Values: []storage.MatchValue{value},
			})
		}
		match.Composite = append(match.Composite, storage.CompositeMatch{Components: components})
	}
	return match, nil
}
