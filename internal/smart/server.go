package smart

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The authorization server.
//
// A dev and conformance server that made you stand up Keycloak before it would
// answer a SMART launch would be one nobody bothers to point Inferno at, so the
// authorization server is built in. It is deliberately small: authorization
// code with PKCE, refresh tokens, and client credentials for backend services.
//
// It holds no session state. An authorization code is a short-lived signed
// token carrying the grant it will become, which is why there is no store to
// clean up and no way for two replicas to disagree about which codes are
// outstanding.

// Lifetimes, chosen so a conformance run is not spent waiting and a stale
// token does not linger.
const (
	codeLifetime    = 5 * time.Minute
	tokenLifetime   = time.Hour
	refreshLifetime = 24 * time.Hour
)

// Client is a registered application.
type Client struct {
	ID string
	// Secret is empty for a public client, which is what a browser or mobile
	// app is: it cannot keep one, so PKCE is what proves the token request came
	// from whoever started the flow.
	Secret string
	// RedirectURIs are matched exactly. A prefix or wildcard match is how an
	// open redirect gets built by accident.
	RedirectURIs []string
}

// Config configures the authorization server.
type Config struct {
	// Issuer is this server's own FHIR base URL, which is what a token's "iss"
	// and the discovery document are written against.
	Issuer string
	Keys   *Keys
	// Clients are the registered applications, by id.
	Clients map[string]Client
	// Patient is the launch context handed to patient-scoped tokens.
	//
	// A real authorization server asks a human which patient an app may see.
	// This one is a test fixture and is told, because a conformance run has
	// nobody to ask -- and saying so is better than a login screen that
	// approves everything while looking like it decided something.
	Patient string
}

// Server is the SMART authorization server.
type Server struct{ cfg Config }

func New(cfg Config) *Server { return &Server{cfg: cfg} }

// Routes registers the authorization endpoints on a mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/smart-configuration", s.discovery)
	mux.HandleFunc("GET /auth/jwks", s.jwks)
	mux.HandleFunc("GET /auth/authorize", s.authorize)
	mux.HandleFunc("POST /auth/token", s.token)
}

// AuthorizeURL and the rest name the endpoints, so the discovery document and
// the CapabilityStatement cannot drift from the routes.
func (s *Server) AuthorizeURL() string { return s.cfg.Issuer + "/auth/authorize" }
func (s *Server) TokenURL() string     { return s.cfg.Issuer + "/auth/token" }
func (s *Server) JWKSURL() string      { return s.cfg.Issuer + "/auth/jwks" }

// Verify checks a bearer token and returns the grant it carries.
func (s *Server) Verify(token string) (Grant, error) {
	claims, err := s.cfg.Keys.Verify(token)
	if err != nil {
		return Grant{}, err
	}
	if claims.Kind != "access" {
		return Grant{}, fmt.Errorf("smart: that token is a %s token, not an access token", claims.Kind)
	}
	return Grant{
		Scopes:   ParseScopes(claims.Scope),
		Patient:  claims.Patient,
		Subject:  claims.Subject,
		ClientID: claims.ClientID,
	}, nil
}

// ---- discovery ----

// discovery serves .well-known/smart-configuration, which is how an app finds
// the endpoints without being configured with them.
func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                 s.cfg.Issuer,
		"jwks_uri":               s.JWKSURL(),
		"authorization_endpoint": s.AuthorizeURL(),
		"token_endpoint":         s.TokenURL(),
		"grant_types_supported": []string{
			"authorization_code", "refresh_token", "client_credentials",
		},
		"scopes_supported": []string{
			"openid", "fhirUser", "launch/patient", "offline_access",
			"patient/*.rs", "user/*.rs", "system/*.rs",
			"patient/*.cruds", "user/*.cruds", "system/*.cruds",
		},
		"response_types_supported":              []string{"code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"capabilities": []string{
			"launch-standalone",
			"client-public",
			"client-confidential-symmetric",
			"context-standalone-patient",
			"permission-patient",
			"permission-user",
			"permission-v1",
			"permission-v2",
			"authorize-post",
		},
	})
}

func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.Keys.JWKS())
}

