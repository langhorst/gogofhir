package resource

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
)

// jsonNumber is a number preserved in its source spelling.
//
// FHIR decimals are exact: a lab value of 1.10 asserts a precision that 1.1
// does not, and the specification requires the distinction to survive a round
// trip. Decoding into float64 destroys it before anything else can go wrong, so
// numbers are held as text and converted only where a numeric type is actually
// needed.
type jsonNumber string

// String returns the number as written, which is what makes a document's
// numbers legible to anything holding them as plain values -- validation
// comparing against a fixed value, for one.
func (n jsonNumber) String() string { return string(n) }

func (n jsonNumber) Int64() (int64, error) {
	d, err := fhirpath.NewDecimal(string(n))
	if err != nil {
		return 0, err
	}
	if !d.IsInt() {
		return 0, fmt.Errorf("%s is not an integer", n)
	}
	return d.Rat().Num().Int64(), nil
}

// FromJSON builds a document from FHIR JSON.
func FromJSON(idx *conformance.Index, data []byte) (*Node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	// Numbers stay as text; see jsonNumber.
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("resource: parsing JSON: %w", err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resource: expected a JSON object at the top level")
	}
	return newRoot(idx, convertNumbers(obj))
}

// New wraps a document the server built itself -- a Bundle, an
// OperationOutcome, a CapabilityStatement -- so it serializes through the same
// path as a stored resource and comes out in element order, in either format.
//
// Numbers must be jsonNumber or a Go scalar; see Number.
func New(idx *conformance.Index, obj map[string]any) (*Node, error) {
	return newRoot(idx, obj)
}

// Number wraps a numeric literal for a document built with New, keeping the
// spelling the caller chose rather than routing it through a float.
func Number(text string) any { return jsonNumber(text) }

// newRoot wraps an already-decoded document, resolving its resourceType.
func newRoot(idx *conformance.Index, value any) (*Node, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resource: expected an object at the top level")
	}
	typeName, _ := obj["resourceType"].(string)
	if typeName == "" {
		return nil, fmt.Errorf("resource: missing resourceType")
	}
	def, ok := idx.Type(typeName)
	if !ok {
		return nil, fmt.Errorf("resource: unknown resource type %q", typeName)
	}
	return &Node{idx: idx, def: def, path: "", name: typeName, fhirType: typeName, value: obj}, nil
}

// ConvertNumbers converts a document decoded elsewhere into the representation
// this package uses, so callers embedding stored JSON in a Bundle keep decimal
// precision.
func ConvertNumbers(v any) any { return convertNumbers(v) }

// convertNumbers replaces json.Number with jsonNumber throughout, so the rest
// of the package sees one number representation regardless of wire format.
func convertNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		return jsonNumber(x)
	case map[string]any:
		for k, item := range x {
			x[k] = convertNumbers(item)
		}
		return x
	case []any:
		for i, item := range x {
			x[i] = convertNumbers(item)
		}
		return x
	default:
		return v
	}
}
