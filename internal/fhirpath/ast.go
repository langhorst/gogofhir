package fhirpath

import (
	"strconv"
	"strings"
)

// Expr is a node in a parsed FHIRPath expression.
//
// The AST stays close to the specification's grammar rather than being
// normalized: an evaluator needs to distinguish "a.b" (member access) from
// "a.b()" (function call) and "a is X" (type test) from "a.is(X)", and folding
// them together early loses information the evaluator has to recover.
type Expr interface {
	// String renders the node back to FHIRPath source. Round-tripping is how
	// the parser tests assert structure without exposing the node types, and it
	// makes parse failures legible in test output.
	String() string
	exprNode()
}

// Invocation is a member access or function call applied to a subject:
// "Patient.name" and "name.where(use = 'official')". Subject is nil at the
// start of an expression, where the invocation applies to the context.
type Invocation struct {
	Subject Expr
	Name    string
	// Args is non-nil for a function call, nil for a plain member access. An
	// empty-but-non-nil slice is a call with no arguments, "name.first()".
	Args []Expr
	// Delimited records that the name arrived backtick-quoted, so rendering
	// round-trips and a property named "div" survives.
	Delimited bool
}

func (e *Invocation) exprNode() {}
func (e *Invocation) String() string {
	var b strings.Builder
	if e.Subject != nil {
		b.WriteString(e.Subject.String())
		b.WriteByte('.')
	}
	if e.Delimited {
		b.WriteByte('`')
		b.WriteString(e.Name)
		b.WriteByte('`')
	} else {
		b.WriteString(e.Name)
	}
	if e.Args != nil {
		b.WriteByte('(')
		for i, a := range e.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.String())
		}
		b.WriteByte(')')
	}
	return b.String()
}

// IsFunction reports whether this invocation is a call rather than a member
// access.
func (e *Invocation) IsFunction() bool { return e.Args != nil }

// Indexer is "expr[index]".
type Indexer struct {
	Subject Expr
	Index   Expr
}

func (e *Indexer) exprNode() {}
func (e *Indexer) String() string {
	return e.Subject.String() + "[" + e.Index.String() + "]"
}

// Unary is a polarity expression, "-value".
type Unary struct {
	Op      string // "+" or "-"
	Operand Expr
}

func (e *Unary) exprNode()      {}
func (e *Unary) String() string { return e.Op + e.Operand.String() }

// Binary is any infix operator. Op holds the operator's source spelling, so
// word operators ("div", "implies") and symbolic ones share a node.
type Binary struct {
	Op          string
	Left, Right Expr
}

func (e *Binary) exprNode() {}
func (e *Binary) String() string {
	return "(" + e.Left.String() + " " + e.Op + " " + e.Right.String() + ")"
}

// TypeOp is "expr is Type" or "expr as Type". These are separate from Binary
// because the right side is a type name, not an expression.
type TypeOp struct {
	Op      string // "is" or "as"
	Operand Expr
	// Type is a possibly-qualified type name, "Quantity" or "FHIR.Quantity".
	Type string
}

func (e *TypeOp) exprNode()      {}
func (e *TypeOp) String() string { return "(" + e.Operand.String() + " " + e.Op + " " + e.Type + ")" }

// Variable is "$this", "$index", or "$total".
type Variable struct{ Name string }

func (e *Variable) exprNode()      {}
func (e *Variable) String() string { return "$" + e.Name }

// ExternalConstant is "%name" or "%`delimited name`".
type ExternalConstant struct {
	Name      string
	Delimited bool
}

func (e *ExternalConstant) exprNode() {}
func (e *ExternalConstant) String() string {
	if e.Delimited {
		return "%`" + e.Name + "`"
	}
	return "%" + e.Name
}

// LiteralKind classifies a Literal.
type LiteralKind int

const (
	LitEmpty LiteralKind = iota // {}
	LitBoolean
	LitString
	LitNumber
	LitLong
	LitDateTime // @-prefixed date, time, or dateTime
	LitQuantity
)

// Literal is a constant value. Text holds the source spelling; typed accessors
// convert on demand rather than eagerly, so a malformed literal is reported by
// the evaluator with context rather than by the parser out of context.
type Literal struct {
	Kind LiteralKind
	Text string
	// Unit is set for a quantity: either a UCUM code in quotes or a calendar
	// duration word ("year", "days").
	Unit string
	// UnitQuoted distinguishes 4 'mg' from 4 days.
	UnitQuoted bool
}

func (e *Literal) exprNode() {}
func (e *Literal) String() string {
	switch e.Kind {
	case LitEmpty:
		return "{}"
	case LitString:
		return "'" + escapeString(e.Text) + "'"
	case LitDateTime:
		return "@" + e.Text
	case LitLong:
		return e.Text + "L"
	case LitQuantity:
		if e.UnitQuoted {
			return e.Text + " '" + escapeString(e.Unit) + "'"
		}
		return e.Text + " " + e.Unit
	default:
		return e.Text
	}
}

// Bool reports a boolean literal's value.
func (e *Literal) Bool() bool { return e.Text == "true" }

// Int converts a number or long literal to an integer.
func (e *Literal) Int() (int64, error) { return strconv.ParseInt(e.Text, 10, 64) }

// Float converts a number literal to a float.
func (e *Literal) Float() (float64, error) { return strconv.ParseFloat(e.Text, 64) }

// IsInteger reports whether a number literal has no fractional part, which
// FHIRPath treats as a different type from a decimal.
func (e *Literal) IsInteger() bool {
	return e.Kind == LitLong || (e.Kind == LitNumber && !strings.Contains(e.Text, "."))
}

func escapeString(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\f", `\f`,
	)
	return r.Replace(s)
}
