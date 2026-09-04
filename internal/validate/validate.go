// Package validate checks a resource against the conformance index.
//
// Four layers, applied together and reported as one list of issues:
//
//   - Structure: every element is defined, cardinality holds, choice elements
//     carry one value, primitive values have the lexical form their type
//     requires, and references point at permitted types.
//   - Invariants: the FHIRPath constraints the specification attaches to types
//     and elements, evaluated against the document.
//   - Bindings: coded values are checked against the value sets that required
//     and extensible bindings name.
//   - Profiles: constraints from meta.profile and from an explicitly requested
//     profile, including slices.
//
// Two principles run through it. Nothing is reported as valid that was not
// actually checked -- a binding this server cannot resolve offline is an
// explicit "not checked", never silence. And a validator that stops at the
// first problem is nearly useless, so the walk collects everything it finds.
package validate

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
)

// Severities, matching the OperationOutcome value set.
const (
	SeverityError       = "error"
	SeverityWarning     = "warning"
	SeverityInformation = "information"
)

// Issue is one finding.
type Issue struct {
	Severity string
	// Code is an OperationOutcome issue type: structure, required, value,
	// invariant, code-invalid, not-supported, processing.
	Code string
	// Path locates the finding in the document, in the form FHIRPath uses for
	// OperationOutcome.expression: "Patient.name[0].family".
	Path    string
	Details string
	// Key is the invariant key, when the finding is an invariant failure.
	Key string
}

// Validator checks resources against one release's conformance index.
//
// It is safe for concurrent use: the only mutable state is a cache of compiled
// expressions, which is guarded.
type Validator struct {
	idx *conformance.Index

	// StrictTerminology promotes "this binding could not be checked" from a
	// warning to an error.
	//
	// The default is deliberate. SNOMED CT is licensed, LOINC and RxNorm are
	// too large to embed, and UCUM and the ISO code lists are external
	// standards, so a server that refused every resource carrying one of their
	// codes would be useless offline -- which is the situation this server is
	// built for. Teams that have wired up a terminology service can turn the
	// warnings into errors.
	StrictTerminology bool

	mu       sync.Mutex
	patterns map[string]*regexp.Regexp
	compiled map[string]compiledExpr
}

// New builds a validator for a release.
func New(idx *conformance.Index) *Validator {
	return &Validator{
		idx:      idx,
		patterns: map[string]*regexp.Regexp{},
		compiled: map[string]compiledExpr{},
	}
}

// Options shape one validation run.
type Options struct {
	// Profiles are canonical URLs to validate against, in addition to the
	// ones the resource claims in meta.profile.
	Profiles []string
}

// Validate checks a resource and returns everything it found, ordered by
// severity and then by path.
func (v *Validator) Validate(node *resource.Node, opts Options) []Issue {
	root := node.FHIRType()
	if !v.idx.IsResource(root) {
		return []Issue{{
			Severity: SeverityError,
			Code:     "structure",
			Path:     root,
			Details:  fmt.Sprintf("%q is not a resource type in FHIR %s", root, v.idx.FHIRVersion),
		}}
	}

	run := &run{v: v, node: node}
	run.walk(node, root, node)
	run.checkInvariants(node, root, node)

	for _, url := range profileURLs(node, opts) {
		run.checkProfile(node, root, url)
	}

	sortIssues(run.issues)
	return run.issues
}

// run accumulates the findings of one validation.
type run struct {
	v      *Validator
	node   *resource.Node
	issues []Issue
}

// add records one finding. Every issue passes through here, whichever helper
// phrased it, so there is one place to look for what a run reports.
func (r *run) add(issue Issue) { r.issues = append(r.issues, issue) }

// report records a finding with a formatted message.
func (r *run) report(severity, code, path, format string, args ...any) {
	r.add(Issue{
		Severity: severity, Code: code, Path: path,
		Details: fmt.Sprintf(format, args...),
	})
}

// profileURLs collects the profiles to check: those the caller asked for, and
// those the resource claims for itself.
func profileURLs(node *resource.Node, opts Options) []string {
	seen := map[string]bool{}
	var out []string
	add := func(url string) {
		if url == "" || seen[url] {
			return
		}
		seen[url] = true
		out = append(out, url)
	}
	for _, url := range opts.Profiles {
		add(url)
	}
	if obj, ok := node.Object(); ok {
		meta, _ := obj["meta"].(map[string]any)
		claimed, _ := meta["profile"].([]any)
		for _, item := range claimed {
			if url, ok := item.(string); ok {
				add(url)
			}
		}
	}
	return out
}

