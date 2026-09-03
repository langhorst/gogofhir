package rest_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/rest"
	"github.com/langhorst/gogofhir/internal/storage/sqlite"
)

// $validate, and validation on write.

// issues reads an OperationOutcome's issues as "severity|diagnostics" lines.
func (r *response) issues(t *testing.T) []string {
	t.Helper()
	body := r.json(t)
	if got, _ := body["resourceType"].(string); got != "OperationOutcome" {
		t.Fatalf("response is a %s, want an OperationOutcome: %s", got, r.body)
	}
	raw, _ := body["issue"].([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		issue, _ := item.(map[string]any)
		severity, _ := issue["severity"].(string)
		diagnostics, _ := issue["diagnostics"].(string)
		out = append(out, severity+"|"+diagnostics)
	}
	return out
}

func hasIssue(issues []string, severity, phrase string) bool {
	for _, issue := range issues {
		if strings.HasPrefix(issue, severity+"|") && strings.Contains(issue, phrase) {
			return true
		}
	}
	return false
}

// A resource with problems is still a successful operation: the question was
// answered. Only a malformed request is an error status.
func TestValidateReportsWithoutFailing(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusOK, "POST", "/Patient/$validate", `{
	  "resourceType": "Patient", "gender": "lady", "birthDate": "25-12-1974"
	}`)
	issues := resp.issues(t)
	if !hasIssue(issues, "error", "is not a valid date") {
		t.Errorf("no date error in %v", issues)
	}
	if !hasIssue(issues, "error", "administrative-gender") {
		t.Errorf("no binding error in %v", issues)
	}
}

// $validate stores nothing: a client asking whether something would be accepted
// has not asked for it to be kept.
func TestValidateStoresNothing(t *testing.T) {
	c := newServer(t)
	c.expect(http.StatusOK, "POST", "/Patient/$validate",
		patientJSON("V1", "Ephemeral"))
	if got := c.total("/Patient?family=Ephemeral"); got != 0 {
		t.Errorf("$validate stored the resource: total = %v", got)
	}
}

func TestValidateCleanResource(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusOK, "POST", "/Patient/$validate", `{
	  "resourceType": "Patient",
	  "text": {"status": "generated", "div": "<div xmlns=\"http://www.w3.org/1999/xhtml\">Ann</div>"},
	  "name": [{"family": "Chalmers"}], "gender": "female"
	}`)
	for _, issue := range resp.issues(t) {
		if strings.HasPrefix(issue, "error|") {
			t.Errorf("unexpected error: %s", issue)
		}
	}
}

// An instance can be validated without a body, which is how a client asks
// whether something it stored earlier still conforms.
func TestValidateStoredInstance(t *testing.T) {
	c := newServer(t)
	id := c.createPatient("V2", "Chalmers")
	resp := c.expect(http.StatusOK, "POST", "/Patient/"+id+"/$validate", "")
	if len(resp.issues(t)) == 0 {
		t.Error("validating a stored instance produced no outcome at all")
	}
	c.expect(http.StatusNotFound, "POST", "/Patient/nonexistent/$validate", "")
}

// The profile parameter validates against a profile the resource does not
// itself claim.
func TestValidateAgainstProfile(t *testing.T) {
	c := newServer(t)
	resp := c.expect(http.StatusOK,
		"POST", "/Observation/$validate?profile=http://hl7.org/fhir/StructureDefinition/bp", `{
	  "resourceType": "Observation", "status": "final",
	  "code": {"coding": [{"system": "http://loinc.org", "code": "85354-9"}]},
	  "subject": {"reference": "Patient/1"}, "effectiveDateTime": "2024-01-01"
	}`)
	if !hasIssue(resp.issues(t), "error", "requires at least 2 component") {
		t.Errorf("the profile was not applied: %v", resp.issues(t))
	}
}

func TestValidateErrors(t *testing.T) {
	c := newServer(t)
	// A resource of the wrong type is a finding, not a failed request.
	resp := c.expect(http.StatusOK, "POST", "/Patient/$validate",
		`{"resourceType":"Observation","status":"final","code":{"text":"x"}}`)
	if !hasIssue(resp.issues(t), "error", "was sent to Patient/$validate") {
		t.Errorf("wrong-type outcome missing: %v", resp.issues(t))
	}
	// A bad mode is a bad request.
	c.expect(http.StatusBadRequest, "POST", "/Patient/$validate?mode=sideways",
		`{"resourceType":"Patient"}`)
	// Nothing to validate at all.
	c.expect(http.StatusBadRequest, "POST", "/Patient/$validate", "")
}

// Writes are accepted unvalidated by default: a developer building up a
// resource wants it to round-trip. With -validate-writes the same resource is
// refused with 422 -- the request was understood, the content is what failed.
func TestValidateWrites(t *testing.T) {
	broken := `{"resourceType":"Patient","gender":"lady"}`

	lenient := newServer(t)
	lenient.expect(http.StatusCreated, "POST", "/Patient", broken)

	strict := newValidatingServer(t)
	resp := strict.expect(http.StatusUnprocessableEntity, "POST", "/Patient", broken)
	if !hasIssue(resp.issues(t), "error", "administrative-gender") {
		t.Errorf("the 422 does not say what was wrong: %v", resp.issues(t))
	}
	// A sound resource still goes through.
	strict.expect(http.StatusCreated, "POST", "/Patient",
		`{"resourceType":"Patient","gender":"female"}`)
	// And so does an update of one.
	strict.expect(http.StatusCreated, "PUT", "/Patient/ok",
		`{"resourceType":"Patient","id":"ok","gender":"male"}`)
	strict.expect(http.StatusUnprocessableEntity, "PUT", "/Patient/ok",
		`{"resourceType":"Patient","id":"ok","gender":"lady"}`)
}

// newValidatingServer is a server that refuses resources with validation
// errors, as -validate-writes does.
func newValidatingServer(t *testing.T) *client {
	t.Helper()
	idx := conformance.MustLoad(conformance.R5)
	store, err := sqlite.Open(":memory:", idx)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	server := &rest.Server{Index: idx, Store: store, ValidateWrites: true}
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return &client{t: t, base: srv.URL}
}

// The CapabilityStatement has to advertise $validate, or a client has no way to
// discover it.
func TestCapabilityDeclaresValidate(t *testing.T) {
	c := newServer(t)
	body := c.expect(http.StatusOK, "GET", "/metadata", "").json(t)
	restEntries, _ := body["rest"].([]any)
	first, _ := restEntries[0].(map[string]any)
	resources, _ := first["resource"].([]any)
	for _, raw := range resources {
		entry, _ := raw.(map[string]any)
		if entry["type"] != "Patient" {
			continue
		}
		operations, _ := entry["operation"].([]any)
		for _, item := range operations {
			operation, _ := item.(map[string]any)
			if operation["name"] == "validate" {
				return
			}
		}
	}
	t.Error("the CapabilityStatement does not declare $validate on Patient")
}
