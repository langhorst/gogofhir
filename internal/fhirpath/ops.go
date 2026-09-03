package fhirpath

import (
	"math/big"
	"strings"
	"unicode"
)

// Equality, comparison, and arithmetic.
//
// The recurring rule is that an operation involving empty yields empty, and an
// operation that cannot be decided also yields empty rather than false. Only a
// genuine type mismatch is an error. Conflating those three outcomes is the
// most common way an implementation fails the conformance suite.

// equality implements =, !=, ~, and !~.
func (ev *evaluator) equality(op string, left, right Collection) (Collection, error) {
	equiv := op == "~" || op == "!~"
	negate := op == "!=" || op == "!~"

	if !equiv {
		// Equality with an empty operand is unknown.
		if left.Empty() || right.Empty() {
			return nil, nil
		}
	} else if left.Empty() || right.Empty() {
		// Equivalence is total: two empties are equivalent, and empty is not
		// equivalent to anything else.
		return boolCollection(negate != (left.Empty() && right.Empty())), nil
	}

	if len(left) != len(right) {
		return boolCollection(negate != false), nil
	}

	if equiv {
		// Equivalence disregards order: two collections are equivalent when
		// each item on one side has a partner on the other.
		return boolCollection(negate != collectionsEquivalent(left, right)), nil
	}
	for i := range left {
		eq, known := valuesEqualKnown(left[i], right[i])
		if !known {
			return nil, nil
		}
		if !eq {
			return boolCollection(negate != false), nil
		}
	}
	return boolCollection(negate != true), nil
}

// collectionsEquivalent pairs off two equal-length collections, ignoring order.
// Each item may be used once, so duplicates are handled correctly.
func collectionsEquivalent(a, b Collection) bool {
	used := make([]bool, len(b))
	for _, x := range a {
		matched := false
		for j, y := range b {
			if used[j] || !valuesEquivalent(x, y) {
				continue
			}
			used[j] = true
			matched = true
			break
		}
		if !matched {
			return false
		}
	}
	return true
}

// valuesEqual is equality where an indeterminate result counts as unequal. It
// backs union deduplication and membership, which need a total answer.
func valuesEqual(a, b Value) bool {
	eq, known := valuesEqualKnown(a, b)
	return known && eq
}

// valuesEqualKnown reports equality and whether it could be determined. Partial
// temporal values are the case that can be unknown.
func valuesEqualKnown(a, b Value) (eq, known bool) {
	av, bv := unwrap(a), unwrap(b)

	// Complex elements compare structurally, by their children.
	an, aIsNode := av.(Node)
	bn, bIsNode := bv.(Node)
	if aIsNode || bIsNode {
		if !aIsNode || !bIsNode {
			return false, true
		}
		return nodesEqual(an, bn), true
	}

	switch x := av.(type) {
	case Boolean:
		y, ok := bv.(Boolean)
		return ok && x == y, true
	case String_:
		y, ok := bv.(String_)
		return ok && x == y, true
	case Integer:
		switch y := bv.(type) {
		case Integer:
			return x == y, true
		case Decimal:
			return DecimalFromInt(int64(x)).Rat().Cmp(y.Rat()) == 0, true
		}
		return false, true
	case Decimal:
		switch y := bv.(type) {
		case Integer:
			return x.Rat().Cmp(DecimalFromInt(int64(y)).Rat()) == 0, true
		case Decimal:
			return x.Rat().Cmp(y.Rat()) == 0, true
		}
		return false, true
	case Quantity:
		y, ok := bv.(Quantity)
		if !ok {
			return false, true
		}
		xr, yr, ok := alignQuantities(x, y)
		if !ok {
			// Units that do not convert make the question unanswerable rather
			// than false: "1 'mo' = 1 month" is empty.
			return false, false
		}
		return xr.Cmp(yr) == 0, true
	case Temporal:
		y, ok := bv.(Temporal)
		if !ok {
			return false, true
		}
		if (x.Kind == "Time") != (y.Kind == "Time") {
			return false, true
		}
		c, determinate := compareTemporal(x, y)
		if !determinate {
			return false, false
		}
		return c == 0, true
	}
	return false, true
}

