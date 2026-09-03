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

	var params []any
	for _, sp := range s.Index.SearchParamsFor(typeName) {
		if _, indexed := indexKindFor(sp.Type); !indexed {
			// Composite and "special" parameters are declared by the
			// specification but not yet indexed here. Advertising them would
			// promise searches that return nothing.
			continue
		}
		params = append(params, map[string]any{
			"name": sp.Code,
			"type": sp.Type,
		})
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
	if len(params) > 0 {
		entry["searchParam"] = params
	}
	return entry
}
