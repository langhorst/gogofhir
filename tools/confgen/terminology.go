package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance/model"
)

// Compiling value sets.
//
// A validator needs one thing from a value set: whether a code is in it. That
// question is answered at build time here rather than at runtime by a
// terminology service, which is what lets the server check required bindings
// with no network and no dependencies.
//
// Not every value set can be answered offline, and the ones that cannot are the
// point of the terminology policy: SNOMED CT is licensed, LOINC and RxNorm are
// too large to embed, and UCUM, ISO 3166 and BCP 47 are external standards the
// packages only reference. Those are recorded as unresolvable with the reason,
// so a binding to one is reported as *unchecked* rather than quietly treated as
// satisfied. Silence there would overstate what the server verified.

// terminology holds the raw ValueSet and CodeSystem resources of one package,
// which value set expansion draws on.
type terminology struct {
	valueSets   map[string]map[string]any
	codeSystems map[string]map[string]any
}

func newTerminology() *terminology {
	return &terminology{
		valueSets:   map[string]map[string]any{},
		codeSystems: map[string]map[string]any{},
	}
}

func (t *terminology) add(res map[string]any) {
	url := canonical(str(res, "url"))
	if url == "" {
		return
	}
	switch str(res, "resourceType") {
	case "ValueSet":
		t.valueSets[url] = res
	case "CodeSystem":
		t.codeSystems[url] = res
	}
}

// maxExpansionDepth bounds value set composition. Value sets include other
// value sets, and a cycle in published terminology should cost a diagnostic
// rather than the build.
const maxExpansionDepth = 8

// compile expands the value sets a set of bindings reaches.
//
// Only required and extensible bindings are compiled. A preferred or example
// binding is advice a validator does not enforce, and carrying its codes would
// be weight for nothing.
func (t *terminology) compile(wanted map[string]string) map[string]*model.ValueSet {
	out := make(map[string]*model.ValueSet, len(wanted))
	for url := range wanted {
		vs, ok := t.valueSets[url]
		if !ok {
			out[url] = &model.ValueSet{
				URL:          url,
				Unresolvable: "the package does not define this value set",
			}
			continue
		}
		compiled := &model.ValueSet{URL: url, Name: str(vs, "name")}
		codes := map[string]map[string]bool{}
		if reason := t.expand(vs, codes, 0); reason != "" {
			compiled.Unresolvable = reason
		} else {
			compiled.Systems = flatten(codes)
		}
		out[url] = compiled
	}
	return out
}

// expand accumulates a value set's codes, returning why it could not be
// enumerated or "" on success.
func (t *terminology) expand(vs map[string]any, into map[string]map[string]bool, depth int) string {
	if depth > maxExpansionDepth {
		return "the value set composition nests more than 8 deep"
	}
	compose, _ := vs["compose"].(map[string]any)
	if compose == nil {
		return "the value set has no compose element"
	}
	includes, _ := compose["include"].([]any)
	if len(includes) == 0 {
		return "the value set includes nothing"
	}
	for _, item := range includes {
		include, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if reason := t.expandInclude(include, into, depth); reason != "" {
			return reason
		}
	}
	// Exclusions are applied after every inclusion, which is the order the
	// specification defines: an excluded code stays out however it got in.
	for _, item := range sliceOf(compose["exclude"]) {
		exclude, ok := item.(map[string]any)
		if !ok {
			continue
		}
		removed := map[string]map[string]bool{}
		if reason := t.expandInclude(exclude, removed, depth); reason != "" {
			return "an exclusion could not be resolved: " + reason
		}
		for system, codes := range removed {
			for code := range codes {
				delete(into[system], code)
			}
		}
	}
	return ""
}

