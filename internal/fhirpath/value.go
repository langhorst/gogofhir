package fhirpath

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Everything in FHIRPath is a collection. There are no scalars: "Patient.name"
// yields a collection, a literal yields a collection of one, and an expression
// that matches nothing yields an empty collection rather than null. Most of the
// language's surprising corners follow from that one rule -- notably that an
// operation on empty usually produces empty rather than an error -- so the
// evaluator works in Collections throughout and never unwraps implicitly.
type Collection []Value

// Value is a single item in a collection: either a System primitive defined by
// FHIRPath itself, or a Node from the resource being evaluated.
type Value interface {
	// TypeName is the namespaced type used by is, as, and ofType:
	// "System.String" for primitives, "FHIR.HumanName" for model elements.
	TypeName() string
	// String renders the value the way the conformance suite writes expected
	// output, which is also how toString() renders it.
	String() string
}

// Node is one element of the resource under evaluation.
//
// The evaluator reaches documents only through this interface, which keeps this
// package free of any dependency on the storage or conformance layers: it can
// be built and tested against the official suite on its own. internal/resource
// supplies the implementation, consulting the conformance index for the type
// information the interface exposes.
type Node interface {
	Value

	// FHIRType is the unqualified FHIR type name of this node: "Patient",
	// "HumanName", "date". TypeName returns the same thing namespaced.
	FHIRType() string
	// Children returns the child elements with the given name, in document
	// order. An empty name returns every child, which is what children() and
	// descendants() need.
	Children(name string) []Node
	// Primitive returns this node's value when it is a FHIR primitive, already
	// converted to the corresponding System type. Complex elements report false.
	//
	// A FHIR primitive is both a value and a node -- Patient.birthDate is a Date
	// but Patient.birthDate.extension is also navigable -- so implementations
	// must answer both this and Children.
	Primitive() (Value, bool)
}

// PrimitiveTyped is implemented by nodes that know whether their element type
// is a FHIR primitive.
//
// It separates two cases that Primitive() reports identically: a primitive
// carrying only extensions (no value -- behaves as empty) and a complex element
// (a type error when a string or number was required). Without the distinction,
// "Appointment.identifier.contains('x')" would quietly answer empty instead of
// failing, which the conformance suite checks.
type PrimitiveTyped interface {
	IsPrimitiveType() bool
}

// valuelessPrimitive reports whether v is a primitive element with no value,
// which every operator treats as empty.
func valuelessPrimitive(v Value) bool {
	node, ok := v.(Node)
	if !ok {
		return false
	}
	if _, hasValue := node.Primitive(); hasValue {
		return false
	}
	pt, knows := node.(PrimitiveTyped)
	return knows && pt.IsPrimitiveType()
}

// ---- System primitives ----

// Boolean is System.Boolean.
type Boolean bool

func (b Boolean) TypeName() string { return "System.Boolean" }
func (b Boolean) String() string {
	if b {
		return "true"
	}
	return "false"
}

// String_ is System.String. The trailing underscore avoids colliding with the
// String() method that every Value must provide.
type String_ string

func (s String_) TypeName() string { return "System.String" }
func (s String_) String() string   { return string(s) }

// Integer is System.Integer.
type Integer int64

func (i Integer) TypeName() string { return "System.Integer" }
func (i Integer) String() string   { return strconv.FormatInt(int64(i), 10) }

// Decimal is System.Decimal.
//
// FHIR decimals are exact: a lab result of 1.10 differs from 1.1 in whether the
// trailing digit was measured, and the specification requires that precision
// survive a round trip. Backing this with float64 would corrupt values on the
// way in, so it holds an exact rational plus the scale it was written with.
type Decimal struct {
	rat *big.Rat
	// scale is the number of digits after the decimal point as written, used to
	// render the value back the way it arrived. Negative means "unspecified",
	// as for a computed result.
	scale int
}

// NewDecimal parses a decimal from its source spelling, preserving scale.
func NewDecimal(text string) (Decimal, error) {
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return Decimal{}, fmt.Errorf("invalid decimal %q", text)
	}
	scale := 0
	if i := strings.IndexByte(text, '.'); i >= 0 {
		scale = len(text) - i - 1
	}
	return Decimal{rat: r, scale: scale}, nil
}

// DecimalFromRat wraps an exact rational whose scale is not known, as produced
// by arithmetic.
func DecimalFromRat(r *big.Rat) Decimal { return Decimal{rat: r, scale: -1} }

// Neg returns the negation, keeping the scale. Losing it would make the value
// look as though it had been written with no decimal places, which changes what
// lowBoundary and highBoundary report.
func (d Decimal) Neg() Decimal {
	return Decimal{rat: new(big.Rat).Neg(d.Rat()), scale: d.scale}
}

// Scale reports the number of decimal places the value was written with, or -1
// when it was computed rather than written.
func (d Decimal) Scale() int { return d.scale }

// DecimalFromInt converts an integer, which arithmetic needs when mixing types.
func DecimalFromInt(i int64) Decimal {
	return Decimal{rat: new(big.Rat).SetInt64(i), scale: 0}
}

func (d Decimal) TypeName() string { return "System.Decimal" }

// Rat exposes the exact value for arithmetic and comparison.
func (d Decimal) Rat() *big.Rat {
	if d.rat == nil {
		return new(big.Rat)
	}
	return d.rat
}

