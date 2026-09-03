package fhirpath

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FHIRPath dates and times carry a precision, and that precision is part of the
// value rather than presentation. "@2012" is the year 2012, not midnight on the
// first of January, and comparing it with "@2012-01" cannot be answered -- the
// specification requires empty, not false, because the question is
// indeterminate rather than negative.
//
// Collapsing partial values onto time.Time would silently invent the missing
// components and turn those indeterminate comparisons into confident wrong
// answers, so temporal values keep their components and precision explicitly.

// Precision is how far a temporal value is specified.
type Precision int

const (
	PrecNone Precision = iota
	PrecYear
	PrecMonth
	PrecDay
	PrecHour
	PrecMinute
	// PrecSecond covers fractional seconds too: the specification treats
	// milliseconds as part of the seconds value rather than a further level of
	// precision, so "10:30:00" and "10:30:00.0" are equally precise and compare
	// equal. Ranking milliseconds separately makes every such comparison return
	// empty instead of a result.
	PrecSecond
)

// Temporal is a Date, DateTime, or Time.
type Temporal struct {
	// Kind is "Date", "DateTime", or "Time".
	Kind string
	Prec Precision

	Year, Month, Day  int
	Hour, Minute, Sec int
	Milli             int
	// HasMillis records that a fractional second was written. Precision is
	// tracked only to the second (see PrecSecond), but precision() reports
	// digit counts and must still tell "10:30:00" from "10:30:00.000".
	HasMillis         bool
	HasZone           bool
	ZoneOffsetMinutes int
	// text is the source spelling, so a value renders exactly as written rather
	// than in a normalized form the suite would not recognize.
	text string
}

func (t Temporal) TypeName() string { return "System." + t.Kind }

func (t Temporal) String() string {
	if t.text != "" {
		return t.text
	}
	return t.render()
}

func (t Temporal) render() string {
	var b strings.Builder
	if t.Kind == "Time" {
		b.WriteString("T")
		b.WriteString(t.renderTime())
		return b.String()
	}
	fmt.Fprintf(&b, "%04d", t.Year)
	if t.Prec >= PrecMonth {
		fmt.Fprintf(&b, "-%02d", t.Month)
	}
	if t.Prec >= PrecDay {
		fmt.Fprintf(&b, "-%02d", t.Day)
	}
	if t.Prec >= PrecHour {
		b.WriteString("T")
		b.WriteString(t.renderTime())
		b.WriteString(t.renderZone())
	}
	return b.String()
}

func (t Temporal) renderTime() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%02d", t.Hour)
	if t.Prec >= PrecMinute {
		fmt.Fprintf(&b, ":%02d", t.Minute)
	}
	if t.Prec >= PrecSecond {
		fmt.Fprintf(&b, ":%02d", t.Sec)
	}
	if t.HasMillis {
		fmt.Fprintf(&b, ".%03d", t.Milli)
	}
	return b.String()
}

func (t Temporal) renderZone() string {
	if !t.HasZone {
		return ""
	}
	if t.ZoneOffsetMinutes == 0 {
		return "Z"
	}
	sign, off := "+", t.ZoneOffsetMinutes
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("%s%02d:%02d", sign, off/60, off%60)
}

// ParseTemporal reads a FHIRPath temporal literal without its leading "@".
// A leading "T" marks a time-of-day; everything else starts with a year.
func ParseTemporal(s string) (Temporal, error) {
	if strings.HasPrefix(s, "T") {
		t, err := parseTimeOfDay(s[1:])
		if err != nil {
			return Temporal{}, err
		}
		// A time of day has no timezone: an offset is only meaningful once a
		// date anchors it. "@T14:34:28+10:00" is not a valid literal.
		if t.HasZone {
			return Temporal{}, fmt.Errorf("a Time literal may not carry a timezone: %q", s)
		}
		t.Kind = "Time"
		t.text = "T" + s[1:]
		return t, nil
	}
	return parseDateTime(s)
}

