package rest

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
	"net/http"
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
// router against a transaction-scoped store. That is the point: a create
// inside a bundle is the same create, with the same conditional handling,
// status codes, and OperationOutcomes, rather than a second implementation that
// drifts from the first.
//
// The work is in phases, one file each: parse the entries, resolve what each
// acts on, execute them in the order the specification gives, and build the
// response.

// maxBundleEntries bounds a bundle. Every entry is an interaction, and an
// unbounded bundle is a cheap way to make the server do expensive work inside
// one transaction that also holds the database's only writer.
const maxBundleEntries = 500

// entryRequest is what one entry asked for, as parsed. Nothing in it has
// touched storage, so a malformed bundle is refused before any work begins.
type entryRequest struct {
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
}

// entryTarget is what an entry acts on once its conditional criteria have been
// evaluated: the concrete Type/id, decided before anything is written.
type entryTarget struct {
	resourceType string
	id           string
	// done marks an entry settled before execution -- a conditional create
	// whose resource already exists, or a conditional delete that matched
	// nothing.
	done bool
}

// entryResult is what an entry's interaction produced.
type entryResult struct {
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

// txEntry is one entry of a transaction or batch bundle through its three
// phases: what it asked for, what that resolved to, and what happened.
type txEntry struct {
	entryRequest
	entryTarget
	entryResult
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

// runBatch executes entries independently.
//
// There is no shared transaction and no reference resolution between entries:
// a batch is a bundle of interactions that happen to travel together, and the
// specification is explicit that they do not depend on each other. A failing
// entry carries its own status into the response; the batch itself succeeds.
func (s *Server) runBatch(w http.ResponseWriter, r *http.Request, entries []*txEntry) {
	for _, entry := range entries {
		s.dispatch(r.Context(), r, entry)
	}
	s.writeBundleResponse(w, r, "batch-response", entries)
}

func (s *Server) runTransaction(w http.ResponseWriter, r *http.Request, entries []*txEntry) {
	var failed *txEntry
	err := s.backend(r.Context()).Tx(r.Context(), func(ctx context.Context, store storage.Backend) error {
		// Everything below runs against the transaction's own store, so the
		// resolution searches see earlier entries' writes and nothing escapes if
		// a later entry fails. The store travels in the context, which is what
		// lets the same router serve the entries.
		ctx = withBackend(ctx, store)

		if err := s.resolveTargets(ctx, entries); err != nil {
			return err
		}
		if err := checkDuplicates(entries); err != nil {
			return err
		}
		if err := s.resolveReferences(ctx, entries); err != nil {
			return err
		}

		for _, entry := range executionOrder(entries) {
			if entry.done {
				continue
			}
			s.dispatch(ctx, r, entry)
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
	node, err := resource.FromJSON(s.index, body)
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
