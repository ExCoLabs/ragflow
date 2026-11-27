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
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"ragflow/internal/agent/runtime"
	agenttool "ragflow/internal/agent/tool"
)

// scriptedLoopModel answers each round of a run from a script: the canvas
// Agent's rounds from root, a sub-agent's from child. It records what every
// round was sent, so a test can read the tool results the model received.
type scriptedLoopModel struct {
	mu          sync.Mutex
	root, child []*schema.Message
	rootInputs  [][]*schema.Message
}

func (m *scriptedLoopModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}

func (m *scriptedLoopModel) Generate(ctx context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	script := &m.root
	if subAgentDepth(ctx) > 0 {
		script = &m.child
	} else {
		m.rootInputs = append(m.rootInputs, append([]*schema.Message(nil), in...))
	}
	if len(*script) == 0 {
		return nil, errors.New("scripted model: no round left")
	}
	next := (*script)[0]
	*script = (*script)[1:]
	return next, nil
}

func (m *scriptedLoopModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
}

func toolCallRound(calls ...schema.ToolCall) *schema.Message {
	return &schema.Message{Role: schema.Assistant, ToolCalls: calls}
}

func toolCall(id, name, args string) schema.ToolCall {
	return schema.ToolCall{ID: id, Type: "function", Function: schema.FunctionCall{Name: name, Arguments: args}}
}

type scriptedTool struct {
	name string
	run  func(ctx context.Context, args string) (string, error)
}

func (t *scriptedTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name, Desc: t.name}, nil
}

func (t *scriptedTool) InvokableRun(ctx context.Context, args string, _ ...einotool.Option) (string, error) {
	return t.run(ctx, args)
}

// useScriptedLoop runs the real ReAct loop (runEinoReActAgent, the tool
// wrappers, sub-agent tools) over a scripted model and the given plain tools.
func useScriptedLoop(t *testing.T, mdl *scriptedLoopModel, tools ...einotool.BaseTool) {
	t.Helper()
	previousModel, previousTools := agentChatModelBuilder, agentToolsBuilder
	agentChatModelBuilder = func(context.Context, AgentParam) (model.ToolCallingChatModel, error) { return mdl, nil }
	agentToolsBuilder = func(ctx context.Context, p AgentParam) ([]einotool.BaseTool, error) {
		built := []einotool.BaseTool{}
		if subAgentDepth(ctx) == 0 {
			built = append(built, tools...)
		} else {
			built = append(built, &scriptedTool{name: "search_archa_data", run: func(context.Context, string) (string, error) { return "chunks", nil }})
		}
		for _, sub := range p.SubAgents {
			built = append(built, &subAgentTool{name: sub.Name, spec: sub})
		}
		return built, nil
	}
	t.Cleanup(func() { agentChatModelBuilder, agentToolsBuilder = previousModel, previousTools })
}

type recordedStream struct {
	mu      sync.Mutex
	content strings.Builder
}

func (r *recordedStream) context(ctx context.Context) context.Context {
	return runtime.WithAgentMessageEmitter(ctx, func(contentDelta, _ string) {
		r.mu.Lock()
		r.content.WriteString(contentDelta)
		r.mu.Unlock()
	})
}

func coordinatorScript() *scriptedLoopModel {
	return &scriptedLoopModel{
		root: []*schema.Message{
			toolCallRound(
				toolCall("c1", "is_query", `{"sql":"SELECT nope FROM projects"}`),
				toolCall("c2", "files_agent", `{"user_prompt":"x"}`),
				toolCall("c3", "retrieval_agent", `{"user_prompt":"find   the acoustic report","reasoning":"r","context":"c"}`),
			),
			{Role: schema.Assistant, Content: "coordinator answer"},
		},
		child: []*schema.Message{
			toolCallRound(
				toolCall("k1", "ghost_search", `{"query":"acoustic"}`),
				toolCall("k2", "search_archa_data", `{"query":"acoustic","filters":[{"key":"phase","op":"=","value":["DSP"]}]}`),
			),
			{Role: schema.Assistant, Content: "child report"},
		},
	}
}

