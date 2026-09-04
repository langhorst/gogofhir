package rest

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/storage"
)

// The RESTful interactions.

// handleRead: GET /{type}/{id}
func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !s.permits(r, resourceType, id, grantFrom(r)) {
		s.outOfCompartment(w, r, resourceType, id)
		return
	}
	res, err := s.backend(r.Context()).Read(r.Context(), resourceType, id)
	if err != nil {
		s.failStorage(w, r, err)
		return
	}
	// A conditional read short-circuits when the client already has the current
	// version, which saves sending a body it would discard.
	if match := r.Header.Get("If-None-Match"); match != "" &&
		parseETag(match) == strconv.FormatInt(res.VersionID, 10) {
		w.Header().Set("ETag", etagFor(res.VersionID))
		w.WriteHeader(http.StatusNotModified)
		return
	}
	node, err := s.stored(res)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "stored resource is unreadable: %v", err)
		return
	}
	node, ok = s.subsetForRequest(w, r, node)
	if !ok {
		return
	}
	s.write(w, r, http.StatusOK, node, s.resourceHeaders(s.base(r), res))
}

// subsetForRequest applies _summary and _elements to a single-resource
// response. They shape a read as much as a search, and a client that asks for a
// summary on one and not the other has to special-case the server.
func (s *Server) subsetForRequest(w http.ResponseWriter, r *http.Request, node *resource.Node) (*resource.Node, bool) {
	var opts searchOptions
	if err := parseSubsetOptions(r.URL.Query(), &opts); err != nil {
		s.fail(w, r, http.StatusBadRequest, "%s", err.Error())
		return nil, false
	}
	return opts.subset(node), true
}

// handleVRead: GET /{type}/{id}/_history/{vid}
func (s *Server) handleVRead(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !s.permits(r, resourceType, id, grantFrom(r)) {
		s.outOfCompartment(w, r, resourceType, id)
		return
	}
	res, err := s.backend(r.Context()).VRead(r.Context(), resourceType, id, r.PathValue("vid"))
	if err != nil {
		s.failStorage(w, r, err)
		return
	}
	node, err := s.stored(res)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "stored resource is unreadable: %v", err)
		return
	}
	node, ok = s.subsetForRequest(w, r, node)
	if !ok {
		return
	}
	s.write(w, r, http.StatusOK, node, s.resourceHeaders(s.base(r), res))
}

// handleCreate: POST /{type}, with optional conditional create.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	node, ok := s.readResource(w, r)
	if !ok {
		return
	}
	if node.FHIRType() != resourceType {
		s.fail(w, r, http.StatusBadRequest,
			"resource is a %s but was posted to %s", node.FHIRType(), resourceType)
		return
	}

	// If-None-Exist makes the create conditional: if the search finds a match,
	// nothing is created and the existing resource is returned. It is how a
	// client makes "create unless it already exists" atomic.
	if criteria := r.Header.Get("If-None-Exist"); criteria != "" {
		existing, err := s.matchOne(r.Context(), resourceType, criteria)
		switch {
		case errors.Is(err, storage.ErrMultipleMatches):
			s.failStorage(w, r, err)
			return
		case err != nil && !errors.Is(err, storage.ErrNotFound):
			s.failStorage(w, r, err)
			return
		case existing != nil:
			node, err := s.stored(existing)
			if err != nil {
				s.fail(w, r, http.StatusInternalServerError, "stored resource is unreadable: %v", err)
				return
			}
			s.write(w, r, http.StatusOK, node, s.resourceHeaders(s.base(r), existing))
			return
		}
	}

	if !s.validateOnWrite(w, r, node) {
		return
	}
	// The server assigns the id on a create: any id the client sent is ignored,
	// since POST means "you choose".
	node.SetID(newID())
	res, err := s.backend(r.Context()).Create(r.Context(), node)
	if err != nil {
		s.failStorage(w, r, err)
		return
	}
	s.writeCreated(w, r, res)
}

func (s *Server) writeCreated(w http.ResponseWriter, r *http.Request, res *storage.Resource) {
	node, err := s.stored(res)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "stored resource is unreadable: %v", err)
		return
	}
	headers := s.resourceHeaders(s.base(r), res)
	s.write(w, r, http.StatusCreated, node, headers)
}

