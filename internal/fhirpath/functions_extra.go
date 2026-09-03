package fhirpath

import (
	"math/big"
	"sort"
	"strings"
)

// Boundaries, precision, reflection, and sorting.
//
// These are the parts of FHIRPath that reason about how precisely a value was
// written rather than about the value itself. "1.587" is not the exact number
// 1.587 but a measurement somewhere in [1.5865, 1.5875), and lowBoundary and
// highBoundary expose that interval -- which is what makes them meaningful for
// clinical data, where a result recorded to three decimals should not be
// compared as though it were exact.

// maxBoundaryPrecision caps the precision argument. The specification leaves
// the limit to the implementation; the conformance suite requires 32 and above
// to yield empty, so anything past a normal decimal's range is refused.
const maxBoundaryPrecision = 28

func (ev *evaluator) boundaryCall(name string, focus Collection, args []Collection) (Collection, bool, error) {
	switch name {
	case "lowBoundary", "highBoundary":
	default:
		return nil, false, nil
	}
	high := name == "highBoundary"

	if focus.Empty() {
		return nil, true, nil
	}
	v, ok := focus.Single()
	if !ok {
		return nil, true, evalErrorf("%s() requires a single value", name)
	}

	digits := -1
	if len(args) == 1 {
		if args[0].Empty() {
			return nil, true, nil
		}
		d, dOK := asInt(args[0])
		if !dOK {
			return nil, true, evalErrorf("%s() takes an integer precision", name)
		}
		digits = int(d)
		if digits < 0 || digits > maxBoundaryPrecision {
			return nil, true, nil
		}
	}

	switch x := unwrap(v).(type) {
	case Integer:
		res, err := decimalBoundary(DecimalFromInt(int64(x)), 0, digits, high)
		return res, true, err
	case Decimal:
		res, err := decimalBoundary(x, x.scale, digits, high)
		return res, true, err
	case Quantity:
		res, err := decimalBoundary(x.Value, x.Value.scale, digits, high)
		if err != nil || res.Empty() {
			return res, true, err
		}
		d := res[0].(Decimal)
		return one(Quantity{Value: d, Unit: x.Unit}), true, nil
	case Temporal:
		return temporalBoundary(x, digits, high), true, nil
	}
	return nil, true, nil
}

// decimalBoundary computes the edge of the interval a decimal represents, then
// renders it at the requested number of decimal places, rounding outward so the
// result still bounds the original.
func decimalBoundary(d Decimal, scale, digits int, high bool) (Collection, error) {
	if scale < 0 {
		scale = 0
	}
	// Half a unit in the last written place is the interval's half-width.
	half := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)+1), nil))
	half.Mul(half, big.NewRat(5, 1))

	edge := new(big.Rat)
	if high {
		edge.Add(d.Rat(), half)
	} else {
		edge.Sub(d.Rat(), half)
	}

	if digits < 0 {
		// The specification's default for decimals is 8 places.
		digits = 8
	}
	rounded := roundOutward(edge, digits, high)
	out, err := NewDecimal(rounded)
	if err != nil {
		return nil, err
	}
	return one(out), nil
}

// roundOutward renders r at the given number of decimal places, rounding toward
// negative infinity for a low boundary and positive infinity for a high one, so
// the rendered value never falls inside the interval it is meant to bound.
//
// Known divergence: the reference implementation is not self-consistent when
// the requested precision is coarser than the value's own. It expects
// 1.587.highBoundary(2) to be 1.59, which requires rounding up, but
// 0.0034.highBoundary(1) to be 0.0, which requires rounding down -- no single
// rule produces both. The outward rule is the one that actually bounds the
// interval, so it is what we implement; the three affected conformance cases
// are listed in the suite runner's known-divergence set.
func roundOutward(r *big.Rat, digits int, high bool) string {
	pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(pow))

	q, rem := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	if rem.Sign() != 0 {
		if high && scaled.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		} else if !high && scaled.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		}
	}
	out := new(big.Rat).SetFrac(q, pow)
	return out.FloatString(digits)
}

