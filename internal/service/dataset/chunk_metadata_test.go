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

import (
	"reflect"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/entity"
)

func TestNextChunkMetadataConfigReadyIsOwnedByBackfill(t *testing.T) {
	ready := &common.ChunkMetadataConfig{Enabled: true, Fields: []string{"year", "dept"}, Ready: true}
	enabled, disabled := true, false

	cases := []struct {
		name      string
		current   *common.ChunkMetadataConfig
		req       ChunkMetadataConfigRequest
		wantReady bool
	}{
		{"same fields in another order keep ready", ready, ChunkMetadataConfigRequest{Fields: []any{"dept", "year"}}, true},
		{"enabling again keeps ready", ready, ChunkMetadataConfigRequest{Enabled: &enabled}, true},
		{"a changed whitelist resets ready", ready, ChunkMetadataConfigRequest{Fields: []any{"year"}}, false},
		{"disabling resets ready", ready, ChunkMetadataConfigRequest{Enabled: &disabled}, false},
		{"a new config is never ready", nil, ChunkMetadataConfigRequest{Enabled: &enabled, Fields: []any{"year"}}, false},
	}
	for _, tc := range cases {
		got, err := nextChunkMetadataConfig(tc.current, &tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Ready != tc.wantReady {
			t.Errorf("%s: ready = %v, want %v", tc.name, got.Ready, tc.wantReady)
		}
	}
	if _, err := nextChunkMetadataConfig(nil, &ChunkMetadataConfigRequest{Fields: []any{"doc_id"}}); err == nil {
		t.Fatal("a reserved field must be refused")
	}
}

func TestGroupDocumentsByChunkMetadata(t *testing.T) {
	metas := map[string]map[string]any{
		"d1":    {"dept": "hr", "other": "x"},
		"d2":    {"dept": "hr"},
		"d3":    {"dept": "it"},
		"ghost": {"dept": "it"},
	}
	groups := groupDocumentsByChunkMetadata(metas, []string{"d1", "d2", "d3", "d4", "d1"}, []string{"dept"})
	got := make([][]string, len(groups))
	for i, g := range groups {
		got[i] = g.docIDs
	}
	want := [][]string{{"d1", "d2"}, {"d3", "ghost"}, {"d4"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	if len(groups[2].fields) != 0 {
		t.Fatalf("a document without metadata gets the empty group (stale fields removed): %v", groups[2].fields)
	}
}

func TestPreserveDatasetParserConfigStateKeepsChunkMetadata(t *testing.T) {
	stored := map[string]any{"enabled": true, "fields": []any{"year"}, "ready": true}
	existing := entity.JSONMap{common.ChunkMetadataConfigKey: stored}
	got := preserveDatasetParserConfigState(entity.JSONMap{}, existing, map[string]interface{}{})
	if !reflect.DeepEqual(got[common.ChunkMetadataConfigKey], stored) {
		t.Fatalf("a parser_config rebuild must keep chunk_metadata, got %v", got)
	}
}
