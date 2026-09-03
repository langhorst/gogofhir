package fhirpath

import (
	"encoding/base64"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// String, math, conversion, and the date/time utilities.
//
// A note that governs the whole file: FHIRPath string functions index by
// character, not byte. Go's native slicing is by byte, so every offset here
// goes through runes. Getting that wrong only shows up on non-ASCII input,
// which is exactly the kind of bug that survives to production.

func (ev *evaluator) stringMathCall(name string, focus Collection, args []Collection) (Collection, error) {
	switch name {
	// ---- conversion ----
	case "toString":
		return convertOne(focus, toStringValue)
	case "convertsToString":
		return convertsTo(focus, toStringValue)
	case "toBoolean":
		return convertOne(focus, toBooleanValue)
	case "convertsToBoolean":
		return convertsTo(focus, toBooleanValue)
	case "toInteger":
		return convertOne(focus, toIntegerValue)
	case "convertsToInteger":
		return convertsTo(focus, toIntegerValue)
	case "toDecimal":
		return convertOne(focus, toDecimalValue)
	case "convertsToDecimal":
		return convertsTo(focus, toDecimalValue)
	case "toDate":
		return convertOne(focus, toDateValue)
	case "convertsToDate":
		return convertsTo(focus, toDateValue)
	case "toDateTime":
		return convertOne(focus, toDateTimeValue)
	case "convertsToDateTime":
		return convertsTo(focus, toDateTimeValue)
	case "toTime":
		return convertOne(focus, toTimeValue)
	case "convertsToTime":
		return convertsTo(focus, toTimeValue)
	case "toQuantity":
		return convertOne(focus, toQuantityValue)
	case "convertsToQuantity":
		return convertsTo(focus, toQuantityValue)

	// ---- strings ----
	case "length":
		s, ok, empty := singleString(focus)
		if empty {
			return nil, nil
		}
		if !ok {
			return nil, evalErrorf("length() requires a string")
		}
		return one(Integer(utf8.RuneCountInString(s))), nil
	case "upper", "lower", "trim":
		s, ok, empty := singleString(focus)
		if empty {
			return nil, nil
		}
		if !ok {
			return nil, evalErrorf("%s() requires a string", name)
		}
		switch name {
		case "upper":
			return one(String_(strings.ToUpper(s))), nil
		case "lower":
			return one(String_(strings.ToLower(s))), nil
		default:
			return one(String_(strings.TrimSpace(s))), nil
		}
	case "toChars":
		s, ok, empty := singleString(focus)
		if empty {
			return nil, nil
		}
		if !ok {
			return nil, evalErrorf("toChars() requires a string")
		}
		var out Collection
		for _, r := range s {
			out = append(out, String_(string(r)))
		}
		return out, nil
	case "startsWith", "endsWith", "contains", "indexOf":
		s, ok, empty := singleString(focus)
		if empty {
			return nil, nil
		}
		if !ok {
			return nil, evalErrorf("%s() requires a string", name)
		}
		if len(args) != 1 {
			return nil, evalErrorf("%s() takes exactly one argument", name)
		}
		arg, argOK := asString(args[0])
		if args[0].Empty() {
			return nil, nil
		}
		if !argOK {
			return nil, evalErrorf("%s() takes a string argument", name)
		}
		switch name {
		case "startsWith":
			return boolCollection(strings.HasPrefix(s, arg)), nil
		case "endsWith":
			return boolCollection(strings.HasSuffix(s, arg)), nil
		case "contains":
			return boolCollection(strings.Contains(s, arg)), nil
		default:
			return one(Integer(runeIndex(s, arg))), nil
		}
	case "substring":
		return substring(focus, args)
	case "replace":
		s, ok, empty := singleString(focus)
		if empty {
			return nil, nil
		}
		if !ok || len(args) != 2 {
			return nil, evalErrorf("replace() requires a string and two string arguments")
		}
		if args[0].Empty() || args[1].Empty() {
			return nil, nil
		}
		pattern, ok1 := asString(args[0])
		repl, ok2 := asString(args[1])
		if !ok1 || !ok2 {
			return nil, evalErrorf("replace() takes string arguments")
		}
		if pattern == "" {
			// Replacing the empty string surrounds every character, per the
			// specification's worked example.
			var b strings.Builder
			b.WriteString(repl)
			for _, r := range s {
				b.WriteRune(r)
				b.WriteString(repl)
			}
			return one(String_(b.String())), nil
		}
		return one(String_(strings.ReplaceAll(s, pattern, repl))), nil
	case "matches", "replaceMatches", "matchesFull":
		return regexCall(name, focus, args)
	case "split":
		s, ok, empty := singleString(focus)
		if empty {
			return nil, nil
		}
		if !ok || len(args) != 1 {
			return nil, evalErrorf("split() requires a string and one separator")
		}
		sep, sepOK := asString(args[0])
		if !sepOK {
			return nil, evalErrorf("split() takes a string separator")
		}
		var out Collection
		for _, part := range strings.Split(s, sep) {
			out = append(out, String_(part))
		}
		return out, nil
	case "join":
		sep := ""
		if len(args) == 1 {
			s, ok := asString(args[0])
			if !ok && !args[0].Empty() {
				return nil, evalErrorf("join() takes a string separator")
			}
			sep = s
		}
		parts := make([]string, 0, len(focus))
		for _, v := range focus {
			s, err := toStringValue(unwrap(v))
			if err != nil {
				return nil, err
			}
			parts = append(parts, s.String())
		}
		return one(String_(strings.Join(parts, sep))), nil
	case "encode", "decode":
		return codec(name, focus, args)
	case "escape", "unescape":
		return escapeFn(name, focus, args)

	// ---- math ----
	case "abs", "ceiling", "floor", "truncate", "sqrt", "exp", "ln", "round", "power", "log":
		return mathCall(name, focus, args)

	// ---- date/time utilities ----
	case "now":
		return one(nowDateTime()), nil
	case "today":
		return one(todayDate()), nil
	case "timeOfDay":
		return one(nowTimeOfDay()), nil
	}

	return nil, evalErrorf("unknown function %s()", name)
}

// singleString reads a single string from a collection, reporting separately
// whether the collection was empty (which is not an error) and whether the item
// was a string (which, when it is not, usually is).
func singleString(c Collection) (s string, ok, empty bool) {
	if c.Empty() {
		return "", false, true
	}
	v, single := c.Single()
	if !single {
		return "", false, false
	}
	// A FHIR primitive that carries only extensions has no value. It is not a
	// type error to ask for its length -- there is simply nothing there -- so
	// it behaves as an empty collection. A complex element is a different
	// matter and still fails.
	if valuelessPrimitive(v) {
		return "", false, true
	}
	unwrapped := unwrap(v)
	str, isStr := unwrapped.(String_)
	if !isStr {
		return "", false, false
	}
	return string(str), true, false
}

// runeIndex reports the character offset of substr, or -1.
func runeIndex(s, substr string) int {
	byteIdx := strings.Index(s, substr)
	if byteIdx < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:byteIdx])
}

