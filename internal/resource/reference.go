package resource

// References.
//
// A transaction bundle rewrites the references between its entries, which means
// finding them exactly. A structural search for a JSON key named "reference"
// is not exact: DetectedIssue.reference, Expression.reference,
// Immunization.education.reference, ActorDefinition.reference and
// Requirements.reference are all plain URIs across R4 and R5, and rewriting one
// of those would corrupt the resource. So the walk is schema-driven -- it finds
// nodes whose type is Reference and touches only those.

// References returns every Reference.reference value in the document, in
// document order.
func (n *Node) References() []string {
	var out []string
	n.walkReferences(func(_ map[string]any, value string) {
		out = append(out, value)
	})
	return out
}

// RewriteReferences replaces reference values for which replace returns a
// substitute, and reports how many it changed.
//
// The document is modified in place: a caller holding a node built from a
// request body is expected to want exactly that, and storage clones before
// stamping anyway.
func (n *Node) RewriteReferences(replace func(string) (string, bool)) int {
	changed := 0
	n.walkReferences(func(obj map[string]any, value string) {
		if replacement, ok := replace(value); ok && replacement != value {
			obj["reference"] = replacement
			changed++
		}
	})
	return changed
}

// walkReferences visits every Reference element carrying a reference string.
//
// Nodes share the underlying decoded maps rather than copying them, so the
// object handed to visit is the document's own and writing to it edits the
// document.
func (n *Node) walkReferences(visit func(obj map[string]any, value string)) {
	if n.fhirType == "Reference" {
		if obj, ok := n.value.(map[string]any); ok {
			if value, ok := obj["reference"].(string); ok {
				visit(obj, value)
			}
		}
	}
	for _, child := range n.Children("") {
		if node, ok := child.(*Node); ok {
			node.walkReferences(visit)
		}
	}
}

// Object returns the document's underlying JSON object.
//
// It is the escape hatch for the few callers that work with a resource's shape
// directly rather than through navigation -- reading a Bundle's entries, or
// embedding one document inside another -- and it returns the live map, not a
// copy.
func (n *Node) Object() (map[string]any, bool) {
	obj, ok := n.value.(map[string]any)
	return obj, ok
}
