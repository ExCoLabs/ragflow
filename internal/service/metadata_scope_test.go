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
	"strings"
	"testing"

	"ragflow/internal/common"
	modelModule "ragflow/internal/entity/models"
)

// metaFilterDriver answers GenMetaFilter with a fixed JSON reply and records
// the system prompt it was given.
type metaFilterDriver struct {
	*modelModule.DummyModel
	reply  string
	prompt string
	calls  int
}

func (d *metaFilterDriver) ChatWithMessages(ctx context.Context, modelName string, messages []modelModule.Message, apiConfig *modelModule.APIConfig, cfg *modelModule.ChatConfig, usage *common.ModelUsage) (*modelModule.ChatResponse, error) {
	d.calls++
	d.prompt, _ = messages[0].Content.(string)
	return &modelModule.ChatResponse{Answer: &d.reply}, nil
}

func newMetaFilterModel(reply string) (*modelModule.ChatModel, *metaFilterDriver) {
	name := "fake"
	driver := &metaFilterDriver{DummyModel: modelModule.NewDummyModel(nil, modelModule.URLSuffix{}), reply: reply}
	return &modelModule.ChatModel{ModelDriver: driver, ModelName: &name, APIConfig: &modelModule.APIConfig{}}, driver
}

func scopeTestMetadata() common.MetaData {
	return common.MetaData{
		"dept": {"hr": {"d1"}, "it": {"d2"}},
		"year": {"2023": {"d1"}, "2024": {"d2"}},
		"tag":  {"x": {"d1", "d2"}},
	}
}

var activeChunkMeta = &common.ChunkMetadataConfig{Enabled: true, Ready: true, Fields: []string{"dept", "year"}}

func TestApplyMetaDataScopeInactiveIsDocIDPath(t *testing.T) {
	filter := map[string]interface{}{
		"method": "manual",
		"manual": []interface{}{map[string]interface{}{"key": "dept", "op": "=", "value": "hr"}},
		"boost":  map[string]interface{}{"method": "manual", "manual": []interface{}{map[string]interface{}{"key": "year", "op": "max"}}},
	}
	for _, cfg := range []*common.ChunkMetadataConfig{nil, {Enabled: true, Fields: []string{"dept"}}} {
		scope := ApplyMetaDataScope(t.Context(), filter, scopeTestMetadata(), "q", nil, nil, nil, cfg)
		if !reflect.DeepEqual(scope.DocIDs, []string{"d1"}) || scope.ChunkMeta != nil {
			t.Fatalf("cfg %+v: scope = %+v, want the doc-id path with no boost", cfg, scope)
		}
	}
}

func TestApplyMetaDataScopeManualOnChunkFields(t *testing.T) {
	filter := map[string]interface{}{
		"method": "manual",
		"logic":  "or",
		"manual": []interface{}{
			map[string]interface{}{"key": "dept", "op": "=", "value": "hr"},
			map[string]interface{}{"key": "year", "op": ">=", "value": "2024"},
		},
		"boost": map[string]interface{}{
			"method":    "manual",
			"manual":    []interface{}{map[string]interface{}{"key": "year", "op": "max", "weight": 0.1}},
			"max_total": 0.2,
		},
	}
	scope := ApplyMetaDataScope(t.Context(), filter, scopeTestMetadata(), "q", nil, []string{"d9"}, nil, activeChunkMeta)
	if !reflect.DeepEqual(scope.DocIDs, []string{"d9"}) || scope.NoMatch {
		t.Fatalf("a chunk-field filter keeps the base doc ids: %+v", scope)
	}
	cm := scope.ChunkMeta
	if cm == nil || cm.Filter == nil || cm.Filter.Logic != "or" || cm.Filter.Conditions[1]["op"] != "≥" {
		t.Fatalf("chunk filter = %+v", cm)
	}
	if len(cm.Boosts) != 1 || cm.Boosts[0].Op != "max" || cm.BoostMaxTotal != 0.2 {
		t.Fatalf("boosts = %+v", cm)
	}
}

func TestApplyMetaDataScopeManualFallsBackToDocIDs(t *testing.T) {
	filter := map[string]interface{}{
		"method": "manual",
		"manual": []interface{}{map[string]interface{}{"key": "tag", "op": "=", "value": "nope"}},
		"boost":  map[string]interface{}{"method": "off", "manual": []interface{}{map[string]interface{}{"key": "year", "op": "max"}}},
	}
	scope := ApplyMetaDataScope(t.Context(), filter, scopeTestMetadata(), "q", nil, nil, nil, activeChunkMeta)
	if !reflect.DeepEqual(scope.DocIDs, []string{NoMatchDocIDSentinel}) {
		t.Fatalf("a key outside the whitelist uses doc ids; no match is the sentinel: %+v", scope)
	}
	if scope.ChunkMeta != nil {
		t.Fatalf("boost method off must drop the fixed preferences: %+v", scope.ChunkMeta)
	}
}

