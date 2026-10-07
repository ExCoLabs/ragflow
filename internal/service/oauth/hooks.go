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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"ragflow/internal/common"
	"strings"
	"time"

	"go.uber.org/zap"
)

const (
	webhookTimeout = 5 * time.Second
	avatarTimeout  = 15 * time.Second
	maxAvatarBytes = 2 << 20
)

// LoginEvent is the body POSTed to OAUTH_POST_LOGIN_WEBHOOK.
type LoginEvent struct {
	Email   string `json:"email"`
	UserID  string `json:"user_id"`
	Channel string `json:"channel"`
	NewUser bool   `json:"new_user"`
}

var webhookClient = &http.Client{Timeout: webhookTimeout}

// NotifyPostLogin POSTs the event to url in the background. It never blocks
// the login and failures are only logged. The returned channel is closed once
// the attempt finished (used by tests).
func NotifyPostLogin(url string, event LoginEvent) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		body, err := json.Marshal(event)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			common.Warn("OAuth post-login webhook: invalid URL", zap.Error(err))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := webhookClient.Do(req)
		if err != nil {
			common.Warn("OAuth post-login webhook failed", zap.String("user_id", event.UserID), zap.Error(err))
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			common.Warn("OAuth post-login webhook returned an error status",
				zap.String("user_id", event.UserID), zap.Int("status", resp.StatusCode))
		}
	}()
	return done
}

// DownloadAvatar fetches a profile picture and returns it as a data URI, or
// "" on any failure. The URL comes from the identity provider, so it goes
// through the SSRF-guarded client.
func DownloadAvatar(ctx context.Context, url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, avatarTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := common.GetSSRFHTTPClient().Do(req)
	if err != nil {
		common.Warn("Failed to download OAuth avatar", zap.Error(err))
		return ""
	}
	defer resp.Body.Close()
	contentType := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(contentType, "image/") {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAvatarBytes+1))
	if err != nil || len(data) > maxAvatarBytes {
		return ""
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	return fmt.Sprintf("data:%s;base64,%s", strings.TrimSpace(mediaType), base64.StdEncoding.EncodeToString(data))
}
