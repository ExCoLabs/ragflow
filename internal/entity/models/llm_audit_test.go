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
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"ragflow/internal/llmaudit"
)

func TestEinoChatModelAuditsGenerateWithToolsAndToolResults(t *testing.T) {
	name, answer := "qwen", "Odpoveď [ID:0]"
	driver := &captureToolDriver{script: []*ChatResponse{
		{ToolCalls: []map[string]interface{}{{"id": "c1", "function": map[string]any{"name": "search", "arguments": `{"q":"x"}`}}}},
		{Answer: &answer},
	}}
	model := NewEinoChatModel(NewChatModel(driver, &name, &APIConfig{}), nil)
	bound, err := model.WithTools([]*schema.ToolInfo{{Name: "search", Desc: "Search the archive"}})
	if err != nil {
		t.Fatal(err)
	}
	run := llmaudit.NewRun("run1", "sess", "msg", "agent", "user", true)
	ctx := llmaudit.WithRun(t.Context(), run)
	msgs := []*schema.Message{schema.SystemMessage("You are ARCHA"), schema.UserMessage("question?")}
	if _, err := bound.Generate(ctx, msgs); err != nil {
		t.Fatal(err)
	}
	msgs = append(msgs, &schema.Message{Role: schema.Tool, ToolCallID: "c1", Content: "chunk text"})
	if _, err := bound.Generate(ctx, msgs); err != nil {
		t.Fatal(err)
	}

	calls := run.Snapshot().Calls
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	first, second := calls[0], calls[1]
	if !strings.Contains(string(first.Messages), "You are ARCHA") || !strings.Contains(string(first.Tools), "Search the archive") {
		t.Fatalf("first call lost system prompt or tools: %s / %s", first.Messages, first.Tools)
	}
	if len(first.Output.ToolCalls) != 1 {
		t.Fatalf("first output tool calls = %#v", first.Output)
	}
	if !strings.Contains(string(second.Messages), "chunk text") || second.Output.Content != answer {
		t.Fatalf("second call = %s / %#v", second.Messages, second.Output)
	}
}

func TestEinoChatModelAuditsStreamAndErrors(t *testing.T) {
	name, answer, think := "qwen", "streamed answer", "thinking"
	driver := &captureToolDriver{resp: &ChatResponse{Answer: &answer, ReasonContent: &think}}
	model := NewEinoChatModel(NewChatModel(driver, &name, &APIConfig{}), nil)
	run := llmaudit.NewRun("run1", "", "", "", "", true)
	ctx := llmaudit.WithRun(t.Context(), run)
	sr, err := model.Stream(ctx, []*schema.Message{schema.UserMessage("q")})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := sr.Recv(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	failing := NewEinoChatModel(NewChatModel(&failingDriver{captureToolDriver: &captureToolDriver{}, err: errors.New("quota")}, &name, &APIConfig{}), nil)
	if _, err := failing.Generate(ctx, []*schema.Message{schema.UserMessage("q")}); err == nil {
		t.Fatal("want error")
	}

	calls := run.Snapshot().Calls
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].Mode != "stream" || calls[0].Output.Content != answer || calls[0].Output.Reasoning != think {
		t.Fatalf("stream call = %#v", calls[0].Output)
	}
	if calls[1].Error != "quota" {
		t.Fatalf("error call = %q", calls[1].Error)
	}
}

func TestEinoChatModelSkipsAuditWhenNotCapturing(t *testing.T) {
	name, answer := "qwen", "a"
	model := NewEinoChatModel(NewChatModel(&captureToolDriver{resp: &ChatResponse{Answer: &answer}}, &name, &APIConfig{}), nil)
	run := llmaudit.NewRun("run1", "", "", "", "", false)
	if _, err := model.Generate(llmaudit.WithRun(t.Context(), run), []*schema.Message{schema.UserMessage("q")}); err != nil {
		t.Fatal(err)
	}
	if n := len(run.Snapshot().Calls); n != 0 {
		t.Fatalf("calls = %d, want 0", n)
	}
}
