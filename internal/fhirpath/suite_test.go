package fhirpath_test

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
	"github.com/langhorst/gogofhir/internal/resource"
)

// The official HL7 FHIRPath conformance suites.
//
// This is the measure of the engine: an implementation that does not pass these
// is not a reference implementation, whatever else it does. The suites and the
// resources they evaluate against are vendored under testdata (see
// third_party/packages.lock), so this runs offline as part of `make check`.
//
// The two suites differ in shape -- R5 namespaces its XML, R4 does not -- so
// the loader accepts either.

// suiteTests is the parsed form of one tests.xml.
type suiteTests struct {
	Groups []suiteGroup `xml:"group"`
}

type suiteGroup struct {
	Name  string      `xml:"name,attr"`
	Tests []suiteTest `xml:"test"`
}

type suiteTest struct {
	Name      string `xml:"name,attr"`
	InputFile string `xml:"inputfile,attr"`
	Predicate bool   `xml:"predicate,attr"`
	Mode      string `xml:"mode,attr"`
	Ordered   bool   `xml:"ordered,attr"`
	Version   string `xml:"version,attr"`

	Expression struct {
		Text string `xml:",chardata"`
		// Invalid marks a test that must fail: "syntax", "semantic", or
		// "execution". They are all failures to us -- we do not run a separate
		// static-analysis pass -- but the distinction is preserved for
		// reporting.
		Invalid string `xml:"invalid,attr"`
	} `xml:"expression"`

	Outputs []struct {
		Type string `xml:"type,attr"`
		Text string `xml:",chardata"`
	} `xml:"output"`
}

// modesNotImplemented are evaluation modes outside the FHIRPath engine itself.
// They are reported as skipped rather than silently passed.
var modesNotImplemented = map[string]string{
	"cda":  "CDA documents use a different model than FHIR resources",
	"html": "requires the htmlChecks() validation hook",
	"tx":   "requires a terminology server",
}

func loadSuite(t *testing.T, path string) *suiteTests {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading suite: %v", err)
	}
	var suite suiteTests
	// encoding/xml matches on local names when the struct tags carry none, so
	// the same types load the namespaced R5 suite and the bare R4 one.
	if err := xml.Unmarshal(raw, &suite); err != nil {
		t.Fatalf("parsing suite %s: %v", path, err)
	}
	return &suite
}

// inputCache avoids re-reading and re-parsing patient-example.xml for the ~900
// tests that use it.
type inputCache struct {
	dir   string
	idx   *conformance.Index
	nodes map[string]*resource.Node
}

func (c *inputCache) load(name string) (*resource.Node, error) {
	if n, ok := c.nodes[name]; ok {
		return n, nil
	}
	data, err := os.ReadFile(filepath.Join(c.dir, name))
	if err != nil {
		return nil, err
	}
	var node *resource.Node
	if strings.HasSuffix(name, ".json") {
		node, err = resource.FromJSON(c.idx, data)
	} else {
		node, err = resource.FromXML(c.idx, data)
	}
	if err != nil {
		return nil, err
	}
	c.nodes[name] = node
	return node, nil
}

// knownDivergences are the conformance cases this engine deliberately does not
// pass, each with the reason. They are reported but do not fail the build.
//
// The list is kept honest in both directions: an unlisted failure fails the
// suite, and a listed case that starts passing also fails it, so a fixed
// divergence cannot linger here unnoticed.
//
// Keys are "<release>/<group>/<test>".
var knownDivergences = map[string]string{
	// The reference implementation is not self-consistent about rounding a
	// boundary to a precision coarser than the value's own: it wants
	// 1.587.highBoundary(2) = 1.59 (rounding away from the value) but
	// 0.0034.highBoundary(1) = 0.0 (rounding toward it). We implement the rule
	// that actually bounds the interval. See roundOutward.
	"r4/HighBoundary/HighBoundaryDecimal15": "reference rounding is inconsistent for coarse precision",
	"r5/HighBoundary/HighBoundaryDecimal15": "reference rounding is inconsistent for coarse precision",
	"r4/HighBoundary/HighBoundaryDecimal16": "reference rounding is inconsistent for coarse precision",
	"r5/HighBoundary/HighBoundaryDecimal16": "reference rounding is inconsistent for coarse precision",
	"r4/LowBoundary/LowBoundaryDecimal15":   "reference rounding is inconsistent for coarse precision",
	"r5/LowBoundary/LowBoundaryDecimal15":   "reference rounding is inconsistent for coarse precision",

	// These expect errors that only a static analyser can raise: navigating an
	// element the type does not define is a runtime no-op yielding empty, and
	// diagnosing it needs the expression to be checked against a type before
	// evaluation. A static checker is worth building -- it would catch typos in
	// stored search parameters and invariants -- but it is a separate component
	// from the evaluator.
	"r4/testBasics/testSimpleFail":             "needs static type checking of element names",
	"r5/testBasics/testSimpleFail":             "needs static type checking of element names",
	"r4/testBasics/testSimpleWithWrongContext": "needs static type checking of element names",
	"r5/testBasics/testSimpleWithWrongContext": "needs static type checking of element names",
	"r4/testObservations/testPolymorphismAsB":  "needs static type checking of element names",
	"r5/testObservations/testPolymorphismAsB":  "needs static type checking of element names",
	"r4/testObservations/testPolymorphismB":    "needs static type checking of element names",
	"r5/testObservations/testPolymorphismB":    "needs static type checking of element names",
	"r4/testDollar/testDollarOrderNotAllowed":  "needs static type checking of element names",
	"r5/testDollar/testDollarOrderNotAllowed":  "needs static type checking of element names",
	"r4/polymorphics/testPolymorphicsB":        "strict mode rejects naming a choice element's expansion directly",
	"r5/polymorphics/testPolymorphicsB":        "strict mode rejects naming a choice element's expansion directly",

	// Multiplying quantities needs real UCUM algebra: cm times m is m2 only
	// after converting and combining dimensions. The unit table here compares
	// within a dimension but does not derive new ones.
	"r4/testQuantity/testQuantity9": "needs UCUM dimensional algebra for unit products",
	"r5/testQuantity/testQuantity9": "needs UCUM dimensional algebra for unit products",

	// The two suites disagree: R4 expects "+ 0.1 's'" to truncate to zero,
	// R5 expects it to add 100 milliseconds. R5 is the maintained suite (the
	// repository marks r4 as no longer maintained), so we follow it.
	"r4/testPlus/testPlusDate19": "R4 and R5 suites disagree; we follow R5",

	// Resolving references across a Bundle, which the evaluator supports
	// through Context.ResolveReference but no store is wired to here.
	"r5/miscEngineTests/testMultipleResolve": "needs a reference resolver over the containing Bundle",
}

