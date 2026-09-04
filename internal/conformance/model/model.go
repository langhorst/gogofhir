// Package model defines the compiled FHIR conformance index — the type system,
// search parameters, and compartments of one FHIR release — independently of
// how it is stored or loaded.
//
// It exists as its own package to break a bootstrap cycle: the generator
// (tools/confgen) needs these types to write an index, while the loader
// (internal/conformance) embeds the generated files with go:embed. Were they
// one package, deleting the generated data would stop the generator from
// compiling, leaving no way to regenerate it. Nothing here embeds or reads
// anything.
//
// Callers should use the aliases re-exported by internal/conformance rather
// than importing this package directly.
package model

import (
	"slices"
	"strings"
	"sync"
)

// Release names a supported FHIR release. It doubles as the data/ subdirectory
// name and the -version flag value accepted by tools/confgen.
type Release string

const (
	R4 Release = "r4"
	R5 Release = "r5"
)

// Releases lists every release with a compiled index, in ascending order.
func Releases() []Release { return []Release{R4, R5} }

// Valid reports whether r names a supported release.
func (r Release) Valid() bool { return r == R4 || r == R5 }

// Index is the compiled conformance data for one FHIR release.
//
// gogofhir stores resources as untyped JSON documents rather than generated Go
// structs, so this index supplies every piece of knowledge a generated type
// would otherwise carry: which elements a resource has, how they are typed,
// which are choice elements, what a search parameter selects, and which
// invariants must hold. FHIRPath evaluation, search extraction, and validation
// all consult it.
type Index struct {
	// Release is the short name ("r4"); FHIRVersion is the full version the
	// specification uses in CapabilityStatement.fhirVersion ("4.0.1").
	Release     Release `json:"release"`
	FHIRVersion string  `json:"fhirVersion"`
	PackageID   string  `json:"packageId"`

	// Types covers resources, complex types, and primitive types alike. A FHIR
	// snapshot does not recurse into datatypes — Patient.name is simply typed
	// HumanName — so navigating past an element requires that datatype's own
	// definition, and all of them must be present.
	Types map[string]*TypeDef `json:"types"`

	// SearchParams is keyed by the resource type a parameter is declared on.
	// Parameters common to every resource stay under their declaring base type
	// ("Resource", "DomainResource") rather than being copied to all 160-odd
	// concrete types; SearchParam and SearchParamsFor walk the base chain.
	SearchParams map[string][]*SearchParam `json:"searchParams"`

	// Compartments is keyed by compartment code ("patient", "encounter").
	Compartments map[string]*Compartment `json:"compartments"`

	// ValueSets holds the expansions of the value sets that required and
	// extensible bindings point at, keyed by canonical URL. Only those two
	// strengths are compiled: a preferred or example binding is advice, and
	// carrying thousands of codes to check advice against would be waste.
	ValueSets map[string]*ValueSet `json:"valueSets,omitempty"`

	// Profiles holds StructureDefinitions that constrain a type rather than
	// define one, keyed by canonical URL. Validation resolves meta.profile and
	// the $validate profile parameter through this.
	Profiles map[string]*Profile `json:"profiles,omitempty"`

	once          sync.Once
	resourceTypes []string

	byURLOnce sync.Once
	byURL     map[string]*SearchParam
}

// ValueSet is a value set reduced to the question a validator asks of it: is
// this code in it?
//
// The expansion is computed once at build time, so no runtime terminology
// service is needed for the codes FHIR itself defines -- which is the great
// majority of required bindings.
type ValueSet struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
	// Systems maps a code system URL to the codes it contributes. Grouping by
	// system rather than flattening keeps "that code is not in this system"
	// distinguishable from "that code is not in the value set at all".
	Systems map[string][]string `json:"systems,omitempty"`
	// Unresolvable, when set, says why the value set could not be enumerated
	// offline: it draws on SNOMED CT, LOINC, UCUM, ISO country or currency
	// codes, or another system too large or too encumbered to embed. A binding
	// to one of these is reported as *unchecked*, never as satisfied --
	// claiming otherwise would overstate what the server verified.
	Unresolvable string `json:"unresolvable,omitempty"`

	once  sync.Once
	index map[string]map[string]bool
}

// Contains reports whether a code is in the value set, and whether the value
// set even covers the system it was written in.
//
// The two answers are separate because they mean different things to a reader:
// an unknown system usually means the resource used a different terminology,
// while a known system with an unknown code is a plain mistake.
func (v *ValueSet) Contains(system, code string) (found, knownSystem bool) {
	v.once.Do(func() {
		v.index = make(map[string]map[string]bool, len(v.Systems))
		for url, codes := range v.Systems {
			set := make(map[string]bool, len(codes))
			for _, c := range codes {
				set[c] = true
			}
			v.index[url] = set
		}
	})
	if system == "" {
		// A code written without a system is checked against every system the
		// value set draws on, which is the most a validator can do with it.
		for _, set := range v.index {
			if set[code] {
				return true, true
			}
		}
		return false, false
	}
	set, knownSystem := v.index[system]
	return knownSystem && set[code], knownSystem
}

