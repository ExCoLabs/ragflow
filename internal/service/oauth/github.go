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
	"net/http"
	"ragflow/internal/server/config"
	"strings"

	"golang.org/x/oauth2"
)

const (
	githubAuthorizationURL = "https://github.com/login/oauth/authorize"
	githubTokenURL         = "https://github.com/login/oauth/access_token"
	githubUserInfoURL      = "https://api.github.com/user"
)

// githubClient is OAuth2 against GitHub, which has no OIDC userinfo: the
// profile comes from /user and the email from /user/emails.
type githubClient struct {
	oauth2Client
}

// newGitHubClient fills in GitHub's endpoints; authorization_url, token_url
// and userinfo_url may still be set to point at a GitHub Enterprise server.
func newGitHubClient(cfg config.OAuthChannelConfig, httpClient *http.Client) *githubClient {
	if cfg.AuthorizationURL == "" {
		cfg.AuthorizationURL = githubAuthorizationURL
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = githubTokenURL
	}
	if cfg.UserInfoURL == "" {
		cfg.UserInfoURL = githubUserInfoURL
	}
	cfg.Scope = "user:email"
	return &githubClient{oauth2Client{
		conf:        newOAuth2Config(cfg, oauth2.Endpoint{AuthURL: cfg.AuthorizationURL, TokenURL: cfg.TokenURL}),
		userInfoURL: cfg.UserInfoURL,
		httpClient:  httpClient,
	}}
}

func (c *githubClient) Authenticate(ctx context.Context, code, _, verifier string) (*UserInfo, error) {
	tok, err := exchange(ctx, c.conf, c.httpClient, code, verifier)
	if err != nil {
		return nil, err
	}
	profile, err := fetchJSON(ctx, c.httpClient, c.userInfoURL, tok.AccessToken)
	if err != nil {
		return nil, &Error{Code: ErrCodeUserInfoFailed, Err: err}
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := getJSON(ctx, c.httpClient, c.userInfoURL+"/emails", tok.AccessToken, &emails); err != nil {
		return nil, &Error{Code: ErrCodeUserInfoFailed, Err: err}
	}
	email := ""
	for _, e := range emails {
		if e.Primary {
			if !e.Verified {
				return nil, &Error{Code: ErrCodeEmailUnverified, Err: errors.New("primary GitHub email is not verified")}
			}
			email = e.Email
			break
		}
	}

	info := &UserInfo{Email: email, Username: claimString(profile, "login")}
	if info.Username == "" {
		info.Username, _, _ = strings.Cut(email, "@")
	}
	info.Nickname = claimString(profile, "name")
	if info.Nickname == "" {
		info.Nickname = info.Username
	}
	info.AvatarURL = claimString(profile, "avatar_url")
	return info, nil
}
