package rest

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/resource"
	"github.com/langhorst/gogofhir/internal/smart"
	"github.com/langhorst/gogofhir/internal/storage"
	"github.com/langhorst/gogofhir/internal/validate"
)

const (
	defaultPageSize = 50
	maxPageSize     = 1000
	// maxBodyBytes bounds a request body. Resources are documents, not uploads;
	// without a limit a single request could exhaust memory.
	maxBodyBytes = 32 << 20
)

// Server is the FHIR RESTful API.
type Server struct {
	Index *conformance.Index
	Store storage.Backend
	// BaseURL is the server's own address, used to build fullUrl and Location
	// headers. Empty means derive it from each request.
	BaseURL string
	Log     *slog.Logger

	// StrictTerminology promotes a binding this server cannot check offline
	// from a warning to an error.
	StrictTerminology bool
	// ValidateWrites rejects a create or update whose resource has validation
	// errors. Off by default: a developer building up a resource wants it to
	// round-trip, and a server that refuses half-finished data is one they work
	// around rather than with.
	ValidateWrites bool

	// SMART, when set, requires an access token on every interaction and
	// enforces its scopes. Nil leaves the server open, which is the default for
	// the same reason validation is off: a developer should not have to
	// negotiate an OAuth flow before their first GET.
	SMART *smart.Server

	// validator is built by Handler and shared by every request, including the
	// scoped servers a transaction runs its entries through -- it caches
	// compiled expressions and patterns, and is safe for concurrent use.
	validator *validate.Validator
}

// Handler builds the route table.
//
// The patterns rely on Go's own specificity rules: "/{type}/_history" is more
// specific than "/{type}/{id}", so history routes win without ordering tricks
// or a regexp table.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.validator == nil {
		s.validator = validate.New(s.Index)
		s.validator.StrictTerminology = s.StrictTerminology
	}
	mux := http.NewServeMux()
	if s.SMART != nil {
		s.SMART.Routes(mux)
	}

	mux.HandleFunc("GET /metadata", s.handleCapabilities)
	mux.HandleFunc("GET /_history", s.handleSystemHistory)
	// "{$}" matches the root path exactly, which is where transaction and batch
	// bundles are posted.
	mux.HandleFunc("POST /{$}", s.handleBundle)

	mux.HandleFunc("GET /{type}", s.handleSearch)
	mux.HandleFunc("POST /{type}", s.handleCreate)
	mux.HandleFunc("PUT /{type}", s.handleConditionalUpdate)
	mux.HandleFunc("DELETE /{type}", s.handleConditionalDelete)
	mux.HandleFunc("GET /{type}/_history", s.handleTypeHistory)
	mux.HandleFunc("GET /{type}/_search", s.handleSearch)
	mux.HandleFunc("POST /{type}/_search", s.handleSearchPost)
	mux.HandleFunc("POST /{type}/$validate", s.handleValidateType)

	mux.HandleFunc("GET /{type}/{id}", s.handleRead)
	mux.HandleFunc("PUT /{type}/{id}", s.handleUpdate)
	mux.HandleFunc("DELETE /{type}/{id}", s.handleDelete)
	mux.HandleFunc("GET /{type}/{id}/_history", s.handleInstanceHistory)
	mux.HandleFunc("GET /{type}/{id}/_history/{vid}", s.handleVRead)
	mux.HandleFunc("POST /{type}/{id}/$validate", s.handleValidateInstance)

	// Anything unmatched. Without it the mux answers with Go's plain-text 404,
	// and a FHIR client is entitled to an OperationOutcome on every error --
	// including the ones the router produces.
	mux.HandleFunc("/", s.handleUnknownRoute)

	// Authorization wraps the whole router rather than each handler, so a route
	// added later cannot be added unguarded.
	return s.guard(mux)
}

func (s *Server) handleUnknownRoute(w http.ResponseWriter, r *http.Request) {
	s.fail(w, r, http.StatusNotFound, "no interaction at %s %s", r.Method, r.URL.Path)
}

// ---- responses ----

