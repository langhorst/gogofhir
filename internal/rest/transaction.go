package rest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

// Transaction and batch bundles: POST / with a Bundle of entries, each entry a
// RESTful interaction.
//
// The two differ in one thing that changes everything. A batch is a convenience
// -- independent interactions posted together, each succeeding or failing on its
// own. A transaction is a unit: entries may refer to each other, and either all
// of them happen or none do. A half-applied transaction is worse than a
// rejected one, because the client has no way to find out what landed.
//
// Entries are executed by dispatching them back through the server's own
// handler against a transaction-scoped store. That is the point: a create
// inside a bundle is the same create, with the same conditional handling,
// status codes, and OperationOutcomes, rather than a second implementation that
// drifts from the first.

// maxBundleEntries bounds a bundle. Every entry is an interaction, and an
// unbounded bundle is a cheap way to make the server do expensive work inside
// one transaction that also holds the database's only writer.
const maxBundleEntries = 500

// txEntry is one entry of a transaction or batch bundle, from parsing through
// to its response.
type txEntry struct {
	// position is the entry's index in the request bundle. Responses are
	// returned in request order however the entries were executed.
	position int
	fullURL  string
	node     *resource.Node

	method string
	// path and query are request.url split apart: "Patient/123", or "Patient"
	// with criteria for a conditional operation.
	path        string
	query       string
	ifNoneExist string
	ifMatch     string
	ifNoneMatch string

	// resolved before execution: the concrete Type/id an entry acts on, once
	// conditional criteria have been evaluated.
	resourceType string
	id           string
	// done marks an entry settled before execution -- a conditional create
	// whose resource already exists, or a conditional delete that matched
	// nothing.
	done bool

	status   int
	location string
	etag     string
	lastMod  string
	// body is the response body the interaction produced, kept as JSON so it
	// can be re-embedded in a bundle serialized as either format.
	body []byte
	// carry says whether body belongs in the response entry. A GET returns what
	// it read; a write returns its resource only when the client asked for it.
	carry bool
}

// handleBundle: POST /, the transaction and batch endpoint.
func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	node, ok := s.readResource(w, r)
	if !ok {
		return
	}
	if node.FHIRType() != "Bundle" {
		s.fail(w, r, http.StatusBadRequest,
			"the root endpoint takes a Bundle, got a %s", node.FHIRType())
		return
	}
	obj, _ := node.Object()
	bundleType, _ := obj["type"].(string)
	switch bundleType {
	case "transaction", "batch":
	case "":
		s.fail(w, r, http.StatusBadRequest, "the Bundle has no type; expected transaction or batch")
		return
	default:
		s.fail(w, r, http.StatusBadRequest,
			"the root endpoint accepts transaction and batch bundles, got %q", bundleType)
		return
	}

	entries, err := parseEntries(s, obj)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if bundleType == "batch" {
		s.runBatch(w, r, entries)
		return
	}
	s.runTransaction(w, r, entries)
}

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
		entry := &txEntry{position: i}
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
			node, err := resource.New(s.Index, nested)
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

// ---- batch ----

// runBatch executes entries independently.
//
// There is no shared transaction and no reference resolution between entries:
// a batch is a bundle of interactions that happen to travel together, and the
// specification is explicit that they do not depend on each other. A failing
// entry carries its own status into the response; the batch itself succeeds.
func (s *Server) runBatch(w http.ResponseWriter, r *http.Request, entries []*txEntry) {
	handler := s.Handler()
	for _, entry := range entries {
		s.dispatch(r, handler, entry)
	}
	s.writeBundleResponse(w, r, "batch-response", entries)
}

// ---- transaction ----

