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
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"ragflow/internal/common"
	"ragflow/internal/engine/clickhouse"
	"ragflow/internal/engine/kvrocks"
	"ragflow/internal/entity"
	"ragflow/internal/server"
	"ragflow/internal/server/config"
	"ragflow/internal/server/local"
	"ragflow/internal/service"
	"ragflow/internal/service/oauth"
	"ragflow/internal/utility"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	// groupSyncTimeout bounds the group sync the callback runs before it
	// redirects; a slow directory delays the login at most this long.
	groupSyncTimeout = 15 * time.Second

	// oauthStateCookie carries the signed state/nonce/PKCE verifier from the
	// login redirect to the callback.
	oauthStateCookie = "ragflow_oauth_state"
	oauthStateTTL    = 10 * time.Minute
)

// oauthErrorRedirect sends the browser back to the SPA, which shows the
// `error` query parameter on the login page.
func oauthErrorRedirect(c *gin.Context, code string) {
	c.Redirect(http.StatusFound, "/?error="+url.QueryEscape(code))
}

func requestIsHTTPS(c *gin.Context) bool {
	return c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}

func setOAuthStateCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		// Lax: the cookie must survive the top-level redirect back from the
		// identity provider.
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(c),
	})
}

// oauthLogin redirects the browser to the channel's authorization endpoint
// (GET /api/v1/auth/login/:channel).
func (h *UserHandler) oauthLogin(c *gin.Context, channel string) {
	ctx := c.Request.Context()
	cfg := server.GetConfig()
	channelCfg, ok := cfg.GetOAuthChannel(channel)
	if !ok {
		oauthErrorRedirect(c, "invalid_channel")
		return
	}
	client, err := oauth.GetClient(ctx, channel, channelCfg)
	if err != nil {
		common.Error("OAuth login: failed to initialise channel "+channel, err)
		oauthErrorRedirect(c, "provider_unavailable")
		return
	}
	secretKey, err := server.GetSecretKey(ctx, kvrocks.Get())
	if err != nil {
		common.Error("OAuth login: failed to get secret key", err)
		oauthErrorRedirect(c, "login_failed")
		return
	}
	state := oauth.NewLoginState(channel, oauthStateTTL, time.Now())
	cookie, err := state.Encode(secretKey)
	if err != nil {
		common.Error("OAuth login: failed to encode state", err)
		oauthErrorRedirect(c, "login_failed")
		return
	}
	setOAuthStateCookie(c, cookie, int(oauthStateTTL.Seconds()))
	common.Info("OAuth login initiated", zap.String("channel", channel))
	c.Redirect(http.StatusFound, client.AuthCodeURL(state.State, state.Nonce, state.Verifier))
}

