package rest_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/langhorst/gogofhir/internal/smart"
)

// SMART App Launch, end to end: discovery, the authorization code flow with
// PKCE, scope enforcement, and the patient compartment.
//
// The compartment cases are the ones that matter most. A server that checks
// scopes but not the compartment looks authorized and leaks every patient to
// any app that asks for patient/Observation.rs.

const (
	testClientID = "test-app"
	testRedirect = "http://localhost:9999/redirect"
	testVerifier = "a-code-verifier-long-enough-to-be-plausible-0123456789"
)

// smartServer starts a server with SMART enabled and a launch patient, and
// returns the client plus the patient the token will be scoped to.
func smartServer(t *testing.T) (*client, string) {
	t.Helper()
	// The patient has to exist before the authorization server points at it,
	// so an unauthenticated server seeds the data first.
	seed := newServer(t)
	patient := seed.createPatient("S1", "Chalmers")
	seed.createObservationFor(patient, "29463-7", 70)
	other := seed.createPatient("S2", "Nowak")
	seed.createObservationFor(other, "29463-7", 80)

	keys, err := smart.NewKeys()
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	authorization := smart.New(smart.Config{
		Issuer: seed.base,
		Keys:   keys,
		Clients: map[string]smart.Client{testClientID: {
			ID: testClientID, RedirectURIs: []string{testRedirect},
		}},
		Patient: patient,
	})
	// The same store, now behind authorization.
	guarded := seed.restart(t, withSMART(authorization))
	return guarded, patient
}

// token runs the authorization code flow and returns the access token.
func (c *client) token(t *testing.T, scope string) (accessToken, launchPatient string) {
	t.Helper()
	digest := sha256.Sum256([]byte(testVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])

	authorize := "/auth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {testClientID},
		"redirect_uri":          {testRedirect},
		"scope":                 {scope},
		"state":                 {"xyz"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	resp := c.expect(http.StatusFound, "GET", authorize, "")
	location, err := url.Parse(resp.headers.Get("Location"))
	if err != nil {
		t.Fatalf("parsing the redirect: %v", err)
	}
	if got := location.Query().Get("error"); got != "" {
		t.Fatalf("authorization failed: %s (%s)", got, location.Query().Get("error_description"))
	}
	if got := location.Query().Get("state"); got != "xyz" {
		t.Errorf("state = %q, want it echoed back", got)
	}
	code := location.Query().Get("code")
	if code == "" {
		t.Fatalf("no authorization code in %s", location)
	}

	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {testRedirect},
		"client_id":     {testClientID},
		"code_verifier": {testVerifier},
	}.Encode()
	tokenResp := c.form(http.StatusOK, "/auth/token", body)
	var payload map[string]any
	if err := json.Unmarshal(tokenResp.body, &payload); err != nil {
		t.Fatalf("decoding the token response: %v\n%s", err, tokenResp.body)
	}
	accessToken, _ = payload["access_token"].(string)
	launchPatient, _ = payload["patient"].(string)
	if accessToken == "" {
		t.Fatalf("no access token in %s", tokenResp.body)
	}
	return accessToken, launchPatient
}

func TestSMARTDiscovery(t *testing.T) {
	c, _ := smartServer(t)
	resp := c.expect(http.StatusOK, "GET", "/.well-known/smart-configuration", "")
	var config map[string]any
	if err := json.Unmarshal(resp.body, &config); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, key := range []string{"authorization_endpoint", "token_endpoint", "issuer",
		"capabilities", "code_challenge_methods_supported"} {
		if config[key] == nil {
			t.Errorf("the discovery document has no %s: %s", key, resp.body)
		}
	}

	// The CapabilityStatement has to say the server is protected, and where the
	// endpoints are: a client with only a base URL reads it there.
	metadata := c.expect(http.StatusOK, "GET", "/metadata", "").json(t)
	restBlock, _ := metadata["rest"].([]any)
	first, _ := restBlock[0].(map[string]any)
	security, _ := first["security"].(map[string]any)
	if security == nil || security["service"] == nil || security["extension"] == nil {
		t.Errorf("the CapabilityStatement does not declare SMART security: %v", security)
	}
}

