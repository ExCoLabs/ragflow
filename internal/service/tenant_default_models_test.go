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
	"encoding/json"
	"testing"

	"ragflow/internal/entity"
	"ragflow/internal/server/config"
)

func boolPtr(b bool) *bool { return &b }

func TestProvisionDefaultModels(t *testing.T) {
	db := setupSharedTestDB(t)
	user := createTestAccount(t, db, "dave@example.com")

	const factory = "OpenAI-API-Compatible"
	cfg := config.UserDefaultLLMConfig{
		Factory: factory, APIKey: "key-1", BaseURL: "https://llm.example.com/v1", InstanceName: "main",
		Models: []config.UserDefaultLLMModel{
			{Type: "chat", Name: "big-chat", Factory: factory, APIKey: "key-1", BaseURL: "https://llm.example.com/v1", MaxTokens: 131072, IsTools: boolPtr(true), Default: true},
			{Type: "embedding", Name: "embed-1", Factory: factory, APIKey: "key-1", BaseURL: "https://llm.example.com/v1", Default: true},
			{Type: "image2text", Name: "big-chat", Factory: factory, APIKey: "key-1", BaseURL: "https://llm.example.com/v1", Default: true},
			{Type: "rerank", Name: "rerank-1", Factory: factory, APIKey: "key-2", BaseURL: "https://rerank.example.com", Default: true},
			{Type: "chat", Name: "small-chat", Factory: factory, APIKey: "key-1", BaseURL: "https://llm.example.com/v1"},
		},
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := ProvisionDefaultModels(context.Background(), user.ID, cfg); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	var providers []entity.TenantModelProvider
	db.Where("tenant_id = ?", user.ID).Find(&providers)
	if len(providers) != 1 || providers[0].ProviderName != factory {
		t.Fatalf("providers = %+v", providers)
	}
	var instances []entity.TenantModelInstance
	db.Where("provider_id = ?", providers[0].ID).Order("instance_name").Find(&instances)
	if len(instances) != 2 || instances[0].InstanceName != "main" || instances[1].InstanceName != "main-rerank" ||
		instances[0].APIKey != "key-1" || instances[1].APIKey != "key-2" {
		t.Fatalf("instances = %+v", instances)
	}
	var models []entity.TenantModel
	db.Where("instance_id = ?", instances[0].ID).Find(&models)
	byName := map[string]entity.TenantModel{}
	for _, m := range models {
		byName[m.ModelName] = m
	}
	if len(models) != 3 {
		t.Fatalf("models of main = %+v", models)
	}
	bigChat := entity.ModelType(byName["big-chat"].ModelType)
	if !bigChat.Has(entity.ModelTypeChat) || !bigChat.Has(entity.ModelTypeImage2Text) {
		t.Errorf("big-chat types = %d, want chat|image2text", byName["big-chat"].ModelType)
	}
	var extra map[string]any
	_ = json.Unmarshal([]byte(byName["big-chat"].Extra), &extra)
	if extra["is_tools"] != true || extra["max_tokens"] != float64(131072) {
		t.Errorf("big-chat extra = %v", extra)
	}

	var tenant entity.Tenant
	if err := db.First(&tenant, "id = ?", user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if tenant.LLMID != "big-chat@main@"+factory || tenant.EmbdID != "embed-1@main@"+factory ||
		tenant.Img2TxtID != "big-chat@main@"+factory || tenant.RerankID != "rerank-1@main-rerank@"+factory {
		t.Errorf("tenant defaults = llm %q embd %q img2txt %q rerank %q", tenant.LLMID, tenant.EmbdID, tenant.Img2TxtID, tenant.RerankID)
	}
	if tenant.TenantLLMID == nil || *tenant.TenantLLMID != byName["big-chat"].ID {
		t.Errorf("tenant_llm_id = %v, want %s", tenant.TenantLLMID, byName["big-chat"].ID)
	}
}
