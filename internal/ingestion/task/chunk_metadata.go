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

package task

import (
	"context"
	"encoding/json"
	"fmt"

	"ragflow/internal/common"
	"ragflow/internal/engine"
	enginetypes "ragflow/internal/engine/types"
)

// storedDocMetadataFunc reads a document's metadata from the doc metadata
// index. A variable so tests can stub the engine read.
var storedDocMetadataFunc = storedDocMetadata

// stampChunkMetadata copies the dataset's whitelisted document metadata onto
// every chunk this run writes (common.ChunkMetadataFields), so a re-parsed
// document keeps the fields chunk-level metadata filters and boosts rely on.
// Metadata the pipeline extracts in this run reaches the chunks afterwards,
// through the document metadata write (DocumentService.SetDocumentMetadata).
// Failures are logged: missing fields only send queries back to the doc-id
// path once the dataset is re-backfilled, they never fail the ingestion.
func (s *PipelineExecutor) stampChunkMetadata(ctx context.Context, chunks []map[string]any) {
	cfg := common.ParseChunkMetadataConfig(map[string]any(s.taskCtx.KB.ParserConfig))
	if cfg == nil || !cfg.Enabled || len(cfg.Fields) == 0 || len(chunks) == 0 {
		return
	}
	docEngine := engine.Get()
	if _, ok := engine.AsChunkMetadataStore(docEngine); !ok {
		return
	}
	meta, err := storedDocMetadataFunc(ctx, docEngine, s.taskCtx.KB.TenantID, s.taskCtx.Doc.KbID, s.taskCtx.Doc.ID)
	if err != nil {
		common.Warn(fmt.Sprintf("reading document metadata for chunk stamping failed for %s: %v", s.taskCtx.Doc.ID, err))
		return
	}
	applyChunkMetadataFields(chunks, common.ChunkMetadataFields(meta, cfg.Fields))
}

func applyChunkMetadataFields(chunks []map[string]any, fields map[string]any) {
	if len(fields) == 0 {
		return
	}
	for _, ck := range chunks {
		for k, v := range fields {
			ck[k] = v
		}
	}
}

func storedDocMetadata(ctx context.Context, docEngine engine.DocEngine, tenantID, kbID, docID string) (map[string]any, error) {
	res, err := docEngine.SearchMetadata(ctx, &enginetypes.SearchMetadataRequest{
		TenantID: tenantID,
		Limit:    1,
		Filter:   map[string]interface{}{"id": []string{docID}, "kb_id": kbID},
	})
	if err != nil || res == nil || len(res.MetadataRecords) == 0 {
		return nil, err
	}
	switch v := res.MetadataRecords[0]["meta_fields"].(type) {
	case map[string]any:
		return v, nil
	case string:
		meta := map[string]any{}
		if err := json.Unmarshal([]byte(v), &meta); err != nil {
			return nil, err
		}
		return meta, nil
	}
	return nil, nil
}