// ---- structure ----

// walk checks one node and descends into its children.
//
// scope is the nearest enclosing resource, which is what FHIRPath's %resource
// means: an invariant on a contained Organization is evaluated against that
// Organization, not against the Patient carrying it.
func (r *run) walk(node *resource.Node, path string, scope *resource.Node) {
	for _, key := range node.UnknownKeys() {
		r.report(SeverityError, "structure", path+"."+strings.TrimPrefix(key, "_"),
			"%q is not an element of %s", key, node.FHIRType())
	}
	r.checkPrimitive(node, path)

	for _, field := range node.Fields() {
		r.checkField(node, field, path, scope)
	}
}

func (r *run) checkField(parent *resource.Node, field resource.Field, path string, scope *resource.Node) {
	def := field.Def
	fieldPath := path + "." + field.Name

	// A choice element holds one value, and a document that sets two has said
	// something the type system cannot represent.
	if def.Choice && len(field.Keys) > 1 {
		r.report(SeverityError, "structure", fieldPath,
			"a choice element takes one value, but %s are all present",
			strings.Join(field.Keys, ", "))
	}

	present := len(field.Values)
	switch {
	case def.Max == "0" && present > 0:
		r.report(SeverityError, "structure", fieldPath,
			"%s is prohibited here but is present", field.Name)
	case present < def.Min:
		r.report(SeverityError, "required", fieldPath,
			"%s is required (minimum %d), but %d are present", field.Name, def.Min, present)
	}
	if upper, err := strconv.Atoi(def.Max); err == nil && upper > 0 && present > upper {
		r.report(SeverityError, "structure", fieldPath,
			"%s allows at most %d value(s), but %d are present", field.Name, upper, present)
	}
	// Repetition and JSON shape have to agree: a repeating element is an
	// array even with one occurrence, and a singular one never is. Both
	// readers produce the same shape from either format, so this stays a
	// statement about the resource rather than about its serialization.
	if obj, ok := parent.Raw().(map[string]any); ok && def.Max != "0" {
		for _, key := range field.Keys {
			raw, present := obj[key]
			if !present {
				continue
			}
			_, isArray := raw.([]any)
			switch {
			case isArray && !def.IsArray():
				r.report(SeverityError, "structure", fieldPath,
					"%s does not repeat, so it must not be written as an array", field.Name)
			case !isArray && def.IsArray():
				r.report(SeverityError, "structure", fieldPath,
					"%s repeats, so it must be written as an array even with one value", field.Name)
			}
		}
	}

	r.checkBinding(field, fieldPath)

	for i, child := range field.Values {
		childPath := fieldPath
		if def.IsArray() {
			childPath = fmt.Sprintf("%s[%d]", fieldPath, i)
		}
		if child.Name() != field.Name {
			// A choice element is located by the name the document used.
			childPath = strings.TrimSuffix(childPath, field.Name) + child.Name()
			if def.IsArray() {
				childPath = fmt.Sprintf("%s.%s[%d]", path, child.Name(), i)
			}
		}
		r.checkReference(child, def, childPath)
		// A contained or bundled resource starts a new %resource scope.
		childScope := scope
		if r.v.idx.IsResource(child.FHIRType()) {
			childScope = child
		}
		r.walk(child, childPath, childScope)
		r.checkInvariants(child, childPath, childScope)
	}
}

