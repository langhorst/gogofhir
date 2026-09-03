package resource_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
)

// The conformance suite's example resources double as serialization fixtures:
// they are real, published documents with narrative, extensions, contained
// resources, choice elements, and deep nesting. They live under the fhirpath
// package because that is what vendors them; nothing else is shared.
const examplesDir = "../fhirpath/testdata/r5"

func loadExample(t *testing.T, name string) *resource.Node {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(examplesDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	idx := conformance.MustLoad(conformance.R5)
	if strings.HasSuffix(name, ".json") {
		node, err := resource.FromJSON(idx, data)
		if err != nil {
			t.Fatalf("FromJSON(%s): %v", name, err)
		}
		return node
	}
	node, err := resource.FromXML(idx, data)
	if err != nil {
		t.Fatalf("FromXML(%s): %v", name, err)
	}
	return node
}

// probes are expressions exercised on every round trip. They reach primitives,
// repeated elements, nested backbones, choice elements, and extensions, so a
// serializer that drops or reorders any of those is caught.
var probes = []string{
	"$this.descendants().count()",
	"$this.descendants().ofType(string).count()",
	"$this.children().count()",
}

func snapshot(t *testing.T, node *resource.Node, extra ...string) []string {
	t.Helper()
	var out []string
	for _, expr := range append(append([]string{}, probes...), extra...) {
		out = append(out, expr+" = "+strings.Join(eval(t, node, expr), ","))
	}
	return out
}

func assertSameSnapshot(t *testing.T, label string, a, b []string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: snapshot lengths differ", label)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("%s:\n  before %s\n  after  %s", label, a[i], b[i])
		}
	}
}

// A document must survive JSON -> JSON and XML -> XML unchanged, and must also
// survive crossing formats. The cross-format case is the demanding one: it
// proves the two representations really do carry the same information, which is
// what content negotiation will rely on.
func TestRoundTripPreservesContent(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)
	for _, name := range []string{
		"patient-example.xml",
		"observation-example.xml",
		"questionnaire-example.xml",
		"valueset-example-expansion.xml",
		"parameters-example-types.xml",
		"appointment-examplereq.json",
		"patient-name-extensions.json",
		"patient-container-example.json",
	} {
		t.Run(name, func(t *testing.T) {
			original := loadExample(t, name)
			want := snapshot(t, original)

			jsonBytes, err := original.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			viaJSON, err := resource.FromJSON(idx, jsonBytes)
			if err != nil {
				t.Fatalf("re-reading serialized JSON: %v", err)
			}
			assertSameSnapshot(t, "JSON round trip", want, snapshot(t, viaJSON))

			xmlBytes, err := original.XML("")
			if err != nil {
				t.Fatalf("MarshalXML: %v", err)
			}
			viaXML, err := resource.FromXML(idx, xmlBytes)
			if err != nil {
				t.Fatalf("re-reading serialized XML: %v\n%s", err, truncate(xmlBytes))
			}
			assertSameSnapshot(t, "XML round trip", want, snapshot(t, viaXML))

			// Crossing formats: serialize as XML, read it, serialize that as
			// JSON, and read again.
			crossed, err := viaXML.MarshalJSON()
			if err != nil {
				t.Fatalf("MarshalJSON after XML: %v", err)
			}
			viaBoth, err := resource.FromJSON(idx, crossed)
			if err != nil {
				t.Fatalf("re-reading cross-format JSON: %v", err)
			}
			assertSameSnapshot(t, "XML then JSON", want, snapshot(t, viaBoth))
		})
	}
}

// Serialization is stable: the same document written twice is byte-identical,
// and a re-read of the output serializes to the same bytes again. Without that,
// stored resources and golden files churn for no reason.
func TestSerializationIsStable(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)
	node := loadExample(t, "patient-example.xml")

	first, err := node.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	second, err := node.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("MarshalJSON is not deterministic")
	}
	reread, err := resource.FromJSON(idx, first)
	if err != nil {
		t.Fatal(err)
	}
	third, err := reread.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(third) {
		t.Errorf("serialization is not a fixed point:\n  first  %s\n  reread %s",
			truncate(first), truncate(third))
	}
}

// Elements come out in the order the StructureDefinition declares them, not
// alphabetically. XML requires it, and it keeps JSON output readable.
func TestElementOrderFollowsDefinition(t *testing.T) {
	node := fromJSON(t, `{
	  "resourceType": "Patient",
	  "birthDate": "1974-12-25",
	  "active": true,
	  "name": [{"family": "Chalmers"}],
	  "id": "x"
	}`)
	out, err := node.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	// Patient declares id, then active, then name, then birthDate.
	want := []string{"resourceType", "id", "active", "name", "birthDate"}
	pos := make([]int, len(want))
	for i, key := range want {
		pos[i] = strings.Index(string(out), `"`+key+`"`)
		if pos[i] < 0 {
			t.Fatalf("%q missing from output: %s", key, out)
		}
	}
	for i := 1; i < len(pos); i++ {
		if pos[i] < pos[i-1] {
			t.Errorf("%q should follow %q:\n%s", want[i], want[i-1], out)
		}
	}
}

// Decimals must survive serialization exactly, which means writing the number
// verbatim rather than through a float.
func TestDecimalSurvivesSerialization(t *testing.T) {
	const value = "12345678901234567890.12345"
	node := fromJSON(t, `{"resourceType":"Observation","status":"final",
	  "code":{"text":"x"},"valueQuantity":{"value":`+value+`}}`)
	out, err := node.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), value) {
		t.Errorf("decimal not preserved verbatim: %s", out)
	}
}

// Narrative is markup, and must come back as markup rather than escaped text.
func TestNarrativeSurvivesXML(t *testing.T) {
	node := loadExample(t, "patient-example.xml")
	out, err := node.XML("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `<div xmlns="http://www.w3.org/1999/xhtml">`) {
		t.Errorf("narrative div missing from XML output")
	}
	if strings.Contains(string(out), "&lt;div") {
		t.Error("narrative was escaped instead of emitted as markup")
	}
}

func truncate(b []byte) string {
	if len(b) > 400 {
		return string(b[:400]) + "..."
	}
	return string(b)
}
