package fhirpath_test

import (
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/fhirpath"
)

// Round-tripping through Expr.String is how these tests assert structure:
// Binary and TypeOp parenthesize themselves, so the rendered form shows exactly
// how the parser grouped the input.
func TestParsePrecedence(t *testing.T) {
	tests := []struct {
		src  string
		want string
	}{
		// The precedence ladder, checked at each rung.
		{"a implies b or c", "(a implies (b or c))"},
		{"a or b and c", "(a or (b and c))"},
		{"a and b = c", "(a and (b = c))"},
		{"a = b < c", "(a = (b < c))"},
		{"a = b | c", "(a = (b | c))"},
		{"a | b is Quantity", "(a | (b is Quantity))"},
		{"a is Quantity | b", "((a is Quantity) | b)"},
		{"1 + 2 * 3", "(1 + (2 * 3))"},
		{"1 * 2 + 3", "((1 * 2) + 3)"},
		{"1 + 2 - 3", "((1 + 2) - 3)"},
		{"8 div 3 mod 2", "((8 div 3) mod 2)"},
		{"a in b", "(a in b)"},
		{"a contains b", "(a contains b)"},
		{"'x' & 'y'", "('x' & 'y')"},

		// Left associativity throughout.
		{"a or b or c", "((a or b) or c)"},
		{"a - b - c", "((a - b) - c)"},

		// Path navigation binds tighter than any operator.
		{"a.b | c.d", "(a.b | c.d)"},
		{"a.b.c", "a.b.c"},
		{"-a.b", "-a.b"},

		// Unary polarity.
		{"-1", "-1"},
		{"- - 1", "--1"},
		{"1 + -2", "(1 + -2)"},
	}
	for _, tt := range tests {
		e, err := fhirpath.Parse(tt.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.src, err)
			continue
		}
		if got := e.String(); got != tt.want {
			t.Errorf("Parse(%q)\n  got  %s\n  want %s", tt.src, got, tt.want)
		}
	}
}

func TestParseInvocations(t *testing.T) {
	tests := []struct{ src, want string }{
		{"name.where(use = 'official').family", "name.where((use = 'official')).family"},
		{"name.first()", "name.first()"},
		{"Patient.name.given[0]", "Patient.name.given[0]"},
		{"telecom.where(system = 'phone')[1]", "telecom.where((system = 'phone'))[1]"},
		{"value.ofType(Quantity)", "value.ofType(Quantity)"},
		{"iif(a, b, c)", "iif(a, b, c)"},
		{"$this.value", "$this.value"},
		{"$index", "$index"},
		{"%resource.id", "%resource.id"},
		{"%`vs-name`", "%`vs-name`"},
		{"extension('http://x').value", "extension('http://x').value"},
	}
	for _, tt := range tests {
		e, err := fhirpath.Parse(tt.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.src, err)
			continue
		}
		if got := e.String(); got != tt.want {
			t.Errorf("Parse(%q)\n  got  %s\n  want %s", tt.src, got, tt.want)
		}
	}
}

// FHIRPath has no reserved words: every operator name is also a legal property
// name, and the parser must decide by position. These are the cases that break
// a lexer which treats them as keywords.
func TestKeywordsAreUsableAsNames(t *testing.T) {
	tests := []struct{ src, want string }{
		{"Patient.contains", "Patient.contains"},
		{"Group.characteristic.exclude", "Group.characteristic.exclude"},
		{"a.div", "a.div"},
		{"a.as", "a.as"},
		{"a.is", "a.is"},
		{"a.`div`", "a.`div`"},
		{"a.in", "a.in"},
		// A word operator still works when it is genuinely an operator.
		{"a div b", "(a div b)"},
	}
	for _, tt := range tests {
		e, err := fhirpath.Parse(tt.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.src, err)
			continue
		}
		if got := e.String(); got != tt.want {
			t.Errorf("Parse(%q)\n  got  %s\n  want %s", tt.src, got, tt.want)
		}
	}
}

// A type specifier must stop at the type name; the "is"/"as" operators are
// routinely followed by a path applied to the result.
func TestTypeOperators(t *testing.T) {
	tests := []struct{ src, want string }{
		{"value is Quantity", "(value is Quantity)"},
		{"value as Quantity", "(value as Quantity)"},
		{"value is FHIR.Quantity", "(value is FHIR.Quantity)"},
		{"(value as Quantity).value", "(value as Quantity).value"},
		{"subject.where(resolve() is Patient)", "subject.where((resolve() is Patient))"},
		{"value.as(Quantity)", "value.as(Quantity)"},
		{"value.is(Quantity)", "value.is(Quantity)"},
	}
	for _, tt := range tests {
		e, err := fhirpath.Parse(tt.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.src, err)
			continue
		}
		if got := e.String(); got != tt.want {
			t.Errorf("Parse(%q)\n  got  %s\n  want %s", tt.src, got, tt.want)
		}
	}
}