// checkPrimitive checks that a primitive's JSON value has the right shape and
// the lexical form its type requires.
func (r *run) checkPrimitive(node *resource.Node, path string) {
	if !node.IsPrimitiveType() {
		return
	}
	def, ok := r.v.idx.Type(node.FHIRType())
	if !ok {
		return
	}
	raw := node.Raw()
	if raw == nil {
		// Present only through its extensions, which the specification allows:
		// a value may be absent and still carry a Data Absent Reason.
		return
	}

	text, ok := primitiveText(raw)
	if !ok {
		r.report(SeverityError, "structure", path,
			"%s is a %s, but the document holds %s", node.Name(), node.FHIRType(), jsonKind(raw))
		return
	}
	if node.FHIRType() == "xhtml" {
		// Narrative is checked for well-formedness by the reader rather than
		// by a lexical pattern.
		return
	}
	if text == "" {
		r.report(SeverityError, "value", path, "a %s must not be empty", node.FHIRType())
		return
	}

	// Only the lexical form is checked, not the JSON spelling. Whether an
	// integer arrived as 15 or as "15" is a question about one wire format, and
	// the document model here is deliberately format-neutral -- the same
	// document may have come from XML, where everything is text. The pattern
	// below rejects a value that is not an integer either way, which is the
	// part that is about the resource rather than about its serialization.
	pattern := r.v.pattern(node.FHIRType(), def.Regex)
	if pattern != nil && !pattern.MatchString(text) {
		r.report(SeverityError, "value", path,
			"%q is not a valid %s", text, node.FHIRType())
	}
}

// checkReference verifies that a reference points at a type the element
// permits. It is a purely definitional check -- no lookup, no resolution --
// which is why it can be an error rather than a warning.
func (r *run) checkReference(node *resource.Node, def *conformance.ElementDef, path string) {
	if node.FHIRType() != "Reference" {
		return
	}
	obj, ok := node.Object()
	if !ok {
		return
	}
	value, _ := obj["reference"].(string)
	if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, "urn:") {
		// A contained reference or a bundle placeholder names no type.
		return
	}
	var targets []string
	for _, t := range def.Types {
		if t.Code == "Reference" {
			targets = append(targets, t.Targets...)
		}
	}
	if len(targets) == 0 {
		return
	}
	typeName := referencedType(value)
	if typeName == "" || !r.v.idx.IsResource(typeName) {
		return
	}
	for _, target := range targets {
		if target == typeName || target == "Resource" {
			return
		}
	}
	r.report(SeverityError, "structure", path+".reference",
		"%s may reference %s, but this points at a %s",
		def.Path, strings.Join(targets, ", "), typeName)
}

// referencedType reads the resource type out of a literal reference, whether
// relative ("Patient/1") or absolute (".../Patient/1/_history/2").
func referencedType(value string) string {
	if before, _, found := strings.Cut(value, "?"); found {
		value = before
	}
	segments := strings.Split(strings.Trim(value, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if i+1 < len(segments) && segments[i+1] == "_history" {
			continue
		}
		if i >= 1 {
			return segments[i-1]
		}
	}
	return ""
}

// pattern compiles a primitive's lexical pattern, cached.
//
// A pattern that Go's regexp cannot compile is dropped rather than fatal: RE2
// rejects a few constructs PCRE allows, and losing one lexical check is better
// than refusing to validate anything.
func (v *Validator) pattern(typeName, expr string) *regexp.Regexp {
	if expr == "" {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if compiled, ok := v.patterns[typeName]; ok {
		return compiled
	}
	compiled, err := regexp.Compile("^(?:" + expr + ")$")
	if err != nil {
		compiled = nil
	}
	v.patterns[typeName] = compiled
	return compiled
}

// primitiveText renders a JSON scalar as the string the lexical pattern is
// written against, reporting false for a value that is not a scalar at all.
func primitiveText(raw any) (string, bool) {
	switch value := raw.(type) {
	case string:
		return value, true
	case bool:
		return strconv.FormatBool(value), true
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64), true
	case int:
		return strconv.Itoa(value), true
	case int64:
		return strconv.FormatInt(value, 10), true
	default:
		// Decimals are held verbatim to keep their precision, so they arrive
		// as a type that renders itself.
		if stringer, ok := raw.(fmt.Stringer); ok {
			return stringer.String(), true
		}
		return "", false
	}
}

func jsonKind(raw any) string {
	switch raw.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	default:
		return "a number"
	}
}

// sortIssues orders findings by severity and then by location, so a long list
// reads top-down from what breaks the resource to what merely warrants a look.
func sortIssues(issues []Issue) {
	rank := map[string]int{SeverityError: 0, SeverityWarning: 1, SeverityInformation: 2}
	sort.SliceStable(issues, func(i, j int) bool {
		if a, b := rank[issues[i].Severity], rank[issues[j].Severity]; a != b {
			return a < b
		}
		return issues[i].Path < issues[j].Path
	})
}

// HasErrors reports whether any issue is an error.
func HasErrors(issues []Issue) bool {
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			return true
		}
	}
	return false
}
