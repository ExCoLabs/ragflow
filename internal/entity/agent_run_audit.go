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

package entity

// AgentRunAudit is one agent turn kept for audit/debugging: the question, the
// answer the user saw, and every LLM request/response of the turn (Calls, a
// JSON array). ID matches run= in the agent_run_timings log line.
type AgentRunAudit struct {
	ID          string  `gorm:"column:id;primaryKey;size:32" json:"id"`
	SessionID   string  `gorm:"column:session_id;size:32;index" json:"session_id"`
	MessageID   string  `gorm:"column:message_id;size:64" json:"message_id"`
	AgentID     string  `gorm:"column:agent_id;size:32;index" json:"agent_id"`
	TenantID    string  `gorm:"column:tenant_id;size:32;index" json:"tenant_id"`
	UserID      string  `gorm:"column:user_id;size:255;index" json:"user_id"`
	Question    string  `gorm:"column:question;type:longtext" json:"question"`
	Answer      string  `gorm:"column:answer;type:longtext" json:"answer"`
	Outcome     string  `gorm:"column:outcome;size:32;index" json:"outcome"`
	Models      string  `gorm:"column:models;size:512" json:"models"`
	TotalMS     int64   `gorm:"column:total_ms" json:"total_ms"`
	LLMMS       int64   `gorm:"column:llm_ms" json:"llm_ms"`
	ToolsMS     int64   `gorm:"column:tools_ms" json:"tools_ms"`
	GroundingMS int64   `gorm:"column:grounding_ms" json:"grounding_ms"`
	LLMCalls    int     `gorm:"column:llm_calls" json:"llm_calls"`
	ToolCalls   int     `gorm:"column:tool_calls" json:"tool_calls"`
	Agents      string  `gorm:"column:agents;type:text" json:"agents"`
	Calls       string  `gorm:"column:calls;type:longtext" json:"calls"`
	CallsBytes  int64   `gorm:"column:calls_bytes" json:"calls_bytes"`
	Truncated   bool    `gorm:"column:truncated" json:"truncated"`
	Error       *string `gorm:"column:error;type:text" json:"error,omitempty"`
	BaseModel
}

func (AgentRunAudit) TableName() string {
	return "agent_run_audit"
}
