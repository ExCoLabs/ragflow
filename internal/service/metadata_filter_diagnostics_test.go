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

package service

import (
	"context"
	"reflect"
	"testing"

	"ragflow/internal/common"
	modelModule "ragflow/internal/entity/models"
)

// metaFilterAnswerDriver answers every chat call with a fixed gen_meta_filter
// response, standing in for the LLM behind auto/semi_auto filters.
type metaFilterAnswerDriver struct {
	*modelModule.DummyModel
	answer string
}

func (d *metaFilterAnswerDriver) ChatWithMessages(
	ctx context.Context,
	modelName string,
	messages []modelModule.Message,
	apiConfig *modelModule.APIConfig,
	chatModelConfig *modelModule.ChatConfig,
	modelUsage *common.ModelUsage,
) (*modelModule.ChatResponse, error) {
	answer := d.answer
	return &modelModule.ChatResponse{Answer: &answer}, nil
}

func metaFilterChatModel(answer string) *modelModule.ChatModel {
	modelName := "fake-model"
	return &modelModule.ChatModel{
		ModelDriver: &metaFilterAnswerDriver{
			DummyModel: modelModule.NewDummyModel(nil, modelModule.URLSuffix{}),
			answer:     answer,
		},
		ModelName: &modelName,
		APIConfig: &modelModule.APIConfig{},
	}
}

var diagnosticsTestMeta = common.MetaData{
	"year":   {"2025": {"doc1"}, "2026": {"doc2", "doc3"}},
	"author": {"ann": {"doc1", "doc2"}},
}

func applyWithDiagnostics(t *testing.T, filter map[string]interface{}, chatModel *modelModule.ChatModel, resolver ...ManualValueResolver) ([]string, bool, common.MetadataFilterDiagnostic) {
	t.Helper()
	var diagnostic common.MetadataFilterDiagnostic
	docIDs, empty := ApplyMetaDataFilterWithDiagnostics(
		context.Background(), filter, diagnosticsTestMeta, "question", chatModel, nil, nil, &diagnostic, resolver...,
	)
	return docIDs, empty, diagnostic
}

func TestApplyMetaDataFilterWithDiagnostics_AutoReportsGeneratedConditions(t *testing.T) {
	chatModel := metaFilterChatModel(`{"conditions": [{"key": "year", "op": "=", "value": "2026"}], "logic": "and"}`)

	docIDs, empty, diagnostic := applyWithDiagnostics(t, map[string]interface{}{"method": "auto"}, chatModel)

	if empty || len(docIDs) != 2 {
		t.Fatalf("docIDs = %v, empty = %v, want the two 2026 documents", docIDs, empty)
	}
	want := common.MetadataFilterDiagnostic{
		Method:               "auto",
		Status:               "applied",
		Conditions:           []map[string]interface{}{{"key": "year", "op": "=", "value": "2026"}},
		Logic:                "and",
		MatchedDocumentCount: 2,
	}
	if !reflect.DeepEqual(diagnostic, want) {
		t.Fatalf("diagnostic = %+v, want %+v", diagnostic, want)
	}
}

func TestApplyMetaDataFilterWithDiagnostics_AutoNoMatches(t *testing.T) {
	chatModel := metaFilterChatModel(`{"conditions": [{"key": "year", "op": "=", "value": "1999"}], "logic": "or"}`)

	docIDs, empty, diagnostic := applyWithDiagnostics(t, map[string]interface{}{"method": "auto"}, chatModel)

	if !empty || docIDs != nil {
		t.Fatalf("docIDs = %v, empty = %v, want the auto filter to report empty", docIDs, empty)
	}
	if diagnostic.Status != "no_matches" || diagnostic.Logic != "or" || len(diagnostic.Conditions) != 1 || diagnostic.MatchedDocumentCount != 0 {
		t.Fatalf("diagnostic = %+v, want no_matches with the generated condition", diagnostic)
	}
}