func TestApplyMetaDataScopeAutoSplitsHardAndSoft(t *testing.T) {
	model, driver := newMetaFilterModel(`{"logic":"and","conditions":[
		{"key":"dept","value":"hr","op":"=","strength":"hard"},
		{"key":"year","value":"2024","op":"=","strength":"soft"},
		{"key":"tag","value":"x","op":"=","strength":"soft"}]}`)
	filter := map[string]interface{}{"method": "auto", "boost": map[string]interface{}{"method": "auto", "auto_weight": 0.05}}
	scope := ApplyMetaDataScope(t.Context(), filter, scopeTestMetadata(), "hr reports, preferably 2024", model, nil, nil, activeChunkMeta)
	if driver.calls != 1 {
		t.Fatalf("filter and boost must share one LLM call, got %d", driver.calls)
	}
	if !strings.Contains(driver.prompt, `"strength"`) {
		t.Fatal("auto boost must ask the model for hard/soft strength")
	}
	cm := scope.ChunkMeta
	if cm == nil || cm.Filter == nil || len(cm.Filter.Conditions) != 1 || cm.Filter.Conditions[0]["key"] != "dept" {
		t.Fatalf("the hard condition must filter on chunk fields: %+v", cm)
	}
	want := []common.ChunkMetaBoost{{Key: "year", Op: "=", Value: "2024", Weight: 0.05}}
	if !reflect.DeepEqual(cm.Boosts, want) {
		t.Fatalf("boosts = %+v, want %+v (soft on a non-whitelisted key is dropped)", cm.Boosts, want)
	}
}

func TestApplyMetaDataScopeSemiAutoBoostKeys(t *testing.T) {
	model, driver := newMetaFilterModel(`{"logic":"and","conditions":[
		{"key":"dept","value":"it","op":"="},
		{"key":"year","value":"2023","op":"≥"}]}`)
	filter := map[string]interface{}{
		"method":    "semi_auto",
		"semi_auto": []interface{}{"dept"},
		"boost": map[string]interface{}{
			"method":    "semi_auto",
			"semi_auto": []interface{}{map[string]interface{}{"key": "year", "op": "≥", "weight": 0.2}, "dept"},
		},
	}
	scope := ApplyMetaDataScope(t.Context(), filter, scopeTestMetadata(), "it since 2023", model, nil, nil, activeChunkMeta)
	if strings.Contains(driver.prompt, `"strength"`) {
		t.Fatal("semi_auto boost must not ask for strength")
	}
	if !strings.Contains(driver.prompt, `"year":"≥"`) {
		t.Fatalf("pinned boost operator must reach the prompt constraints:\n%s", driver.prompt)
	}
	cm := scope.ChunkMeta
	if cm == nil || cm.Filter == nil || cm.Filter.Conditions[0]["key"] != "dept" {
		t.Fatalf("a key listed by the filter stays a filter: %+v", cm)
	}
	if len(cm.Boosts) != 1 || cm.Boosts[0].Key != "year" || cm.Boosts[0].Weight != 0.2 {
		t.Fatalf("boosts = %+v", cm.Boosts)
	}
}

func TestApplyMetaDataScopeLLMFilterMatchedNothing(t *testing.T) {
	model, _ := newMetaFilterModel(`{"logic":"and","conditions":[{"key":"tag","value":"zzz","op":"="}]}`)
	filter := map[string]interface{}{"method": "auto"}
	scope := ApplyMetaDataScope(t.Context(), filter, scopeTestMetadata(), "q", model, nil, nil, activeChunkMeta)
	if scope.DocIDs != nil || !scope.NoMatch || scope.ChunkMeta != nil {
		t.Fatalf("an LLM filter that matches nothing means no filter: %+v", scope)
	}
}

func TestMetaFilterNeedsLLM(t *testing.T) {
	cases := map[string]struct {
		filter map[string]interface{}
		want   bool
	}{
		"nil":        {nil, false},
		"manual":     {map[string]interface{}{"method": "manual"}, false},
		"auto":       {map[string]interface{}{"method": "auto"}, true},
		"boost only": {map[string]interface{}{"method": "manual", "boost": map[string]interface{}{"method": "semi_auto"}}, true},
	}
	for name, tc := range cases {
		if got := MetaFilterNeedsLLM(tc.filter); got != tc.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestMetadataConditionAndBoostToChunkMeta(t *testing.T) {
	cond := map[string]interface{}{"logic": "and", "conditions": []interface{}{
		map[string]interface{}{"name": "dept", "comparison_operator": "is", "value": "hr"},
	}}
	if got := MetadataConditionToChunkMeta(cond, activeChunkMeta); got == nil || got.Filter.Conditions[0]["op"] != "=" {
		t.Fatalf("metadata_condition on whitelisted keys goes to chunk fields: %+v", got)
	}
	if MetadataConditionToChunkMeta(cond, nil) != nil {
		t.Fatal("inactive chunk metadata keeps the doc-id path")
	}
	boost := MetadataBoostToChunkMeta(map[string]interface{}{
		"manual":    []interface{}{map[string]interface{}{"key": "year", "op": "=", "value": "2024", "weight": 0.1}},
		"max_total": 0.15,
	}, activeChunkMeta)
	if boost == nil || len(boost.Boosts) != 1 || boost.BoostMaxTotal != 0.15 {
		t.Fatalf("boost = %+v", boost)
	}
	if MetadataBoostToChunkMeta([]interface{}{map[string]interface{}{"key": "year", "op": "max"}}, nil) != nil {
		t.Fatal("a boost is ignored without active chunk metadata")
	}
}

func TestRenderMetaFilterTemplateAllowSoft(t *testing.T) {
	withSoft, err := renderMetaFilterTemplate("2026-01-01", `{"a":["b"]}`, "q", "", MetaFilterPromptOptions{AllowSoft: true})
	if err != nil {
		t.Fatal(err)
	}
	without, err := renderMetaFilterTemplate("2026-01-01", `{"a":["b"]}`, "q", "", MetaFilterPromptOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withSoft, "Requirement vs. preference") || !strings.Contains(withSoft, `"strength": {`) {
		t.Fatal("allow_soft blocks must be rendered")
	}
	if strings.Contains(without, "strength") || strings.Contains(without, "{%") {
		t.Fatal("without allow_soft no strength and no template tags may remain")
	}
}
