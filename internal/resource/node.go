// Package resource holds FHIR resources as untyped documents and exposes them
// to FHIRPath.
//
// Because gogofhir generates no Go structs for resource types, a document is
// just decoded JSON -- maps, slices, and scalars -- and all of its meaning comes
// from the conformance index. This package joins the two: it wraps a decoded
// document in nodes that answer what element they are, what FHIR type that
// element has, and what children it holds, which is exactly the interface
// FHIRPath needs.
//
// Both the JSON and XML readers produce the same in-memory shape, so navigation,
// evaluation, and validation are written once and work for either wire format.
package resource

import (
	"slices"
	"strings"
	"time"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
)

// Node is one element of a document. It implements fhirpath.Node.
//
// A node's identity is a type plus a path *within* that type, not simply a
// type. FHIR defines backbone elements inline in their owning resource's
// snapshot -- Patient.contact.name is an element of Patient, not of some
// standalone Contact type -- so navigating into a backbone keeps the same
// TypeDef and extends the path. Modelling backbones as separate types is a
// common way to end up unable to resolve their children.
type Node struct {
	idx *conformance.Index
	// def is the type whose element map defines this node's children.
	def *conformance.TypeDef
	// path locates this node inside def: "" at the type's root, "contact" for a
	// backbone element, "contact.name" one level deeper.
	path string
	// name is the element name this node arrived under, for diagnostics.
	name string
	// fhirType is the node's own type name, which for a backbone element is
	// "BackboneElement" rather than def.Name.
	fhirType string
	// value is the decoded JSON value: map[string]any for complex elements, or
	// a scalar for primitives.
	value any
	// primitiveExt holds the sibling "_name" object carrying a primitive's id
	// and extensions, if present.
	primitiveExt map[string]any
}

var (
	_ fhirpath.Node           = (*Node)(nil)
	_ fhirpath.PrimitiveTyped = (*Node)(nil)
)

// IsPrimitiveType reports whether this element's type is a FHIR primitive,
// regardless of whether it currently carries a value.
func (n *Node) IsPrimitiveType() bool {
	def, ok := n.idx.Type(n.fhirType)
	return ok && def.Kind == "primitive-type"
}

// TypeName is the namespaced type used by is, as, and ofType.
func (n *Node) TypeName() string { return "FHIR." + n.fhirType }

// FHIRType is the node's unqualified FHIR type name.
func (n *Node) FHIRType() string { return n.fhirType }

// String renders the node's primitive value, or its element name when it is
// complex. FHIRPath only asks this of values it is about to print.
func (n *Node) String() string {
	if v, ok := n.Primitive(); ok {
		return v.String()
	}
	return n.name
}

// Primitive converts a FHIR primitive element to its System value. Complex
// elements report false.
//
// A FHIR primitive is simultaneously a value and a node: Patient.birthDate is a
// Date, and Patient.birthDate.extension is also navigable. Both this and
// Children must therefore answer for the same node.
func (n *Node) Primitive() (fhirpath.Value, bool) {
	if n.value == nil {
		return nil, false
	}
	def, ok := n.idx.Type(n.fhirType)
	if !ok || def.Kind != "primitive-type" {
		return nil, false
	}
	return primitiveValue(n.fhirType, n.value)
}

// primitiveValue maps a FHIR primitive type and its decoded value onto the
// System type FHIRPath works in.
func primitiveValue(fhirType string, raw any) (fhirpath.Value, bool) {
	switch fhirType {
	case "boolean":
		b, ok := raw.(bool)
		return fhirpath.Boolean(b), ok
	case "integer", "positiveInt", "unsignedInt", "integer64":
		switch v := raw.(type) {
		case jsonNumber:
			i, err := v.Int64()
			return fhirpath.Integer(i), err == nil
		case string:
			// integer64 is carried as a string on the wire to survive
			// languages without 64-bit integers.
			d, err := fhirpath.NewDecimal(v)
			if err != nil || !d.IsInt() {
				return nil, false
			}
			return fhirpath.Integer(d.Rat().Num().Int64()), true
		}
		return nil, false
	case "decimal":
		v, ok := raw.(jsonNumber)
		if !ok {
			return nil, false
		}
		d, err := fhirpath.NewDecimal(string(v))
		return d, err == nil
	case "date", "dateTime", "instant", "time":
		s, ok := raw.(string)
		if !ok {
			return nil, false
		}
		t, err := fhirpath.ParseFHIRTemporal(fhirType, s)
		if err != nil {
			// A malformed date is still a value; treating it as a string keeps
			// evaluation going so validation can report it properly.
			return fhirpath.String_(s), true
		}
		return t, true
	default:
		// Every other primitive -- string, code, id, uri, url, canonical, oid,
		// uuid, markdown, base64Binary, xhtml -- is a String.
		s, ok := raw.(string)
		return fhirpath.String_(s), ok
	}
}

