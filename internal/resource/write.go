package resource

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
)

// Serialization back to both wire formats.
//
// Elements are emitted in the order the StructureDefinition declares them
// rather than alphabetically. Neither JSON nor XML requires it -- and for XML
// the schema does -- but it also makes stored documents diffable and keeps
// output stable, which matters for golden-file tests and for anyone reading a
// response.

// MarshalJSON serializes a document as FHIR JSON.
func (n *Node) MarshalJSON() ([]byte, error) {
	obj, ok := n.value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resource: cannot serialize a non-object node")
	}
	var buf bytes.Buffer
	if err := writeJSONObject(&buf, n.idx, n.def, "", obj, ""); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// JSON serializes a document as FHIR JSON. An empty indent produces compact
// output. MarshalJSON is the same thing satisfying json.Marshaler.
func (n *Node) JSON(indent string) ([]byte, error) {
	raw, err := n.MarshalJSON()
	if err != nil {
		return nil, err
	}
	if indent == "" {
		return raw, nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", indent); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// orderedKeys returns an object's keys in element-definition order. Keys the
// definition does not mention -- resourceType, and anything a document carries
// that the release does not define -- keep a stable place rather than being
// dropped: silently discarding unknown content on a round trip would be worse
// than emitting it.
func orderedKeys(idx *conformance.Index, def *conformance.TypeDef, path string, obj map[string]any) []string {
	rank := map[string]int{}
	next := 0
	// resourceType always comes first; it is how a reader identifies the type.
	rank["resourceType"] = next
	next++

	if def != nil {
		d, p := followContentReference(idx, def, path)
		prefix := ""
		if p != "" {
			prefix = p + "."
		}
		for _, el := range d.Elements {
			rest, ok := strings.CutPrefix(el.Path, prefix)
			if !ok || rest == "" || strings.Contains(rest, ".") {
				continue
			}
			names := []string{rest}
			if el.Choice {
				names = el.Expansions
			}
			for _, name := range names {
				if _, seen := rank[name]; !seen {
					rank[name] = next
					next++
				}
				// A primitive's extension sidecar sorts with the primitive.
				if _, seen := rank["_"+name]; !seen {
					rank["_"+name] = next
					next++
				}
			}
		}
	}

	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ri, iKnown := rank[keys[i]]
		rj, jKnown := rank[keys[j]]
		switch {
		case iKnown && jKnown:
			return ri < rj
		case iKnown != jKnown:
			return iKnown // defined elements before undefined ones
		default:
			return keys[i] < keys[j]
		}
	})
	return keys
}

