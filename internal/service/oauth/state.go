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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// LoginState is what the login redirect remembers for the callback. The
// Go server has no server-side session, so it travels in a short-lived
// HMAC-signed cookie (the Python server kept the state in its session).
type LoginState struct {
	Channel  string `json:"c"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Expires  int64  `json:"e"`
}

// NewLoginState returns fresh random state, nonce and PKCE verifier.
func NewLoginState(channel string, ttl time.Duration, now time.Time) *LoginState {
	return &LoginState{
		Channel:  channel,
		State:    rand.Text(),
		Nonce:    rand.Text(),
		Verifier: oauth2.GenerateVerifier(),
		Expires:  now.Add(ttl).Unix(),
	}
}

func stateMAC(secret string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte("ragflow-oauth-state:"+secret))
	mac.Write(payload)
	return mac.Sum(nil)
}

// Encode serializes and signs the state for the cookie.
func (s *LoginState) Encode(secret string) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(stateMAC(secret, payload)), nil
}

var errBadState = errors.New("invalid or expired oauth state")

// DecodeLoginState verifies the signature and expiry of a cookie value.
func DecodeLoginState(value, secret string, now time.Time) (*LoginState, error) {
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return nil, errBadState
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(payloadB64)
	if err != nil {
		return nil, errBadState
	}
	sig, err := enc.DecodeString(sigB64)
	if err != nil || !hmac.Equal(sig, stateMAC(secret, payload)) {
		return nil, errBadState
	}
	var s LoginState
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil, errBadState
	}
	if now.Unix() > s.Expires {
		return nil, errBadState
	}
	return &s, nil
}