// Children returns the child elements named name, in document order. An empty
// name returns every child, which children() and descendants() need.
func (n *Node) Children(name string) []fhirpath.Node {
	obj, ok := n.value.(map[string]any)
	if !ok {
		// A primitive has no object of its own, but its id and extensions live
		// in the sibling "_name" object and must still be navigable.
		if n.primitiveExt != nil {
			return n.extensionChildren(name)
		}
		return nil
	}
	var out []fhirpath.Node
	for _, el := range n.childElements() {
		if name != "" && el.Path != name {
			// A choice element may also be addressed by one of its expanded
			// names: "Observation.valueQuantity" rather than
			// "Observation.value.ofType(Quantity)". Strict mode rejects that
			// spelling, but it is widespread and reads unambiguously.
			if !el.def.Choice || !slices.Contains(el.def.Expansions, name) {
				continue
			}
			out = append(out, n.choiceChild(obj, el, name)...)
			continue
		}
		out = append(out, n.childrenFor(obj, el)...)
	}
	if n.primitiveExt != nil {
		out = append(out, n.extensionChildren(name)...)
	}
	return out
}

// extensionChildren serves the id and extension entries carried in a
// primitive's parallel "_name" object.
func (n *Node) extensionChildren(name string) []fhirpath.Node {
	extDef, _ := n.idx.Type("Extension")
	var out []fhirpath.Node

	if name == "" || name == "id" {
		if id, ok := n.primitiveExt["id"].(string); ok {
			out = append(out, &Node{idx: n.idx, name: "id", fhirType: "string", value: id})
		}
	}
	if name != "" && name != "extension" {
		return out
	}
	items, _ := n.primitiveExt["extension"].([]any)
	for _, item := range items {
		out = append(out, &Node{
			idx: n.idx, def: extDef, name: "extension",
			fhirType: "Extension", value: item,
		})
	}
	return out
}

// childElements lists the element definitions one level below this node's path,
// in snapshot order.
func (n *Node) childElements() []childElement {
	def, path := n.resolveDefinition()
	if def == nil {
		return nil
	}
	prefix := ""
	if path != "" {
		prefix = path + "."
	}
	var out []childElement
	for _, el := range def.Elements {
		rest, ok := strings.CutPrefix(el.Path, prefix)
		if !ok || rest == "" || strings.Contains(rest, ".") {
			continue // not a direct child
		}
		out = append(out, childElement{Path: rest, def: el, owner: def, ownerPath: el.Path})
	}
	return out
}

// childElement is one element definition relative to its parent node.
type childElement struct {
	// Path is the element's name relative to the parent, with any "[x]" already
	// stripped by the generator.
	Path      string
	def       *conformance.ElementDef
	owner     *conformance.TypeDef
	ownerPath string
}

// resolveDefinition follows a contentReference when the node's own path carries
// one. FHIR expresses recursive structures that way -- Questionnaire.item.item
// points back at "#Questionnaire.item" -- and without following it, navigation
// stops one level deep.
func (n *Node) resolveDefinition() (*conformance.TypeDef, string) {
	if n.def == nil {
		return nil, ""
	}
	if el, ok := n.def.Element(n.path); ok && el.ContentReference != "" {
		target := strings.TrimPrefix(el.ContentReference, "#")
		typeName, rest, _ := strings.Cut(target, ".")
		if def, ok := n.idx.Type(typeName); ok {
			return def, rest
		}
	}
	return n.def, n.path
}

// childrenFor materializes the nodes for one element definition, handling
// choice-type expansion, repetition, and the parallel "_name" object that
// carries a primitive's extensions.
func (n *Node) childrenFor(obj map[string]any, el childElement) []fhirpath.Node {
	// A choice element appears in the document under one of its expansions.
	keys := []string{el.Path}
	types := el.def.Types
	if el.def.Choice {
		keys = el.def.Expansions
	}

	var out []fhirpath.Node
	for i, key := range keys {
		raw, present := obj[key]
		ext, hasExt := obj["_"+key].(map[string]any)
		extList, hasExtList := obj["_"+key].([]any)
		if !present && !hasExt && !hasExtList {
			continue
		}
		childType := ""
		if el.def.Choice && i < len(types) {
			childType = types[i].Code
		} else if len(types) > 0 {
			childType = types[0].Code
		}

		// When the element itself is absent, its occurrences come from the
		// "_name" sidecar alone; running the value loop as well would emit a
		// second, valueless node for the same element.
		var items []any
		if present {
			items = []any{raw}
			if arr, isArr := raw.([]any); isArr {
				items = arr
			}
		}
		for j, item := range items {
			child := n.newChild(el, key, childType, item)
			// Attach the matching "_name" entry so extensions on a primitive
			// remain reachable.
			switch {
			case hasExt && !el.def.IsArray():
				child.primitiveExt = ext
			case hasExtList && j < len(extList):
				if m, ok := extList[j].(map[string]any); ok {
					child.primitiveExt = m
				}
			}
			out = append(out, child)
		}
		// A primitive present only as "_name" still exists as a node carrying
		// extensions, with no value of its own.
		if !present && (hasExt || hasExtList) {
			extItems := []any{any(ext)}
			if hasExtList {
				extItems = extList
			}
			for _, e := range extItems {
				m, _ := e.(map[string]any)
				child := n.newChild(el, key, childType, nil)
				child.primitiveExt = m
				out = append(out, child)
			}
		}
	}
	return out
}

