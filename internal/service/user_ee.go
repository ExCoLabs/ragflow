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
	"ragflow/internal/common"
	"ragflow/internal/server"
	"unicode"
)

type LoginChannel struct {
	Channel     string `json:"channel"`
	DisplayName string `json:"display_name"`
	Icon        string `json:"icon"`
}

// GetLoginChannels gets all supported authentication channels
func (s *UserService) GetLoginChannels() ([]*LoginChannel, common.ErrorCode, error) {
	channels := make([]*LoginChannel, 0)
	cfg := server.GetConfig()
	if cfg == nil {
		return channels, common.CodeSuccess, nil
	}
	for _, name := range cfg.GetOAuthChannelNames() {
		ch, _ := cfg.GetOAuthChannel(name)
		channel := &LoginChannel{Channel: name, DisplayName: ch.DisplayName, Icon: ch.Icon}
		if channel.DisplayName == "" {
			channel.DisplayName = titleCase(name)
		}
		if channel.Icon == "" {
			channel.Icon = "sso"
		}
		channels = append(channels, channel)
	}
	return channels, common.CodeSuccess, nil
}

// titleCase mirrors Python's str.title(), the default display name of a
// channel: upper-case each letter that follows a non-letter.
func titleCase(s string) string {
	out := []rune(s)
	prevLetter := false
	for i, r := range out {
		if unicode.IsLetter(r) {
			if prevLetter {
				out[i] = unicode.ToLower(r)
			} else {
				out[i] = unicode.ToUpper(r)
			}
			prevLetter = true
		} else {
			prevLetter = false
		}
	}
	return string(out)
}