type suiteResult struct {
	passed   int
	failed   []string
	diverged []string
	// fixed lists known divergences that unexpectedly passed, so the list above
	// cannot go stale.
	fixed   []string
	skipped map[string]int
}

func runSuite(t *testing.T, release conformance.Release) *suiteResult {
	t.Helper()
	idx, err := conformance.Load(release)
	if err != nil {
		t.Fatalf("loading conformance index: %v", err)
	}
	dir := filepath.Join("testdata", string(release))
	suite := loadSuite(t, filepath.Join(dir, "tests.xml"))
	cache := &inputCache{dir: dir, idx: idx, nodes: map[string]*resource.Node{}}

	res := &suiteResult{skipped: map[string]int{}}
	for _, group := range suite.Groups {
		for _, tc := range group.Tests {
			if reason, skip := modesNotImplemented[tc.Mode]; skip {
				res.skipped[reason]++
				continue
			}
			key := fmt.Sprintf("%s/%s/%s", release, group.Name, tc.Name)
			reason, isKnown := knownDivergences[key]
			err := runOne(cache, tc)
			switch {
			case err != nil && isKnown:
				res.diverged = append(res.diverged, fmt.Sprintf("%s/%s (%s)", group.Name, tc.Name, reason))
			case err != nil:
				res.failed = append(res.failed,
					fmt.Sprintf("%s/%s: %v\n      expression: %s", group.Name, tc.Name, err, strings.TrimSpace(tc.Expression.Text)))
			case isKnown:
				res.fixed = append(res.fixed, fmt.Sprintf("%s/%s", group.Name, tc.Name))
				res.passed++
			default:
				res.passed++
			}
		}
	}
	return res
}

// runOne evaluates a single test and reports whether it behaved as specified.
func runOne(cache *inputCache, tc suiteTest) error {
	expr := strings.TrimSpace(tc.Expression.Text)

	var root *resource.Node
	if tc.InputFile != "" {
		var err error
		root, err = cache.load(tc.InputFile)
		if err != nil {
			return fmt.Errorf("loading input %s: %w", tc.InputFile, err)
		}
	}

	parsed, parseErr := fhirpath.Parse(expr)
	var got fhirpath.Collection
	var evalErr error
	if parseErr == nil {
		ctx := resource.NewContext(cache.idx, root)
		var input fhirpath.Collection
		if root != nil {
			input = fhirpath.Collection{root}
		}
		got, evalErr = fhirpath.Eval(parsed, input, ctx)
	}

	// A test marked invalid must fail somewhere: parsing or evaluation.
	if tc.Expression.Invalid != "" {
		if parseErr == nil && evalErr == nil {
			return fmt.Errorf("expected a %s error, but the expression evaluated to %s",
				tc.Expression.Invalid, renderCollection(got))
		}
		return nil
	}
	if parseErr != nil {
		return fmt.Errorf("parse error: %w", parseErr)
	}
	if evalErr != nil {
		return fmt.Errorf("evaluation error: %w", evalErr)
	}

	// A predicate test asks only whether anything was selected.
	if tc.Predicate {
		want := len(tc.Outputs) == 1 && tc.Outputs[0].Text == "true"
		if got.Empty() == want {
			return fmt.Errorf("predicate: got %s, want %v", renderCollection(got), want)
		}
		return nil
	}

	if len(got) != len(tc.Outputs) {
		return fmt.Errorf("got %d results %s, want %d %s",
			len(got), renderCollection(got), len(tc.Outputs), renderExpected(tc))
	}
	for i, want := range tc.Outputs {
		if !valueMatches(got[i], want.Type, strings.TrimSpace(want.Text)) {
			return fmt.Errorf("result %d: got %q (%s), want %q (%s)",
				i, got[i].String(), got[i].TypeName(), strings.TrimSpace(want.Text), want.Type)
		}
	}
	return nil
}