// choiceChild materializes just one expansion of a choice element, for
// navigation that names it directly.
func (n *Node) choiceChild(obj map[string]any, el childElement, expansion string) []fhirpath.Node {
	raw, present := obj[expansion]
	if !present {
		return nil
	}
	childType := ""
	for i, exp := range el.def.Expansions {
		if exp == expansion && i < len(el.def.Types) {
			childType = el.def.Types[i].Code
		}
	}
	items := []any{raw}
	if arr, isArr := raw.([]any); isArr {
		items = arr
	}
	out := make([]fhirpath.Node, 0, len(items))
	for _, item := range items {
		out = append(out, n.newChild(el, expansion, childType, item))
	}
	return out
}

// newChild builds the node for one occurrence of an element, deciding whether
// it continues inside the current type (a backbone) or moves to another.
func (n *Node) newChild(el childElement, key, childType string, item any) *Node {
	def, _ := n.resolveDefinition()
	child := &Node{idx: n.idx, name: key, value: item, fhirType: childType}

	// An element defined purely by a contentReference (Questionnaire.item.item)
	// declares no type of its own; it is a backbone whose shape lives at the
	// referenced path.
	if childType == "" && el.def.ContentReference != "" {
		childType = "BackboneElement"
		child.fhirType = childType
	}

	switch {
	case childType == "BackboneElement" || childType == "Element":
		// Backbones are defined inline in the owning type, so stay put and
		// extend the path.
		child.def, child.path = def, el.ownerPath
	case childType == "Resource" || childType == "DomainResource":
		// A contained or bundled resource carries its own type.
		child.def, child.path = n.resourceDefFor(item), ""
		if child.def != nil {
			child.fhirType = child.def.Name
		}
	default:
		if td, ok := n.idx.Type(childType); ok {
			child.def, child.path = td, ""
		} else {
			// Unknown type: keep the node navigable but definition-less.
			child.def, child.path = def, el.ownerPath
		}
	}
	return child
}

// resourceDefFor reads the resourceType of a nested resource.
func (n *Node) resourceDefFor(item any) *conformance.TypeDef {
	obj, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	typeName, _ := obj["resourceType"].(string)
	if typeName == "" {
		return nil
	}
	def, _ := n.idx.Type(typeName)
	return def
}

// ---- identity and metadata ----

// ID returns the resource's logical id, or "" if it has none.
func (n *Node) ID() string {
	obj, ok := n.value.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := obj["id"].(string)
	return id
}

// SetID sets the resource's logical id.
func (n *Node) SetID(id string) {
	if obj, ok := n.value.(map[string]any); ok {
		obj["id"] = id
	}
}

// SetMeta stamps meta.versionId and meta.lastUpdated, which the server owns:
// a client may send them, but the values it sends are never authoritative.
func (n *Node) SetMeta(versionID string, lastUpdated time.Time) {
	obj, ok := n.value.(map[string]any)
	if !ok {
		return
	}
	meta, _ := obj["meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["meta"] = meta
	}
	meta["versionId"] = versionID
	// FHIR instants are written to at least seconds with a timezone; this uses
	// milliseconds in UTC, which is what servers conventionally emit.
	meta["lastUpdated"] = lastUpdated.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// Meta returns the stored versionId and lastUpdated, if present.
func (n *Node) Meta() (versionID, lastUpdated string) {
	obj, ok := n.value.(map[string]any)
	if !ok {
		return "", ""
	}
	meta, _ := obj["meta"].(map[string]any)
	if meta == nil {
		return "", ""
	}
	versionID, _ = meta["versionId"].(string)
	lastUpdated, _ = meta["lastUpdated"].(string)
	return versionID, lastUpdated
}

// Clone returns a deep copy, so a caller can stamp metadata onto a document
// without disturbing the one it was handed.
func (n *Node) Clone() *Node {
	c := *n
	c.value = deepCopy(n.value)
	return &c
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = deepCopy(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopy(item)
		}
		return out
	default:
		return v
	}
}
