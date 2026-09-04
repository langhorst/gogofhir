package rest

import (
	"bytes"
	"context"
	"strings"

	"net/http"
)

// Executing an entry through the server's own router.

// dispatch runs one entry through the server's own handler.
//
// Reusing the handler is what keeps a bundled interaction identical to the
// standalone one: the same conditional handling, the same status codes, the
// same OperationOutcome on failure. The alternative -- a second implementation
// of each interaction for bundles -- is a reliable source of divergence between
// what a server does directly and what it does in a transaction.
func (s *Server) dispatch(ctx context.Context, outer *http.Request, entry *txEntry) {
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
	req, err := http.NewRequestWithContext(ctx, entry.dispatchMethod(), target, bytes.NewReader(body))
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
	s.handler.ServeHTTP(rec, req)

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