// Size reports how many codes the expansion holds.
func (v *ValueSet) Size() int {
	n := 0
	for _, codes := range v.Systems {
		n += len(codes)
	}
	return n
}

// Profile is a StructureDefinition whose derivation is "constraint": it narrows
// an existing type rather than defining a new one.
//
// Its elements come from the published snapshot rather than from the
// differential. Generating a snapshot from a differential is one of the harder
// parts of FHIR tooling -- it means replaying a chain of constraints over a
// base type -- and the packages already ship the answer.
type Profile struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
	// Type is the base type constrained: "Observation", or "Extension" for an
	// extension definition.
	Type string `json:"type"`
	// Base is the canonical URL this profile derives from, which may itself be
	// another profile.
	Base string `json:"base,omitempty"`
	// Contexts are where an extension may appear, empty for other profiles.
	Contexts []ProfileContext  `json:"contexts,omitempty"`
	Elements []*ProfileElement `json:"elements,omitempty"`
}

// ProfileContext is one place an extension is allowed.
type ProfileContext struct {
	Type       string `json:"type"` // element, fhirpath, extension
	Expression string `json:"expression"`
}

// ProfileElement is one constrained element of a profile.
type ProfileElement struct {
	// Path is the element path relative to the profiled type, without slice
	// names: "identifier.system".
	Path string `json:"path"`
	// Slice is the slice this element belongs to, from the element id's ":"
	// segments: "mrn", or "mrn/type" for a slice within a slice. Empty for the
	// unsliced element.
	Slice   string    `json:"slice,omitempty"`
	Min     int       `json:"min,omitempty"`
	Max     string    `json:"max,omitempty"`
	Types   []TypeRef `json:"types,omitempty"`
	Binding *Binding  `json:"binding,omitempty"`
	// Fixed and Pattern hold fixed[x] and pattern[x]. A fixed value must match
	// exactly; a pattern must be present but may be accompanied by more.
	Fixed   any `json:"fixed,omitempty"`
	Pattern any `json:"pattern,omitempty"`
	// MustSupport is recorded but not enforced: what "supported" means is
	// defined by the implementation guide, not by the resource.
	MustSupport bool `json:"mustSupport,omitempty"`
	// Slicing is set on the element that introduces slices, and describes how
	// occurrences are assigned to them.
	Slicing *Slicing `json:"slicing,omitempty"`
	// Invariants are constraints the profile adds at this element.
	Invariants []*Invariant `json:"invariants,omitempty"`
}

// Slicing says how the occurrences of a repeating element are divided.
type Slicing struct {
	// Rules is "open", "closed", or "openAtEnd": whether occurrences matching
	// no slice are permitted.
	Rules          string          `json:"rules,omitempty"`
	Ordered        bool            `json:"ordered,omitempty"`
	Discriminators []Discriminator `json:"discriminators,omitempty"`
}

// Discriminator is how one slice is told apart from another.
type Discriminator struct {
	// Type is "value", "pattern", "type", "profile", "exists", or "position".
	Type string `json:"type"`
	// Path is a restricted FHIRPath relative to the sliced element.
	Path string `json:"path"`
}

// TypeDef is one StructureDefinition reduced to what the server needs.
type TypeDef struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // resource, complex-type, primitive-type
	Abstract bool   `json:"abstract,omitempty"`
	// Base is the immediate base type's name ("DomainResource"), empty at the
	// root of the hierarchy.
	Base string `json:"base,omitempty"`

	// Elements are in snapshot (document) order, keyed by a path relative to
	// this type: "name.family", not "Patient.name.family". The type's own root
	// element is omitted; its constraints surface as Invariants.
	Elements []*ElementDef `json:"elements,omitempty"`

	// Invariants are the constraints this type itself declares, each carrying
	// the element path it is evaluated against. Inherited constraints stay on
	// the type that declares them — see Index.Invariants.
	Invariants []*Invariant `json:"invariants,omitempty"`

	// Regex is the lexical form a primitive type's value must take, from the
	// specification's own definition rather than a copy kept here: the release
	// owns its syntax, and a hand-maintained table would drift from it.
	Regex string `json:"regex,omitempty"`

	byPath map[string]*ElementDef
	once   sync.Once

	childrenOnce sync.Once
	children     map[string][]Child
}