func substring(focus Collection, args []Collection) (Collection, error) {
	s, ok, empty := singleString(focus)
	if empty {
		return nil, nil
	}
	if !ok {
		return nil, evalErrorf("substring() requires a string")
	}
	if len(args) < 1 || len(args) > 2 {
		return nil, evalErrorf("substring() takes one or two arguments")
	}
	start, startOK := asInt(args[0])
	if !startOK {
		return nil, nil
	}
	runes := []rune(s)
	if start < 0 || int(start) >= len(runes) {
		// Out of range yields empty rather than an error.
		return nil, nil
	}
	end := len(runes)
	if len(args) == 2 {
		if args[1].Empty() {
			return nil, nil
		}
		length, lenOK := asInt(args[1])
		if !lenOK {
			return nil, evalErrorf("substring() length must be an integer")
		}
		if length < 0 {
			return nil, nil
		}
		if int(start)+int(length) < end {
			end = int(start) + int(length)
		}
	}
	return one(String_(string(runes[start:end]))), nil
}

// fhirPathRegex adapts a FHIRPath regex to Go's engine. FHIRPath specifies
// single-line mode off and multi-line semantics matching XSD, and Go's default
// is close enough that only the dot-matches-newline flag needs setting.
func fhirPathRegex(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile("(?s)" + pattern)
	if err != nil {
		return nil, evalErrorf("invalid regular expression %q: %v", pattern, err)
	}
	return re, nil
}

