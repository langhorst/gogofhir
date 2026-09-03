// Command confgen compiles a published FHIR conformance package into the
// compact index that internal/conformance embeds.
//
// The published packages are large and mostly irrelevant to a server: R5 core
// ships 2969 JSON files, of which the great majority are examples, narrative,
// and terminology the runtime never consults. confgen keeps the type system,
// the search parameters, the compartments, and the invariants, and discards the
// rest.
//
// Its output is committed, so building gogofhir requires neither the packages
// nor a network. `make gen-check` fails the build if the committed output
// drifts from what the pinned packages produce.
//
// Usage:
//
//	go run ./tools/confgen -version r5 -src third_party/packages -out internal/conformance/data
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance/model"
)

func main() {
	release := flag.String("version", "", "FHIR release to compile (r4, r5)")
	src := flag.String("src", "third_party/packages", "directory holding the vendored packages")
	out := flag.String("out", "internal/conformance/data", "directory to write compiled indexes into")
	flag.Parse()

	if err := run(model.Release(*release), *src, *out); err != nil {
		fmt.Fprintf(os.Stderr, "confgen: %v\n", err)
		os.Exit(1)
	}
}

func run(release model.Release, src, out string) error {
	if !release.Valid() {
		return fmt.Errorf("unknown release %q (have %v)", release, model.Releases())
	}
	pkgDir := filepath.Join(src, string(release))
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return fmt.Errorf("reading package %s (run `make vendor`): %w", pkgDir, err)
	}
	// The terminology package alongside it, if vendored. R5 moved most of its
	// code systems out of core and into THO, so without it two thirds of R5's
	// required bindings would be uncheckable only because the codes live in a
	// package that was not read.
	termDir := filepath.Join(src, string(release)+"-terminology")
	termEntries, _ := os.ReadDir(termDir)

	idx := &model.Index{
		Release:      release,
		Types:        map[string]*model.TypeDef{},
		SearchParams: map[string][]*model.SearchParam{},
		Compartments: map[string]*model.Compartment{},
		Profiles:     map[string]*model.Profile{},
	}

	terms := newTerminology()
	var pending []map[string]any
	var stats struct {
		types, params, compartments, invariants, profiles, valueSets, unresolvable, skipped int
	}

	// Terminology is read first so a core definition of the same URL wins: the
	// release's own value sets are authoritative for the release.
	for _, e := range termEntries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if !strings.HasPrefix(name, "ValueSet-") && !strings.HasPrefix(name, "CodeSystem-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(termDir, name))
		if err != nil {
			return err
		}
		var res map[string]any
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}
		terms.add(res)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		// The filename prefix is the resource type, which is enough to skip the
		// thousands of examples and terminology resources without parsing them.
		switch {
		case strings.HasPrefix(name, "StructureDefinition-"),
			strings.HasPrefix(name, "SearchParameter-"),
			strings.HasPrefix(name, "CompartmentDefinition-"),
			strings.HasPrefix(name, "ValueSet-"),
			strings.HasPrefix(name, "CodeSystem-"):
		default:
			continue
		}

		var res map[string]any
		raw, err := os.ReadFile(filepath.Join(pkgDir, name))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("parsing %s: %w", name, err)
		}

		switch str(res, "resourceType") {
		case "ValueSet", "CodeSystem":
			// Terminology is held aside: which value sets are worth expanding
			// is only known once every binding has been seen.
			terms.add(res)
		case "StructureDefinition":
			if str(res, "derivation") == "constraint" {
				// Profiles are held until the type system is complete: what a
				// profile constrains is only visible against the type it
				// constrains.
				pending = append(pending, res)
				continue
			}
			td, keep := compileType(res)
			if !keep {
				stats.skipped++
				continue
			}
			if prev, dup := idx.Types[td.Name]; dup {
				return fmt.Errorf("%s: duplicate definition of type %q (kind %s and %s)",
					name, td.Name, prev.Kind, td.Kind)
			}
			idx.Types[td.Name] = td
			stats.types++
			stats.invariants += len(td.Invariants)
		case "SearchParameter":
			sp, keep := compileSearchParam(res)
			if !keep {
				stats.skipped++
				continue
			}
			for _, base := range sp.Base {
				idx.SearchParams[base] = append(idx.SearchParams[base], sp)
			}
			stats.params++
		case "CompartmentDefinition":
			c := compileCompartment(res)
			if c == nil {
				stats.skipped++
				continue
			}
			if _, dup := idx.Compartments[c.Code]; dup {
				return fmt.Errorf("%s: duplicate compartment %q", name, c.Code)
			}
			idx.Compartments[c.Code] = c
			stats.compartments++
		}
	}

	if stats.types == 0 {
		return fmt.Errorf("no type definitions found in %s", pkgDir)
	}
	for _, res := range pending {
		profile, keep := compileProfile(res, idx)
		if !keep {
			stats.skipped++
			continue
		}
		if _, dup := idx.Profiles[profile.URL]; dup {
			return fmt.Errorf("duplicate profile %q", profile.URL)
		}
		idx.Profiles[profile.URL] = profile
		stats.profiles++
	}

	idx.ValueSets = terms.compile(boundValueSets(idx))
	stats.valueSets = len(idx.ValueSets)
	for _, vs := range idx.ValueSets {
		if vs.Unresolvable != "" {
			stats.unresolvable++
		}
	}
	idx.FHIRVersion, idx.PackageID = packageMeta(pkgDir, idx)

	// Sort every slice so the committed output is byte-stable across runs and
	// filesystem orderings — a generated file whose diff is noise is a
	// generated file nobody reviews.
	for _, params := range idx.SearchParams {
		slices.SortFunc(params, func(a, b *model.SearchParam) int {
			return strings.Compare(a.Code, b.Code)
		})
	}

	dir := filepath.Join(out, string(release))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Indented, so the diff of a regeneration is reviewable line by line.
	encoded, err := json.MarshalIndent(idx, "", " ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(dir, "index.json"), encoded, 0o644); err != nil {
		return err
	}

	fmt.Printf("  %s (%s): %d types, %d search parameters, %d compartments, %d invariants,\n"+
		"      %d profiles, %d value sets (%d unresolvable offline) (%d resources skipped), %d KiB\n",
		release, idx.FHIRVersion, stats.types, stats.params, stats.compartments, stats.invariants,
		stats.profiles, stats.valueSets, stats.unresolvable, stats.skipped, len(encoded)/1024)
	return nil
}

