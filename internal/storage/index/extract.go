// Package index turns resources into search index entries.
//
// Every search parameter carries a FHIRPath expression selecting the values it
// indexes. Extraction evaluates those expressions once per write and produces
// typed entries -- a token needs a system and a code, a date needs a range, a
// reference needs a target type and id -- which the backend writes into one
// table per kind, so a query at read time is an ordinary join rather than a
// scan that parses documents.
//
// It sits beside the storage contract rather than inside it because the two
// are different things: storage.Backend says what a backend must do, and this
// package says how a document becomes rows. A backend needs the entries; the
// REST layer needs the same folding and range rules to build queries that
// match them; neither needs the other.
package index

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/fhirpath"
	"github.com/langhorst/gogofhir/internal/resource"
)

// The parameter's declared type decides how a selected value is indexed, and
// the same value can index differently depending on it: a CodeableConcept
// selected by a token parameter yields one row per coding, while the same
// element selected by a string parameter yields its text.

// Kind names one of the search index tables.
//
// FHIR defines nine search parameter types, and each is indexed differently:
// a token needs a system and a code, a date needs a range, a reference needs a
// target type and id. Splitting them into typed tables -- rather than one
// stringly-typed table or engine-specific JSON indexes -- is what keeps the
// schema portable and the queries ordinary B-tree lookups.
type Kind string

const (
	String    Kind = "string"
	Token     Kind = "token"
	Reference Kind = "reference"
	Date      Kind = "date"
	Quantity  Kind = "quantity"
	URI       Kind = "uri"
	Number    Kind = "number"
	// FullText backs _text and _content. It is not a typed index table
	// like the others but a full-text index, and it is the one place the two
	// backends genuinely diverge: SQLite uses FTS5 and PostgreSQL will use
	// tsvector. Everything else is ordinary B-tree lookups on both.
	FullText Kind = "fulltext"
)

// Entry is one extracted, indexable value.
type Entry struct {
	Code string
	Kind Kind

	// Token
	System string
	Value  string

	// String: Normalized is folded for matching, Exact keeps the original.
	Normalized string
	Exact      string

	// Reference
	RefType string
	RefID   string
	RefURL  string

	// DateLow and DateHigh bound the instant range a date covers, in
	// microseconds since the epoch.
	//
	// Dates are ranges because "2024" denotes a year rather than an instant:
	// storing a point makes every prefix comparison subtly wrong, and it is the
	// single most common way FHIR date search goes astray. Microseconds keep
	// the column an ordinary integer, which both engines index identically.
	DateLow, DateHigh int64

	// NumLow and NumHigh bound a number or quantity, which are ranges for the
	// same reason: a result recorded as 1.1 means [1.05, 1.15).
	NumLow, NumHigh float64

	// Quantity
	QuantitySystem string
	QuantityCode   string

	// URI
	URI string

	// Seq groups the rows that came from the same occurrence of a composite
	// parameter's base expression. Ordinary parameters leave it zero.
	Seq int
}

// Extractor turns resources into index entries.
//
// One extractor is shared by every write, and the caches below are the only
// mutable state in it. They are guarded because concurrent writes are only
// serialized today by SQLite's single connection -- an accident that a
// transaction-scoped store and the PostgreSQL backend both remove.
type Extractor struct {
	idx *conformance.Index

	mu sync.Mutex
	// compiled caches parsed expressions. The R5 index carries 1988 of them and
	// they are evaluated on every write, so parsing them repeatedly would
	// dominate the cost of storing a resource.
	compiled map[string]fhirpath.Expr
	// unusable records expressions that failed to parse, so a broken parameter
	// is reported once rather than on every write.
	unusable map[string]bool
}

// New builds an extractor for a release.
func New(idx *conformance.Index) *Extractor {
	return &Extractor{
		idx:      idx,
		compiled: map[string]fhirpath.Expr{},
		unusable: map[string]bool{},
	}
}

