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

// The document scope FilterDocuments returns is passed straight into
// nlp.RetrievalRequest.DocIDs, so it decides what the agent can find at all.
// Nothing downstream unpacks the "-999" no-match sentinel -- it reaches the doc
// store as a literal document id and matches no chunk. That is the intended
// answer for manual conditions the user wrote, and the wrong answer for an auto
// filter that could not narrow the search, which ApplyMetaDataFilter reports as
// nil, i.e. no metadata narrowing at all.

package retrievalbridge

import (
	"context"
	"reflect"
	"testing"

	"ragflow/internal/common"
	"ragflow/internal/engine"
	"ragflow/internal/engine/types"
	modelModule "ragflow/internal/entity/models"
	"ragflow/internal/service"

	"gorm.io/gorm"
)

func newFilterChatModel() *modelModule.ChatModel {
	name := "fake-model"
	driver := modelModule.NewDummyModel(nil, modelModule.URLSuffix{})
	return &modelModule.ChatModel{ModelDriver: driver, ModelName: &name, APIConfig: &modelModule.APIConfig{}}
}

// An auto filter that yields no conditions -- because the question named no
// metadata, or because the value space did not fit the model's context and
// generation was refused -- must leave the caller's scope alone. Answering with
// the no-match sentinel turns "we could not narrow this" into zero recall,
// which is strictly worse than searching the caller's whole scope.
func TestFilterDocumentsKeepsBaseScopeWhenTheFilterCannotNarrow(t *testing.T) {
	base := []string{"doc-1", "doc-2"}

	var diagnostic common.MetadataFilterDiagnostic
	got, err := NewEnhancer(nil, nil).FilterDocuments(
		t.Context(), map[string]any{"method": "auto"}, "who wrote it?", newFilterChatModel(), base, nil, &diagnostic,
	)
	if err != nil {
		t.Fatalf("FilterDocuments: %v", err)
	}
	// The agent reference reports the same outcome the retrieval test does.
	if diagnostic.Method != "auto" || diagnostic.Status != "not_generated" {
		t.Fatalf("diagnostic = %+v, want auto/not_generated", diagnostic)
	}

	for _, id := range got {
		if id == service.NoMatchDocIDSentinel {
			t.Fatalf("got %v: a filter that produced no conditions must not scope retrieval to nothing", got)
		}
	}
	if len(got) != len(base) {
		t.Fatalf("got %v, want the caller's scope %v", got, base)
	}
	for i := range got {
		if got[i] != base[i] {
			t.Fatalf("got %v, want the caller's scope %v", got, base)
		}
	}
}

// Manual conditions are the user's own, so a manual filter that matches nothing
// still scopes retrieval to nothing. This is the one case the sentinel exists for.
func TestFilterDocumentsReturnsTheSentinelForManualNoMatch(t *testing.T) {
	filter := map[string]any{
		"method": "manual",
		"logic":  "and",
		"manual": []any{map[string]any{"key": "author", "op": "=", "value": "nobody"}},
	}

	got, err := NewEnhancer(nil, nil).FilterDocuments(
		t.Context(), filter, "", nil, []string{"doc-1"}, nil, nil,
	)
	if err != nil {
		t.Fatalf("FilterDocuments: %v", err)
	}
	if len(got) != 1 || got[0] != service.NoMatchDocIDSentinel {
		t.Fatalf("got %v, want [%s]", got, service.NoMatchDocIDSentinel)
	}
}

// pushdownEngine answers the metadata push-down with a fixed result and counts
// the flattened-metadata reads the push-down path is meant to avoid.
type pushdownEngine struct {
	engine.DocEngine
	ids        []string
	conditions []map[string]interface{}
	logic      string
	scans      int
}

func (e *pushdownEngine) FilterDocIdsByMetaPushdown(_ context.Context, _ *gorm.DB, _ []string, conditions []map[string]interface{}, logic string) []string {
	e.conditions, e.logic = conditions, logic
	return e.ids
}

