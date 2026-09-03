package validate_test

import (
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/validate"
)

// Profiles from a vendored implementation guide, exercised against the
// International Patient Summary.
//
// The point is not IPS in particular but that a guide vendored beside the core
// package validates like anything else: its profiles come from the same index,
// through the same compiler, and are checked by the same engine. US Core would
// land the same way -- see third_party/packages.lock for why it is not here.

const ipsPatient = "http://hl7.org/fhir/uv/ips/StructureDefinition/Patient-uv-ips"

func checkR4(t *testing.T, doc string, profiles ...string) []validate.Issue {
	t.Helper()
	idx := conformance.MustLoad(conformance.R4)
	node, err := resource.FromJSON(idx, []byte(doc))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	return validate.New(idx).Validate(node, validate.Options{Profiles: profiles})
}

// IPS requires a name and a birth date of every patient, neither of which the
// base Patient does.
func TestImplementationGuideProfile(t *testing.T) {
	conforming := `{
	  "resourceType": "Patient",
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">Ann</div>"},
	  "name": [{"family": "Chalmers", "given": ["Ann"]}],
	  "gender": "female",
	  "birthDate": "1974-12-25"
	}`
	assertNoErrors(t, checkR4(t, conforming, ipsPatient))

	// The same resource is perfectly valid against the base type ...
	bare := `{
	  "resourceType": "Patient",
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">x</div>"},
	  "gender": "female"
	}`
	assertNoErrors(t, checkR4(t, bare))
	// ... and fails the guide, which is the whole reason to have one.
	issues := checkR4(t, bare, ipsPatient)
	assertError(t, issues, "requires at least 1 name")
	assertError(t, issues, "requires at least 1 birthDate")
}

// A guide's profiles are reachable through meta.profile too, which is how a
// resource declares which guide it was written for.
func TestImplementationGuideProfileFromMeta(t *testing.T) {
	issues := checkR4(t, `{
	  "resourceType": "Patient",
	  "meta": {"profile": ["`+ipsPatient+`"]},
	  "gender": "female"
	}`)
	assertError(t, issues, "requires at least 1 name")
}

// Every profile the guide ships is compiled, not just the ones a test names.
func TestImplementationGuideProfilesAreLoaded(t *testing.T) {
	idx := conformance.MustLoad(conformance.R4)
	var found int
	for url := range idx.Profiles {
		if strings.Contains(url, "/uv/ips/") {
			found++
		}
	}
	if found < 20 {
		t.Errorf("the index holds %d IPS profiles, want the guide's full set", found)
	}
	if _, ok := idx.Profile(ipsPatient); !ok {
		t.Errorf("%s is missing from the index", ipsPatient)
	}
}
