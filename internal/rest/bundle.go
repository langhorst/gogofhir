package rest

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

// Bundles carry every multi-resource response: search results, history, and
// transaction and batch responses.
//
// A search bundle distinguishes matches from resources pulled in alongside them
// through entry.search.mode. The distinction is mandatory and easy to omit: a
// client that cannot tell them apart cannot tell which resources answered its
// query.

// bundleEntry is one entry under construction.
type bundleEntry struct {
	fullURL string
	content []byte
	// mode is "match", "include", or "outcome" for a search bundle; empty
	// otherwise.
	mode string
	// request and response describe an entry's interaction: a history entry
	// records one that happened, a transaction-response entry reports one the
	// server just performed.
	method   string
	url      string
	status   string
	location string
	etag     string
	lastMod  string
}

// bundleContext is what a search bundle needs besides its results.
type bundleContext struct {
	base    string
	request *url.URL
	// total is reported only when hasTotal is set; a search that skipped the
	// count has no number to report.
	total    int
	hasTotal bool
	cursor   string
	options  searchOptions
	// included are the resources _include and _revinclude pulled in. They are
	// kept separate from the matches so the bundle can mark them, which is the
	// whole point: a client must be able to tell what answered its query from
	// what merely came with it.
	included []*storage.Resource
}

// searchBundle builds a searchset Bundle with the paging links a client follows.
func searchBundle(idx *conformance.Index, ctx bundleContext, results []*storage.Resource) (*resource.Node, error) {
	entries := make([]bundleEntry, 0, len(results))
	for _, res := range results {
		entries = append(entries, bundleEntry{
			fullURL: fmt.Sprintf("%s/%s/%s", ctx.base, res.Type, res.ID),
			content: res.Content,
			mode:    "match",
		})
	}
	for _, res := range ctx.included {
		entries = append(entries, bundleEntry{
			fullURL: fmt.Sprintf("%s/%s/%s", ctx.base, res.Type, res.ID),
			content: res.Content,
			mode:    "include",
		})
	}
	var total *int
	if ctx.hasTotal {
		// The total counts matches only. Included resources are context, not
		// results, and counting them would make paging arithmetic nonsense.
		total = &ctx.total
	}
	return buildBundle(idx, "searchset", total, entries, pagingLinks(ctx), ctx.options)
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
	return buildBundle(idx, "history", nil, entries, nil, searchOptions{})
}

func buildBundle(idx *conformance.Index, bundleType string, total *int, entries []bundleEntry,
	links []any, opts searchOptions) (*resource.Node, error) {
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
			// The stored content is already canonical JSON; reading it back
			// into the document tree keeps one serialization path, so an XML
			// response is produced by the same writer as a JSON one.
			node, err := resource.FromJSON(idx, e.content)
			if err != nil {
				return nil, fmt.Errorf("rest: reading stored resource: %w", err)
			}
			// _summary and _elements trim each entry, and mark it SUBSETTED so
			// a client cannot mistake a filtered resource for a sparse one.
			if opts.summary != "" || len(opts.elements) > 0 {
				node = opts.subset(node)
			}
			obj, ok := node.Object()
			if !ok {
				return nil, fmt.Errorf("rest: stored resource is not an object")
			}
			entry["resource"] = obj
		}
		if e.mode != "" {
			entry["search"] = map[string]any{"mode": e.mode}
		}
		if e.method != "" {
			entry["request"] = map[string]any{"method": e.method, "url": e.url}
		}
		// A transaction response carries a response without a request: the
		// request is the entry the client sent, at the same position.
		if e.status != "" {
			response := map[string]any{"status": e.status}
			if e.location != "" {
				response["location"] = e.location
			}
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

// pagingLinks builds self and, when there is more to fetch, next.
//
// Paging is by cursor: the next link carries an opaque token encoding where the
// previous page stopped, so a resource created between two fetches cannot shift
// the ones after it. Offset paging, which this replaced, silently repeats or
// skips rows under concurrent writes -- and a conformance suite paging through
// a dataset someone else is writing to will find that.
//
// There is deliberately no "last" link, and no "previous". Both need an offset
// to point at, which a keyset cursor does not have; a client that needs to go
// back keeps the links it followed. Clients are told to follow links rather
// than construct them, which is what makes this substitutable at all.
func pagingLinks(ctx bundleContext) []any {
	link := func(relation, target string) any {
		return map[string]any{"relation": relation, "url": target}
	}
	links := []any{link("self", ctx.request.String())}
	if ctx.cursor == "" {
		return links
	}
	next := *ctx.request
	values := next.Query()
	values.Set("_cursor", ctx.cursor)
	// An offset would contradict the cursor; the cursor already says where to
	// resume.
	values.Del("_offset")
	next.RawQuery = values.Encode()
	return append(links, link("next", next.String()))
}
