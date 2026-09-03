package fhirpath

import (
	"fmt"
	"strings"
)

// EvalError is a runtime failure, as distinct from an expression that simply
// matches nothing. FHIRPath draws that line sharply -- navigating a nonexistent
// element yields empty, but adding a string to a date is an error -- and the
// conformance suite tests both, so the two must never be conflated.
type EvalError struct {
	Msg string
}

func (e *EvalError) Error() string { return "fhirpath: " + e.Msg }

func evalErrorf(format string, args ...any) error {
	return &EvalError{Msg: fmt.Sprintf(format, args...)}
}

// Context supplies what an expression may reference beyond its input.
type Context struct {
	// Root is the resource the expression is evaluated against, exposed as
	// %resource and, absent a containing resource, %rootResource.
	Root Node
	// Vars holds external constants referenced as %name. The evaluator never
	// mutates it.
	Vars map[string]Collection
	// ResolveReference resolves a Reference to the resource it points at, for
	// resolve(). Nil means references do not resolve, which yields empty rather
	// than an error -- a server without the target loaded is a normal state.
	ResolveReference func(ref string) Node
	// TypeHierarchy returns a FHIR type's base chain, nearest first, so type
	// tests honour inheritance: a "code" is a "string" is an "Element". This
	// package holds no conformance data of its own, so the caller supplies it;
	// without it, type tests fall back to exact matching.
	TypeHierarchy func(typeName string) []string
	// ConformsTo backs the conformsTo() function: it reports whether a node
	// conforms to a profile, and whether the question could be answered at all.
	// An unresolvable profile is an error, not a negative answer.
	ConformsTo func(node Node, profile string) (conforms, known bool)
}

// scope carries the per-iteration bindings that functions like where() and
// aggregate() introduce. It is a linked list so an inner scope can shadow an
// outer one without copying the variable map.
type scope struct {
	this   Collection
	index  int
	total  Collection
	name   string
	value  Collection
	parent *scope
}

func (s *scope) lookup(name string) (Collection, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if cur.name == name {
			return cur.value, true
		}
	}
	return nil, false
}

// defined reports whether a variable is already bound in this scope chain.
// defineVariable is required to reject redefinition within one scope.
func (s *scope) defined(name string) bool {
	_, ok := s.lookup(name)
	return ok
}

type evaluator struct {
	ctx *Context
}

// Eval evaluates a parsed expression against an input collection.
func Eval(e Expr, input Collection, ctx *Context) (Collection, error) {
	if ctx == nil {
		ctx = &Context{}
	}
	ev := &evaluator{ctx: ctx}
	return ev.eval(e, input, &scope{this: input, index: -1})
}

// EvalNode is the common case: evaluate against a single resource.
func EvalNode(e Expr, root Node, ctx *Context) (Collection, error) {
	if ctx == nil {
		ctx = &Context{}
	}
	if ctx.Root == nil {
		ctx.Root = root
	}
	return Eval(e, Collection{root}, ctx)
}

func (ev *evaluator) eval(e Expr, input Collection, sc *scope) (Collection, error) {
	switch n := e.(type) {
	case *Literal:
		return ev.literal(n)
	case *Variable:
		return ev.variable(n, input, sc)
	case *ExternalConstant:
		return ev.external(n, sc)
	case *Invocation:
		return ev.invocation(n, input, sc)
	case *Indexer:
		return ev.indexer(n, input, sc)
	case *Unary:
		return ev.unary(n, input, sc)
	case *TypeOp:
		return ev.typeOp(n, input, sc)
	case *Binary:
		return ev.binary(n, input, sc)
	default:
		return nil, evalErrorf("unsupported expression %T", e)
	}
}

