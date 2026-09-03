package validate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/validate"
)

func check(t *testing.T, doc string, configure ...func(*validate.Validator)) []validate.Issue {
	t.Helper()
	idx := conformance.MustLoad(conformance.R5)
	node, err := resource.FromJSON(idx, []byte(doc))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	v := validate.New(idx)
	for _, apply := range configure {
		apply(v)
	}
	return v.Validate(node, validate.Options{})
}

// errorsFor returns the details of every error, for assertions that care what
// was said rather than only that something was.
func errorsFor(issues []validate.Issue) []string {
	var out []string
	for _, issue := range issues {
		if issue.Severity == validate.SeverityError {
			out = append(out, issue.Path+": "+issue.Details)
		}
	}
	return out
}

// assertError insists that exactly one error mentions a phrase, and reports
// everything found when it does not.
func assertError(t *testing.T, issues []validate.Issue, phrase string) {
	t.Helper()
	for _, detail := range errorsFor(issues) {
		if strings.Contains(detail, phrase) {
			return
		}
	}
	t.Errorf("no error mentioning %q; got:\n  %s", phrase, strings.Join(errorsFor(issues), "\n  "))
}

func assertNoErrors(t *testing.T, issues []validate.Issue) {
	t.Helper()
	if found := errorsFor(issues); len(found) > 0 {
		t.Errorf("unexpected errors:\n  %s", strings.Join(found, "\n  "))
	}
}

func TestValidResourcePasses(t *testing.T) {
	issues := check(t, `{
	  "resourceType": "Patient",
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">Ann</div>"},
	  "identifier": [{"system": "http://example.org/mrn", "value": "A1"}],
	  "name": [{"family": "Chalmers", "given": ["Ann"]}],
	  "gender": "female",
	  "birthDate": "1974-12-25",
	  "deceasedBoolean": false
	}`)
	assertNoErrors(t, issues)
}

// An element nobody defined is the one error a schema-driven server must not
// shrug off: it is a typo, a resource from another FHIR version, or an
// extension written without the extension mechanism.
func TestUnknownElement(t *testing.T) {
	issues := check(t, `{"resourceType": "Patient", "favouriteColour": "blue"}`)
	assertError(t, issues, `"favouriteColour" is not an element of Patient`)
}

func TestCardinality(t *testing.T) {
	// gender does not repeat.
	assertError(t, check(t, `{"resourceType": "Patient", "gender": ["female", "male"]}`),
		"does not repeat")
	// name does, so one value still has to be an array.
	assertError(t, check(t, `{"resourceType": "Patient", "name": {"family": "Chalmers"}}`),
		"must be written as an array")
	// Observation.status is required.
	assertError(t, check(t, `{"resourceType": "Observation", "code": {"text": "x"}}`),
		"status is required")
}

// A choice element takes one value. A document setting two has said something
// the type system cannot represent, and picking one would be a guess.
func TestChoiceElement(t *testing.T) {
	issues := check(t, `{
	  "resourceType": "Patient",
	  "deceasedBoolean": true,
	  "deceasedDateTime": "2020-01-01"
	}`)
	assertError(t, issues, "a choice element takes one value")
}

func TestPrimitiveLexicalForm(t *testing.T) {
	cases := []struct{ doc, phrase string }{
		{`{"resourceType": "Patient", "birthDate": "25-12-1974"}`, "is not a valid date"},
		{`{"resourceType": "Patient", "birthDate": ""}`, "must not be empty"},
		{`{"resourceType": "Patient", "id": "not a valid id!"}`, "is not a valid id"},
		{`{"resourceType": "Patient", "active": "yes"}`, "is not a valid boolean"},
		{`{"resourceType": "Patient", "name": [{"family": {"nested": 1}}]}`, "the document holds an object"},
	}
	for _, tc := range cases {
		t.Run(tc.phrase, func(t *testing.T) {
			assertError(t, check(t, tc.doc), tc.phrase)
		})
	}
}

// A reference pointing at a type the element does not permit is definitional:
// no lookup is needed to know it is wrong, which is why it is an error rather
// than a warning.
func TestReferenceTargetTypes(t *testing.T) {
	issues := check(t, `{
	  "resourceType": "Observation", "status": "final", "code": {"text": "x"},
	  "performer": [{"reference": "Medication/1"}]
	}`)
	assertError(t, issues, "but this points at a Medication")

	// A permitted target, a contained reference and a bundle placeholder are
	// all fine.
	assertNoErrors(t, check(t, `{
	  "resourceType": "Observation", "status": "final", "code": {"text": "x"},
	  "performer": [{"reference": "Practitioner/1"}, {"reference": "#local"},
	                {"reference": "urn:uuid:11111111-1111-1111-1111-111111111111"}],
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">x</div>"},
	  "contained": [{"resourceType": "Practitioner", "id": "local"}]
	}`))
}

func TestRequiredBinding(t *testing.T) {
	issues := check(t, `{"resourceType": "Patient", "gender": "lady"}`)
	assertError(t, issues, "is not in http://hl7.org/fhir/ValueSet/administrative-gender")
	assertError(t, issues, "required bound")
}

