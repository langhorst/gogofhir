package rest_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/rest"
	"github.com/langhorst/gogofhir/internal/storage/sqlite"
)

// End-to-end tests over a real HTTP server and a real database. Nothing is
// mocked: these exercise the path a FHIR client actually takes, which is the
// only way to catch the things that live between layers -- status codes,
// headers, and the shape of what comes back.

type client struct {
	t    *testing.T
	base string
}

func newServer(t *testing.T) *client {
	t.Helper()
	idx := conformance.MustLoad(conformance.R5)
	store, err := sqlite.Open(":memory:", idx)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	srv := httptest.NewServer((&rest.Server{Index: idx, Store: store}).Handler())
	t.Cleanup(srv.Close)
	return &client{t: t, base: srv.URL}
}

type response struct {
	status  int
	headers http.Header
	body    []byte
}

// json decodes the body, failing the test if it is not JSON.
func (r *response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decoding response: %v\nbody: %s", err, r.body)
	}
	return out
}

func (c *client) do(method, path, body string, headers ...string) *response {
	c.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("building request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/fhir+json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return &response{status: resp.StatusCode, headers: resp.Header, body: raw}
}

func (c *client) expect(want int, method, path, body string, headers ...string) *response {
	c.t.Helper()
	resp := c.do(method, path, body, headers...)
	if resp.status != want {
		c.t.Fatalf("%s %s: status %d, want %d\nbody: %s", method, path, resp.status, want, resp.body)
	}
	return resp
}

const patientBody = `{
  "resourceType": "Patient",
  "identifier": [{"system": "http://example.org/mrn", "value": "%s"}],
  "name": [{"family": %q, "given": ["Ann"]}],
  "gender": "female",
  "birthDate": "1974-12-25"
}`

func patientJSON(mrn, family string) string {
	return fmt.Sprintf(patientBody, mrn, family)
}

// createPatient posts a patient and returns its assigned id.
func (c *client) createPatient(mrn, family string) string {
	c.t.Helper()
	resp := c.expect(http.StatusCreated, "POST", "/Patient", patientJSON(mrn, family))
	id, _ := resp.json(c.t)["id"].(string)
	if id == "" {
		c.t.Fatalf("create did not return an id: %s", resp.body)
	}
	return id
}

func TestCreateAssignsIdAndHeaders(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusCreated, "POST", "/Patient", patientJSON("A1", "Chalmers"))

	body := resp.json(t)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("no id assigned")
	}
	// The server assigns the id on a create; the client does not choose it.
	if got := resp.headers.Get("ETag"); got != `W/"1"` {
		t.Errorf("ETag = %q, want W/\"1\"", got)
	}
	if resp.headers.Get("Last-Modified") == "" {
		t.Error("Last-Modified missing")
	}
	wantLocation := "/Patient/" + id + "/_history/1"
	if got := resp.headers.Get("Location"); !strings.HasSuffix(got, wantLocation) {
		t.Errorf("Location = %q, want it to end with %q", got, wantLocation)
	}
	meta, _ := body["meta"].(map[string]any)
	if meta["versionId"] != "1" {
		t.Errorf("meta.versionId = %v, want \"1\"", meta["versionId"])
	}
}

// A create ignores any id the client sends: POST means the server chooses.
func TestCreateIgnoresClientID(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusCreated, "POST", "/Patient",
		`{"resourceType":"Patient","id":"client-chosen"}`)
	if id, _ := resp.json(t)["id"].(string); id == "client-chosen" {
		t.Error("the client's id was honoured on a create; POST must assign one")
	}
}

func TestReadAndConditionalRead(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")

	resp := c.expect(http.StatusOK, "GET", "/Patient/"+id, "")
	if got := resp.headers.Get("ETag"); got != `W/"1"` {
		t.Errorf("ETag = %q", got)
	}
	// A client that already has the current version gets 304 and no body.
	notModified := c.expect(http.StatusNotModified, "GET", "/Patient/"+id, "",
		"If-None-Match", `W/"1"`)
	if len(notModified.body) != 0 {
		t.Errorf("304 carried a body: %s", notModified.body)
	}
	// A stale ETag still gets the resource.
	c.expect(http.StatusOK, "GET", "/Patient/"+id, "", "If-None-Match", `W/"0"`)
}

func TestUpdateCreatesAndVersions(t *testing.T) {
	c := newServer(t)

	// A PUT to an unused id creates the resource.
	resp := c.expect(http.StatusCreated, "PUT", "/Patient/chosen-id",
		`{"resourceType":"Patient","id":"chosen-id","gender":"male"}`)
	if id, _ := resp.json(t)["id"].(string); id != "chosen-id" {
		t.Errorf("id = %q, want chosen-id (PUT lets the client choose)", id)
	}

	resp = c.expect(http.StatusOK, "PUT", "/Patient/chosen-id",
		`{"resourceType":"Patient","id":"chosen-id","gender":"female"}`)
	if got := resp.headers.Get("ETag"); got != `W/"2"` {
		t.Errorf("ETag after update = %q, want W/\"2\"", got)
	}
}

