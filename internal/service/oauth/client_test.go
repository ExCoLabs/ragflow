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

package oauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"ragflow/internal/server/config"
	"ragflow/internal/service/oauth"
	"ragflow/internal/service/oauth/oauthtest"
)

func oidcChannel(p *oauthtest.Provider) config.OAuthChannelConfig {
	return config.OAuthChannelConfig{
		Type:         "oidc",
		Issuer:       p.Issuer(),
		ClientID:     oauthtest.ClientID,
		ClientSecret: oauthtest.ClientSecret,
		RedirectURI:  "http://ragflow.test/api/v1/auth/oauth/sso/callback",
	}
}

// startLogin builds the authorization URL and lets the fake provider record
// its nonce and PKCE challenge.
func startLogin(t *testing.T, p *oauthtest.Provider, cli oauth.Client, state *oauth.LoginState) url.Values {
	t.Helper()
	u, err := url.Parse(cli.AuthCodeURL(state.State, state.Nonce, state.Verifier))
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	p.Authorize(q)
	return q
}

func TestOIDCClientAuthenticate(t *testing.T) {
	p := oauthtest.New(t)
	p.UserInfo = map[string]any{"sub": p.Subject, "email": p.Email, "nickname": "Alice", "picture": "https://example.com/a.png"}
	cli, err := oauth.GetClient(context.Background(), "sso", oidcChannel(p))
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	state := oauth.NewLoginState("sso", time.Minute, time.Now())
	q := startLogin(t, p, cli, state)
	if got := q.Get("scope"); got != "openid profile email" {
		t.Errorf("default scope = %q", got)
	}
	if q.Get("state") != state.State || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != oauthtest.ClientID {
		t.Errorf("unexpected authorization query: %v", q)
	}

	info, err := cli.Authenticate(context.Background(), p.Code, state.Nonce, state.Verifier)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	want := &oauth.UserInfo{Email: "alice@example.com", Username: "alice", Nickname: "Alice", AvatarURL: "https://example.com/a.png"}
	if info.Email != want.Email || info.Username != want.Username || info.Nickname != want.Nickname || info.AvatarURL != want.AvatarURL {
		t.Errorf("user info = %+v, want %+v", *info, *want)
	}
}

func TestOIDCClientRejectsBadTokens(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(p *oauthtest.Provider, state *oauth.LoginState)
		wantCode string
	}{
		{"wrong audience", func(p *oauthtest.Provider, _ *oauth.LoginState) { p.Audience = "someone-else" }, oauth.ErrCodeInvalidIDToken},
		{"wrong nonce", func(p *oauthtest.Provider, _ *oauth.LoginState) { p.Nonce = "replayed" }, oauth.ErrCodeInvalidIDToken},
		{"wrong code verifier", func(_ *oauthtest.Provider, s *oauth.LoginState) { s.Verifier = "x" + s.Verifier }, oauth.ErrCodeTokenFailed},
		{"userinfo for another subject", func(p *oauthtest.Provider, _ *oauth.LoginState) {
			p.UserInfo = map[string]any{"sub": "other", "email": "mallory@example.com"}
		}, oauth.ErrCodeUserInfoFailed},
		{"unverified email", func(p *oauthtest.Provider, _ *oauth.LoginState) {
			p.UserInfo = map[string]any{"sub": p.Subject, "email": p.Email, "email_verified": false}
		}, oauth.ErrCodeEmailUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := oauthtest.New(t)
			cli, err := oauth.GetClient(context.Background(), "sso", oidcChannel(p))
			if err != nil {
				t.Fatalf("GetClient: %v", err)
			}
			state := oauth.NewLoginState("sso", time.Minute, time.Now())
			startLogin(t, p, cli, state)
			tc.mutate(p, state)
			_, err = cli.Authenticate(context.Background(), p.Code, state.Nonce, state.Verifier)
			var oauthErr *oauth.Error
			if !errors.As(err, &oauthErr) || oauthErr.Code != tc.wantCode {
				t.Fatalf("err = %v, want code %s", err, tc.wantCode)
			}
		})
	}
}

func TestOIDCClientIssuerTrailingSlash(t *testing.T) {
	p := oauthtest.New(t)
	cfg := oidcChannel(p)
	cfg.Issuer += "/"
	if _, err := oauth.GetClient(context.Background(), "sso", cfg); err != nil {
		t.Fatalf("GetClient with trailing slash: %v", err)
	}
}

func TestOAuth2ClientAuthenticate(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("code") != "c1" || r.PostForm.Get("client_secret") != "s1" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"at","token_type":"bearer"}`)
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"email": "bob@example.com", "username": "bobby"})
	})
	cli, err := oauth.GetClient(context.Background(), "plain", config.OAuthChannelConfig{
		Type: "oauth2", ClientID: "c", ClientSecret: "s1",
		AuthorizationURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", UserInfoURL: srv.URL + "/userinfo",
		RedirectURI: "http://ragflow.test/cb",
	})
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if u := cli.AuthCodeURL("st", "n", "v"); !strings.HasPrefix(u, srv.URL+"/authorize?") || strings.Contains(u, "scope=") {
		t.Errorf("auth url = %s", u)
	}
	info, err := cli.Authenticate(context.Background(), "c1", "", "v")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if info.Email != "bob@example.com" || info.Username != "bobby" || info.Nickname != "bobby" {
		t.Errorf("user info = %+v", *info)
	}
}

func TestLoginStateRoundTrip(t *testing.T) {
	now := time.Now()
	s := oauth.NewLoginState("sso", time.Minute, now)
	v, err := s.Encode("secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := oauth.DecodeLoginState(v, "secret", now)
	if err != nil || *got != *s {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if _, err := oauth.DecodeLoginState(v, "other-secret", now); err == nil {
		t.Error("accepted a state signed with another key")
	}
	if _, err := oauth.DecodeLoginState(v, "secret", now.Add(2*time.Minute)); err == nil {
		t.Error("accepted an expired state")
	}
	payload, sig, _ := strings.Cut(v, ".")
	if _, err := oauth.DecodeLoginState(payload+"x."+sig, "secret", now); err == nil {
		t.Error("accepted a tampered state")
	}
}

func TestNotifyPostLogin(t *testing.T) {
	got := make(chan oauth.LoginEvent, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev oauth.LoginEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		got <- ev
	}))
	defer srv.Close()
	want := oauth.LoginEvent{Email: "a@example.com", UserID: "u1", Channel: "sso", NewUser: true}
	<-oauth.NotifyPostLogin(srv.URL, want)
	select {
	case ev := <-got:
		if ev != want {
			t.Errorf("event = %+v, want %+v", ev, want)
		}
	default:
		t.Fatal("webhook not called")
	}

	// An unreachable webhook only logs.
	<-oauth.NotifyPostLogin("http://127.0.0.1:1/hook", want)
}
