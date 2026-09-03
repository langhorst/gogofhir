package resource_test

import (
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
	"github.com/langhorst/gogofhir/internal/resource"
)

// eval is a small helper: parse an expression, evaluate it against a document,
// and render the results as strings.
func eval(t *testing.T, node *resource.Node, expr string) []string {
	t.Helper()
	idx := conformance.MustLoad(conformance.R5)
	e, err := fhirpath.Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	got, err := fhirpath.EvalNode(e, node, resource.NewContext(idx, node))
	if err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	out := make([]string, len(got))
	for i, v := range got {
		out[i] = v.String()
	}
	return out
}

func fromJSON(t *testing.T, doc string) *resource.Node {
	t.Helper()
	node, err := resource.FromJSON(conformance.MustLoad(conformance.R5), []byte(doc))
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	return node
}

func fromXML(t *testing.T, doc string) *resource.Node {
	t.Helper()
	node, err := resource.FromXML(conformance.MustLoad(conformance.R5), []byte(doc))
	if err != nil {
		t.Fatalf("FromXML: %v", err)
	}
	return node
}

func assertEqual(t *testing.T, expr string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %v, want %v", expr, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: got %v, want %v", expr, got, want)
			return
		}
	}
}

// The two readers must produce documents that navigate identically. That is the
// claim the design rests on: navigation, evaluation, and validation are written
// once, against one shape, and work for either wire format. If the readers ever
// diverge, everything downstream inherits the divergence.
func TestJSONAndXMLNavigateIdentically(t *testing.T) {
	jsonDoc := `{
	  "resourceType": "Patient",
	  "id": "example",
	  "active": true,
	  "name": [
	    {"use": "official", "family": "Chalmers", "given": ["Peter", "James"]},
	    {"use": "usual", "given": ["Jim"]}
	  ],
	  "birthDate": "1974-12-25",
	  "deceasedBoolean": false,
	  "telecom": [{"system": "phone", "value": "(03) 5555 6473", "rank": 1}]
	}`
	xmlDoc := `<Patient xmlns="http://hl7.org/fhir">
	  <id value="example"/>
	  <active value="true"/>
	  <name><use value="official"/><family value="Chalmers"/><given value="Peter"/><given value="James"/></name>
	  <name><use value="usual"/><given value="Jim"/></name>
	  <birthDate value="1974-12-25"/>
	  <deceasedBoolean value="false"/>
	  <telecom><system value="phone"/><value value="(03) 5555 6473"/><rank value="1"/></telecom>
	</Patient>`

	j, x := fromJSON(t, jsonDoc), fromXML(t, xmlDoc)
	for _, expr := range []string{
		"Patient.id",
		"Patient.active",
		"Patient.name.count()",
		"Patient.name.family",
		"Patient.name.given",
		"Patient.name.where(use = 'official').given",
		"Patient.birthDate",
		"Patient.deceased",
		"Patient.deceased.ofType(boolean)",
		"Patient.telecom.rank",
		"Patient.name.given.count()",
		"Patient.descendants().count()",
	} {
		fromJSONResult := eval(t, j, expr)
		fromXMLResult := eval(t, x, expr)
		assertEqual(t, "XML vs JSON: "+expr, fromXMLResult, fromJSONResult)
	}
}

// A choice element is stored under an expanded name but addressed by its base
// name. Both spellings must reach it.
func TestChoiceElements(t *testing.T) {
	node := fromJSON(t, `{
	  "resourceType": "Observation",
	  "status": "final",
	  "code": {"text": "weight"},
	  "valueQuantity": {"value": 185, "unit": "lbs", "system": "http://unitsofmeasure.org", "code": "[lb_av]"}
	}`)

	assertEqual(t, "value.unit", eval(t, node, "Observation.value.unit"), []string{"lbs"})
	assertEqual(t, "valueQuantity.unit", eval(t, node, "Observation.valueQuantity.unit"), []string{"lbs"})
	assertEqual(t, "value.ofType(Quantity).value", eval(t, node, "Observation.value.ofType(Quantity).value"), []string{"185"})
	// A quantity converts to a System Quantity for comparison.
	assertEqual(t, "value > 100 '[lb_av]'", eval(t, node, "Observation.value > 100 '[lb_av]'"), []string{"true"})
}