// oauthCallback completes the login (GET /api/v1/auth/oauth/:channel/callback):
// it checks the state, exchanges the code, finds or just-in-time registers the
// user by email and redirects to `/?auth=<token>`, which the SPA stores like
// the token of a password login.
func (h *UserHandler) oauthCallback(c *gin.Context, channel string) {
	ctx := c.Request.Context()
	startAt := time.Now()
	operationLog := &common.OperationLog{
		EventTime:    startAt,
		Operation:    "login",
		APIPath:      c.FullPath(),
		HTTPMethod:   c.Request.Method,
		IPAddress:    c.ClientIP(),
		ResourceName: channel,
	}
	fail := func(code string) {
		operationLog.ErrorCode = uint16(common.CodeAuthenticationError)
		operationLog.Message = code
		oauthErrorRedirect(c, code)
	}
	defer func() {
		operationLog.DurationMS = time.Since(startAt).Milliseconds()
		if err := clickhouse.GetDriver().SaveOperationLog(operationLog); err != nil {
			common.Warn("Failed to save OAuth login operation log", zap.Error(err))
		}
	}()

	secretKey, err := server.GetSecretKey(ctx, kvrocks.Get())
	if err != nil {
		common.Error("OAuth callback: failed to get secret key", err)
		fail("login_failed")
		return
	}

	// The state cookie is single use.
	stateCookie, _ := c.Cookie(oauthStateCookie)
	setOAuthStateCookie(c, "", -1)
	state, err := oauth.DecodeLoginState(stateCookie, secretKey, time.Now())
	if err != nil || state.Channel != channel || subtle.ConstantTimeCompare([]byte(state.State), []byte(c.Query("state"))) != 1 {
		common.Warn("OAuth callback: invalid state", zap.String("channel", channel))
		fail("invalid_state")
		return
	}

	code := c.Query("code")
	if code == "" {
		if idpErr := c.Query("error"); idpErr != "" {
			common.Warn("OAuth callback: provider returned an error", zap.String("channel", channel),
				zap.String("error", idpErr), zap.String("error_description", c.Query("error_description")))
		}
		fail("missing_code")
		return
	}

	cfg := server.GetConfig()
	channelCfg, ok := cfg.GetOAuthChannel(channel)
	if !ok {
		fail("invalid_channel")
		return
	}
	if !local.IsAdminAvailable() {
		fail(local.GetAdminStatus().Reason)
		return
	}
	client, err := oauth.GetClient(ctx, channel, channelCfg)
	if err != nil {
		common.Error("OAuth callback: failed to initialise channel "+channel, err)
		fail("provider_unavailable")
		return
	}

	info, err := client.Authenticate(ctx, code, state.Nonce, state.Verifier)
	if err != nil {
		common.Error("OAuth callback: authentication with "+channel+" failed", err)
		var oauthErr *oauth.Error
		if errors.As(err, &oauthErr) {
			fail(oauthErr.Code)
		} else {
			fail("login_failed")
		}
		return
	}
	if info.Email == "" {
		fail("email_missing")
		return
	}
	operationLog.ResourceName = info.Email

	user, created, err := h.userService.LoginOAuthUser(ctx, channel, info, cfg.OAuthAutoRegister())
	switch {
	case errors.Is(err, service.ErrOAuthRegistrationDisabled):
		fail("registration_disabled")
		return
	case errors.Is(err, service.ErrOAuthUserInactive):
		fail("user_inactive")
		return
	case err != nil:
		common.Error("OAuth callback: login failed", err)
		fail("login_failed")
		return
	}
	operationLog.UserID = user.ID
	syncGroups(ctx, h.userService, channel, channelCfg, user, info)

	authToken, err := utility.DumpAccessToken(*user.AccessToken, secretKey)
	if err != nil {
		common.Error("OAuth callback: failed to generate auth token", err)
		fail("login_failed")
		return
	}

	if webhook := cfg.OAuthPostLoginWebhook(); webhook != "" {
		oauth.NotifyPostLogin(webhook, oauth.LoginEvent{
			Email:   user.Email,
			UserID:  user.ID,
			Channel: channel,
			NewUser: created,
		})
	}

	common.Info("OAuth login successful", zap.String("user_id", user.ID), zap.String("channel", channel), zap.Bool("new_user", created))
	setOAuthAuthCookie(c, authToken)
	c.Redirect(http.StatusFound, "/?auth="+url.QueryEscape(authToken))
}

// syncGroups applies the channel's group -> team mapping to the user. It is
// bounded by groupSyncTimeout and never fails the login: errors are logged and
// the user keeps the memberships they had.
func syncGroups(ctx context.Context, userService *service.UserService, channel string, cfg config.OAuthChannelConfig, user *entity.User, info *oauth.UserInfo) {
	syncer := oauth.GetGroupSyncer(channel, cfg)
	if syncer == nil || !syncer.Applies(user.Email) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), groupSyncTimeout)
	defer cancel()
	plan, err := syncer.Plan(ctx, info)
	if err == nil {
		err = userService.SyncTeamMemberships(ctx, user, plan)
	}
	if err != nil {
		common.Warn("SSO group sync failed; login continues", zap.String("user_id", user.ID),
			zap.String("channel", channel), zap.Error(err))
	}
}
