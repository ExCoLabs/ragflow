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

package models

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ragflow/internal/llmaudit"
)

// streamCapture rebuilds a streamed answer for the audit record. Guarded by
// the Stream sender's mutex.
type streamCapture struct {
	enabled   bool
	content   strings.Builder
	reasoning strings.Builder
}

func (s *streamCapture) add(content, reasoning *string) {
	if !s.enabled {
		return
	}
	if content != nil {
		s.content.WriteString(*content)
	}
	if reasoning != nil {
		s.reasoning.WriteString(*reasoning)
	}
}

// reset drops a failed attempt's partial deltas: the next attempt resends
// the whole answer.
func (s *streamCapture) reset() {
	s.content.Reset()
	s.reasoning.Reset()
}

func auditGenerate(ctx context.Context, modelName string, start time.Time, msgs []Message, cfg *ChatConfig, resp *ChatResponse, err error) {
	run := llmaudit.FromContext(ctx)
	if !run.Capturing() {
		return
	}
	call := auditCallBase(modelName, "generate", start, cfg, err)
	if resp != nil {
		out := &llmaudit.Output{ToolCalls: resp.ToolCalls}
		if resp.Answer != nil {
			out.Content = *resp.Answer
		}
		if resp.ReasonContent != nil {
			out.Reasoning = *resp.ReasonContent
		}
		call.Output = out
		call.Usage = auditUsage(resp.Usage)
	}
	run.AddCall(call, msgs, auditTools(cfg))
}

func auditStream(ctx context.Context, modelName string, start time.Time, msgs []Message, cfg *ChatConfig, streamed *streamCapture, err error) {
	run := llmaudit.FromContext(ctx)
	if !run.Capturing() {
		return
	}
	call := auditCallBase(modelName, "stream", start, cfg, err)
	out := &llmaudit.Output{Content: streamed.content.String(), Reasoning: streamed.reasoning.String()}
	if cfg != nil && cfg.ToolCallsResult != nil {
		out.ToolCalls = *cfg.ToolCallsResult
	}
	call.Output = out
	if cfg != nil {
		call.Usage = auditUsage(cfg.UsageResult)
	}
	run.AddCall(call, msgs, auditTools(cfg))
}

func auditCallBase(modelName, mode string, start time.Time, cfg *ChatConfig, err error) llmaudit.Call {
	call := llmaudit.Call{
		Model: modelName, Mode: mode, Start: start,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if cfg != nil {
		if cfg.ToolChoiceValue != nil {
			call.ToolChoice = fmt.Sprint(cfg.ToolChoiceValue)
		} else if cfg.ToolChoice != nil {
			call.ToolChoice = *cfg.ToolChoice
		}
	}
	if err != nil {
		call.Error = err.Error()
	}
	return call
}

func auditTools(cfg *ChatConfig) any {
	if cfg == nil {
		return nil
	}
	return cfg.Tools
}

func auditUsage(u *TokenUsage) *llmaudit.Usage {
	if u == nil {
		return nil
	}
	return &llmaudit.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
}