// FHIR decimals must survive the round trip exactly: 1.10 asserts a precision
// that 1.1 does not, and float64 would silently destroy the distinction.
func TestDecimalPrecisionIsPreserved(t *testing.T) {
	node := fromJSON(t, `{
	  "resourceType": "Observation",
	  "status": "final",
	  "code": {"text": "x"},
	  "valueQuantity": {"value": 1.10}
	}`)
	assertEqual(t, "value.value", eval(t, node, "Observation.value.value"), []string{"1.10"})

	// A value beyond float64's exact range must not be rounded.
	big := fromJSON(t, `{
	  "resourceType": "Observation",
	  "status": "final",
	  "code": {"text": "x"},
	  "valueQuantity": {"value": 12345678901234567890.12345}
	}`)
	assertEqual(t, "large decimal", eval(t, big, "Observation.value.value"),
		[]string{"12345678901234567890.12345"})
}

// A primitive is both a value and a node: it has extensions of its own, and it
// may carry them with no value at all.
func TestPrimitiveExtensions(t *testing.T) {
	jsonDoc := `{
	  "resourceType": "Patient",
	  "birthDate": "1974-12-25",
	  "_birthDate": {
	    "extension": [{"url": "http://hl7.org/fhir/StructureDefinition/patient-birthTime",
	                   "valueDateTime": "1974-12-25T14:35:45-05:00"}]
	  }
	}`
	xmlDoc := `<Patient xmlns="http://hl7.org/fhir">
	  <birthDate value="1974-12-25">
	    <extension url="http://hl7.org/fhir/StructureDefinition/patient-birthTime">
	      <valueDateTime value="1974-12-25T14:35:45-05:00"/>
	    </extension>
	  </birthDate>
	</Patient>`

	for name, node := range map[string]*resource.Node{"json": fromJSON(t, jsonDoc), "xml": fromXML(t, xmlDoc)} {
		t.Run(name, func(t *testing.T) {
			assertEqual(t, "birthDate", eval(t, node, "Patient.birthDate"), []string{"1974-12-25"})
			assertEqual(t, "extension count", eval(t, node, "Patient.birthDate.extension.count()"), []string{"1"})
			assertEqual(t, "extension by url",
				eval(t, node, "Patient.birthDate.extension('http://hl7.org/fhir/StructureDefinition/patient-birthTime').value.exists()"),
				[]string{"true"})
			assertEqual(t, "hasValue", eval(t, node, "Patient.birthDate.hasValue()"), []string{"true"})
		})
	}
}

// A primitive present only as extensions has no value, and behaves as empty
// rather than as an error.
func TestPrimitiveWithoutValue(t *testing.T) {
	node := fromJSON(t, `{
	  "resourceType": "Patient",
	  "name": [{"_family": {"extension": [{"url": "http://example.com/x", "valueString": "y"}]}}]
	}`)
	assertEqual(t, "family exists", eval(t, node, "Patient.name.family.exists()"), []string{"true"})
	assertEqual(t, "hasValue", eval(t, node, "Patient.name.family.hasValue()"), []string{"false"})
	// String functions see nothing there rather than failing.
	assertEqual(t, "length", eval(t, node, "Patient.name.family.length()"), nil)
}

// Recursive structures are defined by contentReference. Both readers must
// follow it, or documents silently truncate at the depth the definition itself
// spells out.
func TestRecursiveBackboneElements(t *testing.T) {
	xmlDoc := `<Questionnaire xmlns="http://hl7.org/fhir">
	  <status value="active"/>
	  <item><linkId value="1"/><type value="group"/>
	    <item><linkId value="1.1"/><type value="group"/>
	      <item><linkId value="1.1.1"/><type value="string"/></item>
	    </item>
	  </item>
	</Questionnaire>`
	jsonDoc := `{"resourceType":"Questionnaire","status":"active","item":[
	  {"linkId":"1","type":"group","item":[
	    {"linkId":"1.1","type":"group","item":[
	      {"linkId":"1.1.1","type":"string"}]}]}]}`

	for name, node := range map[string]*resource.Node{"json": fromJSON(t, jsonDoc), "xml": fromXML(t, xmlDoc)} {
		t.Run(name, func(t *testing.T) {
			assertEqual(t, "descendant linkIds",
				eval(t, node, "Questionnaire.descendants().linkId"),
				[]string{"1", "1.1", "1.1.1"})
			assertEqual(t, "repeat(item) count",
				eval(t, node, "Questionnaire.repeat(item).count()"), []string{"3"})
		})
	}
}

