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

package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"ragflow/internal/server/config"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// defaultOIDCScope is requested when an oidc channel configures no scope.
const defaultOIDCScope = "openid profile email"

// oidcClient is the authorization-code flow of OpenID Connect. Endpoints come
// from the issuer's discovery document; the ID token is verified against the
// provider's JWKS (signature, iss, aud, exp and the login nonce).
type oidcClient struct {
	oauth2Client
	verifier *oidc.IDTokenVerifier
}

func newOIDCClient(ctx context.Context, cfg config.OAuthChannelConfig, httpClient *http.Client) (*oidcClient, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("oidc channel needs an issuer")
	}
	// The context only configures the HTTP client go-oidc keeps for the JWKS
	// fetches; it is not used for cancellation.
	discoveryCtx := oidc.ClientContext(context.WithoutCancel(ctx), httpClient)
	provider, err := oidc.NewProvider(discoveryCtx, cfg.Issuer)
	var mismatch *oidc.IssuerMismatchError
	if errors.As(err, &mismatch) && strings.TrimRight(mismatch.Discovered, "/") == strings.TrimRight(cfg.Issuer, "/") {
		// Tolerate a trailing-slash difference between the configured and the
		// advertised issuer; ID tokens are then checked against the advertised one.
		provider, err = oidc.NewProvider(oidc.InsecureIssuerURLContext(discoveryCtx, mismatch.Discovered), cfg.Issuer)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to fetch OIDC metadata: %w", err)
	}

	if cfg.Scope == "" {
		cfg.Scope = defaultOIDCScope
	}
	// SupportedSigningAlgs is left empty: go-oidc then uses the algorithms the
	// provider advertises, restricted to asymmetric ones (RS256 if none), so a
	// token header cannot select "none" or an HMAC algorithm.
	return &oidcClient{
		oauth2Client: oauth2Client{
			conf:        newOAuth2Config(cfg, provider.Endpoint()),
			userInfoURL: provider.UserInfoEndpoint(),
			httpClient:  httpClient,
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (c *oidcClient) AuthCodeURL(state, nonce, verifier string) string {
	return c.conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

func (c *oidcClient) Authenticate(ctx context.Context, code, nonce, verifier string) (*UserInfo, error) {
	tok, err := exchange(ctx, c.conf, c.httpClient, code, verifier)
	if err != nil {
		return nil, err
	}
	rawIDToken, _ := tok.Extra("id_token").(string)
	if rawIDToken == "" {
		return nil, newError(ErrCodeInvalidIDToken, "token response has no id_token")
	}
	idToken, err := c.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, &Error{Code: ErrCodeInvalidIDToken, Err: err}
	}
	if idToken.Nonce != nonce {
		return nil, newError(ErrCodeInvalidIDToken, "id_token nonce does not match the login request")
	}
	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, &Error{Code: ErrCodeInvalidIDToken, Err: err}
	}

	// Userinfo claims take precedence over the ID token (as in the Python
	// client); ID token claims fill whatever userinfo leaves out.
	if c.userInfoURL != "" {
		userInfo, err := fetchJSON(ctx, c.httpClient, c.userInfoURL, tok.AccessToken)
		if err != nil {
			return nil, &Error{Code: ErrCodeUserInfoFailed, Err: err}
		}
		if sub := claimString(userInfo, "sub"); sub != "" && sub != idToken.Subject {
			return nil, newError(ErrCodeUserInfoFailed, "userinfo sub does not match the id_token")
		}
		for k, v := range userInfo {
			if v != nil && v != "" {
				claims[k] = v
			}
		}
	}
	if err := checkEmailVerified(claims); err != nil {
		return nil, err
	}
	return normalizeUserInfo(claims), nil
}
