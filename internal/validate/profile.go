package validate

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
)

// Profile validation.
//
// A profile narrows a type: it tightens cardinality, fixes values, binds
// elements to other value sets, and divides repeating elements into slices.
// Checking one means walking the document a second time against the profile's
// element list rather than the base type's.
//
// Cardinality is the part that is easy to get quietly wrong. A profile saying
// Observation.component.code is required means required *in each component*,
// not that some component somewhere has one -- so occurrences are grouped by
// their parent before they are counted.

// occurrence is one node in the document together with where it lives.
type occurrence struct {
	node *resource.Node
	path string
}

// group is one parent and the occurrences of a given element it holds.
type group struct {
	parent   occurrence
	children []occurrence
	// path is the element's location under this parent, without an index.
	path string
}

// checkProfile validates a resource against one profile.
func (r *run) checkProfile(node *resource.Node, path, url string) {
	profile, ok := r.v.idx.Profile(url)
	if !ok {
		// Saying nothing would let a reader take the resource as conforming.
		r.report(SeverityInformation, "not-supported", path,
			"this server has no definition of the profile %s, so conformance to it was NOT checked", url)
		return
	}
	if profile.Type != node.FHIRType() {
		r.report(SeverityError, "structure", path,
			"the profile %s constrains %s, but this is a %s", url, profile.Type, node.FHIRType())
		return
	}

	root := occurrence{node: node, path: path}
	for _, el := range profile.Elements {
		if el.Path == "" {
			for _, inv := range el.Invariants {
				r.evaluate(node, path, inv, node)
			}
			continue
		}
		if el.Slice != "" {
			// Slice members are checked through the element that introduces the
			// slicing, which is where the occurrences are assigned.
			continue
		}
		groups := groupsAt(root, el.Path)
		for _, g := range groups {
			r.checkProfileElement(profile, el, g, url)
		}
	}

	if nested := nestedSlicing(profile); len(nested) > 0 {
		r.report(SeverityInformation, "not-supported", path,
			"the profile %s slices within slices (%s), which this server does not check",
			shortName(profile, url), strings.Join(nested, "; "))
	}
}

// checkProfileElement applies one profile constraint to one parent's
// occurrences of an element.
func (r *run) checkProfileElement(profile *conformance.Profile, el *conformance.ProfileElement, g group, url string) {
	present := len(g.children)
	if present < el.Min {
		r.report(SeverityError, "required", g.path,
			"the profile %s requires at least %d %s, but %d are present",
			shortName(profile, url), el.Min, el.Path, present)
	}
	if upper, err := strconv.Atoi(el.Max); err == nil && present > upper {
		r.report(SeverityError, "structure", g.path,
			"the profile %s allows at most %d %s, but %d are present",
			shortName(profile, url), upper, el.Path, present)
	}

	for _, child := range g.children {
		if el.Fixed != nil && !equalValue(child.node.Raw(), el.Fixed) {
			r.report(SeverityError, "value", child.path,
				"the profile %s fixes this to %s", shortName(profile, url), render(el.Fixed))
		}
		if el.Pattern != nil && !matchesPattern(child.node.Raw(), el.Pattern) {
			r.report(SeverityError, "value", child.path,
				"the profile %s requires this to match the pattern %s",
				shortName(profile, url), render(el.Pattern))
		}
		if el.Binding != nil {
			r.checkProfileBinding(child, el, url)
		}
		for _, inv := range el.Invariants {
			r.evaluate(child.node, child.path, inv, r.node)
		}
	}

	if el.Slicing != nil {
		r.checkSlices(profile, el, g, url)
	}
}

// checkProfileBinding applies a binding the profile added or tightened.
func (r *run) checkProfileBinding(child occurrence, el *conformance.ProfileElement, url string) {
	binding := el.Binding
	switch binding.Strength {
	case "required", "extensible":
	default:
		return
	}
	valueSet, known := r.v.idx.ValueSet(binding.ValueSet)
	if !known {
		r.reportUnchecked(child.path, binding, "this server has no expansion of %s", binding.ValueSet)
		return
	}
	if valueSet.Unresolvable != "" {
		r.reportUnchecked(child.path, binding, "%s", valueSet.Unresolvable)
		return
	}
	for _, coded := range codings(child.node, child.path) {
		r.checkCode(coded, binding, valueSet)
	}
	_ = url
}

// ---- slicing ----