// Extract returns the index entries for a resource.
func (e *Extractor) Extract(node *resource.Node) []Entry {
	resourceType := node.FHIRType()
	ctx := resource.NewContext(e.idx, node)
	ctx.ResolveReference = resolveForIndexing

	var out []Entry
	for _, sp := range e.idx.SearchParamsFor(resourceType) {
		if sp.Type == "composite" {
			out = append(out, e.extractComposite(node, ctx, sp)...)
			continue
		}
		kind, ok := KindFor(sp.Type)
		if !ok {
			// The "special" parameters (_text, _content, near) are implemented
			// by hand rather than by expression.
			continue
		}
		expr := e.expression(sp)
		if expr == nil {
			continue
		}
		values, err := fhirpath.EvalNode(expr, node, ctx)
		if err != nil {
			// A parameter whose expression fails on this particular resource
			// indexes nothing. It is not a reason to reject the write: the
			// document is still valid, and other parameters still index.
			continue
		}
		for _, v := range values {
			out = append(out, entriesFor(sp, kind, v)...)
		}
	}
	return out
}

// extractComposite indexes a composite parameter's components, tagging every
// row from one occurrence of the base expression with the same sequence number.
//
// A composite asks about a single occurrence: code-value-quantity means "the
// code and the value of the same measurement", not "some code and some value
// anywhere in the resource". Without the sequence the two are
// indistinguishable, and the query degenerates into an AND of unrelated
// conditions -- which quietly returns wrong results rather than failing.
func (e *Extractor) extractComposite(node *resource.Node, ctx *fhirpath.Context, sp *conformance.SearchParam) []Entry {
	base := e.expression(sp)
	if base == nil || len(sp.Components) == 0 {
		return nil
	}
	occurrences, err := fhirpath.EvalNode(base, node, ctx)
	if err != nil {
		return nil
	}

	var out []Entry
	for seq, occurrence := range occurrences {
		for i, component := range sp.Components {
			target, ok := e.idx.SearchParamByURL(component.Definition)
			if !ok {
				continue
			}
			kind, ok := KindFor(target.Type)
			if !ok {
				continue
			}
			expr := e.componentExpression(component.Definition, component.Expression)
			if expr == nil {
				continue
			}
			values, err := fhirpath.Eval(expr, fhirpath.Collection{occurrence}, ctx)
			if err != nil {
				continue
			}
			code := CompositeComponentCode(sp.Code, i)
			for _, v := range values {
				for _, entry := range entriesFor(&conformance.SearchParam{Code: code}, kind, v) {
					entry.Seq = seq
					out = append(out, entry)
				}
			}
		}
	}
	return out
}

// CompositeComponentCode is the synthetic code a composite's nth component is
// indexed under. Query building uses the same function, so the two cannot drift.
func CompositeComponentCode(code string, n int) string {
	return code + "$" + strconv.Itoa(n)
}

// componentExpression parses a composite component's expression, cached by the
// component's canonical URL.
func (e *Extractor) componentExpression(definition, expression string) fhirpath.Expr {
	return e.parse("component:"+definition+":"+expression, expression)
}

func (e *Extractor) expression(sp *conformance.SearchParam) fhirpath.Expr {
	return e.parse(strings.Join(sp.Base, ",")+"/"+sp.Code, sp.Expression)
}

// parse returns a cached expression, parsing it on first use. An expression
// that fails to parse is remembered as unusable so a broken parameter costs one
// parse rather than one per write.
func (e *Extractor) parse(key, expression string) fhirpath.Expr {
	e.mu.Lock()
	defer e.mu.Unlock()
	if expr, ok := e.compiled[key]; ok {
		return expr
	}
	if e.unusable[key] {
		return nil
	}
	expr, err := fhirpath.Parse(expression)
	if err != nil {
		e.unusable[key] = true
		return nil
	}
	e.compiled[key] = expr
	return expr
}

// resolveForIndexing answers resolve() during extraction.
//
// Around fifty published search parameters are written as
// "subject.where(resolve() is Patient)" -- the type-narrowing idiom that makes
// Observation.patient distinct from Observation.subject. Without an answer to
// resolve(), every one of them indexes nothing, and the parameters silently
// return no results.
//
// Actually loading the referenced resource would make indexing depend on what
// else is stored, and would leave an index wrong until the target arrives.
// Instead the reference's own type prefix answers the question: "Patient/123"
// is a Patient whether or not that Patient exists. That is all the idiom asks,
// and it keeps a write self-contained.
func resolveForIndexing(ref string) fhirpath.Node {
	typeName, _ := splitReference(ref)
	if typeName == "" {
		return nil
	}
	return referenceStub{typeName: typeName}
}