func (s *Server) runTransaction(w http.ResponseWriter, r *http.Request, entries []*txEntry) {
	var failed *txEntry
	err := s.Store.Tx(r.Context(), func(ctx context.Context, store storage.Backend) error {
		// Everything below runs against the transaction's own store, so the
		// resolution searches see earlier entries' writes and nothing escapes if
		// a later entry fails.
		scoped := *s
		scoped.Store = store
		handler := scoped.Handler()

		if err := scoped.resolveTargets(ctx, entries); err != nil {
			return err
		}
		if err := checkDuplicates(entries); err != nil {
			return err
		}
		if err := scoped.resolveReferences(ctx, entries); err != nil {
			return err
		}

		for _, entry := range executionOrder(entries) {
			if entry.done {
				continue
			}
			scoped.dispatch(r, handler, entry)
			if entry.status >= http.StatusBadRequest {
				// One failure rolls the whole transaction back. Returning the
				// entry rather than a message keeps its OperationOutcome, which
				// says what actually went wrong.
				failed = entry
				return errTransactionFailed
			}
		}
		return nil
	})

	switch {
	case failed != nil:
		s.failEntry(w, r, failed)
		return
	case err != nil:
		var se *searchError
		if errors.As(err, &se) {
			s.fail(w, r, http.StatusBadRequest, "%s", se.Error())
			return
		}
		s.failStorage(w, r, err)
		return
	}
	s.writeBundleResponse(w, r, "transaction-response", entries)
}

// errTransactionFailed unwinds the storage transaction after an entry failed.
// The entry itself carries the response, so this value never reaches a client.
var errTransactionFailed = errors.New("rest: transaction entry failed")

// failEntry reports a rolled-back transaction with the failing entry's own
// status and outcome, saying which entry it was. A bare 400 would leave the
// client to guess.
func (s *Server) failEntry(w http.ResponseWriter, r *http.Request, entry *txEntry) {
	detail := strings.TrimSpace(outcomeDiagnostics(s, entry.body))
	if detail == "" {
		detail = http.StatusText(entry.status)
	}
	s.fail(w, r, entry.status,
		"the transaction was rolled back: entry %d (%s %s) failed: %s",
		entry.position, entry.method, entry.requestURL(), detail)
}

// outcomeDiagnostics pulls the diagnostics out of an OperationOutcome body, so
// the reason an entry failed survives into the transaction's own outcome.
func outcomeDiagnostics(s *Server, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	node, err := resource.FromJSON(s.Index, body)
	if err != nil || node.FHIRType() != "OperationOutcome" {
		return ""
	}
	obj, _ := node.Object()
	issues, _ := obj["issue"].([]any)
	var parts []string
	for _, raw := range issues {
		issue, _ := raw.(map[string]any)
		if text, _ := issue["diagnostics"].(string); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "; ")
}

// resolveTargets settles which resource each entry acts on before anything is
// written.
//
// It has to happen first because references between entries are resolved
// against those identities: an entry cannot point at a resource whose id is
// still going to be chosen. Server-assigned ids are therefore chosen here, and
// conditional criteria are evaluated here, which turns every entry into a plain
// instance-level interaction.
func (s *Server) resolveTargets(ctx context.Context, entries []*txEntry) error {
	for _, entry := range entries {
		segments := strings.Split(entry.path, "/")

		switch entry.method {
		case http.MethodGet, http.MethodHead:
			continue // reads act on whatever the URL names

		case http.MethodPost:
			if len(segments) != 1 || !s.Index.IsResource(entry.resourceType) {
				return &searchError{fmt.Sprintf(
					"entry %d posts to %q, which is not a resource type", entry.position, entry.path)}
			}
			// A conditional create asks the server not to create a second copy.
			// Resolving it now means a reference to this entry points at
			// whichever resource ends up being the one.
			if entry.ifNoneExist != "" {
				existing, err := s.matchOne(ctx, entry.resourceType, entry.ifNoneExist)
				switch {
				case err == nil:
					entry.id, entry.done = existing.ID, true
					entry.status = http.StatusOK
					entry.location = fmt.Sprintf("%s/%s/_history/%d",
						existing.Type, existing.ID, existing.VersionID)
					entry.etag = etagFor(existing.VersionID)
					entry.lastMod = httpDate(existing.LastUpdated)
					continue
				case errors.Is(err, storage.ErrNotFound):
					// Nothing matched, so the create proceeds.
				default:
					return entryError(entry, err)
				}
			}
			entry.id = newID()
			entry.node.SetID(entry.id)

		case http.MethodPut:
			if !s.Index.IsResource(entry.resourceType) {
				return &searchError{fmt.Sprintf(
					"entry %d targets %q, which is not a resource type", entry.position, entry.path)}
			}
			switch {
			case len(segments) == 2:
				entry.id = segments[1]
			case entry.query != "":
				// A conditional update resolves to the single match, or creates
				// at a new id when nothing matches.
				existing, err := s.matchOne(ctx, entry.resourceType, entry.query)
				switch {
				case err == nil:
					entry.id = existing.ID
				case errors.Is(err, storage.ErrNotFound):
					entry.id = firstNonEmpty(entry.node.ID(), newID())
				default:
					return entryError(entry, err)
				}
			default:
				return &searchError{fmt.Sprintf(
					"entry %d is a PUT to %q with no id and no criteria", entry.position, entry.path)}
			}
			// The URL's id is authoritative, and a conditional update has just
			// acquired one; the body follows it.
			entry.node.SetID(entry.id)

		case http.MethodDelete:
			if !s.Index.IsResource(entry.resourceType) {
				return &searchError{fmt.Sprintf(
					"entry %d targets %q, which is not a resource type", entry.position, entry.path)}
			}
			switch {
			case len(segments) == 2:
				entry.id = segments[1]
			case entry.query != "":
				existing, err := s.matchOne(ctx, entry.resourceType, entry.query)
				switch {
				case err == nil:
					entry.id = existing.ID
				case errors.Is(err, storage.ErrNotFound):
					// Delete is idempotent, so nothing to delete is a success.
					entry.done, entry.status = true, http.StatusNoContent
				default:
					return entryError(entry, err)
				}
			default:
				return &searchError{fmt.Sprintf(
					"entry %d is a DELETE to %q with no id and no criteria", entry.position, entry.path)}
			}
		}
	}
	return nil
}