// boundValueSets collects the value sets that required and extensible bindings
// reach, mapped to the strongest binding that reaches each.
//
// Only those two strengths are worth expanding: a preferred or example binding
// is advice a validator does not enforce, and carrying its codes would add
// weight for nothing. Profiles are included, since a profile may bind an
// element the base type left open.
func boundValueSets(idx *model.Index) map[string]string {
	wanted := map[string]string{}
	note := func(b *model.Binding) {
		if b == nil || b.ValueSet == "" {
			return
		}
		switch b.Strength {
		case "required":
			wanted[b.ValueSet] = "required"
		case "extensible":
			if wanted[b.ValueSet] == "" {
				wanted[b.ValueSet] = "extensible"
			}
		}
	}
	for _, t := range idx.Types {
		for _, el := range t.Elements {
			note(el.Binding)
		}
	}
	for _, p := range idx.Profiles {
		for _, el := range p.Elements {
			note(el.Binding)
		}
	}
	return wanted
}

// compileType reduces a StructureDefinition to a TypeDef, reporting false for
// definitions the server has no use for.
func compileType(res map[string]any) (*model.TypeDef, bool) {
	kind := str(res, "kind")
	switch kind {
	case "resource", "complex-type", "primitive-type":
	default:
		// "logical" models describe things that are not FHIR resources.
		return nil, false
	}
	// Profiles (derivation "constraint") constrain a base type rather than
	// defining one. They matter for profile validation at M6 and are compiled
	// separately then; the type system is built only from specializations.
	if str(res, "derivation") == "constraint" {
		return nil, false
	}
	snapshot, ok := res["snapshot"].(map[string]any)
	if !ok {
		return nil, false
	}
	elements, ok := snapshot["element"].([]any)
	if !ok || len(elements) == 0 {
		return nil, false
	}

	name := str(res, "type")
	if name == "" {
		return nil, false
	}
	td := &model.TypeDef{
		Name:     name,
		Kind:     kind,
		Abstract: boolean(res, "abstract"),
		Base:     lastURLSegment(str(res, "baseDefinition")),
	}
	ownURL := str(res, "url")

	for _, raw := range elements {
		el, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := str(el, "path")
		if path == "" {
			continue
		}
		rel := strings.TrimPrefix(path, name)
		rel = strings.TrimPrefix(rel, ".")
		// rel == "" is the type's own root element: it carries no field of its
		// own, only type-level constraints.

		td.Invariants = append(td.Invariants, compileInvariants(el, rel, ownURL)...)
		if rel == "" {
			continue
		}
		if kind == "primitive-type" && rel == "value" {
			td.Regex = valueRegex(el)
		}
		td.Elements = append(td.Elements, compileElement(el, rel))
	}
	return td, true
}

