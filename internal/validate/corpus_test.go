package validate_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/validate"
)

// The corpus gate: every published example resource is validated, and the
// errors found are pinned.
//
// The documents are HL7's own, vendored for the FHIRPath suites, which makes
// them a far better test than anything written here: they use extensions,
// contained resources, choice elements, and narrative the way real data does.
//
// The expectations below are enforced in both directions, as the FHIRPath
// divergence list is. A file that starts failing fails the build, and so does a
// file that stops -- because an expectation that quietly became wrong is how a
// gate turns into decoration.

// expectedErrors records the files that do not validate cleanly, with why.
//
// Every one is a defect in the example rather than in the validator, which is
// the point of writing the reason down: an unexplained entry here is a bug
// somebody decided to live with.
var expectedErrors = map[string]int{
	// HL7 ships this CodeSystem with a duplicate concept code, annotated
	// "<!-- wrong! -->" in the source, so csd-1 fails as it should.
	"r4/codesystem-example.xml": 1,
	"r5/codesystem-example.xml": 1,

	// A fragment used to exercise FHIRPath, not a complete resource: it carries
	// none of ExplanationOfBenefit's required elements.
	"r4/explanationofbenefit-example.json": 11,
	"r5/explanationofbenefit-example.json": 6,

	// The contained Organization is {"resourceType":"Organization","id":"1"}
	// and nothing else, which org-1 forbids: an organization needs a name or an
	// identifier. Nothing references "#1" either, so dom-3 fails as well --
	// but only under R5. R4 spells dom-3 with ".as(canonical)" over
	// descendants(), and as() is defined for a single item, so the expression
	// is unevaluable on any document with more than one descendant. It is
	// reported as NOT checked rather than as passing, which is the whole
	// difference between an honest validator and a quiet one.
	"r4/patient-container-example.json": 1,
	"r5/patient-container-example.json": 2,

	// The example claims meta.profile = shareablevalueset and then omits the
	// title that profile requires. A real profile finding, on a real example.
	"r5/valueset-example-expansion.xml": 1,
}

func TestExampleCorpus(t *testing.T) {
	for _, release := range conformance.Releases() {
		idx := conformance.MustLoad(release)
		v := validate.New(idx)
		dir := filepath.Join("..", "fhirpath", "testdata", string(release))

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "tests.xml" || !isDocument(name) {
				continue
			}
			key := string(release) + "/" + name
			t.Run(key, func(t *testing.T) {
				raw, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("reading: %v", err)
				}
				node, err := parseDocument(idx, name, raw)
				if err != nil {
					// One input is a CDA document, which is not a FHIR
					// resource at all; the reader rejects it and there is
					// nothing to validate.
					t.Skipf("not a FHIR resource: %v", err)
				}

				issues := v.Validate(node, validate.Options{})
				var found []string
				for _, issue := range issues {
					if issue.Severity == validate.SeverityError {
						found = append(found, issue.Path+": "+issue.Details)
					}
				}
				sort.Strings(found)

				want := expectedErrors[key]
				if len(found) == want {
					return
				}
				if want == 0 {
					t.Errorf("%d unexpected error(s):\n  %s", len(found), strings.Join(found, "\n  "))
					return
				}
				t.Errorf("expected %d error(s) but found %d; if the validator improved, update expectedErrors:\n  %s",
					want, len(found), strings.Join(found, "\n  "))
			})
		}
	}
}

// TestExpectedErrorsAreLive fails when the list above names a file the corpus
// no longer holds, so a stale expectation cannot sit there unnoticed.
func TestExpectedErrorsAreLive(t *testing.T) {
	for key := range expectedErrors {
		release, name, _ := strings.Cut(key, "/")
		path := filepath.Join("..", "fhirpath", "testdata", release, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expectedErrors names %s, which is not in the corpus", key)
		}
	}
}

func isDocument(name string) bool {
	return strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".xml")
}

func parseDocument(idx *conformance.Index, name string, raw []byte) (*resource.Node, error) {
	if strings.HasSuffix(name, ".xml") {
		return resource.FromXML(idx, raw)
	}
	return resource.FromJSON(idx, raw)
}