// nodesEqual compares two complex elements by their children, which is what
// the specification means by equality for non-primitive types.
func nodesEqual(a, b Node) bool {
	ap, aPrim := a.Primitive()
	bp, bPrim := b.Primitive()
	if aPrim != bPrim {
		return false
	}
	if aPrim {
		eq, known := valuesEqualKnown(ap, bp)
		return known && eq
	}
	ac, bc := a.Children(""), b.Children("")
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if ac[i].FHIRType() != bc[i].FHIRType() || !nodesEqual(ac[i], bc[i]) {
			return false
		}
	}
	return true
}

// valuesEquivalent implements ~: like equality, but case- and
// whitespace-insensitive for strings, and never unknown.
func valuesEquivalent(a, b Value) bool {
	av, bv := unwrap(a), unwrap(b)
	if x, ok := av.(String_); ok {
		if y, ok := bv.(String_); ok {
			return normalizeForEquivalence(string(x)) == normalizeForEquivalence(string(y))
		}
		return false
	}
	if x, ok := av.(Temporal); ok {
		if y, ok := bv.(Temporal); ok {
			// Equivalence treats differing precision as simply not equivalent
			// rather than unknown.
			if (x.Kind == "Time") != (y.Kind == "Time") || x.Prec != y.Prec {
				return false
			}
			c, determinate := compareTemporal(x, y)
			return determinate && c == 0
		}
		return false
	}
	if an, ok := av.(Node); ok {
		if bn, ok := bv.(Node); ok {
			return nodesEquivalent(an, bn)
		}
		return false
	}
	// Decimals compare at the lesser of the two precisions, so 1.2/1.8 is
	// equivalent to 0.67 even though the quotient runs on forever.
	if x, ok := av.(Decimal); ok {
		if y, ok := bv.(Decimal); ok {
			return decimalsEquivalent(x, y)
		}
	}
	// Quantities convert to a common unit first, then compare the same way:
	// 4 g is equivalent to 4040 mg, because 4.04 rounded to the precision of
	// "4" is 4.
	if x, ok := av.(Quantity); ok {
		if y, ok := bv.(Quantity); ok {
			xr, yr, convertible := alignQuantities(x, y)
			if !convertible {
				return false
			}
			return decimalsEquivalent(
				Decimal{rat: xr, scale: x.Value.Scale()},
				Decimal{rat: yr, scale: y.Value.Scale()})
		}
	}
	eq, known := valuesEqualKnown(av, bv)
	return known && eq
}

// decimalsEquivalent rounds both values to the coarser of their two scales
// before comparing. A computed value has no scale of its own and takes the
// other's.
func decimalsEquivalent(a, b Decimal) bool {
	scale := a.Scale()
	switch {
	case a.Scale() < 0 && b.Scale() < 0:
		return a.Rat().Cmp(b.Rat()) == 0
	case a.Scale() < 0:
		scale = b.Scale()
	case b.Scale() < 0:
		scale = a.Scale()
	case b.Scale() < scale:
		scale = b.Scale()
	}
	return a.Rat().FloatString(scale) == b.Rat().FloatString(scale)
}

func nodesEquivalent(a, b Node) bool {
	ap, aPrim := a.Primitive()
	bp, bPrim := b.Primitive()
	if aPrim != bPrim {
		return false
	}
	if aPrim {
		return valuesEquivalent(ap, bp)
	}
	ac, bc := a.Children(""), b.Children("")
	if len(ac) != len(bc) {
		return false
	}
	for i := range ac {
		if !nodesEquivalent(ac[i], bc[i]) {
			return false
		}
	}
	return true
}

// normalizeForEquivalence collapses whitespace runs to a single space and
// lowercases, which is what the specification prescribes for string
// equivalence.
func normalizeForEquivalence(s string) string {
	var b strings.Builder
	lastSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
			}
			lastSpace = true
			continue
		}
		lastSpace = false
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.TrimSpace(b.String())
}

