package resource

import (
	"sort"

	"github.com/langhorst/gogofhir/internal/conformance"
)

// Inspection: what a document holds at a node, measured against what the
// element definitions allow.
//
// Navigation elsewhere in this package answers "what is here", silently
// ignoring anything the definitions do not cover -- which is right for
// evaluating an expression and wrong for validating a document, where an
// element nobody defined is the finding. These two methods expose both halves.

// Field is one element definition available at a node, together with whatever
// the document holds for it.
type Field struct {
	// Name is the element name relative to the node, with a choice element's
	// "[x]" already stripped: "deceased", not "deceased[x]".
	Name string
	Def  *conformance.ElementDef
	// Keys are the document keys the element actually appeared under: its own
	// name, or one or more of a choice element's expansions. More than one key
	// means the document set a choice element twice, which is an error a
	// validator has to see rather than resolve.
	Keys []string
	// Values are the occurrences present, one per repetition.
	Values []*Node
}

// Fields lists every element defined at this node, present or not.
func (n *Node) Fields() []Field {
	obj, ok := n.value.(map[string]any)
	if !ok {
		return nil
	}
	elements := n.childElements()
	out := make([]Field, 0, len(elements))
	for _, el := range elements {
		field := Field{Name: el.Path, Def: el.def}
		for _, key := range documentKeys(el) {
			_, present := obj[key]
			_, hasExt := obj["_"+key]
			if present || hasExt {
				field.Keys = append(field.Keys, key)
			}
		}
		for _, child := range n.childrenFor(obj, el) {
			if node, ok := child.(*Node); ok {
				field.Values = append(field.Values, node)
			}
		}
		out = append(out, field)
	}
	return out
}

// UnknownKeys lists the document keys at this node that no element definition
// covers, sorted.
//
// An unknown element is the one error a schema-driven server cannot shrug off:
// it is either a typo, a resource from another FHIR version, or an extension
// written without the extension mechanism, and all three should be told apart
// from a valid document.
func (n *Node) UnknownKeys() []string {
	obj, ok := n.value.(map[string]any)
	if !ok {
		// A primitive carries its id and extensions in a parallel object, and
		// nothing else belongs there.
		var out []string
		for key := range n.primitiveExt {
			if key != "id" && key != "extension" {
				out = append(out, key)
			}
		}
		sort.Strings(out)
		return out
	}

	known := map[string]bool{}
	// "resourceType" is the document's own discriminator, and only a resource
	// root has one.
	if def, path := n.resolveDefinition(); def != nil && def.Kind == "resource" && path == "" {
		known["resourceType"] = true
	}
	for _, el := range n.childElements() {
		for _, key := range documentKeys(el) {
			known[key], known["_"+key] = true, true
		}
	}

	var out []string
	for key := range obj {
		if !known[key] {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// documentKeys are the names an element may appear under: its own, or a choice
// element's expansions.
func documentKeys(el childElement) []string {
	if el.def.Choice {
		return el.def.Expansions
	}
	return []string{el.Path}
}

// Name returns the element name this node arrived under, empty at a resource
// root.
func (n *Node) Name() string { return n.name }

// Raw returns the node's decoded JSON value: a map for a complex element, a
// scalar for a primitive, nil for a primitive present only through its
// extensions.
//
// A validator needs the value as it was written, not as it was interpreted: a
// boolean element holding the string "true" is a finding, and every typed
// accessor would hide it.
func (n *Node) Raw() any { return n.value }

// HasPrimitiveExtensions reports whether the node carries a parallel "_name"
// object.
func (n *Node) HasPrimitiveExtensions() bool { return n.primitiveExt != nil }
