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

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/service/oauth"
	"ragflow/internal/utility"

	"go.uber.org/zap"
)

var (
	// ErrOAuthRegistrationDisabled is returned when an unknown SSO user logs
	// in while OAUTH_AUTO_REGISTER is off.
	ErrOAuthRegistrationDisabled = errors.New("registration_disabled")
	// ErrOAuthUserInactive is returned for a disabled account.
	ErrOAuthUserInactive = errors.New("user_inactive")
)

// LoginOAuthUser logs in the local user matching an SSO identity by email,
// creating it (with tenant, root folder and default models, like sign-up)
// when it does not exist and autoRegister allows it. On success the user
// carries a fresh access token; created reports a just-in-time registration.
func (s *UserService) LoginOAuthUser(ctx context.Context, channel string, info *oauth.UserInfo, autoRegister bool) (user *entity.User, created bool, err error) {
	user, err = s.userDAO.GetByEmail(ctx, dao.DB, info.Email)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, fmt.Errorf("failed to look up user: %w", err)
	}

	if user == nil {
		if !autoRegister {
			common.Warn("OAuth/OIDC JIT registration blocked",
				zap.String("email", info.Email), zap.String("channel", channel))
			return nil, false, ErrOAuthRegistrationDisabled
		}
		user = newOAuthUser(ctx, channel, info)
		if err = s.createUserWithTenant(ctx, user); err != nil {
			return nil, false, err
		}
		common.Info("OAuth/OIDC user registered", zap.String("user_id", user.ID), zap.String("channel", channel))
		return user, true, nil
	}

	if user.IsActive == "0" {
		return nil, false, ErrOAuthUserInactive
	}
	token := utility.GenerateToken()
	user.AccessToken = &token
	now := time.Now().Truncate(time.Second)
	user.LastLoginTime = &now
	if err = s.userDAO.Update(ctx, dao.DB, user); err != nil {
		return nil, false, fmt.Errorf("failed to update user: %w", err)
	}
	return user, false, nil
}

func newOAuthUser(ctx context.Context, channel string, info *oauth.UserInfo) *entity.User {
	nickname := strings.TrimSpace(info.Nickname)
	if nickname == "" {
		nickname, _, _ = strings.Cut(info.Email, "@")
	}
	accessToken := utility.GenerateToken()
	status := "1"
	isSuperuser := false
	language := defaultUserLanguage()
	colorSchema := "Bright"
	timezone := "UTC+8\tAsia/Shanghai"
	now := time.Now().Truncate(time.Second)
	user := &entity.User{
		ID:              utility.GenerateToken(),
		AccessToken:     &accessToken,
		Email:           info.Email,
		Nickname:        nickname,
		Status:          &status,
		Language:        &language,
		ColorSchema:     &colorSchema,
		Timezone:        &timezone,
		IsActive:        "1",
		IsAuthenticated: "1",
		IsAnonymous:     "0",
		LastLoginTime:   &now,
		LoginChannel:    &channel,
		IsSuperuser:     &isSuperuser,
	}
	if avatar := oauth.DownloadAvatar(ctx, info.AvatarURL); avatar != "" {
		user.Avatar = &avatar
	}
	return user
}