// ---- authorization ----

// authorize is the front door of the authorization code flow.
//
// There is no login screen. A test server has nobody to ask, and a screen that
// approves whatever it is shown would be theatre -- worse than none, because it
// looks like a decision was made. The grant is issued as configured and the
// code goes straight back to the redirect URI.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirect := q.Get("redirect_uri")
	state := q.Get("state")

	client, known := s.cfg.Clients[clientID]
	if !known {
		// An unknown client or a redirect the client did not register cannot be
		// reported by redirecting: doing so is the open redirect itself.
		s.authorizeError(w, http.StatusBadRequest, "unknown client_id")
		return
	}
	if !allowedRedirect(client, redirect) {
		s.authorizeError(w, http.StatusBadRequest,
			"redirect_uri is not registered for this client")
		return
	}

	fail := func(code, description string) {
		target, _ := url.Parse(redirect)
		values := target.Query()
		values.Set("error", code)
		values.Set("error_description", description)
		if state != "" {
			values.Set("state", state)
		}
		target.RawQuery = values.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	}

	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only the authorization code flow is supported")
		return
	}
	challenge := q.Get("code_challenge")
	if client.Secret == "" && challenge == "" {
		// A public client has no secret, so PKCE is the only thing tying the
		// token request to whoever started the flow.
		fail("invalid_request", "a public client must use PKCE")
		return
	}
	if challenge != "" && q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "code_challenge_method must be S256")
		return
	}

	scope := q.Get("scope")
	claims := Claims{
		Issuer: s.cfg.Issuer, Audience: s.cfg.Issuer,
		Scope: scope, ClientID: clientID, Kind: "code",
		Challenge: challenge, Redirect: redirect,
		Subject: "gogofhir-test-user",
	}
	if wantsPatientContext(scope) {
		claims.Patient = s.cfg.Patient
	}
	code, err := s.cfg.Keys.Sign(claims, codeLifetime)
	if err != nil {
		fail("server_error", "issuing the authorization code failed")
		return
	}

	target, _ := url.Parse(redirect)
	values := target.Query()
	values.Set("code", code)
	if state != "" {
		values.Set("state", state)
	}
	target.RawQuery = values.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// authorizeError reports a failure that must not be redirected, as a page
// rather than as JSON: whoever sees it is a browser that followed a link.
func (s *Server) authorizeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><title>Authorization failed</title>"+
		"<h1>Authorization failed</h1><p>%s</p>", html.EscapeString(message))
}

// wantsPatientContext reports whether the request asked to be launched with a
// patient, either explicitly or by asking for patient-scoped data.
func wantsPatientContext(scope string) bool {
	for _, field := range strings.Fields(scope) {
		if field == "launch/patient" || strings.HasPrefix(field, "patient/") {
			return true
		}
	}
	return false
}

func allowedRedirect(client Client, redirect string) bool {
	for _, allowed := range client.RedirectURIs {
		// Exact match only. A prefix match is how an open redirect gets built
		// by accident.
		if allowed == redirect {
			return true
		}
	}
	return false
}

