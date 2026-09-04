package rest

import (
	"fmt"
	"strings"

	"github.com/langhorst/gogofhir/internal/resource"
	"net/http"
	"net/url"
)

// Parsing a bundle's entries into executable form.

// parseEntries reads the bundle's entries into executable form. Nothing here
// touches storage, so a malformed bundle is refused before any work begins.
func parseEntries(s *Server, obj map[string]any) ([]*txEntry, error) {
	raw, _ := obj["entry"].([]any)
	if len(raw) > maxBundleEntries {
		return nil, &searchError{fmt.Sprintf(
			"the bundle has %d entries; this server accepts at most %d", len(raw), maxBundleEntries)}
	}

	entries := make([]*txEntry, 0, len(raw))
	for i, item := range raw {
		item, ok := item.(map[string]any)
		if !ok {
			return nil, &searchError{fmt.Sprintf("entry %d is not an object", i)}
		}
		entry := &txEntry{entryRequest: entryRequest{position: i}}
		entry.fullURL, _ = item["fullUrl"].(string)

		request, _ := item["request"].(map[string]any)
		if request == nil {
			return nil, &searchError{fmt.Sprintf(
				"entry %d has no request; every entry of a transaction or batch needs one", i)}
		}
		entry.method, _ = request["method"].(string)
		entry.method = strings.ToUpper(entry.method)
		rawURL, _ := request["url"].(string)
		entry.ifNoneExist, _ = request["ifNoneExist"].(string)
		entry.ifMatch, _ = request["ifMatch"].(string)
		entry.ifNoneMatch, _ = request["ifNoneMatch"].(string)

		switch entry.method {
		case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete:
		case http.MethodPatch:
			// PATCH is a distinct interaction with its own body formats (JSON
			// Patch, FHIRPath Patch) that this server does not implement yet.
			// Accepting it in a bundle and quietly doing nothing would be worse
			// than saying so.
			return nil, &searchError{fmt.Sprintf(
				"entry %d uses PATCH, which this server does not implement yet", i)}
		default:
			return nil, &searchError{fmt.Sprintf(
				"entry %d has an unusable request.method %q", i, entry.method)}
		}
		if rawURL == "" {
			return nil, &searchError{fmt.Sprintf("entry %d has no request.url", i)}
		}
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return nil, &searchError{fmt.Sprintf("entry %d has a malformed request.url %q", i, rawURL)}
		}
		entry.path = strings.Trim(parsed.Path, "/")
		entry.query = parsed.RawQuery
		entry.resourceType, _, _ = strings.Cut(entry.path, "/")

		if nested, ok := item["resource"].(map[string]any); ok {
			node, err := resource.New(s.index, nested)
			if err != nil {
				return nil, &searchError{fmt.Sprintf("entry %d holds an unreadable resource: %v", i, err)}
			}
			entry.node = node
		}
		if entry.node == nil && (entry.method == http.MethodPost || entry.method == http.MethodPut) {
			return nil, &searchError{fmt.Sprintf(
				"entry %d is a %s but carries no resource", i, entry.method)}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
