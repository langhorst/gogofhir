// Package rest is the FHIR RESTful API.
//
// Two rules shape it. Every error carries an OperationOutcome rather than a
// bare status code, because that is what the specification requires and what
// conformance suites check. And every response goes through one negotiation
// path, so JSON and XML clients see the same server.
package rest

import (
	"net/http"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
)

const (
	severityError   = "error"
	severityWarning = "warning"
)

// Issue is one problem in an OperationOutcome.
type Issue struct {
	Severity    string
	Code        string
	Diagnostics string
	// Expression locates the problem within a resource, when it is about
	// content rather than about the request.
	Expression []string
}

// outcome builds an OperationOutcome document.
//
// It is assembled as a plain document rather than from a generated struct, for
// the same reason resources are: the conformance index already knows the shape,
// and one serialization path means the result is right in both formats.
func outcome(idx *conformance.Index, issues ...Issue) *resource.Node {
	entries := make([]any, 0, len(issues))
	for _, issue := range issues {
		entry := map[string]any{"severity": issue.Severity, "code": issue.Code}
		if issue.Diagnostics != "" {
			entry["diagnostics"] = issue.Diagnostics
		}
		if len(issue.Expression) > 0 {
			exprs := make([]any, len(issue.Expression))
			for i, e := range issue.Expression {
				exprs[i] = e
			}
			entry["expression"] = exprs
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		entries = append(entries, map[string]any{"severity": severityError, "code": "processing"})
	}
	node, err := resource.New(idx, map[string]any{
		"resourceType": "OperationOutcome",
		"issue":        entries,
	})
	if err != nil {
		// The shape is fixed and OperationOutcome exists in every release; a
		// failure here would mean the index itself is broken.
		panic("rest: building OperationOutcome: " + err.Error())
	}
	return node
}

// issueCodeForStatus maps a status onto the IssueType the specification
// prescribes, so clients can branch on the outcome rather than on the status.
func issueCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid"
	case http.StatusUnauthorized:
		return "login"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not-found"
	case http.StatusMethodNotAllowed:
		return "not-supported"
	case http.StatusConflict, http.StatusPreconditionFailed:
		return "conflict"
	case http.StatusGone:
		return "deleted"
	case http.StatusUnsupportedMediaType:
		return "not-supported"
	case http.StatusUnprocessableEntity:
		return "processing"
	default:
		if status >= 500 {
			return "exception"
		}
		return "processing"
	}
}
