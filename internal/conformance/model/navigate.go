package model

import (
	"strings"
)

// Navigating the type system.
//
// Three things used to walk element paths on their own -- JSON navigation, the
// XML reader, and the profile compiler -- each with its own handling of choice
// expansions, contentReference, and whether a child stays inside its owning
// type or moves to a datatype. One of them had a bug the other two did not.
// This file is the single implementation they now share.
//
// The vocabulary is a Cursor, which is a position in the type system, and a
// Step, which is what descending one element name from a cursor yields.

// Cursor is a position in the type system: an element path within the type
// whose element list defines it.
//
// The root of a type is (def, ""). A backbone element deeper inside is the
// same def with a longer path, because FHIR defines backbones inline in their
// owner's snapshot -- Patient.contact.name is an element of Patient, not of
// some standalone Contact type. A datatype-valued element starts a new cursor
// at that datatype's root.
type Cursor struct {
	Def  *TypeDef
	Path string
}

// Child is one element definition directly below a cursor.
type Child struct {
	// Name is the element's name relative to the cursor, with a choice
	// element's "[x]" already stripped: "deceased", not "deceased[x]".
	Name string
	Def  *ElementDef
}

// Step is what descending one element name from a cursor yields.
type Step struct {
	// Element is the definition the name resolved to. For a choice expansion
	// such as "valueQuantity" it is the choice element itself, whose
	// cardinality governs every expansion.
	Element *ElementDef
	// Type is the FHIR type of the value found under the name: the element's
	// declared type, the expansion's type for a choice, or "BackboneElement"
	// for an element defined only by a contentReference. It is empty when a
	// choice element is addressed by its base name, since which type the
	// document used is not something the name says.
	Type string
	// Child is where the value's own children are defined: inside the owner
	// for a backbone, at a datatype's root otherwise. Its Def is nil for a
	// nested resource, whose definition comes from the document, and for a
	// type the index does not know.
	Child Cursor
	// Nested reports a Resource- or DomainResource-typed element -- contained
	// resources, bundle entries -- whose real type is the document's own
	// resourceType.
	Nested bool
}

// Resolve follows a contentReference at the cursor.
//
// FHIR expresses recursive structures that way: Questionnaire.item.item is
// defined as "#Questionnaire.item". Without following it, navigation stops
// one level deep and silently truncates a nested document.
func (i *Index) Resolve(c Cursor) Cursor {
	if c.Def == nil {
		return c
	}
	el, ok := c.Def.Element(c.Path)
	if !ok || el.ContentReference == "" {
		return c
	}
	target := strings.TrimPrefix(el.ContentReference, "#")
	typeName, rest, _ := strings.Cut(target, ".")
	def, ok := i.Type(typeName)
	if !ok {
		return c
	}
	return Cursor{Def: def, Path: rest}
}

// Children lists the element definitions one level below a cursor, in
// snapshot order, with any contentReference already followed.
func (i *Index) Children(c Cursor) []Child {
	c = i.Resolve(c)
	if c.Def == nil {
		return nil
	}
	return c.Def.childrenAt(c.Path)
}

// Step descends one element name from a cursor. The name may be the element's
// own or an expansion of a choice element at that level: "valueQuantity"
// resolves to the "value" choice with type Quantity.
//
// Expansions are matched among the cursor's own children rather than anywhere
// in the type. Observation.value[x] and Observation.component.value[x] share
// every expansion name, and a lookup that ignored the level would pick one
// element's cardinality for the other's occurrences.
func (i *Index) Step(c Cursor, name string) (Step, bool) {
	c = i.Resolve(c)
	if c.Def == nil {
		return Step{}, false
	}

	var el *ElementDef
	typeCode := ""
	for _, child := range c.Def.childrenAt(c.Path) {
		if child.Name == name {
			el = child.Def
			if !el.Choice && len(el.Types) > 0 {
				typeCode = el.Types[0].Code
			}
			break
		}
		if child.Def.Choice {
			for j, expansion := range child.Def.Expansions {
				if expansion == name && j < len(child.Def.Types) {
					el, typeCode = child.Def, child.Def.Types[j].Code
					break
				}
			}
			if el != nil {
				break
			}
		}
	}
	if el == nil {
		return Step{}, false
	}

	// An element defined only by a contentReference declares no type of its
	// own; it is a backbone whose shape lives at the referenced path, which
	// Resolve finds on the next descent.
	if typeCode == "" && el.ContentReference != "" {
		typeCode = "BackboneElement"
	}

	inline := Cursor{Def: c.Def, Path: joinPath(c.Path, el.Path)}
	step := Step{Element: el, Type: typeCode}
	switch typeCode {
	case "":
		// A choice addressed by its base name: no single type to descend into.
	case "BackboneElement", "Element":
		// Backbones are defined inline in the owning type, so the cursor stays
		// put and the path grows.
		step.Child = inline
	case "Resource", "DomainResource":
		step.Nested = true
	default:
		if def, ok := i.Type(typeCode); ok {
			step.Child = Cursor{Def: def}
		} else {
			// An unknown type: keep the position navigable, definition-less.
			step.Child = inline
		}
	}
	return step, true
}

// Walk descends a dotted path from a cursor and returns the element it names.
//
// It stops at a choice element addressed by its base name -- "value.unit"
// cannot know whether value is a Quantity -- and at a nested resource, whose
// elements belong to whatever the document turns out to hold.
func (i *Index) Walk(c Cursor, path string) (*ElementDef, bool) {
	if c.Def == nil || path == "" {
		return nil, false
	}
	segments := strings.Split(path, ".")
	for n, segment := range segments {
		step, ok := i.Step(c, segment)
		if !ok {
			return nil, false
		}
		if n == len(segments)-1 {
			return step.Element, true
		}
		if step.Type == "" || step.Nested || step.Child.Def == nil {
			return nil, false
		}
		c = step.Child
	}
	return nil, false
}

// joinPath appends an element's own path to a cursor's. Element paths are
// already relative to the type, so the owner's path is a prefix of them and
// the element's path is the answer whenever the cursor is inside the type.
func joinPath(cursorPath, elementPath string) string {
	if cursorPath == "" || strings.HasPrefix(elementPath, cursorPath+".") {
		return elementPath
	}
	return cursorPath + "." + elementPath
}

// childrenAt lists the direct children of a path within the type, built once
// per type. Navigation asks this question at every step of every expression,
// and scanning the whole element list each time was the cost that dominated
// FHIRPath evaluation.
func (t *TypeDef) childrenAt(path string) []Child {
	t.childrenOnce.Do(func() {
		t.children = map[string][]Child{}
		for _, el := range t.Elements {
			parent, name := "", el.Path
			if i := strings.LastIndex(el.Path, "."); i >= 0 {
				parent, name = el.Path[:i], el.Path[i+1:]
			}
			t.children[parent] = append(t.children[parent], Child{Name: name, Def: el})
		}
	})
	return t.children[path]
}
