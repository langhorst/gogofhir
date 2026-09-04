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
	for _, child := range n.idx.Children(n.cursor()) {
		if name != "" && child.Name != name {
			// A choice element may also be addressed by one of its expanded
			// names: "Observation.valueQuantity" rather than
			// "Observation.value.ofType(Quantity)". Strict mode rejects that
			// spelling, but it is widespread and reads unambiguously.
			if !child.Def.Choice || !slices.Contains(child.Def.Expansions, name) {
				continue
			}
			out = append(out, n.occurrences(obj, []string{name})...)
			continue
		}
		out = append(out, n.occurrences(obj, documentKeys(child))...)
	}
	if n.primitiveExt != nil {
		out = append(out, n.extensionChildren(name)...)
	}
	return out
}

// cursor is the node's position in the type system.
func (n *Node) cursor() conformance.Cursor {
	return conformance.Cursor{Def: n.def, Path: n.path}
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

// occurrences materializes the nodes found under the given document keys --
// an element's own name, or a choice element's expansions -- handling
// repetition and the parallel "_name" object that carries a primitive's
// extensions.
func (n *Node) occurrences(obj map[string]any, keys []string) []fhirpath.Node {
	var out []fhirpath.Node
	for _, key := range keys {
		raw, present := obj[key]
		ext, hasExt := obj["_"+key].(map[string]any)
		extList, hasExtList := obj["_"+key].([]any)
		if !present && !hasExt && !hasExtList {
			continue
		}
		step, _ := n.idx.Step(n.cursor(), key)
		repeats := step.Element != nil && step.Element.IsArray()

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
			child := n.newChild(step, key, item)
			// Attach the matching "_name" entry so extensions on a primitive
			// remain reachable.
			switch {
			case hasExt && !repeats:
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
				child := n.newChild(step, key, nil)
				child.primitiveExt = m
				out = append(out, child)
			}
		}
	}
	return out
}

// newChild builds the node for one occurrence of an element at the position
// the step resolved: inside the owning type for a backbone, at a datatype's
// root otherwise.
func (n *Node) newChild(step conformance.Step, key string, item any) *Node {
	child := &Node{idx: n.idx, name: key, value: item, fhirType: step.Type}
	if step.Nested {
		// A contained or bundled resource carries its own type.
		child.def = n.resourceDefFor(item)
		if child.def != nil {
			child.fhirType = child.def.Name
		}
		return child
	}
	child.def, child.path = step.Child.Def, step.Child.Path
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