// handleUpdate: PUT /{type}/{id}
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !s.permits(r, resourceType, id, grantFrom(r)) {
		s.outOfCompartment(w, r, resourceType, id)
		return
	}
	node, ok := s.readResource(w, r)
	if !ok {
		return
	}
	if node.FHIRType() != resourceType {
		s.fail(w, r, http.StatusBadRequest,
			"resource is a %s but was sent to %s", node.FHIRType(), resourceType)
		return
	}
	// The id in the body must agree with the id in the URL. Silently preferring
	// one would let a client update a resource it did not name.
	if bodyID := node.ID(); bodyID != "" && bodyID != id {
		s.fail(w, r, http.StatusBadRequest,
			"resource id %q does not match the URL id %q", bodyID, id)
		return
	}
	node.SetID(id)
	if !s.validateOnWrite(w, r, node) {
		return
	}

	created, res, err := s.backend(r.Context()).Update(r.Context(), node, parseETag(r.Header.Get("If-Match")))
	if err != nil {
		// A failed If-Match is Precondition Failed when the client stated one,
		// and Conflict otherwise.
		if errors.Is(err, storage.ErrConflict) && r.Header.Get("If-Match") != "" {
			s.fail(w, r, http.StatusPreconditionFailed,
				"the resource has changed since version %s", parseETag(r.Header.Get("If-Match")))
			return
		}
		s.failStorage(w, r, err)
		return
	}
	if created {
		s.writeCreated(w, r, res)
		return
	}
	stored, err := s.stored(res)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "stored resource is unreadable: %v", err)
		return
	}
	s.write(w, r, http.StatusOK, stored, s.resourceHeaders(s.base(r), res))
}

// handleDelete: DELETE /{type}/{id}
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !s.permits(r, resourceType, id, grantFrom(r)) {
		s.outOfCompartment(w, r, resourceType, id)
		return
	}
	_, res, err := s.backend(r.Context()).Delete(r.Context(), resourceType, id,
		parseETag(r.Header.Get("If-Match")))
	if err != nil {
		if errors.Is(err, storage.ErrConflict) && r.Header.Get("If-Match") != "" {
			s.fail(w, r, http.StatusPreconditionFailed, "the resource has changed")
			return
		}
		s.failStorage(w, r, err)
		return
	}
	if res != nil {
		w.Header().Set("ETag", etagFor(res.VersionID))
	}
	// 204 with no body: deleting something already absent is still a success,
	// because delete has to be idempotent.
	w.WriteHeader(http.StatusNoContent)
}

// handleConditionalUpdate: PUT /{type}?criteria
func (s *Server) handleConditionalUpdate(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery == "" {
		s.fail(w, r, http.StatusBadRequest, "a conditional update needs search criteria")
		return
	}
	node, ok := s.readResource(w, r)
	if !ok {
		return
	}
	if node.FHIRType() != resourceType {
		s.fail(w, r, http.StatusBadRequest,
			"resource is a %s but was sent to %s", node.FHIRType(), resourceType)
		return
	}

	existing, err := s.matchOne(r.Context(), resourceType, r.URL.RawQuery)
	switch {
	case errors.Is(err, storage.ErrMultipleMatches):
		s.failStorage(w, r, err)
		return
	case err != nil && !errors.Is(err, storage.ErrNotFound):
		s.failStorage(w, r, err)
		return
	case existing != nil:
		node.SetID(existing.ID)
	case node.ID() == "":
		node.SetID(newID())
	}
	if !s.validateOnWrite(w, r, node) {
		return
	}

	created, res, err := s.backend(r.Context()).Update(r.Context(), node, "")
	if err != nil {
		s.failStorage(w, r, err)
		return
	}
	if created {
		s.writeCreated(w, r, res)
		return
	}
	stored, err := s.stored(res)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "stored resource is unreadable: %v", err)
		return
	}
	s.write(w, r, http.StatusOK, stored, s.resourceHeaders(s.base(r), res))
}