// write serializes a document in the client's chosen format.
func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, node *resource.Node, headers map[string]string) {
	f := responseFormat(r)
	indent := ""
	if r.URL.Query().Get("_pretty") == "true" {
		indent = "  "
	}

	var body []byte
	var err error
	if f == formatXML {
		body, err = node.XML(indent)
	} else {
		body, err = node.JSON(indent)
	}
	if err != nil {
		s.Log.Error("serializing response", "error", err)
		http.Error(w, "serialization failed", http.StatusInternalServerError)
		return
	}
	for name, value := range headers {
		if value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Content-Type", f.mediaType()+"; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// fail writes an OperationOutcome. Every error path goes through here, so no
// response can leave without one.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, format string, args ...any) {
	diagnostics := fmt.Sprintf(format, args...)
	if status >= 500 {
		s.Log.Error("request failed", "status", status, "detail", diagnostics,
			"method", r.Method, "path", r.URL.Path)
	}
	s.write(w, r, status, outcome(s.Index, Issue{
		Severity:    severityError,
		Code:        issueCodeForStatus(status),
		Diagnostics: diagnostics,
	}), nil)
}

// failStorage maps a storage error onto the status the specification defines.
func (s *Server) failStorage(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "resource not found")
	case errors.Is(err, storage.ErrDeleted):
		// Gone rather than Not Found: the client is entitled to know the
		// resource existed.
		s.fail(w, r, http.StatusGone, "resource deleted")
	case errors.Is(err, storage.ErrConflict):
		s.fail(w, r, http.StatusConflict, "version conflict: the resource was modified by someone else")
	case errors.Is(err, storage.ErrDuplicate):
		s.fail(w, r, http.StatusConflict, "a resource with that id already exists")
	case errors.Is(err, storage.ErrMultipleMatches):
		s.fail(w, r, http.StatusPreconditionFailed, "the search criteria matched more than one resource")
	default:
		s.fail(w, r, http.StatusInternalServerError, "storage error: %v", err)
	}
}

// ---- helpers ----

// resourceType validates the {type} path segment against the release being
// served, so an unknown type is a clean 404 rather than an empty search.
func (s *Server) resourceType(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("type")
	if !s.Index.IsResource(name) {
		s.fail(w, r, http.StatusNotFound, "unknown resource type %q", name)
		return "", false
	}
	return name, true
}

// base returns the server's external base URL.
func (s *Server) base(r *http.Request) string {
	if s.BaseURL != "" {
		return strings.TrimSuffix(s.BaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}
	return scheme + "://" + r.Host
}

func etagFor(version int64) string {
	// FHIR versions are weak ETags: they identify a version, not a byte-for-byte
	// representation, and the same version serializes differently as JSON and
	// as XML.
	return `W/"` + strconv.FormatInt(version, 10) + `"`
}

func httpDate(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

// parseETag pulls the version out of an If-Match header, accepting the weak
// form the server emits, the strong form, and a bare version.
func parseETag(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "W/")
	return strings.Trim(value, `"`)
}

// readResource decodes a request body in whichever format it arrived.
func (s *Server) readResource(w http.ResponseWriter, r *http.Request) (*resource.Node, bool) {
	f, ok := requestFormat(r)
	if !ok {
		s.fail(w, r, http.StatusUnsupportedMediaType,
			"unsupported Content-Type %q; use application/fhir+json or application/fhir+xml",
			r.Header.Get("Content-Type"))
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.fail(w, r, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
		return nil, false
	}
	if len(body) == 0 {
		s.fail(w, r, http.StatusBadRequest, "request body is empty")
		return nil, false
	}
	var node *resource.Node
	if f == formatXML {
		node, err = resource.FromXML(s.Index, body)
	} else {
		node, err = resource.FromJSON(s.Index, body)
	}
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "cannot parse resource: %v", err)
		return nil, false
	}
	return node, true
}

// stored turns a stored resource back into a document for serialization.
func (s *Server) stored(res *storage.Resource) (*resource.Node, error) {
	return resource.FromJSON(s.Index, res.Content)
}

// resourceHeaders are the headers every single-resource response carries.
func (s *Server) resourceHeaders(base string, res *storage.Resource) map[string]string {
	return map[string]string{
		"ETag":          etagFor(res.VersionID),
		"Last-Modified": httpDate(res.LastUpdated),
		"Location":      fmt.Sprintf("%s/%s/%s/_history/%d", base, res.Type, res.ID, res.VersionID),
	}
}

// searchValues returns a request's query parameters.
func searchValues(r *http.Request) url.Values { return r.URL.Query() }