// comparison implements <, <=, >, >=.
func (ev *evaluator) comparison(op string, left, right Collection) (Collection, error) {
	if left.Empty() || right.Empty() {
		return nil, nil
	}
	lv, lok := left.Single()
	rv, rok := right.Single()
	if !lok || !rok {
		return nil, evalErrorf("comparison requires single values")
	}
	if valuelessPrimitive(lv) || valuelessPrimitive(rv) {
		return nil, nil
	}
	cmp, known, err := compareValues(unwrap(lv), unwrap(rv))
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, nil
	}
	switch op {
	case "<":
		return boolCollection(cmp < 0), nil
	case "<=":
		return boolCollection(cmp <= 0), nil
	case ">":
		return boolCollection(cmp > 0), nil
	case ">=":
		return boolCollection(cmp >= 0), nil
	}
	return nil, evalErrorf("unsupported comparison %q", op)
}

// compareValues orders two values, reporting known=false when the comparison is
// indeterminate and an error when the types cannot be compared at all.
func compareValues(a, b Value) (cmp int, known bool, err error) {
	switch x := a.(type) {
	case String_:
		y, ok := b.(String_)
		if !ok {
			return 0, false, evalErrorf("cannot compare %s with %s", a.TypeName(), b.TypeName())
		}
		return strings.Compare(string(x), string(y)), true, nil
	case Integer:
		switch y := b.(type) {
		case Integer:
			return cmpInt64(int64(x), int64(y)), true, nil
		case Decimal:
			return DecimalFromInt(int64(x)).Rat().Cmp(y.Rat()), true, nil
		}
	case Decimal:
		switch y := b.(type) {
		case Integer:
			return x.Rat().Cmp(DecimalFromInt(int64(y)).Rat()), true, nil
		case Decimal:
			return x.Rat().Cmp(y.Rat()), true, nil
		}
	case Quantity:
		y, ok := b.(Quantity)
		if !ok {
			return 0, false, evalErrorf("cannot compare %s with %s", a.TypeName(), b.TypeName())
		}
		xr, yr, ok := alignQuantities(x, y)
		if !ok {
			// Differing, unconvertible units are not an ordering failure but an
			// unanswerable question.
			return 0, false, nil
		}
		return xr.Cmp(yr), true, nil
	case Temporal:
		y, ok := b.(Temporal)
		if !ok {
			return 0, false, evalErrorf("cannot compare %s with %s", a.TypeName(), b.TypeName())
		}
		// A Date and a DateTime are comparable: the specification treats a Date
		// as a DateTime whose time is simply unspecified, so the ordinary
		// precision rules decide the answer. Only a Time is incomparable with
		// either, having no date to anchor it.
		if (x.Kind == "Time") != (y.Kind == "Time") {
			return 0, false, evalErrorf("cannot compare %s with %s", x.TypeName(), y.TypeName())
		}
		c, determinate := compareTemporal(x, y)
		return c, determinate, nil
	case Boolean:
		return 0, false, evalErrorf("booleans are not ordered")
	}
	return 0, false, evalErrorf("cannot compare %s with %s", a.TypeName(), b.TypeName())
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// unitsComparable reports whether two quantity units can be compared directly.
// Without a UCUM implementation only identical units qualify, plus the
// equivalence between the empty unit and the dimensionless "1".
func unitsComparable(a, b string) bool {
	norm := func(u string) string {
		if u == "" {
			return "1"
		}
		return u
	}
	return norm(a) == norm(b)
}

// arithmetic implements +, -, *, /, div, mod, and &.
func (ev *evaluator) arithmetic(op string, left, right Collection) (Collection, error) {
	// String concatenation with "&" treats empty as the empty string, which is
	// its entire reason for existing alongside "+".
	if op == "&" {
		ls, _ := asString(left)
		rs, _ := asString(right)
		if !left.Empty() {
			if _, ok := asString(left); !ok {
				return nil, evalErrorf("'&' requires strings")
			}
		}
		if !right.Empty() {
			if _, ok := asString(right); !ok {
				return nil, evalErrorf("'&' requires strings")
			}
		}
		return one(String_(ls + rs)), nil
	}

	if left.Empty() || right.Empty() {
		return nil, nil
	}
	lv, lok := left.Single()
	rv, rok := right.Single()
	if !lok || !rok {
		return nil, evalErrorf("operator %q requires single values", op)
	}
	a, b := unwrap(lv), unwrap(rv)

	if s, ok := a.(String_); ok {
		t, ok := b.(String_)
		if !ok || op != "+" {
			return nil, evalErrorf("cannot apply %q to %s and %s", op, a.TypeName(), b.TypeName())
		}
		return one(String_(string(s) + string(t))), nil
	}

	// Date arithmetic adds or subtracts a duration.
	if t, ok := a.(Temporal); ok {
		q, ok := b.(Quantity)
		if !ok || (op != "+" && op != "-") {
			return nil, evalErrorf("cannot apply %q to %s and %s", op, a.TypeName(), b.TypeName())
		}
		return addDuration(t, q, op == "-")
	}

	an, aIsNum := numericRat(a)
	bn, bIsNum := numericRat(b)
	if !aIsNum || !bIsNum {
		return nil, evalErrorf("cannot apply %q to %s and %s", op, a.TypeName(), b.TypeName())
	}

	// A quantity result keeps its unit; mixing different units is not supported
	// without UCUM conversion.
	unit, isQuantity, err := arithmeticUnit(op, a, b)
	if err != nil {
		return nil, err
	}

	bothInt := isIntegerValue(a) && isIntegerValue(b)
	res := new(big.Rat)
	switch op {
	case "+":
		res.Add(an, bn)
	case "-":
		res.Sub(an, bn)
	case "*":
		res.Mul(an, bn)
	case "/", "div", "mod":
		if bn.Sign() == 0 {
			// Division by zero is empty, not an error.
			return nil, nil
		}
		switch op {
		case "/":
			res.Quo(an, bn)
			bothInt = false // division always produces a decimal
		case "div":
			q := new(big.Rat).Quo(an, bn)
			res.SetInt(new(big.Int).Quo(q.Num(), q.Denom()))
		case "mod":
			q := new(big.Rat).Quo(an, bn)
			whole := new(big.Rat).SetInt(new(big.Int).Quo(q.Num(), q.Denom()))
			res.Sub(an, new(big.Rat).Mul(whole, bn))
		}
	default:
		return nil, evalErrorf("unsupported operator %q", op)
	}

	if isQuantity {
		return one(Quantity{Value: DecimalFromRat(res), Unit: unit}), nil
	}
	if bothInt && res.IsInt() {
		return one(Integer(res.Num().Int64())), nil
	}
	return one(DecimalFromRat(res)), nil
}

// arithmeticUnit determines the unit of a numeric result, rejecting operations
// that would need unit conversion.
func arithmeticUnit(op string, a, b Value) (unit string, isQuantity bool, err error) {
	aq, aIsQ := a.(Quantity)
	bq, bIsQ := b.(Quantity)
	switch {
	case !aIsQ && !bIsQ:
		return "", false, nil
	case aIsQ && bIsQ:
		switch op {
		case "+", "-":
			if !unitsComparable(aq.Unit, bq.Unit) {
				return "", false, evalErrorf("cannot apply %q to quantities with units %q and %q", op, aq.Unit, bq.Unit)
			}
			return aq.Unit, true, nil
		case "*":
			return combineUnits(aq.Unit, bq.Unit, "."), true, nil
		case "/":
			if unitsComparable(aq.Unit, bq.Unit) {
				// The units cancel, leaving a dimensionless quantity.
				return "1", true, nil
			}
			return combineUnits(aq.Unit, bq.Unit, "/"), true, nil
		}
		return aq.Unit, true, nil
	case aIsQ:
		return aq.Unit, true, nil
	default:
		return bq.Unit, true, nil
	}
}

func numericRat(v Value) (*big.Rat, bool) {
	switch x := v.(type) {
	case Integer:
		return new(big.Rat).SetInt64(int64(x)), true
	case Decimal:
		return x.Rat(), true
	case Quantity:
		return x.Value.Rat(), true
	}
	return nil, false
}

func isIntegerValue(v Value) bool {
	_, ok := v.(Integer)
	return ok
}

// addDuration shifts a temporal value by a calendar or UCUM duration. Calendar
// units are not fixed spans -- adding a month to January 31 lands on the last
// day of February, not on March 3 -- so this defers to the calendar rather than
// adding a number of seconds.
func addDuration(t Temporal, q Quantity, subtract bool) (Collection, error) {
	// A UCUM month or year has no definite length, so it cannot be added to a
	// date; the calendar keywords "month" and "year" can, because they mean
	// "the same day next month" rather than a fixed span.
	if q.Unit == "mo" || q.Unit == "a" {
		return nil, evalErrorf("cannot add the UCUM duration %q to a date; use the calendar unit instead", q.Unit)
	}
	// A fractional amount is carried down into the next finer unit where one
	// exists -- 0.1 's' is 100 milliseconds -- and truncated otherwise.
	value := q.Value.Rat()
	unit := durationUnit(q.Unit)
	if !value.IsInt() {
		if finer, scale, ok := finerDurationUnit(unit); ok {
			value = new(big.Rat).Mul(value, new(big.Rat).SetInt64(scale))
			unit = finer
			q = Quantity{Value: DecimalFromRat(value), Unit: finer, Calendar: q.Calendar}
		}
	}
	whole := new(big.Int).Quo(value.Num(), value.Denom())
	n := int(whole.Int64())
	if subtract {
		n = -n
	}
	out := t
	out.text = "" // the value no longer matches its source spelling

	switch unit {
	case "year":
		out.Year += n
	case "month":
		total := out.Year*12 + (out.Month - 1) + n
		out.Year, out.Month = total/12, total%12+1
		if out.Month < 1 {
			out.Year--
			out.Month += 12
		}
		out = clampDay(out)
	case "week":
		return shiftDays(out, n*7)
	case "day":
		return shiftDays(out, n)
	case "hour":
		return shiftSeconds(out, n*3600)
	case "minute":
		return shiftSeconds(out, n*60)
	case "second":
		return shiftSeconds(out, n)
	case "millisecond":
		return shiftMillis(out, n)
	default:
		return nil, evalErrorf("unsupported duration unit %q", q.Unit)
	}
	return one(out), nil
}

// durationUnits maps every spelling of a time unit -- calendar keyword, plural,
// and UCUM code -- onto one canonical name.
//
// The codes are not a suffix pattern: "ms" is milliseconds and "s" is seconds,
// so singularizing by trimming a trailing "s" turns milliseconds into minutes.
var durationUnits = map[string]string{
	"year": "year", "years": "year", "a": "year",
	"month": "month", "months": "month", "mo": "month",
	"week": "week", "weeks": "week", "wk": "week",
	"day": "day", "days": "day", "d": "day",
	"hour": "hour", "hours": "hour", "h": "hour",
	"minute": "minute", "minutes": "minute", "min": "minute",
	"second": "second", "seconds": "second", "s": "second",
	"millisecond": "millisecond", "milliseconds": "millisecond", "ms": "millisecond",
}

func durationUnit(u string) string { return durationUnits[u] }

// durationSeconds gives the length of a unit in seconds for comparing
// quantities. Calendar months and years vary in length, so they are excluded:
// comparing them requires an anchor date that a bare quantity does not have.
var durationSeconds = map[string]*big.Rat{
	"week":        big.NewRat(604800, 1),
	"day":         big.NewRat(86400, 1),
	"hour":        big.NewRat(3600, 1),
	"minute":      big.NewRat(60, 1),
	"second":      big.NewRat(1, 1),
	"millisecond": big.NewRat(1, 1000),
}

// alignQuantities puts two quantities on a common scale so they can be
// compared. Identical units need no work; time units convert through seconds.
// Anything else -- a UCUM conversion in general -- is out of scope without a
// units library, and reports false so callers can answer "unknown" rather than
// "unequal".
func alignQuantities(a, b Quantity) (x, y *big.Rat, ok bool) {
	if unitsComparable(a.Unit, b.Unit) {
		return a.Value.Rat(), b.Value.Rat(), true
	}
	// A calendar keyword and a UCUM code convert into one another only for
	// definite durations. A UCUM "mo" is a fixed span while a calendar month is
	// whatever the calendar says, so "1 'mo' = 1 month" is unanswerable -- but
	// "7 days = 1 'wk'" is simply true.
	if calendarUnit(a.Unit) != calendarUnit(b.Unit) {
		if !definiteDuration(a.Unit) || !definiteDuration(b.Unit) {
			return nil, nil, false
		}
	}
	// Units within one dimension convert through their base unit.
	if as, aOK := ucumScale[a.Unit]; aOK {
		if bs, bOK := ucumScale[b.Unit]; bOK && as.dim == bs.dim {
			return new(big.Rat).Mul(a.Value.Rat(), as.scale),
				new(big.Rat).Mul(b.Value.Rat(), bs.scale), true
		}
	}
	as, aOK := durationSeconds[durationUnit(a.Unit)]
	bs, bOK := durationSeconds[durationUnit(b.Unit)]
	if !aOK || !bOK {
		return nil, nil, false
	}
	return new(big.Rat).Mul(a.Value.Rat(), as), new(big.Rat).Mul(b.Value.Rat(), bs), true
}

// combineUnits builds the unit of a product or quotient. This is expression
// building, not UCUM algebra: "g" over "m" becomes "g/m". A dimensionless
// operand contributes nothing.
func combineUnits(a, b, sep string) string {
	if a == "" || a == "1" {
		if sep == "/" {
			return "1" + sep + b
		}
		return b
	}
	if b == "" || b == "1" {
		return a
	}
	return a + sep + b
}

// calendarUnit reports whether a unit is a calendar keyword rather than a UCUM
// code. The two do not compare: a UCUM "mo" is a fixed span while a calendar
// "month" is not, so "1 \'mo\' = 1 month" is empty rather than true.
func calendarUnit(u string) bool {
	switch u {
	case "year", "years", "month", "months", "week", "weeks", "day", "days",
		"hour", "hours", "minute", "minutes", "second", "seconds",
		"millisecond", "milliseconds":
		return true
	}
	return false
}

// ucumScale gives each supported unit's size in its dimension's base unit, so
// quantities within one dimension can be compared: 4 g equals 4000 mg.
//
// This is a table, not a UCUM implementation. It covers the units the
// conformance suite exercises; a general implementation parses UCUM's grammar
// and belongs in its own package.
var ucumScale = map[string]struct {
	dim   string
	scale *big.Rat
}{
	"g":       {"mass", big.NewRat(1, 1)},
	"mg":      {"mass", big.NewRat(1, 1000)},
	"ug":      {"mass", big.NewRat(1, 1000000)},
	"kg":      {"mass", big.NewRat(1000, 1)},
	"[lb_av]": {"mass", big.NewRat(45359237, 100000)},
	"[oz_av]": {"mass", big.NewRat(45359237, 1600000)},
	"m":       {"length", big.NewRat(1, 1)},
	"cm":      {"length", big.NewRat(1, 100)},
	"mm":      {"length", big.NewRat(1, 1000)},
	"km":      {"length", big.NewRat(1000, 1)},
	"[in_i]":  {"length", big.NewRat(254, 10000)},
	"[ft_i]":  {"length", big.NewRat(3048, 10000)},
	"L":       {"volume", big.NewRat(1, 1)},
	"dL":      {"volume", big.NewRat(1, 10)},
	"mL":      {"volume", big.NewRat(1, 1000)},
}

// definiteDuration reports whether a unit denotes a fixed span of time. Months
// and years do not: their length depends on where in the calendar they fall.
func definiteDuration(u string) bool {
	switch durationUnit(u) {
	case "week", "day", "hour", "minute", "second", "millisecond":
		return true
	}
	return false
}

// finerDurationUnit gives the next smaller unit and how many of it make one of
// the current unit.
//
// Only seconds carry a fraction. The specification truncates a fractional
// amount to the precision of its own unit, so "+ 7.7 days" adds seven days
// rather than seven days and seventeen hours -- but a dateTime records
// milliseconds as part of its seconds field, so "+ 0.1 's'" is exact.
func finerDurationUnit(unit string) (string, int64, bool) {
	if unit == "second" {
		return "millisecond", 1000, true
	}
	return "", 0, false
}