// Without a token nothing but discovery answers, and the refusal says how to
// get one rather than leaving the client to guess.
func TestSMARTRequiresAToken(t *testing.T) {
	c, _ := smartServer(t)
	resp := c.expect(http.StatusUnauthorized, "GET", "/Patient", "")
	if got := resp.headers.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	assertOutcome(t, resp, "login")

	// Discovery and the CapabilityStatement stay open, since they are how a
	// client finds out it needs a token at all.
	c.expect(http.StatusOK, "GET", "/metadata", "")
	c.expect(http.StatusOK, "GET", "/.well-known/smart-configuration", "")

	// A token that is not one.
	c.expect(http.StatusUnauthorized, "GET", "/Patient", "", "Authorization", "Bearer nonsense")
}

// The scopes decide which types and which operations a token covers, and the
// refusal names the scope that would have worked.
func TestSMARTScopeEnforcement(t *testing.T) {
	c, _ := smartServer(t)
	token, _ := c.token(t, "user/Patient.rs")
	auth := []string{"Authorization", "Bearer " + token}

	c.expect(http.StatusOK, "GET", "/Patient", "", auth...)
	resp := c.expect(http.StatusForbidden, "GET", "/Observation", "", auth...)
	assertOutcome(t, resp, "forbidden")
	if !strings.Contains(string(resp.body), "user/Observation.s") {
		t.Errorf("the refusal does not name a scope that would work: %s", resp.body)
	}
	// Reading is not writing.
	c.expect(http.StatusForbidden, "POST", "/Patient", patientJSON("S9", "New"), auth...)

	// v1 syntax is accepted, and "read" covers searching.
	readToken, _ := c.token(t, "user/*.read")
	c.expect(http.StatusOK, "GET", "/Observation", "", "Authorization", "Bearer "+readToken)
	c.expect(http.StatusForbidden, "POST", "/Patient", patientJSON("S9", "New"),
		"Authorization", "Bearer "+readToken)
}

// The part a server that only checks scopes gets wrong: a patient-scoped token
// may read Observations, but only the ones in its patient's compartment.
func TestSMARTPatientCompartment(t *testing.T) {
	c, patient := smartServer(t)
	token, launch := c.token(t, "launch/patient patient/*.rs")
	if launch != patient {
		t.Errorf("the token response carried patient %q, want %q", launch, patient)
	}
	auth := []string{"Authorization", "Bearer " + token}

	// Two patients and two observations exist; this token sees one of each.
	if got := c.total("/Patient", auth...); got != 1 {
		t.Errorf("a patient-scoped search returned %v patients, want only the one in context", got)
	}
	if got := c.total("/Observation", auth...); got != 1 {
		t.Errorf("a patient-scoped search returned %v observations, want only the compartment's", got)
	}

	// The instance interactions are confined the same way. The patient in
	// context reads; the other one is simply not there -- 404 rather than 403,
	// because telling an app that a resource it may not see nonetheless exists
	// is itself a disclosure.
	c.expect(http.StatusOK, "GET", "/Patient/"+patient, "", auth...)
	c.expect(http.StatusNotFound, "GET", "/Patient/"+c.otherPatient(t, patient), "", auth...)

	// An Observation is in a patient's compartment through subject *or*
	// performer, so the confinement is a disjunction over every parameter the
	// compartment names -- taking only the first would hide a patient's own
	// records from an app entitled to see them.
	if got := c.total("/Observation?code=29463-7", auth...); got != 1 {
		t.Errorf("the client's own criteria and the compartment must both apply: got %v", got)
	}
	if got := c.total("/Observation?code=nonesuch", auth...); got != 0 {
		t.Errorf("the compartment must narrow the client's criteria, not replace them: got %v", got)
	}
}

