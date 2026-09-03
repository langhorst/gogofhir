package rest

import (
	"net/http"
	"time"

	"github.com/langhorst/gogofhir/internal/resource"
)

// The CapabilityStatement is generated from the conformance index rather than
// written out, so it cannot drift from what the server actually does: the
// resource types it advertises are the types the index defines, and the search
// parameters are the ones extraction indexes.
//
// A hand-maintained statement is worse than none. Clients configure themselves
// from it, and conformance suites read it to decide what to test, so one that
// overstates the server sends everyone down paths that do not work.

// softwareVersion is stamped at release time by the daemon.
var softwareVersion = "dev"

// SetSoftwareVersion records the build version advertised in /metadata.
func SetSoftwareVersion(v string) {
	if v != "" {
		softwareVersion = v
	}
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	// "mode" selects a variant; only the full statement is offered, and asking
	// for another is better refused than silently answered with this one.
	if mode := r.URL.Query().Get("mode"); mode != "" && mode != "full" {
		s.fail(w, r, http.StatusBadRequest, "unsupported metadata mode %q", mode)
		return
	}
	node, err := s.capabilityStatement(r)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "building the CapabilityStatement failed: %v", err)
		return
	}
	s.write(w, r, http.StatusOK, node, nil)
}

func (s *Server) capabilityStatement(r *http.Request) (*resource.Node, error) {
	resources := make([]any, 0, len(s.Index.ResourceTypes()))
	for _, typeName := range s.Index.ResourceTypes() {
		resources = append(resources, s.capabilityForType(typeName))
	}

	statement := map[string]any{
		"resourceType": "CapabilityStatement",
		"status":       "active",
		"date":         time.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		"kind":         "instance",
		"software": map[string]any{
			"name":    "gogofhir",
			"version": softwareVersion,
		},
		"implementation": map[string]any{
			"description": "gogofhir development and conformance server",
			"url":         s.base(r),
		},
		"fhirVersion": s.Index.FHIRVersion,
		"format":      []any{"application/fhir+json", "application/fhir+xml"},
		"rest": []any{map[string]any{
			"mode":     "server",
			"resource": resources,
			"interaction": []any{
				map[string]any{"code": "history-system"},
			},
			// Declared once for the server rather than per resource: these
			// shape a response rather than filter it.
			"searchParam": []any{
				map[string]any{"name": "_count", "type": "number",
					"documentation": "Page size. Paging is by cursor: follow the bundle's next link."},
				map[string]any{"name": "_sort", "type": "string"},
				map[string]any{"name": "_summary", "type": "token"},
				map[string]any{"name": "_elements", "type": "string"},
				map[string]any{"name": "_total", "type": "token"},
				map[string]any{"name": "_filter", "type": "string",
					"documentation": "Filter expressions, with and/or/not over the parameters below."},
			},
		}},
	}
	return resource.New(s.Index, statement)
}

// capabilityForType describes one resource type: the interactions implemented
// and the search parameters that are actually indexed.
func (s *Server) capabilityForType(typeName string) map[string]any {
	interactions := []any{
		map[string]any{"code": "read"},
		map[string]any{"code": "vread"},
		map[string]any{"code": "update"},
		map[string]any{"code": "delete"},
		map[string]any{"code": "history-instance"},
		map[string]any{"code": "history-type"},
		map[string]any{"code": "create"},
		map[string]any{"code": "search-type"},
	}

	// The common parameters are handled by the server rather than extracted
	// from an expression, so they are not in the index and must be declared
	// here or clients will not know they work.
	params := []any{
		map[string]any{"name": "_id", "type": "token"},
		map[string]any{"name": "_lastUpdated", "type": "date"},
		map[string]any{"name": "_text", "type": "special",
			"documentation": "Full-text search over the resource's narrative."},
		map[string]any{"name": "_content", "type": "special",
			"documentation": "Full-text search over the resource's text values."},
	}
	var includes []any
	for _, sp := range s.Index.SearchParamsFor(typeName) {
		if _, indexed := indexKindFor(sp.Type); !indexed && sp.Type != "composite" {
			// The "special" parameters -- near, and the like -- are declared by
			// the specification but not indexed here. Advertising them would
			// promise searches that return nothing.
			continue
		}
		params = append(params, map[string]any{
			"name": sp.Code,
			"type": sp.Type,
		})
		if sp.Type == "reference" {
			includes = append(includes, typeName+":"+sp.Code)
		}
	}

	entry := map[string]any{
		"type":              typeName,
		"interaction":       interactions,
		"versioning":        "versioned",
		"readHistory":       true,
		"updateCreate":      true,
		"conditionalCreate": true,
		"conditionalUpdate": true,
		"conditionalDelete": "single",
		"conditionalRead":   "modified-since",
	}
	entry["searchParam"] = params
	if len(includes) > 0 {
		entry["searchInclude"] = includes
	}
	// searchRevInclude is deliberately absent: every reference parameter on
	// every type that can point here would qualify, and the list is quadratic
	// in the number of resource types. _revinclude is supported regardless.
	return entry
}
