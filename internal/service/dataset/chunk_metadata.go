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

package dataset

// Dataset-level configuration and backfill of document metadata on chunks
// (common.ChunkMetadataConfig). The configuration lives in
// parser_config.chunk_metadata; "ready" is owned by the backfill: the client
// can enable the feature and pick the fields, but only a completed backfill
// lets queries rely on the chunk fields.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/entity"

	"go.uber.org/zap"
)

// chunkMetadataDocIDsPerUpdate keeps each update_by_query's terms clause well
// under Elasticsearch's index.max_terms_count (65 536).
const chunkMetadataDocIDsPerUpdate = 20000

// ChunkMetadataConfigRequest is the body of PUT /datasets/:id/chunk_metadata.
type ChunkMetadataConfigRequest struct {
	Enabled *bool `json:"enabled"`
	Fields  []any `json:"fields"`
}

// chunkMetadataBackfillState is the progress of a running or finished backfill
// on this server instance; "ready" in parser_config is the durable outcome.
type chunkMetadataBackfillState struct {
	Running    bool      `json:"running"`
	Error      string    `json:"error,omitempty"`
	Documents  int       `json:"documents"`
	Groups     int       `json:"groups"`
	GroupsDone int       `json:"groups_done"`
	StartedAt  time.Time `json:"started_at"`
}

var (
	chunkMetadataBackfillsMu sync.Mutex
	chunkMetadataBackfills   = map[string]*chunkMetadataBackfillState{}
)

func chunkMetadataBackfillSnapshot(datasetID string) *chunkMetadataBackfillState {
	chunkMetadataBackfillsMu.Lock()
	defer chunkMetadataBackfillsMu.Unlock()
	if st, ok := chunkMetadataBackfills[datasetID]; ok {
		cp := *st
		return &cp
	}
	return nil
}

func (d *DatasetService) accessibleDataset(ctx context.Context, datasetID, userID string) (*entity.Knowledgebase, common.ErrorCode, error) {
	datasetID = strings.TrimSpace(datasetID)
	if datasetID == "" {
		return nil, common.CodeDataError, errors.New(`Lack of "Dataset ID"`)
	}
	if !d.Accessible(ctx, datasetID, userID) {
		return nil, common.CodeDataError, errors.New("no authorization")
	}
	kb, err := d.kbDAO.GetByID(ctx, dao.DB, datasetID)
	if err != nil || kb == nil {
		if err == nil || dao.IsNotFoundErr(err) {
			return nil, common.CodeDataError, errors.New("Invalid Dataset ID")
		}
		return nil, common.CodeServerError, errors.New("database operation failed")
	}
	return kb, common.CodeSuccess, nil
}

func (d *DatasetService) chunkMetadataStatus(kb *entity.Knowledgebase) map[string]any {
	_, supported := engine.AsChunkMetadataStore(d.docEngine)
	cfg := common.ParseChunkMetadataConfig(kb.ParserConfig)
	var config any
	if cfg != nil {
		config = cfg.ToMap()
	}
	status := map[string]any{
		"supported": supported,
		"config":    config,
		"active":    supported && cfg.Active(),
	}
	if st := chunkMetadataBackfillSnapshot(kb.ID); st != nil {
		status["backfill"] = st
	}
	return status
}

// GetChunkMetadata reports whether the engine supports chunk metadata, the
// dataset's configuration, whether filters and boosts are active, and the
// last backfill run on this server.
func (d *DatasetService) GetChunkMetadata(ctx context.Context, datasetID, userID string) (map[string]any, common.ErrorCode, error) {
	kb, code, err := d.accessibleDataset(ctx, datasetID, userID)
	if err != nil {
		return nil, code, err
	}
	return d.chunkMetadataStatus(kb), common.CodeSuccess, nil
}

// nextChunkMetadataConfig applies a configuration update. ready survives only
// while the feature stays enabled with the same whitelist: existing chunks do
// not carry new fields until the backfill has run.
func nextChunkMetadataConfig(current *common.ChunkMetadataConfig, req *ChunkMetadataConfigRequest) (*common.ChunkMetadataConfig, error) {
	next := &common.ChunkMetadataConfig{}
	if current != nil {
		next.Enabled = current.Enabled
		next.Fields = append([]string(nil), current.Fields...)
	}
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.Fields != nil {
		fields, err := common.ValidateChunkMetadataFields(req.Fields)
		if err != nil {
			return nil, err
		}
		next.Fields = fields
	}
	if next.Fields == nil {
		next.Fields = []string{}
	}
	sameFields := current != nil && slices.Equal(sortedCopy(current.Fields), sortedCopy(next.Fields))
	next.Ready = current != nil && current.Ready && next.Enabled && sameFields
	return next, nil
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	slices.Sort(out)
	return out
}

