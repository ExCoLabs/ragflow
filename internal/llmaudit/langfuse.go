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

// Package llmaudit records one agent run for audit and debugging: the
// question, every LLM request exactly as sent to the driver, every response,
// and the answer the user received. It is a leaf package so the model layer
// can record calls without importing the agent or service layers.
package llmaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// LangfuseExporter exports over OTLP/HTTP: Langfuse v4 in its default
// events_only write mode rejects trace-create/generation-create on
// /api/public/ingestion (HTTP 207, per-event 400).
type LangfuseExporter struct {
	host   string
	auth   string
	client *http.Client
}

// NewLangfuseExporter returns nil unless host and both keys are set.
func NewLangfuseExporter(host, publicKey, secretKey string, client *http.Client) *LangfuseExporter {
	host = strings.TrimRight(host, "/")
	if host == "" || publicKey == "" || secretKey == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(publicKey+":"+secretKey))
	return &LangfuseExporter{host: host, auth: auth, client: client}
}

// One span per request keeps a single large agent round from failing the
// whole trace; bodies above this lose their input (still in agent_run_audit).
const langfuseMaxSpanBytes = 4 << 20

type otlpAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

type otlpSpan struct {
	TraceID      string         `json:"traceId"`
	SpanID       string         `json:"spanId"`
	ParentSpanID string         `json:"parentSpanId,omitempty"`
	Name         string         `json:"name"`
	Kind         int            `json:"kind"`
	Start        string         `json:"startTimeUnixNano"`
	End          string         `json:"endTimeUnixNano"`
	Attributes   []otlpAttr     `json:"attributes"`
	Status       map[string]any `json:"status,omitempty"`
}

func strAttr(k, v string) otlpAttr { return otlpAttr{Key: k, Value: map[string]any{"stringValue": v}} }

func jsonAttr(k string, v any) otlpAttr {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	return strAttr(k, string(b))
}

func unixNano(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

// spanID derives a stable 16-hex id so a re-sent run overwrites, not duplicates.
func spanID(runID string, n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", runID, n)))
	return hex.EncodeToString(sum[:8])
}

func (l *LangfuseExporter) Send(ctx context.Context, s Snapshot) error {
	t := s.Totals()
	rootID := spanID(s.ID, -1)
	root := otlpSpan{
		TraceID: s.ID, SpanID: rootID, Name: "agent " + s.AgentID, Kind: 1,
		Start: unixNano(s.Started), End: unixNano(s.Ended),
		Attributes: []otlpAttr{
			strAttr("langfuse.trace.name", "agent "+s.AgentID),
			strAttr("langfuse.session.id", s.SessionID),
			strAttr("langfuse.user.id", s.UserID),
			strAttr("langfuse.trace.input", s.Question),
			strAttr("langfuse.trace.output", s.Answer),
			{Key: "langfuse.trace.tags", Value: map[string]any{"arrayValue": map[string]any{"values": []map[string]any{
				{"stringValue": "agent"}, {"stringValue": s.Outcome}}}}},
			jsonAttr("langfuse.trace.metadata", map[string]any{
				"outcome": s.Outcome, "message_id": s.MessageID, "agent_id": s.AgentID, "tenant_id": s.TenantID,
				"total_ms": t.TotalMS, "llm_ms": t.LLMMS, "tools_ms": t.ToolsMS, "grounding_ms": t.GroundingMS,
				"llm_calls": t.LLMCalls, "tool_calls": t.ToolCalls, "agents": s.Agents,
				"truncated": s.Truncated, "error": s.Error,
			}),
		},
	}
	if s.Error != "" {
		root.Status = map[string]any{"code": 2, "message": s.Error}
	}
	spans := []otlpSpan{root}
	for i, c := range s.Calls {
		end := c.Start.Add(time.Duration(c.DurationMS) * time.Millisecond)
		sp := otlpSpan{
			TraceID: s.ID, SpanID: spanID(s.ID, i), ParentSpanID: rootID,
			Name: fmt.Sprintf("llm %d (%s)", i+1, c.Mode), Kind: 3,
			Start: unixNano(c.Start), End: unixNano(end),
			Attributes: []otlpAttr{
				strAttr("langfuse.observation.type", "generation"),
				strAttr("langfuse.observation.model.name", c.Model),
				jsonAttr("langfuse.observation.input", map[string]any{"messages": c.Messages, "tools": c.Tools, "tool_choice": c.ToolChoice}),
				jsonAttr("langfuse.observation.output", c.Output),
				jsonAttr("langfuse.observation.metadata", map[string]any{"truncated": c.Truncated, "input_bytes": c.InputBytes}),
			},
		}
		if c.Usage != nil {
			sp.Attributes = append(sp.Attributes, jsonAttr("langfuse.observation.usage_details",
				map[string]int{"input": c.Usage.PromptTokens, "output": c.Usage.CompletionTokens, "total": c.Usage.TotalTokens}))
		}
		if c.Error != "" {
			sp.Attributes = append(sp.Attributes,
				strAttr("langfuse.observation.level", "ERROR"),
				strAttr("langfuse.observation.status_message", c.Error))
			sp.Status = map[string]any{"code": 2, "message": c.Error}
		}
		spans = append(spans, sp)
	}
	var firstErr error
	for _, sp := range spans {
		if err := l.post(ctx, sp); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("span %s: %w", sp.Name, err)
		}
	}
	return firstErr
}

func otlpBody(sp otlpSpan) ([]byte, error) {
	return json.Marshal(map[string]any{"resourceSpans": []map[string]any{{
		"resource":   map[string]any{"attributes": []otlpAttr{strAttr("service.name", "ragflow")}},
		"scopeSpans": []map[string]any{{"scope": map[string]any{"name": "ragflow.agent_audit"}, "spans": []otlpSpan{sp}}},
	}}})
}

func (l *LangfuseExporter) post(ctx context.Context, sp otlpSpan) error {
	body, err := otlpBody(sp)
	if err != nil {
		return err
	}
	if len(body) > langfuseMaxSpanBytes {
		for i, a := range sp.Attributes {
			if a.Key == "langfuse.observation.input" || a.Key == "langfuse.trace.input" {
				sp.Attributes[i] = strAttr(a.Key, fmt.Sprintf("omitted: %d bytes; full input in agent_run_audit.calls", len(body)))
			}
		}
		if body, err = otlpBody(sp); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.host+"/api/public/otel/v1/traces", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", l.auth)
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, respBody)
	}
	// OTLP reports per-span rejections in a 200 body.
	var result struct {
		PartialSuccess struct {
			RejectedSpans json.Number `json:"rejectedSpans"`
			ErrorMessage  string      `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(respBody, &result) == nil && result.PartialSuccess.RejectedSpans != "" && result.PartialSuccess.RejectedSpans != "0" {
		return fmt.Errorf("rejected %s spans: %s", result.PartialSuccess.RejectedSpans, result.PartialSuccess.ErrorMessage)
	}
	return nil
}
