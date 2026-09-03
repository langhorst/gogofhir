package rest_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Transaction and batch bundles.
//
// The assertions that matter most are the negative ones: that a transaction
// which fails partway leaves nothing behind, and that a batch which fails
// partway leaves everything else. Those two are the whole difference between
// the interactions, and a server that gets them backwards looks correct until
// something goes wrong.

// bundleResponse is one entry of a transaction-response or batch-response.
type bundleResponse struct {
	status   string
	location string
	etag     string
	resource map[string]any
	fullURL  string
}

// postBundle posts a bundle and returns its response entries in order.
func (c *client) postBundle(want int, body string) []bundleResponse {
	c.t.Helper()
	resp := c.expect(want, "POST", "/", body)
	if want >= http.StatusBadRequest {
		return nil
	}
	decoded := resp.json(c.t)
	if got, _ := decoded["resourceType"].(string); got != "Bundle" {
		c.t.Fatalf("response is a %s, want a Bundle: %s", got, resp.body)
	}
	entries, _ := decoded["entry"].([]any)
	out := make([]bundleResponse, 0, len(entries))
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		response, _ := entry["response"].(map[string]any)
		result := bundleResponse{}
		result.status, _ = response["status"].(string)
		result.location, _ = response["location"].(string)
		result.etag, _ = response["etag"].(string)
		result.fullURL, _ = entry["fullUrl"].(string)
		result.resource, _ = entry["resource"].(map[string]any)
		out = append(out, result)
	}
	return out
}

// bundleType reads a response bundle's type.
func (c *client) bundleType(body string) string {
	c.t.Helper()
	resp := c.expect(http.StatusOK, "POST", "/", body)
	got, _ := resp.json(c.t)["type"].(string)
	return got
}

const patientURN = "urn:uuid:11111111-1111-1111-1111-111111111111"

// transactionBody is a two-entry transaction: a patient, and an observation
// that refers to it by the placeholder in its fullUrl.
func transactionBody(bundleType string) string {
	return fmt.Sprintf(`{
	  "resourceType": "Bundle",
	  "type": %q,
	  "entry": [
	    {
	      "fullUrl": %q,
	      "resource": {
	        "resourceType": "Patient",
	        "identifier": [{"system": "http://example.org/mrn", "value": "TX1"}],
	        "name": [{"family": "Chalmers"}]
	      },
	      "request": {"method": "POST", "url": "Patient"}
	    },
	    {
	      "resource": {
	        "resourceType": "Observation",
	        "status": "final",
	        "code": {"coding": [{"system": "http://loinc.org", "code": "29463-7"}]},
	        "subject": {"reference": %q},
	        "valueQuantity": {"value": 70, "unit": "kg"}
	      },
	      "request": {"method": "POST", "url": "Observation"}
	    }
	  ]
	}`, bundleType, patientURN, patientURN)
}

// A transaction's entries can refer to each other by placeholder, which is the
// reason to post them together: neither resource exists yet, so neither can be
// named by id.
func TestTransactionResolvesInternalReferences(t *testing.T) {
	c := newServer(t)
	entries := c.postBundle(http.StatusOK, transactionBody("transaction"))
	if len(entries) != 2 {
		t.Fatalf("got %d response entries, want 2", len(entries))
	}
	for i, entry := range entries {
		if entry.status != "201 Created" {
			t.Errorf("entry %d status = %q, want 201 Created", i, entry.status)
		}
		if entry.etag != `W/"1"` {
			t.Errorf("entry %d etag = %q, want W/\"1\"", i, entry.etag)
		}
	}
	if !strings.HasPrefix(entries[0].location, "Patient/") {
		t.Errorf("entry 0 location = %q, want a Patient", entries[0].location)
	}

	// The observation's subject must name the patient the transaction created,
	// not the placeholder it was written with.
	patientID := strings.Split(entries[0].location, "/")[1]
	observation := c.expect(http.StatusOK, "GET",
		"/"+strings.Join(strings.Split(entries[1].location, "/")[:2], "/"), "").json(t)
	subject, _ := observation["subject"].(map[string]any)
	if got, _ := subject["reference"].(string); got != "Patient/"+patientID {
		t.Errorf("subject.reference = %q, want Patient/%s", got, patientID)
	}
	// And the reference has to be real, not merely well-formed.
	if got := c.total("/Observation?patient.family=chal"); got != 1 {
		t.Errorf("chained search through the resolved reference = %v, want 1", got)
	}
}

