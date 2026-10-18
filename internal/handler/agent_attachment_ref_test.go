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
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"ragflow/internal/common"
	"ragflow/internal/entity"
	"ragflow/internal/service/file"
)

type recordingAttachmentFileService struct {
	fakeAgentFileService
	owner, id string
}

func (f *recordingAttachmentFileService) DownloadAgentFile(ctx context.Context, owner, id string) ([]byte, error) {
	f.owner, f.id = owner, id
	return f.fakeAgentFileService.DownloadAgentFile(ctx, owner, id)
}

func withAttachmentRefSecret(t *testing.T, secret string, err error) {
	t.Helper()
	original := attachmentRefSecret
	attachmentRefSecret = func(context.Context) (string, error) { return secret, err }
	t.Cleanup(func() { attachmentRefSecret = original })
}

func byRefCtx(t *testing.T, body string, user *entity.User) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/v1/agents/attachments/by_ref", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if user != nil {
		c.Set("user", user)
	}
	return c, w
}

func signedRef(t *testing.T, secret string, expires time.Time) string {
	t.Helper()
	ref := file.AttachmentRef{Owner: "uploader-1", ID: "att-1", Name: "rozpočet.xlsx",
		MimeType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Expires: expires.Unix()}
	encoded, err := ref.Encode(secret)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return encoded
}

func responseCode(t *testing.T, w *httptest.ResponseRecorder) common.ErrorCode {
	t.Helper()
	var body struct {
		Code common.ErrorCode `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return body.Code
}

func TestDownloadAttachmentByRef_ReadsUploaderBucket(t *testing.T) {
	withAttachmentRefSecret(t, "secret", nil)
	svc := &recordingAttachmentFileService{fakeAgentFileService: fakeAgentFileService{blob: []byte("xlsx-bytes")}}
	h := &AgentHandler{fileService: svc}

	body, _ := json.Marshal(map[string]string{"ref": signedRef(t, "secret", time.Now().Add(time.Hour))})
	c, w := byRefCtx(t, string(body), &entity.User{ID: "service-user"})
	h.DownloadAttachmentByRef(c)

	if w.Code != 200 || w.Body.String() != "xlsx-bytes" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if svc.owner != "uploader-1" || svc.id != "att-1" {
		t.Fatalf("read %s/%s, want the uploader's bucket uploader-1/att-1", svc.owner, svc.id)
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "spreadsheetml") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.Contains(got, "rozpo") {
		t.Errorf("Content-Disposition = %q, want the original filename", got)
	}
}

func TestDownloadAttachmentByRef_Rejects(t *testing.T) {
	user := &entity.User{ID: "service-user"}
	valid := func(t *testing.T) string {
		body, _ := json.Marshal(map[string]string{"ref": signedRef(t, "secret", time.Now().Add(time.Hour))})
		return string(body)
	}
	cases := []struct {
		name      string
		body      func(*testing.T) string
		user      *entity.User
		secretErr error
		blobErr   error
		wantCode  common.ErrorCode
	}{
		{"no user", valid, nil, nil, nil, common.CodeUnauthorized},
		{"missing ref", func(*testing.T) string { return `{}` }, user, nil, nil, common.CodeArgumentError},
		{"forged ref", func(t *testing.T) string {
			body, _ := json.Marshal(map[string]string{"ref": signedRef(t, "other-secret", time.Now().Add(time.Hour))})
			return string(body)
		}, user, nil, nil, common.CodeArgumentError},
		{"expired ref", func(t *testing.T) string {
			body, _ := json.Marshal(map[string]string{"ref": signedRef(t, "secret", time.Now().Add(-time.Minute))})
			return string(body)
		}, user, nil, nil, common.CodeArgumentError},
		{"secret unavailable", valid, user, errors.New("kvrocks down"), nil, common.CodeServerError},
		{"blob missing", valid, user, nil, errors.New("not found"), common.CodeDataError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withAttachmentRefSecret(t, "secret", tc.secretErr)
			h := &AgentHandler{fileService: &fakeAgentFileService{blob: []byte("x"), err: tc.blobErr}}
			c, w := byRefCtx(t, tc.body(t), tc.user)
			h.DownloadAttachmentByRef(c)
			if got := responseCode(t, w); got != tc.wantCode {
				t.Fatalf("code = %d, want %d; body=%s", got, tc.wantCode, w.Body.String())
			}
		})
	}
}