// ---- tokens ----

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, http.StatusBadRequest, "invalid_request", "the request body is not a form")
		return
	}
	clientID, secret, ok := clientCredentials(r)
	if !ok {
		tokenError(w, http.StatusBadRequest, "invalid_request", "no client was identified")
		return
	}
	client, known := s.cfg.Clients[clientID]
	if !known || (client.Secret != "" && client.Secret != secret) {
		// One message for both cases: which of the two failed is not something
		// an unauthenticated caller should be able to probe for.
		tokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}

	switch grantType := r.PostFormValue("grant_type"); grantType {
	case "authorization_code":
		s.exchangeCode(w, r, client)
	case "refresh_token":
		s.refresh(w, r, client)
	case "client_credentials":
		s.clientCredentials(w, r, client)
	default:
		tokenError(w, http.StatusBadRequest, "unsupported_grant_type",
			fmt.Sprintf("%q is not a grant type this server issues", grantType))
	}
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, client Client) {
	claims, err := s.cfg.Keys.Verify(r.PostFormValue("code"))
	if err != nil || claims.Kind != "code" {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "the authorization code is not valid")
		return
	}
	if claims.ClientID != client.ID {
		tokenError(w, http.StatusBadRequest, "invalid_grant",
			"the authorization code was issued to another client")
		return
	}
	// The redirect_uri has to match the one the code was issued against, which
	// is what stops a code stolen from one app being redeemed by another.
	if redirect := r.PostFormValue("redirect_uri"); redirect != "" && redirect != claims.Redirect {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match")
		return
	}
	if claims.Challenge != "" && !VerifyPKCE(r.PostFormValue("code_verifier"), claims.Challenge) {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "the PKCE verifier does not match")
		return
	}
	s.issue(w, claims.Scope, claims.Patient, claims.Subject, client, strings.Contains(claims.Scope, "offline_access"))
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, client Client) {
	claims, err := s.cfg.Keys.Verify(r.PostFormValue("refresh_token"))
	if err != nil || claims.Kind != "refresh" || claims.ClientID != client.ID {
		tokenError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is not valid")
		return
	}
	scope := claims.Scope
	// A refresh may narrow the grant but never widen it.
	if requested := r.PostFormValue("scope"); requested != "" {
		if !within(requested, claims.Scope) {
			tokenError(w, http.StatusBadRequest, "invalid_scope",
				"a refresh may narrow the original grant, not widen it")
			return
		}
		scope = requested
	}
	s.issue(w, scope, claims.Patient, claims.Subject, client, true)
}

// clientCredentials is the SMART Backend Services shape, for a service acting
// on its own behalf rather than for a user.
//
// The specification authenticates such a client with a signed JWT assertion
// against a public key it registered. This server accepts a client secret
// instead, which is a documented divergence: the flow, the scopes and the
// tokens are the real thing, and only the client's own authentication is
// simpler.
func (s *Server) clientCredentials(w http.ResponseWriter, r *http.Request, client Client) {
	if client.Secret == "" {
		tokenError(w, http.StatusUnauthorized, "invalid_client",
			"client_credentials needs a confidential client")
		return
	}
	scope := r.PostFormValue("scope")
	for _, parsed := range ParseScopes(scope) {
		if parsed.Context != "system" {
			tokenError(w, http.StatusBadRequest, "invalid_scope",
				"client_credentials issues system/ scopes only, since there is no user or patient in context")
			return
		}
	}
	s.issue(w, scope, "", "", client, false)
}

// issue writes a token response.
func (s *Server) issue(w http.ResponseWriter, scope, patient, subject string, client Client, offline bool) {
	claims := Claims{
		Issuer: s.cfg.Issuer, Audience: s.cfg.Issuer, Subject: subject,
		Scope: scope, Patient: patient, ClientID: client.ID, Kind: "access",
	}
	access, err := s.cfg.Keys.Sign(claims, tokenLifetime)
	if err != nil {
		tokenError(w, http.StatusInternalServerError, "server_error", "issuing the token failed")
		return
	}

	body := map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   int(tokenLifetime.Seconds()),
		"scope":        scope,
	}
	if patient != "" {
		// The launch context travels in the token response, which is how an app
		// learns which patient it was launched for.
		body["patient"] = patient
	}
	if offline {
		claims.Kind = "refresh"
		if refresh, err := s.cfg.Keys.Sign(claims, refreshLifetime); err == nil {
			body["refresh_token"] = refresh
		}
	}
	// Tokens must not be cached anywhere on the way back.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, body)
}

// within reports whether every scope in requested appears in granted.
func within(requested, granted string) bool {
	held := map[string]bool{}
	for _, field := range strings.Fields(granted) {
		held[field] = true
	}
	for _, field := range strings.Fields(requested) {
		if !held[field] {
			return false
		}
	}
	return true
}

// clientCredentials reads the client's identity from either place OAuth allows.
func clientCredentials(r *http.Request) (id, secret string, ok bool) {
	if id, secret, ok := r.BasicAuth(); ok {
		return id, secret, true
	}
	id = r.PostFormValue("client_id")
	return id, r.PostFormValue("client_secret"), id != ""
}

func tokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]any{
		"error":             code,
		"error_description": description,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