func TestLiterals(t *testing.T) {
	tests := []struct{ src, want string }{
		{"{}", "{}"},
		{"true", "true"},
		{"false", "false"},
		{"'hello'", "'hello'"},
		{`'it\'s'`, `'it\'s'`},
		{"1", "1"},
		{"1.5", "1.5"},
		{"3L", "3L"},
		{"@2014-01-25", "@2014-01-25"},
		{"@2014-01-25T14:30:14.559Z", "@2014-01-25T14:30:14.559Z"},
		{"@T12:00", "@T12:00"},
		{"4 days", "4 days"},
		{"4 'mg'", "4 'mg'"},
		// A following word that is not a calendar unit must not be swallowed as
		// a unit -- this would silently change the meaning of a comparison.
		{"1 and true", "(1 and true)"},
	}
	for _, tt := range tests {
		e, err := fhirpath.Parse(tt.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.src, err)
			continue
		}
		if got := e.String(); got != tt.want {
			t.Errorf("Parse(%q)\n  got  %s\n  want %s", tt.src, got, tt.want)
		}
	}
}

// "1.is(Integer)" is the classic lexer trap: consuming "1." as a malformed
// decimal loses the invocation.
func TestNumberFollowedByDot(t *testing.T) {
	e, err := fhirpath.Parse("1.is(Integer)")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := e.String(), "1.is(Integer)"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestComments(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a // trailing\n.b", "a.b"},
		{"a /* inline */ .b", "a.b"},
		{"/* leading */ a", "a"},
		// "//" must be recognized as a comment before "/" is read as division.
		{"a.b // c / d", "a.b"},
	}
	for _, tt := range tests {
		e, err := fhirpath.Parse(tt.src)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.src, err)
			continue
		}
		if got := e.String(); got != tt.want {
			t.Errorf("Parse(%q)\n  got  %s\n  want %s", tt.src, got, tt.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		src     string
		wantMsg string
	}{
		{"", "expected an expression"},
		{"a +", "expected an expression"},
		{"(a", "unclosed '('"},
		{"a[1", "unclosed '['"},
		{"a.where(", "expected an expression"},
		{"a.", "expected a name after '.'"},
		{"'unterminated", "unterminated string literal"},
		{"`unterminated", "unterminated delimited identifier"},
		{"a b", "unexpected"},
		{"@", "'@' must be followed by"},
		{"$", "'$' must be followed by"},
		{"a # b", "unexpected character"},
		{"{", "empty collection"},
	}
	for _, tt := range tests {
		_, err := fhirpath.Parse(tt.src)
		if err == nil {
			t.Errorf("Parse(%q): expected an error", tt.src)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantMsg) {
			t.Errorf("Parse(%q) error = %q, want it to mention %q", tt.src, err, tt.wantMsg)
		}
	}
}

// Errors must carry a position; "something went wrong" in a 300-character
// search parameter expression is not actionable.
// The grammar allows single quotes only, but R5's ElementDefinition eld-11
// invariant is written with double quotes and every reference implementation
// accepts it. We match them, deliberately -- see the note in the lexer. This
// test exists so the leniency cannot be removed by accident, and so it is
// visible as a decision rather than an oversight.
func TestDoubleQuotedStringIsAcceptedForEld11(t *testing.T) {
	e, err := fhirpath.Parse(`type.code.contains(":")`)
	if err != nil {
		t.Fatalf("published invariant eld-11 must parse: %v", err)
	}
	// It is treated as an ordinary string literal, so it renders in the
	// canonical single-quoted form.
	if got, want := e.String(), `type.code.contains(':')`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestErrorCarriesPosition(t *testing.T) {
	_, err := fhirpath.Parse("name.where(use = )")
	if err == nil {
		t.Fatal("expected an error")
	}
	perr, ok := err.(*fhirpath.Error)
	if !ok {
		t.Fatalf("error type = %T, want *fhirpath.Error", err)
	}
	if perr.Pos <= 0 {
		t.Errorf("Pos = %d, want a position inside the expression", perr.Pos)
	}
}

// Reparsing a rendered expression must produce the same rendering. This is a
// cheap structural check: any grouping the printer gets wrong shows up as a
// mismatch on the second pass.
func TestRoundTripIsStable(t *testing.T) {
	for _, src := range []string{
		"Patient.name.where(use = 'official').given",
		"(a or b) and c",
		"value.ofType(Quantity) | value.ofType(Range)",
		"a implies b and c or d",
		"telecom.where(system = 'email').value[0]",
	} {
		first, err := fhirpath.Parse(src)
		if err != nil {
			t.Errorf("Parse(%q): %v", src, err)
			continue
		}
		second, err := fhirpath.Parse(first.String())
		if err != nil {
			t.Errorf("reparse of %q: %v", first.String(), err)
			continue
		}
		if first.String() != second.String() {
			t.Errorf("unstable round trip:\n  1st %s\n  2nd %s", first, second)
		}
	}
}
