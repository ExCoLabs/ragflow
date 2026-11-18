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
	"fmt"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	"ragflow/internal/llmaudit"
)

// Agent run audit. RAGFLOW_AGENT_AUDIT=1 stores every agent turn in
// agent_run_audit; RAGFLOW_AGENT_AUDIT_LANGFUSE_{HOST,PUBLIC_KEY,SECRET_KEY}
// also export it to one Langfuse project. Unlike the tenant Langfuse setting
// (chat completions only) this covers agent runs on every model.
type agentAuditConfig struct {
	mysql    bool
	langfuse *llmaudit.LangfuseExporter
}

var agentAudit = sync.OnceValue(func() agentAuditConfig {
	cfg := agentAuditConfig{mysql: os.Getenv("RAGFLOW_AGENT_AUDIT") == "1"}
	cfg.langfuse = llmaudit.NewLangfuseExporter(os.Getenv("RAGFLOW_AGENT_AUDIT_LANGFUSE_HOST"),
		os.Getenv("RAGFLOW_AGENT_AUDIT_LANGFUSE_PUBLIC_KEY"), os.Getenv("RAGFLOW_AGENT_AUDIT_LANGFUSE_SECRET_KEY"), nil)
	if cfg.mysql || cfg.langfuse != nil {
		common.Info(fmt.Sprintf("agent_audit enabled mysql=%t langfuse=%t", cfg.mysql, cfg.langfuse != nil))
	}
	return cfg
})

func (c agentAuditConfig) capturing() bool { return c.mysql || c.langfuse != nil }

// startAgentAudit attaches a run to ctx even with auditing off: the run id is
// what ties agent_run_timings log lines to one turn.
func startAgentAudit(ctx context.Context, sessionID, messageID, agentID, userID string) (context.Context, *llmaudit.Run) {
	run := llmaudit.NewRun(common.GenerateUUID(), sessionID, messageID, agentID, userID, agentAudit().capturing())
	return llmaudit.WithRun(ctx, run), run
}

// finishAgentAudit writes the run in the background so the user's answer is
// never delayed by the audit sinks.
func finishAgentAudit(ctx context.Context, run *llmaudit.Run, runErr error, interrupted bool) {
	run.Finish(runErr, interrupted)
	cfg := agentAudit()
	if !run.Capturing() {
		return
	}
	snap := run.Snapshot()
	// Detached: a run ending on its 600 s deadline or a client disconnect
	// has an already-cancelled ctx, and those are the runs worth auditing.
	bg := context.WithoutCancel(ctx)
	go func() {
		writeCtx, cancel := context.WithTimeout(bg, 2*time.Minute)
		defer cancel()
		if cfg.mysql {
			if err := insertAgentRunAudit(writeCtx, snap); err != nil {
				common.Warn("agent_audit mysql insert failed", zap.String("run", snap.ID), zap.Error(err))
			}
		}
		if cfg.langfuse != nil {
			if err := cfg.langfuse.Send(writeCtx, snap); err != nil {
				common.Warn("agent_audit langfuse export failed", zap.String("run", snap.ID), zap.Error(err))
			}
		}
	}()
}

func insertAgentRunAudit(ctx context.Context, s llmaudit.Snapshot) error {
	if dao.DB == nil {
		return fmt.Errorf("no database")
	}
	calls, err := json.Marshal(s.Calls)
	if err != nil {
		return err
	}
	agents, _ := json.Marshal(s.Agents)
	t := s.Totals()
	created := s.Started.UnixMilli()
	createdDate := s.Started.Local().Truncate(time.Second)
	row := &entity.AgentRunAudit{
		ID: s.ID, SessionID: s.SessionID, MessageID: s.MessageID, AgentID: s.AgentID,
		TenantID: s.TenantID, UserID: s.UserID,
		Question: s.Question, Answer: s.Answer, Outcome: s.Outcome, Models: truncateRunes(s.Models(), 512),
		TotalMS: t.TotalMS, LLMMS: t.LLMMS, ToolsMS: t.ToolsMS, GroundingMS: t.GroundingMS,
		LLMCalls: t.LLMCalls, ToolCalls: t.ToolCalls,
		Agents: string(agents), Calls: string(calls), CallsBytes: int64(len(calls)), Truncated: s.Truncated,
		BaseModel: entity.BaseModel{CreateTime: &created, CreateDate: &createdDate},
	}
	if s.Error != "" {
		row.Error = &s.Error
	}
	return dao.DB.WithContext(ctx).Create(row).Error
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