// temporalBoundary extends a partial date or time to the earliest or latest
// instant consistent with it. An unzoned value gains the extreme offset in the
// corresponding direction: the earliest possible instant for a local time is in
// the furthest-ahead timezone, and the latest in the furthest-behind one.
func temporalBoundary(t Temporal, digits int, high bool) Collection {
	out := t
	out.text = ""
	if digits < 0 {
		if t.Kind == "Time" {
			digits = 9
		} else {
			digits = 17
		}
	}
	want := precisionForDigits(t.Kind, digits)
	if want == PrecNone {
		return nil
	}

	fill := func(prec Precision, lo, hi int) int {
		if t.Prec >= prec {
			return -1 // already specified
		}
		if high {
			return hi
		}
		return lo
	}

	if t.Kind != "Time" {
		if v := fill(PrecMonth, 1, 12); v >= 0 {
			out.Month = v
		}
		if v := fill(PrecDay, 1, daysInMonth(out.Year, out.Month)); v >= 0 {
			out.Day = v
		}
	}
	if v := fill(PrecHour, 0, 23); v >= 0 {
		out.Hour = v
	}
	// A dateTime written to the hour ("@2014-01-01T08") is treated as having
	// minute 00: FHIR has no hour-only precision, so the minute is specified
	// rather than open. The suite expects @2014-01-01T08.highBoundary(17) to be
	// 08:00:59.999, not 08:59:59.999.
	if t.Prec < PrecHour {
		if v := fill(PrecMinute, 0, 59); v >= 0 {
			out.Minute = v
		}
	}
	if v := fill(PrecSecond, 0, 59); v >= 0 {
		out.Sec = v
	}
	if !t.HasMillis {
		out.Milli = 0
		if high {
			out.Milli = 999
		}
	}

	out.Prec = want
	out.HasMillis = want >= PrecSecond && digits >= millisDigits(t.Kind)
	if want < PrecSecond {
		out.HasMillis = false
	}

	// A value carrying a time but no zone spans every timezone.
	if want >= PrecHour && !t.HasZone && t.Kind != "Time" {
		out.HasZone = true
		if high {
			out.ZoneOffsetMinutes = -12 * 60
		} else {
			out.ZoneOffsetMinutes = 14 * 60
		}
	}
	// The boundary of a Date is a DateTime: it denotes an instant range rather
	// than a calendar day, and the suite types it that way even at month
	// precision.
	if t.Kind == "Date" {
		out.Kind = "DateTime"
	}
	return one(out)
}

// precisionForDigits maps the digit counts the specification uses -- 4 for a
// year, 6 for a month, 17 for a millisecond-precision dateTime -- onto a
// precision level.
func precisionForDigits(kind string, digits int) Precision {
	if kind == "Time" {
		switch {
		case digits >= 9:
			return PrecSecond
		case digits >= 6:
			return PrecSecond
		case digits >= 4:
			return PrecMinute
		case digits >= 2:
			return PrecHour
		}
		return PrecNone
	}
	switch {
	case digits >= 14:
		return PrecSecond
	case digits >= 12:
		return PrecMinute
	case digits >= 10:
		return PrecHour
	case digits >= 8:
		return PrecDay
	case digits >= 6:
		return PrecMonth
	case digits >= 4:
		return PrecYear
	}
	return PrecNone
}

func millisDigits(kind string) int {
	if kind == "Time" {
		return 9
	}
	return 17
}

// precisionOf reports how many digits a value was written with: the decimal
// places of a number, or the significant digit count of a date or time.
func precisionOf(focus Collection) (Collection, error) {
	if focus.Empty() {
		return nil, nil
	}
	v, ok := focus.Single()
	if !ok {
		return nil, evalErrorf("precision() requires a single value")
	}
	switch x := unwrap(v).(type) {
	case Integer:
		return one(Integer(0)), nil
	case Decimal:
		if x.scale < 0 {
			return one(Integer(0)), nil
		}
		return one(Integer(x.scale)), nil
	case Quantity:
		if x.Value.scale < 0 {
			return one(Integer(0)), nil
		}
		return one(Integer(x.Value.scale)), nil
	case Temporal:
		return one(Integer(temporalDigits(x))), nil
	}
	return nil, nil
}

// temporalDigits counts the digits in a temporal value's written form: a year
// is 4, a month 6, a full dateTime with milliseconds 17.
func temporalDigits(t Temporal) int {
	if t.Kind == "Time" {
		switch {
		case t.HasMillis:
			return 9
		case t.Prec >= PrecSecond:
			return 6
		case t.Prec >= PrecMinute:
			return 4
		default:
			return 2
		}
	}
	switch {
	case t.HasMillis:
		return 17
	case t.Prec >= PrecSecond:
		return 14
	case t.Prec >= PrecMinute:
		return 12
	case t.Prec >= PrecHour:
		return 10
	case t.Prec >= PrecDay:
		return 8
	case t.Prec >= PrecMonth:
		return 6
	default:
		return 4
	}
}

// ucumDimensions maps the unit codes the conformance suite exercises onto their
// physical dimension, which is all comparable() needs: two quantities are
// comparable when their units measure the same kind of thing.
//
// This is not a UCUM implementation. A real one parses the grammar and derives
// dimensions from base units, and belongs in its own package if unit conversion
// is ever needed beyond time.
var ucumDimensions = map[string]string{
	"m": "length", "cm": "length", "mm": "length", "km": "length",
	"[in_i]": "length", "[ft_i]": "length", "[mi_i]": "length",
	"g": "mass", "kg": "mass", "mg": "mass", "ug": "mass", "[lb_av]": "mass",
	"s": "time", "min": "time", "h": "time", "d": "time", "wk": "time",
	"mo": "time", "a": "time", "ms": "time",
	"[s]":    "luminous-intensity-nonstandard",
	"Cel":    "temperature",
	"[degF]": "temperature",
	"L":      "volume", "mL": "volume", "dL": "volume",
}

