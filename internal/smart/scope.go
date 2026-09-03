// Package smart implements SMART App Launch: the authorization layer a FHIR
// server needs before anyone will point a real app at it.
//
// It is off unless asked for. gogofhir exists to be developed against, and a
// server that demands an access token before it will answer `GET /Patient` is
// one a developer spends the first afternoon working around. Turned on, it is
// the whole flow -- discovery, authorization with PKCE, tokens, scopes, and
// launch context -- because a conformance target that only pretends to
// authorize is worse than one that does not try.
package smart

import (
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
)

// Scope is one SMART scope: who is asking, about what, and what they may do.
//
// Two syntaxes are in use and both are accepted. SMART v1 spells permissions
// "read", "write" or "*"; v2 spells them as letters from "cruds" -- create,
// read, update, delete, search -- so that "read" can be split into reading one
// resource and searching for many. v2 is the syntax to write; v1 is what most
// existing apps send.
type Scope struct {
	// Context is "patient", "user", or "system".
	//
	// patient/ is limited to one patient's compartment, user/ to what the
	// authorized user may see, and system/ has no such limit -- it is for
	// backend services acting on their own behalf.
	Context string
	// Resource is a resource type, or "*" for every type.
	Resource string
	// Permissions are the letters granted: c, r, u, d, s.
	Permissions string
}

// The permission letters, in the order the specification writes them.
const (
	PermCreate = 'c'
	PermRead   = 'r'
	PermUpdate = 'u'
	PermDelete = 'd'
	PermSearch = 's'
)

// ParseScopes reads a space-separated scope string, keeping only the scopes
// that grant access to resources.
//
// Everything else a client asks for -- openid, fhirUser, launch,
// offline_access -- is about the flow rather than about data, and is handled
// where it matters rather than being carried around as a permission.
func ParseScopes(raw string) []Scope {
	var out []Scope
	for _, field := range strings.Fields(raw) {
		if scope, ok := parseScope(field); ok {
			out = append(out, scope)
		}
	}
	return out
}

func parseScope(raw string) (Scope, bool) {
	context, rest, found := strings.Cut(raw, "/")
	if !found {
		return Scope{}, false
	}
	switch context {
	case "patient", "user", "system":
	default:
		return Scope{}, false
	}
	// A v2 scope may carry a search-parameter filter after "?", which narrows
	// what the grant covers. Enforcing it needs the filter applied to every
	// query, which this server does not do, so a scope carrying one is refused
	// rather than silently widened to the whole resource type.
	if strings.Contains(rest, "?") {
		return Scope{}, false
	}
	resource, permissions, found := strings.Cut(rest, ".")
	if !found || resource == "" {
		return Scope{}, false
	}

	switch permissions {
	case "read":
		// v1 "read" covers reading an instance and searching for many.
		permissions = "rs"
	case "write":
		permissions = "cud"
	case "*":
		permissions = "cruds"
	default:
		// v2: a subset of "cruds", in any order, and nothing else.
		for _, c := range permissions {
			if !strings.ContainsRune("cruds", c) {
				return Scope{}, false
			}
		}
		if permissions == "" {
			return Scope{}, false
		}
	}
	return Scope{Context: context, Resource: resource, Permissions: permissions}, true
}

// String renders a scope in v2 syntax.
func (s Scope) String() string { return s.Context + "/" + s.Resource + "." + s.Permissions }

// Grants reports whether a scope permits an operation on a resource type.
func (s Scope) Grants(resourceType string, permission rune) bool {
	if s.Resource != "*" && s.Resource != resourceType {
		return false
	}
	return strings.ContainsRune(s.Permissions, permission)
}

// Grant is the set of scopes an access token carries, with the launch context
// that came with them.
type Grant struct {
	Scopes []Scope
	// Patient is the launch context patient's id, when the token was issued for
	// one. patient/ scopes are confined to that patient's compartment; without
	// it they grant nothing, because "this patient" would name nobody.
	Patient string
	// Subject is the authenticated user, for the fhirUser claim.
	Subject  string
	ClientID string
}

// Allows reports whether the grant permits an operation, and which context
// allowed it -- which is what decides whether the result must then be confined
// to a compartment.
func (g Grant) Allows(resourceType string, permission rune) (context string, ok bool) {
	// The widest context wins: a token holding both user/ and patient/ scopes
	// for the same resource is not confined by the patient one.
	best := ""
	for _, scope := range g.Scopes {
		if !scope.Grants(resourceType, permission) {
			continue
		}
		if scope.Context == "patient" && g.Patient == "" {
			// A patient scope with no launch context names nobody.
			continue
		}
		switch {
		case scope.Context == "system", scope.Context == "user":
			return scope.Context, true
		case best == "":
			best = scope.Context
		}
	}
	return best, best != ""
}

// CompartmentFilter builds the _filter expression confining a resource type to
// one patient's compartment.
//
// The compartment definitions are already in the conformance index, which makes
// this an answer rather than a hardcoded table: whichever release is being
// served says which parameters link a type to a patient. Most types have
// several -- an Observation is in a patient's compartment through subject *or*
// performer -- so the confinement is a disjunction, which is precisely what
// _filter exists to express and what a plain search parameter cannot.
//
// Taking only the first parameter would be the tempting simplification and the
// wrong one: it would hide a patient's own records from an app authorized to
// see them, which reads as data loss rather than as a permission error.
func CompartmentFilter(idx *conformance.Index, resourceType, patientID string) (string, bool) {
	compartment, ok := idx.Compartments["Patient"]
	if !ok {
		return "", false
	}
	var terms []string
	for _, code := range compartment.Params[resourceType] {
		// "{def}" is the specification's marker for the compartment's own
		// resource, which is named by its id rather than by a reference.
		if code == "{def}" {
			terms = append(terms, "_id eq "+patientID)
			continue
		}
		terms = append(terms, code+" eq Patient/"+patientID)
	}
	if len(terms) == 0 {
		return "", false
	}
	if len(terms) == 1 {
		return terms[0], true
	}
	return "(" + strings.Join(terms, " or ") + ")", true
}