func writeJSONObject(buf *bytes.Buffer, idx *conformance.Index, def *conformance.TypeDef, path string, obj map[string]any, _ string) error {
	buf.WriteByte('{')
	first := true
	for _, key := range orderedKeys(idx, def, path, obj) {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		encoded, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		buf.WriteByte(':')

		childDef, childPath := childLocation(idx, def, path, strings.TrimPrefix(key, "_"))
		if err := writeJSONValue(buf, idx, childDef, childPath, obj[key]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

func writeJSONValue(buf *bytes.Buffer, idx *conformance.Index, def *conformance.TypeDef, path string, v any) error {
	switch x := v.(type) {
	case map[string]any:
		return writeJSONObject(buf, idx, def, path, x, "")
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSONValue(buf, idx, def, path, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	case jsonNumber:
		// Written verbatim so the precision the value arrived with survives.
		buf.WriteString(string(x))
		return nil
	default:
		encoded, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(encoded)
		return nil
	}
}

// childLocation resolves where a child element's own children are defined,
// mirroring the reader's lookup so both agree on backbone elements and nested
// resources.
func childLocation(idx *conformance.Index, def *conformance.TypeDef, path, name string) (*conformance.TypeDef, string) {
	if def == nil {
		return nil, ""
	}
	elDef, childType, childPath, childDef := lookupChild(idx, def, path, name)
	if elDef == nil {
		return nil, ""
	}
	switch childType {
	case "Resource", "DomainResource":
		// A nested resource names its own type in the document.
		return nil, ""
	}
	if childDef != nil {
		return childDef, childPath
	}
	return def, childPath
}

// ---- XML ----

const fhirNamespace = "http://hl7.org/fhir"

// XML serializes a document as FHIR XML. An empty indent produces compact
// output.
//
// Deliberately not named MarshalXML: encoding/xml reserves that name for a
// different signature, and a method that looks like the interface but is not it
// would be picked up by nothing and confuse everyone.
func (n *Node) XML(indent string) ([]byte, error) {
	obj, ok := n.value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resource: cannot serialize a non-object node")
	}
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	if indent != "" {
		buf.WriteByte('\n')
	}
	w := &xmlWriter{buf: &buf, idx: n.idx, indent: indent}
	if err := w.element(n.fhirType, n.def, "", obj, 0, true); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type xmlWriter struct {
	buf    *bytes.Buffer
	idx    *conformance.Index
	indent string
}

func (w *xmlWriter) pad(depth int) {
	if w.indent == "" {
		return
	}
	w.buf.WriteString(strings.Repeat(w.indent, depth))
}

func (w *xmlWriter) newline() {
	if w.indent != "" {
		w.buf.WriteByte('\n')
	}
}

// element writes one complex element and its children.
func (w *xmlWriter) element(name string, def *conformance.TypeDef, path string, obj map[string]any, depth int, root bool) error {
	w.pad(depth)
	w.buf.WriteString("<" + name)
	if root {
		w.buf.WriteString(` xmlns="` + fhirNamespace + `"`)
	}
	// id and url are attributes in XML, not child elements.
	for _, attr := range []string{"id", "url"} {
		if s, ok := obj[attr].(string); ok {
			w.buf.WriteString(" " + attr + `="` + escapeXMLAttr(s) + `"`)
		}
	}

	children := w.childKeys(def, path, obj)
	if len(children) == 0 {
		w.buf.WriteString("/>")
		w.newline()
		return nil
	}
	w.buf.WriteString(">")
	w.newline()
	for _, key := range children {
		if err := w.child(def, path, key, obj, depth+1); err != nil {
			return err
		}
	}
	w.pad(depth)
	w.buf.WriteString("</" + name + ">")
	w.newline()
	return nil
}

// childKeys lists the object's keys that become child elements, in definition
// order. Attributes and the primitive sidecars are handled elsewhere.
func (w *xmlWriter) childKeys(def *conformance.TypeDef, path string, obj map[string]any) []string {
	var out []string
	for _, key := range orderedKeys(w.idx, def, path, obj) {
		switch {
		case key == "resourceType", key == "id", key == "url":
			continue
		case strings.HasPrefix(key, "_"):
			// Emitted alongside the primitive they annotate.
			continue
		}
		out = append(out, key)
	}
	return out
}

func (w *xmlWriter) child(def *conformance.TypeDef, path, key string, obj map[string]any, depth int) error {
	elDef, childType, childPath, childDef := lookupChild(w.idx, def, path, key)
	values := []any{obj[key]}
	if arr, isArr := obj[key].([]any); isArr {
		values = arr
	}
	// The parallel "_key" object carries each occurrence's id and extensions.
	var sidecars []any
	switch ext := obj["_"+key].(type) {
	case map[string]any:
		sidecars = []any{ext}
	case []any:
		sidecars = ext
	}

	for i, v := range values {
		var sidecar map[string]any
		if i < len(sidecars) {
			sidecar, _ = sidecars[i].(map[string]any)
		}
		if err := w.occurrence(key, elDef, childType, childPath, childDef, v, sidecar, depth); err != nil {
			return err
		}
	}
	return nil
}

func (w *xmlWriter) occurrence(key string, elDef *conformance.ElementDef, childType, childPath string,
	childDef *conformance.TypeDef, v any, sidecar map[string]any, depth int) error {

	// xhtml is markup: its value is already a serialized <div>.
	if childType == "xhtml" {
		if s, ok := v.(string); ok {
			w.pad(depth)
			w.buf.WriteString(s)
			w.newline()
		}
		return nil
	}

	// A nested resource is wrapped in an element named for the field, with the
	// resource's own element inside it.
	if childType == "Resource" || childType == "DomainResource" {
		inner, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		typeName, _ := inner["resourceType"].(string)
		def, known := w.idx.Type(typeName)
		if !known {
			return nil
		}
		w.pad(depth)
		w.buf.WriteString("<" + key + ">")
		w.newline()
		if err := w.element(typeName, def, "", inner, depth+1, false); err != nil {
			return err
		}
		w.pad(depth)
		w.buf.WriteString("</" + key + ">")
		w.newline()
		return nil
	}

	if obj, isObj := v.(map[string]any); isObj {
		if childDef == nil {
			childDef, _ = w.idx.Type(childType)
		}
		return w.element(key, childDef, childPath, obj, depth, false)
	}

	// A primitive: its value is an attribute, and any id or extensions become
	// children.
	w.pad(depth)
	w.buf.WriteString("<" + key)
	if v != nil {
		w.buf.WriteString(` value="` + escapeXMLAttr(primitiveText(v)) + `"`)
	}
	if id, ok := sidecar["id"].(string); ok {
		w.buf.WriteString(` id="` + escapeXMLAttr(id) + `"`)
	}
	exts, _ := sidecar["extension"].([]any)
	if len(exts) == 0 {
		w.buf.WriteString("/>")
		w.newline()
		return nil
	}
	w.buf.WriteString(">")
	w.newline()
	extDef, _ := w.idx.Type("Extension")
	for _, e := range exts {
		obj, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if err := w.element("extension", extDef, "", obj, depth+1, false); err != nil {
			return err
		}
	}
	w.pad(depth)
	w.buf.WriteString("</" + key + ">")
	w.newline()
	return nil
}

// primitiveText renders a primitive for an XML value attribute.
func primitiveText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case jsonNumber:
		return string(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(x)
	}
}

func escapeXMLAttr(s string) string {
	var b bytes.Buffer
	// EscapeText handles the attribute-significant characters; it escapes
	// newlines and tabs too, which is correct inside an attribute value.
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