// referenceStub stands in for a referenced resource during indexing. It knows
// its type and nothing else, which is exactly what "resolve() is X" needs; any
// attempt to navigate into it yields empty rather than a wrong answer.
type referenceStub struct{ typeName string }

func (r referenceStub) TypeName() string                  { return "FHIR." + r.typeName }
func (r referenceStub) String() string                    { return r.typeName }
func (r referenceStub) FHIRType() string                  { return r.typeName }
func (r referenceStub) Children(string) []fhirpath.Node   { return nil }
func (r referenceStub) Primitive() (fhirpath.Value, bool) { return nil, false }

// KindFor maps a search parameter type onto the index it uses. Composite and
// "special" parameters have no index of their own and report false.
func KindFor(paramType string) (Kind, bool) {
	switch paramType {
	case "string":
		return String, true
	case "token":
		return Token, true
	case "reference":
		return Reference, true
	case "date":
		return Date, true
	case "quantity":
		return Quantity, true
	case "uri":
		return URI, true
	case "number":
		return Number, true
	}
	return "", false
}

// entriesFor turns one selected value into index rows.
func entriesFor(sp *conformance.SearchParam, kind Kind, v fhirpath.Value) []Entry {
	node, isNode := v.(fhirpath.Node)
	switch kind {
	case Token:
		if !isNode {
			return tokenEntry(sp.Code, "", v.String())
		}
		return tokenEntries(sp.Code, node)
	case String:
		if !isNode {
			return stringEntry(sp.Code, v.String())
		}
		return stringEntries(sp.Code, node)
	case Reference:
		if !isNode {
			return referenceEntry(sp.Code, v.String())
		}
		return referenceEntries(sp.Code, node)
	case URI:
		return uriEntry(sp.Code, primitiveString(v))
	case Date:
		return dateEntries(sp.Code, v)
	case Number:
		return numberEntries(sp.Code, v)
	case Quantity:
		return quantityEntries(sp.Code, v)
	}
	return nil
}

// ---- token ----

// tokenEntries indexes the several shapes a token parameter can select.
func tokenEntries(code string, node fhirpath.Node) []Entry {
	switch node.FHIRType() {
	case "CodeableConcept":
		var out []Entry
		for _, coding := range node.Children("coding") {
			out = append(out, tokenEntries(code, coding)...)
		}
		// The concept's own text is searchable through the :text modifier.
		if text := childString(node, "text"); text != "" {
			out = append(out, Entry{Code: code, Kind: String,
				Normalized: normalize(text), Exact: text})
		}
		return out
	case "Coding":
		return tokenEntry(code, childString(node, "system"), childString(node, "code"))
	case "Identifier":
		out := tokenEntry(code, childString(node, "system"), childString(node, "value"))
		if t := firstChild(node, "type"); t != nil {
			out = append(out, tokenEntries(code, t)...)
		}
		return out
	case "ContactPoint":
		return tokenEntry(code, childString(node, "system"), childString(node, "value"))
	case "Quantity":
		return tokenEntry(code, childString(node, "system"), childString(node, "code"))
	default:
		// A primitive selected by a token parameter indexes its own value:
		// Patient.gender, Observation.status, and every boolean flag.
		if p, ok := node.Primitive(); ok {
			return tokenEntry(code, "", p.String())
		}
		return nil
	}
}

func tokenEntry(code, system, value string) []Entry {
	if value == "" && system == "" {
		return nil
	}
	return []Entry{{Code: code, Kind: Token, System: system, Value: value}}
}

// ---- string ----

