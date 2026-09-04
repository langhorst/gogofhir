package resource

import (
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
)

// Subsetting a resource for _summary and _elements.
//
// Both return a reduced copy rather than mutating: the caller usually holds a
// document read straight out of storage, and trimming it in place would corrupt
// anything else looking at it.
//
// A subsetted resource must say so. The specification requires the SUBSETTED
// tag, and without it a client cannot tell a resource that lacks an element
// from one where the element was filtered away -- which is exactly the sort of
// thing that turns a display bug into a clinical one.

const (
	subsettedSystem = "http://terminology.hl7.org/CodeSystem/v3-ObservationValue"
	subsettedCode   = "SUBSETTED"
)

// alwaysKeep are the elements retained by every subset. Without an id a
// resource cannot be referred to, and without meta a client cannot tell which
// version it has or that the resource was subsetted at all.
var alwaysKeep = map[string]bool{
	"resourceType":  true,
	"id":            true,
	"meta":          true,
	"implicitRules": true,
	// A modifier extension changes the meaning of what it is attached to, so it
	// can never be silently dropped.
	"modifierExtension": true,
}

// Summary returns the copy _summary=true asks for: the elements the
// specification marks as summary, which is its answer to "enough to identify
// and triage this resource".
func (n *Node) Summary() *Node {
	out := n.Clone()
	if obj, ok := out.value.(map[string]any); ok {
		out.value = filterSummary(n.idx, n.def, "", obj)
		markSubsetted(out.value.(map[string]any))
	}
	return out
}

// SummaryText returns the copy _summary=text asks for: the narrative and the
// few elements needed to make sense of it.
func (n *Node) SummaryText() *Node {
	out := n.Clone()
	obj, ok := out.value.(map[string]any)
	if !ok {
		return out
	}
	keep := map[string]any{}
	for key, value := range obj {
		if alwaysKeep[key] || key == "text" {
			keep[key] = value
		}
	}
	markSubsetted(keep)
	out.value = keep
	return out
}

// SummaryData returns the copy _summary=data asks for: everything except the
// narrative, for a client that renders the data itself.
func (n *Node) SummaryData() *Node {
	out := n.Clone()
	obj, ok := out.value.(map[string]any)
	if !ok {
		return out
	}
	delete(obj, "text")
	markSubsetted(obj)
	return out
}

// Elements returns the copy _elements asks for: the named top-level elements
// and nothing else. Names that the type does not define are ignored rather than
// rejected, since a client asking for an element that is simply absent has not
// made an error.
func (n *Node) Elements(names []string) *Node {
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[strings.TrimSpace(name)] = true
	}
	out := n.Clone()
	obj, ok := out.value.(map[string]any)
	if !ok {
		return out
	}
	keep := map[string]any{}
	for key, value := range obj {
		base := strings.TrimPrefix(key, "_")
		if alwaysKeep[key] || wanted[base] || wanted[choiceBaseName(n.idx, n.def, base)] {
			keep[key] = value
		}
	}
	markSubsetted(keep)
	out.value = keep
	return out
}

// choiceBaseName maps a concrete choice element back to the name a client would
// have asked for: _elements=value should keep valueQuantity.
func choiceBaseName(idx *conformance.Index, def *conformance.TypeDef, key string) string {
	if def == nil {
		return key
	}
	for _, el := range def.Elements {
		if !el.Choice || strings.Contains(el.Path, ".") {
			continue
		}
		for _, expansion := range el.Expansions {
			if expansion == key {
				return el.Path
			}
		}
	}
	return key
}

// filterSummary keeps the elements marked isSummary, recursing into the ones it
// keeps so nested content is trimmed the same way.
func filterSummary(idx *conformance.Index, def *conformance.TypeDef, path string, obj map[string]any) map[string]any {
	keep := map[string]any{}
	for key, value := range obj {
		base := strings.TrimPrefix(key, "_")
		if alwaysKeep[key] {
			keep[key] = value
			continue
		}
		el, isSummary := summaryElement(idx, def, path, base)
		if !isSummary {
			continue
		}
		childDef, childPath := childLocation(idx, def, path, base)
		keep[key] = filterSummaryValue(idx, childDef, childPath, value)
		_ = el
	}
	return keep
}

func filterSummaryValue(idx *conformance.Index, def *conformance.TypeDef, path string, value any) any {
	switch x := value.(type) {
	case map[string]any:
		if def == nil {
			return x
		}
		return filterSummary(idx, def, path, x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = filterSummaryValue(idx, def, path, item)
		}
		return out
	default:
		return value
	}
}

// summaryElement reports whether an element is part of the summary view.
func summaryElement(idx *conformance.Index, def *conformance.TypeDef, path, name string) (*conformance.ElementDef, bool) {
	if def == nil {
		return nil, false
	}
	step, ok := idx.Step(conformance.Cursor{Def: def, Path: path}, name)
	if !ok {
		return nil, false
	}
	// A mandatory element stays regardless: omitting it would produce a
	// document that does not satisfy its own definition.
	return step.Element, step.Element.Summary || step.Element.Required()
}

// markSubsetted adds the SUBSETTED tag, creating meta if needed.
func markSubsetted(obj map[string]any) {
	meta, _ := obj["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["meta"] = meta
	}
	tags, _ := meta["tag"].([]any)
	for _, raw := range tags {
		tag, _ := raw.(map[string]any)
		if tag["code"] == subsettedCode {
			return
		}
	}
	meta["tag"] = append(tags, map[string]any{
		"system": subsettedSystem,
		"code":   subsettedCode,
	})
}
