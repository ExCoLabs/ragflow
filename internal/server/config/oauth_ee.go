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

package config

import (
	"fmt"
	"ragflow/internal/common"
	"sort"
	"strings"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// OAuthChannelConfig is one entry of the `oauth:` section of
// service_conf.yaml. The keys match the ones the Python server read.
type OAuthChannelConfig struct {
	// Type is "oauth2", "oidc" or "github". When empty it is "oidc" if
	// Issuer is set and "oauth2" otherwise.
	Type             string `mapstructure:"type"`
	DisplayName      string `mapstructure:"display_name"`
	Icon             string `mapstructure:"icon"`
	ClientID         string `mapstructure:"client_id"`
	ClientSecret     string `mapstructure:"client_secret"`
	AuthorizationURL string `mapstructure:"authorization_url"`
	TokenURL         string `mapstructure:"token_url"`
	UserInfoURL      string `mapstructure:"userinfo_url"`
	RedirectURI      string `mapstructure:"redirect_uri"`
	Scope            string `mapstructure:"scope"`
	Issuer           string `mapstructure:"issuer"`
}

// OAuthConfig holds the configured SSO login channels.
type OAuthConfig struct {
	Channels map[string]OAuthChannelConfig
	// AutoRegister allows just-in-time creation of users that log in through
	// a channel for the first time (OAUTH_AUTO_REGISTER, default true).
	// Independent of the local sign-up switch.
	AutoRegister bool
	// PostLoginWebhook, when set, receives a JSON POST after every
	// successful SSO login (OAUTH_POST_LOGIN_WEBHOOK).
	PostLoginWebhook string
}

func (c *Config) ParseOAuthConfig(v *viper.Viper) error {
	c.oAuth = OAuthConfig{Channels: map[string]OAuthChannelConfig{}, AutoRegister: true}

	if envVal := strings.ToLower(strings.TrimSpace(common.GetEnv(common.EnvOAuthAutoRegister))); envVal != "" {
		c.oAuth.AutoRegister = envVal == "true" || envVal == "1" || envVal == "yes" || envVal == "on"
	}
	c.oAuth.PostLoginWebhook = strings.TrimSpace(common.GetEnv(common.EnvOAuthPostLoginWebhook))

	if !v.IsSet("oauth") {
		return nil
	}
	channels := map[string]OAuthChannelConfig{}
	if err := v.UnmarshalKey("oauth", &channels); err != nil {
		return fmt.Errorf("invalid oauth section: %w", err)
	}
	for name, ch := range channels {
		ch.Type = strings.ToLower(strings.TrimSpace(ch.Type))
		if ch.Type == "" {
			if ch.Issuer != "" {
				ch.Type = "oidc"
			} else {
				ch.Type = "oauth2"
			}
		}
		switch ch.Type {
		case "oauth2", "oidc", "github":
		default:
			common.Warn("Skipping OAuth channel with unsupported type",
				zap.String("channel", name), zap.String("type", ch.Type))
			continue
		}
		c.oAuth.Channels[name] = ch
	}
	return nil
}

// GetOAuthChannel returns the configuration of a login channel.
func (c *Config) GetOAuthChannel(name string) (OAuthChannelConfig, bool) {
	ch, ok := c.oAuth.Channels[name]
	return ch, ok
}

// GetOAuthChannelNames returns the configured login channels in a stable
// (alphabetical) order.
func (c *Config) GetOAuthChannelNames() []string {
	names := make([]string, 0, len(c.oAuth.Channels))
	for name := range c.oAuth.Channels {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// OAuthAutoRegister reports whether first-time SSO users are created.
func (c *Config) OAuthAutoRegister() bool {
	return c.oAuth.AutoRegister
}

// OAuthPostLoginWebhook returns the post-login webhook URL, if any.
func (c *Config) OAuthPostLoginWebhook() string {
	return c.oAuth.PostLoginWebhook
}
