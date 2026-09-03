package rest

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

// Bundles carry every multi-resource response: search results, history, and
// later transactions.
//
// A search bundle distinguishes matches from resources pulled in alongside them
// through entry.search.mode. Nothing in M2 pulls anything in yet, but the
// distinction is mandatory and easy to omit, so it is set from the start rather
// than retrofitted when _include arrives.

// bundleEntry is one entry under construction.
type bundleEntry struct {
	fullURL string
	content []byte
	// mode is "match", "include", or "outcome" for a search bundle; empty
	// otherwise.
	mode string
	// request and response describe a history entry's interaction.
	method  string
	url     string
	status  string
	etag    string
	lastMod string
}

// searchBundle builds a searchset Bundle with the paging links a client follows.
func searchBundle(idx *conformance.Index, base string, requestURL *url.URL, results []*storage.Resource, total int, q storage.SearchQuery) (*resource.Node, error) {
	entries := make([]bundleEntry, 0, len(results))
	for _, res := range results {
		entries = append(entries, bundleEntry{
			fullURL: fmt.Sprintf("%s/%s/%s", base, res.Type, res.ID),
			content: res.Content,
			mode:    "match",
		})
	}
	links := pagingLinks(requestURL, total, q)
	return buildBundle(idx, "searchset", &total, entries, links)
}

// historyBundle builds a history Bundle. Its entries carry request and response
// details rather than search modes, because a history entry describes an
// interaction rather than a match.
func historyBundle(idx *conformance.Index, base string, versions []*storage.Resource) (*resource.Node, error) {
	entries := make([]bundleEntry, 0, len(versions))
	for _, v := range versions {
		entry := bundleEntry{
			fullURL: fmt.Sprintf("%s/%s/%s", base, v.Type, v.ID),
			content: v.Content,
			url:     v.Type + "/" + v.ID,
			etag:    etagFor(v.VersionID),
			lastMod: httpDate(v.LastUpdated),
		}
		switch {
		case v.Deleted:
			entry.method, entry.status = "DELETE", "204 No Content"
		case v.VersionID == 1:
			entry.method, entry.status = "POST", "201 Created"
			entry.url = v.Type
		default:
			entry.method, entry.status = "PUT", "200 OK"
		}
		entries = append(entries, entry)
	}
	return buildBundle(idx, "history", nil, entries, nil)
}

func buildBundle(idx *conformance.Index, bundleType string, total *int, entries []bundleEntry, links []any) (*resource.Node, error) {
	obj := map[string]any{
		"resourceType": "Bundle",
		"type":         bundleType,
	}
	if total != nil {
		obj["total"] = resource.Number(strconv.Itoa(*total))
	}
	if len(links) > 0 {
		obj["link"] = links
	}

	built := make([]any, 0, len(entries))
	for _, e := range entries {
		entry := map[string]any{}
		if e.fullURL != "" {
			entry["fullUrl"] = e.fullURL
		}
		if len(e.content) > 0 {
			// The stored content is already canonical JSON; decoding it back
			// into the document tree keeps one serialization path, so an XML
			// response is produced by the same writer as a JSON one.
			var nested map[string]any
			dec := json.NewDecoder(strings.NewReader(string(e.content)))
			dec.UseNumber()
			if err := dec.Decode(&nested); err != nil {
				return nil, fmt.Errorf("rest: reading stored resource: %w", err)
			}
			entry["resource"] = resource.ConvertNumbers(nested)
		}
		if e.mode != "" {
			entry["search"] = map[string]any{"mode": e.mode}
		}
		if e.method != "" {
			entry["request"] = map[string]any{"method": e.method, "url": e.url}
			response := map[string]any{"status": e.status}
			if e.etag != "" {
				response["etag"] = e.etag
			}
			if e.lastMod != "" {
				response["lastModified"] = e.lastMod
			}
			entry["response"] = response
		}
		built = append(built, entry)
	}
	if len(built) > 0 {
		obj["entry"] = built
	}
	return resource.New(idx, obj)
}

// pagingLinks builds self, first, previous, next, and last.
//
// Paging is by offset in M2. That is honest but not stable under concurrent
// writes: a resource created between two page fetches shifts everything after
// it. Cursor paging replaces this when search is completed; the link shape a
// client sees does not change, which is the point of expressing paging as
// opaque links rather than as documented parameters.
func pagingLinks(requestURL *url.URL, total int, q storage.SearchQuery) []any {
	pageSize := q.Count
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	withOffset := func(offset int) string {
		u := *requestURL
		values := u.Query()
		values.Set("_count", strconv.Itoa(pageSize))
		values.Set("_offset", strconv.Itoa(offset))
		u.RawQuery = values.Encode()
		return u.String()
	}
	link := func(relation, target string) any {
		return map[string]any{"relation": relation, "url": target}
	}

	links := []any{link("self", requestURL.String())}
	if total <= pageSize && q.Offset == 0 {
		return links
	}
	links = append(links, link("first", withOffset(0)))
	if q.Offset > 0 {
		previous := q.Offset - pageSize
		if previous < 0 {
			previous = 0
		}
		links = append(links, link("previous", withOffset(previous)))
	}
	if q.Offset+pageSize < total {
		links = append(links, link("next", withOffset(q.Offset+pageSize)))
	}
	if last := ((total - 1) / pageSize) * pageSize; last > 0 {
		links = append(links, link("last", withOffset(last)))
	}
	return links
}