// valueMatches compares one result against the suite's expected output. The
// suite writes expectations as text, so comparison is on the rendered form,
// with the declared type checked loosely: it names the FHIR or System type, and
// the engine may legitimately return either for a primitive.
func valueMatches(got fhirpath.Value, wantType, wantText string) bool {
	// The suite writes temporal expectations in FHIRPath literal notation
	// ("@1974-12-25"), which is how you would write the value in an expression
	// rather than what the value renders as. toString() of a date has no "@".
	if strings.HasPrefix(wantText, "@") {
		wantText = wantText[1:]
	}
	if got.String() != wantText {
		return false
	}
	if wantType == "" {
		return true
	}
	gotType := strings.TrimPrefix(strings.TrimPrefix(got.TypeName(), "System."), "FHIR.")
	return strings.EqualFold(gotType, wantType) || compatibleType(gotType, wantType)
}

// compatibleType allows the System type an engine returns to satisfy an
// expectation written as the FHIR type, and vice versa.
func compatibleType(got, want string) bool {
	equivalents := map[string][]string{
		"String": {"string", "code", "id", "uri", "url", "markdown", "oid", "uuid", "canonical", "base64Binary"},
		// Element.id is typed http://hl7.org/fhirpath/System.String in the
		// published definitions; the generator normalizes that to "string",
		// while the suite still calls the result an "id".
		"string":   {"id", "code", "uri", "url", "markdown", "oid", "uuid", "canonical"},
		"Integer":  {"integer", "positiveInt", "unsignedInt", "integer64"},
		"Decimal":  {"decimal"},
		"Boolean":  {"boolean"},
		"Date":     {"date"},
		"DateTime": {"dateTime", "instant"},
		"Time":     {"time"},
		"Quantity": {"Quantity"},
	}
	for _, alias := range equivalents[got] {
		if strings.EqualFold(alias, want) {
			return true
		}
	}
	// The expectation may name the System type where a FHIR node was returned.
	for system, aliases := range equivalents {
		if !strings.EqualFold(system, want) {
			continue
		}
		for _, alias := range aliases {
			if strings.EqualFold(alias, got) {
				return true
			}
		}
	}
	return false
}

func renderCollection(c fhirpath.Collection) string {
	if c.Empty() {
		return "{}"
	}
	parts := make([]string, len(c))
	for i, v := range c {
		parts[i] = fmt.Sprintf("%q", v.String())
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func renderExpected(tc suiteTest) string {
	parts := make([]string, len(tc.Outputs))
	for i, o := range tc.Outputs {
		parts[i] = fmt.Sprintf("%q", strings.TrimSpace(o.Text))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func TestOfficialFHIRPathSuites(t *testing.T) {
	for _, release := range conformance.Releases() {
		t.Run(string(release), func(t *testing.T) {
			res := runSuite(t, release)
			total := res.passed + len(res.failed) + len(res.diverged)
			for _, n := range res.skipped {
				total += n
			}

			for _, f := range res.fixed {
				t.Errorf("%s is listed as a known divergence but now passes; remove it from knownDivergences", f)
			}

			if len(res.failed) > 0 {
				sort.Strings(res.failed)
				// Truncated by default so a broad regression does not bury the
				// summary; GOGOFHIR_SUITE_ALL=1 prints every failure, which is
				// what you want while working through them.
				shown := res.failed
				if os.Getenv("GOGOFHIR_SUITE_ALL") == "" && len(shown) > 40 {
					shown = shown[:40]
				}
				for _, f := range shown {
					t.Errorf("%s", f)
				}
				if len(res.failed) > len(shown) {
					t.Errorf("... and %d more failures", len(res.failed)-len(shown))
				}
			}
			var skipNotes []string
			for reason, n := range res.skipped {
				skipNotes = append(skipNotes, fmt.Sprintf("%d skipped (%s)", n, reason))
			}
			sort.Strings(skipNotes)
			t.Logf("%s: %d/%d passed, %d failed, %d known divergences%s",
				release, res.passed, total, len(res.failed), len(res.diverged),
				func() string {
					if len(skipNotes) == 0 {
						return ""
					}
					return "; " + strings.Join(skipNotes, "; ")
				}())
		})
	}
}
