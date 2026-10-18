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
	"net/http"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"

	"ragflow/internal/common"
	"ragflow/internal/engine/kvrocks"
	"ragflow/internal/server"
	"ragflow/internal/service/file"
	"ragflow/internal/utility"
)

var attachmentRefSecret = func(ctx context.Context) (string, error) {
	return server.GetSecretKey(ctx, kvrocks.Get())
}

type attachmentByRefRequest struct {
	Ref string `json:"ref" binding:"required"`
}

// DownloadAttachmentByRef POST /api/v1/agents/attachments/by_ref
//
// Returns an agent upload identified by the signed "Attachment ref" that an
// Agent run adds to sys.files. Tools such as MCP servers call it with their
// own API key, so the signature, not the caller, proves which upload may be
// read. The ref travels in the body to keep it out of access logs.
func (h *AgentHandler) DownloadAttachmentByRef(c *gin.Context) {
	if _, code, msg := GetUser(c); code != common.CodeSuccess {
		common.ResponseWithCodeData(c, code, nil, msg)
		return
	}
	var req attachmentByRefRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ResponseWithCodeData(c, common.CodeArgumentError, nil, "`ref` is required.")
		return
	}
	if h.fileService == nil {
		common.ResponseWithCodeData(c, common.CodeServerError, nil, "file service not configured")
		return
	}

	ctx := c.Request.Context()
	secret, err := attachmentRefSecret(ctx)
	if err != nil {
		common.ResponseWithCodeData(c, common.CodeServerError, nil, "secret key unavailable")
		return
	}
	ref, err := file.DecodeAttachmentRef(req.Ref, secret, time.Now())
	if err != nil {
		common.ResponseWithCodeData(c, common.CodeArgumentError, nil, "invalid or expired attachment ref.")
		return
	}
	blob, err := h.fileService.DownloadAgentFile(ctx, ref.Owner, ref.ID)
	if err != nil {
		common.ResponseWithCodeData(c, common.CodeDataError, nil, "Attachment not found!")
		return
	}

	contentType, ext := utility.ResolveAttachmentContentType(filepath.Ext(ref.Name), ref.MimeType)
	utility.SetDownloadFileResponseHeaders(c.Writer.Header(), contentType, ext, ref.Name)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Data(http.StatusOK, contentType, blob)
}
