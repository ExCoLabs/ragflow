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

package component

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"

	"ragflow/internal/agent/runtime"
)

// coordinatorRunner stands in for a coordinator's ReAct loop: it calls two
// sub-agents in parallel, each of which streams its report the way a child
// Agent run does, then answers itself.
func coordinatorRunner(t *testing.T, answer string) func(context.Context, AgentParam) (*schema.Message, error) {
	t.Helper()
	return func(ctx context.Context, p AgentParam) (*schema.Message, error) {
		if subAgentDepth(ctx) > 0 {
			runtime.EmitAgentMessage(ctx, "child report ", "child thinking")
			runtime.EmitAgentMessage(ctx, "[ID:feedfacefeedface]", "")
			return &schema.Message{Role: schema.Assistant, Content: "child report [ID:feedfacefeedface]"}, nil
		}
		var wg sync.WaitGroup
		for _, name := range []string{"navigo_agent", "retrieval_agent"} {
			wg.Go(func() {
				child := &subAgentTool{spec: SubAgentTool{Name: name, Param: AgentParam{ModelID: "stub", MaxRounds: 1}}}
				out, err := child.InvokableRun(ctx, `{"user_prompt":"task","reasoning":"r","context":"c"}`)
				if err != nil || !strings.HasPrefix(out, "child report") {
					t.Errorf("%s: out=%q err=%v", name, out, err)
				}
			})
		}
		wg.Wait()
		return &schema.Message{Role: schema.Assistant, Content: answer}, nil
	}
}

// A sub-agent's report is a tool result, not answer text: the client is sent
// the coordinator's answer only, and citation validation still cleans it,
// instead of standing down because "content was already streamed".
func TestAgent_SubAgentOutputStaysOutOfTheAnswerStream(t *testing.T) {
	const answer = "final answer [ID:0123456789abcdef]"
	const cleaned = "final answer"

	t.Run("agent streams to a Message node", func(t *testing.T) {
		withAgentRunner(t, coordinatorRunner(t, answer))
		ctx := runtime.WithComponentExecutionOptions(t.Context(), runtime.ComponentExecutionOptions{DeferAgentToMessage: true})
		ctx = runtime.WithAgentMessageEmitter(ctx, func(string, string) {})
		out, err := NewAgentComponent(AgentParam{ModelID: "stub", MaxRounds: 1, Cite: true}).Invoke(ctx, nil, map[string]any{"user_prompt": "hello"})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		var content, thinking strings.Builder
		final, err := out["content"].(*runtime.DeferredStream).Open(ctx, func(contentDelta, thinkingDelta string) {
			content.WriteString(contentDelta)
			thinking.WriteString(thinkingDelta)
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got := strings.TrimSpace(content.String()); got != cleaned {
			t.Fatalf("streamed content = %q, want only the coordinator's cleaned answer %q", got, cleaned)
		}
		if thinking.Len() != 0 {
			t.Fatalf("sub-agent thinking reached the stream: %q", thinking.String())
		}
		if final["citation_validation_status"] == "skipped_streamed" || final["removed_citation_count"] != 1 {
			t.Fatalf("citation validation = %v, removed = %v; want the unknown marker removed", final["citation_validation_status"], final["removed_citation_count"])
		}
		if got := strings.TrimSpace(final["content"].(string)); got != cleaned {
			t.Fatalf("final content = %q, want %q", got, cleaned)
		}
		if got := strings.TrimSpace(runtime.GetAgentStreamedContent(ctx)); got != cleaned {
			t.Fatalf("run-level streamed content = %q, want %q", got, cleaned)
		}
	})

	t.Run("agent streams directly", func(t *testing.T) {
		withAgentRunner(t, coordinatorRunner(t, answer))
		var mu sync.Mutex
		var content strings.Builder
		ctx := runtime.WithAgentMessageEmitter(t.Context(), func(contentDelta, _ string) {
			mu.Lock()
			content.WriteString(contentDelta)
			mu.Unlock()
		})
		final, err := NewAgentComponent(AgentParam{ModelID: "stub", MaxRounds: 1, Cite: true}).Invoke(ctx, nil, map[string]any{"user_prompt": "hello"})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if got := strings.TrimSpace(content.String()); got != cleaned {
			t.Fatalf("streamed content = %q, want %q", got, cleaned)
		}
		if final["citation_validation_status"] == "skipped_streamed" {
			t.Fatal("citation validation was skipped as already streamed")
		}
	})
}

// A sub-agent never hands its caller a lazy stream, whatever the caller's
// graph node was compiled to do.
func TestAgent_SubAgentIgnoresTheCallersDeferral(t *testing.T) {
	withAgentRunner(t, func(context.Context, AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "child answer"}, nil
	})
	ctx := runtime.WithComponentExecutionOptions(t.Context(), runtime.ComponentExecutionOptions{DeferAgentToMessage: true, SuppressAgentMessageEvents: true})
	child := &subAgentTool{spec: SubAgentTool{Name: "child", Param: AgentParam{ModelID: "stub", MaxRounds: 1}}}
	out, err := child.InvokableRun(ctx, `{"user_prompt":"task"}`)
	if err != nil || out != "child answer" {
		t.Fatalf("out=%q err=%v, want the child's answer text", out, err)
	}
}
