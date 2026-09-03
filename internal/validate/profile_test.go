package validate_test

import (
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/validate"
)

// Profile validation, exercised against a real published profile rather than
// an invented one: hl7.org/fhir/StructureDefinition/bp, the blood pressure
// vital sign. It requires two components, slices them by LOINC code, and fixes
// the code of each slice -- which is the whole mechanism in one place.

const bloodPressureProfile = "http://hl7.org/fhir/StructureDefinition/bp"

func checkProfile(t *testing.T, doc string, profiles ...string) []validate.Issue {
	t.Helper()
	idx := conformance.MustLoad(conformance.R5)
	node, err := resource.FromJSON(idx, []byte(doc))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	return validate.New(idx).Validate(node, validate.Options{Profiles: profiles})
}

func bloodPressure(components string) string {
	return `{
	  "resourceType": "Observation",
	  "status": "final",
	  "category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category",
	                            "code": "vital-signs"}]}],
	  "code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
	  "subject": {"reference": "Patient/1"},
	  "effectiveDateTime": "2024-01-01",
	  "component": [` + components + `]
	}`
}

const systolic = `{"code": {"coding": [{"system": "http://loinc.org", "code": "8480-6"}]},
  "valueQuantity": {"value": 120, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}}`

const diastolic = `{"code": {"coding": [{"system": "http://loinc.org", "code": "8462-4"}]},
  "valueQuantity": {"value": 80, "unit": "mmHg", "system": "http://unitsofmeasure.org", "code": "mm[Hg]"}}`

func TestProfileConformingInstance(t *testing.T) {
	assertNoErrors(t, checkProfile(t, bloodPressure(systolic+","+diastolic), bloodPressureProfile))
}

// The slice is what makes this a blood pressure rather than two numbers: a
// reading with only a diastolic component satisfies neither the profile's own
// cardinality nor the systolic slice's.
func TestProfileSliceCardinality(t *testing.T) {
	issues := checkProfile(t, bloodPressure(diastolic), bloodPressureProfile)
	assertError(t, issues, "requires at least 2 component")
	assertError(t, issues, `matching the slice "SystolicBP"`)
}

// Two systolic components is the other half of the slice's cardinality, and the
// half a server that only counts occurrences would miss.
func TestProfileSliceUpperBound(t *testing.T) {
	issues := checkProfile(t, bloodPressure(systolic+","+systolic+","+diastolic), bloodPressureProfile)
	assertError(t, issues, `at most 1 component matching the slice "SystolicBP"`)
}

// A profile claimed in meta.profile is checked without being asked for
// separately: the resource said it conforms, so the server checks that it does.
func TestProfileFromMeta(t *testing.T) {
	doc := `{
	  "resourceType": "Observation", "status": "final",
	  "meta": {"profile": ["` + bloodPressureProfile + `"]},
	  "code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
	  "subject": {"reference": "Patient/1"}, "effectiveDateTime": "2024-01-01",
	  "category": [{"coding": [{"system": "http://terminology.hl7.org/CodeSystem/observation-category",
	                            "code": "vital-signs"}]}]
	}`
	assertError(t, checkProfile(t, doc), "requires at least 2 component")
}

// A profile the server does not have is reported as not checked. Silence would
// let a reader take the resource as conforming to something nobody looked at.
func TestUnknownProfileIsReportedAsUnchecked(t *testing.T) {
	issues := checkProfile(t, `{"resourceType": "Patient"}`,
		"http://example.org/StructureDefinition/invented")
	assertNoErrors(t, issues)
	found := false
	for _, issue := range issues {
		if strings.Contains(issue.Details, "NOT checked") &&
			strings.Contains(issue.Details, "invented") {
			found = true
		}
	}
	if !found {
		t.Errorf("an unknown profile must be reported as unchecked; got %v", issues)
	}
}

// A profile constrains one type, and validating a resource of another against
// it is a mistake worth naming rather than a list of spurious findings.
func TestProfileWrongType(t *testing.T) {
	assertError(t, checkProfile(t, `{"resourceType": "Patient"}`, bloodPressureProfile),
		"constrains Observation, but this is a Patient")
}

// Slices within slices are not checked, and the outcome says so: a profile
// whose nested slices went unexamined has not been fully verified.
func TestNestedSlicingIsReported(t *testing.T) {
	issues := checkProfile(t, bloodPressure(systolic+","+diastolic), bloodPressureProfile)
	found := false
	for _, issue := range issues {
		if strings.Contains(issue.Details, "slices within slices") {
			found = true
			// Extension slicing is boilerplate in nearly every profile and
			// would bury the nested slices that carry meaning.
			if strings.Contains(issue.Details, ".extension within") {
				t.Errorf("extension slicing should not be listed: %s", issue.Details)
			}
		}
	}
	if !found {
		t.Errorf("nested slicing should be reported as unchecked; got %v", issues)
	}
}