// checkSlices assigns a parent's occurrences of a sliced element to slices and
// checks each slice's own cardinality.
func (r *run) checkSlices(profile *conformance.Profile, el *conformance.ProfileElement, g group, url string) {
	slices := sliceNames(profile, el.Path)
	if len(slices) == 0 {
		return
	}
	if len(el.Slicing.Discriminators) == 0 {
		r.report(SeverityInformation, "not-supported", g.path,
			"the profile %s slices %s without a discriminator, so its slices were NOT checked",
			shortName(profile, url), el.Path)
		return
	}

	counts := map[string]int{}
	unmatched := 0
	for _, child := range g.children {
		matched := ""
		for _, name := range slices {
			ok, decidable := r.matchesSlice(profile, el, name, child)
			if !decidable {
				// One undecidable discriminator makes the whole assignment
				// unsafe: a wrong slice is worse than no slice.
				matched = ""
				unmatched = -1
				break
			}
			if ok {
				matched = name
				break
			}
		}
		if unmatched < 0 {
			r.report(SeverityInformation, "not-supported", g.path,
				"the slices of %s in %s use a discriminator this server cannot evaluate, so they were NOT checked",
				el.Path, shortName(profile, url))
			return
		}
		if matched == "" {
			unmatched++
			continue
		}
		counts[matched]++
	}

	for _, name := range slices {
		member := sliceElement(profile, el.Path, name)
		if member == nil {
			continue
		}
		present := counts[name]
		if present < member.Min {
			r.report(SeverityError, "required", g.path,
				"the profile %s requires at least %d %s matching the slice %q, but %d do",
				shortName(profile, url), member.Min, el.Path, name, present)
		}
		if upper, err := strconv.Atoi(member.Max); err == nil && present > upper {
			r.report(SeverityError, "structure", g.path,
				"the profile %s allows at most %d %s matching the slice %q, but %d do",
				shortName(profile, url), upper, el.Path, name, present)
		}
	}
	if unmatched > 0 && el.Slicing.Rules == "closed" {
		r.report(SeverityError, "structure", g.path,
			"the profile %s closes the slicing of %s, but %d occurrence(s) match no slice",
			shortName(profile, url), el.Path, unmatched)
	}
}

// matchesSlice decides whether an occurrence belongs to a slice, and whether
// that decision could be made at all.
func (r *run) matchesSlice(profile *conformance.Profile, el *conformance.ProfileElement,
	name string, child occurrence) (matched, decidable bool) {
	for _, d := range el.Slicing.Discriminators {
		switch d.Type {
		case "value", "pattern":
			member := discriminatorElement(profile, joinPath(el.Path, d.Path), name)
			if member == nil || (member.Fixed == nil && member.Pattern == nil) {
				return false, false
			}
			values := valuesAt(child, d.Path)
			if len(values) == 0 {
				return false, true
			}
			want := member.Fixed
			exact := true
			if want == nil {
				want, exact = member.Pattern, false
			}
			hit := false
			for _, v := range values {
				if exact && equalValue(v.node.Raw(), want) {
					hit = true
				}
				if !exact && matchesPattern(v.node.Raw(), want) {
					hit = true
				}
			}
			if !hit {
				return false, true
			}

		case "type":
			member := discriminatorElement(profile, joinPath(el.Path, d.Path), name)
			if member == nil || len(member.Types) != 1 {
				return false, false
			}
			values := valuesAt(child, d.Path)
			if len(values) == 0 {
				return false, true
			}
			for _, v := range values {
				if v.node.FHIRType() != member.Types[0].Code {
					return false, true
				}
			}

		case "exists":
			member := discriminatorElement(profile, joinPath(el.Path, d.Path), name)
			if member == nil {
				return false, false
			}
			present := len(valuesAt(child, d.Path)) > 0
			if present != (member.Min > 0) {
				return false, true
			}

		default:
			// "profile" needs recursive conformance checking and "position"
			// needs ordering guarantees; neither is answered here.
			return false, false
		}
	}
	return true, true
}

// sliceNames lists the slices declared for an element, in declaration order --
// which is the order occurrences are matched in, so an earlier slice wins.
func sliceNames(profile *conformance.Profile, path string) []string {
	seen := map[string]bool{}
	var out []string
	for _, el := range profile.Elements {
		if el.Path != path || el.Slice == "" || strings.Contains(el.Slice, "/") {
			continue
		}
		if !seen[el.Slice] {
			seen[el.Slice] = true
			out = append(out, el.Slice)
		}
	}
	return out
}

// sliceElement finds the profile element for one path within one slice.
func sliceElement(profile *conformance.Profile, path, slice string) *conformance.ProfileElement {
	for _, el := range profile.Elements {
		if el.Path == path && el.Slice == slice {
			return el
		}
	}
	return nil
}

// discriminatorElement finds the constraint a discriminator compares against.
//
// It may be held one level down, in a slice of a slice: the blood pressure
// profile tells its component slices apart by code.coding.code, and fixes that
// code inside a nested "SBPCode" slice of the coding. The value is what
// discriminates either way, so an exact match is preferred and a sub-slice
// accepted.
func discriminatorElement(profile *conformance.Profile, path, slice string) *conformance.ProfileElement {
	if el := sliceElement(profile, path, slice); el != nil {
		return el
	}
	prefix := slice + "/"
	for _, el := range profile.Elements {
		if el.Path == path && strings.HasPrefix(el.Slice, prefix) {
			return el
		}
	}
	return nil
}

