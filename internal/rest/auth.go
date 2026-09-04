package rest

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/langhorst/gogofhir/internal/smart"
)

// The resource server half of SMART: what a request may do, given the token it
// arrived with.
//
// Two things have to happen and both are easy to half-do. The scopes decide
// which resource types and which operations are permitted. And a patient-scoped
// token is confined to one patient's compartment -- which is not a scope check
// but a query rewrite, because a token that may read Observations may only read
// *that patient's* Observations, and a server enforcing only the first is one
// that leaks every other patient to any app that asks.

// permissionForMethod maps an interaction onto the permission letter it needs.
//
// A search and a read are separate permissions in SMART v2, which is the whole
// reason the letters exist: an app allowed to read the patient it was launched
// for is not thereby allowed to search across the server.
func permissionForMethod(r *http.Request, instance bool) rune {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if instance {
			return smart.PermRead
		}
		return smart.PermSearch
	case http.MethodPost:
		return smart.PermCreate
	case http.MethodPut, http.MethodPatch:
		return smart.PermUpdate
	case http.MethodDelete:
		return smart.PermDelete
	default:
		return smart.PermRead
	}
}

// authorize checks a request against the token it carries, and reports the
// grant so a search can be confined to a compartment.
//
// With SMART disabled every request is authorized and unconfined, which is the
// default: this server exists to be developed against, and one that demands a
// token before it will answer GET /Patient is one a developer spends the first
// afternoon working around.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, resourceType string, instance bool) (*smart.Grant, bool) {
	if s.SMART == nil {
		return nil, true
	}

	token, ok := bearerToken(r)
	if !ok {
		s.unauthorized(w, r, "invalid_token", "this server requires a SMART access token")
		return nil, false
	}
	grant, err := s.SMART.Verify(token)
	if err != nil {
		reason := "the access token is not valid"
		if errors.Is(err, smart.ErrTokenExpired) {
			reason = "the access token has expired"
		}
		s.unauthorized(w, r, "invalid_token", reason)
		return nil, false
	}

	// The system-level interactions -- metadata, transactions, whole-server
	// history -- are not about one resource type, so they are checked against
	// the request rather than against a type.
	if resourceType == "" {
		return &grant, true
	}

	permission := permissionForMethod(r, instance)
	context, allowed := grant.Allows(resourceType, permission)
	if !allowed {
		s.forbidden(w, r, resourceType, permission)
		return nil, false
	}
	if context != "patient" {
		return &grant, true
	}
	return &grant, true
}

// confine restricts a search to the launch context patient's compartment.
//
// This is the part that matters. A patient-scoped token may read Observations,
// but only the ones in its patient's compartment, and the compartment
// definitions in the conformance index say which search parameter ties each
// resource type to a patient. A type with no such parameter is not in the
// compartment at all, so the search returns nothing rather than everything --
// the failure mode has to be closed.
func (s *Server) confine(values url.Values, resourceType string, grant *smart.Grant) (url.Values, bool) {
	if s.SMART == nil || grant == nil || grant.Patient == "" {
		return values, true
	}
	context, allowed := grant.Allows(resourceType, smart.PermSearch)
	if !allowed || context != "patient" {
		return values, true
	}
	filter, ok := smart.CompartmentFilter(s.Index, resourceType, grant.Patient)
	if !ok {
		return values, false
	}
	// Added rather than set: _filter parameters conjoin, so the client's own
	// criteria survive and the compartment narrows them.
	confined := cloneValues(values)
	confined.Add("_filter", filter)
	return confined, true
}

// permits reports whether a patient-scoped grant may see a specific resource,
// for the instance-level interactions a compartment filter cannot reach.
//
// It is answered by searching the compartment for that one id, which is the
// same question the confined search asks and therefore cannot disagree with it.
func (s *Server) permits(r *http.Request, resourceType, id string, grant *smart.Grant) bool {
	if s.SMART == nil || grant == nil || grant.Patient == "" {
		return true
	}
	context, allowed := grant.Allows(resourceType, smart.PermRead)
	if !allowed || context != "patient" {
		return true
	}
	filter, ok := smart.CompartmentFilter(s.Index, resourceType, grant.Patient)
	if !ok {
		return false
	}
	values := url.Values{}
	values.Set("_id", id)
	values.Add("_filter", filter)
	q, _, err := parseSearch(s.Index, resourceType, values)
	if err != nil {
		return false
	}
	q.Count, q.SkipTotal = 1, true
	result, err := s.Store.Search(r.Context(), q)
	return err == nil && len(result.Matches) == 1
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values)+1)
	for key, list := range values {
		out[key] = append([]string(nil), list...)
	}
	return out
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if len(header) < 7 || !strings.EqualFold(header[:7], "bearer ") {
		return "", false
	}
	token := strings.TrimSpace(header[7:])
	return token, token != ""
}