// The defining property: a transaction that fails partway leaves nothing
// behind. A client that gets an error must not have to go looking for what
// happened anyway.
func TestTransactionRollsBack(t *testing.T) {
	c := newServer(t)
	body := `{
	  "resourceType": "Bundle",
	  "type": "transaction",
	  "entry": [
	    {"resource": {"resourceType": "Patient", "name": [{"family": "Rollback"}]},
	     "request": {"method": "POST", "url": "Patient"}},
	    {"resource": {"resourceType": "Patient", "name": [{"family": "Doomed"}]},
	     "request": {"method": "PUT", "url": "Patient/nonexistent", "ifMatch": "W/\"7\""}}
	  ]
	}`
	resp := c.do("POST", "/", body)
	if resp.status < http.StatusBadRequest {
		t.Fatalf("status %d, want an error: %s", resp.status, resp.body)
	}
	assertOutcome(t, resp, "conflict")
	if !strings.Contains(string(resp.body), "rolled back") {
		t.Errorf("outcome does not say the transaction was rolled back: %s", resp.body)
	}
	if got := c.total("/Patient?family=Rollback"); got != 0 {
		t.Errorf("the first entry survived a rolled-back transaction: total = %v", got)
	}
}

// A batch is the opposite bargain: entries are independent, so one failing does
// not take the others with it.
func TestBatchEntriesAreIndependent(t *testing.T) {
	c := newServer(t)
	body := `{
	  "resourceType": "Bundle",
	  "type": "batch",
	  "entry": [
	    {"resource": {"resourceType": "Patient", "name": [{"family": "Kept"}]},
	     "request": {"method": "POST", "url": "Patient"}},
	    {"request": {"method": "GET", "url": "Patient/nonexistent"}}
	  ]
	}`
	entries := c.postBundle(http.StatusOK, body)
	if len(entries) != 2 {
		t.Fatalf("got %d response entries, want 2", len(entries))
	}
	if entries[0].status != "201 Created" {
		t.Errorf("entry 0 status = %q, want 201 Created", entries[0].status)
	}
	if !strings.HasPrefix(entries[1].status, "404") {
		t.Errorf("entry 1 status = %q, want 404", entries[1].status)
	}
	// A failed entry carries its OperationOutcome: it is the only record of why.
	if got, _ := entries[1].resource["resourceType"].(string); got != "OperationOutcome" {
		t.Errorf("failed entry carries a %q, want an OperationOutcome", got)
	}
	if got := c.total("/Patient?family=Kept"); got != 1 {
		t.Errorf("the succeeding entry was rolled back with the failing one: total = %v", got)
	}
}

// A batch does not resolve references between its entries -- they are
// independent interactions that happen to travel together -- so a placeholder
// in one is stored as written rather than pointing at another entry.
func TestBatchDoesNotResolveReferences(t *testing.T) {
	c := newServer(t)
	entries := c.postBundle(http.StatusOK, transactionBody("batch"))
	if len(entries) != 2 || entries[1].status != "201 Created" {
		t.Fatalf("batch entries: %+v", entries)
	}
	observation := c.expect(http.StatusOK, "GET",
		"/"+strings.Join(strings.Split(entries[1].location, "/")[:2], "/"), "").json(t)
	subject, _ := observation["subject"].(map[string]any)
	if got, _ := subject["reference"].(string); got != patientURN {
		t.Errorf("subject.reference = %q, want the placeholder %q left alone", got, patientURN)
	}
}

func TestBundleTypes(t *testing.T) {
	c := newServer(t)
	if got := c.bundleType(transactionBody("transaction")); got != "transaction-response" {
		t.Errorf("bundle type = %q, want transaction-response", got)
	}
	if got := c.bundleType(`{"resourceType":"Bundle","type":"batch"}`); got != "batch-response" {
		t.Errorf("bundle type = %q, want batch-response", got)
	}
}