// UpdateChunkMetadata sets parser_config.chunk_metadata.enabled/fields.
func (d *DatasetService) UpdateChunkMetadata(ctx context.Context, datasetID, userID string, req *ChunkMetadataConfigRequest) (map[string]any, common.ErrorCode, error) {
	kb, code, err := d.accessibleDataset(ctx, datasetID, userID)
	if err != nil {
		return nil, code, err
	}
	if req == nil {
		req = &ChunkMetadataConfigRequest{}
	}
	next, err := nextChunkMetadataConfig(common.ParseChunkMetadataConfig(kb.ParserConfig), req)
	if err != nil {
		return nil, common.CodeArgumentError, err
	}
	parserConfig := kb.ParserConfig
	if parserConfig == nil {
		parserConfig = entity.JSONMap{}
	}
	parserConfig[common.ChunkMetadataConfigKey] = next.ToMap()
	if err := d.kbDAO.UpdateByID(ctx, dao.DB, kb.ID, map[string]any{"parser_config": parserConfig}); err != nil {
		return nil, common.CodeServerError, errors.New("update chunk metadata error.(Database error)")
	}
	kb.ParserConfig = parserConfig
	return d.chunkMetadataStatus(kb), common.CodeSuccess, nil
}

// RunChunkMetadataBackfill starts copying the whitelisted document metadata
// onto every existing chunk of the dataset. It runs in the background on this
// server; when it completes, parser_config.chunk_metadata.ready is set.
func (d *DatasetService) RunChunkMetadataBackfill(ctx context.Context, datasetID, userID string) (map[string]any, common.ErrorCode, error) {
	kb, code, err := d.accessibleDataset(ctx, datasetID, userID)
	if err != nil {
		return nil, code, err
	}
	cfg := common.ParseChunkMetadataConfig(kb.ParserConfig)
	if cfg == nil || !cfg.Enabled || len(cfg.Fields) == 0 {
		return nil, common.CodeDataError, errors.New("parser_config.chunk_metadata must be enabled with at least one field")
	}
	store, ok := engine.AsChunkMetadataStore(d.docEngine)
	if !ok {
		return nil, common.CodeDataError, fmt.Errorf("the document engine (%s) does not support chunk metadata fields", engine.Type(d.docEngine))
	}

	chunkMetadataBackfillsMu.Lock()
	if st, running := chunkMetadataBackfills[kb.ID]; running && st.Running {
		chunkMetadataBackfillsMu.Unlock()
		return nil, common.CodeDataError, errors.New("a chunk metadata backfill is already running for this dataset")
	}
	state := &chunkMetadataBackfillState{Running: true, StartedAt: time.Now()}
	chunkMetadataBackfills[kb.ID] = state
	chunkMetadataBackfillsMu.Unlock()

	go func() {
		runCtx := context.WithoutCancel(ctx)
		err := d.backfillChunkMetadata(runCtx, store, kb, cfg, state)
		chunkMetadataBackfillsMu.Lock()
		state.Running = false
		if err != nil {
			state.Error = err.Error()
		}
		chunkMetadataBackfillsMu.Unlock()
		if err != nil {
			common.Warn("chunk metadata backfill failed", zap.String("dataset_id", kb.ID), zap.Error(err))
			return
		}
		common.Info("chunk metadata backfill done", zap.String("dataset_id", kb.ID),
			zap.Int("documents", state.Documents), zap.Int("groups", state.Groups), zap.Strings("fields", cfg.Fields))
	}()

	return map[string]any{"fields": cfg.Fields, "running": true}, common.CodeSuccess, nil
}

// chunkMetadataGroup is a set of documents whose chunks get the same fields.
type chunkMetadataGroup struct {
	fields map[string]any
	docIDs []string
}

