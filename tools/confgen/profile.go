package main

import (
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance/model"
)

// Compiling profiles.
//
// A profile is a StructureDefinition whose derivation is "constraint": it
// narrows an existing type instead of defining a new one. Its elements are
// taken from the published snapshot rather than from the differential, because
// building a snapshot means replaying a chain of constraints down a derivation
// tree, and the packages already ship the answer.

// compileProfile reduces a constraining StructureDefinition, reporting false
// for one the server cannot use.
func compileProfile(res map[string]any, idx *model.Index) (*model.Profile, bool) {
	if str(res, "derivation") != "constraint" {
		return nil, false
	}
	typeName := str(res, "type")
	url := canonical(str(res, "url"))
	if typeName == "" || url == "" {
		return nil, false
	}
	snapshot, _ := res["snapshot"].(map[string]any)
	elements := sliceOf(snapshot["element"])
	if len(elements) == 0 {
		// Without a snapshot there is nothing to validate against, and
		// generating one is a different tool.
		return nil, false
	}

	profile := &model.Profile{
		URL:  url,
		Name: str(res, "name"),
		Type: typeName,
		Base: canonical(str(res, "baseDefinition")),
	}
	for _, item := range sliceOf(res["context"]) {
		context, ok := item.(map[string]any)
		if !ok {
			continue
		}
		profile.Contexts = append(profile.Contexts, model.ProfileContext{
			Type:       str(context, "type"),
			Expression: str(context, "expression"),
		})
	}

	base, _ := idx.Type(typeName)
	for _, item := range elements {
		el, ok := item.(map[string]any)
		if !ok {
			continue
		}
		compiled := compileProfileElement(el, typeName, url, idx, base)
		if compiled == nil {
			continue
		}
		profile.Elements = append(profile.Elements, compiled)
	}
	if len(profile.Elements) == 0 {
		return nil, false
	}
	return profile, true
}

func compileProfileElement(el map[string]any, typeName, ownURL string, idx *model.Index, base *model.TypeDef) *model.ProfileElement {
	path := str(el, "path")
	rel := strings.TrimPrefix(strings.TrimPrefix(path, typeName), ".")

	pe := &model.ProfileElement{
		Path:        rel,
		Slice:       sliceName(str(el, "id")),
		Min:         integer(el, "min"),
		Max:         str(el, "max"),
		Types:       compileTypes(el),
		MustSupport: boolean(el, "mustSupport"),
		Invariants:  compileInvariants(el, rel, ownURL),
	}
	if base, found := strings.CutSuffix(pe.Path, "[x]"); found {
		pe.Path = base
	}
	if binding, ok := el["binding"].(map[string]any); ok {
		if vs := canonical(str(binding, "valueSet")); vs != "" {
			pe.Binding = &model.Binding{Strength: str(binding, "strength"), ValueSet: vs}
		}
	}
	pe.Fixed = choiceValue(el, "fixed")
	pe.Pattern = choiceValue(el, "pattern")
	pe.Slicing = compileSlicing(el)

	// A snapshot restates the whole base type at every element -- its
	// cardinality, its types, its binding -- so a profile compiled verbatim
	// says almost nothing while costing almost everything. Only what the
	// profile actually narrows is kept, which is also exactly what a validator
	// would otherwise have to diff at runtime.
	narrowAgainstBase(pe, idx, base)

	// The root element is kept only for its invariants; any other element that
	// narrows nothing is weight in the index for no gain.
	if pe.Path == "" {
		if len(pe.Invariants) == 0 {
			return nil
		}
		return pe
	}
	if pe.Min == 0 && pe.Max == "" && pe.Binding == nil && pe.Fixed == nil &&
		pe.Pattern == nil && pe.Slicing == nil && !pe.MustSupport &&
		len(pe.Invariants) == 0 && len(pe.Types) == 0 {
		return nil
	}
	return pe
}

// narrowAgainstBase strips the parts of an element that merely repeat the base
// type, leaving what the profile genuinely constrains.
func narrowAgainstBase(pe *model.ProfileElement, idx *model.Index, base *model.TypeDef) {
	// "*" is the default upper bound, so it never narrows anything.
	if pe.Max == "*" {
		pe.Max = ""
	}
	baseEl := baseElement(idx, base, pe.Path)
	if baseEl == nil {
		// No base element to compare against -- an element the profile added,
		// or a type outside this index. Types are still only interesting where
		// they discriminate or narrow to one.
		if len(pe.Types) > 1 {
			pe.Types = nil
		}
		return
	}
	if pe.Min <= baseEl.Min {
		pe.Min = 0
	}
	if pe.Max == baseEl.Max {
		pe.Max = ""
	}
	if pe.Binding != nil && baseEl.Binding != nil &&
		pe.Binding.ValueSet == baseEl.Binding.ValueSet &&
		pe.Binding.Strength == baseEl.Binding.Strength {
		pe.Binding = nil
	}
	if sameTypes(pe.Types, baseEl.Types) {
		pe.Types = nil
	}
}

