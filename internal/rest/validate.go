package rest

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/validate"
)

// The $validate operation, and validation on write.
//
// $validate answers "would this be accepted, and what is wrong with it" without
// storing anything. Its result is always an OperationOutcome, and -- this is the
// part servers get wrong -- a resource with problems is still a *successful*
// operation: the question was answered. Only a malformed request is an error
// status.

// handleValidateType: POST /{type}/$validate
func (s *Server) handleValidateType(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	s.validateRequest(w, r, resourceType, "")
}

// handleValidateInstance: POST /{type}/{id}/$validate
//
// With a body it validates that body as a replacement for the instance; with
// none it validates what is stored, which is how a client asks whether
// something it saved earlier still conforms.
func (s *Server) handleValidateInstance(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	s.validateRequest(w, r, resourceType, r.PathValue("id"))
}

func (s *Server) validateRequest(w http.ResponseWriter, r *http.Request, resourceType, id string) {
	values := r.URL.Query()
	mode := values.Get("mode")
	switch mode {
	case "", "create", "update", "profile":
	case "delete":
		// Deleting needs no document, and what the server would check is
		// referential integrity it does not track.
		s.write(w, r, http.StatusOK, outcome(s.index, Issue{
			Severity: severityInformation, Code: "informational",
			Diagnostics: "this server places no constraints on deletion, so mode=delete always passes",
		}), nil)
		return
	default:
		s.fail(w, r, http.StatusBadRequest,
			"mode must be create, update, delete, or profile, got %q", mode)
		return
	}

	node, ok := s.validationTarget(w, r, resourceType, id)
	if !ok {
		return
	}
	if node.FHIRType() != resourceType {
		s.write(w, r, http.StatusOK, outcome(s.index, Issue{
			Severity: severityError, Code: "invalid",
			Diagnostics: fmt.Sprintf("the resource is a %s but was sent to %s/$validate",
				node.FHIRType(), resourceType),
		}), nil)
		return
	}

	opts := validate.Options{}
	for _, raw := range values["profile"] {
		for _, url := range strings.Split(raw, ",") {
			if url = strings.TrimSpace(url); url != "" {
				opts.Profiles = append(opts.Profiles, url)
			}
		}
	}

	issues := s.validator.Validate(node, opts)
	// The operation succeeded whatever it found; the outcome carries the
	// verdict. A 400 here would say the *request* was wrong, which it was not.
	s.write(w, r, http.StatusOK, outcome(s.index, translateIssues(issues)...), nil)
}

// validationTarget reads the document to validate, from the request body or,
// when there is none, from storage.
func (s *Server) validationTarget(w http.ResponseWriter, r *http.Request, resourceType, id string) (*resource.Node, bool) {
	if r.ContentLength != 0 {
		return s.readResource(w, r)
	}
	if id == "" {
		s.fail(w, r, http.StatusBadRequest, "$validate needs a resource to validate")
		return nil, false
	}
	res, err := s.backend(r.Context()).Read(r.Context(), resourceType, id)
	if err != nil {
		s.failStorage(w, r, err)
		return nil, false
	}
	node, err := s.stored(res)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "stored resource is unreadable: %v", err)
		return nil, false
	}
	return node, true
}

// translateIssues turns validator findings into OperationOutcome issues,
// adding the "all clear" one the specification asks for when there is nothing
// to report.
func translateIssues(issues []validate.Issue) []Issue {
	if len(issues) == 0 {
		return []Issue{{
			Severity: severityInformation, Code: "informational",
			Diagnostics: "validation succeeded: no issues found",
		}}
	}
	out := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		converted := Issue{
			Severity:    issue.Severity,
			Code:        issue.Code,
			Diagnostics: issue.Details,
		}
		if issue.Path != "" {
			converted.Expression = []string{issue.Path}
		}
		out = append(out, converted)
	}
	return out
}

// validateOnWrite checks a resource being stored, when the server is configured
// to.
//
// It is off by default. This server exists to be developed against, and a
// client working through a data model wants its half-finished resources to
// round-trip; a server that refuses them is one the developer works around
// rather than with. Turning it on makes the server a stricter target, which is
// exactly what a conformance run wants.
func (s *Server) validateOnWrite(w http.ResponseWriter, r *http.Request, node *resource.Node) bool {
	if !s.validateWrites {
		return true
	}
	issues := s.validator.Validate(node, validate.Options{})
	if !validate.HasErrors(issues) {
		return true
	}
	// 422 rather than 400: the request was well-formed and understood, and the
	// content is what failed.
	s.write(w, r, http.StatusUnprocessableEntity, outcome(s.index, translateIssues(issues)...), nil)
	return false
}