// groupDocumentsByChunkMetadata groups documents that share the same
// whitelisted metadata so each group is written with one update_by_query.
// Documents without any whitelisted key get the empty group, so stale fields
// from a previous whitelist are removed from them too.
func groupDocumentsByChunkMetadata(metas map[string]map[string]any, allDocIDs []string, keys []string) []*chunkMetadataGroup {
	bySig := map[string]*chunkMetadataGroup{}
	var order []string
	add := func(docID string, fields map[string]any) {
		sigBytes, _ := json.Marshal(fields) // map keys are sorted
		sig := string(sigBytes)
		g, ok := bySig[sig]
		if !ok {
			g = &chunkMetadataGroup{fields: fields}
			bySig[sig] = g
			order = append(order, sig)
		}
		g.docIDs = append(g.docIDs, docID)
	}
	seen := make(map[string]bool, len(allDocIDs))
	for _, docID := range allDocIDs {
		if docID == "" || seen[docID] {
			continue
		}
		seen[docID] = true
		add(docID, common.ChunkMetadataFields(metas[docID], keys))
	}
	// Metadata rows whose document is not in the database (being deleted)
	// still have chunks worth keeping consistent.
	extra := make([]string, 0)
	for docID := range metas {
		if !seen[docID] {
			extra = append(extra, docID)
		}
	}
	slices.Sort(extra)
	for _, docID := range extra {
		add(docID, common.ChunkMetadataFields(metas[docID], keys))
	}
	out := make([]*chunkMetadataGroup, 0, len(order))
	for _, sig := range order {
		out = append(out, bySig[sig])
	}
	return out
}

func (d *DatasetService) backfillChunkMetadata(ctx context.Context, store engine.ChunkMetadataStore, kb *entity.Knowledgebase, cfg *common.ChunkMetadataConfig, state *chunkMetadataBackfillState) error {
	metas := map[string]map[string]any{}
	if err := store.ScanDocMetadata(ctx, kb.TenantID, kb.ID, func(docID string, meta map[string]any) error {
		metas[docID] = meta
		return nil
	}); err != nil {
		return fmt.Errorf("read document metadata: %w", err)
	}
	rows, err := d.documentDAO.GetAllDocIDsByKBIDs(ctx, dao.DB, []string{kb.ID})
	if err != nil {
		return fmt.Errorf("list documents: %w", err)
	}
	docIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		docIDs = append(docIDs, row["id"])
	}
	groups := groupDocumentsByChunkMetadata(metas, docIDs, cfg.Fields)
	chunkMetadataBackfillsMu.Lock()
	state.Documents, state.Groups = len(docIDs), len(groups)
	chunkMetadataBackfillsMu.Unlock()

	indexName := fmt.Sprintf("ragflow_%s", kb.TenantID)
	allFields := common.ChunkMetaFieldNames(cfg.Fields)
	failed := 0
	for i, g := range groups {
		remove := make([]string, 0, len(allFields))
		for _, f := range allFields {
			if _, set := g.fields[f]; !set {
				remove = append(remove, f)
			}
		}
		for start := 0; start < len(g.docIDs); start += chunkMetadataDocIDsPerUpdate {
			batch := g.docIDs[start:min(start+chunkMetadataDocIDsPerUpdate, len(g.docIDs))]
			// Refresh once at the end: a per-request refresh on a large index is the slow part.
			if err := store.UpdateChunkMetadata(ctx, indexName, kb.ID, batch, g.fields, remove, false); err != nil {
				common.Warn("chunk metadata backfill batch failed", zap.String("dataset_id", kb.ID), zap.Error(err))
				failed += len(batch)
			}
		}
		chunkMetadataBackfillsMu.Lock()
		state.GroupsDone = i + 1
		chunkMetadataBackfillsMu.Unlock()
	}
	if err := store.RefreshChunkIndex(ctx, indexName); err != nil {
		common.Warn("refresh after chunk metadata backfill failed", zap.String("index", indexName), zap.Error(err))
	}
	if failed > 0 {
		return fmt.Errorf("chunk metadata backfill failed for %d document(s); ready left unchanged", failed)
	}

	// Mark ready only if the configuration did not change while the backfill ran.
	latest, err := d.kbDAO.GetByID(ctx, dao.DB, kb.ID)
	if err != nil || latest == nil {
		return fmt.Errorf("reload dataset: %w", err)
	}
	current := common.ParseChunkMetadataConfig(latest.ParserConfig)
	if current == nil || !current.Enabled || !slices.Equal(sortedCopy(current.Fields), sortedCopy(cfg.Fields)) {
		return errors.New("chunk_metadata changed while the backfill ran; run it again")
	}
	current.Ready = true
	parserConfig := latest.ParserConfig
	parserConfig[common.ChunkMetadataConfigKey] = current.ToMap()
	if err := d.kbDAO.UpdateByID(ctx, dao.DB, kb.ID, map[string]any{"parser_config": parserConfig}); err != nil {
		return fmt.Errorf("chunks written but saving chunk_metadata.ready failed: %w", err)
	}
	return nil
}