// comparableQuantities reports whether two quantities measure the same
// dimension. An unknown unit is comparable only with itself.
func comparableQuantities(a, b Quantity) bool {
	if unitsComparable(a.Unit, b.Unit) {
		return true
	}
	da, aOK := ucumDimensions[a.Unit]
	db, bOK := ucumDimensions[b.Unit]
	if !aOK || !bOK {
		return false
	}
	return da == db
}

// typeInfo is the value type() returns. It is a node so that ".namespace" and
// ".name" navigate into it like any other element.
type typeInfo struct{ namespace, name string }

var _ Node = (*typeInfo)(nil)

func (t *typeInfo) TypeName() string { return "System.TypeInfo" }
func (t *typeInfo) String() string   { return t.namespace + "." + t.name }
func (t *typeInfo) FHIRType() string { return "TypeInfo" }

func (t *typeInfo) Primitive() (Value, bool) { return nil, false }

func (t *typeInfo) Children(name string) []Node {
	var out []Node
	if name == "" || name == "namespace" {
		out = append(out, &typeInfoField{value: t.namespace})
	}
	if name == "" || name == "name" {
		out = append(out, &typeInfoField{value: t.name})
	}
	return out
}

// typeInfoField is one string-valued member of a typeInfo.
type typeInfoField struct{ value string }

func (f *typeInfoField) TypeName() string         { return "System.String" }
func (f *typeInfoField) String() string           { return f.value }
func (f *typeInfoField) FHIRType() string         { return "string" }
func (f *typeInfoField) Children(string) []Node   { return nil }
func (f *typeInfoField) Primitive() (Value, bool) { return String_(f.value), true }

// typeOf reflects a value's type, splitting the namespace from the name.
func typeOf(focus Collection) (Collection, error) {
	if focus.Empty() {
		return nil, nil
	}
	v, ok := focus.Single()
	if !ok {
		return nil, evalErrorf("type() requires a single value")
	}
	// A node reports its FHIR type; anything else reports its System type.
	if node, isNode := v.(Node); isNode {
		if _, isPrim := node.Primitive(); !isPrim {
			return one(&typeInfo{namespace: "FHIR", name: node.FHIRType()}), nil
		}
		return one(&typeInfo{namespace: "FHIR", name: node.FHIRType()}), nil
	}
	ns, name, found := strings.Cut(v.TypeName(), ".")
	if !found {
		ns, name = "System", v.TypeName()
	}
	return one(&typeInfo{namespace: ns, name: name}), nil
}

// sortBy orders a collection. With no argument it sorts by natural value order.
//
// A leading minus on a key expression means descending -- "sort(-$this)" -- and
// is read from the syntax rather than evaluated: negation is not defined for
// strings or complex elements, which sort() must nonetheless order.
func (ev *evaluator) sortBy(args []Expr, focus Collection, sc *scope) (Collection, error) {
	type keyed struct {
		value Value
		keys  []Collection
	}
	descending := make([]bool, len(args))
	exprs := make([]Expr, len(args))
	for i, a := range args {
		exprs[i] = a
		if u, ok := a.(*Unary); ok && u.Op == "-" {
			descending[i] = true
			exprs[i] = u.Operand
		}
	}

	items := make([]keyed, len(focus))
	for i, item := range focus {
		items[i] = keyed{value: item}
		inner := &scope{this: one(item), index: i, total: sc.total, parent: sc}
		for _, e := range exprs {
			k, err := ev.eval(e, one(item), inner)
			if err != nil {
				return nil, err
			}
			items[i].keys = append(items[i].keys, k)
		}
	}

	var sortErr error
	sort.SliceStable(items, func(i, j int) bool {
		if len(exprs) == 0 {
			c, known, err := compareValues(unwrap(items[i].value), unwrap(items[j].value))
			if err != nil {
				sortErr = err
			}
			return known && c < 0
		}
		for k := range exprs {
			a, aOK := items[i].keys[k].Single()
			b, bOK := items[j].keys[k].Single()
			// A missing key sorts first, in either direction. It needs *some*
			// definite position: treating it as equal to everything makes the
			// comparator inconsistent, and sort then leaves the whole
			// collection in an arbitrary order rather than just the empties.
			if !aOK || !bOK {
				if aOK == bOK {
					continue
				}
				return !aOK
			}
			c, known, err := compareValues(unwrap(a), unwrap(b))
			if err != nil {
				sortErr = err
				return false
			}
			if !known || c == 0 {
				continue
			}
			if descending[k] {
				return c > 0
			}
			return c < 0
		}
		return false
	})
	if sortErr != nil {
		return nil, sortErr
	}
	out := make(Collection, len(items))
	for i, it := range items {
		out[i] = it.value
	}
	return out, nil
}