// ParseFHIRTemporal reads the value of a FHIR date, dateTime, instant, or time
// element. The FHIR wire formats are the same grammar without the "@".
func ParseFHIRTemporal(fhirType, s string) (Temporal, error) {
	switch fhirType {
	case "time":
		t, err := parseTimeOfDay(s)
		if err != nil {
			return Temporal{}, err
		}
		t.Kind = "Time"
		t.text = s
		return t, nil
	default:
		t, err := parseDateTime(s)
		if err != nil {
			return Temporal{}, err
		}
		// A FHIR "date" is a Date even when it carries full precision, and a
		// dateTime is a DateTime even when it carries only a year.
		if fhirType == "date" {
			t.Kind = "Date"
		} else {
			t.Kind = "DateTime"
		}
		t.text = s
		return t, nil
	}
}

func parseDateTime(s string) (Temporal, error) {
	t := Temporal{Kind: "Date", text: s}
	rest := s

	datePart, timePart, hasTime := strings.Cut(rest, "T")
	fields := strings.Split(datePart, "-")
	if len(fields) == 0 || len(fields) > 3 {
		return Temporal{}, fmt.Errorf("invalid date %q", s)
	}
	var err error
	if t.Year, err = atoiExact(fields[0], 4); err != nil {
		return Temporal{}, fmt.Errorf("invalid year in %q", s)
	}
	t.Prec = PrecYear
	if len(fields) > 1 {
		if t.Month, err = atoiExact(fields[1], 2); err != nil {
			return Temporal{}, fmt.Errorf("invalid month in %q", s)
		}
		t.Prec = PrecMonth
	}
	if len(fields) > 2 {
		if t.Day, err = atoiExact(fields[2], 2); err != nil {
			return Temporal{}, fmt.Errorf("invalid day in %q", s)
		}
		t.Prec = PrecDay
	}
	if !hasTime {
		return t, nil
	}

	// The presence of a time component makes this a DateTime even if the time
	// itself is empty, as in "@2014-01-25T".
	t.Kind = "DateTime"
	if timePart == "" {
		return t, nil
	}
	tod, err := parseTimeOfDay(timePart)
	if err != nil {
		return Temporal{}, err
	}
	t.Hour, t.Minute, t.Sec, t.Milli = tod.Hour, tod.Minute, tod.Sec, tod.Milli
	t.HasMillis = tod.HasMillis
	t.HasMillis = tod.HasMillis
	t.Prec = tod.Prec
	t.HasZone, t.ZoneOffsetMinutes = tod.HasZone, tod.ZoneOffsetMinutes
	return t, nil
}

func parseTimeOfDay(s string) (Temporal, error) {
	t := Temporal{Kind: "Time"}

	// Split off the timezone first so it does not confuse field parsing.
	if i := strings.IndexAny(s, "Zz"); i >= 0 && i == len(s)-1 {
		t.HasZone = true
		s = s[:i]
	} else if i := strings.LastIndexAny(s, "+-"); i > 0 {
		offset := s[i+1:]
		sign := 1
		if s[i] == '-' {
			sign = -1
		}
		h, m, ok := strings.Cut(offset, ":")
		if !ok {
			return Temporal{}, fmt.Errorf("invalid timezone offset in %q", s)
		}
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil {
			return Temporal{}, fmt.Errorf("invalid timezone offset in %q", s)
		}
		t.HasZone = true
		t.ZoneOffsetMinutes = sign * (hh*60 + mm)
		s = s[:i]
	}

	secPart, milliPart, hasMilli := strings.Cut(s, ".")
	fields := strings.Split(secPart, ":")
	if len(fields) == 0 || len(fields) > 3 {
		return Temporal{}, fmt.Errorf("invalid time %q", s)
	}
	var err error
	if t.Hour, err = atoiExact(fields[0], 2); err != nil {
		return Temporal{}, fmt.Errorf("invalid hour in %q", s)
	}
	t.Prec = PrecHour
	if len(fields) > 1 {
		if t.Minute, err = atoiExact(fields[1], 2); err != nil {
			return Temporal{}, fmt.Errorf("invalid minute in %q", s)
		}
		t.Prec = PrecMinute
	}
	if len(fields) > 2 {
		if t.Sec, err = atoiExact(fields[2], 2); err != nil {
			return Temporal{}, fmt.Errorf("invalid second in %q", s)
		}
		t.Prec = PrecSecond
	}
	if hasMilli {
		// Milliseconds are the first three digits, zero-padded; longer
		// fractions are truncated rather than rejected.
		frac := milliPart + "000"
		if t.Milli, err = strconv.Atoi(frac[:3]); err != nil {
			return Temporal{}, fmt.Errorf("invalid fractional second in %q", s)
		}
		// Precision stays at seconds; see the note on PrecSecond.
		t.Prec = PrecSecond
		t.HasMillis = true
		t.HasMillis = true
	}
	return t, nil
}

