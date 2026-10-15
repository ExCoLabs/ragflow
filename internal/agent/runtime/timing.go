// Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
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

package runtime

import (
	"context"
	"sync"
	"time"
)

// PhaseTimings accumulates per-phase wall-clock time inside one Agent run so
// the component can emit a single breakdown line when the run ends. Model
// calls, tool calls and the citation-grounding pass are the variable parts of
// an answer's latency; tracking them separately is what makes a slow answer
// explainable (provider stall vs retrieval vs grounding).
type PhaseTimings struct {
	mu        sync.Mutex
	llm       time.Duration
	llmCalls  int
	tools     time.Duration
	toolCalls int
	grounding time.Duration
}

type phaseTimingsCtxKey struct{}

// WithPhaseTimings attaches an accumulator to ctx; nil-safe to read back.
func WithPhaseTimings(ctx context.Context, t *PhaseTimings) context.Context {
	return context.WithValue(ctx, phaseTimingsCtxKey{}, t)
}

// PhaseTimingsFromContext returns the attached accumulator, or nil.
func PhaseTimingsFromContext(ctx context.Context) *PhaseTimings {
	t, _ := ctx.Value(phaseTimingsCtxKey{}).(*PhaseTimings)
	return t
}

func (t *PhaseTimings) AddLLM(d time.Duration) {
	t.mu.Lock()
	t.llm += d
	t.llmCalls++
	t.mu.Unlock()
}

func (t *PhaseTimings) AddTool(d time.Duration) {
	t.mu.Lock()
	t.tools += d
	t.toolCalls++
	t.mu.Unlock()
}

func (t *PhaseTimings) AddGrounding(d time.Duration) {
	t.mu.Lock()
	t.grounding += d
	t.mu.Unlock()
}

// Snapshot returns (llm, tools, grounding, llmCalls, toolCalls).
func (t *PhaseTimings) Snapshot() (time.Duration, time.Duration, time.Duration, int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.llm, t.tools, t.grounding, t.llmCalls, t.toolCalls
}
