package resource

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
)

// FHIR XML is the same information model as FHIR JSON in a different shape:
// primitives carry their value in a "value" attribute rather than as a scalar,
// repetition is repeated elements rather than an array, and a primitive's
// extensions are children rather than a parallel "_name" object.
//
// Rather than teach navigation two shapes, the reader converts XML into the
// same maps the JSON reader produces. That gives one implementation of
// navigation, evaluation, and validation -- and, as a side effect, an
// XML-to-JSON converter for content negotiation later.

// xmlElement is a raw parsed element, before FHIR meaning is applied.
type xmlElement struct {
	name     string
	attrs    map[string]string
	children []*xmlElement
	// inner is the verbatim serialized content, used for xhtml where the
	// markup itself is the value.
	inner string
}

// FromXML builds a document from FHIR XML.
func FromXML(idx *conformance.Index, data []byte) (*Node, error) {
	root, err := parseXML(data)
	if err != nil {
		return nil, err
	}
	def, ok := idx.Type(root.name)
	if !ok {
		return nil, fmt.Errorf("resource: unknown resource type %q", root.name)
	}
	obj := elementToMap(idx, def, "", root)
	obj["resourceType"] = root.name
	return &Node{idx: idx, def: def, path: "", name: root.name, fhirType: root.name, value: obj}, nil
}

func parseXML(data []byte) (*xmlElement, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	// FHIR instances are namespaced, but the namespace is fixed and carries no
	// information for us; local names are enough.
	dec.Strict = false
	var stack []*xmlElement
	var root *xmlElement

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("resource: parsing XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			el := &xmlElement{name: t.Name.Local, attrs: map[string]string{}}
			for _, a := range t.Attr {
				if a.Name.Local == "xmlns" || a.Name.Space == "xmlns" {
					continue
				}
				el.attrs[a.Name.Local] = a.Value
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, el)
			} else {
				root = el
			}
			stack = append(stack, el)

			// xhtml content is markup, so capture it verbatim rather than
			// walking into it.
			if t.Name.Local == "div" {
				var raw struct {
					Inner string `xml:",innerxml"`
				}
				if err := dec.DecodeElement(&raw, &t); err != nil {
					return nil, fmt.Errorf("resource: parsing xhtml: %w", err)
				}
				el.inner = raw.Inner
				stack = stack[:len(stack)-1]
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("resource: empty XML document")
	}
	return root, nil
}

// elementToMap converts one element's children into the JSON-shaped map,
// consulting the index so repetition and types match what the JSON reader would
// have produced.
func elementToMap(idx *conformance.Index, def *conformance.TypeDef, path string, el *xmlElement) map[string]any {
	obj := map[string]any{}

	// Element-level attributes that are data rather than markup.
	if id, ok := el.attrs["id"]; ok {
		obj["id"] = id
	}
	if url, ok := el.attrs["url"]; ok {
		obj["url"] = url
	}

	// Group children by name so repetition can be detected in one pass.
	order := []string{}
	groups := map[string][]*xmlElement{}
	for _, child := range el.children {
		if _, seen := groups[child.name]; !seen {
			order = append(order, child.name)
		}
		groups[child.name] = append(groups[child.name], child)
	}

	for _, name := range order {
		children := groups[name]
		elDef, childType, childPath, childDef := lookupChild(idx, def, path, name)

		values := make([]any, 0, len(children))
		for _, child := range children {
			values = append(values, xmlValue(idx, child, childType, childDef, childPath))
		}

		isArray := elDef != nil && elDef.IsArray()
		if isArray {
			obj[name] = values
		} else {
			obj[name] = values[0]
		}

		// A primitive's extensions arrive as children in XML but belong in the
		// parallel "_name" object in the JSON shape.
		if exts := primitiveSidecars(idx, children, childType); exts != nil {
			if isArray {
				obj["_"+name] = exts
			} else {
				obj["_"+name] = exts[0]
			}
		}
	}
	return obj
}

