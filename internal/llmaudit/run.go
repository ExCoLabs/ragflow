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
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

// MaxRunBytes caps the serialized calls of one run. Past it, further calls are
// kept as metadata-only stubs so a runaway loop cannot produce a row that
// MySQL (max_allowed_packet) or Langfuse refuses.
const MaxRunBytes = 16 << 20

// Call is one request/response pair with a chat model.
type Call struct {
	Model      string          `json:"model"`
	Mode       string          `json:"mode"`
	Start      time.Time       `json:"start"`
	DurationMS int64           `json:"duration_ms"`
	Messages   json.RawMessage `json:"input,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice string          `json:"tool_choice,omitempty"`
	Output     *Output         `json:"output,omitempty"`
	Usage      *Usage          `json:"usage,omitempty"`
	Error      string          `json:"error,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
	InputBytes int             `json:"input_bytes,omitempty"`
}

// Output is what the model returned for one call.
type Output struct {
	Content   string           `json:"content,omitempty"`
	Reasoning string           `json:"reasoning,omitempty"`
	ToolCalls []map[string]any `json:"tool_calls,omitempty"`
}

// Usage is the provider-reported token split of one call.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// AgentTiming is the per-Agent-component breakdown also logged as agent_run_timings.
type AgentTiming struct {
	Outcome         string `json:"outcome"`
	Model           string `json:"model"`
	TotalMS         int64  `json:"total_ms"`
	LLMMS           int64  `json:"llm_ms"`
	ToolsMS         int64  `json:"tools_ms"`
	GroundingMS     int64  `json:"grounding_ms"`
	LLMCalls        int    `json:"llm_calls"`
	ToolCalls       int    `json:"tool_calls"`
	GroundingStatus string `json:"grounding_status,omitempty"`
}

// Run accumulates one agent turn. All methods are nil-safe so call sites need
// no guard when auditing is not installed on the context.
type Run struct {
	ID        string
	SessionID string
	MessageID string
	AgentID   string
	UserID    string
	Started   time.Time

	capture bool

	mu        sync.Mutex
	tenantID  string
	question  string
	answer    string
	hasAnswer bool
	outcome   string
	errText   string
	ended     time.Time
	calls     []Call
	bytes     int
	truncated bool
	agents    []AgentTiming
}

// NewRun starts a run. capture=false keeps only ids and timings, which still
// lets the timing log line carry the run id for correlation.
func NewRun(id, sessionID, messageID, agentID, userID string, capture bool) *Run {
	return &Run{
		ID: id, SessionID: sessionID, MessageID: messageID, AgentID: agentID, UserID: userID,
		Started: time.Now(), capture: capture,
	}
}

type ctxKey struct{}

// WithRun attaches r to ctx.
func WithRun(ctx context.Context, r *Run) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, r)
}

// FromContext returns the attached run, or nil.
func FromContext(ctx context.Context) *Run {
	r, _ := ctx.Value(ctxKey{}).(*Run)
	return r
}

// Capturing reports whether LLM calls should be recorded.
func (r *Run) Capturing() bool { return r != nil && r.capture }

// RunID returns the run id, or "" when r is nil.
func (r *Run) RunID() string {
	if r == nil {
		return ""
	}
	return r.ID
}

func (r *Run) SetTenant(tenantID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.tenantID = tenantID
	r.mu.Unlock()
}

func (r *Run) SetQuestion(q string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.question = q
	r.mu.Unlock()
}

// SetAnswer records the answer as persisted to the session, i.e. after
// citation grounding and failure text — not the last model output.
func (r *Run) SetAnswer(a string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.answer = a
	r.hasAnswer = true
	r.mu.Unlock()
}

func (r *Run) AddAgentTiming(t AgentTiming) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.agents = append(r.agents, t)
	r.mu.Unlock()
}

// AddCall records one model call. Messages and tools are marshaled here, at
// call time, because the ReAct loop keeps appending to the same slices.
func (r *Run) AddCall(c Call, messages, tools any) {
	if !r.Capturing() {
		return
	}
	in, inErr := json.Marshal(messages)
	if inErr != nil {
		in, _ = json.Marshal(map[string]string{"marshal_error": inErr.Error()})
	}
	if tools != nil {
		if t, err := json.Marshal(tools); err == nil && string(t) != "null" {
			c.Tools = t
		}
	}
	size := len(in) + len(c.Tools)
	if c.Output != nil {
		size += len(c.Output.Content) + len(c.Output.Reasoning)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bytes+size > MaxRunBytes {
		r.truncated = true
		c.Truncated = true
		c.InputBytes = len(in)
		c.Tools = nil
		c.Output = nil
	} else {
		c.Messages = in
		r.bytes += size
	}
	r.calls = append(r.calls, c)
}

// Finish fixes the outcome from the run error and the agents' own outcomes.
func (r *Run) Finish(runErr error, interrupted bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = time.Now()
	switch {
	case interrupted:
		r.outcome = "interrupted"
	case errors.Is(runErr, context.Canceled):
		r.outcome = "canceled"
	case runErr != nil:
		r.outcome = "error"
	default:
		r.outcome = "ok"
		for _, a := range r.agents {
			if a.Outcome != "" && a.Outcome != "ok" {
				r.outcome = a.Outcome
			}
		}
	}
	if runErr != nil {
		r.errText = runErr.Error()
	}
}

// Snapshot is an immutable copy of a finished run for the sinks.
type Snapshot struct {
	ID, SessionID, MessageID, AgentID, UserID, TenantID string
	Question, Answer, Outcome, Error                    string
	HasAnswer, Truncated                                bool
	Started, Ended                                      time.Time
	Calls                                               []Call
	Agents                                              []AgentTiming
	CallsBytes                                          int
}

// Models lists the distinct models called, in first-use order.
func (s Snapshot) Models() string {
	seen := map[string]bool{}
	var out []string
	for _, a := range s.Agents {
		if a.Model != "" && !seen[a.Model] {
			seen[a.Model] = true
			out = append(out, a.Model)
		}
	}
	for _, c := range s.Calls {
		if c.Model != "" && !seen[c.Model] {
			seen[c.Model] = true
			out = append(out, c.Model)
		}
	}
	return strings.Join(out, ",")
}

// Totals sums the agents' timing breakdown.
func (s Snapshot) Totals() AgentTiming {
	var t AgentTiming
	for _, a := range s.Agents {
		t.LLMMS += a.LLMMS
		t.ToolsMS += a.ToolsMS
		t.GroundingMS += a.GroundingMS
		t.LLMCalls += a.LLMCalls
		t.ToolCalls += a.ToolCalls
	}
	t.TotalMS = s.Ended.Sub(s.Started).Milliseconds()
	return t
}

func (r *Run) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Snapshot{
		ID: r.ID, SessionID: r.SessionID, MessageID: r.MessageID, AgentID: r.AgentID,
		UserID: r.UserID, TenantID: r.tenantID,
		Question: r.question, Answer: r.answer, Outcome: r.outcome, Error: r.errText,
		HasAnswer: r.hasAnswer, Truncated: r.truncated,
		Started: r.Started, Ended: r.ended,
		Calls:      append([]Call(nil), r.calls...),
		Agents:     append([]AgentTiming(nil), r.agents...),
		CallsBytes: r.bytes,
	}
}