// An extensible binding permits codes from elsewhere when none of its own fits,
// so an unknown code is a warning rather than an error.
func TestExtensibleBindingWarns(t *testing.T) {
	issues := check(t, `{
	  "resourceType": "Patient",
	  "maritalStatus": {"coding": [
	    {"system": "http://example.org/marital", "code": "situationship"}]}
	}`)
	assertNoErrors(t, issues)
	found := false
	for _, issue := range issues {
		if issue.Severity == validate.SeverityWarning && strings.Contains(issue.Details, "extensible") {
			found = true
		}
	}
	if !found {
		t.Errorf("an unknown code under an extensible binding should warn; got %v", issues)
	}
}

// The terminology policy: a binding this server cannot resolve offline is
// reported as NOT CHECKED. Saying nothing would let a reader conclude the code
// was verified, which would overstate what the server did.
func TestUncheckableBindingIsReportedAsUnchecked(t *testing.T) {
	// Resource.language is required-bound to all-languages, which draws on
	// BCP 47 -- an external standard the packages only reference.
	doc := `{"resourceType": "Patient", "language": "not-a-language-tag"}`
	issues := check(t, doc)
	assertNoErrors(t, issues)

	unchecked := ""
	for _, issue := range issues {
		if strings.Contains(issue.Details, "NOT checked") {
			unchecked = issue.Details
		}
	}
	if unchecked == "" {
		t.Fatalf("an unresolvable binding must be reported as unchecked; got %v", issues)
	}
	if !strings.Contains(unchecked, "bcp:47") {
		t.Errorf("the message should say which system could not be checked: %q", unchecked)
	}

	// Under --strict-terminology the same case is an error.
	strict := check(t, doc, func(v *validate.Validator) { v.StrictTerminology = true })
	assertError(t, strict, "NOT checked")
}

// Invariants come from the specification itself, evaluated as FHIRPath.
func TestInvariants(t *testing.T) {
	// A contained resource must not itself contain resources (dom-2), and a
	// contained resource has no narrative of its own (dom-1 in R4 / dom-6).
	issues := check(t, `{
	  "resourceType": "Patient",
	  "contained": [{"resourceType": "Organization", "id": "o1"}]
	}`)
	assertError(t, issues, "org-1")

	// An invariant on the resource itself: Observation.dataAbsentReason may not
	// accompany a value.
	issues = check(t, `{
	  "resourceType": "Observation", "status": "final", "code": {"text": "x"},
	  "valueString": "present",
	  "dataAbsentReason": {"coding": [
	    {"system": "http://terminology.hl7.org/CodeSystem/data-absent-reason", "code": "unknown"}]}
	}`)
	assertError(t, issues, "obs-6")
}

// %resource is the resource an element belongs to, which for a contained
// resource is that resource rather than the one carrying it. Getting this wrong
// makes every invariant on a contained resource evaluate against the wrong
// document.
func TestContainedResourceScope(t *testing.T) {
	issues := check(t, `{
	  "resourceType": "Patient",
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">p</div>"},
	  "contained": [{"resourceType": "Organization", "id": "o1", "name": "General Hospital"}],
	  "managingOrganization": {"reference": "#o1"}
	}`)
	assertNoErrors(t, issues)
}

func TestNotAResource(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)
	node, err := resource.FromJSON(idx, []byte(`{"resourceType": "HumanName", "family": "X"}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	issues := validate.New(idx).Validate(node, validate.Options{})
	assertError(t, issues, "is not a resource type")
}

// Every finding must carry a location: an error without one leaves the client
// to search a large document by hand.
func TestIssuesCarryPaths(t *testing.T) {
	issues := check(t, `{
	  "resourceType": "Patient",
	  "name": [{"family": "A"}, {"family": "B", "nosuch": 1}]
	}`)
	assertError(t, issues, "not an element of HumanName")
	for _, issue := range issues {
		if issue.Path == "" {
			t.Errorf("issue without a path: %+v", issue)
		}
	}
	found := false
	for _, issue := range issues {
		if issue.Path == "Patient.name[1].nosuch" {
			found = true
		}
	}
	if !found {
		t.Errorf("the path should name the offending occurrence; got %v", paths(issues))
	}
}

func paths(issues []validate.Issue) []string {
	out := make([]string, len(issues))
	for i, issue := range issues {
		out[i] = fmt.Sprintf("%s(%s)", issue.Path, issue.Severity)
	}
	return out
}

// Both releases must validate, and the checks must not depend on R5's spelling
// of anything.
func TestR4(t *testing.T) {
	idx := conformance.MustLoad(conformance.R4)
	node, err := resource.FromJSON(idx, []byte(`{
	  "resourceType": "Patient", "gender": "lady", "birthDate": "1974-12-25"
	}`))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	assertError(t, validate.New(idx).Validate(node, validate.Options{}),
		"is not in http://hl7.org/fhir/ValueSet/administrative-gender")
}