// handleConditionalDelete: DELETE /{type}?criteria
func (s *Server) handleConditionalDelete(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery == "" {
		s.fail(w, r, http.StatusBadRequest, "a conditional delete needs search criteria")
		return
	}
	existing, err := s.matchOne(r.Context(), resourceType, r.URL.RawQuery)
	switch {
	case errors.Is(err, storage.ErrMultipleMatches):
		s.failStorage(w, r, err)
		return
	case errors.Is(err, storage.ErrNotFound):
		// Nothing matched. Delete is idempotent, so this is a success.
		w.WriteHeader(http.StatusNoContent)
		return
	case err != nil:
		s.failStorage(w, r, err)
		return
	}
	if _, _, err := s.backend(r.Context()).Delete(r.Context(), resourceType, existing.ID, ""); err != nil {
		s.failStorage(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// matchOne runs conditional-operation criteria and insists on a single result.
// More than one match is an error rather than a choice: the specification will
// not let a server guess which resource the client meant.
func (s *Server) matchOne(ctx context.Context, resourceType, rawQuery string) (*storage.Resource, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, &searchError{"malformed search criteria"}
	}
	q, _, err := parseSearch(s.index, resourceType, values)
	if err != nil {
		return nil, err
	}
	// Two is enough to know the criteria are ambiguous, and the total is not
	// needed to find that out.
	q.Count, q.SkipTotal = 2, true
	result, err := s.backend(ctx).Search(ctx, q)
	if err != nil {
		return nil, err
	}
	switch len(result.Matches) {
	case 0:
		return nil, storage.ErrNotFound
	case 1:
		return result.Matches[0], nil
	default:
		return nil, storage.ErrMultipleMatches
	}
}

// handleSearch: GET /{type}
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	s.search(w, r, resourceType, searchValues(r))
}

// handleSearchPost: POST /{type}/_search, where criteria arrive as a form body
// so a long query does not have to fit in a URL.
func (s *Server) handleSearchPost(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "cannot parse search criteria: %v", err)
		return
	}
	s.search(w, r, resourceType, r.Form)
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, resourceType string, values url.Values) {
	// A patient-scoped token sees one patient's compartment, which is a
	// restriction on the query rather than on the parameters: the scope says
	// the app may read Observations, and the compartment says which ones.
	values, inCompartment := s.confine(values, resourceType, grantFrom(r))
	if !inCompartment {
		s.fail(w, r, http.StatusForbidden,
			"%s is not in the patient compartment, so a patient-scoped token cannot search it",
			resourceType)
		return
	}
	q, opts, err := parseSearch(s.index, resourceType, values)
	if err != nil {
		var se *searchError
		if errors.As(err, &se) {
			s.fail(w, r, http.StatusBadRequest, "%s", se.Error())
			return
		}
		s.fail(w, r, http.StatusInternalServerError, "search failed: %v", err)
		return
	}
	result, err := s.backend(r.Context()).Search(r.Context(), q)
	if err != nil {
		var se *searchError
		if errors.As(err, &se) {
			s.fail(w, r, http.StatusBadRequest, "%s", se.Error())
			return
		}
		s.failStorage(w, r, err)
		return
	}
	if opts.countOnly {
		// _summary=count wants the number and nothing else.
		result.Matches, result.Next = nil, ""
	}

	// Includes resolve against the matches, after paging: a client asking for
	// twenty results and their subjects wants the subjects of those twenty, not
	// of every resource that matched.
	var included []*storage.Resource
	if len(opts.includes) > 0 && len(result.Matches) > 0 {
		included, err = s.backend(r.Context()).Include(r.Context(), result.Matches, opts.includes)
		if err != nil {
			s.failStorage(w, r, err)
			return
		}
	}

	bundle, err := searchBundle(s.index, bundleContext{
		base:     s.base(r),
		request:  r.URL,
		total:    result.Total,
		hasTotal: result.HasTotal,
		cursor:   result.Next,
		options:  opts,
		included: included,
	}, result.Matches)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "building the search bundle failed: %v", err)
		return
	}
	s.write(w, r, http.StatusOK, bundle, nil)
}

// ---- history ----

func (s *Server) handleInstanceHistory(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !s.permits(r, resourceType, id, grantFrom(r)) {
		s.outOfCompartment(w, r, resourceType, id)
		return
	}
	s.history(w, r, storage.HistoryQuery{Type: resourceType, ID: id})
}

func (s *Server) handleTypeHistory(w http.ResponseWriter, r *http.Request) {
	resourceType, ok := s.resourceType(w, r)
	if !ok {
		return
	}
	s.history(w, r, storage.HistoryQuery{Type: resourceType})
}

func (s *Server) handleSystemHistory(w http.ResponseWriter, r *http.Request) {
	s.history(w, r, storage.HistoryQuery{})
}

func (s *Server) history(w http.ResponseWriter, r *http.Request, q storage.HistoryQuery) {
	values := r.URL.Query()
	if raw := values.Get("_count"); raw != "" {
		count, err := strconv.Atoi(raw)
		if err != nil || count < 0 {
			s.fail(w, r, http.StatusBadRequest, "_count must be a non-negative integer")
			return
		}
		if count > maxPageSize {
			count = maxPageSize
		}
		q.Count = count
	}
	if raw := values.Get("_since"); raw != "" {
		since, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "_since must be an instant, got %q", raw)
			return
		}
		q.Since = since
	}

	versions, err := s.backend(r.Context()).History(r.Context(), q)
	if err != nil {
		s.failStorage(w, r, err)
		return
	}
	if len(versions) == 0 && q.ID != "" {
		// History for a resource that never existed is a 404, while an existing
		// resource with no matching versions is an empty bundle.
		if _, err := s.backend(r.Context()).Read(r.Context(), q.Type, q.ID); errors.Is(err, storage.ErrNotFound) {
			s.fail(w, r, http.StatusNotFound, "resource not found")
			return
		}
	}
	bundle, err := historyBundle(s.index, s.base(r), versions)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "building the history bundle failed: %v", err)
		return
	}
	s.write(w, r, http.StatusOK, bundle, nil)
}

// newID generates a server-assigned logical id.
//
// FHIR ids are opaque strings of at most 64 characters from a restricted
// alphabet, so a UUID's hex-and-dashes form is safe and collision-free without
// coordinating with storage.
func newID() string {
	return strings.ToLower(uuidV4())
}