// valueRegex reads the lexical pattern a primitive type's value must match.
//
// The specification carries it as an extension on the value element's type
// rather than as a field, which is why this looks the way it does. Taking it
// from the definitions rather than hard-coding a table keeps the validator's
// idea of a valid date in step with the release it is serving.
func valueRegex(el map[string]any) string {
	for _, item := range sliceOf(el["type"]) {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for _, raw := range sliceOf(t["extension"]) {
			ext, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if str(ext, "url") == "http://hl7.org/fhir/StructureDefinition/regex" {
				return str(ext, "valueString")
			}
		}
	}
	return ""
}

func compileElement(el map[string]any, rel string) *model.ElementDef {
	e := &model.ElementDef{
		Path:             rel,
		Min:              integer(el, "min"),
		Max:              str(el, "max"),
		Types:            compileTypes(el),
		ContentReference: str(el, "contentReference"),
		Summary:          boolean(el, "isSummary"),
		Modifier:         boolean(el, "isModifier"),
	}
	if base, found := strings.CutSuffix(rel, "[x]"); found {
		e.Choice = true
		e.Path = base
		// Expansions are element names as they appear in a document, so they
		// are built from the final path segment only. Building them from the
		// full relative path yields "parameter.valueString" where the document
		// says "valueString", which silently breaks every nested choice element
		// while leaving top-level ones like Patient.deceased[x] working.
		e.Expansions = model.ExpandChoice(lastPathSegment(base), e.Types)
	}
	if b, ok := el["binding"].(map[string]any); ok {
		if vs := str(b, "valueSet"); vs != "" {
			e.Binding = &model.Binding{
				Strength: str(b, "strength"),
				// Canonicals carry a "|version" suffix that would defeat lookup
				// by URL; the version is implied by the release anyway.
				ValueSet: strings.SplitN(vs, "|", 2)[0],
			}
		}
	}
	return e
}

func compileTypes(el map[string]any) []model.TypeRef {
	raw, ok := el["type"].([]any)
	if !ok {
		return nil
	}
	var out []model.TypeRef
	for _, item := range raw {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		code := str(t, "code")
		if code == "" {
			continue
		}
		ref := model.TypeRef{Code: resolveTypeCode(t, code)}
		for _, tp := range strSlice(t, "targetProfile") {
			ref.Targets = append(ref.Targets, lastURLSegment(tp))
		}
		out = append(out, ref)
	}
	return out
}

// compileInvariants keeps only constraints this StructureDefinition itself
// declares. A snapshot repeats every inherited constraint at every element —
// ele-1 alone appears on tens of thousands of elements across a release — so
// storing them flat would bloat the index for no gain. Each stays recorded once
// on the type that declares it, and a validator finds it by walking the base
// chain, which it must do in any case.
func compileInvariants(el map[string]any, rel, ownURL string) []*model.Invariant {
	raw, ok := el["constraint"].([]any)
	if !ok {
		return nil
	}
	var out []*model.Invariant
	for _, item := range raw {
		c, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if src := str(c, "source"); src != "" && src != ownURL {
			continue
		}
		expr := str(c, "expression")
		if expr == "" {
			// A constraint with no computable expression (a handful are prose
			// only) cannot be checked and is not worth carrying.
			continue
		}
		out = append(out, &model.Invariant{
			Key:        str(c, "key"),
			Severity:   str(c, "severity"),
			Human:      str(c, "human"),
			Expression: expr,
			Path:       rel,
		})
	}
	return out
}

func compileSearchParam(res map[string]any) (*model.SearchParam, bool) {
	code := str(res, "code")
	base := strSlice(res, "base")
	typ := str(res, "type")
	if code == "" || typ == "" || len(base) == 0 {
		return nil, false
	}
	sp := &model.SearchParam{
		Code:       code,
		URL:        str(res, "url"),
		Base:       base,
		Type:       typ,
		Expression: str(res, "expression"),
	}
	for _, t := range strSlice(res, "target") {
		sp.Targets = append(sp.Targets, lastURLSegment(t))
	}
	if comps, ok := res["component"].([]any); ok {
		for _, item := range comps {
			c, ok := item.(map[string]any)
			if !ok {
				continue
			}
			sp.Components = append(sp.Components, model.SearchParamComponent{
				Definition: str(c, "definition"),
				Expression: str(c, "expression"),
			})
		}
	}
	// A parameter with no expression selects nothing and cannot be indexed.
	// The "special" ones (_text, _content, near) are implemented by hand rather
	// than by expression, so they are kept regardless.
	if sp.Expression == "" && typ != "special" {
		return nil, false
	}
	return sp, true
}

