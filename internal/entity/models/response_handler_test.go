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
	"net/http"
	"net/http/httptest"
	"ragflow/internal/common"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestModelStreamHTTPDiagnostics(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	prev := common.Logger
	common.Logger = zap.New(core)
	t.Cleanup(func() { common.Logger = prev })
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		logs.TakeAll()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("x-request-id", "req-test")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("PRIVATE_RESPONSE"))
		}))
		model := &BaseModel{httpClient: server.Client()}
		key := "PRIVATE_KEY"
		body := map[string]any{"model": "Qwen3.5", "temperature": 0.2, "messages": "PRIVATE_PROMPT"}
		wantMode := "unspecified"
		if status == http.StatusServiceUnavailable {
			body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
			wantMode = "disabled"
		}
		err := model.doStreamRequest(common.WithSessionID(t.Context(), "session-test"), server.URL, &APIConfig{ApiKey: &key},
			body, time.Second,
			func(body io.ReadCloser) error { _, err := io.Copy(io.Discard, body); return err })
		server.Close()
		if (err != nil) != (status != http.StatusOK) {
			t.Fatalf("unexpected error: %v", err)
		}
		entries := logs.FilterMessage("ModelStreamHTTP").All()
		if len(entries) != 1 {
			t.Fatalf("entries=%d", len(entries))
		}
		fields := entries[0].ContextMap()
		if fields["http_status"] != int64(status) || fields["provider_request_id"] != "req-test" || fields["session_id"] != "session-test" {
			t.Errorf("bad correlation: %v", fields)
		}
		if fields["max_tokens_sent"] != false || fields["top_p_sent"] != false || fields["temperature_sent"] != true {
			t.Errorf("wrong effective flags: %v", fields)
		}
		if fields["thinking_mode"] != wantMode || fields["thinking_control_sent"] != (wantMode == "disabled") {
			t.Errorf("wrong thinking flags: %v", fields)
		}
		for _, field := range entries[0].Context {
			if strings.Contains(field.String, "PRIVATE") {
				t.Fatal("private HTTP data logged")
			}
		}
	}
}

func TestHandleStreamingResponseDiagnostics(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	prev := common.Logger
	common.Logger = zap.New(core)
	t.Cleanup(func() { common.Logger = prev })
	for _, tc := range []struct {
		name, wire string
		wantError  bool
		fields     map[string]any
	}{
		{"reasoning_only", "data: {\"id\":\"chatcmpl-test\",\"choices\":[{\"delta\":{\"reasoning\":\"PRIVATE_REASON\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n", false,
			map[string]any{"provider_response_id": "chatcmpl-test", "finish_reason": "length", "sse_done": true, "reasoning_bytes": int64(len("PRIVATE_REASON")), "content_bytes": int64(0)}},
		{"ignored_shapes", "data: {\"id\":\"PRIVATE\\nID\",\"choices\":[{\"delta\":{\"content\":[{\"text\":\"PRIVATE_CONTENT\"}],\"tool_calls\":[{\"function\":{\"name\":\"PRIVATE_TOOL\",\"arguments\":\"PRIVATE_ARGS\"}}]},\"finish_reason\":\"PRIVATE_REASON\"}]}\n\ndata: [DONE]\n", false,
			map[string]any{"provider_response_id": "", "finish_reason": "other", "non_string_content_frames": int64(1), "ignored_tool_call_deltas": int64(1), "accepted_tool_calls": int64(0)}},
		{"missing_delta", "data: {\"choices\":[{\"message\":{\"content\":\"PRIVATE_CONTENT\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n", false,
			map[string]any{"missing_delta_frames": int64(1), "content_bytes": int64(0), "finish_reason": "stop"}},
		{"truncated", "data: {\"choices\":[{\"delta\":{\"content\":\"PRIVATE_CONTENT\"}}]}\n", true,
			map[string]any{"sse_done": false, "terminal_seen": false, "stream_error": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.TakeAll()
			err := HandleStreamingResponse(common.WithSessionID(t.Context(), "session-test"), strings.NewReader(tc.wire), nil, nil, OpenAIParserConfig, func(*string, *string) error { return nil })
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			entries := logs.FilterMessage("ModelStreamDiagnostics").All()
			if len(entries) != 1 {
				t.Fatalf("diagnostic entries = %d", len(entries))
			}
			fields := entries[0].ContextMap()
			if fields["session_id"] != "session-test" {
				t.Fatal("missing session correlation")
			}
			for key, want := range tc.fields {
				if fields[key] != want {
					t.Errorf("%s = %v, want %v", key, fields[key], want)
				}
			}
			for _, field := range entries[0].Context {
				if strings.Contains(field.String, "PRIVATE") {
					t.Errorf("private content leaked in %s", field.Key)
				}
			}
		})
	}
}

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
