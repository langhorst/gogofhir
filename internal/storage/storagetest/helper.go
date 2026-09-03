package storagetest

import (
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
	"github.com/langhorst/gogofhir/internal/resource"
)

// eval evaluates a FHIRPath expression against a stored document and joins the
// results, so assertions can read a value out of stored JSON without unpacking
// it by hand.
func eval(t *testing.T, node *resource.Node, expr string) string {
	t.Helper()
	idx := conformance.MustLoad(conformance.R5)
	parsed, err := fhirpath.Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	got, err := fhirpath.EvalNode(parsed, node, resource.NewContext(idx, node))
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	parts := make([]string, len(got))
	for i, v := range got {
		parts[i] = v.String()
	}
	return strings.Join(parts, ",")
}