func (ev *evaluator) literal(n *Literal) (Collection, error) {
	switch n.Kind {
	case LitEmpty:
		return nil, nil
	case LitBoolean:
		return boolCollection(n.Bool()), nil
	case LitString:
		return one(String_(n.Text)), nil
	case LitNumber:
		if n.IsInteger() {
			i, err := n.Int()
			if err != nil {
				return nil, evalErrorf("invalid integer literal %q", n.Text)
			}
			return one(Integer(i)), nil
		}
		d, err := NewDecimal(n.Text)
		if err != nil {
			return nil, evalErrorf("invalid decimal literal %q", n.Text)
		}
		return one(d), nil
	case LitLong:
		i, err := n.Int()
		if err != nil {
			return nil, evalErrorf("invalid long literal %q", n.Text)
		}
		return one(Integer(i)), nil
	case LitQuantity:
		d, err := NewDecimal(n.Text)
		if err != nil {
			return nil, evalErrorf("invalid quantity value %q", n.Text)
		}
		return one(Quantity{Value: d, Unit: n.Unit, Calendar: !n.UnitQuoted}), nil
	case LitDateTime:
		t, err := ParseTemporal(n.Text)
		if err != nil {
			return nil, evalErrorf("invalid date/time literal @%s: %v", n.Text, err)
		}
		return one(t), nil
	default:
		return nil, evalErrorf("unsupported literal")
	}
}

func (ev *evaluator) variable(n *Variable, input Collection, sc *scope) (Collection, error) {
	switch n.Name {
	case "this":
		return sc.this, nil
	case "index":
		if sc.index < 0 {
			return nil, nil
		}
		return one(Integer(sc.index)), nil
	case "total":
		return sc.total, nil
	default:
		return nil, evalErrorf("unknown variable $%s", n.Name)
	}
}

func (ev *evaluator) external(n *ExternalConstant, sc *scope) (Collection, error) {
	// Variables introduced by defineVariable shadow the context's constants.
	if v, ok := sc.lookup(n.Name); ok {
		return v, nil
	}
	if v, ok := ev.ctx.Vars[n.Name]; ok {
		return v, nil
	}
	switch n.Name {
	case "resource", "context", "rootResource":
		if ev.ctx.Root == nil {
			return nil, nil
		}
		return one(ev.ctx.Root), nil
	case "ucum":
		return one(String_("http://unitsofmeasure.org")), nil
	case "sct":
		return one(String_("http://snomed.info/sct")), nil
	case "loinc":
		return one(String_("http://loinc.org")), nil
	}
	// FHIR defines two shorthands that expand to canonical URLs rather than
	// being supplied by the host environment.
	if rest, ok := strings.CutPrefix(n.Name, "ext-"); ok {
		return one(String_("http://hl7.org/fhir/StructureDefinition/" + rest)), nil
	}
	if rest, ok := strings.CutPrefix(n.Name, "vs-"); ok {
		return one(String_("http://hl7.org/fhir/ValueSet/" + rest)), nil
	}
	// An unknown constant is an error, not empty: the specification treats it
	// as a broken expression rather than a missing value.
	return nil, evalErrorf("unknown external constant %%%s", n.Name)
}

// invocation handles both member navigation and function calls.
func (ev *evaluator) invocation(n *Invocation, input Collection, sc *scope) (Collection, error) {
	focus, inner, err := ev.evalChain(n, input, sc)
	_ = inner
	return focus, err
}

// evalChain evaluates one link of an invocation chain and returns the scope the
// *next* link should use.
//
// Only defineVariable extends the scope, and the distinction matters: its
// binding must be visible to later steps of the same chain but not to sibling
// branches, so that
//
//	defineVariable('n1', a).select(%n1) | defineVariable('n1', b).select(%n1)
//
// is legal rather than a redefinition. Threading the scope through the chain --
// rather than mutating one shared scope -- is what keeps the two branches
// independent.
func (ev *evaluator) evalChain(e Expr, input Collection, sc *scope) (Collection, *scope, error) {
	inv, ok := e.(*Invocation)
	if !ok {
		c, err := ev.eval(e, input, sc)
		return c, sc, err
	}

	focus := input
	inner := sc
	if inv.Subject != nil {
		var err error
		focus, inner, err = ev.evalChain(inv.Subject, input, sc)
		if err != nil {
			return nil, nil, err
		}
	}
	if !inv.IsFunction() {
		// The identity rule below applies only at the start of an expression.
		return ev.navigate(focus, inv.Name, inv.Subject == nil), inner, nil
	}
	if inv.Name == "defineVariable" {
		return ev.defineVariableScoped(inv.Args, focus, inner)
	}
	out, err := ev.call(inv, focus, inner)
	return out, inner, err
}