// ElementDef is one ElementDefinition from a snapshot.
type ElementDef struct {
	Path string `json:"path"`
	Min  int    `json:"min,omitempty"`
	// Max is "1", "*", or "0" for a prohibited element — the spec's own
	// spelling, kept verbatim rather than normalized to a number so "*" needs
	// no sentinel.
	Max   string    `json:"max,omitempty"`
	Types []TypeRef `json:"types,omitempty"`

	// Choice marks an element whose path ended in "[x]". Path holds the base
	// name with the suffix stripped ("deceased"), and Expansions lists the
	// concrete element names a document may actually use, positionally aligned
	// with Types.
	Choice     bool     `json:"choice,omitempty"`
	Expansions []string `json:"expansions,omitempty"`

	Binding *Binding `json:"binding,omitempty"`

	// ContentReference points at another element whose definition applies here
	// ("#Questionnaire.item"), the spec's device for recursive structures.
	ContentReference string `json:"contentReference,omitempty"`

	Summary  bool `json:"summary,omitempty"`
	Modifier bool `json:"modifier,omitempty"`
}

// IsArray reports whether the element may repeat.
func (e *ElementDef) IsArray() bool {
	return e.Max != "" && e.Max != "0" && e.Max != "1"
}

// Required reports whether at least one occurrence must be present.
func (e *ElementDef) Required() bool { return e.Min > 0 }

// TypeRef is one permitted type for an element.
type TypeRef struct {
	Code string `json:"code"`
	// Targets are the resource types a Reference may point to, already reduced
	// from canonical URLs to bare type names.
	Targets []string `json:"targets,omitempty"`
}

// Binding ties an element to a value set.
type Binding struct {
	Strength string `json:"strength"` // required, extensible, preferred, example
	ValueSet string `json:"valueSet"` // canonical URL, version suffix stripped
}

// Invariant is a constraint expressed in FHIRPath.
type Invariant struct {
	Key      string `json:"key"`
	Severity string `json:"severity"` // error, warning
	Human    string `json:"human"`
	// Expression is FHIRPath source. It is stored unparsed because the FHIRPath
	// engine is built after this index exists; pre-parsing to an AST at
	// generation time is a planned follow-up once that package can be imported.
	Expression string `json:"expression"`
	// Path is the element the invariant is evaluated against, relative to the
	// owning type; empty means the type's root.
	Path string `json:"path,omitempty"`
}

// SearchParam is one SearchParameter reduced to what search needs.
type SearchParam struct {
	Code string `json:"code"`
	// URL is the parameter's canonical identifier. Composite parameters name
	// their components by URL rather than by code, so the lookup is needed to
	// find out what type each component is.
	URL  string   `json:"url,omitempty"`
	Base []string `json:"base"`
	// Type is one of the nine FHIR search parameter types: number, date,
	// string, token, reference, composite, quantity, uri, special.
	Type string `json:"type"`
	// Expression is the FHIRPath selecting the indexed values, stored unparsed
	// for the same reason as Invariant.Expression.
	Expression string   `json:"expression,omitempty"`
	Targets    []string `json:"targets,omitempty"`
	// Components are the constituent parameters of a composite; empty
	// otherwise.
	Components []SearchParamComponent `json:"components,omitempty"`
}

// SearchParamComponent is one leg of a composite search parameter.
type SearchParamComponent struct {
	// Definition is the canonical URL of the parameter this leg reuses.
	Definition string `json:"definition"`
	Expression string `json:"expression"`
}

// Compartment is a CompartmentDefinition: which search parameters place a
// resource in a given compartment.
type Compartment struct {
	Code string `json:"code"`
	// Params maps a resource type to the search parameter codes that link it to
	// this compartment. A resource type present with no codes belongs to the
	// compartment only through other resources.
	Params map[string][]string `json:"params"`
}

// Type returns the definition of a named type.
func (i *Index) Type(name string) (*TypeDef, bool) {
	t, ok := i.Types[name]
	return t, ok
}

// IsResource reports whether name is a concrete (non-abstract) resource type.
func (i *Index) IsResource(name string) bool {
	t, ok := i.Types[name]
	return ok && t.Kind == "resource" && !t.Abstract
}

// ResourceTypes lists every concrete resource type, sorted. This is the set the
// REST layer routes on and the CapabilityStatement advertises.
func (i *Index) ResourceTypes() []string {
	i.once.Do(func() {
		for name, t := range i.Types {
			if t.Kind == "resource" && !t.Abstract {
				i.resourceTypes = append(i.resourceTypes, name)
			}
		}
		slices.Sort(i.resourceTypes)
	})
	return i.resourceTypes
}