func TestApplyMetaDataFilterWithDiagnostics_AutoWithoutConditions(t *testing.T) {
	chatModel := metaFilterChatModel(`{"conditions": [], "logic": "and"}`)

	_, _, diagnostic := applyWithDiagnostics(t, map[string]interface{}{"method": "auto"}, chatModel)

	if diagnostic.Method != "auto" || diagnostic.Status != "not_generated" || len(diagnostic.Conditions) != 0 {
		t.Fatalf("diagnostic = %+v, want auto/not_generated with no conditions", diagnostic)
	}
	if diagnostic.Conditions == nil {
		t.Fatal("conditions should serialize as [] rather than null")
	}
}

func TestApplyMetaDataFilterWithDiagnostics_GenerationError(t *testing.T) {
	// A nil chat model makes GenMetaFilter fail; the filter is skipped.
	docIDs, empty, diagnostic := applyWithDiagnostics(t, map[string]interface{}{"method": "auto"}, nil)

	if empty || docIDs != nil {
		t.Fatalf("docIDs = %v, empty = %v, want the unfiltered base", docIDs, empty)
	}
	if diagnostic.Method != "auto" || diagnostic.Status != "not_generated" {
		t.Fatalf("diagnostic = %+v, want auto/not_generated", diagnostic)
	}
}

func TestApplyMetaDataFilterWithDiagnostics_SemiAutoWithoutUsableKeys(t *testing.T) {
	filter := map[string]interface{}{"method": "semi_auto", "semi_auto": []interface{}{"missing"}}

	_, empty, diagnostic := applyWithDiagnostics(t, filter, metaFilterChatModel(`{}`))

	if empty {
		t.Fatal("semi_auto without usable keys should not report an empty filter")
	}
	if diagnostic.Method != "semi_auto" || diagnostic.Status != "not_generated" {
		t.Fatalf("diagnostic = %+v, want semi_auto/not_generated", diagnostic)
	}
}

func TestApplyMetaDataFilterWithDiagnostics_ManualReportsResolvedConditions(t *testing.T) {
	filter := map[string]interface{}{
		"method": "manual",
		"logic":  "or",
		"manual": []interface{}{map[string]interface{}{"key": "author", "op": "=", "value": "someone"}},
	}
	resolver := func(cond map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"key": cond["key"], "op": cond["op"], "value": "ann"}
	}

	docIDs, _, diagnostic := applyWithDiagnostics(t, filter, nil, resolver)

	if len(docIDs) != 2 {
		t.Fatalf("docIDs = %v, want ann's two documents", docIDs)
	}
	want := common.MetadataFilterDiagnostic{
		Method:               "manual",
		Status:               "applied",
		Conditions:           []map[string]interface{}{{"key": "author", "op": "=", "value": "ann"}},
		Logic:                "or",
		MatchedDocumentCount: 2,
	}
	if !reflect.DeepEqual(diagnostic, want) {
		t.Fatalf("diagnostic = %+v, want %+v", diagnostic, want)
	}
}

func TestApplyMetaDataFilterWithDiagnostics_ManualNoMatches(t *testing.T) {
	filter := map[string]interface{}{
		"method": "manual",
		"manual": []interface{}{map[string]interface{}{"key": "author", "op": "=", "value": "nobody"}},
	}

	docIDs, _, diagnostic := applyWithDiagnostics(t, filter, nil)

	if len(docIDs) != 1 || docIDs[0] != NoMatchDocIDSentinel {
		t.Fatalf("docIDs = %v, want the no-match sentinel", docIDs)
	}
	if diagnostic.Status != "no_matches" || len(diagnostic.Conditions) != 1 {
		t.Fatalf("diagnostic = %+v, want no_matches with the condition", diagnostic)
	}
}

func TestApplyMetaDataFilterWithDiagnostics_DisabledAndUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter map[string]interface{}
		method string
		status string
	}{
		{"nil", nil, "disabled", "disabled"},
		{"explicit", map[string]interface{}{"method": "disabled"}, "disabled", "disabled"},
		{"unknown", map[string]interface{}{"method": "magic"}, "magic", "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, diagnostic := applyWithDiagnostics(t, tc.filter, nil)
			if diagnostic.Method != tc.method || diagnostic.Status != tc.status {
				t.Fatalf("diagnostic = %+v, want %s/%s", diagnostic, tc.method, tc.status)
			}
		})
	}
}