// The id in the body and the id in the URL must agree; preferring one silently
// would let a client update a resource it did not name.
func TestUpdateRejectsMismatchedID(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusBadRequest, "PUT", "/Patient/url-id",
		`{"resourceType":"Patient","id":"body-id"}`)
	assertOutcome(t, resp, "invalid")
}

func TestUpdateRejectsWrongType(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusBadRequest, "POST", "/Patient",
		`{"resourceType":"Observation","status":"final","code":{"text":"x"}}`)
	assertOutcome(t, resp, "invalid")
}

func TestOptimisticConcurrency(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")
	body := `{"resourceType":"Patient","id":"` + id + `","gender":"male"}`

	// A stale If-Match is Precondition Failed, not Conflict: the client stated
	// a precondition and it did not hold.
	resp := c.expect(http.StatusPreconditionFailed, "PUT", "/Patient/"+id, body,
		"If-Match", `W/"99"`)
	assertOutcome(t, resp, "conflict")

	c.expect(http.StatusOK, "PUT", "/Patient/"+id, body, "If-Match", `W/"1"`)
}

func TestDeleteIsIdempotentAndReadsAreGone(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")

	c.expect(http.StatusNoContent, "DELETE", "/Patient/"+id, "")
	// Gone rather than Not Found: the client may know the resource existed.
	resp := c.expect(http.StatusGone, "GET", "/Patient/"+id, "")
	assertOutcome(t, resp, "deleted")

	// Deleting again, and deleting something that never existed, both succeed.
	c.expect(http.StatusNoContent, "DELETE", "/Patient/"+id, "")
	c.expect(http.StatusNoContent, "DELETE", "/Patient/never-existed", "")

	// The old version is still reachable through vread.
	c.expect(http.StatusOK, "GET", "/Patient/"+id+"/_history/1", "")
}

func TestVRead(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "First")
	c.expect(http.StatusOK, "PUT", "/Patient/"+id,
		`{"resourceType":"Patient","id":"`+id+`","name":[{"family":"Second"}]}`)

	first := c.expect(http.StatusOK, "GET", "/Patient/"+id+"/_history/1", "").json(t)
	names, _ := first["name"].([]any)
	name, _ := names[0].(map[string]any)
	if name["family"] != "First" {
		t.Errorf("version 1 family = %v, want First", name["family"])
	}
	c.expect(http.StatusNotFound, "GET", "/Patient/"+id+"/_history/99", "")
}

func TestHistory(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")
	c.expect(http.StatusOK, "PUT", "/Patient/"+id,
		`{"resourceType":"Patient","id":"`+id+`","gender":"male"}`)
	c.expect(http.StatusNoContent, "DELETE", "/Patient/"+id, "")

	bundle := c.expect(http.StatusOK, "GET", "/Patient/"+id+"/_history", "").json(t)
	if bundle["type"] != "history" {
		t.Errorf("bundle type = %v, want history", bundle["type"])
	}
	entries, _ := bundle["entry"].([]any)
	if len(entries) != 3 {
		t.Fatalf("history entries = %d, want 3 (create, update, delete)", len(entries))
	}
	// Newest first, and each entry describes the interaction that made it.
	wantMethods := []string{"DELETE", "PUT", "POST"}
	for i, want := range wantMethods {
		entry, _ := entries[i].(map[string]any)
		request, _ := entry["request"].(map[string]any)
		if request["method"] != want {
			t.Errorf("entry %d method = %v, want %s", i, request["method"], want)
		}
	}
	// System-wide and type-wide history are both reachable.
	c.expect(http.StatusOK, "GET", "/_history", "")
	c.expect(http.StatusOK, "GET", "/Patient/_history", "")
	c.expect(http.StatusNotFound, "GET", "/Patient/never-existed/_history", "")
}