func (t *terminology) expandInclude(include map[string]any, into map[string]map[string]bool, depth int) string {
	system := canonical(str(include, "system"))

	// An explicit concept list is the simplest and most common case.
	if concepts := sliceOf(include["concept"]); len(concepts) > 0 {
		for _, item := range concepts {
			concept, _ := item.(map[string]any)
			addCode(into, system, str(concept, "code"))
		}
		return ""
	}

	// A composition of other value sets.
	if refs := sliceOf(include["valueSet"]); len(refs) > 0 {
		for _, item := range refs {
			url, _ := item.(string)
			nested, ok := t.valueSets[canonical(url)]
			if !ok {
				return "it composes " + url + ", which the package does not define"
			}
			if reason := t.expand(nested, into, depth+1); reason != "" {
				return reason
			}
		}
		return ""
	}

	if system == "" {
		return "an include names neither a system nor a value set"
	}
	cs, ok := t.codeSystems[system]
	if !ok {
		return "it draws on " + system + ", which is defined outside this package"
	}
	if content := str(cs, "content"); content != "complete" {
		return fmt.Sprintf("the code system %s is published as %q rather than complete", system, content)
	}

	filters := sliceOf(include["filter"])
	if len(filters) == 0 {
		collectConcepts(cs["concept"], into, system)
		return ""
	}
	for _, item := range filters {
		filter, _ := item.(map[string]any)
		if reason := applyFilter(cs, filter, into, system); reason != "" {
			return reason
		}
	}
	return ""
}

// applyFilter handles the filter operations that can be answered from an
// embedded code system's own concept hierarchy. Anything else -- a filter on a
// property, a regex, a subsumption query against an external server -- is
// reported as unresolvable rather than approximated.
func applyFilter(cs, filter map[string]any, into map[string]map[string]bool, system string) string {
	property, op, value := str(filter, "property"), str(filter, "op"), str(filter, "value")
	switch {
	case op == "=" && property == "code":
		addCode(into, system, value)
		return ""
	case (op == "is-a" || op == "descendent-of") && property == "concept":
		concept, found := findConcept(cs["concept"], value)
		if !found {
			return fmt.Sprintf("the filter names the code %q, which %s does not define", value, system)
		}
		if op == "is-a" {
			addCode(into, system, value)
		}
		collectConcepts(concept["concept"], into, system)
		return ""
	case op == "is-not-a" && property == "concept":
		// Everything in the system except one subtree, which is computable
		// whenever the system is embedded whole.
		excluded := map[string]map[string]bool{}
		if concept, found := findConcept(cs["concept"], value); found {
			addCode(excluded, system, value)
			collectConcepts(concept["concept"], excluded, system)
		}
		all := map[string]map[string]bool{}
		collectConcepts(cs["concept"], all, system)
		for code := range all[system] {
			if !excluded[system][code] {
				addCode(into, system, code)
			}
		}
		return ""
	case op == "in" && property == "code":
		for _, code := range strings.Split(value, ",") {
			addCode(into, system, strings.TrimSpace(code))
		}
		return ""
	default:
		return fmt.Sprintf("it selects codes with the filter %q %s %q, which needs a terminology service",
			property, op, value)
	}
}

// findConcept locates a concept by code anywhere in a code system's hierarchy.
func findConcept(raw any, code string) (map[string]any, bool) {
	for _, item := range sliceOf(raw) {
		concept, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if str(concept, "code") == code {
			return concept, true
		}
		if found, ok := findConcept(concept["concept"], code); ok {
			return found, true
		}
	}
	return nil, false
}

// collectConcepts walks a code system's concept tree. Codes nest, and a value
// set that includes a system includes every level of it.
func collectConcepts(raw any, into map[string]map[string]bool, system string) {
	for _, item := range sliceOf(raw) {
		concept, ok := item.(map[string]any)
		if !ok {
			continue
		}
		addCode(into, system, str(concept, "code"))
		collectConcepts(concept["concept"], into, system)
	}
}

func addCode(into map[string]map[string]bool, system, code string) {
	if code == "" {
		return
	}
	if into[system] == nil {
		into[system] = map[string]bool{}
	}
	into[system][code] = true
}

// flatten turns the accumulated sets into sorted slices, so the committed index
// is byte-stable across runs.
func flatten(codes map[string]map[string]bool) map[string][]string {
	out := make(map[string][]string, len(codes))
	for system, set := range codes {
		list := make([]string, 0, len(set))
		for code := range set {
			list = append(list, code)
		}
		sort.Strings(list)
		out[system] = list
	}
	return out
}

// canonical strips the "|version" suffix a canonical URL may carry. An index
// holds one release, so the version is implied and would only defeat lookup.
func canonical(url string) string {
	before, _, _ := strings.Cut(url, "|")
	return before
}

func sliceOf(raw any) []any {
	out, _ := raw.([]any)
	return out
}