// lookupChild resolves an element name against the current type and path,
// returning its definition, its FHIR type, and where its own children are
// defined.
func lookupChild(idx *conformance.Index, def *conformance.TypeDef, path, name string) (
	elDef *conformance.ElementDef, childType, childPath string, childDef *conformance.TypeDef) {
	if def == nil {
		return nil, "", "", nil
	}
	// Follow a contentReference before looking up children. Recursive
	// structures -- Questionnaire.item.item points back at "#Questionnaire.item"
	// -- otherwise stop converting at the depth the definition itself spells
	// out, silently truncating deeply nested documents.
	def, path = followContentReference(idx, def, path)
	prefix := ""
	if path != "" {
		prefix = path + "."
	}
	// Try the name directly, then as an expansion of a choice element.
	if el, ok := def.Element(prefix + name); ok {
		elDef = el
		if len(el.Types) > 0 {
			childType = el.Types[0].Code
		}
	} else if code, ok := def.ExpansionType(name); ok {
		childType = code
		// The choice element's own definition governs repetition.
		for _, e := range def.Elements {
			if e.Choice {
				for _, exp := range e.Expansions {
					if exp == name {
						elDef = e
					}
				}
			}
		}
	} else {
		return nil, "", "", nil
	}

	// An element defined only by a contentReference declares no type; it is a
	// backbone whose shape lives at the referenced path.
	if childType == "" && elDef != nil && elDef.ContentReference != "" {
		childType = "BackboneElement"
	}

	switch childType {
	case "BackboneElement", "Element":
		return elDef, childType, prefix + name, def
	case "Resource", "DomainResource":
		return elDef, childType, "", nil
	default:
		if td, ok := idx.Type(childType); ok {
			return elDef, childType, "", td
		}
		return elDef, childType, prefix + name, def
	}
}

// xmlValue converts one child element to its JSON-shaped value.
func xmlValue(idx *conformance.Index, el *xmlElement, childType string, childDef *conformance.TypeDef, childPath string) any {
	// xhtml is markup, carried as a string.
	if childType == "xhtml" || el.name == "div" {
		return "<div xmlns=\"http://www.w3.org/1999/xhtml\">" + el.inner + "</div>"
	}

	// A contained or bundled resource is a wrapper around the real resource
	// element, which supplies its own type.
	if childType == "Resource" || childType == "DomainResource" {
		if len(el.children) == 1 {
			inner := el.children[0]
			if def, ok := idx.Type(inner.name); ok {
				obj := elementToMap(idx, def, "", inner)
				obj["resourceType"] = inner.name
				return obj
			}
		}
		return map[string]any{}
	}

	// A primitive with no element children is just its value attribute.
	if isPrimitiveType(idx, childType) {
		return primitiveFromAttr(childType, el.attrs["value"])
	}

	if childDef == nil {
		childDef = idx.Types[childType]
	}
	return elementToMap(idx, childDef, childPath, el)
}

// followContentReference resolves a path whose element defers its definition to
// another location, returning the type and path that actually define it.
func followContentReference(idx *conformance.Index, def *conformance.TypeDef, path string) (*conformance.TypeDef, string) {
	if def == nil || path == "" {
		return def, path
	}
	el, ok := def.Element(path)
	if !ok || el.ContentReference == "" {
		return def, path
	}
	target := strings.TrimPrefix(el.ContentReference, "#")
	typeName, rest, _ := strings.Cut(target, ".")
	if target, ok := idx.Type(typeName); ok {
		return target, rest
	}
	return def, path
}

func isPrimitiveType(idx *conformance.Index, name string) bool {
	def, ok := idx.Type(name)
	return ok && def.Kind == "primitive-type"
}

// primitiveFromAttr converts a value attribute to the representation the JSON
// reader would have produced, so numbers and booleans are not left as strings.
func primitiveFromAttr(fhirType, raw string) any {
	switch fhirType {
	case "boolean":
		return raw == "true"
	case "integer", "positiveInt", "unsignedInt", "decimal":
		if raw == "" {
			return nil
		}
		return jsonNumber(raw)
	default:
		return raw
	}
}

// primitiveSidecars extracts the id and extension children of primitive
// elements into the "_name" objects the JSON shape uses. It returns nil when
// none of the occurrences carry any, so the common case adds nothing.
func primitiveSidecars(idx *conformance.Index, children []*xmlElement, childType string) []any {
	if !isPrimitiveType(idx, childType) {
		return nil
	}
	out := make([]any, len(children))
	any_ := false
	for i, child := range children {
		side := map[string]any{}
		if id, ok := child.attrs["id"]; ok {
			side["id"] = id
		}
		var exts []any
		extDef, _ := idx.Type("Extension")
		for _, sub := range child.children {
			if sub.name != "extension" {
				continue
			}
			exts = append(exts, elementToMap(idx, extDef, "", sub))
		}
		if len(exts) > 0 {
			side["extension"] = exts
		}
		if len(side) > 0 {
			out[i] = side
			any_ = true
		}
	}
	if !any_ {
		return nil
	}
	return out
}
