package validate

import (
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
	"github.com/langhorst/gogofhir/internal/resource"
)

// Invariants: the FHIRPath constraints the specification attaches to types and
// elements.
//
// Two kinds apply to any node. Those declared on the node's own type at its
// root -- ele-1 on Element, dom-3 on DomainResource, pat-1 on Patient.contact
// -- and those the owning type declares against this element's path. The index
// records each constraint once on the type that declares it, so both are found
// by walking the base chain from the node's type.
//
// An invariant this engine cannot evaluate is reported as unevaluated, never as
// satisfied. Some published constraints call functions no server implements
// offline (terminology subsumption, HTML checks), and treating those as passing
// would be a claim the server has not earned.

// compiledExpr is a parsed invariant, or the reason it could not be parsed.
type compiledExpr struct {
	expr fhirpath.Expr
	err  error
}

// checkInvariants evaluates every constraint that applies to a node.
func (r *run) checkInvariants(node *resource.Node, path string, scope *resource.Node) {
	typeName := node.FHIRType()
	if typeName == "" {
		return
	}
	for _, inv := range r.v.idx.Invariants(typeName) {
		if inv.Path != "" {
			// A path-scoped constraint belongs to the element at that path
			// inside the type, and is evaluated when the walk reaches it.
			continue
		}
		r.evaluate(node, path, inv, scope)
	}
}

// evaluate runs one invariant against one node.
func (r *run) evaluate(node *resource.Node, path string, inv *conformance.Invariant, scope *resource.Node) {
	if inv.Expression == "" {
		return
	}
	compiled := r.v.expression(inv.Expression)
	if compiled.err != nil {
		r.report(SeverityInformation, "not-supported", path,
			"the invariant %s could not be parsed, so it was not checked: %v", inv.Key, compiled.err)
		return
	}

	// %resource is the resource the element belongs to and %rootResource the
	// outermost one; they differ exactly when a resource is contained in
	// another, which is where invariants most often go wrong.
	ctx := resource.NewContext(r.v.idx, scope)
	ctx.Vars = map[string]fhirpath.Collection{"rootResource": {r.node}}
	values, err := fhirpath.EvalNode(compiled.expr, node, ctx)
	if err != nil {
		// An expression that fails at runtime usually means it calls something
		// this engine does not provide. Saying so is the honest answer; saying
		// nothing would let it pass as satisfied.
		r.add(Issue{
			Severity: SeverityInformation, Code: "not-supported", Path: path, Key: inv.Key,
			Details: "the invariant " + inv.Key + " could not be evaluated, so it was not checked: " + err.Error(),
		})
		return
	}

	satisfied, known := singletonBoolean(values)
	if !known {
		r.add(Issue{
			Severity: SeverityInformation, Code: "not-supported", Path: path, Key: inv.Key,
			Details: "the invariant " + inv.Key + " did not evaluate to a single boolean, so it was not checked",
		})
		return
	}
	if satisfied {
		return
	}
	severity := SeverityError
	if inv.Severity == "warning" {
		severity = SeverityWarning
	}
	r.add(Issue{
		Severity: severity, Code: "invariant", Path: path, Key: inv.Key,
		Details: inv.Key + ": " + strings.TrimSpace(inv.Human),
	})
}

// singletonBoolean applies FHIRPath's rule for a constraint's result: exactly
// one boolean is an answer, and an empty collection is not.
func singletonBoolean(values fhirpath.Collection) (result, known bool) {
	if len(values) != 1 {
		return false, false
	}
	b, ok := values[0].(fhirpath.Boolean)
	if !ok {
		return false, false
	}
	return bool(b), true
}

// expression parses and caches an invariant's FHIRPath.
func (v *Validator) expression(source string) compiledExpr {
	v.mu.Lock()
	defer v.mu.Unlock()
	if compiled, ok := v.compiled[source]; ok {
		return compiled
	}
	expr, err := fhirpath.Parse(source)
	compiled := compiledExpr{expr: expr, err: err}
	v.compiled[source] = compiled
	return compiled
}
