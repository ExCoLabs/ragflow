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
	"bytes"
	"io"
	"ragflow/internal/common"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestHandleStreamingResponseBareRootFinishReason verifies the shared handler
// terminates the stream when an event carries only a root-level finish_reason
// with no choices and no trailing [DONE]. Before the fix this event was
// skipped by the choices guard and the handler returned "stream ended before
// [DONE] or finish_reason".
func TestHandleStreamingResponseBareRootFinishReason(t *testing.T) {
	sse := "data: {\"finish_reason\":\"stop\"}\n"

	var contentChunks, reasonChunks []string
	err := HandleStreamingResponse(
		t.Context(),
		io.NopCloser(bytes.NewBufferString(sse)),
		nil,
		nil,
		OpenAIParserConfig,
		func(content *string, reason *string) error {
			if content != nil && *content != "" {
				contentChunks = append(contentChunks, *content)
			}
			if reason != nil && *reason != "" {
				reasonChunks = append(reasonChunks, *reason)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("HandleStreamingResponse: %v", err)
	}

	// The terminal [DONE] marker is the only content the handler emits for a
	// finish_reason-only event: no tokens arrived and no reasoning was sent.
	if got := strings.Join(contentChunks, ""); got != "[DONE]" {
		t.Errorf("content=%q, want only the [DONE] marker", got)
	}
	if len(reasonChunks) != 0 {
		t.Errorf("reasoning=%q, want none for a finish_reason-only event", strings.Join(reasonChunks, ""))
	}
}

// TestHandleStreamingResponseReasoningAlias verifies the shared OpenAI
// parser surfaces reasoning emitted under delta.reasoning (OpenRouter,
// OVH AI Endpoints) as well as delta.reasoning_content.
func TestHandleStreamingResponseReasoningAlias(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"reasoning\":\"step 1. \"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step 2. \"}}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n"

	var contentChunks, reasonChunks []string
	err := HandleStreamingResponse(
		t.Context(),
		io.NopCloser(bytes.NewBufferString(sse)),
		nil,
		nil,
		OpenAIParserConfig,
		func(content *string, reason *string) error {
			if content != nil && *content != "" {
				contentChunks = append(contentChunks, *content)
			}
			if reason != nil && *reason != "" {
				reasonChunks = append(reasonChunks, *reason)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("HandleStreamingResponse: %v", err)
	}
	if got := strings.Join(reasonChunks, ""); got != "step 1. step 2. " {
		t.Errorf("reasoning=%q, want both aliases collected", got)
	}
	if got := strings.Join(contentChunks, ""); got != "answer[DONE]" {
		t.Errorf("content=%q, want answer + [DONE]", got)
	}
}

// TestHandleStreamingResponseStreamUsageCarriesSessionID verifies the
// StreamUsage log line inherits the session correlation id from ctx, so an
// agent run's provider usage can be reconstructed by session.
func TestHandleStreamingResponseStreamUsageCarriesSessionID(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	prev := common.Logger
	common.Logger = zap.New(core)
	t.Cleanup(func() { common.Logger = prev })

	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n" +
		"data: [DONE]\n"

	err := HandleStreamingResponse(
		common.WithSessionID(t.Context(), "sess-123"),
		io.NopCloser(bytes.NewBufferString(sse)),
		nil,
		&ChatConfig{},
		OpenAIParserConfig,
		func(*string, *string) error { return nil },
	)
	if err != nil {
		t.Fatalf("HandleStreamingResponse: %v", err)
	}

	var usageEntry *observer.LoggedEntry
	for i, e := range logs.All() {
		if e.Message == "StreamUsage" {
			entry := logs.All()[i]
			usageEntry = &entry
		}
	}
	if usageEntry == nil {
		t.Fatal("StreamUsage log entry not found")
	}
	found := false
	for _, f := range usageEntry.Context {
		if f.Key == "session_id" && f.String == "sess-123" {
			found = true
		}
	}
	if !found {
		t.Errorf("StreamUsage missing session_id=sess-123, fields=%v", usageEntry.Context)
	}
}

// TestHandleStreamingResponseTruncatedStream verifies a stream that ends
// without [DONE], a root-level finish_reason, or a per-choice finish_reason
// is rejected as truncated. This is the counterpart of the bare
// finish_reason case: only a genuine truncation must fail.
func TestHandleStreamingResponseTruncatedStream(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n"

	err := HandleStreamingResponse(
		t.Context(),
		io.NopCloser(bytes.NewBufferString(sse)),
		nil,
		nil,
		OpenAIParserConfig,
		func(*string, *string) error { return nil },
	)
	if err == nil || !strings.Contains(err.Error(), "stream ended before [DONE] or finish_reason") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}