func regexCall(name string, focus Collection, args []Collection) (Collection, error) {
	s, ok, empty := singleString(focus)
	if empty {
		return nil, nil
	}
	if !ok {
		return nil, evalErrorf("%s() requires a string", name)
	}
	if len(args) < 1 {
		return nil, evalErrorf("%s() requires a pattern", name)
	}
	if args[0].Empty() {
		return nil, nil
	}
	pattern, patOK := asString(args[0])
	if !patOK {
		return nil, evalErrorf("%s() takes a string pattern", name)
	}
	switch name {
	case "matches":
		re, err := fhirPathRegex(pattern)
		if err != nil {
			return nil, err
		}
		return boolCollection(re.MatchString(s)), nil
	case "matchesFull":
		re, err := fhirPathRegex("^(?:" + pattern + ")$")
		if err != nil {
			return nil, err
		}
		return boolCollection(re.MatchString(s)), nil
	default: // replaceMatches
		if len(args) != 2 {
			return nil, evalErrorf("replaceMatches() takes two arguments")
		}
		if args[1].Empty() {
			return nil, nil
		}
		repl, replOK := asString(args[1])
		if !replOK {
			return nil, evalErrorf("replaceMatches() takes a string substitution")
		}
		if pattern == "" {
			// An empty pattern matches at every position; Go would splice the
			// substitution between each character. The specification treats it
			// as matching nothing, leaving the string untouched.
			return one(String_(s)), nil
		}
		re, err := fhirPathRegex(pattern)
		if err != nil {
			return nil, err
		}
		// FHIRPath substitutions use $1; Go uses ${1}. Converting keeps
		// published expressions working.
		return one(String_(re.ReplaceAllString(s, dollarToGoRefs(repl)))), nil
	}
}

// dollarToGoRefs rewrites $1 into ${1} so a bare group reference followed by a
// digit or letter is not misread as a longer group number.
func dollarToGoRefs(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) || s[i+1] < '0' || s[i+1] > '9' {
			b.WriteByte(s[i])
			continue
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		b.WriteString("${")
		b.WriteString(s[i+1 : j])
		b.WriteString("}")
		i = j - 1
	}
	return b.String()
}

func codec(name string, focus Collection, args []Collection) (Collection, error) {
	s, ok, empty := singleString(focus)
	if empty {
		return nil, nil
	}
	if !ok || len(args) != 1 {
		return nil, evalErrorf("%s() requires a string and a format", name)
	}
	format, fmtOK := asString(args[0])
	if !fmtOK {
		return nil, evalErrorf("%s() takes a string format", name)
	}
	switch format {
	case "base64":
		if name == "encode" {
			return one(String_(base64.StdEncoding.EncodeToString([]byte(s)))), nil
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, nil
		}
		return one(String_(raw)), nil
	case "urlbase64", "base64url":
		if name == "encode" {
			return one(String_(base64.URLEncoding.EncodeToString([]byte(s)))), nil
		}
		raw, err := base64.URLEncoding.DecodeString(s)
		if err != nil {
			return nil, nil
		}
		return one(String_(raw)), nil
	case "hex":
		if name == "encode" {
			var b strings.Builder
			for i := 0; i < len(s); i++ {
				b.WriteString(strconv.FormatInt(int64(s[i]), 16))
			}
			return one(String_(b.String())), nil
		}
		if len(s)%2 != 0 {
			return nil, nil
		}
		out := make([]byte, 0, len(s)/2)
		for i := 0; i < len(s); i += 2 {
			v, err := strconv.ParseUint(s[i:i+2], 16, 8)
			if err != nil {
				return nil, nil
			}
			out = append(out, byte(v))
		}
		return one(String_(out)), nil
	}
	return nil, evalErrorf("unsupported %s format %q", name, format)
}