// nestedSlicing lists the slices a profile declares inside another slice, which
// are not checked here.
//
// Reporting them is the point: a profile whose nested slices went unexamined
// has not been fully verified, and saying so is the difference between "this
// conforms" and "nothing I checked was wrong".
func nestedSlicing(profile *conformance.Profile) []string {
	seen := map[string]bool{}
	var out []string
	for _, el := range profile.Elements {
		if el.Slicing == nil || el.Slice == "" {
			continue
		}
		// Nearly every published profile slices extension by url, which is
		// boilerplate rather than a constraint anyone is relying on. Listing it
		// would bury the nested slices that do carry meaning.
		if last := lastSegment(el.Path); last == "extension" || last == "modifierExtension" {
			continue
		}
		key := el.Slice + ":" + el.Path
		if !seen[key] {
			seen[key] = true
			out = append(out, el.Path+" within the slice "+el.Slice)
		}
	}
	return out
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

func joinPath(base, rest string) string {
	if rest == "" || rest == "$this" {
		return base
	}
	return base + "." + rest
}

func shortName(profile *conformance.Profile, url string) string {
	if profile.Name != "" {
		return profile.Name
	}
	return url
}

// ---- document navigation ----

// groupsAt resolves a dotted element path from a root, returning one group per
// parent that reaches the final segment.
//
// Grouping is the whole point: a constraint on "component.code" is about each
// component, so counting every code in the resource together would let one
// component's code satisfy the requirement for all of them.
func groupsAt(root occurrence, path string) []group {
	segments := strings.Split(path, ".")
	parents := []occurrence{root}
	for _, segment := range segments[:len(segments)-1] {
		var next []occurrence
		for _, parent := range parents {
			next = append(next, valuesAt(parent, segment)...)
		}
		parents = next
	}
	last := segments[len(segments)-1]
	out := make([]group, 0, len(parents))
	for _, parent := range parents {
		out = append(out, group{
			parent:   parent,
			children: valuesAt(parent, last),
			path:     parent.path + "." + last,
		})
	}
	return out
}

// valuesAt returns the occurrences of one element name under a node, following
// a dotted path if given.
func valuesAt(parent occurrence, path string) []occurrence {
	if path == "" || path == "$this" {
		return []occurrence{parent}
	}
	current := []occurrence{parent}
	for _, segment := range strings.Split(path, ".") {
		var next []occurrence
		for _, node := range current {
			next = append(next, childrenNamed(node, segment)...)
		}
		current = next
	}
	return current
}

func childrenNamed(parent occurrence, name string) []occurrence {
	var out []occurrence
	for _, field := range parent.node.Fields() {
		if field.Name != name {
			continue
		}
		for i, value := range field.Values {
			path := parent.path + "." + value.Name()
			if field.Def.IsArray() {
				path = fmt.Sprintf("%s.%s[%d]", parent.path, value.Name(), i)
			}
			out = append(out, occurrence{node: value, path: path})
		}
	}
	return out
}

// ---- value comparison ----

// equalValue is FHIR's "fixed" semantics: the value must be exactly this.
func equalValue(got, want any) bool {
	switch expected := want.(type) {
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok || len(actual) != len(expected) {
			return false
		}
		for key, value := range expected {
			if !equalValue(actual[key], value) {
				return false
			}
		}
		return true
	case []any:
		actual, ok := got.([]any)
		if !ok || len(actual) != len(expected) {
			return false
		}
		for i := range expected {
			if !equalValue(actual[i], expected[i]) {
				return false
			}
		}
		return true
	default:
		return sameScalar(got, want)
	}
}

// matchesPattern is FHIR's "pattern" semantics: everything the pattern states
// must be present, and anything else is allowed alongside it.
func matchesPattern(got, want any) bool {
	switch expected := want.(type) {
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range expected {
			if !matchesPattern(actual[key], value) {
				return false
			}
		}
		return true
	case []any:
		actual, ok := got.([]any)
		if !ok {
			return false
		}
		// Every element the pattern names must be matched by some element of
		// the value; order and extra elements do not matter.
		for _, item := range expected {
			found := false
			for _, candidate := range actual {
				if matchesPattern(candidate, item) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return sameScalar(got, want)
	}
}

// sameScalar compares two JSON scalars by their textual form.
//
// The two sides come from different decoders: the index is decoded with the
// standard library, while a document keeps its numbers verbatim so decimal
// precision survives. Comparing the rendered text sidesteps the mismatch.
func sameScalar(got, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	a, okA := primitiveText(got)
	b, okB := primitiveText(want)
	return okA && okB && a == b
}

func render(value any) string {
	text, ok := primitiveText(value)
	if ok {
		return strconv.Quote(text)
	}
	return fmt.Sprintf("%v", value)
}