// atoiExact parses a fixed-width, all-digit field. Accepting "1" where "01" is
// required would let malformed dates through.
func atoiExact(s string, width int) (int, error) {
	if len(s) != width {
		return 0, fmt.Errorf("expected %d digits, got %q", width, s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("non-digit in %q", s)
		}
	}
	return strconv.Atoi(s)
}

// compareTemporal orders two temporal values.
//
// The third result is false when the comparison is indeterminate: the values
// agree as far as both are specified, but one is less precise, so no ordering
// can be asserted. "@2012" versus "@2012-01" is the canonical case. Callers
// turn that into an empty collection.
func compareTemporal(a, b Temporal) (int, bool) {
	// Comparing a Time with a Date is a type error, caught by the caller.
	prec := a.Prec
	if b.Prec < prec {
		prec = b.Prec
	}

	// When either side carries a timezone and the other does not, the
	// comparison is only safe at day precision or coarser; below that the
	// offset could change the answer.
	if a.HasZone != b.HasZone && prec >= PrecHour {
		return 0, false
	}
	af, bf := a.fields(), b.fields()
	if a.HasZone && b.HasZone && a.ZoneOffsetMinutes != b.ZoneOffsetMinutes && prec >= PrecHour {
		// Normalize both to UTC before comparing.
		af = a.utc().fields()
		bf = b.utc().fields()
	}

	for i := PrecYear; i <= prec; i++ {
		x, y := af[i], bf[i]
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	// Seconds carry their fraction, so two second-precision values still differ
	// if their milliseconds do.
	if prec >= PrecSecond && a.Milli != b.Milli {
		if a.Milli < b.Milli {
			return -1, true
		}
		return 1, true
	}
	if a.Prec != b.Prec {
		// Equal so far, but one side says more. The result is unknowable.
		return 0, false
	}
	return 0, true
}

// fields exposes the components indexed by Precision so comparison can walk
// them uniformly.
func (t Temporal) fields() map[Precision]int {
	return map[Precision]int{
		PrecYear: t.Year, PrecMonth: t.Month, PrecDay: t.Day,
		PrecHour: t.Hour, PrecMinute: t.Minute, PrecSecond: t.Sec,
	}
}

// utc shifts a zoned value to UTC so two differently-offset values can be
// compared component by component.
func (t Temporal) utc() Temporal {
	if !t.HasZone || t.ZoneOffsetMinutes == 0 {
		return t
	}
	base := time.Date(t.Year, time.Month(max1(t.Month)), max1(t.Day),
		t.Hour, t.Minute, t.Sec, t.Milli*int(time.Millisecond), time.UTC)
	base = base.Add(-time.Duration(t.ZoneOffsetMinutes) * time.Minute)
	out := t
	out.Year, out.Month, out.Day = base.Year(), int(base.Month()), base.Day()
	out.Hour, out.Minute, out.Sec = base.Hour(), base.Minute(), base.Second()
	out.Milli = base.Nanosecond() / int(time.Millisecond)
	out.ZoneOffsetMinutes = 0
	out.text = ""
	return out
}

// max1 substitutes 1 for an unspecified month or day so the value can be
// anchored on the calendar for timezone shifting.
func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}

