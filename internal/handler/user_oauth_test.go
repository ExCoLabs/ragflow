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

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/server"
	"ragflow/internal/server/local"
	"ragflow/internal/service"
	"ragflow/internal/service/oauth"
	"ragflow/internal/service/oauth/oauthtest"
)

const oauthTestSecretKey = "oauth-handler-test-secret-key"

type oauthTestEnv struct {
	provider *oauthtest.Provider
	router   *gin.Engine
	db       *gorm.DB
}

// setupOAuthTest wires a fake OIDC provider into the `oauth:` config as the
// channel "sso", an in-memory database and the SSO routes. extraConf is
// appended to service_conf.yaml.
func setupOAuthTest(t *testing.T, autoRegister, webhook string, extraConf ...string) *oauthTestEnv {
	t.Helper()
	p := oauthtest.New(t)

	confPath := filepath.Join(t.TempDir(), "service_conf.yaml")
	conf := fmt.Sprintf(`oauth:
  sso:
    display_name: "Company SSO"
    type: oidc
    issuer: %q
    client_id: %q
    client_secret: %q
    scope: "openid profile email"
    redirect_uri: "http://ragflow.test/api/v1/auth/oauth/sso/callback"
`, p.Issuer(), oauthtest.ClientID, oauthtest.ClientSecret) + strings.Join(extraConf, "")
	if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RAGFLOW_SECRET_KEY", oauthTestSecretKey)
	t.Setenv("OAUTH_AUTO_REGISTER", autoRegister)
	t.Setenv("OAUTH_POST_LOGIN_WEBHOOK", webhook)
	if err := server.Init(confPath); err != nil {
		t.Fatalf("server.Init: %v", err)
	}

	prevAdmin := local.GetAdminStatus()
	local.SetAdminStatus(0, "")
	t.Cleanup(func() { local.SetAdminStatus(prevAdmin.Status, prevAdmin.Reason) })

	db, err := gorm.Open(sqlite.Open("file:"+url.QueryEscape(t.Name())+"?mode=memory&cache=shared"), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&entity.User{}, &entity.Tenant{}, &entity.UserTenant{}, &entity.File{},
		&entity.TenantModelProvider{}, &entity.TenantModelInstance{}, &entity.TenantModel{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	origDB := dao.DB
	dao.DB = db
	t.Cleanup(func() { dao.DB = origDB })

	gin.SetMode(gin.TestMode)
	h := NewUserHandler(service.NewUserService())
	r := gin.New()
	api := r.Group("/api/v1")
	api.GET("/auth/login/channels", h.GetLoginChannels)
	api.GET("/auth/login/:channel", h.OAuthLogin)
	api.GET("/auth/oauth/:channel/callback", h.OAuthChannelCallback)
	return &oauthTestEnv{provider: p, router: r, db: db}
}

func (e *oauthTestEnv) get(path string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	return w
}

// login runs the login redirect and returns the state sent to the provider
// and the state cookie.
func (e *oauthTestEnv) login(t *testing.T) (string, []*http.Cookie) {
	t.Helper()
	w := e.get("/api/v1/auth/login/sso", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("login status = %d, body %s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(loc.String(), e.provider.Issuer()+"/authorize?") {
		t.Fatalf("login redirect = %q", w.Header().Get("Location"))
	}
	e.provider.Authorize(loc.Query())
	return loc.Query().Get("state"), w.Result().Cookies()
}

func (e *oauthTestEnv) callback(state string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	q := url.Values{"state": {state}, "code": {e.provider.Code}}
	return e.get("/api/v1/auth/oauth/sso/callback?"+q.Encode(), cookies)
}

// expectRedirect asserts the callback redirected to "/?<key>=..." and
// returns the value.
func expectRedirect(t *testing.T, w *httptest.ResponseRecorder, key string) string {
	t.Helper()
	if w.Code != http.StatusFound {
		t.Fatalf("callback status = %d, body %s", w.Code, w.Body.String())
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || loc.Path != "/" || !loc.Query().Has(key) {
		t.Fatalf("callback redirect = %q, want /?%s=...", w.Header().Get("Location"), key)
	}
	return loc.Query().Get(key)
}

func userByToken(t *testing.T, db *gorm.DB, auth string) *entity.User {
	t.Helper()
	tokenCtx, err := common.ExtractAccessTokenContext(auth, oauthTestSecretKey)
	if err != nil {
		t.Fatalf("auth token does not verify: %v", err)
	}
	var user entity.User
	if err := db.Where("access_token = ?", tokenCtx.AccessToken).First(&user).Error; err != nil {
		t.Fatalf("no user holds the issued access token: %v", err)
	}
	return &user
}

func countRows(t *testing.T, db *gorm.DB, model any, where string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Model(model).Where(where, args...).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOAuthLoginChannels(t *testing.T) {
	env := setupOAuthTest(t, "true", "")
	w := env.get("/api/v1/auth/login/channels", nil)
	var resp struct {
		Code int                    `json:"code"`
		Data []service.LoginChannel `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	want := []service.LoginChannel{{Channel: "sso", DisplayName: "Company SSO", Icon: "sso"}}
	if resp.Code != 0 || len(resp.Data) != 1 || resp.Data[0] != want[0] {
		t.Fatalf("channels = %+v", resp)
	}
}

func TestOAuthCallbackExistingUser(t *testing.T) {
	env := setupOAuthTest(t, "false", "")
	oldToken := common.GenerateToken()
	existing := &entity.User{ID: common.GenerateToken(), Email: env.provider.Email, Nickname: "alice", AccessToken: &oldToken, IsActive: "1"}
	if err := env.db.Create(existing).Error; err != nil {
		t.Fatal(err)
	}

	state, cookies := env.login(t)
	w := env.callback(state, cookies)
	user := userByToken(t, env.db, expectRedirect(t, w, "auth"))
	if user.ID != existing.ID || *user.AccessToken == oldToken {
		t.Fatalf("logged in user %s with token %s; want %s with a fresh token", user.ID, *user.AccessToken, existing.ID)
	}
	if n := countRows(t, env.db, &entity.User{}, "1 = 1"); n != 1 {
		t.Errorf("users = %d, want 1", n)
	}
	// The browser is told to drop the state cookie.
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == oauthStateCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("callback did not clear the state cookie")
	}
}

func TestOAuthCallbackInactiveUser(t *testing.T) {
	env := setupOAuthTest(t, "true", "")
	token := common.GenerateToken()
	if err := env.db.Create(&entity.User{ID: common.GenerateToken(), Email: env.provider.Email, Nickname: "alice", AccessToken: &token, IsActive: "0"}).Error; err != nil {
		t.Fatal(err)
	}
	state, cookies := env.login(t)
	if got := expectRedirect(t, env.callback(state, cookies), "error"); got != "user_inactive" {
		t.Fatalf("error = %q, want user_inactive", got)
	}
}

func TestOAuthCallbackRegistersNewUser(t *testing.T) {
	events := make(chan oauth.LoginEvent, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev oauth.LoginEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		events <- ev
	}))
	defer hook.Close()
	env := setupOAuthTest(t, "true", hook.URL)
	env.provider.Email = "new.user@example.com"

	state, cookies := env.login(t)
	user := userByToken(t, env.db, expectRedirect(t, env.callback(state, cookies), "auth"))
	if user.Email != "new.user@example.com" || user.Nickname != "new.user" || user.LoginChannel == nil || *user.LoginChannel != "sso" {
		t.Fatalf("registered user = %+v", user)
	}
	if user.Password != nil {
		t.Error("SSO user must not get a password")
	}
	if countRows(t, env.db, &entity.Tenant{}, "id = ?", user.ID) != 1 ||
		countRows(t, env.db, &entity.UserTenant{}, "user_id = ? AND tenant_id = ? AND role = ?", user.ID, user.ID, "owner") != 1 ||
		countRows(t, env.db, &entity.File{}, "tenant_id = ? AND name = ? AND type = ?", user.ID, "/", "folder") != 1 {
		t.Error("tenant, owner relation or root folder missing for the new user")
	}

	select {
	case ev := <-events:
		want := oauth.LoginEvent{Email: user.Email, UserID: user.ID, Channel: "sso", NewUser: true}
		if ev != want {
			t.Errorf("webhook event = %+v, want %+v", ev, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-login webhook was not called")
	}
}

func TestOAuthCallbackRegistrationDisabled(t *testing.T) {
	env := setupOAuthTest(t, "false", "")
	state, cookies := env.login(t)
	if got := expectRedirect(t, env.callback(state, cookies), "error"); got != "registration_disabled" {
		t.Fatalf("error = %q, want registration_disabled", got)
	}
	if n := countRows(t, env.db, &entity.User{}, "1 = 1"); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

func TestOAuthCallbackBadState(t *testing.T) {
	env := setupOAuthTest(t, "true", "")
	state, cookies := env.login(t)
	if got := expectRedirect(t, env.callback(state+"x", cookies), "error"); got != "invalid_state" {
		t.Errorf("tampered state: error = %q", got)
	}
	state, _ = env.login(t)
	if got := expectRedirect(t, env.callback(state, nil), "error"); got != "invalid_state" {
		t.Errorf("missing cookie: error = %q", got)
	}
	if n := countRows(t, env.db, &entity.User{}, "1 = 1"); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

func TestOAuthCallbackBadIDTokenAudience(t *testing.T) {
	env := setupOAuthTest(t, "true", "")
	env.provider.Audience = "another-client"
	state, cookies := env.login(t)
	if got := expectRedirect(t, env.callback(state, cookies), "error"); got != oauth.ErrCodeInvalidIDToken {
		t.Fatalf("error = %q, want %s", got, oauth.ErrCodeInvalidIDToken)
	}
	if n := countRows(t, env.db, &entity.User{}, "1 = 1"); n != 0 {
		t.Errorf("users = %d, want 0", n)
	}
}

func TestOAuthCallbackGroupSyncAndDefaultModels(t *testing.T) {
	graph := oauthtest.NewGraph(t)
	const engGroup = "33333333-3333-3333-3333-333333333333"
	graph.Groups["Engineering"] = engGroup
	graph.Members["new.user@example.com"] = []string{engGroup}
	t.Setenv("ENTRA_GROUP_SYNC", "Engineering:eng-team@example.com")
	t.Setenv("SSO_DOMAINS", "example.com")

	env := setupOAuthTest(t, "true", "", fmt.Sprintf(`    group_sync:
      enabled: true
      require_mapped_group: true
      graph_url: %q
      graph_token_url: %q
      default_teams: ["everyone@example.com"]
user_default_llm:
  factory: "OpenAI-API-Compatible"
  api_key: "llm-key"
  base_url: "https://llm.example.com/v1"
  instance_name: "main"
  default_models:
    chat_model:
      name: "chat-1"
      max_tokens: 32768
      is_tools: true
    embedding_model: "embed-1"
`, graph.URL(), graph.TokenURL()))
	env.provider.Email = "new.user@example.com"

	teams := map[string]string{}
	for _, owner := range []string{"eng-team@example.com", "everyone@example.com"} {
		status, token := "1", common.GenerateToken()
		team := &entity.User{ID: common.GenerateToken(), Email: owner, Nickname: owner, AccessToken: &token, IsActive: "1", Status: &status}
		if err := env.db.Create(team).Error; err != nil {
			t.Fatal(err)
		}
		if err := env.db.Create(&entity.UserTenant{ID: common.GenerateToken(), UserID: team.ID, TenantID: team.ID, Role: "owner", InvitedBy: team.ID, Status: &status}).Error; err != nil {
			t.Fatal(err)
		}
		teams[owner] = team.ID
	}

	state, cookies := env.login(t)
	user := userByToken(t, env.db, expectRedirect(t, env.callback(state, cookies), "auth"))
	for owner, tenantID := range teams {
		if countRows(t, env.db, &entity.UserTenant{}, "user_id = ? AND tenant_id = ? AND role = ? AND status = ?", user.ID, tenantID, "normal", "1") != 1 {
			t.Errorf("not a member of team %s", owner)
		}
	}

	var tenant entity.Tenant
	if err := env.db.First(&tenant, "id = ?", user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if tenant.LLMID != "chat-1@main@OpenAI-API-Compatible" || tenant.EmbdID != "embed-1@main@OpenAI-API-Compatible" {
		t.Errorf("tenant defaults = %q / %q", tenant.LLMID, tenant.EmbdID)
	}
}

func TestOAuthRequiredGroupRejectsInvalidConfiguration(t *testing.T) {
	t.Setenv("ENTRA_GROUP_SYNC", "")
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service_conf.yaml")
			data := fmt.Sprintf("oauth:\n  sso:\n    type: oidc\n    group_sync:\n      enabled: %t\n      require_mapped_group: true\n", enabled)
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := server.Init(path); err == nil || !strings.Contains(err.Error(), "require_mapped_group") {
				t.Fatalf("configuration error = %v", err)
			}
		})
	}
}

func TestOAuthCallbackRequiresMappedGroup(t *testing.T) {
	const group = "44444444-4444-4444-4444-444444444444"
	for _, tc := range []struct {
		name      string
		email     string
		member    bool
		existing  bool
		graphDown bool
	}{
		{name: "nonmember", email: "new.user@example.com"},
		{name: "existing nonmember", email: "new.user@example.com", existing: true},
		{name: "outside domain", email: "new.user@external.com", member: true},
		{name: "directory unavailable", email: "new.user@example.com", graphDown: true},
		{name: "member", email: "new.user@example.com", member: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := oauthtest.NewGraph(t)
			graph.Members[tc.email] = []string{}
			if tc.member {
				graph.Members[tc.email] = []string{group}
			}
			t.Setenv("ENTRA_GROUP_SYNC", "")
			t.Setenv("SSO_DOMAINS", "example.com")
			graphURL := graph.URL()
			if tc.graphDown {
				graphURL = "http://127.0.0.1:1/v1.0"
			}
			env := setupOAuthTest(t, "true", "", fmt.Sprintf(`    group_sync:
      enabled: true
      require_mapped_group: true
      graph_url: %q
      graph_token_url: %q
      default_teams: ["everyone@example.com"]
      mapping:
        %q: ["eng-team@example.com"]
`, graphURL, graph.TokenURL(), group))
			env.provider.Email = tc.email
			if tc.existing {
				token := "original-token"
				if err := env.db.Create(&entity.User{ID: "existing", Email: tc.email, IsActive: "1", AccessToken: &token}).Error; err != nil {
					t.Fatal(err)
				}
			}
			allowed := tc.member && !tc.graphDown && strings.HasSuffix(tc.email, "@example.com")
			for attempt := 0; attempt < 2; attempt++ {
				state, cookies := env.login(t)
				response := env.callback(state, cookies)
				if allowed {
					userByToken(t, env.db, expectRedirect(t, response, "auth"))
					continue
				}
				want := "access_denied"
				if tc.graphDown {
					want = "group_lookup_failed"
				}
				if got := expectRedirect(t, response, "error"); got != want {
					t.Fatalf("error = %q, want %q", got, want)
				}
				for _, cookie := range response.Result().Cookies() {
					if cookie.Name == "ragflow_auth" && cookie.Value != "" {
						t.Fatal("denied login issued an auth cookie")
					}
				}
			}
			if allowed {
				if got := graph.CallCount("check"); got != 2 {
					t.Fatalf("directory checks = %d, want one per login", got)
				}
				graph.Members[tc.email] = []string{}
				state, cookies := env.login(t)
				if got := expectRedirect(t, env.callback(state, cookies), "error"); got != "access_denied" {
					t.Fatalf("revoked member error = %q", got)
				}
				return
			}
			wantUsers := int64(0)
			if tc.existing {
				wantUsers = 1
				var user entity.User
				if err := env.db.First(&user, "id = ?", "existing").Error; err != nil || user.AccessToken == nil || *user.AccessToken != "original-token" {
					t.Fatal("denied login modified existing user token")
				}
			}
			if got := countRows(t, env.db, &entity.User{}, "1 = 1"); got != wantUsers {
				t.Fatalf("users = %d, want %d", got, wantUsers)
			}
			if countRows(t, env.db, &entity.UserTenant{}, "1 = 1") != 0 {
				t.Fatal("denied login created team memberships")
			}
		})
	}
}

func TestOAuthRequiredGroupSyncFailureBlocksTokenIssuance(t *testing.T) {
	graph := oauthtest.NewGraph(t)
	const group = "55555555-5555-5555-5555-555555555555"
	graph.Members["existing@example.com"] = []string{group}
	t.Setenv("ENTRA_GROUP_SYNC", "")
	t.Setenv("SSO_DOMAINS", "example.com")
	env := setupOAuthTest(t, "true", "", fmt.Sprintf(`    group_sync:
      enabled: true
      require_mapped_group: true
      graph_url: %q
      graph_token_url: %q
      mapping:
        %q: ["eng-team@example.com"]
`, graph.URL(), graph.TokenURL(), group))
	env.provider.Email = "existing@example.com"
	token := "original-token"
	if err := env.db.Create(&entity.User{ID: "existing", Email: env.provider.Email, IsActive: "1", AccessToken: &token}).Error; err != nil {
		t.Fatal(err)
	}
	if err := env.db.Callback().Query().Before("gorm:query").Register("deny_group_sync", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_tenant" {
			tx.AddError(fmt.Errorf("team membership database unavailable"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	state, cookies := env.login(t)
	response := env.callback(state, cookies)
	if got := expectRedirect(t, response, "error"); got != "group_sync_failed" {
		t.Fatalf("error = %q", got)
	}
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == oauthAuthCookie && cookie.Value != "" {
			t.Fatal("failed membership sync issued an auth cookie")
		}
	}
}

func TestOAuthCallbackGroupSyncFailureDoesNotBlockLogin(t *testing.T) {
	env := setupOAuthTest(t, "true", "", `    group_sync:
      enabled: true
      graph_url: "http://127.0.0.1:1/v1.0"
      graph_token_url: "http://127.0.0.1:1/token"
      mapping:
        "44444444-4444-4444-4444-444444444444": ["eng-team@example.com"]
`)
	state, cookies := env.login(t)
	userByToken(t, env.db, expectRedirect(t, env.callback(state, cookies), "auth"))
}
