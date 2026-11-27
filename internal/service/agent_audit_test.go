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
	"time"

	"ragflow/internal/agent/canvas"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
)

func TestAgentRunWritesAuditRow(t *testing.T) {
	testDB := setupServiceTestDB(t)
	if err := testDB.AutoMigrate(&entity.API4Conversation{}, &entity.API4ConversationMessage{}, &entity.API4ConversationReference{}, &entity.AgentRunAudit{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	originalDB, originalCfg := dao.DB, agentAudit
	dao.DB = testDB
	agentAudit = func() agentAuditConfig { return agentAuditConfig{mysql: true} }
	t.Cleanup(func() { dao.DB, agentAudit = originalDB, originalCfg })

	if err := dao.NewAPI4ConversationDAO().Create(t.Context(), testDB, &entity.API4Conversation{
		ID: "session-audit", DialogID: "canvas-audit", UserID: "user-1",
		Message: json.RawMessage(`[]`), Reference: json.RawMessage(`[]`),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	svc := NewAgentService()
	if err := svc.persistAgentRunQuestion(t.Context(), "canvas-audit", "user-1", "session-audit", "message-1", "world", 1); err != nil {
		t.Fatalf("persist question: %v", err)
	}
	dsl := map[string]any{
		"components": map[string]any{
			"begin_0": map[string]any{
				"obj":        map[string]any{"component_name": "Begin", "params": map[string]any{}},
				"downstream": []any{"message_0"},
			},
			"message_0": map[string]any{
				"obj":      map[string]any{"component_name": "Message", "params": map[string]any{"text": "hello {{sys.query}}"}},
				"upstream": []any{"begin_0"},
			},
		},
		"path": []any{"begin_0", "message_0"},
	}
	events := make(chan canvas.RunEvent, 32)
	if _, err := svc.buildRunFunc("canvas-audit", nil, dsl)(context.Background(), map[string]any{
		"__events__":     events,
		"__message_id__": "message-1",
		"__session_id__": "session-audit",
		"user_id":        "user-1",
		"tenant_id":      "tenant-1",
		"user_input":     "world",
	}); err != nil {
		t.Fatalf("run: %v", err)
	}

	var row entity.AgentRunAudit
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := testDB.Where("session_id = ?", "session-audit").First(&row).Error
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit row not written: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(row.ID) != 32 || row.MessageID != "message-1" || row.AgentID != "canvas-audit" || row.UserID != "user-1" || row.TenantID != "tenant-1" {
		t.Fatalf("ids = %+v", row)
	}
	if row.Question != "world" || row.Answer != "hello world" || row.Outcome != "ok" {
		t.Fatalf("question/answer/outcome = %q / %q / %q", row.Question, row.Answer, row.Outcome)
	}
	if row.CreateTime == nil || *row.CreateTime <= 0 {
		t.Fatal("create_time not set")
	}
}
