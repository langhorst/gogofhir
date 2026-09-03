package smart

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Access tokens, signed and verified here rather than by a dependency.
//
// A JWT is three base64url segments and a signature, and writing those ninety
// lines keeps the single-static-binary promise and the dependency list short.
// The keys are RSA so the JWKS this server publishes is one a real client
// library can consume: a symmetric secret would work between our own two halves
// and be useless to anybody else.

// Errors a caller distinguishes.
var (
	ErrTokenMalformed = errors.New("smart: the access token is malformed")
	ErrTokenSignature = errors.New("smart: the access token's signature does not verify")
	ErrTokenExpired   = errors.New("smart: the access token has expired")
)

// Keys signs and verifies this server's tokens.
type Keys struct {
	private *rsa.PrivateKey
	// id names the key in the JWKS and in each token's header, so a client can
	// tell which key to verify with when one is rotated.
	id string
}

// NewKeys generates a signing key.
//
// It is generated per process rather than persisted: tokens do not outlive the
// server that issued them, which for a dev and conformance server is the right
// trade -- there is no key file to manage, leak, or forget to rotate, and a
// restart simply invalidates outstanding tokens.
func NewKeys() (*Keys, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("smart: generating a signing key: %w", err)
	}
	fingerprint := sha256.Sum256(key.PublicKey.N.Bytes())
	return &Keys{private: key, id: base64.RawURLEncoding.EncodeToString(fingerprint[:8])}, nil
}

// Claims are what a token asserts.
type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub,omitempty"`
	Audience  string `json:"aud,omitempty"`
	ExpiresAt int64  `json:"exp"`
	IssuedAt  int64  `json:"iat"`
	// Scope is the space-separated grant, as the client asked for it.
	Scope string `json:"scope,omitempty"`
	// Patient is the launch context patient, when there is one.
	Patient  string `json:"patient,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	// Kind separates access tokens from refresh tokens and authorization
	// codes, so one cannot be presented where another is expected.
	Kind string `json:"gogofhir_kind,omitempty"`
	// Challenge carries a PKCE code challenge on an authorization code, so the
	// code and its verifier travel together without server-side state.
	Challenge string `json:"gogofhir_challenge,omitempty"`
	Redirect  string `json:"gogofhir_redirect,omitempty"`
}

// Sign issues a token.
func (k *Keys) Sign(claims Claims, lifetime time.Duration) (string, error) {
	now := time.Now()
	claims.IssuedAt = now.Unix()
	claims.ExpiresAt = now.Add(lifetime).Unix()

	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": k.id})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := encode(header) + "." + encode(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, k.private, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + encode(signature), nil
}

// Verify checks a token's signature and expiry and returns its claims.
func (k *Keys) Verify(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, ErrTokenMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&k.private.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		return Claims{}, ErrTokenSignature
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrTokenMalformed
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrTokenMalformed
	}
	if time.Now().Unix() >= claims.ExpiresAt {
		return Claims{}, ErrTokenExpired
	}
	return claims, nil
}

// JWKS renders the public key as a JSON Web Key Set, which is how a client
// verifies a token this server issued without asking it anything.
func (k *Keys) JWKS() map[string]any {
	public := k.private.PublicKey
	return map[string]any{"keys": []any{map[string]any{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": k.id,
		"n":   base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
	}}}
}

func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// VerifyPKCE checks a code verifier against the challenge an authorization
// request carried.
//
// Only S256 is accepted. The "plain" method sends the verifier itself as the
// challenge, which protects against nothing, and SMART v2 requires S256.
func VerifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:]) == challenge
}