// A conditional create inside a transaction is how a client says "this patient
// if you have them, otherwise make one" -- and either way, the entries that
// refer to it must end up pointing at the same resource.
func TestTransactionConditionalCreate(t *testing.T) {
	c := newServer(t)
	existing := c.createPatient("TX1", "Chalmers")

	body := fmt.Sprintf(`{
	  "resourceType": "Bundle",
	  "type": "transaction",
	  "entry": [
	    {
	      "fullUrl": %q,
	      "resource": {"resourceType": "Patient",
	        "identifier": [{"system": "http://example.org/mrn", "value": "TX1"}]},
	      "request": {"method": "POST", "url": "Patient",
	        "ifNoneExist": "identifier=http://example.org/mrn|TX1"}
	    },
	    {
	      "resource": {"resourceType": "Observation", "status": "final",
	        "code": {"coding": [{"system": "http://loinc.org", "code": "29463-7"}]},
	        "subject": {"reference": %q}},
	      "request": {"method": "POST", "url": "Observation"}
	    }
	  ]
	}`, patientURN, patientURN)

	entries := c.postBundle(http.StatusOK, body)
	if entries[0].status != "200 OK" {
		t.Errorf("conditional create status = %q, want 200 OK (nothing was created)", entries[0].status)
	}
	if want := "Patient/" + existing; !strings.HasPrefix(entries[0].location, want) {
		t.Errorf("location = %q, want it to name the existing %s", entries[0].location, want)
	}
	if got := c.total("/Patient?identifier=http://example.org/mrn|TX1"); got != 1 {
		t.Errorf("patients with that identifier = %v, want 1 (no duplicate was created)", got)
	}
	// The observation must point at the resource that already existed.
	observation := c.expect(http.StatusOK, "GET",
		"/"+strings.Join(strings.Split(entries[1].location, "/")[:2], "/"), "").json(t)
	subject, _ := observation["subject"].(map[string]any)
	if got, _ := subject["reference"].(string); got != "Patient/"+existing {
		t.Errorf("subject.reference = %q, want Patient/%s", got, existing)
	}
}

// A conditional reference names a resource by search criteria instead of by id,
// for when the client knows an identifier but not what the server called it.
func TestTransactionConditionalReference(t *testing.T) {
	c := newServer(t)
	existing := c.createPatient("TX9", "Nowak")

	body := `{
	  "resourceType": "Bundle",
	  "type": "transaction",
	  "entry": [{
	    "resource": {"resourceType": "Observation", "status": "final",
	      "code": {"coding": [{"system": "http://loinc.org", "code": "29463-7"}]},
	      "subject": {"reference": "Patient?identifier=http://example.org/mrn|TX9"}},
	    "request": {"method": "POST", "url": "Observation"}
	  }]
	}`
	entries := c.postBundle(http.StatusOK, body)
	observation := c.expect(http.StatusOK, "GET",
		"/"+strings.Join(strings.Split(entries[0].location, "/")[:2], "/"), "").json(t)
	subject, _ := observation["subject"].(map[string]any)
	if got, _ := subject["reference"].(string); got != "Patient/"+existing {
		t.Errorf("subject.reference = %q, want Patient/%s", got, existing)
	}
}

// A conditional reference that matches nothing, or more than one resource, is
// an error rather than a guess: storing the criteria verbatim would leave a
// reference no client can follow.
func TestTransactionConditionalReferenceErrors(t *testing.T) {
	c := newServer(t)
	c.createPatient("D1", "Twin")
	c.createPatient("D2", "Twin")

	observationWithSubject := func(reference string) string {
		return fmt.Sprintf(`{
		  "resourceType": "Bundle", "type": "transaction",
		  "entry": [{
		    "resource": {"resourceType": "Observation", "status": "final",
		      "code": {"text": "x"}, "subject": {"reference": %q}},
		    "request": {"method": "POST", "url": "Observation"}
		  }]
		}`, reference)
	}

	resp := c.expect(http.StatusBadRequest, "POST", "/", observationWithSubject("Patient?family=Twin"))
	if !strings.Contains(string(resp.body), "more than one") {
		t.Errorf("outcome does not mention an ambiguous match: %s", resp.body)
	}
	resp = c.expect(http.StatusBadRequest, "POST", "/", observationWithSubject("Patient?family=Absent"))
	if !strings.Contains(string(resp.body), "matched no resource") {
		t.Errorf("outcome does not mention a missing match: %s", resp.body)
	}
	// A placeholder no entry provides would be stored as a dangling reference.
	resp = c.expect(http.StatusBadRequest, "POST", "/",
		observationWithSubject("urn:uuid:99999999-9999-9999-9999-999999999999"))
	if !strings.Contains(string(resp.body), "no entry in the bundle provides") {
		t.Errorf("outcome does not mention the unresolved placeholder: %s", resp.body)
	}
}

