package fhirpath

// Function dispatch.
//
// Most functions receive already-evaluated arguments, but a significant
// minority must not: where() and select() evaluate their argument once per
// item with $this rebound, iif() must not evaluate the branch it does not take,
// and defineVariable() introduces a binding for the rest of the chain. Those
// are handled before the common path, which is why this is a switch rather than
// a table of uniform function values.
func (ev *evaluator) call(inv *Invocation, focus Collection, sc *scope) (Collection, error) {
	name, args := inv.Name, inv.Args

	switch name {
	// ---- lazily-evaluated arguments ----
	case "where":
		return ev.where(args, focus, sc)
	case "select":
		return ev.selectFn(args, focus, sc)
	case "all":
		return ev.all(args, focus, sc)
	case "exists":
		return ev.exists(args, focus, sc)
	case "repeat":
		return ev.repeat(args, focus, sc)
	case "aggregate":
		return ev.aggregate(args, focus, sc)
	case "iif":
		return ev.iif(args, focus, sc)
	case "defineVariable":
		return ev.defineVariable(args, focus, sc)
	case "trace":
		return ev.trace(args, focus, sc)
	case "sort":
		return ev.sortBy(args, focus, sc)
	case "ofType":
		return ev.ofType(args, focus)
	case "is":
		return ev.isAsFn("is", args, focus)
	case "as":
		return ev.isAsFn("as", args, focus)
	}

	// Arguments are evaluated against the enclosing context, not against the
	// function's focus: in "name.select(use.union(given))" the argument "given"
	// resolves against each name, even though union's focus is that name's use.
	// (iif is the exception and binds $this to its focus; see there.)
	evaluated := make([]Collection, len(args))
	for i, a := range args {
		v, err := ev.eval(a, sc.this, sc)
		if err != nil {
			return nil, err
		}
		evaluated[i] = v
	}
	return ev.simpleCall(name, focus, evaluated)
}

// ---- functions with lazily-evaluated arguments ----

// forEach evaluates body once per item with $this and $index bound to it.
func (ev *evaluator) forEach(body Expr, focus Collection, sc *scope, fn func(i int, res Collection) (stop bool, err error)) error {
	for i, item := range focus {
		inner := &scope{this: one(item), index: i, total: sc.total, parent: sc}
		res, err := ev.eval(body, one(item), inner)
		if err != nil {
			return err
		}
		stop, err := fn(i, res)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
	return nil
}

func (ev *evaluator) where(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) != 1 {
		return nil, evalErrorf("where() takes exactly one argument")
	}
	var out Collection
	err := ev.forEach(args[0], focus, sc, func(i int, res Collection) (bool, error) {
		keep, ok := singletonBool(res)
		if !ok && !res.Empty() {
			return false, evalErrorf("where() criteria must evaluate to a boolean")
		}
		if ok && keep {
			out = append(out, focus[i])
		}
		return false, nil
	})
	return out, err
}

func (ev *evaluator) selectFn(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) != 1 {
		return nil, evalErrorf("select() takes exactly one argument")
	}
	var out Collection
	err := ev.forEach(args[0], focus, sc, func(i int, res Collection) (bool, error) {
		// select flattens: each item's projection contributes its items.
		out = append(out, res...)
		return false, nil
	})
	return out, err
}