func failingMCPTool() einotool.BaseTool {
	return &scriptedTool{name: "is_query", run: func(context.Context, string) (string, error) {
		return "", &agenttool.MCPToolError{Tool: "is_query", Text: "no such column: nope"}
	}}
}

func coordinatorParam(toolErrorsToModel bool) AgentParam {
	child := AgentParam{ModelID: "stub", MaxRounds: 2, ToolErrorsToModel: toolErrorsToModel}
	return AgentParam{
		ModelID: "stub", MaxRounds: 2, ToolErrorsToModel: toolErrorsToModel,
		SubAgents: []SubAgentTool{{Name: "retrieval_agent", Param: child}},
	}
}

func toolResults(round []*schema.Message) map[string]string {
	results := map[string]string{}
	for _, msg := range round {
		if msg.Role == schema.Tool {
			results[msg.ToolCallID] = msg.Content
		}
	}
	return results
}

// With tool_errors_to_model an MCP tool's own error and a call to a tool
// that does not exist come back to the model as tool results, in the canvas
// Agent and in a sub-agent alike, and the run completes.
func TestAgent_ToolErrorsToModel(t *testing.T) {
	mdl := coordinatorScript()
	useScriptedLoop(t, mdl, failingMCPTool())
	stream := &recordedStream{}
	out, err := NewAgentComponent(coordinatorParam(true)).Invoke(stream.context(t.Context()), nil, map[string]any{"user_prompt": "question"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out["content"] != "coordinator answer" || stream.content.String() != "coordinator answer" {
		t.Fatalf("content = %v, streamed = %q", out["content"], stream.content.String())
	}
	if len(mdl.rootInputs) != 2 {
		t.Fatalf("coordinator rounds = %d, want 2", len(mdl.rootInputs))
	}
	results := toolResults(mdl.rootInputs[1])
	if got := results["c1"]; !strings.Contains(got, "TOOL ERROR: is_query failed: no such column: nope") {
		t.Errorf("MCP error as tool result = %q", got)
	}
	if got := results["c2"]; !strings.Contains(got, `no tool named "files_agent"`) || !strings.Contains(got, "is_query, retrieval_agent") {
		t.Errorf("unknown tool as tool result = %q", got)
	}
	if got := results["c3"]; got != "child report" {
		t.Errorf("sub-agent result = %q, want its report (it recovered from its own unknown tool)", got)
	}
}

// Without the opt-in both failures abort the run, as before.
func TestAgent_ToolErrorsStayFatalByDefault(t *testing.T) {
	mdl := coordinatorScript()
	mdl.root[0] = toolCallRound(toolCall("c1", "is_query", `{}`))
	useScriptedLoop(t, mdl, failingMCPTool())
	_, err := NewAgentComponent(coordinatorParam(false)).Invoke(t.Context(), nil, map[string]any{"user_prompt": "question"})
	var toolErr *agenttool.MCPToolError
	if !errors.As(err, &toolErr) || !strings.Contains(err.Error(), `mcp tool "is_query" returned isError: no such column: nope`) {
		t.Fatalf("err = %v, want the MCP isError failure", err)
	}

	mdl = coordinatorScript()
	mdl.root[0] = toolCallRound(toolCall("c2", "files_agent", `{}`))
	useScriptedLoop(t, mdl, failingMCPTool())
	if _, err = NewAgentComponent(coordinatorParam(false)).Invoke(t.Context(), nil, map[string]any{"user_prompt": "question"}); err == nil || !strings.Contains(err.Error(), "files_agent") {
		t.Fatalf("err = %v, want the unknown-tool failure", err)
	}
}

// Any other tool failure, e.g. the query embedding request losing its
// connection, also reaches the model; only a run whose context is done stops.
func TestAgent_ToolErrorsToModelCoversTransportFailures(t *testing.T) {
	mdl := coordinatorScript()
	mdl.root[0] = toolCallRound(toolCall("c1", "is_query", `{}`))
	boom := errors.New("GetVector failed: unexpected EOF")
	useScriptedLoop(t, mdl, &scriptedTool{name: "is_query", run: func(context.Context, string) (string, error) { return "", boom }})
	out, err := NewAgentComponent(coordinatorParam(true)).Invoke(t.Context(), nil, map[string]any{"user_prompt": "question"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out["content"] != "coordinator answer" {
		t.Fatalf("content = %v", out["content"])
	}
	if got := toolResults(mdl.rootInputs[1])["c1"]; !strings.Contains(got, "TOOL ERROR: is_query failed: GetVector failed: unexpected EOF") {
		t.Errorf("transport error as tool result = %q", got)
	}

	mdl = coordinatorScript()
	mdl.root[0] = toolCallRound(toolCall("c1", "is_query", `{}`))
	ctx, cancel := context.WithCancel(t.Context())
	useScriptedLoop(t, mdl, &scriptedTool{name: "is_query", run: func(context.Context, string) (string, error) {
		cancel()
		return "", context.Canceled
	}})
	if _, err := NewAgentComponent(coordinatorParam(true)).Invoke(ctx, nil, map[string]any{"user_prompt": "question"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation", err)
	}
}

// The new switch is a DSL parameter. A run's inputs — for a sub-agent, the
// calling model's arguments — cannot turn it on.
func TestAgent_RuntimeSwitchesComeFromTheDSLOnly(t *testing.T) {
	p := agentParamFromMap(map[string]any{"tool_errors_to_model": true})
	if !p.ToolErrorsToModel {
		t.Fatalf("DSL switch not read: %+v", p)
	}
	merged := mergeAgentParam(AgentParam{}, map[string]any{"tool_errors_to_model": true})
	if merged.ToolErrorsToModel {
		t.Fatalf("run inputs switched it on: %+v", merged)
	}
	if kept := mergeAgentParam(p, map[string]any{"user_prompt": "x"}); !kept.ToolErrorsToModel {
		t.Fatalf("DSL switch lost on merge: %+v", kept)
	}
}

// The real loop, with the Agent feeding a Message node and citation on: the
// sub-agent's model output reaches neither the stream nor the caller's
// "already streamed" bookkeeping, and the coordinator's answer is sent.
func TestAgent_SubAgentModelOutputStaysOutOfTheRealLoopStream(t *testing.T) {
	mdl := coordinatorScript()
	mdl.root[0] = toolCallRound(toolCall("c3", "retrieval_agent", `{"user_prompt":"task"}`))
	mdl.root[1] = &schema.Message{Role: schema.Assistant, Content: "coordinator answer [ID:0123456789abcdef]"}
	mdl.child = []*schema.Message{{Role: schema.Assistant, Content: "child report [ID:feedfacefeedface]", ReasoningContent: "child thinking"}}
	useScriptedLoop(t, mdl)

	ctx := runtime.WithComponentExecutionOptions(t.Context(), runtime.ComponentExecutionOptions{DeferAgentToMessage: true})
	ctx = runtime.WithAgentMessageEmitter(ctx, func(string, string) {})
	param := coordinatorParam(false)
	param.Cite = true
	out, err := NewAgentComponent(param).Invoke(ctx, nil, map[string]any{"user_prompt": "question"})
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
	if got := strings.TrimSpace(content.String()); got != "coordinator answer" {
		t.Fatalf("streamed content = %q, want the coordinator's answer with its unknown marker removed", got)
	}
	if thinking.Len() != 0 {
		t.Fatalf("sub-agent thinking reached the stream: %q", thinking.String())
	}
	if final["citation_validation_status"] == "skipped_streamed" || final["removed_citation_count"] != 1 {
		t.Fatalf("citation validation = %v, removed = %v", final["citation_validation_status"], final["removed_citation_count"])
	}
}