// navigate walks one element name across every node in the focus.
//
// leading says this identifier begins the expression, which is the only place
// the specification lets a name select the focus itself rather than a child:
// "Patient.name" evaluated with a Patient as the focus. Applying that rule at
// every step would make ".code" match any node whose *type* is code -- so
// Questionnaire.status and .subjectType would answer to it -- and quietly
// inflate the results of expressions like "children().code".
func (ev *evaluator) navigate(focus Collection, name string, leading bool) Collection {
	var out Collection
	for _, v := range focus {
		node, ok := v.(Node)
		if !ok {
			// System values have no navigable members.
			continue
		}
		if leading && node.FHIRType() == name {
			out = append(out, node)
			continue
		}
		for _, child := range node.Children(name) {
			out = append(out, child)
		}
	}
	return out
}

func (ev *evaluator) indexer(n *Indexer, input Collection, sc *scope) (Collection, error) {
	subject, err := ev.eval(n.Subject, input, sc)
	if err != nil {
		return nil, err
	}
	idxColl, err := ev.eval(n.Index, input, sc)
	if err != nil {
		return nil, err
	}
	if idxColl.Empty() {
		return nil, nil
	}
	i, ok := asInt(idxColl)
	if !ok {
		return nil, evalErrorf("index must be a single integer")
	}
	if i < 0 || int(i) >= len(subject) {
		// Out of range is empty, not an error.
		return nil, nil
	}
	return one(subject[i]), nil
}

func (ev *evaluator) unary(n *Unary, input Collection, sc *scope) (Collection, error) {
	operand, err := ev.eval(n.Operand, input, sc)
	if err != nil {
		return nil, err
	}
	if operand.Empty() {
		return nil, nil
	}
	v, ok := operand.Single()
	if !ok {
		return nil, evalErrorf("unary %s requires a single value", n.Op)
	}
	if n.Op == "+" {
		return one(unwrap(v)), nil
	}
	switch x := unwrap(v).(type) {
	case Integer:
		return one(-x), nil
	case Decimal:
		return one(x.Neg()), nil
	case Quantity:
		return one(Quantity{Value: x.Value.Neg(), Unit: x.Unit, Calendar: x.Calendar}), nil
	default:
		return nil, evalErrorf("cannot negate %s", x.TypeName())
	}
}

func (ev *evaluator) typeOp(n *TypeOp, input Collection, sc *scope) (Collection, error) {
	operand, err := ev.eval(n.Operand, input, sc)
	if err != nil {
		return nil, err
	}
	if err := ev.checkTypeName(n.Type); err != nil {
		return nil, err
	}
	if operand.Empty() {
		return nil, nil
	}
	v, ok := operand.Single()
	if !ok {
		if n.Op == "is" {
			return nil, evalErrorf("'is' requires a single value")
		}
		// "as" over a multi-item collection filters it.
		var out Collection
		for _, item := range operand {
			if ev.valueIsTypeMode(item, n.Type, true) {
				out = append(out, item)
			}
		}
		return out, nil
	}
	matches := ev.valueIsTypeMode(v, n.Type, n.Op == "as")
	if n.Op == "is" {
		return boolCollection(matches), nil
	}
	if matches {
		return one(v), nil
	}
	return nil, nil
}

// valueIsType implements the type test behind is, as, and ofType. Names may be
// namespace-qualified ("FHIR.Patient", "System.String") or bare, and FHIR
// primitives satisfy both their FHIR type and the System type they map to.
//
// Type tests honour the FHIR hierarchy: Patient.gender is a "code", and a code
// is a string, so "Patient.gender.is(string)" is true. That requires the
// conformance index, which this package does not hold, so it comes through
// Context.TypeHierarchy.
func (ev *evaluator) valueIsType(v Value, want string) bool {
	return ev.valueIsTypeMode(v, want, false)
}