// unauthorized answers a missing or bad token with 401 and the
// WWW-Authenticate header OAuth requires, so a client knows to go and get one
// rather than guessing why it was refused.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request, code, description string) {
	w.Header().Set("WWW-Authenticate",
		`Bearer realm="gogofhir", error="`+code+`", error_description="`+description+`"`)
	s.write(w, r, http.StatusUnauthorized, outcome(s.Index, Issue{
		Severity: severityError, Code: "login", Diagnostics: description,
	}), nil)
}

// forbidden answers a valid token that does not cover this request. It names
// the scope that would have, because "forbidden" without that is a puzzle.
func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, resourceType string, permission rune) {
	s.write(w, r, http.StatusForbidden, outcome(s.Index, Issue{
		Severity: severityError, Code: "forbidden",
		Diagnostics: "the access token does not grant " + string(permission) +
			" on " + resourceType + "; a scope such as user/" + resourceType +
			"." + string(permission) + " would",
	}), nil)
}

// ---- the choke point ----

type grantKey struct{}

// guard authorizes every request before it reaches a handler.
//
// One choke point rather than a check in each handler, deliberately: eleven
// handlers today and more later, and a check that has to be remembered on each
// new route is one that will eventually be forgotten. A forgotten validation
// check is a bug; a forgotten authorization check is a breach.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.SMART == nil || openPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// A bundle entry inherits the grant of the request that carried it: it
		// was authorized once, at the transaction, and re-presenting the token
		// to ourselves would prove nothing.
		if grantFrom(r) != nil {
			next.ServeHTTP(w, r)
			return
		}

		resourceType, instance := routeTarget(s, r.URL.Path)
		grant, ok := s.authorize(w, r, resourceType, instance)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey{}, grant)))
	})
}

// openPath names the endpoints that must answer without a token: the ones an
// app uses to find out how to get one, and the CapabilityStatement it reads to
// discover the server at all.
func openPath(path string) bool {
	switch {
	case path == "/metadata",
		strings.HasPrefix(path, "/.well-known/"),
		strings.HasPrefix(path, "/auth/"):
		return true
	}
	return false
}

// routeTarget reads the resource type out of a path, and whether the request
// names one instance.
//
// The guard runs before the mux has matched anything, so it cannot ask for a
// path value. A path that names no resource type -- the root, metadata,
// system history -- yields "", which the scope check treats as a system-level
// interaction.
func routeTarget(s *Server, path string) (resourceType string, instance bool) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", false
	}
	if !s.Index.IsResource(segments[0]) {
		return "", false
	}
	if len(segments) == 1 {
		return segments[0], false
	}
	// "Patient/_history" and "Patient/_search" are type-level; anything else
	// with a second segment names an instance.
	return segments[0], !strings.HasPrefix(segments[1], "_")
}

// grantFrom returns the grant the guard attached to a request, or nil when
// SMART is off.
func grantFrom(r *http.Request) *smart.Grant {
	grant, _ := r.Context().Value(grantKey{}).(*smart.Grant)
	return grant
}

// withGrant carries a grant into a request the server makes of itself, which is
// how a transaction's entries inherit the authorization of the bundle.
func withGrant(r *http.Request, grant *smart.Grant) *http.Request {
	if grant == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), grantKey{}, grant))
}

// outOfCompartment answers a request for a resource outside the token's
// patient compartment.
//
// It is 404 rather than 403 on purpose. Telling an app that a resource it may
// not see nonetheless exists is itself a disclosure, and one an attacker can
// enumerate; the resource is simply not there as far as this token is
// concerned.
func (s *Server) outOfCompartment(w http.ResponseWriter, r *http.Request, resourceType, id string) {
	s.fail(w, r, http.StatusNotFound, "resource not found")
}