// otherPatient returns the id of a patient that is not the one given, read
// through an unauthenticated view of the same data.
func (c *client) otherPatient(t *testing.T, exclude string) string {
	t.Helper()
	bundle := c.restart(t).expect(http.StatusOK, "GET", "/Patient", "").json(t)
	entries, _ := bundle["entry"].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		res, _ := entry["resource"].(map[string]any)
		if id, _ := res["id"].(string); id != "" && id != exclude {
			return id
		}
	}
	t.Fatal("the fixture should hold a second patient")
	return ""
}

// A type outside the patient compartment cannot be searched by a
// patient-scoped token at all. The failure mode has to be closed: returning
// everything would be the leak.
func TestSMARTOutsideTheCompartment(t *testing.T) {
	c, _ := smartServer(t)
	token, _ := c.token(t, "launch/patient patient/*.rs")
	resp := c.expect(http.StatusForbidden, "GET", "/Organization", "",
		"Authorization", "Bearer "+token)
	if !strings.Contains(string(resp.body), "patient compartment") {
		t.Errorf("the refusal does not explain the compartment: %s", resp.body)
	}
}

// A public client must prove it started the flow, since it has no secret to
// prove it with.
func TestSMARTRequiresPKCE(t *testing.T) {
	c, _ := smartServer(t)
	authorize := "/auth/authorize?" + url.Values{
		"response_type": {"code"},
		"client_id":     {testClientID},
		"redirect_uri":  {testRedirect},
		"scope":         {"user/Patient.rs"},
	}.Encode()
	resp := c.expect(http.StatusFound, "GET", authorize, "")
	location, _ := url.Parse(resp.headers.Get("Location"))
	if got := location.Query().Get("error"); got != "invalid_request" {
		t.Errorf("a public client without PKCE was allowed through: %v", location)
	}

	// An unregistered redirect is refused without redirecting to it, which is
	// the open redirect it would otherwise be.
	c.expect(http.StatusBadRequest, "GET", "/auth/authorize?"+url.Values{
		"response_type": {"code"},
		"client_id":     {testClientID},
		"redirect_uri":  {"http://evil.example/steal"},
	}.Encode(), "")
}

// The token endpoint must not accept a code without its verifier, or with the
// wrong one.
func TestSMARTTokenExchangeChecksTheVerifier(t *testing.T) {
	c, _ := smartServer(t)
	digest := sha256.Sum256([]byte(testVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	resp := c.expect(http.StatusFound, "GET", "/auth/authorize?"+url.Values{
		"response_type":         {"code"},
		"client_id":             {testClientID},
		"redirect_uri":          {testRedirect},
		"scope":                 {"user/Patient.rs"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode(), "")
	location, _ := url.Parse(resp.headers.Get("Location"))
	code := location.Query().Get("code")

	for _, verifier := range []string{"", "the-wrong-verifier"} {
		body := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {testRedirect},
			"client_id":     {testClientID},
			"code_verifier": {verifier},
		}.Encode()
		c.form(http.StatusBadRequest, "/auth/token", body)
	}
}

// A transaction is authorized once, at the bundle, and its entries inherit that
// rather than being asked for a token the client never sent them.
func TestSMARTTransaction(t *testing.T) {
	c, _ := smartServer(t)
	token, _ := c.token(t, "user/*.cruds")
	resp := c.expect(http.StatusOK, "POST", "/", `{
	  "resourceType": "Bundle", "type": "transaction",
	  "entry": [{"resource": {"resourceType": "Patient", "gender": "female"},
	             "request": {"method": "POST", "url": "Patient"}}]
	}`, "Authorization", "Bearer "+token)
	entries, _ := resp.json(t)["entry"].([]any)
	entry, _ := entries[0].(map[string]any)
	response, _ := entry["response"].(map[string]any)
	if status, _ := response["status"].(string); status != "201 Created" {
		t.Errorf("transaction entry status = %q, want 201 Created", status)
	}
}