// entryError wraps a resolution failure so the message names the entry.
func entryError(entry *txEntry, err error) error {
	if errors.Is(err, storage.ErrMultipleMatches) {
		return &searchError{fmt.Sprintf(
			"entry %d: the criteria %q matched more than one resource, so the server cannot tell which one you meant",
			entry.position, entry.criteria())}
	}
	var se *searchError
	if errors.As(err, &se) {
		return &searchError{fmt.Sprintf("entry %d: %s", entry.position, se.Error())}
	}
	return err
}

func (entry *txEntry) criteria() string {
	if entry.ifNoneExist != "" {
		return entry.ifNoneExist
	}
	return entry.query
}

// checkDuplicates rejects a transaction that acts on the same resource twice.
//
// Two entries writing one resource have no defined order and no defined
// outcome, so the specification makes it an error rather than letting the
// server pick a winner.
func checkDuplicates(entries []*txEntry) error {
	seen := map[string]int{}
	for _, entry := range entries {
		if entry.id == "" || entry.method == http.MethodGet || entry.method == http.MethodHead {
			continue
		}
		key := entry.resourceType + "/" + entry.id
		if first, ok := seen[key]; ok {
			return &searchError{fmt.Sprintf(
				"entries %d and %d both act on %s; a transaction may touch a resource only once",
				first, entry.position, key)}
		}
		seen[key] = entry.position
	}

	urls := map[string]int{}
	for _, entry := range entries {
		if entry.fullURL == "" {
			continue
		}
		if first, ok := urls[entry.fullURL]; ok {
			return &searchError{fmt.Sprintf(
				"entries %d and %d share the fullUrl %s, so a reference to it is ambiguous",
				first, entry.position, entry.fullURL)}
		}
		urls[entry.fullURL] = entry.position
	}
	return nil
}

