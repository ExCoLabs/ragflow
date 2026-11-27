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

package file

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// AttachmentRefTTL bounds how long an agent run's tools can fetch an upload.
// An Agent run is capped at 10 minutes; the margin covers retries.
const AttachmentRefTTL = time.Hour

// AttachmentRef identifies one agent upload for a tool that runs as a
// different user than the uploader. Uploads live in the uploader's
// "<user>-downloads" bucket, which /agents/attachments/:id/download only
// serves to the uploader, so tools get this signed reference instead.
type AttachmentRef struct {
	Owner    string `json:"u"`
	ID       string `json:"i"`
	Name     string `json:"n"`
	MimeType string `json:"m"`
	Expires  int64  `json:"e"`
}

func attachmentRefMAC(secret string, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte("ragflow-attachment-ref:"+secret))
	mac.Write(payload)
	return mac.Sum(nil)
}

// Encode serializes and signs the reference.
func (r *AttachmentRef) Encode(secret string) (string, error) {
	if secret == "" {
		return "", errors.New("attachment ref: empty secret")
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(payload) + "." + enc.EncodeToString(attachmentRefMAC(secret, payload)), nil
}

var ErrInvalidAttachmentRef = errors.New("invalid or expired attachment ref")

// DecodeAttachmentRef verifies the signature and expiry of a reference.
func DecodeAttachmentRef(value, secret string, now time.Time) (*AttachmentRef, error) {
	if secret == "" {
		return nil, ErrInvalidAttachmentRef
	}
	payloadB64, sigB64, ok := strings.Cut(strings.TrimSpace(value), ".")
	if !ok {
		return nil, ErrInvalidAttachmentRef
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(payloadB64)
	if err != nil {
		return nil, ErrInvalidAttachmentRef
	}
	sig, err := enc.DecodeString(sigB64)
	if err != nil || !hmac.Equal(sig, attachmentRefMAC(secret, payload)) {
		return nil, ErrInvalidAttachmentRef
	}
	var r AttachmentRef
	if err := json.Unmarshal(payload, &r); err != nil || r.Owner == "" || r.ID == "" {
		return nil, ErrInvalidAttachmentRef
	}
	if now.Unix() > r.Expires {
		return nil, ErrInvalidAttachmentRef
	}
	return &r, nil
}
