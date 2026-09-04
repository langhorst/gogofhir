package rest

import (
	"context"
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

// Config is what a Server is built from.
type Config struct {
	Index *conformance.Index
	Store storage.Backend
	// BaseURL is the server's own address, used to build fullUrl and Location
	// headers. Empty means derive it from each request.
	BaseURL string
	// Log defaults to slog's default logger.
	Log *slog.Logger

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
}

// Server is the FHIR RESTful API.
//
// It is built once by New and not changed afterwards: the router, the
// validator and the configuration are fixed for its lifetime, and every
// request reads them. What varies per request -- the authorization grant, and
// the transaction-scoped store a bundle's entries run against -- travels in
// the request's context rather than in a copy of the server.
type Server struct {
	index          *conformance.Index
	store          storage.Backend
	baseURL        string
	log            *slog.Logger
	validateWrites bool
	auth           *smart.Server

	// validator caches compiled expressions and patterns and is safe for
	// concurrent use, so one serves every request.
	validator *validate.Validator
	handler   http.Handler
}

// New builds a server. It fails on a configuration that cannot serve, rather
// than serving errors later: a server with no index or no store has nothing to
// answer with.
func New(cfg Config) (*Server, error) {
	if cfg.Index == nil {
		return nil, errors.New("rest: a conformance index is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("rest: a storage backend is required")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	validator := validate.New(cfg.Index)
	validator.StrictTerminology = cfg.StrictTerminology

	s := &Server{
		index:          cfg.Index,
		store:          cfg.Store,
		baseURL:        cfg.BaseURL,
		log:            log,
		validateWrites: cfg.ValidateWrites,
		auth:           cfg.SMART,
		validator:      validator,
	}
	s.handler = s.routes()
	return s, nil
}

// Handler is the server's HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// routes builds the route table.
//
// The patterns rely on Go's own specificity rules: "/{type}/_history" is more
// specific than "/{type}/{id}", so history routes win without ordering tricks
// or a regexp table.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	if s.auth != nil {
		s.auth.Routes(mux)
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

// ---- the store a request runs against ----

type backendKey struct{}

// backend returns the store a request runs against: the transaction-scoped one
// a bundle's entries were given, or the server's own.
//
// Carrying it in the context is what lets one router serve both a request from
// a client and the requests a transaction makes of itself. The alternative --
// a copy of the server with a different store and its own router -- is what
// this replaced.
func (s *Server) backend(ctx context.Context) storage.Backend {
	if store, ok := ctx.Value(backendKey{}).(storage.Backend); ok {
		return store
	}
	return s.store
}

// withBackend scopes a context to a store.
func withBackend(ctx context.Context, store storage.Backend) context.Context {
	return context.WithValue(ctx, backendKey{}, store)
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
		s.log.Error("serializing response", "error", err)
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
		s.log.Error("request failed", "status", status, "detail", diagnostics,
			"method", r.Method, "path", r.URL.Path)
	}
	s.write(w, r, status, outcome(s.index, Issue{
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
	if !s.index.IsResource(name) {
		s.fail(w, r, http.StatusNotFound, "unknown resource type %q", name)
		return "", false
	}
	return name, true
}

// base returns the server's external base URL.
func (s *Server) base(r *http.Request) string {
	if s.baseURL != "" {
		return strings.TrimSuffix(s.baseURL, "/")
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
		node, err = resource.FromXML(s.index, body)
	} else {
		node, err = resource.FromJSON(s.index, body)
	}
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "cannot parse resource: %v", err)
		return nil, false
	}
	return node, true
}

// stored turns a stored resource back into a document for serialization.
func (s *Server) stored(res *storage.Resource) (*resource.Node, error) {
	return resource.FromJSON(s.index, res.Content)
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