// resolveReferences rewrites the references between entries.
//
// An entry that creates a Patient and an entry that creates an Observation of
// that patient are posted together precisely because neither exists yet: the
// Observation names the Patient by the placeholder in its fullUrl, and the
// server substitutes the id it assigned. Conditional references -- a reference
// written as search criteria -- are resolved here too, against the transaction
// so that a resource created by an earlier entry can satisfy one.
func (s *Server) resolveReferences(ctx context.Context, entries []*txEntry) error {
	targets := map[string]string{}
	for _, entry := range entries {
		if entry.fullURL == "" || entry.id == "" {
			continue
		}
		if entry.method != http.MethodPost && entry.method != http.MethodPut {
			continue
		}
		targets[entry.fullURL] = entry.resourceType + "/" + entry.id
	}

	var failure error
	for _, entry := range entries {
		if entry.node == nil {
			continue
		}
		entry.node.RewriteReferences(func(reference string) (string, bool) {
			if failure != nil {
				return "", false
			}
			if target, ok := targets[reference]; ok {
				return target, true
			}
			// A conditional reference is search criteria in place of an id.
			resourceType, criteria, ok := strings.Cut(reference, "?")
			if !ok || criteria == "" || !s.Index.IsResource(resourceType) {
				return "", false
			}
			existing, err := s.matchOne(ctx, resourceType, criteria)
			switch {
			case err == nil:
				return existing.Type + "/" + existing.ID, true
			case errors.Is(err, storage.ErrNotFound):
				failure = &searchError{fmt.Sprintf(
					"entry %d refers to %q, which matched no resource", entry.position, reference)}
			case errors.Is(err, storage.ErrMultipleMatches):
				failure = &searchError{fmt.Sprintf(
					"entry %d refers to %q, which matched more than one resource",
					entry.position, reference)}
			default:
				failure = err
			}
			return "", false
		})
		if failure != nil {
			return failure
		}
	}

	// A placeholder that survives rewriting would be stored as a dangling
	// reference, which is a data error the client cannot see. Catch it here.
	for _, entry := range entries {
		if entry.node == nil {
			continue
		}
		for _, reference := range entry.node.References() {
			if strings.HasPrefix(reference, "urn:uuid:") || strings.HasPrefix(reference, "urn:oid:") {
				return &searchError{fmt.Sprintf(
					"entry %d refers to %s, which no entry in the bundle provides",
					entry.position, reference)}
			}
		}
	}
	return nil
}

// methodOrder is the order the specification gives for transaction processing:
// deletes first, then creates, then updates, then reads.
//
// It is not arbitrary. Deleting before creating lets one transaction replace a
// resource; creating before updating lets an update refer to something the same
// bundle just made; reading last means a GET observes the transaction's own
// result.
var methodOrder = map[string]int{
	http.MethodDelete: 0,
	http.MethodPost:   1,
	http.MethodPut:    2,
	http.MethodPatch:  2,
	http.MethodGet:    3,
	http.MethodHead:   3,
}

func executionOrder(entries []*txEntry) []*txEntry {
	ordered := make([]*txEntry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(i, j int) bool {
		return methodOrder[ordered[i].method] < methodOrder[ordered[j].method]
	})
	return ordered
}

// ---- execution ----

// dispatch runs one entry through the server's own handler.
//
// Reusing the handler is what keeps a bundled interaction identical to the
// standalone one: the same conditional handling, the same status codes, the
// same OperationOutcome on failure. The alternative -- a second implementation
// of each interaction for bundles -- is a reliable source of divergence between
// what a server does directly and what it does in a transaction.
func (s *Server) dispatch(outer *http.Request, handler http.Handler, entry *txEntry) {
	var body []byte
	if entry.node != nil {
		encoded, err := entry.node.MarshalJSON()
		if err != nil {
			entry.status = http.StatusInternalServerError
			return
		}
		body = encoded
	}

	target := "/" + entry.requestURL()
	req, err := http.NewRequestWithContext(outer.Context(), entry.dispatchMethod(), target, bytes.NewReader(body))
	if err != nil {
		entry.status = http.StatusBadRequest
		return
	}
	// The inner exchange is always JSON: the response is re-embedded in the
	// outer bundle, which serializes in whichever format the client asked for.
	req.Header.Set("Accept", "application/fhir+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	if entry.ifNoneExist != "" && entry.dispatchMethod() == http.MethodPost {
		// In a transaction the conditional create was already resolved, and the
		// entry now names the id it will occupy; the header would only make the
		// create handler search a second time.
		req.Header.Set("If-None-Exist", entry.ifNoneExist)
	}
	if entry.ifMatch != "" {
		req.Header.Set("If-Match", entry.ifMatch)
	}
	if entry.ifNoneMatch != "" {
		req.Header.Set("If-None-Match", entry.ifNoneMatch)
	}
	// Location headers are built from the request, so the inner request has to
	// look like it arrived where the outer one did.
	req.Host, req.TLS = outer.Host, outer.TLS
	req.Header.Set("X-Forwarded-Proto", schemeOf(outer))
	// The entry inherits the bundle's authorization: it was checked once, when
	// the transaction arrived, and re-presenting the token to ourselves would
	// prove nothing.
	req = withGrant(req, grantFrom(outer))

	rec := &recorder{header: http.Header{}}
	handler.ServeHTTP(rec, req)

	entry.status = rec.status
	entry.location = strings.TrimPrefix(rec.header.Get("Location"), s.base(outer)+"/")
	entry.etag = rec.header.Get("ETag")
	entry.lastMod = rec.header.Get("Last-Modified")
	entry.body = rec.body.Bytes()

	// What comes back in the entry: a read returns what it read, and a write
	// returns its resource only where the client asked for one. A failure
	// always returns its OperationOutcome, which is the only record of why.
	switch {
	case entry.status >= http.StatusBadRequest:
		entry.carry = true
	case entry.method == http.MethodGet || entry.method == http.MethodHead:
		entry.carry = true
	default:
		entry.carry = strings.Contains(outer.Header.Get("Prefer"), "return=representation")
	}
}

