package smart_test

import (
	"strings"
	"testing"
	"time"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/smart"
)

// Scopes, tokens and PKCE, tested at the unit level: the REST suite exercises
// the flow, and these pin the pieces a wrong answer would quietly widen.

func TestScopeParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want string // rendered in v2 syntax, or "" when the scope is refused
	}{
		// v2 spells permissions as letters, which is what lets reading one
		// resource be told apart from searching for many.
		{"patient/Observation.rs", "patient/Observation.rs"},
		{"user/*.cruds", "user/*.cruds"},
		{"system/Patient.r", "system/Patient.r"},
		// v1 is what most existing apps send.
		{"patient/Observation.read", "patient/Observation.rs"},
		{"user/Patient.write", "user/Patient.cud"},
		{"patient/*.*", "patient/*.cruds"},
		// Not permission scopes at all.
		{"openid", ""},
		{"fhirUser", ""},
		{"launch/patient", ""},
		{"offline_access", ""},
		// Malformed, or a context this server does not know.
		{"admin/Patient.read", ""},
		{"patient/Observation", ""},
		{"patient/Observation.xyz", ""},
		// A v2 scope may carry a search filter. Enforcing it would mean
		// applying the filter to every query; accepting it without doing so
		// would silently widen the grant to the whole resource type.
		{"patient/Observation.rs?category=vital-signs", ""},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := smart.ParseScopes(tc.raw)
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("parsed %v, want the scope refused", got)
				}
				return
			}
			if len(got) != 1 || got[0].String() != tc.want {
				t.Errorf("parsed %v, want %s", got, tc.want)
			}
		})
	}
}

// A patient scope with no launch context names nobody, so it grants nothing --
// the alternative is a token that reads "this patient's data" and is enforced
// as "everyone's".
func TestPatientScopeNeedsContext(t *testing.T) {
	grant := smart.Grant{Scopes: smart.ParseScopes("patient/Observation.rs")}
	if _, ok := grant.Allows("Observation", smart.PermRead); ok {
		t.Error("a patient scope without a launch patient granted access")
	}
	grant.Patient = "p1"
	context, ok := grant.Allows("Observation", smart.PermRead)
	if !ok || context != "patient" {
		t.Errorf("context = %q, ok = %v; want a patient-confined grant", context, ok)
	}
}

// A token holding both is not confined by the narrower one.
func TestWiderContextWins(t *testing.T) {
	grant := smart.Grant{
		Scopes:  smart.ParseScopes("patient/Observation.rs user/Observation.rs"),
		Patient: "p1",
	}
	if context, _ := grant.Allows("Observation", smart.PermRead); context != "user" {
		t.Errorf("context = %q, want user: the wider grant is not narrowed by the other", context)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	keys, err := smart.NewKeys()
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	token, err := keys.Sign(smart.Claims{
		Issuer: "https://example.org/fhir", Scope: "user/*.rs", Kind: "access",
	}, time.Minute)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	claims, err := keys.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.Scope != "user/*.rs" {
		t.Errorf("scope = %q", claims.Scope)
	}

	// A token whose payload was edited must not verify, which is the whole
	// point of signing it.
	parts := strings.Split(token, ".")
	tampered := parts[0] + ".eyJzY29wZSI6InN5c3RlbS8qLmNydWRzIn0." + parts[2]
	if _, err := keys.Verify(tampered); err == nil {
		t.Error("a tampered token verified")
	}

	// An expired token is refused, and distinguishably so.
	expired, err := keys.Sign(smart.Claims{Issuer: "x"}, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Verify(expired); err != smart.ErrTokenExpired {
		t.Errorf("expired token gave %v, want ErrTokenExpired", err)
	}

	// The JWKS has to carry the key a client would verify with.
	jwks, _ := keys.JWKS()["keys"].([]any)
	if len(jwks) != 1 {
		t.Fatalf("JWKS holds %d keys, want 1", len(jwks))
	}
	key, _ := jwks[0].(map[string]any)
	if key["kty"] != "RSA" || key["n"] == nil || key["e"] == nil {
		t.Errorf("the JWKS entry is not a usable RSA key: %v", key)
	}
}

func TestPKCE(t *testing.T) {
	// The example from RFC 7636, so the implementation is checked against the
	// specification rather than against itself.
	const (
		verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	if !smart.VerifyPKCE(verifier, challenge) {
		t.Error("the RFC 7636 example does not verify")
	}
	if smart.VerifyPKCE("something-else", challenge) {
		t.Error("a wrong verifier passed")
	}
	if smart.VerifyPKCE("", challenge) || smart.VerifyPKCE(verifier, "") {
		t.Error("an empty verifier or challenge passed")
	}
}

// The compartment confinement is a disjunction over every parameter the
// release says links a type to a patient. Taking only the first would hide a
// patient's own records from an app entitled to see them.
func TestCompartmentFilter(t *testing.T) {
	idx := conformance.MustLoad(conformance.R5)

	filter, ok := smart.CompartmentFilter(idx, "Observation", "p1")
	if !ok {
		t.Fatal("Observation is in the patient compartment")
	}
	if !strings.Contains(filter, "subject eq Patient/p1") || !strings.Contains(filter, " or ") {
		t.Errorf("filter = %q, want a disjunction naming every linking parameter", filter)
	}

	// The compartment's own resource is named by its id.
	filter, ok = smart.CompartmentFilter(idx, "Patient", "p1")
	if !ok || !strings.Contains(filter, "_id eq p1") {
		t.Errorf("Patient filter = %q, want it matched by id", filter)
	}

	// A type outside the compartment has no filter, and the caller has to
	// refuse rather than search unconfined.
	if _, ok := smart.CompartmentFilter(idx, "Organization", "p1"); ok {
		t.Error("Organization is not in the patient compartment")
	}
}