var (
	htmlEscaper = strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	htmlUnescaper = strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")
	jsonEscaper = strings.NewReplacer(
		`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\b", `\b`, "\f", `\f`)
	jsonUnescaper = strings.NewReplacer(
		`\\`, `\`, `\"`, `"`, `\n`, "\n", `\r`, "\r", `\t`, "\t", `\b`, "\b", `\f`, "\f")
)

func escapeFn(name string, focus Collection, args []Collection) (Collection, error) {
	s, ok, empty := singleString(focus)
	if empty {
		return nil, nil
	}
	if !ok || len(args) != 1 {
		return nil, evalErrorf("%s() requires a string and a target", name)
	}
	target, targetOK := asString(args[0])
	if !targetOK {
		return nil, evalErrorf("%s() takes a string target", name)
	}
	switch target {
	case "html":
		if name == "escape" {
			return one(String_(htmlEscaper.Replace(s))), nil
		}
		return one(String_(htmlUnescaper.Replace(s))), nil
	case "json":
		if name == "escape" {
			return one(String_(jsonEscaper.Replace(s))), nil
		}
		return one(String_(jsonUnescaper.Replace(s))), nil
	}
	return nil, evalErrorf("unsupported %s target %q", name, target)
}

func mathCall(name string, focus Collection, args []Collection) (Collection, error) {
	if focus.Empty() {
		return nil, nil
	}
	v, ok := focus.Single()
	if !ok {
		return nil, evalErrorf("%s() requires a single value", name)
	}
	val := unwrap(v)

	// abs is the only one defined on quantities, which keep their unit.
	if q, isQ := val.(Quantity); isQ && name == "abs" {
		return one(Quantity{Value: DecimalFromRat(new(big.Rat).Abs(q.Value.Rat())), Unit: q.Unit}), nil
	}

	r, isNum := numericRat(val)
	if !isNum {
		return nil, evalErrorf("%s() requires a number", name)
	}
	wasInt := isIntegerValue(val)

	switch name {
	case "abs":
		out := new(big.Rat).Abs(r)
		if wasInt {
			return one(Integer(out.Num().Int64())), nil
		}
		return one(DecimalFromRat(out)), nil
	case "ceiling", "floor", "truncate":
		i := new(big.Int)
		switch name {
		case "ceiling":
			i.Quo(r.Num(), r.Denom())
			if r.Sign() > 0 && !r.IsInt() {
				i.Add(i, big.NewInt(1))
			}
		case "floor":
			i.Quo(r.Num(), r.Denom())
			if r.Sign() < 0 && !r.IsInt() {
				i.Sub(i, big.NewInt(1))
			}
		default:
			i.Quo(r.Num(), r.Denom())
		}
		return one(Integer(i.Int64())), nil
	case "round":
		digits := int64(0)
		if len(args) == 1 {
			d, dOK := asInt(args[0])
			if !dOK {
				return nil, evalErrorf("round() takes an integer precision")
			}
			digits = d
		}
		if digits < 0 {
			return nil, evalErrorf("round() precision must not be negative")
		}
		rounded, err := NewDecimal(r.FloatString(int(digits)))
		if err != nil {
			return nil, err
		}
		return one(rounded), nil
	}

	// The remaining functions are transcendental, so they leave exact rational
	// arithmetic behind. A float64 carries more precision than the conformance
	// suite asks of them.
	f, _ := r.Float64()
	var res float64
	switch name {
	case "sqrt":
		if f < 0 {
			return nil, nil
		}
		res = math.Sqrt(f)
	case "exp":
		res = math.Exp(f)
	case "ln":
		if f <= 0 {
			return nil, nil
		}
		res = math.Log(f)
	case "log":
		if len(args) != 1 {
			return nil, evalErrorf("log() takes a base")
		}
		if args[0].Empty() {
			return nil, nil
		}
		baseRat, baseOK := numericRat(unwrapSingle(args[0]))
		if !baseOK {
			return nil, evalErrorf("log() takes a numeric base")
		}
		b, _ := baseRat.Float64()
		if f <= 0 || b <= 0 {
			return nil, nil
		}
		res = math.Log(f) / math.Log(b)
	case "power":
		if len(args) != 1 {
			return nil, evalErrorf("power() takes an exponent")
		}
		if args[0].Empty() {
			return nil, nil
		}
		expRat, expOK := numericRat(unwrapSingle(args[0]))
		if !expOK {
			return nil, evalErrorf("power() takes a numeric exponent")
		}
		e, _ := expRat.Float64()
		res = math.Pow(f, e)
		if math.IsNaN(res) {
			// A negative base with a fractional exponent has no real result.
			return nil, nil
		}
		if wasInt && isIntegerValue(unwrapSingle(args[0])) && res == math.Trunc(res) {
			return one(Integer(int64(res))), nil
		}
	}
	if math.IsInf(res, 0) || math.IsNaN(res) {
		return nil, nil
	}
	return one(floatDecimal(res)), nil
}

// unwrapSingle pulls the lone value out of a collection, or nil.
func unwrapSingle(c Collection) Value {
	v, ok := c.Single()
	if !ok {
		return nil
	}
	return unwrap(v)
}

// floatDecimal converts a computed float back to a decimal, trimming the
// artifacts of binary floating point rather than exposing them.
func floatDecimal(f float64) Decimal {
	d, err := NewDecimal(strconv.FormatFloat(f, 'f', -1, 64))
	if err != nil {
		return DecimalFromRat(new(big.Rat).SetFloat64(f))
	}
	return d
}

// ---- conversions ----

// convertOne applies a conversion to a singleton, yielding empty when the value
// cannot convert. Conversion failure is not an error in FHIRPath.
func convertOne(c Collection, fn func(Value) (Value, error)) (Collection, error) {
	if c.Empty() {
		return nil, nil
	}
	v, ok := c.Single()
	if !ok {
		return nil, evalErrorf("conversion requires a single value")
	}
	out, err := fn(unwrap(v))
	if err != nil {
		return nil, nil
	}
	return one(out), nil
}

// convertsTo is the predicate form: it reports whether the conversion would
// succeed, and is empty only when the input is.
func convertsTo(c Collection, fn func(Value) (Value, error)) (Collection, error) {
	if c.Empty() {
		return nil, nil
	}
	v, ok := c.Single()
	if !ok {
		return nil, evalErrorf("conversion requires a single value")
	}
	_, err := fn(unwrap(v))
	return boolCollection(err == nil), nil
}

func toStringValue(v Value) (Value, error) {
	switch x := v.(type) {
	case String_:
		return x, nil
	case Boolean, Integer, Decimal, Quantity, Temporal:
		return String_(x.String()), nil
	}
	return nil, evalErrorf("cannot convert %s to String", v.TypeName())
}

func toBooleanValue(v Value) (Value, error) {
	switch x := v.(type) {
	case Boolean:
		return x, nil
	case String_:
		switch strings.ToLower(string(x)) {
		case "true", "t", "yes", "y", "1", "1.0":
			return Boolean(true), nil
		case "false", "f", "no", "n", "0", "0.0":
			return Boolean(false), nil
		}
	case Integer:
		switch x {
		case 1:
			return Boolean(true), nil
		case 0:
			return Boolean(false), nil
		}
	case Decimal:
		switch x.String() {
		case "1", "1.0":
			return Boolean(true), nil
		case "0", "0.0":
			return Boolean(false), nil
		}
	}
	return nil, evalErrorf("cannot convert %s to Boolean", v.TypeName())
}

func toIntegerValue(v Value) (Value, error) {
	switch x := v.(type) {
	case Integer:
		return x, nil
	case String_:
		i, err := strconv.ParseInt(string(x), 10, 64)
		if err != nil {
			return nil, evalErrorf("not an integer")
		}
		return Integer(i), nil
	case Boolean:
		if x {
			return Integer(1), nil
		}
		return Integer(0), nil
	}
	return nil, evalErrorf("cannot convert %s to Integer", v.TypeName())
}

func toDecimalValue(v Value) (Value, error) {
	switch x := v.(type) {
	case Decimal:
		return x, nil
	case Integer:
		return DecimalFromInt(int64(x)), nil
	case String_:
		d, err := NewDecimal(string(x))
		if err != nil {
			return nil, evalErrorf("not a decimal")
		}
		return d, nil
	case Boolean:
		if x {
			return DecimalFromInt(1), nil
		}
		return DecimalFromInt(0), nil
	}
	return nil, evalErrorf("cannot convert %s to Decimal", v.TypeName())
}

func toDateValue(v Value) (Value, error) {
	return toTemporalValue(v, "Date")
}

func toDateTimeValue(v Value) (Value, error) {
	return toTemporalValue(v, "DateTime")
}

func toTimeValue(v Value) (Value, error) {
	return toTemporalValue(v, "Time")
}

// toTemporalValue converts to a Date, DateTime, or Time. A DateTime converts
// down to a Date by dropping its time, but a Date does not gain one.
func toTemporalValue(v Value, want string) (Value, error) {
	switch x := v.(type) {
	case Temporal:
		if x.Kind == want {
			return x, nil
		}
		if want == "Date" && x.Kind == "DateTime" {
			out := x
			out.Kind = "Date"
			if out.Prec > PrecDay {
				out.Prec = PrecDay
			}
			out.HasZone = false
			out.text = ""
			return out, nil
		}
		if want == "DateTime" && x.Kind == "Date" {
			out := x
			out.Kind = "DateTime"
			out.text = ""
			return out, nil
		}
		return nil, evalErrorf("cannot convert %s to %s", x.Kind, want)
	case String_:
		s := string(x)
		var t Temporal
		var err error
		if want == "Time" {
			t, err = ParseTemporal("T" + strings.TrimPrefix(s, "T"))
		} else {
			t, err = ParseTemporal(s)
		}
		if err != nil {
			return nil, evalErrorf("not a %s", want)
		}
		if want == "DateTime" && t.Kind == "Date" {
			t.Kind = "DateTime"
		}
		if t.Kind != want {
			return nil, evalErrorf("not a %s", want)
		}
		return t, nil
	}
	return nil, evalErrorf("cannot convert %s to %s", v.TypeName(), want)
}

// quantityRE splits a quantity string into its value and unit, accepting both
// a quoted UCUM code and a bare calendar keyword.
var quantityRE = regexp.MustCompile(`^(-?\d+(?:\.\d+)?)\s*(?:'([^']*)'|([a-zA-Z]+))?$`)

func toQuantityValue(v Value) (Value, error) {
	switch x := v.(type) {
	case Quantity:
		return x, nil
	case Integer:
		return Quantity{Value: DecimalFromInt(int64(x)), Unit: "1"}, nil
	case Decimal:
		return Quantity{Value: x, Unit: "1"}, nil
	case Boolean:
		if x {
			return Quantity{Value: DecimalFromInt(1), Unit: "1"}, nil
		}
		return Quantity{Value: DecimalFromInt(0), Unit: "1"}, nil
	case String_:
		m := quantityRE.FindStringSubmatch(string(x))
		if m == nil {
			return nil, evalErrorf("not a quantity")
		}
		d, err := NewDecimal(m[1])
		if err != nil {
			return nil, evalErrorf("not a quantity")
		}
		unit, quoted := m[2], m[2] != "" || m[3] == ""
		if unit == "" {
			// A bare unit must be a calendar keyword; anything else ("1 wk")
			// is not a quantity string.
			if m[3] != "" {
				code, isCalendar := calendarToUCUM[m[3]]
				if !isCalendar {
					return nil, evalErrorf("not a quantity")
				}
				// The specification converts a written calendar keyword to its
				// UCUM equivalent here, which is why "'1 day'.toQuantity()"
				// equals 1 'd'.
				return Quantity{Value: d, Unit: code}, nil
			}
			unit = "1"
		}
		_ = quoted
		return Quantity{Value: d, Unit: unit}, nil
	}
	return nil, evalErrorf("cannot convert %s to Quantity", v.TypeName())
}

// ---- clock ----

// The clock functions are wired through a variable so tests can pin time.
var clockNow = time.Now

func nowDateTime() Temporal {
	t := clockNow()
	_, offset := t.Zone()
	return Temporal{
		Kind: "DateTime", Prec: PrecSecond, HasMillis: true,
		Year: t.Year(), Month: int(t.Month()), Day: t.Day(),
		Hour: t.Hour(), Minute: t.Minute(), Sec: t.Second(),
		Milli:   t.Nanosecond() / int(time.Millisecond),
		HasZone: true, ZoneOffsetMinutes: offset / 60,
	}
}

func todayDate() Temporal {
	t := clockNow()
	return Temporal{
		Kind: "Date", Prec: PrecDay,
		Year: t.Year(), Month: int(t.Month()), Day: t.Day(),
	}
}

func nowTimeOfDay() Temporal {
	t := clockNow()
	return Temporal{
		Kind: "Time", Prec: PrecSecond, HasMillis: true,
		Hour: t.Hour(), Minute: t.Minute(), Sec: t.Second(),
		Milli: t.Nanosecond() / int(time.Millisecond),
	}
}

// calendarToUCUM maps the calendar keywords a quantity string may use onto the
// UCUM codes toQuantity produces.
var calendarToUCUM = map[string]string{
	"year": "a", "years": "a",
	"month": "mo", "months": "mo",
	"week": "wk", "weeks": "wk",
	"day": "d", "days": "d",
	"hour": "h", "hours": "h",
	"minute": "min", "minutes": "min",
	"second": "s", "seconds": "s",
	"millisecond": "ms", "milliseconds": "ms",
}
