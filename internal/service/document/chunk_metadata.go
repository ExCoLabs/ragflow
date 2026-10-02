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

package document

import (
	"context"
	"fmt"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/entity"

	"go.uber.org/zap"
)

// chunkMetadataTarget returns the engine and whitelist a metadata change of
// doc must be fanned out with, or ok=false when its dataset does not copy
// document metadata onto chunks (one dataset read, no engine call).
func (s *DocumentService) chunkMetadataTarget(ctx context.Context, doc *entity.Document) (engine.ChunkMetadataStore, *entity.Knowledgebase, *common.ChunkMetadataConfig, bool) {
	store, ok := engine.AsChunkMetadataStore(s.docEngine)
	if !ok || doc == nil || s.kbDAO == nil {
		return nil, nil, nil, false
	}
	kb, err := s.kbDAO.GetByID(ctx, dao.DB, doc.KbID)
	if err != nil || kb == nil {
		return nil, nil, nil, false
	}
	cfg := common.ParseChunkMetadataConfig(kb.ParserConfig)
	if cfg == nil || !cfg.Enabled || len(cfg.Fields) == 0 {
		return nil, nil, nil, false
	}
	return store, kb, cfg, true
}

// syncChunkMetadata writes the document's whitelisted metadata onto every
// chunk of the document (and drops stale fields), the same way a rename
// updates docnm_kwd on them. Failures are logged, not returned: the doc
// metadata write already succeeded and the dataset backfill can repair the
// chunks at any time.
func (s *DocumentService) syncChunkMetadata(ctx context.Context, doc *entity.Document, meta map[string]interface{}) {
	store, kb, cfg, ok := s.chunkMetadataTarget(ctx, doc)
	if !ok {
		return
	}
	set := common.ChunkMetadataFields(meta, cfg.Fields)
	remove := common.ChunkMetadataRemovalFields(meta, cfg.Fields)
	if err := store.UpdateChunkMetadata(ctx, chunkIndexName(kb.TenantID), kb.ID, []string{doc.ID}, set, remove, true); err != nil {
		common.Warn("chunk metadata sync failed", zap.String("doc_id", doc.ID), zap.Error(err))
	}
}

// clearChunkMetadata drops the chunk fields of the deleted metadata keys (all
// whitelisted keys when keys is nil).
func (s *DocumentService) clearChunkMetadata(ctx context.Context, doc *entity.Document, keys []string) {
	store, kb, cfg, ok := s.chunkMetadataTarget(ctx, doc)
	if !ok {
		return
	}
	cleared := cfg.Fields
	if keys != nil {
		whitelisted := make(map[string]bool, len(cfg.Fields))
		for _, f := range cfg.Fields {
			whitelisted[f] = true
		}
		cleared = make([]string, 0, len(keys))
		for _, k := range keys {
			if whitelisted[k] {
				cleared = append(cleared, k)
			}
		}
	}
	if len(cleared) == 0 {
		return
	}
	if err := store.UpdateChunkMetadata(ctx, chunkIndexName(kb.TenantID), kb.ID, []string{doc.ID}, nil, common.ChunkMetaFieldNames(cleared), true); err != nil {
		common.Warn("chunk metadata clear failed", zap.String("doc_id", doc.ID), zap.Error(err))
	}
}

func chunkIndexName(tenantID string) string {
	return fmt.Sprintf("ragflow_%s", tenantID)
}