// valueIsTypeMode implements the type test. When exact is set, the FHIR type
// hierarchy is not consulted.
//
// The two differ deliberately, and the conformance suite pins it down:
// Patient.gender.is(string) is true because a code is a string, but
// Patient.gender.as(string) and .ofType(string) are empty, because coercion
// and filtering select a type rather than test membership of a hierarchy.
func (ev *evaluator) valueIsTypeMode(v Value, want string, exact bool) bool {
	want = strings.TrimPrefix(want, "FHIR.")
	if strings.HasPrefix(want, "System.") {
		bare := strings.TrimPrefix(want, "System.")
		// Only a genuine System value satisfies a System type test. A FHIR
		// primitive converts to one for arithmetic, but it is not one:
		// "Patient.active.is(System.Boolean)" is false.
		if _, isNode := v.(Node); isNode {
			return false
		}
		return strings.TrimPrefix(v.TypeName(), "System.") == bare
	}
	node, isNode := v.(Node)
	if !isNode {
		return strings.TrimPrefix(v.TypeName(), "System.") == want
	}
	chain := ev.typeChain(node.FHIRType())
	if exact {
		chain = chain[:1]
	}
	for _, typ := range chain {
		if typeMatches(typ, want) {
			return true
		}
	}
	// Deliberately no fallback to the System type here. A FHIR primitive is not
	// a System primitive: "Patient.active.is(Boolean)" is false even though
	// active is a boolean, because its type is FHIR.boolean. Reaching System
	// types requires naming them ("is(System.Boolean)"), which the branch above
	// handles. The suite tests both directions.
	return false
}

// typeChain returns a type and its ancestors, or just the type when no
// hierarchy is available.
func (ev *evaluator) typeChain(name string) []string {
	if ev.ctx.TypeHierarchy == nil {
		return []string{name}
	}
	chain := ev.ctx.TypeHierarchy(name)
	if len(chain) == 0 {
		return []string{name}
	}
	return chain
}

// checkTypeName rejects a type nobody has heard of. Answering false for
// "ofType(string1)" would hide a typo behind an empty result, so the
// specification makes it an error -- but only when a type registry is
// available to say so.
func (ev *evaluator) checkTypeName(want string) error {
	if ev.ctx.TypeHierarchy == nil {
		return nil
	}
	bare := strings.TrimPrefix(want, "FHIR.")
	if strings.HasPrefix(bare, "System.") || systemTypeNames[bare] {
		return nil
	}
	if len(ev.ctx.TypeHierarchy(bare)) == 0 {
		return evalErrorf("unknown type %q", want)
	}
	return nil
}

// systemTypeNames are the FHIRPath System types, which may be named without
// their namespace: "true.is(Boolean)" is legal and has nothing to do with FHIR.
var systemTypeNames = map[string]bool{
	"Boolean": true, "String": true, "Integer": true, "Long": true,
	"Decimal": true, "Date": true, "DateTime": true, "Time": true,
	"Quantity": true, "TypeInfo": true,
}

// typeMatches compares FHIR type names exactly.
//
// Case matters here: FHIR spells its primitives in lower case and the System
// types in upper, and the two are genuinely different. Matching case-
// insensitively would make "Patient.active.is(Boolean)" true, when the
// specification -- and the conformance suite -- say it is false, because
// active's type is FHIR.boolean rather than System.Boolean.
func typeMatches(actual, want string) bool { return actual == want }

