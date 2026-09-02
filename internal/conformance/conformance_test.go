package conformance_test

import (
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
)

// The assertions here are facts about the FHIR specification, not about this
// code: if the generator starts dropping choice expansions or reference
// targets, these break. That makes them a genuine check on the compiled index
// rather than a restatement of it.

func TestLoadBothReleases(t *testing.T) {
	for _, r := range conformance.Releases() {
		idx, err := conformance.Load(r)
		if err != nil {
			t.Fatalf("Load(%s): %v", r, err)
		}
		if idx.Release != r {
			t.Errorf("Load(%s): index reports release %s", r, idx.Release)
		}
		// The spec defines 140-plus resource types in both releases; a count
		// far below that means the generator silently discarded definitions.
		if got := len(idx.ResourceTypes()); got < 140 {
			t.Errorf("Load(%s): %d resource types, want at least 140", r, got)
		}
	}
}

func TestLoadReportsUnknownRelease(t *testing.T) {
	if _, err := conformance.Load("r3"); err == nil {
		t.Fatal("Load(r3): want an error for an unsupported release")
	}
}

func TestFHIRVersions(t *testing.T) {
	// These exact strings appear in CapabilityStatement.fhirVersion, so a
	// regression here is directly visible to clients.
	want := map[conformance.Release]string{
		conformance.R4: "4.0.1",
		conformance.R5: "5.0.0",
	}
	for r, version := range want {
		idx := conformance.MustLoad(r)
		if idx.FHIRVersion != version {
			t.Errorf("%s: FHIRVersion = %q, want %q", r, idx.FHIRVersion, version)
		}
	}
}

func TestResourceClassification(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)

	if !idx.IsResource("Patient") {
		t.Error("Patient should be a concrete resource")
	}
	// DomainResource is a resource but abstract, so it must never be routed to
	// or advertised in a CapabilityStatement.
	if idx.IsResource("DomainResource") {
		t.Error("DomainResource is abstract and should not count as a resource")
	}
	// HumanName is a datatype, not a resource — but it must still be present,
	// since navigating Patient.name.family depends on its definition.
	if idx.IsResource("HumanName") {
		t.Error("HumanName is a datatype, not a resource")
	}
	if _, ok := idx.Type("HumanName"); !ok {
		t.Error("HumanName must be in the index for navigation to work")
	}
}

func TestElementTyping(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)
	patient, ok := idx.Type("Patient")
	if !ok {
		t.Fatal("Patient missing from index")
	}

	name, ok := patient.Element("name")
	if !ok {
		t.Fatal("Patient.name missing")
	}
	if len(name.Types) != 1 || name.Types[0].Code != "HumanName" {
		t.Errorf("Patient.name types = %+v, want one HumanName", name.Types)
	}
	if !name.IsArray() {
		t.Errorf("Patient.name has max %q; it should repeat", name.Max)
	}
}

func TestChoiceExpansion(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)
	patient, _ := idx.Type("Patient")

	// Patient.deceased[x] is boolean or dateTime. The index must store it under
	// its base name and know the concrete names a document may use.
	deceased, ok := patient.Element("deceased")
	if !ok {
		t.Fatal("Patient.deceased missing (choice element should be keyed by base name)")
	}
	if !deceased.Choice {
		t.Error("Patient.deceased should be marked as a choice element")
	}
	want := []string{"deceasedBoolean", "deceasedDateTime"}
	if len(deceased.Expansions) != len(want) {
		t.Fatalf("expansions = %v, want %v", deceased.Expansions, want)
	}
	for i, w := range want {
		if deceased.Expansions[i] != w {
			t.Errorf("expansion %d = %q, want %q", i, deceased.Expansions[i], w)
		}
	}

	// The reverse mapping is what a parser needs when it meets the concrete key.
	if code, ok := patient.ExpansionType("deceasedBoolean"); !ok || code != "boolean" {
		t.Errorf("ExpansionType(deceasedBoolean) = %q, %v; want boolean, true", code, ok)
	}
	if _, ok := patient.ExpansionType("deceasedNonsense"); ok {
		t.Error("ExpansionType should reject a name that is not an expansion")
	}
}

func TestBinding(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)
	patient, _ := idx.Type("Patient")

	gender, ok := patient.Element("gender")
	if !ok {
		t.Fatal("Patient.gender missing")
	}
	if gender.Binding == nil {
		t.Fatal("Patient.gender should carry a required binding")
	}
	if gender.Binding.Strength != "required" {
		t.Errorf("binding strength = %q, want required", gender.Binding.Strength)
	}
	// The published canonical carries a "|5.0.0" suffix; keeping it would break
	// lookup by URL, so the generator strips it.
	const wantVS = "http://hl7.org/fhir/ValueSet/administrative-gender"
	if gender.Binding.ValueSet != wantVS {
		t.Errorf("binding valueSet = %q, want %q (version suffix stripped)", gender.Binding.ValueSet, wantVS)
	}
}