func TestSearch(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")
	c.createPatient("B2", "Windsor")

	cases := []struct {
		query string
		want  float64
	}{
		{"/Patient", 2},
		{"/Patient?family=chal", 1},
		{"/Patient?family=CHAL", 1},             // case-insensitive
		{"/Patient?family:exact=Chalmers", 1},   // exact is case-sensitive
		{"/Patient?family:exact=chalmers", 0},   //
		{"/Patient?family=chal,wind", 2},        // comma is an "or"
		{"/Patient?family=chal&gender=male", 0}, // separate params are an "and"
		{"/Patient?identifier=http://example.org/mrn|A1", 1},
		{"/Patient?identifier=A1", 1},                      // bare code matches any system
		{"/Patient?identifier=http://example.org/mrn|", 2}, // system alone
		{"/Patient?birthdate=1974", 2},                     // a year matches a day-precision date
		{"/Patient?birthdate=ge1975", 0},
		{"/Patient?birthdate=le1975", 2},
		{"/Patient?identifier:missing=true", 0},
		{"/Patient?gender:missing=false", 2},
		{"/Patient?_id=nonexistent", 0},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			bundle := c.expect(http.StatusOK, "GET", tc.query, "").json(t)
			if bundle["type"] != "searchset" {
				t.Errorf("bundle type = %v, want searchset", bundle["type"])
			}
			if bundle["total"] != tc.want {
				t.Errorf("total = %v, want %v", bundle["total"], tc.want)
			}
		})
	}
}

// Search entries must be marked as matches. The distinction from included
// resources is mandatory and easy to omit.
func TestSearchEntriesAreMarkedAsMatches(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")
	bundle := c.expect(http.StatusOK, "GET", "/Patient", "").json(t)
	entries, _ := bundle["entry"].([]any)
	entry, _ := entries[0].(map[string]any)
	search, _ := entry["search"].(map[string]any)
	if search["mode"] != "match" {
		t.Errorf("entry search.mode = %v, want match", search["mode"])
	}
	if _, ok := entry["fullUrl"].(string); !ok {
		t.Error("entry is missing fullUrl")
	}
}

func TestSearchPagingLinks(t *testing.T) {
	c := newServer(t)
	for i := 0; i < 5; i++ {
		c.createPatient(fmt.Sprintf("M%d", i), fmt.Sprintf("Family%d", i))
	}
	bundle := c.expect(http.StatusOK, "GET", "/Patient?_count=2", "").json(t)
	if bundle["total"] != float64(5) {
		t.Errorf("total = %v, want 5 (the count ignores paging)", bundle["total"])
	}
	entries, _ := bundle["entry"].([]any)
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2", len(entries))
	}
	links := map[string]bool{}
	for _, raw := range bundle["link"].([]any) {
		link, _ := raw.(map[string]any)
		links[link["relation"].(string)] = true
	}
	for _, want := range []string{"self", "first", "next", "last"} {
		if !links[want] {
			t.Errorf("missing %q link; have %v", want, links)
		}
	}
}

func TestSearchRejectsUnknownParameter(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusBadRequest, "GET", "/Patient?nonsense=x", "")
	assertOutcome(t, resp, "invalid")
}

func TestConditionalCreate(t *testing.T) {
	c := newServer(t)
	c.createPatient("A1", "Chalmers")

	// A match means nothing is created and the existing resource comes back.
	resp := c.expect(http.StatusOK, "POST", "/Patient", patientJSON("A1", "Other"),
		"If-None-Exist", "identifier=A1")
	if v := resp.json(t)["meta"].(map[string]any)["versionId"]; v != "1" {
		t.Errorf("conditional create bumped the version to %v", v)
	}
	bundle := c.expect(http.StatusOK, "GET", "/Patient", "").json(t)
	if bundle["total"] != float64(1) {
		t.Errorf("conditional create added a resource: total = %v", bundle["total"])
	}

	// No match means an ordinary create.
	c.expect(http.StatusCreated, "POST", "/Patient", patientJSON("B2", "Windsor"),
		"If-None-Exist", "identifier=B2")
}

func TestConditionalUpdateAndDelete(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")

	// One match: the update targets it.
	resp := c.expect(http.StatusOK, "PUT", "/Patient?identifier=A1",
		`{"resourceType":"Patient","identifier":[{"system":"http://example.org/mrn","value":"A1"}],"gender":"male"}`)
	if got, _ := resp.json(t)["id"].(string); got != id {
		t.Errorf("conditional update hit id %q, want %q", got, id)
	}

	// No match: it becomes a create.
	c.expect(http.StatusCreated, "PUT", "/Patient?identifier=ZZ",
		`{"resourceType":"Patient","identifier":[{"system":"http://example.org/mrn","value":"ZZ"}]}`)

	c.expect(http.StatusNoContent, "DELETE", "/Patient?identifier=A1", "")
	c.expect(http.StatusGone, "GET", "/Patient/"+id, "")
	// Matching nothing is still a success, because delete is idempotent.
	c.expect(http.StatusNoContent, "DELETE", "/Patient?identifier=A1", "")
}

// A conditional operation matching several resources is an error: the server
// must not guess which one the client meant.
func TestConditionalOperationRejectsAmbiguity(t *testing.T) {
	c := newServer(t)
	c.createPatient("SAME", "Chalmers")
	c.createPatient("SAME", "Windsor")

	resp := c.expect(http.StatusPreconditionFailed, "DELETE", "/Patient?identifier=SAME", "")
	assertOutcome(t, resp, "conflict")
}