func (e *pushdownEngine) SearchMetadata(context.Context, *types.SearchMetadataRequest) (*types.SearchMetadataResult, error) {
	e.scans++
	return &types.SearchMetadataResult{}, nil
}

func pushdownFilter() map[string]any {
	return map[string]any{"method": "manual", "logic": "and", "manual": []any{
		map[string]any{"key": "project", "op": "contains", "value": "Nivy"},
		map[string]any{"key": "phase", "op": "in", "value": []any{"DSP"}},
	}}
}

// When the metadata index answers, the flattened metadata is never read and
// the result is constrained to the caller's base scope.
func TestPushdownDocumentsSkipsTheFlattenedScan(t *testing.T) {
	docEngine := &pushdownEngine{ids: []string{"doc-2", "doc-3", "doc-2"}}
	enhancer := NewEnhancer(docEngine, service.NewMetadataServiceForTest(nil, docEngine))

	got, err := enhancer.PushdownDocuments(t.Context(), pushdownFilter(), []string{"doc-1", "doc-2"}, []string{"kb-1"})
	if err != nil {
		t.Fatalf("PushdownDocuments: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"doc-2"}) {
		t.Fatalf("got %v, want the base scope's match [doc-2]", got)
	}
	want := []map[string]interface{}{
		{"key": "project", "op": "contains", "value": "Nivy"},
		{"key": "phase", "op": "in", "value": []any{"DSP"}},
	}
	if !reflect.DeepEqual(docEngine.conditions, want) || docEngine.logic != "and" {
		t.Fatalf("pushed down %v (%s), want %v (and)", docEngine.conditions, docEngine.logic, want)
	}
	if docEngine.scans != 0 {
		t.Fatalf("flattened metadata was read %d times", docEngine.scans)
	}

	docEngine.ids = []string{}
	got, err = enhancer.PushdownDocuments(t.Context(), pushdownFilter(), nil, []string{"kb-1"})
	if err != nil || len(got) != 0 {
		t.Fatalf("definitive no-match: got %v err=%v, want nothing", got, err)
	}
}

// When the push-down cannot answer, FilterDocuments decides, and its no-match
// sentinel is not handed on as a document id.
func TestPushdownDocumentsFallsBackWithoutTheSentinel(t *testing.T) {
	docEngine := &pushdownEngine{}
	enhancer := NewEnhancer(docEngine, service.NewMetadataServiceForTest(nil, docEngine))

	got, err := enhancer.PushdownDocuments(t.Context(), pushdownFilter(), []string{"doc-1"}, nil)
	if err != nil {
		t.Fatalf("PushdownDocuments: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want no documents", got)
	}
}

// Project codes look like Python int literals with a digit separator; pushed
// down as "=" they were coerced to numbers and matched nothing, which sent
// every explicit filter to the 10k-capped flattened scan.
func TestPushdownDocumentsKeepsValuesAsStrings(t *testing.T) {
	docEngine := &pushdownEngine{ids: []string{"doc-1"}}
	enhancer := NewEnhancer(docEngine, service.NewMetadataServiceForTest(nil, docEngine))
	filter := map[string]any{"method": "manual", "logic": "and", "manual": []any{
		map[string]any{"key": "project_id", "op": "=", "value": "2018_055"},
		map[string]any{"key": "phase", "op": "in", "value": []string{"sp"}},
		map[string]any{"key": "flow", "op": "in", "value": []any{"expedition"}},
	}}
	if _, err := enhancer.PushdownDocuments(t.Context(), filter, nil, []string{"kb-1"}); err != nil {
		t.Fatalf("PushdownDocuments: %v", err)
	}
	want := []map[string]interface{}{
		{"key": "project_id", "op": "in", "value": []any{"2018_055"}},
		{"key": "phase", "op": "in", "value": []any{"sp"}},
		{"key": "flow", "op": "in", "value": []any{"expedition"}},
	}
	if !reflect.DeepEqual(docEngine.conditions, want) {
		t.Fatalf("pushed down %v, want %v", docEngine.conditions, want)
	}
}