// String renders at the value's own scale when it has one. Computed values are
// rendered exactly if they terminate, and otherwise at the precision the
// specification requires for division.
func (d Decimal) String() string {
	r := d.Rat()
	if d.scale >= 0 {
		return r.FloatString(d.scale)
	}
	if s, exact := exactDecimalString(r); exact {
		return s
	}
	// A non-terminating result (1/3) has no exact decimal form. The
	// specification leaves the precision open; 8 digits matches the reference
	// implementations and the conformance suite's expectations.
	return trimTrailingZeros(r.FloatString(8))
}

// IsInt reports whether the value is a whole number, which several functions
// and the integer/decimal conversions need.
func (d Decimal) IsInt() bool { return d.Rat().IsInt() }

// exactDecimalString renders r exactly when its denominator divides a power of
// ten, which is when a terminating decimal representation exists.
func exactDecimalString(r *big.Rat) (string, bool) {
	den := new(big.Int).Set(r.Denom())
	for _, p := range []int64{2, 5} {
		bp := big.NewInt(p)
		zero := new(big.Int)
		for {
			q, m := new(big.Int).QuoRem(den, bp, new(big.Int))
			if m.Cmp(zero) != 0 {
				break
			}
			den = q
		}
	}
	if den.Cmp(big.NewInt(1)) != 0 {
		return "", false
	}
	// Enough digits for any terminating value of this magnitude.
	digits := len(r.Denom().String()) + 2
	return trimTrailingZeros(r.FloatString(digits)), true
}

func trimTrailingZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Quantity is System.Quantity: a decimal with a unit. The unit is either a UCUM
// code or a calendar duration keyword, which do not compare interchangeably --
// a year is not a fixed number of days.
type Quantity struct {
	Value Decimal
	Unit  string
	// Calendar marks a unit written as a keyword ("1 week") rather than a UCUM
	// code ("1 'wk'"). The two are different units, not two spellings of one:
	// a calendar month is whatever the calendar says, while UCUM's "mo" is a
	// fixed span. They render differently and do not compare.
	Calendar bool
}

func (q Quantity) TypeName() string { return "System.Quantity" }

func (q Quantity) String() string {
	if q.Calendar {
		return q.Value.String() + " " + q.Unit
	}
	return q.Value.String() + " '" + q.Unit + "'"
}

// ---- helpers over collections ----

// Empty reports whether the collection has no items. FHIRPath's empty
// propagation makes this the most common test in the evaluator.
func (c Collection) Empty() bool { return len(c) == 0 }

// Single returns the sole item, reporting false unless the collection has
// exactly one. Many operators are defined only on singletons.
func (c Collection) Single() (Value, bool) {
	if len(c) != 1 {
		return nil, false
	}
	return c[0], true
}

// one wraps a value as a single-item collection.
func one(v Value) Collection { return Collection{v} }

// boolCollection wraps a Go bool as a FHIRPath boolean collection.
func boolCollection(b bool) Collection { return Collection{Boolean(b)} }

// unwrap converts a node that is a FHIR primitive into its System value,
// leaving everything else untouched. Operators work on System values, but
// navigation yields nodes, so nearly every operator begins here.
func unwrap(v Value) Value {
	n, ok := v.(Node)
	if !ok {
		return v
	}
	if p, isPrim := n.Primitive(); isPrim {
		return p
	}
	if q, isQuantity := fhirQuantity(n); isQuantity {
		return q
	}
	return v
}

// quantityTypes are the FHIR types that are Quantity or a profile of it. They
// all carry the same value/unit/code elements.
var quantityTypes = map[string]bool{
	"Quantity": true, "Age": true, "Count": true, "Distance": true,
	"Duration": true, "SimpleQuantity": true, "MoneyQuantity": true,
}

// fhirQuantity converts a FHIR Quantity element to a System Quantity so that
// operators can work on it. The unit is taken from "code" -- the coded UCUM
// unit -- falling back to the human-readable "unit" when no code is present.
//
// Without this, comparing Observation.value against a quantity literal is a
// type error rather than a comparison, which is one of the more common things
// an invariant does.
func fhirQuantity(n Node) (Quantity, bool) {
	if !quantityTypes[n.FHIRType()] {
		return Quantity{}, false
	}
	var value Decimal
	haveValue := false
	for _, child := range n.Children("value") {
		if p, ok := child.Primitive(); ok {
			switch x := p.(type) {
			case Decimal:
				value, haveValue = x, true
			case Integer:
				value, haveValue = DecimalFromInt(int64(x)), true
			}
		}
	}
	if !haveValue {
		return Quantity{}, false
	}
	unit := ""
	for _, name := range []string{"code", "unit"} {
		for _, child := range n.Children(name) {
			if p, ok := child.Primitive(); ok {
				if s, isStr := p.(String_); isStr && unit == "" {
					unit = string(s)
				}
			}
		}
		if unit != "" {
			break
		}
	}
	if unit == "" {
		unit = "1"
	}
	return Quantity{Value: value, Unit: unit}, true
}

// unwrapAll applies unwrap across a collection.
func unwrapAll(c Collection) Collection {
	out := make(Collection, len(c))
	for i, v := range c {
		out[i] = unwrap(v)
	}
	return out
}

// singletonBool implements the specification's singleton evaluation of
// collections as booleans, used by and/or/where and friends. An empty
// collection is neither true nor false, which is why ok exists.
func singletonBool(c Collection) (val, ok bool) {
	if len(c) != 1 {
		return false, false
	}
	switch v := unwrap(c[0]).(type) {
	case Boolean:
		return bool(v), true
	default:
		// A single non-boolean item is true when a boolean is expected, per the
		// specification's singleton conversion rules.
		return true, true
	}
}
