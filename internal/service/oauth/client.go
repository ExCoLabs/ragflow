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

// Package oauth implements the OAuth2 / OpenID Connect clients behind the
// SSO login channels configured in the `oauth:` section of service_conf.yaml.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"ragflow/internal/server/config"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// httpTimeout bounds every call to the identity provider (same as the
// Python client).
const httpTimeout = 7 * time.Second

// maxResponseBytes caps the userinfo / discovery bodies we are willing to read.
const maxResponseBytes = 1 << 20

// Error codes returned to the browser as `/?error=<code>`.
const (
	ErrCodeTokenFailed     = "token_failed"
	ErrCodeInvalidIDToken  = "invalid_id_token"
	ErrCodeUserInfoFailed  = "userinfo_failed"
	ErrCodeEmailUnverified = "email_unverified"
)

// Error is a login failure with a short code that is safe to show to the user;
// the wrapped error carries the details for the log.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

func newError(code string, format string, args ...any) *Error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// UserInfo is the normalized identity returned by every client type.
type UserInfo struct {
	Email     string
	Username  string
	Nickname  string
	AvatarURL string
	// Claims are the raw (merged) provider claims, e.g. for the groups claim.
	Claims map[string]any
}

// Client drives one login channel.
type Client interface {
	// AuthCodeURL returns the provider URL the browser is sent to.
	AuthCodeURL(state, nonce, verifier string) string
	// Authenticate exchanges the authorization code and returns the user.
	Authenticate(ctx context.Context, code, nonce, verifier string) (*UserInfo, error)
}

var (
	clientsMu sync.Mutex
	clients   = map[string]Client{}
)

// GetClient returns the (cached) client for a channel. OIDC discovery is only
// cached once it succeeded, so a provider outage does not stick.
func GetClient(ctx context.Context, channel string, cfg config.OAuthChannelConfig) (Client, error) {
	key := fmt.Sprintf("%s|%#v", channel, cfg)
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if cli, ok := clients[key]; ok {
		return cli, nil
	}
	cli, err := newClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	clients[key] = cli
	return cli, nil
}

func newClient(ctx context.Context, cfg config.OAuthChannelConfig) (Client, error) {
	httpClient := &http.Client{Timeout: httpTimeout}
	switch cfg.Type {
	case "oidc":
		return newOIDCClient(ctx, cfg, httpClient)
	case "github":
		return newGitHubClient(cfg, httpClient), nil
	case "oauth2", "":
		if cfg.AuthorizationURL == "" || cfg.TokenURL == "" || cfg.UserInfoURL == "" {
			return nil, errors.New("oauth2 channel needs authorization_url, token_url and userinfo_url")
		}
		return &oauth2Client{
			conf:        newOAuth2Config(cfg, oauth2.Endpoint{AuthURL: cfg.AuthorizationURL, TokenURL: cfg.TokenURL}),
			userInfoURL: cfg.UserInfoURL,
			httpClient:  httpClient,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported oauth channel type: %s", cfg.Type)
	}
}

func newOAuth2Config(cfg config.OAuthChannelConfig, endpoint oauth2.Endpoint) *oauth2.Config {
	// Client credentials go in the POST body, as the Python client sent them.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     endpoint,
		RedirectURL:  cfg.RedirectURI,
		Scopes:       strings.Fields(cfg.Scope),
	}
}

// oauth2Client is the plain OAuth2 authorization-code flow followed by a
// userinfo call.
type oauth2Client struct {
	conf        *oauth2.Config
	userInfoURL string
	httpClient  *http.Client
}

func (c *oauth2Client) AuthCodeURL(state, _, verifier string) string {
	return c.conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

func (c *oauth2Client) Authenticate(ctx context.Context, code, _, verifier string) (*UserInfo, error) {
	tok, err := exchange(ctx, c.conf, c.httpClient, code, verifier)
	if err != nil {
		return nil, err
	}
	claims, err := fetchJSON(ctx, c.httpClient, c.userInfoURL, tok.AccessToken)
	if err != nil {
		return nil, &Error{Code: ErrCodeUserInfoFailed, Err: err}
	}
	if err := checkEmailVerified(claims); err != nil {
		return nil, err
	}
	return normalizeUserInfo(claims), nil
}

func exchange(ctx context.Context, conf *oauth2.Config, httpClient *http.Client, code, verifier string) (*oauth2.Token, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	tok, err := conf.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, &Error{Code: ErrCodeTokenFailed, Err: err}
	}
	if tok.AccessToken == "" {
		return nil, newError(ErrCodeTokenFailed, "token response has no access_token")
	}
	return tok, nil
}

// fetchJSON GETs url with the bearer token and decodes a JSON object (a
// userinfo document).
func fetchJSON(ctx context.Context, httpClient *http.Client, url, accessToken string) (map[string]any, error) {
	var out map[string]any
	if err := getJSON(ctx, httpClient, url, accessToken, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func getJSON(ctx context.Context, httpClient *http.Client, url, accessToken string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("GET %s: invalid JSON: %w", url, err)
	}
	return nil
}

func claimString(claims map[string]any, key string) string {
	switch v := claims[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return fmt.Sprintf("%v", v)
	default:
		return ""
	}
}

// checkEmailVerified rejects identities whose provider explicitly says the
// email address was not verified; we key accounts by email, so an unverified
// address must not log into the matching local account.
func checkEmailVerified(claims map[string]any) error {
	switch v := claims["email_verified"].(type) {
	case bool:
		if !v {
			return newError(ErrCodeEmailUnverified, "provider reports email_verified=false")
		}
	case string:
		if strings.EqualFold(v, "false") {
			return newError(ErrCodeEmailUnverified, "provider reports email_verified=false")
		}
	}
	return nil
}

// normalizeUserInfo maps provider claims to UserInfo with the same fallbacks
// as the Python client: username defaults to the local part of the email,
// nickname to the username, avatar_url to the OIDC `picture` claim.
func normalizeUserInfo(claims map[string]any) *UserInfo {
	info := &UserInfo{Email: claimString(claims, "email"), Claims: claims}
	info.Username = claimString(claims, "username")
	if info.Username == "" {
		info.Username, _, _ = strings.Cut(info.Email, "@")
	}
	info.Nickname = claimString(claims, "nickname")
	if info.Nickname == "" {
		info.Nickname = info.Username
	}
	info.AvatarURL = claimString(claims, "avatar_url")
	if info.AvatarURL == "" {
		info.AvatarURL = claimString(claims, "picture")
	}
	return info
}