// stringEntries indexes the parts of the composite types a string parameter can
// select. A name is searchable by any of its parts, which is why each becomes
// its own row rather than one concatenated string.
func stringEntries(code string, node fhirpath.Node) []Entry {
	var parts []string
	switch node.FHIRType() {
	case "HumanName":
		parts = childStrings(node, "family", "given", "prefix", "suffix", "text")
	case "Address":
		parts = childStrings(node, "line", "city", "district", "state", "postalCode", "country", "text")
	default:
		if p, ok := node.Primitive(); ok {
			parts = []string{p.String()}
		}
	}
	var out []Entry
	for _, part := range parts {
		out = append(out, stringEntry(code, part)...)
	}
	return out
}

func stringEntry(code, value string) []Entry {
	if value == "" {
		return nil
	}
	return []Entry{{Code: code, Kind: String, Normalized: normalize(value), Exact: value}}
}

// normalize folds a string for matching. FHIR string search is case- and
// accent-insensitive, so both are removed here rather than at query time, where
// it would prevent the index from being used.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if folded, ok := latinFolding[r]; ok {
			b.WriteString(folded)
			continue
		}
		if unicode.IsSpace(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// latinFolding strips diacritics from the Latin-1 range, which covers the
// accented characters that appear in names in practice. A complete
// implementation would decompose and drop combining marks; that needs a Unicode
// table this package does not otherwise require.
var latinFolding = map[rune]string{
	'á': "a", 'à': "a", 'â': "a", 'ä': "a", 'ã': "a", 'å': "a", 'ā': "a",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e", 'ē': "e",
	'í': "i", 'ì': "i", 'î': "i", 'ï': "i", 'ī': "i",
	'ó': "o", 'ò': "o", 'ô': "o", 'ö': "o", 'õ': "o", 'ø': "o", 'ō': "o",
	'ú': "u", 'ù': "u", 'û': "u", 'ü': "u", 'ū': "u",
	'ñ': "n", 'ç': "c", 'ý': "y", 'ÿ': "y", 'ß': "ss", 'æ': "ae", 'œ': "oe",
}

// ---- reference ----

func referenceEntries(code string, node fhirpath.Node) []Entry {
	if node.FHIRType() == "Reference" {
		if ref := childString(node, "reference"); ref != "" {
			return referenceEntry(code, ref)
		}
		// A reference by identifier rather than by URL is indexed as a token so
		// it remains findable.
		if ident := firstChild(node, "identifier"); ident != nil {
			return tokenEntries(code, ident)
		}
		return nil
	}
	if p, ok := node.Primitive(); ok {
		return referenceEntry(code, p.String())
	}
	return nil
}

// referenceEntry splits a reference URL into the type and id a chained search
// joins on, while keeping the original for exact matching.
func referenceEntry(code, ref string) []Entry {
	if ref == "" {
		return nil
	}
	entry := Entry{Code: code, Kind: Reference, RefURL: ref}
	entry.RefType, entry.RefID = splitReference(ref)
	return []Entry{entry}
}

// splitReference pulls "Patient" and "123" out of the reference forms FHIR
// allows: a relative reference, an absolute URL, a versioned reference, or a
// fragment naming a contained resource.
func splitReference(ref string) (typeName, id string) {
	if strings.HasPrefix(ref, "#") {
		return "", ref
	}
	// Drop a version suffix: Patient/123/_history/2.
	if i := strings.Index(ref, "/_history/"); i >= 0 {
		ref = ref[:i]
	}
	parts := strings.Split(strings.TrimSuffix(ref, "/"), "/")
	if len(parts) < 2 {
		return "", ref
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

// ---- uri ----

func uriEntry(code, value string) []Entry {
	if value == "" {
		return nil
	}
	return []Entry{{Code: code, Kind: URI, URI: value}}
}

// ---- date ----

func dateEntries(code string, v fhirpath.Value) []Entry {
	if node, ok := v.(fhirpath.Node); ok {
		switch node.FHIRType() {
		case "Period":
			low, hasLow := temporalOf(firstChild(node, "start"))
			high, hasHigh := temporalOf(firstChild(node, "end"))
			entry := Entry{Code: code, Kind: Date,
				DateLow: math.MinInt64, DateHigh: math.MaxInt64}
			if hasLow {
				entry.DateLow, _ = temporalRange(low)
			}
			if hasHigh {
				_, entry.DateHigh = temporalRange(high)
			}
			if !hasLow && !hasHigh {
				return nil
			}
			return []Entry{entry}
		case "Timing":
			var out []Entry
			for _, when := range node.Children("event") {
				out = append(out, dateEntries(code, when)...)
			}
			return out
		}
	}
	t, ok := temporalOf(v)
	if !ok {
		return nil
	}
	low, high := temporalRange(t)
	return []Entry{{Code: code, Kind: Date, DateLow: low, DateHigh: high}}
}

// temporalOf extracts a temporal value from a node or value, if it is one.
func temporalOf(v fhirpath.Value) (fhirpath.Temporal, bool) {
	if v == nil {
		return fhirpath.Temporal{}, false
	}
	if node, ok := v.(fhirpath.Node); ok {
		p, isPrim := node.Primitive()
		if !isPrim {
			return fhirpath.Temporal{}, false
		}
		v = p
	}
	t, ok := v.(fhirpath.Temporal)
	return t, ok
}

// temporalRange converts a partial date to the instant range it covers, so that
// "2024" matches anything in that year.
func temporalRange(t fhirpath.Temporal) (low, high int64) {
	month, day := 1, 1
	hour, minute, sec, milli := 0, 0, 0, 0
	if t.Prec >= fhirpath.PrecMonth {
		month = t.Month
	}
	if t.Prec >= fhirpath.PrecDay {
		day = t.Day
	}
	if t.Prec >= fhirpath.PrecHour {
		hour = t.Hour
	}
	if t.Prec >= fhirpath.PrecMinute {
		minute = t.Minute
	}
	if t.Prec >= fhirpath.PrecSecond {
		sec, milli = t.Sec, t.Milli
	}
	zone := time.UTC
	if t.HasZone {
		zone = time.FixedZone("", t.ZoneOffsetMinutes*60)
	}
	start := time.Date(t.Year, time.Month(month), day, hour, minute, sec,
		milli*int(time.Millisecond), zone)

	// The end of the range is the start of the next unit at this precision.
	var end time.Time
	switch {
	case t.Prec >= fhirpath.PrecSecond:
		end = start.Add(time.Millisecond)
	case t.Prec >= fhirpath.PrecMinute:
		end = start.Add(time.Minute)
	case t.Prec >= fhirpath.PrecHour:
		end = start.Add(time.Hour)
	case t.Prec >= fhirpath.PrecDay:
		end = start.AddDate(0, 0, 1)
	case t.Prec >= fhirpath.PrecMonth:
		end = start.AddDate(0, 1, 0)
	default:
		end = start.AddDate(1, 0, 0)
	}
	return start.UnixMicro(), end.Add(-time.Microsecond).UnixMicro()
}

// ---- number and quantity ----

func numberEntries(code string, v fhirpath.Value) []Entry {
	value, scale, ok := decimalOf(v)
	if !ok {
		return nil
	}
	low, high := implicitRange(value, scale)
	return []Entry{{Code: code, Kind: Number, NumLow: low, NumHigh: high}}
}

func quantityEntries(code string, v fhirpath.Value) []Entry {
	node, ok := v.(fhirpath.Node)
	if !ok {
		return nil
	}
	value, scale, ok := decimalOf(firstChild(node, "value"))
	if !ok {
		return nil
	}
	low, high := implicitRange(value, scale)
	return []Entry{{
		Code: code, Kind: Quantity,
		NumLow: low, NumHigh: high,
		QuantitySystem: childString(node, "system"),
		QuantityCode:   firstNonEmpty(childString(node, "code"), childString(node, "unit")),
	}}
}

// decimalOf reads a numeric value and the number of decimal places it was
// written with.
func decimalOf(v fhirpath.Value) (value float64, scale int, ok bool) {
	if v == nil {
		return 0, 0, false
	}
	if node, isNode := v.(fhirpath.Node); isNode {
		p, isPrim := node.Primitive()
		if !isPrim {
			return 0, 0, false
		}
		v = p
	}
	switch x := v.(type) {
	case fhirpath.Integer:
		return float64(x), 0, true
	case fhirpath.Decimal:
		f, _ := x.Rat().Float64()
		s := x.Scale()
		if s < 0 {
			s = 0
		}
		return f, s, true
	}
	f, err := strconv.ParseFloat(v.String(), 64)
	return f, 0, err == nil
}

// implicitRange is the interval a written number denotes: 1.1 means anything
// that would round to 1.1, so half a unit in the last written place either way.
// Indexing the point alone would make "value=1.1" miss a stored 1.10001.
func implicitRange(value float64, scale int) (low, high float64) {
	half := 0.5 * math.Pow(10, -float64(scale))
	return value - half, value + half
}

// ---- small helpers over nodes ----

func firstChild(node fhirpath.Node, name string) fhirpath.Node {
	children := node.Children(name)
	if len(children) == 0 {
		return nil
	}
	return children[0]
}

func childString(node fhirpath.Node, name string) string {
	child := firstChild(node, name)
	if child == nil {
		return ""
	}
	return primitiveString(child)
}

// childStrings collects every value of several named children, flattening
// repeats.
func childStrings(node fhirpath.Node, names ...string) []string {
	var out []string
	for _, name := range names {
		for _, child := range node.Children(name) {
			if s := primitiveString(child); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func primitiveString(v fhirpath.Value) string {
	if v == nil {
		return ""
	}
	if node, ok := v.(fhirpath.Node); ok {
		p, isPrim := node.Primitive()
		if !isPrim {
			return ""
		}
		return p.String()
	}
	return v.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// DateRange converts a FHIR date, dateTime, or instant written at any precision
// into the instant range it denotes, for building a query.
//
// It is the same conversion extraction uses, exported so the search layer can
// turn a query value into bounds without duplicating the precision rules.
func DateRange(value string) (low, high int64, err error) {
	t, err := fhirpath.ParseFHIRTemporal("dateTime", value)
	if err != nil {
		return 0, 0, err
	}
	low, high = temporalRange(t)
	return low, high, nil
}

// NumberRange converts a written number into the interval it denotes, so a
// query for 1.1 matches a stored 1.10001 but not 1.2.
func NumberRange(text string) (low, high float64, err error) {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, 0, err
	}
	scale := 0
	if i := strings.IndexByte(text, '.'); i >= 0 {
		scale = len(text) - i - 1
	}
	low, high = implicitRange(value, scale)
	return low, high, nil
}

// Normalize folds a string the way the index does, so a query value and a
// stored value are compared on equal terms.
func Normalize(s string) string { return normalize(s) }

// FullText returns the two bodies the full-text index holds: the rendered
// narrative that _text searches, and every text value in the resource, which is
// what _content searches.
//
// Both are plain text. The narrative arrives as XHTML, so its markup is
// stripped -- indexing tag names would make "div" match every resource.
func (e *Extractor) FullText(node *resource.Node) (narrative, content string) {
	var narrativeParts, contentParts []string
	for _, text := range node.Children("text") {
		for _, div := range text.Children("div") {
			if p, ok := div.Primitive(); ok {
				narrativeParts = append(narrativeParts, stripMarkup(p.String()))
			}
		}
	}
	collectText(node, &contentParts, 0)
	return strings.Join(narrativeParts, " "), strings.Join(contentParts, " ")
}

// collectText walks a resource gathering every primitive string value.
//
// Numbers, booleans, and dates are skipped: they are searchable through their
// own typed indexes, and folding them into free text produces matches nobody
// asked for -- a search for "1974" hitting every resource with that birth year
// as well as every one with it in a note.
func collectText(node fhirpath.Node, out *[]string, depth int) {
	if depth > 40 {
		return
	}
	if value, ok := node.Primitive(); ok {
		if s, isString := value.(fhirpath.String_); isString {
			if text := strings.TrimSpace(string(s)); text != "" {
				*out = append(*out, stripMarkup(text))
			}
		}
		return
	}
	for _, child := range node.Children("") {
		collectText(child, out, depth+1)
	}
}

// stripMarkup removes XML tags and collapses whitespace, leaving the words.
func stripMarkup(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
			b.WriteByte(' ')
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