// baseChain returns name followed by each of its ancestors, nearest first. It
// stops on an unknown or repeated type, so a malformed index cannot loop.
func (i *Index) baseChain(name string) []string {
	var chain []string
	seen := map[string]bool{}
	for name != "" && !seen[name] {
		seen[name] = true
		chain = append(chain, name)
		t, ok := i.Types[name]
		if !ok {
			break
		}
		name = t.Base
	}
	return chain
}

// SearchParam finds a parameter applicable to a resource type by code,
// including those declared on its base types — "_id" and "_lastUpdated" are
// declared once on Resource, not on all 160-odd concrete types.
func (i *Index) SearchParam(resourceType, code string) (*SearchParam, bool) {
	for _, typ := range i.baseChain(resourceType) {
		for _, p := range i.SearchParams[typ] {
			if p.Code == code {
				return p, true
			}
		}
	}
	return nil, false
}

// SearchParamsFor returns every parameter applicable to a resource type,
// inherited ones included, sorted by code. A parameter redeclared closer to the
// resource shadows the inherited one.
func (i *Index) SearchParamsFor(resourceType string) []*SearchParam {
	seen := map[string]bool{}
	var out []*SearchParam
	for _, typ := range i.baseChain(resourceType) {
		for _, p := range i.SearchParams[typ] {
			if seen[p.Code] {
				continue
			}
			seen[p.Code] = true
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b *SearchParam) int { return strings.Compare(a.Code, b.Code) })
	return out
}

// SearchParamByURL finds a parameter by its canonical URL, which is how a
// composite parameter refers to its components.
func (i *Index) SearchParamByURL(url string) (*SearchParam, bool) {
	i.byURLOnce.Do(func() {
		i.byURL = map[string]*SearchParam{}
		for _, params := range i.SearchParams {
			for _, p := range params {
				if p.URL != "" {
					i.byURL[p.URL] = p
				}
			}
		}
	})
	p, ok := i.byURL[url]
	return p, ok
}

// ValueSet returns a compiled value set by canonical URL. A version suffix is
// ignored: an index carries one release, so the version is implied.
func (i *Index) ValueSet(url string) (*ValueSet, bool) {
	if before, _, found := strings.Cut(url, "|"); found {
		url = before
	}
	vs, ok := i.ValueSets[url]
	return vs, ok
}

// Profile returns a profile by canonical URL.
func (i *Index) Profile(url string) (*Profile, bool) {
	if before, _, found := strings.Cut(url, "|"); found {
		url = before
	}
	p, ok := i.Profiles[url]
	return p, ok
}

// Invariants returns every constraint that applies to a type, walking the base
// chain. Snapshots repeat inherited constraints on every element — ele-1 alone
// appears tens of thousands of times per release — so the index records each
// once on its declaring type and reassembles them here.
func (i *Index) Invariants(typeName string) []*Invariant {
	var out []*Invariant
	for _, typ := range i.baseChain(typeName) {
		if t, ok := i.Types[typ]; ok {
			out = append(out, t.Invariants...)
		}
	}
	return out
}

// Element returns the definition at a path relative to the type, such as
// "name.family". A choice element is addressed by its base name ("deceased"),
// not by an expansion ("deceasedBoolean") — use ExpansionType for those.
func (t *TypeDef) Element(path string) (*ElementDef, bool) {
	t.once.Do(func() {
		t.byPath = make(map[string]*ElementDef, len(t.Elements))
		for _, e := range t.Elements {
			// Slicing can introduce repeated paths; the first is the base
			// definition and the one navigation should see.
			if _, dup := t.byPath[e.Path]; !dup {
				t.byPath[e.Path] = e
			}
		}
	})
	e, ok := t.byPath[path]
	return e, ok
}

// ExpansionType resolves a concrete choice-element name to its type code:
// ExpansionType("deceasedBoolean") yields "boolean". It reports false when name
// is not an expansion of a choice element on this type.
func (t *TypeDef) ExpansionType(name string) (string, bool) {
	for _, e := range t.Elements {
		if !e.Choice {
			continue
		}
		for j, exp := range e.Expansions {
			if exp == name && j < len(e.Types) {
				return e.Types[j].Code, true
			}
		}
	}
	return "", false
}

// ExpandChoice returns the concrete element names for a choice element, in the
// order its types are declared: "deceased" with types boolean and dateTime
// yields deceasedBoolean and deceasedDateTime.
func ExpandChoice(base string, types []TypeRef) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, base+upperFirst(t.Code))
	}
	return out
}

// upperFirst capitalizes the first byte. Every FHIR type code is ASCII, so byte
// indexing is safe and avoids dragging in unicode casing.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