// clampDay pulls a day-of-month back to the last valid day after a year or
// month shift: adding one month to January 31 yields February 28 or 29, never
// an overflow into March.
func clampDay(t Temporal) Temporal {
	if t.Prec < PrecDay {
		return t
	}
	if last := daysInMonth(t.Year, t.Month); t.Day > last {
		t.Day = last
	}
	return t
}

func daysInMonth(year, month int) int {
	if month < 1 || month > 12 {
		return 31
	}
	// Day zero of the next month is the last day of this one.
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// shiftDays moves a value by whole days. A value specified only to year or
// month precision cannot absorb a day shift, which the specification treats as
// an error rather than inventing the missing components.
func shiftDays(t Temporal, days int) (Collection, error) {
	if t.Prec < PrecDay {
		return nil, evalErrorf("cannot add days to a value with %s precision", precisionName(t.Prec))
	}
	base := t.asTime().AddDate(0, 0, days)
	return one(t.withDate(base)), nil
}

func shiftSeconds(t Temporal, secs int) (Collection, error) {
	if t.Kind == "Time" {
		// A time of day wraps within the day rather than carrying into a date.
		total := ((t.Hour*3600+t.Minute*60+t.Sec+secs)%86400 + 86400) % 86400
		out := t
		out.Hour, out.Minute, out.Sec = total/3600, (total%3600)/60, total%60
		out.text = ""
		return one(out), nil
	}
	if t.Prec < PrecDay {
		return nil, evalErrorf("cannot add time to a value with %s precision", precisionName(t.Prec))
	}
	base := t.asTime().Add(time.Duration(secs) * time.Second)
	return one(t.withDateTime(base)), nil
}

func shiftMillis(t Temporal, ms int) (Collection, error) {
	if t.Kind == "Time" {
		total := t.Hour*3600000 + t.Minute*60000 + t.Sec*1000 + t.Milli + ms
		total = ((total % 86400000) + 86400000) % 86400000
		out := t
		out.Hour, out.Minute = total/3600000, (total%3600000)/60000
		out.Sec, out.Milli = (total%60000)/1000, total%1000
		out.text = ""
		return one(out), nil
	}
	if t.Prec < PrecDay {
		return nil, evalErrorf("cannot add time to a value with %s precision", precisionName(t.Prec))
	}
	base := t.asTime().Add(time.Duration(ms) * time.Millisecond)
	return one(t.withDateTime(base)), nil
}

// asTime anchors a value on the calendar for arithmetic, substituting 1 for an
// unspecified month or day. Callers guard against precision loss first.
func (t Temporal) asTime() time.Time {
	return time.Date(t.Year, time.Month(max1(t.Month)), max1(t.Day),
		t.Hour, t.Minute, t.Sec, t.Milli*int(time.Millisecond), time.UTC)
}

// withDate copies back the date components, leaving precision and time alone.
func (t Temporal) withDate(base time.Time) Temporal {
	out := t
	out.Year, out.Month, out.Day = base.Year(), int(base.Month()), base.Day()
	out.text = ""
	return out
}

// withDateTime copies back every component.
func (t Temporal) withDateTime(base time.Time) Temporal {
	out := t.withDate(base)
	out.Hour, out.Minute, out.Sec = base.Hour(), base.Minute(), base.Second()
	out.Milli = base.Nanosecond() / int(time.Millisecond)
	return out
}

func precisionName(p Precision) string {
	switch p {
	case PrecYear:
		return "year"
	case PrecMonth:
		return "month"
	case PrecDay:
		return "day"
	case PrecHour:
		return "hour"
	case PrecMinute:
		return "minute"
	case PrecSecond:
		return "second"
	}
	return "unspecified"
}