// dispatchMethod is the HTTP method the entry is executed as.
//
// A create whose id is already chosen goes out as a PUT to that id, which the
// specification defines to create when the id is unused and which produces the
// same 201 and Location. Choosing the id up front is what makes a reference to
// this entry resolvable at all, and POST by definition leaves it to the server.
func (entry *txEntry) dispatchMethod() string {
	if entry.method == http.MethodPost && entry.id != "" {
		return http.MethodPut
	}
	return entry.method
}

// requestURL reassembles an entry's target after resolution: a conditional
// operation has become a plain instance-level one.
func (entry *txEntry) requestURL() string {
	if entry.id != "" && entry.method != http.MethodGet && entry.method != http.MethodHead {
		return entry.resourceType + "/" + entry.id
	}
	if entry.query != "" {
		return entry.path + "?" + entry.query
	}
	return entry.path
}

// reference is the "Type/id" the entry acted on, from the id resolved before
// execution or, failing that, from the Location the interaction reported.
func (entry *txEntry) reference() string {
	if entry.id != "" {
		return entry.resourceType + "/" + entry.id
	}
	segments := strings.Split(entry.location, "/")
	if len(segments) >= 2 && segments[0] != "" && segments[1] != "" {
		return segments[0] + "/" + segments[1]
	}
	return ""
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		return forwarded
	}
	return "http"
}

// recorder captures a handler's response in memory.
//
// It is deliberately not httptest.ResponseRecorder: this is production code,
// and what it needs is small enough that borrowing a testing type to get it
// would be the larger dependency.
type recorder struct {
	status int
	header http.Header
	body   bytes.Buffer
}

func (rec *recorder) Header() http.Header { return rec.header }

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
}

func (rec *recorder) Write(p []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.body.Write(p)
}

// ---- response ----

func (s *Server) writeBundleResponse(w http.ResponseWriter, r *http.Request, bundleType string, entries []*txEntry) {
	built := make([]bundleEntry, 0, len(entries))
	for _, entry := range entries {
		out := bundleEntry{
			status:   statusLine(entry.status),
			location: entry.location,
			etag:     entry.etag,
			lastMod:  entry.lastMod,
		}
		if entry.carry {
			out.content = entry.body
		}
		// A batch never pre-assigns ids, so its entries learn what was written
		// from the Location the interaction returned.
		if reference := entry.reference(); reference != "" {
			out.fullURL = s.base(r) + "/" + reference
		}
		built = append(built, out)
	}
	bundle, err := buildBundle(s.Index, bundleType, nil, built, nil, searchOptions{}, s.Index)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "building the %s bundle failed: %v", bundleType, err)
		return
	}
	s.write(w, r, http.StatusOK, bundle, nil)
}

// statusLine renders a status the way Bundle.entry.response.status wants it:
// the code and its reason phrase.
func statusLine(code int) string {
	if code == 0 {
		code = http.StatusOK
	}
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return fmt.Sprintf("%d", code)
}