func (ev *evaluator) all(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) == 0 {
		// all() with no criteria asks whether every item is true.
		return ev.simpleCall("allTrue", focus, nil)
	}
	if len(args) != 1 {
		return nil, evalErrorf("all() takes at most one argument")
	}
	result := true
	err := ev.forEach(args[0], focus, sc, func(i int, res Collection) (bool, error) {
		v, ok := singletonBool(res)
		if !ok || !v {
			result = false
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return boolCollection(result), nil
}

func (ev *evaluator) exists(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) == 0 {
		return boolCollection(!focus.Empty()), nil
	}
	if len(args) != 1 {
		return nil, evalErrorf("exists() takes at most one argument")
	}
	found := false
	err := ev.forEach(args[0], focus, sc, func(i int, res Collection) (bool, error) {
		if v, ok := singletonBool(res); ok && v {
			found = true
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return boolCollection(found), nil
}

// repeat applies a projection transitively until it stops yielding new items.
// Cycles are possible in FHIR data, so items already seen are not re-expanded.
func (ev *evaluator) repeat(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) != 1 {
		return nil, evalErrorf("repeat() takes exactly one argument")
	}
	var out Collection
	current := focus
	for depth := 0; !current.Empty(); depth++ {
		if depth > 1000 {
			return nil, evalErrorf("repeat() exceeded its depth limit; the projection is probably cyclic")
		}
		var next Collection
		err := ev.forEach(args[0], current, sc, func(i int, res Collection) (bool, error) {
			for _, v := range res {
				if !containsValue(out, v) {
					out = append(out, v)
					next = append(next, v)
				}
			}
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		current = next
	}
	return out, nil
}

func (ev *evaluator) aggregate(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, evalErrorf("aggregate() takes one or two arguments")
	}
	total := Collection(nil)
	if len(args) == 2 {
		init, err := ev.eval(args[1], sc.this, sc)
		if err != nil {
			return nil, err
		}
		total = init
	}
	for i, item := range focus {
		inner := &scope{this: one(item), index: i, total: total, parent: sc}
		res, err := ev.eval(args[0], one(item), inner)
		if err != nil {
			return nil, err
		}
		total = res
	}
	return total, nil
}

func (ev *evaluator) iif(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) < 2 || len(args) > 3 {
		return nil, evalErrorf("iif() takes two or three arguments")
	}
	// iif is defined on a singleton input; a multi-item focus has no single
	// context for the criterion to be evaluated against.
	if len(focus) > 1 {
		return nil, evalErrorf("iif() requires a collection of at most one item, got %d", len(focus))
	}
	// Unlike other functions, iif evaluates its criterion in the context of its
	// own focus: "('context').iif($this = 'context', ...)" must see 'context'.
	inner := &scope{this: focus, index: sc.index, total: sc.total, parent: sc}
	cond, err := ev.eval(args[0], focus, inner)
	if err != nil {
		return nil, err
	}
	// The criterion must actually be boolean. Treating "1 | 2 | 3" or a bare
	// string as truthy would quietly pick a branch the author did not intend.
	if !cond.Empty() {
		if _, isBool := unwrap(cond[0]).(Boolean); !isBool || len(cond) != 1 {
			return nil, evalErrorf("iif() criterion must be a boolean")
		}
	}
	if v, ok := singletonBool(cond); ok && v {
		return ev.eval(args[1], focus, inner)
	}
	if len(args) == 3 {
		return ev.eval(args[2], focus, inner)
	}
	return nil, nil
}

// defineVariable binds a name for the remainder of the invocation chain. The
// specification makes redefining a name in the same scope an error, which the
// conformance suite checks explicitly.
func (ev *evaluator) defineVariable(args []Expr, focus Collection, sc *scope) (Collection, error) {
	out, _, err := ev.defineVariableScoped(args, focus, sc)
	return out, err
}

// defineVariableScoped binds a name and returns the scope later steps in the
// same chain should use.
func (ev *evaluator) defineVariableScoped(args []Expr, focus Collection, sc *scope) (Collection, *scope, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, nil, evalErrorf("defineVariable() takes one or two arguments")
	}
	nameColl, err := ev.eval(args[0], focus, sc)
	if err != nil {
		return nil, nil, err
	}
	name, ok := asString(nameColl)
	if !ok {
		return nil, nil, evalErrorf("defineVariable() requires a string name")
	}
	if isSystemVariable(name) {
		return nil, nil, evalErrorf("cannot redefine the system variable %%%s", name)
	}
	if sc.defined(name) {
		return nil, nil, evalErrorf("variable %%%s is already defined", name)
	}
	value := focus
	if len(args) == 2 {
		value, err = ev.eval(args[1], focus, sc)
		if err != nil {
			return nil, nil, err
		}
	}
	return focus, &scope{
		this: sc.this, index: sc.index, total: sc.total,
		name: name, value: value, parent: sc,
	}, nil
}

// isSystemVariable reports names that defineVariable may not shadow.
func isSystemVariable(name string) bool {
	switch name {
	case "resource", "context", "rootResource", "ucum", "sct", "loinc":
		return true
	}
	return false
}

// trace is a debugging hook. It passes its input through unchanged; the name
// and optional projection exist for engines that log, which this one does not.
func (ev *evaluator) trace(args []Expr, focus Collection, sc *scope) (Collection, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, evalErrorf("trace() takes one or two arguments")
	}
	if _, err := ev.eval(args[0], focus, sc); err != nil {
		return nil, err
	}
	if len(args) == 2 {
		if _, err := ev.eval(args[1], focus, sc); err != nil {
			return nil, err
		}
	}
	return focus, nil
}

func (ev *evaluator) ofType(args []Expr, focus Collection) (Collection, error) {
	if len(args) != 1 {
		return nil, evalErrorf("ofType() takes exactly one argument")
	}
	want, err := typeArgName(args[0])
	if err != nil {
		return nil, err
	}
	if err := ev.checkTypeName(want); err != nil {
		return nil, err
	}
	var out Collection
	for _, v := range focus {
		// ofType selects an exact type, not a hierarchy; see valueIsTypeMode.
		if ev.valueIsTypeMode(v, want, true) {
			out = append(out, v)
		}
	}
	return out, nil
}

func (ev *evaluator) isAsFn(op string, args []Expr, focus Collection) (Collection, error) {
	if len(args) != 1 {
		return nil, evalErrorf("%s() takes exactly one argument", op)
	}
	want, err := typeArgName(args[0])
	if err != nil {
		return nil, err
	}
	if err := ev.checkTypeName(want); err != nil {
		return nil, err
	}
	if focus.Empty() {
		if op == "is" {
			return nil, nil
		}
		return nil, nil
	}
	v, ok := focus.Single()
	if !ok {
		// Both is() and as() are singleton operations: asking whether a
		// collection "is" a type, or coercing it, is only meaningful for one
		// item.
		return nil, evalErrorf("%s() requires a collection of at most one item, got %d", op, len(focus))
	}
	matches := ev.valueIsTypeMode(v, want, op == "as")
	if op == "is" {
		return boolCollection(matches), nil
	}
	if matches {
		return one(v), nil
	}
	return nil, nil
}

// typeArgName reads a type name from an argument. Type arguments are written as
// bare identifiers -- ofType(Quantity) -- so they arrive as an invocation chain
// rather than a string, and must be read structurally rather than evaluated.
func typeArgName(e Expr) (string, error) {
	switch n := e.(type) {
	case *Invocation:
		if n.IsFunction() {
			return "", evalErrorf("expected a type name, found a function call")
		}
		if n.Subject == nil {
			return n.Name, nil
		}
		prefix, err := typeArgName(n.Subject)
		if err != nil {
			return "", err
		}
		return prefix + "." + n.Name, nil
	case *Literal:
		if n.Kind == LitString {
			return n.Text, nil
		}
	}
	return "", evalErrorf("expected a type name")
}

// ---- functions with evaluated arguments ----

func (ev *evaluator) simpleCall(name string, focus Collection, args []Collection) (Collection, error) {
	switch name {
	// Existence.
	case "empty":
		return boolCollection(focus.Empty()), nil
	case "count":
		return one(Integer(len(focus))), nil
	case "distinct":
		return distinct(focus), nil
	case "isDistinct":
		return boolCollection(len(distinct(focus)) == len(focus)), nil
	case "allTrue":
		return everyBool(focus, true, true)
	case "anyTrue":
		return everyBool(focus, true, false)
	case "allFalse":
		return everyBool(focus, false, true)
	case "anyFalse":
		return everyBool(focus, false, false)
	case "subsetOf":
		if len(args) != 1 {
			return nil, evalErrorf("subsetOf() takes exactly one argument")
		}
		return boolCollection(isSubset(focus, args[0])), nil
	case "supersetOf":
		if len(args) != 1 {
			return nil, evalErrorf("supersetOf() takes exactly one argument")
		}
		return boolCollection(isSubset(args[0], focus)), nil

	// Subsetting.
	case "single":
		if focus.Empty() {
			return nil, nil
		}
		if len(focus) > 1 {
			return nil, evalErrorf("single() requires a collection of at most one item, got %d", len(focus))
		}
		return focus, nil
	case "first":
		if focus.Empty() {
			return nil, nil
		}
		return one(focus[0]), nil
	case "last":
		if focus.Empty() {
			return nil, nil
		}
		return one(focus[len(focus)-1]), nil
	case "tail":
		if len(focus) < 2 {
			return nil, nil
		}
		return focus[1:], nil
	case "skip":
		n, ok := asInt(args[0])
		if len(args) != 1 || !ok {
			return nil, evalErrorf("skip() takes a single integer")
		}
		if n <= 0 {
			return focus, nil
		}
		if int(n) >= len(focus) {
			return nil, nil
		}
		return focus[n:], nil
	case "take":
		n, ok := asInt(args[0])
		if len(args) != 1 || !ok {
			return nil, evalErrorf("take() takes a single integer")
		}
		if n <= 0 {
			return nil, nil
		}
		if int(n) >= len(focus) {
			return focus, nil
		}
		return focus[:n], nil
	case "intersect":
		if len(args) != 1 {
			return nil, evalErrorf("intersect() takes exactly one argument")
		}
		var out Collection
		for _, v := range distinct(focus) {
			if containsValue(args[0], v) {
				out = append(out, v)
			}
		}
		return out, nil
	case "exclude":
		if len(args) != 1 {
			return nil, evalErrorf("exclude() takes exactly one argument")
		}
		var out Collection
		for _, v := range focus {
			if !containsValue(args[0], v) {
				out = append(out, v)
			}
		}
		return out, nil

	// Combining.
	case "union":
		if len(args) != 1 {
			return nil, evalErrorf("union() takes exactly one argument")
		}
		return union(focus, args[0]), nil
	case "combine":
		if len(args) != 1 {
			return nil, evalErrorf("combine() takes exactly one argument")
		}
		return append(append(Collection{}, focus...), args[0]...), nil

	// Tree navigation.
	case "children":
		return childrenOf(focus), nil
	case "descendants":
		return descendantsOf(focus), nil

	// FHIR-specific.
	case "extension":
		if len(args) != 1 {
			return nil, evalErrorf("extension() takes exactly one argument")
		}
		url, ok := asString(args[0])
		if !ok {
			return nil, evalErrorf("extension() takes a string url")
		}
		return extensionsWithURL(focus, url), nil
	case "hasValue":
		v, ok := focus.Single()
		if !ok {
			return boolCollection(false), nil
		}
		if n, isNode := v.(Node); isNode {
			_, isPrim := n.Primitive()
			return boolCollection(isPrim), nil
		}
		return boolCollection(true), nil
	case "getValue":
		v, ok := focus.Single()
		if !ok {
			return nil, nil
		}
		return one(unwrap(v)), nil
	case "resolve":
		return ev.resolve(focus), nil
	case "conformsTo":
		if len(args) != 1 {
			return nil, evalErrorf("conformsTo() takes exactly one argument")
		}
		profile, profileOK := asString(args[0])
		if !profileOK {
			return nil, evalErrorf("conformsTo() takes a profile url")
		}
		v, single := focus.Single()
		if !single {
			return nil, nil
		}
		node, isNode := v.(Node)
		if !isNode || ev.ctx.ConformsTo == nil {
			return nil, nil
		}
		conforms, known := ev.ctx.ConformsTo(node, profile)
		if !known {
			return nil, evalErrorf("cannot resolve profile %q", profile)
		}
		return boolCollection(conforms), nil
	case "precision":
		return precisionOf(focus)
	case "type":
		return typeOf(focus)
	case "comparable":
		if len(args) != 1 {
			return nil, evalErrorf("comparable() takes exactly one argument")
		}
		a, aOK := unwrapSingle(focus).(Quantity)
		b, bOK := unwrapSingle(args[0]).(Quantity)
		if !aOK || !bOK {
			return nil, evalErrorf("comparable() compares two quantities")
		}
		return boolCollection(comparableQuantities(a, b)), nil

	// Utility.
	case "not":
		if focus.Empty() {
			return nil, nil
		}
		v, ok := singletonBool(focus)
		if !ok {
			return nil, evalErrorf("not() requires a boolean")
		}
		return boolCollection(!v), nil
	}

	if res, handled, err := ev.boundaryCall(name, focus, args); handled {
		return res, err
	}
	return ev.stringMathCall(name, focus, args)
}

func (ev *evaluator) resolve(focus Collection) Collection {
	if ev.ctx.ResolveReference == nil {
		return nil
	}
	var out Collection
	for _, v := range focus {
		ref := referenceString(v)
		if ref == "" {
			continue
		}
		if n := ev.ctx.ResolveReference(ref); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// referenceString pulls the reference URL out of a value: either a string, or a
// Reference element's "reference" child.
func referenceString(v Value) string {
	if s, ok := unwrap(v).(String_); ok {
		return string(s)
	}
	node, ok := v.(Node)
	if !ok {
		return ""
	}
	for _, child := range node.Children("reference") {
		if p, isPrim := child.Primitive(); isPrim {
			if s, ok := p.(String_); ok {
				return string(s)
			}
		}
	}
	return ""
}

// extensionsWithURL selects the extension children whose url matches.
func extensionsWithURL(focus Collection, url string) Collection {
	var out Collection
	for _, v := range focus {
		node, ok := v.(Node)
		if !ok {
			continue
		}
		for _, ext := range node.Children("extension") {
			for _, u := range ext.Children("url") {
				if p, isPrim := u.Primitive(); isPrim {
					if s, ok := p.(String_); ok && string(s) == url {
						out = append(out, ext)
					}
				}
			}
		}
	}
	return out
}

func childrenOf(focus Collection) Collection {
	var out Collection
	for _, v := range focus {
		if node, ok := v.(Node); ok {
			for _, c := range node.Children("") {
				out = append(out, c)
			}
		}
	}
	return out
}

// descendantsOf is children() applied transitively. Depth is bounded because a
// malformed document could otherwise cycle.
func descendantsOf(focus Collection) Collection {
	var out Collection
	current := childrenOf(focus)
	for depth := 0; !current.Empty() && depth < 100; depth++ {
		out = append(out, current...)
		current = childrenOf(current)
	}
	return out
}

// everyBool backs allTrue/anyTrue/allFalse/anyFalse. want selects which boolean
// to look for; all decides whether every item must match or just one.
func everyBool(focus Collection, want, all bool) (Collection, error) {
	if focus.Empty() {
		// An empty collection satisfies "all" vacuously and fails "any".
		return boolCollection(all), nil
	}
	for _, v := range focus {
		b, ok := unwrap(v).(Boolean)
		if !ok {
			return nil, evalErrorf("expected a collection of booleans")
		}
		if bool(b) == want && !all {
			return boolCollection(true), nil
		}
		if bool(b) != want && all {
			return boolCollection(false), nil
		}
	}
	return boolCollection(all), nil
}

func distinct(c Collection) Collection {
	var out Collection
	for _, v := range c {
		if !containsValue(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func containsValue(c Collection, v Value) bool {
	for _, item := range c {
		if valuesEqual(item, v) {
			return true
		}
	}
	return false
}

func isSubset(sub, super Collection) bool {
	for _, v := range sub {
		if !containsValue(super, v) {
			return false
		}
	}
	return true
}
