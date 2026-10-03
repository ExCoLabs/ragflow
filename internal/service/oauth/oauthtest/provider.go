//
//  Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//

// Package oauthtest provides a fake OpenID Connect provider for tests:
// discovery, JWKS, token and userinfo endpoints backed by httptest.
package oauthtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	ClientID     = "test-client"
	ClientSecret = "test-secret"
	keyID        = "test-key"
	accessToken  = "test-access-token"
)

// Provider is a fake OIDC provider. Set the exported fields before the
// token request to shape the issued ID token and userinfo response.
type Provider struct {
	Server *httptest.Server
	key    *rsa.PrivateKey

	mu sync.Mutex
	// Code is the only authorization code the token endpoint accepts.
	Code string
	// Nonce and CodeChallenge must be copied from the authorization URL
	// (see Authorize) for the token endpoint to issue a matching ID token.
	Nonce         string
	CodeChallenge string
	// Audience of the ID token; defaults to ClientID.
	Audience string
	Subject  string
	Email    string
	// UserInfo overrides the userinfo document when set.
	UserInfo map[string]any
}

// New starts a provider; it is closed when the test ends.
func New(t *testing.T) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p := &Provider{key: key, Code: "test-code", Subject: "user-sub-1", Email: "alice@example.com"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("/jwks", p.jwks)
	mux.HandleFunc("/token", p.token)
	mux.HandleFunc("/userinfo", p.userinfo)
	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

// Issuer is the provider's issuer URL.
func (p *Provider) Issuer() string { return p.Server.URL }

// Authorize records the nonce and PKCE challenge of an authorization URL
// produced by the client, as a real provider would at its login page.
func (p *Provider) Authorize(q map[string][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if v := q["nonce"]; len(v) > 0 {
		p.Nonce = v[0]
	}
	if v := q["code_challenge"]; len(v) > 0 {
		p.CodeChallenge = v[0]
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	base := p.Server.URL
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/authorize",
		"token_endpoint":                        base + "/token",
		"userinfo_endpoint":                     base + "/userinfo",
		"jwks_uri":                              base + "/jwks",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (p *Provider) jwks(w http.ResponseWriter, _ *http.Request) {
	enc := base64.RawURLEncoding
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
		"kty": "RSA",
		"kid": keyID,
		"use": "sig",
		"alg": "RS256",
		"n":   enc.EncodeToString(p.key.N.Bytes()),
		"e":   enc.EncodeToString(big.NewInt(int64(p.key.E)).Bytes()),
	}}})
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	if r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("client_secret") != ClientSecret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != p.Code {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if p.CodeChallenge != "" && base64.RawURLEncoding.EncodeToString(sum[:]) != p.CodeChallenge {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE"})
		return
	}
	audience := p.Audience
	if audience == "" {
		audience = ClientID
	}
	now := time.Now()
	idToken := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   p.Server.URL,
		"sub":   p.Subject,
		"aud":   audience,
		"exp":   now.Add(time.Hour).Unix(),
		"iat":   now.Unix(),
		"nonce": p.Nonce,
		"email": p.Email,
	})
	idToken.Header["kid"] = keyID
	signed, err := idToken.SignedString(p.key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     signed,
	})
}

func (p *Provider) userinfo(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+accessToken {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}
	if p.UserInfo != nil {
		writeJSON(w, http.StatusOK, p.UserInfo)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sub": p.Subject, "email": p.Email})
}
