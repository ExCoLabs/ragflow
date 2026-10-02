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

package nlp

import (
	"context"
	"slices"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine/types"
)

// chunkMetaRecordingEngine records the chunk metadata scope and the selected
// fields every search was sent with.
type chunkMetaRecordingEngine struct {
	retrievalCountEngine
	scopes       []*common.ChunkMetaScope
	selectFields [][]string
}

func (e *chunkMetaRecordingEngine) Search(ctx context.Context, req *types.SearchRequest) (*types.SearchResult, error) {
	e.scopes = append(e.scopes, req.ChunkMeta)
	e.selectFields = append(e.selectFields, req.SelectFields)
	return e.retrievalCountEngine.Search(ctx, req)
}

func TestRetrievalAppliesChunkMetaBoost(t *testing.T) {
	oldQueryBuilder := globalQueryBuilder
	globalQueryBuilder = NewQueryBuilder()
	defer func() { globalQueryBuilder = oldQueryBuilder }()

	rows := []map[string]interface{}{
		{"id": "old", "content_ltks": "alpha", "content_with_weight": "alpha", "_score": 0.9, "meta_date_dt": "2020-01-01"},
		{"id": "new", "content_ltks": "alpha", "content_with_weight": "alpha", "_score": 0.8, "meta_date_dt": "2024-01-01"},
	}
	eng := &chunkMetaRecordingEngine{retrievalCountEngine: retrievalCountEngine{rows: rows}}
	svc := NewRetrievalService(eng, &dao.DocumentDAO{})
	threshold, vectorWeight, aggs := 0.0, 1.0, false
	scope := &common.ChunkMetaScope{
		Filter:        &common.ChunkMetaFilter{Logic: "and", Conditions: []map[string]interface{}{{"key": "dept", "op": "=", "value": "hr"}}},
		Boosts:        []common.ChunkMetaBoost{{Key: "date", Op: "max", Weight: 0.3}},
		BoostMaxTotal: 0.3,
	}
	result, err := svc.Retrieval(t.Context(), &RetrievalRequest{
		Question:               "alpha",
		TenantIDs:              []string{"tenant-1"},
		Page:                   1,
		PageSize:               2,
		SimilarityThreshold:    &threshold,
		VectorSimilarityWeight: &vectorWeight,
		Aggs:                   &aggs,
		ChunkMeta:              scope,
	})
	if err != nil {
		t.Fatalf("Retrieval failed: %v", err)
	}
	if len(eng.scopes) == 0 || eng.scopes[0] != scope {
		t.Fatalf("the engine must receive the chunk metadata scope, got %v", eng.scopes)
	}
	if !slices.Contains(eng.selectFields[0], "meta_date_dt") {
		t.Fatalf("the boost fields must be selected, got %v", eng.selectFields[0])
	}
	if len(result.Chunks) != 2 || result.Chunks[0]["chunk_id"] != "new" {
		t.Fatalf("the max boost must lift the newest chunk first, got %v", result.Chunks)
	}
	if sim := result.Chunks[0]["similarity"].(float64); sim < 1.0 {
		t.Fatalf("boost must be added to the fused similarity, got %v", sim)
	}
}