// Two entries writing one resource have no defined order and no defined
// outcome, so the specification makes it an error rather than letting the
// server pick a winner.
func TestTransactionRejectsDuplicateTargets(t *testing.T) {
	c := newServer(t)
	body := `{
	  "resourceType": "Bundle",
	  "type": "transaction",
	  "entry": [
	    {"resource": {"resourceType": "Patient", "id": "twice", "gender": "male"},
	     "request": {"method": "PUT", "url": "Patient/twice"}},
	    {"resource": {"resourceType": "Patient", "id": "twice", "gender": "female"},
	     "request": {"method": "PUT", "url": "Patient/twice"}}
	  ]
	}`
	resp := c.expect(http.StatusBadRequest, "POST", "/", body)
	if !strings.Contains(string(resp.body), "only once") {
		t.Errorf("outcome does not explain the duplicate: %s", resp.body)
	}

	shared := `{
	  "resourceType": "Bundle",
	  "type": "transaction",
	  "entry": [
	    {"fullUrl": "urn:uuid:dup", "resource": {"resourceType": "Patient"},
	     "request": {"method": "POST", "url": "Patient"}},
	    {"fullUrl": "urn:uuid:dup", "resource": {"resourceType": "Patient"},
	     "request": {"method": "POST", "url": "Patient"}}
	  ]
	}`
	resp = c.expect(http.StatusBadRequest, "POST", "/", shared)
	if !strings.Contains(string(resp.body), "share the fullUrl") {
		t.Errorf("outcome does not explain the shared fullUrl: %s", resp.body)
	}
}

// Deletes run before creates, so one transaction can replace a resource; reads
// run last, so a GET observes the transaction's own writes.
func TestTransactionProcessingOrder(t *testing.T) {
	c := newServer(t)
	c.expect(http.StatusCreated, "PUT", "/Patient/ordered",
		`{"resourceType":"Patient","id":"ordered","gender":"male"}`)

	body := `{
	  "resourceType": "Bundle",
	  "type": "transaction",
	  "entry": [
	    {"request": {"method": "GET", "url": "Patient/replaced"}},
	    {"resource": {"resourceType": "Patient", "id": "replaced", "gender": "female"},
	     "request": {"method": "PUT", "url": "Patient/replaced"}},
	    {"request": {"method": "DELETE", "url": "Patient/ordered"}}
	  ]
	}`
	entries := c.postBundle(http.StatusOK, body)
	// The GET is listed first but executed last, so it sees the PUT's result.
	if entries[0].status != "200 OK" {
		t.Errorf("the GET ran before the write it should observe: status %q", entries[0].status)
	}
	if got, _ := entries[0].resource["gender"].(string); got != "female" {
		t.Errorf("GET returned gender %q, want the value the same transaction wrote", got)
	}
	if entries[1].status != "201 Created" {
		t.Errorf("PUT status = %q, want 201 Created", entries[1].status)
	}
	if !strings.HasPrefix(entries[2].status, "204") {
		t.Errorf("DELETE status = %q, want 204", entries[2].status)
	}
	c.expect(http.StatusGone, "GET", "/Patient/ordered", "")
}