func TestSearchParameters(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)

	name, ok := idx.SearchParam("Patient", "name")
	if !ok {
		t.Fatal("Patient search parameter 'name' missing")
	}
	if name.Type != "string" {
		t.Errorf("Patient:name type = %q, want string", name.Type)
	}
	if name.Expression == "" {
		t.Error("Patient:name should carry a FHIRPath expression")
	}

	// Reference parameters must keep their targets: chained search resolves
	// through them.
	subject, ok := idx.SearchParam("Observation", "subject")
	if !ok {
		t.Fatal("Observation search parameter 'subject' missing")
	}
	if subject.Type != "reference" {
		t.Errorf("Observation:subject type = %q, want reference", subject.Type)
	}
	if !contains(subject.Targets, "Patient") {
		t.Errorf("Observation:subject targets = %v, want Patient among them", subject.Targets)
	}
}

func TestSearchParametersAreInherited(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)

	// _id and _lastUpdated are declared once, on Resource. Every concrete
	// resource must still resolve them, or the generator would have to copy
	// them to 160-odd types.
	for _, code := range []string{"_id", "_lastUpdated"} {
		if _, ok := idx.SearchParam("Patient", code); !ok {
			t.Errorf("Patient should inherit search parameter %q from Resource", code)
		}
	}

	all := idx.SearchParamsFor("Patient")
	if len(all) < 20 {
		t.Errorf("SearchParamsFor(Patient) returned %d parameters, want at least 20", len(all))
	}
	seen := map[string]bool{}
	for _, p := range all {
		if seen[p.Code] {
			t.Errorf("SearchParamsFor returned duplicate code %q", p.Code)
		}
		seen[p.Code] = true
	}
	if !seen["name"] || !seen["_id"] {
		t.Error("SearchParamsFor(Patient) should include both own and inherited parameters")
	}
}

func TestCompositeSearchParameter(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)

	sp, ok := idx.SearchParam("Observation", "code-value-quantity")
	if !ok {
		t.Skip("code-value-quantity not present in this release")
	}
	if sp.Type != "composite" {
		t.Errorf("type = %q, want composite", sp.Type)
	}
	if len(sp.Components) != 2 {
		t.Errorf("components = %d, want 2", len(sp.Components))
	}
	for i, c := range sp.Components {
		if c.Definition == "" || c.Expression == "" {
			t.Errorf("component %d incomplete: %+v", i, c)
		}
	}
}

func TestInvariantsAreInherited(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)

	// dom-2 is declared on DomainResource, not on Patient. Storing it once and
	// resolving through the base chain is what keeps the index from repeating
	// it on every resource.
	patient, ok := idx.Type("Patient")
	if !ok {
		t.Fatal("Patient missing from index")
	}
	for _, inv := range patient.Invariants {
		if inv.Key == "dom-2" {
			t.Fatal("dom-2 should live on DomainResource, not be copied onto Patient")
		}
	}

	var found bool
	for _, inv := range idx.Invariants("Patient") {
		if inv.Key == "dom-2" {
			found = true
			if inv.Expression == "" {
				t.Error("dom-2 should carry a FHIRPath expression")
			}
			if inv.Severity != "error" {
				t.Errorf("dom-2 severity = %q, want error", inv.Severity)
			}
		}
	}
	if !found {
		t.Error("Patient should inherit dom-2 from DomainResource")
	}
}

func TestCompartments(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)

	patient, ok := idx.Compartments["Patient"]
	if !ok {
		// The code is capitalized in the published definitions; try the other
		// spelling before failing so the test reports the real key set.
		if patient, ok = idx.Compartments["patient"]; !ok {
			keys := make([]string, 0, len(idx.Compartments))
			for k := range idx.Compartments {
				keys = append(keys, k)
			}
			t.Fatalf("no patient compartment; have %v", keys)
		}
	}
	if len(patient.Params) == 0 {
		t.Fatal("patient compartment lists no resources")
	}
	if params, ok := patient.Params["Observation"]; !ok || len(params) == 0 {
		t.Errorf("patient compartment should link Observation via a search parameter, got %v", params)
	}
}

func TestR4AndR5Differ(t *testing.T) {
	r4 := conformance.MustLoad(conformance.R4)
	r5 := conformance.MustLoad(conformance.R5)

	// A cheap guard against both files being generated from the same package:
	// R5 added resource types that R4 does not have.
	if len(r5.ResourceTypes()) <= len(r4.ResourceTypes()) {
		t.Errorf("R5 has %d resource types and R4 has %d; R5 should have more",
			len(r5.ResourceTypes()), len(r4.ResourceTypes()))
	}
	if r4.FHIRVersion == r5.FHIRVersion {
		t.Error("R4 and R5 indexes report the same FHIR version")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
