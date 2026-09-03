// Package fhirpath implements FHIRPath 2.0.0, the expression language the FHIR
// specification uses for search parameter extraction, invariants, _filter, and
// subscription criteria.
//
// It is the keystone of gogofhir: because resources are stored as untyped JSON
// documents rather than generated structs, essentially every question the
// server asks about a resource's contents is asked in FHIRPath. The package is
// deliberately free of dependencies on the rest of the server so it can be
// developed and proven on its own, against the official test suite HL7
// publishes.
//
// Specification: https://hl7.org/fhirpath/N1/ (2.0.0).
package fhirpath

import "fmt"

// Kind classifies a token.
//
// Note what is absent: there are no keyword kinds. Words like "div", "and", and
// "contains" are operators in one position and ordinary property names in
// another — "Patient.contains" is a valid path — so the lexer emits every bare
// word as Ident and the parser decides by position. This is the simplest
// correct answer to FHIRPath's context-dependent keywords, and it is why the
// parser matches operators on identifier *text* rather than on token kind.
type Kind int

const (
	EOF Kind = iota
	Ident
	// DelimitedIdent is a backtick-quoted identifier: `div` names a property
	// that would otherwise read as an operator.
	DelimitedIdent
	String
	Number
	// LongNumber is an integer with an L suffix (FHIRPath 2.1).
	LongNumber
	// DateTime covers the @-prefixed date, time, and dateTime literals. The
	// lexer captures their extent; validation of the value happens later.
	DateTime

	Dot
	Comma
	LParen
	RParen
	LBracket
	RBracket
	// EmptyCollection is the "{}" literal.
	EmptyCollection

	Plus
	Minus
	Star
	Slash
	Amp   // string concatenation
	Pipe  // union
	Eq    // =
	Equiv // ~
	Neq   // !=
	NotEquiv
	Lt
	Lte
	Gt
	Gte
	// Percent introduces an external constant: %resource, %`vs-name`.
	Percent
	// Dollar introduces $this, $index, or $total; Text holds the name.
	Dollar
)

var kindNames = map[Kind]string{
	EOF: "end of input", Ident: "identifier", DelimitedIdent: "delimited identifier",
	String: "string", Number: "number", LongNumber: "long number", DateTime: "date/time literal",
	Dot: "'.'", Comma: "','", LParen: "'('", RParen: "')'", LBracket: "'['", RBracket: "']'",
	EmptyCollection: "'{}'", Plus: "'+'", Minus: "'-'", Star: "'*'", Slash: "'/'",
	Amp: "'&'", Pipe: "'|'", Eq: "'='", Equiv: "'~'", Neq: "'!='", NotEquiv: "'!~'",
	Lt: "'<'", Lte: "'<='", Gt: "'>'", Gte: "'>='", Percent: "'%'", Dollar: "'$'",
}

func (k Kind) String() string {
	if n, ok := kindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("token(%d)", int(k))
}

// Token is one lexical unit.
type Token struct {
	Kind Kind
	// Text is the token's semantic value: escapes already decoded for strings
	// and delimited identifiers, the bare name for $-variables, the digits for
	// numbers.
	Text string
	// Pos is the byte offset of the token's first character, used to point at
	// the offending place in an error.
	Pos int
}

func (t Token) String() string {
	switch t.Kind {
	case Ident, String, Number, LongNumber, DateTime, DelimitedIdent, Dollar:
		return fmt.Sprintf("%s %q", t.Kind, t.Text)
	default:
		return t.Kind.String()
	}
}