// A write returns its resource only when the client asks for one; otherwise the
// response entry is metadata, which is what makes a large transaction cheap to
// acknowledge.
func TestTransactionPreferReturn(t *testing.T) {
	c := newServer(t)
	entries := c.postBundle(http.StatusOK, transactionBody("transaction"))
	if entries[0].resource != nil {
		t.Errorf("a write returned its resource without Prefer: %v", entries[0].resource)
	}

	resp := c.expect(http.StatusOK, "POST", "/", transactionBody("transaction"),
		"Prefer", "return=representation")
	var decoded map[string]any
	if err := json.Unmarshal(resp.body, &decoded); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	list, _ := decoded["entry"].([]any)
	first, _ := list[0].(map[string]any)
	returned, _ := first["resource"].(map[string]any)
	if got, _ := returned["resourceType"].(string); got != "Patient" {
		t.Errorf("Prefer: return=representation did not return the resource: %v", first)
	}
}

func TestBundleErrors(t *testing.T) {
	c := newServer(t)
	cases := []struct{ body, phrase string }{
		{`{"resourceType":"Patient"}`, "takes a Bundle"},
		{`{"resourceType":"Bundle"}`, "has no type"},
		{`{"resourceType":"Bundle","type":"searchset"}`, "accepts transaction and batch"},
		{`{"resourceType":"Bundle","type":"transaction","entry":[{"resource":{"resourceType":"Patient"}}]}`,
			"has no request"},
		{`{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":"POST","url":"Patient"}}]}`,
			"carries no resource"},
		{`{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":"GET"}}]}`,
			"has no request.url"},
		{`{"resourceType":"Bundle","type":"transaction","entry":[{"request":{"method":"BREW","url":"Patient"}}]}`,
			"unusable request.method"},
		{`{"resourceType":"Bundle","type":"transaction","entry":[
		    {"resource":{"resourceType":"Patient"},"request":{"method":"PATCH","url":"Patient/1"}}]}`,
			"does not implement yet"},
		{`{"resourceType":"Bundle","type":"transaction","entry":[
		    {"resource":{"resourceType":"Patient"},"request":{"method":"PUT","url":"Patient"}}]}`,
			"no id and no criteria"},
	}
	for _, tc := range cases {
		t.Run(tc.phrase, func(t *testing.T) {
			resp := c.expect(http.StatusBadRequest, "POST", "/", tc.body)
			assertOutcome(t, resp, "invalid")
			if !strings.Contains(string(resp.body), tc.phrase) {
				t.Errorf("outcome does not mention %q: %s", tc.phrase, resp.body)
			}
		})
	}
}

// A conditional update inside a transaction resolves to the matching resource,
// and to a fresh one when nothing matches.
func TestTransactionConditionalUpdate(t *testing.T) {
	c := newServer(t)
	existing := c.createPatient("CU1", "Chalmers")

	body := `{
	  "resourceType": "Bundle",
	  "type": "transaction",
	  "entry": [
	    {"resource": {"resourceType": "Patient",
	       "identifier": [{"system": "http://example.org/mrn", "value": "CU1"}],
	       "gender": "male"},
	     "request": {"method": "PUT", "url": "Patient?identifier=http://example.org/mrn|CU1"}},
	    {"resource": {"resourceType": "Patient",
	       "identifier": [{"system": "http://example.org/mrn", "value": "CU2"}]},
	     "request": {"method": "PUT", "url": "Patient?identifier=http://example.org/mrn|CU2"}}
	  ]
	}`
	entries := c.postBundle(http.StatusOK, body)
	if entries[0].status != "200 OK" {
		t.Errorf("matching conditional update status = %q, want 200 OK", entries[0].status)
	}
	if want := "Patient/" + existing; !strings.HasPrefix(entries[0].location, want) {
		t.Errorf("location = %q, want %s", entries[0].location, want)
	}
	if entries[1].status != "201 Created" {
		t.Errorf("non-matching conditional update status = %q, want 201 Created", entries[1].status)
	}
	updated := c.expect(http.StatusOK, "GET", "/Patient/"+existing, "").json(t)
	if got, _ := updated["gender"].(string); got != "male" {
		t.Errorf("gender = %q, want the transaction's value", got)
	}
}

// An unrouted path is still a FHIR error, so it carries an OperationOutcome
// rather than the router's plain text.
func TestUnknownRouteReturnsOutcome(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusNotFound, "GET", "/Patient/a/b/c/d", "")
	assertOutcome(t, resp, "not-found")
}
