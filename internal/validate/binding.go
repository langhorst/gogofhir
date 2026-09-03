package validate

import (
	"fmt"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
)

// Bindings: checking coded values against the value sets an element names.
//
// The build-time expansion covers the value sets FHIR itself defines, which is
// the great majority of required bindings -- 221 of 225 in R4, 248 of 264 in
// R5. What it cannot cover is exactly what the terminology policy exists for:
// SNOMED CT is licensed, LOINC and RxNorm are too large to embed, and UCUM,
// ISO 3166 and BCP 47 are external standards the packages only reference.
//
// A binding to one of those is reported as *not checked*. That wording is the
// point. Silence would let a reader conclude the code was verified, which would
// overstate what the server did and undermine its value as a conformance
// target; a hard error would make it useless offline. --strict-terminology
// turns the warnings into errors for teams that have a terminology service.

// codedValue is one coding pulled out of a document for checking.
type codedValue struct {
	system string
	code   string
	// path locates the coding, which for a CodeableConcept is one of possibly
	// several codings inside it.
	path string
}

// checkBinding checks an element's values against its binding.
func (r *run) checkBinding(field resource.Field, path string) {
	binding := field.Def.Binding
	if binding == nil || len(field.Values) == 0 {
		return
	}
	switch binding.Strength {
	case "required", "extensible":
	default:
		// Preferred and example bindings are advice. Checking them would
		// produce noise a reader learns to ignore, which costs more than it
		// gains.
		return
	}

	valueSet, known := r.v.idx.ValueSet(binding.ValueSet)
	if !known {
		r.reportUnchecked(path, binding,
			"this server has no expansion of %s", binding.ValueSet)
		return
	}
	if valueSet.Unresolvable != "" {
		r.reportUnchecked(path, binding, "%s", valueSet.Unresolvable)
		return
	}

	for i, value := range field.Values {
		valuePath := path
		if field.Def.IsArray() {
			valuePath = fmt.Sprintf("%s[%d]", path, i)
		}
		for _, coded := range codings(value, valuePath) {
			r.checkCode(coded, binding, valueSet)
		}
	}
}

func (r *run) checkCode(coded codedValue, binding *conformance.Binding, valueSet *conformance.ValueSet) {
	if coded.code == "" {
		return
	}
	found, knownSystem := valueSet.Contains(coded.system, coded.code)
	if found {
		return
	}

	// An extensible binding permits codes outside the set when none of them
	// fits, so it warns where a required binding fails.
	severity := SeverityError
	if binding.Strength == "extensible" {
		severity = SeverityWarning
	}
	switch {
	case coded.system != "" && !knownSystem:
		// A different terminology altogether, which for an extensible binding
		// is the intended escape hatch and for a required one is still wrong.
		r.report(severity, "code-invalid", coded.path,
			"the code system %s is not in %s, which this element is %s bound to",
			coded.system, valueSet.URL, binding.Strength)
	default:
		r.report(severity, "code-invalid", coded.path,
			"%q is not in %s, which this element is %s bound to (%d codes)",
			coded.code, valueSet.URL, binding.Strength, valueSet.Size())
	}
}

// reportUnchecked records a binding the server could not verify, at the
// severity the terminology policy calls for.
func (r *run) reportUnchecked(path string, binding *conformance.Binding, format string, args ...any) {
	severity := SeverityWarning
	if !r.v.StrictTerminology {
		// An extensible binding that cannot be checked is barely news: the
		// element was always allowed to carry codes from elsewhere.
		if binding.Strength == "extensible" {
			severity = SeverityInformation
		}
	} else {
		severity = SeverityError
	}
	r.report(severity, "not-supported", path,
		"the %s binding to %s was NOT checked: %s",
		binding.Strength, binding.ValueSet, fmt.Sprintf(format, args...))
}

// codings pulls the coded values out of whatever type the bound element has.
//
// A binding may sit on a code, a Coding, a CodeableConcept, or a Quantity's
// unit, and each holds its codes somewhere different.
func codings(node *resource.Node, path string) []codedValue {
	obj, isObject := node.Object()
	if !isObject {
		// A bare code or string primitive: the value is the code, and the
		// system is implied by the binding.
		text, ok := primitiveText(node.Raw())
		if !ok || text == "" {
			return nil
		}
		return []codedValue{{code: text, path: path}}
	}

	switch node.FHIRType() {
	case "Coding":
		system, _ := obj["system"].(string)
		code, _ := obj["code"].(string)
		return []codedValue{{system: system, code: code, path: path}}

	case "CodeableConcept":
		items, _ := obj["coding"].([]any)
		out := make([]codedValue, 0, len(items))
		for i, item := range items {
			coding, _ := item.(map[string]any)
			system, _ := coding["system"].(string)
			code, _ := coding["code"].(string)
			if code == "" {
				continue
			}
			out = append(out, codedValue{
				system: system, code: code,
				path: fmt.Sprintf("%s.coding[%d]", path, i),
			})
		}
		return out

	case "Quantity", "SimpleQuantity", "Age", "Count", "Distance", "Duration", "Money":
		system, _ := obj["system"].(string)
		code, _ := obj["code"].(string)
		if code == "" {
			return nil
		}
		return []codedValue{{system: system, code: code, path: path + ".code"}}

	case "CodeableReference":
		concept, ok := obj["concept"].(map[string]any)
		if !ok {
			return nil
		}
		items, _ := concept["coding"].([]any)
		out := make([]codedValue, 0, len(items))
		for i, item := range items {
			coding, _ := item.(map[string]any)
			system, _ := coding["system"].(string)
			code, _ := coding["code"].(string)
			if code != "" {
				out = append(out, codedValue{
					system: system, code: code,
					path: fmt.Sprintf("%s.concept.coding[%d]", path, i),
				})
			}
		}
		return out
	}
	return nil
}
