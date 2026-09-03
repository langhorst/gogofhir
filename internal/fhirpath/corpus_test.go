package fhirpath_test

import (
	"sort"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
)

// The compiled conformance indexes carry every FHIRPath expression the
// specification itself publishes: the expression behind each search parameter,
// each composite's component expressions, and every invariant. Together that is
// a few thousand real expressions written by the spec authors rather than by
// us, which makes them a far better parser corpus than anything hand-written.
//
// This runs before the evaluator exists and stays useful afterward: a grammar
// regression shows up here immediately, against real inputs.

// corpusEntry is one expression and where it came from, so a failure names the
// search parameter or invariant to look at.
type corpusEntry struct {
	origin string
	expr   string
}

func corpus(t *testing.T) []corpusEntry {
	t.Helper()
	var out []corpusEntry
	for _, release := range conformance.Releases() {
		idx, err := conformance.Load(release)
		if err != nil {
			t.Fatalf("loading %s: %v", release, err)
		}
		for base, params := range idx.SearchParams {
			for _, p := range params {
				if p.Expression != "" {
					out = append(out, corpusEntry{
						origin: string(release) + " SearchParameter " + base + "-" + p.Code,
						expr:   p.Expression,
					})
				}
				for i, c := range p.Components {
					if c.Expression != "" {
						out = append(out, corpusEntry{
							origin: string(release) + " SearchParameter " + base + "-" + p.Code + " component",
							expr:   c.Expression,
						})
						_ = i
					}
				}
			}
		}
		for name, typ := range idx.Types {
			for _, inv := range typ.Invariants {
				if inv.Expression != "" {
					out = append(out, corpusEntry{
						origin: string(release) + " " + name + " invariant " + inv.Key,
						expr:   inv.Expression,
					})
				}
			}
		}
	}
	// Map iteration makes the order random; sorting keeps failure output stable
	// between runs.
	sort.Slice(out, func(i, j int) bool {
		if out[i].origin != out[j].origin {
			return out[i].origin < out[j].origin
		}
		return out[i].expr < out[j].expr
	})
	return out
}

func TestParsesEveryPublishedExpression(t *testing.T) {
	entries := corpus(t)

	// Guard against a silently empty corpus: if the index ever stops carrying
	// expressions, this test must fail rather than pass on nothing.
	if len(entries) < 4000 {
		t.Fatalf("corpus has only %d expressions; expected several thousand", len(entries))
	}

	var failures int
	for _, e := range entries {
		if _, err := fhirpath.Parse(e.expr); err != nil {
			failures++
			// Cap the output: a grammar bug can fail hundreds of expressions,
			// and the first several are enough to diagnose it.
			if failures <= 10 {
				t.Errorf("%s\n  expression: %s\n  error: %v", e.origin, e.expr, err)
			}
		}
	}
	if failures > 10 {
		t.Errorf("... and %d more parse failures", failures-10)
	}
	t.Logf("parsed %d published expressions (%d distinct origins)", len(entries), len(entries))
}

// Every parsed expression must render back to something that parses again to
// the same thing. This catches printer bugs and, more importantly, grouping
// bugs: if the parser mis-associates an operator, the second parse of its own
// output usually disagrees.
func TestPublishedExpressionsRoundTrip(t *testing.T) {
	entries := corpus(t)

	var failures int
	for _, e := range entries {
		first, err := fhirpath.Parse(e.expr)
		if err != nil {
			continue // reported by TestParsesEveryPublishedExpression
		}
		rendered := first.String()
		second, err := fhirpath.Parse(rendered)
		if err != nil {
			failures++
			if failures <= 10 {
				t.Errorf("%s: rendering does not reparse\n  source:   %s\n  rendered: %s\n  error: %v",
					e.origin, e.expr, rendered, err)
			}
			continue
		}
		if second.String() != rendered {
			failures++
			if failures <= 10 {
				t.Errorf("%s: unstable round trip\n  source: %s\n  1st:    %s\n  2nd:    %s",
					e.origin, e.expr, rendered, second.String())
			}
		}
	}
	if failures > 10 {
		t.Errorf("... and %d more round-trip failures", failures-10)
	}
}

func BenchmarkParseSearchExpression(b *testing.B) {
	// A representative search parameter expression: navigation, a where clause,
	// and a type test.
	const src = "Observation.subject.where(resolve() is Patient)"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := fhirpath.Parse(src); err != nil {
			b.Fatal(err)
		}
	}
}