func (ev *evaluator) binary(n *Binary, input Collection, sc *scope) (Collection, error) {
	// The boolean operators short-circuit and have three-valued semantics, so
	// they cannot evaluate both sides up front.
	switch n.Op {
	case "and", "or", "xor", "implies":
		return ev.logical(n, input, sc)
	}

	left, err := ev.eval(n.Left, input, sc)
	if err != nil {
		return nil, err
	}
	right, err := ev.eval(n.Right, input, sc)
	if err != nil {
		return nil, err
	}

	switch n.Op {
	case "|":
		return union(left, right), nil
	case "=", "!=", "~", "!~":
		return ev.equality(n.Op, left, right)
	case "<", "<=", ">", ">=":
		return ev.comparison(n.Op, left, right)
	case "+", "-", "*", "/", "div", "mod", "&":
		return ev.arithmetic(n.Op, left, right)
	case "in":
		return ev.membership(left, right)
	case "contains":
		return ev.membership(right, left)
	default:
		return nil, evalErrorf("unsupported operator %q", n.Op)
	}
}

// logical implements FHIRPath's three-valued boolean logic, where empty means
// "unknown". "false and {}" is false because the answer is already determined,
// while "true and {}" is empty because it is not.
func (ev *evaluator) logical(n *Binary, input Collection, sc *scope) (Collection, error) {
	left, err := ev.eval(n.Left, input, sc)
	if err != nil {
		return nil, err
	}
	lv, lok := singletonBool(left)
	if !left.Empty() && !lok {
		return nil, evalErrorf("operator %q requires a boolean", n.Op)
	}

	// Short-circuit where the left operand alone decides the result.
	switch n.Op {
	case "and":
		if lok && !lv {
			return boolCollection(false), nil
		}
	case "or":
		if lok && lv {
			return boolCollection(true), nil
		}
	case "implies":
		if lok && !lv {
			return boolCollection(true), nil
		}
	}

	right, err := ev.eval(n.Right, input, sc)
	if err != nil {
		return nil, err
	}
	rv, rok := singletonBool(right)
	if !right.Empty() && !rok {
		return nil, evalErrorf("operator %q requires a boolean", n.Op)
	}

	switch n.Op {
	case "and":
		if !lok || !rok {
			// Only a false operand can settle an "and" with an unknown side.
			if rok && !rv {
				return boolCollection(false), nil
			}
			return nil, nil
		}
		return boolCollection(lv && rv), nil
	case "or":
		if !lok || !rok {
			if rok && rv {
				return boolCollection(true), nil
			}
			return nil, nil
		}
		return boolCollection(lv || rv), nil
	case "xor":
		if !lok || !rok {
			return nil, nil
		}
		return boolCollection(lv != rv), nil
	case "implies":
		if !lok {
			// Unknown implies true is true; unknown implies anything else is
			// unknown.
			if rok && rv {
				return boolCollection(true), nil
			}
			return nil, nil
		}
		if !rok {
			return nil, nil
		}
		return boolCollection(!lv || rv), nil
	}
	return nil, evalErrorf("unsupported logical operator %q", n.Op)
}

// union concatenates two collections and removes duplicates, preserving the
// order of first appearance.
func union(a, b Collection) Collection {
	var out Collection
	for _, v := range append(append(Collection{}, a...), b...) {
		dup := false
		for _, seen := range out {
			if valuesEqual(seen, v) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

func (ev *evaluator) membership(needle, haystack Collection) (Collection, error) {
	if needle.Empty() {
		return nil, nil
	}
	v, ok := needle.Single()
	if !ok {
		return nil, evalErrorf("membership requires a single value on the left")
	}
	if haystack.Empty() {
		return boolCollection(false), nil
	}
	for _, item := range haystack {
		if valuesEqual(v, item) {
			return boolCollection(true), nil
		}
	}
	return boolCollection(false), nil
}

// asInt extracts a single integer from a collection.
func asInt(c Collection) (int64, bool) {
	v, ok := c.Single()
	if !ok {
		return 0, false
	}
	switch x := unwrap(v).(type) {
	case Integer:
		return int64(x), true
	case Decimal:
		if x.IsInt() {
			return x.Rat().Num().Int64(), true
		}
	}
	return 0, false
}

// asString extracts a single string from a collection.
func asString(c Collection) (string, bool) {
	v, ok := c.Single()
	if !ok {
		return "", false
	}
	if s, ok := unwrap(v).(String_); ok {
		return string(s), true
	}
	return "", false
}