func TestContentNegotiation(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("A1", "Chalmers")

	xml := c.expect(http.StatusOK, "GET", "/Patient/"+id, "", "Accept", "application/fhir+xml")
	if got := xml.headers.Get("Content-Type"); !strings.HasPrefix(got, "application/fhir+xml") {
		t.Errorf("Content-Type = %q, want application/fhir+xml", got)
	}
	if !strings.Contains(string(xml.body), `<Patient xmlns="http://hl7.org/fhir"`) {
		t.Errorf("body is not FHIR XML: %s", xml.body)
	}

	// _format overrides Accept, for clients that cannot set headers.
	viaQuery := c.expect(http.StatusOK, "GET", "/Patient/"+id+"?_format=xml", "")
	if got := viaQuery.headers.Get("Content-Type"); !strings.HasPrefix(got, "application/fhir+xml") {
		t.Errorf("_format=xml gave Content-Type %q", got)
	}

	// A resource can be created from XML too, and comes back the same.
	created := c.expect(http.StatusCreated, "POST", "/Patient",
		`<Patient xmlns="http://hl7.org/fhir"><gender value="other"/></Patient>`,
		"Content-Type", "application/fhir+xml")
	if created.json(t)["gender"] != "other" {
		t.Errorf("XML create lost content: %s", created.body)
	}
}

func TestUnsupportedMediaType(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusUnsupportedMediaType, "POST", "/Patient",
		`nonsense`, "Content-Type", "text/plain")
	assertOutcome(t, resp, "not-supported")
}

func TestMalformedBody(t *testing.T) {
	c := newServer(t)
	assertOutcome(t, c.expect(http.StatusBadRequest, "POST", "/Patient", `{"resourceType":`), "invalid")
	assertOutcome(t, c.expect(http.StatusBadRequest, "POST", "/Patient", `{"id":"x"}`), "invalid")
	assertOutcome(t, c.expect(http.StatusBadRequest, "POST", "/Patient", `{"resourceType":"Nope"}`), "invalid")
}

func TestUnknownResourceType(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusNotFound, "GET", "/Nonexistent/1", "")
	assertOutcome(t, resp, "not-found")
}

// The CapabilityStatement is generated from the conformance index, so it
// describes what the server does rather than what someone once wrote down.
func TestCapabilityStatement(t *testing.T) {
	c := newServer(t)
	statement := c.expect(http.StatusOK, "GET", "/metadata", "").json(t)

	if statement["resourceType"] != "CapabilityStatement" {
		t.Fatalf("resourceType = %v", statement["resourceType"])
	}
	if statement["fhirVersion"] != "5.0.0" {
		t.Errorf("fhirVersion = %v, want 5.0.0", statement["fhirVersion"])
	}
	restEntries, _ := statement["rest"].([]any)
	server, _ := restEntries[0].(map[string]any)
	resources, _ := server["resource"].([]any)
	if len(resources) < 140 {
		t.Errorf("advertised %d resource types, want every concrete type", len(resources))
	}

	var patient map[string]any
	for _, raw := range resources {
		entry, _ := raw.(map[string]any)
		if entry["type"] == "Patient" {
			patient = entry
		}
	}
	if patient == nil {
		t.Fatal("Patient is not advertised")
	}
	params, _ := patient["searchParam"].([]any)
	names := map[string]bool{}
	for _, raw := range params {
		p, _ := raw.(map[string]any)
		names[p["name"].(string)] = true
	}
	// Every advertised parameter must actually work, so spot-check ones the
	// tests above rely on.
	for _, want := range []string{"family", "identifier", "birthdate", "gender"} {
		if !names[want] {
			t.Errorf("Patient does not advertise the %q search parameter", want)
		}
	}
	// Composite parameters are declared by the specification but not indexed
	// yet; advertising them would promise searches that return nothing.
	if names["code-value-quantity"] {
		t.Error("a composite parameter is advertised but not implemented")
	}
}

// assertOutcome checks that an error response carries an OperationOutcome with
// the expected issue code. Every error path must produce one.
func assertOutcome(t *testing.T, resp *response, wantCode string) {
	t.Helper()
	body := resp.json(t)
	if body["resourceType"] != "OperationOutcome" {
		t.Fatalf("error response is not an OperationOutcome: %s", resp.body)
	}
	issues, _ := body["issue"].([]any)
	if len(issues) == 0 {
		t.Fatalf("OperationOutcome has no issues: %s", resp.body)
	}
	issue, _ := issues[0].(map[string]any)
	if issue["code"] != wantCode {
		t.Errorf("issue code = %v, want %v\nbody: %s", issue["code"], wantCode, resp.body)
	}
	if issue["severity"] != "error" {
		t.Errorf("issue severity = %v, want error", issue["severity"])
	}
}