// A contained resource carries its own type and navigates as one.
func TestContainedResources(t *testing.T) {
	node := fromJSON(t, `{
	  "resourceType": "Patient",
	  "contained": [{"resourceType": "Organization", "id": "org1", "name": "Acme"}],
	  "managingOrganization": {"reference": "#org1"}
	}`)
	assertEqual(t, "contained.id", eval(t, node, "Patient.contained.id"), []string{"org1"})
	assertEqual(t, "contained type", eval(t, node, "Patient.contained.ofType(Organization).name"), []string{"Acme"})
}

// Type tests follow the FHIR hierarchy, which is only available through the
// conformance index -- so this also checks that NewContext wires it in.
func TestTypeHierarchy(t *testing.T) {
	node := fromJSON(t, `{"resourceType": "Patient", "gender": "male", "active": true}`)
	// gender is a code, and a code is a string.
	assertEqual(t, "gender is code", eval(t, node, "Patient.gender.is(code)"), []string{"true"})
	assertEqual(t, "gender is string", eval(t, node, "Patient.gender.is(string)"), []string{"true"})
	// A FHIR primitive is not the System type of the same name.
	assertEqual(t, "active is System.Boolean", eval(t, node, "Patient.active.is(System.Boolean)"), []string{"false"})
	assertEqual(t, "active is boolean", eval(t, node, "Patient.active.is(boolean)"), []string{"true"})
}

func TestReaderErrors(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)
	for _, tc := range []struct{ name, doc string }{
		{"not an object", `["a"]`},
		{"no resourceType", `{"id": "x"}`},
		{"unknown resourceType", `{"resourceType": "Nonexistent"}`},
		{"malformed", `{"resourceType":`},
	} {
		if _, err := resource.FromJSON(idx, []byte(tc.doc)); err == nil {
			t.Errorf("FromJSON(%s): expected an error", tc.name)
		}
	}
	if _, err := resource.FromXML(idx, []byte(`<Nonexistent xmlns="http://hl7.org/fhir"/>`)); err == nil {
		t.Error("FromXML with an unknown resource type: expected an error")
	}
}

// A transaction bundle rewrites references between its entries, and finding
// them structurally -- any JSON key named "reference" -- is wrong: several
// elements across R4 and R5 are named that and hold plain URIs. Rewriting one
// of those would corrupt the resource, so the walk is schema-driven.
func TestReferenceWalkIgnoresLookalikes(t *testing.T) {
	node := fromJSON(t, `{
	  "resourceType": "DetectedIssue",
	  "status": "final",
	  "reference": "http://example.org/guidance",
	  "subject": {"reference": "urn:uuid:1"},
	  "author": {"reference": "urn:uuid:2"},
	  "evidence": [{"detail": [{"reference": "Observation/3"}]}]
	}`)

	got := node.References()
	want := []string{"urn:uuid:1", "urn:uuid:2", "Observation/3"}
	if len(got) != len(want) {
		t.Fatalf("References() = %v, want %v", got, want)
	}
	for _, reference := range got {
		if reference == "http://example.org/guidance" {
			t.Fatal("DetectedIssue.reference is a uri, not a Reference; it must not be collected")
		}
	}

	changed := node.RewriteReferences(func(reference string) (string, bool) {
		switch reference {
		case "urn:uuid:1":
			return "Patient/a", true
		case "urn:uuid:2":
			return "Practitioner/b", true
		}
		return "", false
	})
	if changed != 2 {
		t.Errorf("RewriteReferences changed %d references, want 2", changed)
	}
	obj, _ := node.Object()
	if got, _ := obj["reference"].(string); got != "http://example.org/guidance" {
		t.Errorf("DetectedIssue.reference was rewritten to %q", got)
	}
	if eval(t, node, "DetectedIssue.subject.reference")[0] != "Patient/a" {
		t.Errorf("subject was not rewritten: %v", node.References())
	}
}