// baseElement resolves an element path against the type system, descending into
// datatypes as it goes.
//
// A snapshot does not recurse into datatypes -- Observation's stops at
// Observation.category, typed CodeableConcept -- but a profile's does, and
// names elements like "category.coding.system". Without following the datatype
// there is no base element to compare against, and every such element would be
// kept as if it narrowed something.
func baseElement(idx *model.Index, def *model.TypeDef, path string) *model.ElementDef {
	if def == nil || path == "" {
		return nil
	}
	segments := strings.Split(path, ".")
	prefix := ""
	for i, segment := range segments {
		if prefix != "" {
			prefix += "."
		}
		prefix += segment
		last := i == len(segments)-1

		el, ok := def.Element(prefix)
		if !ok {
			// The segment may be a choice element written out in full, as
			// "valueQuantity" for "value[x]".
			code, found := def.ExpansionType(segment)
			if !found || last {
				return nil
			}
			next, ok := idx.Type(code)
			if !ok {
				return nil
			}
			def, prefix = next, ""
			continue
		}
		if last {
			return el
		}
		if el.ContentReference != "" {
			// A recursive structure: Questionnaire.item.item points back at
			// "#Questionnaire.item".
			target := strings.TrimPrefix(el.ContentReference, "#")
			typeName, rest, _ := strings.Cut(target, ".")
			next, ok := idx.Type(typeName)
			if !ok {
				return nil
			}
			def, prefix = next, rest
			continue
		}
		if len(el.Types) != 1 {
			// A choice element addressed by its base name: which type the next
			// segment belongs to is not something the path says.
			return nil
		}
		code := el.Types[0].Code
		if code == "BackboneElement" || code == "Element" {
			// A backbone element stays inside the owning type, with the path
			// simply extended.
			continue
		}
		next, ok := idx.Type(code)
		if !ok {
			return nil
		}
		def, prefix = next, ""
	}
	return nil
}

// sameTypes reports whether a profile's declared types are the base's,
// unchanged. A slice keeps its types regardless, since a slice may be told
// apart by type even when it narrows nothing.
func sameTypes(profile, base []model.TypeRef) bool {
	if len(profile) != len(base) {
		return false
	}
	for i := range profile {
		if profile[i].Code != base[i].Code || len(profile[i].Targets) != len(base[i].Targets) {
			return false
		}
		for j := range profile[i].Targets {
			if profile[i].Targets[j] != base[i].Targets[j] {
				return false
			}
		}
	}
	return true
}

// sliceName reads the slice an element belongs to out of its id.
//
// An element id spells slicing with colons -- "Patient.identifier:mrn.system"
// names the "system" of the "mrn" slice -- and nested slices stack, so the
// segments are joined rather than only the last one kept.
func sliceName(id string) string {
	var parts []string
	for _, segment := range strings.Split(id, ".") {
		if _, name, found := strings.Cut(segment, ":"); found && name != "" {
			parts = append(parts, name)
		}
	}
	return strings.Join(parts, "/")
}

func compileSlicing(el map[string]any) *model.Slicing {
	raw, ok := el["slicing"].(map[string]any)
	if !ok {
		return nil
	}
	slicing := &model.Slicing{
		Rules:   str(raw, "rules"),
		Ordered: boolean(raw, "ordered"),
	}
	for _, item := range sliceOf(raw["discriminator"]) {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		slicing.Discriminators = append(slicing.Discriminators, model.Discriminator{
			Type: str(d, "type"),
			Path: str(d, "path"),
		})
	}
	return slicing
}

// choiceValue reads a fixed[x] or pattern[x] value, whichever type suffix it
// carries. The suffix is the value's type, which the value itself already
// carries in JSON, so only the value is kept.
func choiceValue(el map[string]any, prefix string) any {
	for key, value := range el {
		rest, found := strings.CutPrefix(key, prefix)
		if !found || rest == "" {
			continue
		}
		// The suffix is a type name, which always starts with an upper-case
		// letter -- this is what keeps "fixedCode" from also matching a
		// hypothetical "fixedness".
		if rest[0] >= 'A' && rest[0] <= 'Z' {
			return value
		}
	}
	return nil
}