// compileCompartment reduces a CompartmentDefinition, rejecting the examples
// that ship alongside the real ones. The core packages include
// CompartmentDefinition-example.json, which declares code "Device" and would
// otherwise collide with — and, depending on directory order, silently replace
// — the genuine Device compartment. A normative definition's canonical URL ends
// with its own code; the example's ends with "example", which is the tell.
func compileCompartment(res map[string]any) *model.Compartment {
	code := str(res, "code")
	if code == "" {
		return nil
	}
	if seg := lastURLSegment(str(res, "url")); !strings.EqualFold(seg, code) {
		return nil
	}
	c := &model.Compartment{Code: code, Params: map[string][]string{}}
	resources, _ := res["resource"].([]any)
	for _, item := range resources {
		r, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if typ := str(r, "code"); typ != "" {
			c.Params[typ] = strSlice(r, "param")
		}
	}
	return c
}

// packageMeta reads the package's own manifest for the FHIR version, falling
// back to the version stamped on the definitions when a mirror omits it.
func packageMeta(pkgDir string, idx *model.Index) (fhirVersion, packageID string) {
	raw, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err == nil {
		var manifest map[string]any
		if json.Unmarshal(raw, &manifest) == nil {
			packageID = str(manifest, "name")
			if list := strSlice(manifest, "fhir-version-list"); len(list) > 0 {
				fhirVersion = list[0]
			}
			if v := str(manifest, "version"); fhirVersion == "" {
				fhirVersion = v
			}
		}
	}
	if fhirVersion == "" {
		// Every core definition carries fhirVersion; any one of them will do.
		fhirVersion = "unknown"
	}
	return fhirVersion, packageID
}

// ---- small typed accessors over decoded JSON ----

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func boolean(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

func integer(m map[string]any, key string) int {
	f, _ := m[key].(float64)
	return int(f)
}

func strSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// lastPathSegment returns the final dot-separated component of an element path.
func lastPathSegment(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// systemTypeCodes maps the FHIRPath System type URLs that appear as type codes
// on a few elements -- Element.id and Extension.url among them -- onto the
// equivalent FHIR primitive.
//
// The specification uses those URLs to say "this is a System type, not a FHIR
// one". Carrying the URL through would leave those elements with a type no
// lookup can resolve, so they would stop behaving as primitives at all.
var systemTypeCodes = map[string]string{
	"http://hl7.org/fhirpath/System.String":   "string",
	"http://hl7.org/fhirpath/System.Boolean":  "boolean",
	"http://hl7.org/fhirpath/System.Integer":  "integer",
	"http://hl7.org/fhirpath/System.Decimal":  "decimal",
	"http://hl7.org/fhirpath/System.Date":     "date",
	"http://hl7.org/fhirpath/System.DateTime": "dateTime",
	"http://hl7.org/fhirpath/System.Time":     "time",
}

// resolveTypeCode decides an element's FHIR type from its declared code.
//
// R5 types a handful of elements by their FHIRPath System type -- Resource.id
// and Element.id are System.String -- and names the FHIR type they really are
// in an extension alongside. Reading that extension is what keeps Resource.id
// an "id" rather than a plain string, which matters because "id" has a lexical
// pattern and "string" does not.
func resolveTypeCode(t map[string]any, code string) string {
	if fhirType := typeExtension(t); fhirType != "" {
		return fhirType
	}
	if mapped, ok := systemTypeCodes[code]; ok {
		return mapped
	}
	return code
}

func typeExtension(t map[string]any) string {
	const url = "http://hl7.org/fhir/StructureDefinition/structuredefinition-fhir-type"
	for _, raw := range sliceOf(t["extension"]) {
		ext, ok := raw.(map[string]any)
		if !ok || str(ext, "url") != url {
			continue
		}
		if value := str(ext, "valueUrl"); value != "" {
			return value
		}
		if value := str(ext, "valueString"); value != "" {
			return value
		}
	}
	return ""
}

// lastURLSegment reduces a canonical URL to the type name it ends with:
// "http://hl7.org/fhir/StructureDefinition/Patient" becomes "Patient". A value
// that is already a bare name passes through.
func lastURLSegment(url string) string {
	if url == "" {
		return ""
	}
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[i+1:]
	}
	return url
}
