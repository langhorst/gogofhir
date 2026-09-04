package resource

import (
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
)

// NewContext builds a FHIRPath evaluation context backed by the conformance
// index.
//
// The fhirpath package deliberately knows nothing about FHIR's type system --
// it is written against an interface so it can be developed and proven on its
// own -- so the index is supplied through hooks. Everything evaluating FHIRPath
// against a resource should build its context here rather than assembling one
// by hand, or type tests silently degrade to exact-name matching and
// "Patient.gender.is(string)" starts answering false.
func NewContext(idx *conformance.Index, root *Node) *fhirpath.Context {
	ctx := &fhirpath.Context{
		TypeHierarchy: func(name string) []string { return typeChain(idx, name) },
		ConformsTo: func(node fhirpath.Node, profile string) (bool, bool) {
			return conformsTo(idx, node, profile)
		},
	}
	if root != nil {
		ctx.Root = root
	}
	return ctx
}

// typeChain returns a type and its ancestors, nearest first. An unknown type
// yields nil, which lets is() and ofType() reject a misspelled type name rather
// than quietly answering false.
func typeChain(idx *conformance.Index, name string) []string {
	if _, ok := idx.Type(name); !ok {
		return nil
	}
	var chain []string
	seen := map[string]bool{}
	for cur := name; cur != "" && !seen[cur]; {
		seen[cur] = true
		chain = append(chain, cur)
		def, ok := idx.Type(cur)
		if !ok {
			break
		}
		cur = def.Base
	}
	return chain
}

// conformsTo answers whether a node claims conformance to a profile. The second
// result reports whether the question could be answered at all; an unresolvable
// profile is an error rather than a "no".
//
// Only base-resource profiles are checked here. Real profile validation --
// slicing, invariants, bindings -- is a separate subsystem; this is the
// structural half the specification defines for the FHIRPath function.
func conformsTo(idx *conformance.Index, node fhirpath.Node, profile string) (bool, bool) {
	const base = "http://hl7.org/fhir/StructureDefinition/"
	name, isBase := strings.CutPrefix(profile, base)
	if !isBase {
		// A profile outside the core namespace needs full profile validation,
		// which lives in internal/validate and is not reachable from inside a
		// FHIRPath evaluation.
		return false, false
	}
	if _, known := idx.Type(name); !known {
		return false, false
	}
	for _, typ := range typeChain(idx, node.FHIRType()) {
		if typ == name {
			return true, true
		}
	}
	return false, true
}
