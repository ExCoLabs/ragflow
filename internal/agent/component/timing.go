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

package component

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"ragflow/internal/agent/runtime"
	agenttool "ragflow/internal/agent/tool"
	"ragflow/internal/common"

	"github.com/cloudwego/eino/components/tool"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

const (
	toolProgressArgsLimit  = 200
	toolProgressErrorLimit = 300
)

// toolObserver is what every tool call of an Agent run passes through: it
// times the call into the run's PhaseTimings, reports it as a started/finished
// tool_call event pair, writes the per-call timing line, and — only when the
// Agent opted in — reports progress to the client and hands tool errors to
// the model.
type toolObserver struct {
	name string
	kind string
	acc  *runtime.PhaseTimings
	// errorsToModel is the Agent's tool_errors_to_model.
	errorsToModel bool
}

type observedCall struct {
	start time.Time
	id    string
}

func (o *toolObserver) begin(ctx context.Context, argumentsInJSON string) observedCall {
	call := observedCall{start: time.Now(), id: compose.GetToolCallID(ctx)}
	runtime.EmitToolCall(ctx, runtime.ToolCallEvent{ID: call.id, Name: o.name, Status: runtime.ToolCallStarted})
	runtime.EmitAgentProgress(ctx, runtime.AgentProgressEvent{
		Kind:   o.kind,
		Phase:  "start",
		CallID: call.id,
		Name:   o.name,
		Agent:  runtime.SubAgentPath(ctx),
		Args:   summarizeToolArgs(o.kind, argumentsInJSON),
	})
	return call
}

// end closes a call. resultBytes is -1 for a streamed result, whose size and
// full duration are not known when the stream is handed over; a streamable
// tool counts as finished once its stream opens.
func (o *toolObserver) end(ctx context.Context, call observedCall, out string, resultBytes int, err error) (string, error) {
	took := time.Since(call.start)
	o.acc.AddTool(took)
	runtime.EmitToolCall(ctx, runtime.ToolCallEvent{
		ID: call.id, Name: o.name, Status: runtime.ToolCallFinished,
		ElapsedTime: took.Seconds(), Error: err != nil,
	})

	errText := ""
	if err != nil {
		errText = truncateRunes(err.Error(), toolProgressErrorLimit)
	}
	sessionID, tenantID := "", ""
	if state, sErr := runtime.GetStateFromContext(ctx); sErr == nil && state != nil {
		sessionID, _ = state.Sys["session_id"].(string)
		tenantID, _ = state.Sys["tenant_id"].(string)
	}
	common.AgentTimingLog(fmt.Sprintf(
		"agent_tool_timings tool=%s kind=%s ok=%t duration_ms=%d result_bytes=%d error=%q component=%s agent=%s session=%s tenant=%s",
		o.name, o.kind, err == nil, took.Milliseconds(), resultBytes, errText,
		common.LogComponentFromContext(ctx), runtime.SubAgentPath(ctx), sessionID, tenantID))

	ok := err == nil
	finished := runtime.AgentProgressEvent{
		Kind:       o.kind,
		Phase:      "finish",
		CallID:     call.id,
		Name:       o.name,
		Agent:      runtime.SubAgentPath(ctx),
		OK:         &ok,
		Error:      errText,
		DurationMs: took.Milliseconds(),
	}
	if resultBytes > 0 {
		finished.ResultChars = utf8.RuneCountInString(out)
	}
	runtime.EmitAgentProgress(ctx, finished)

	// A cancelled or timed-out run must still stop; any other failure, also a
	// transient one such as an embedding endpoint dropping the connection, is
	// something the model can retry or answer around.
	if o.errorsToModel && err != nil && ctx.Err() == nil {
		return toolErrorForModel(o.name, errorText(err)), nil
	}
	return out, err
}

func toolErrorForModel(name, text string) string {
	return fmt.Sprintf("TOOL ERROR: %s failed: %s\nThe call did not succeed. Correct the arguments and call it again, use another tool, or continue without this result and say so.", name, strings.TrimSpace(text))
}

// unknownToolForModel answers a call to a tool the Agent does not have, for
// Agents with tool_errors_to_model; without it the ToolsNode fails the run.
func unknownToolForModel(available []string) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, name, _ string) (string, error) {
		return fmt.Sprintf("TOOL ERROR: there is no tool named %q. Available tools: %s. Call one of them or answer without a tool.", name, strings.Join(available, ", ")), nil
	}
}

// summarizeToolArgs keeps what tells a call apart — a sub-agent's task, a
// search query and its filters — and otherwise the arguments themselves, cut
// to a line.
func summarizeToolArgs(kind, argumentsInJSON string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		return truncateRunes(strings.TrimSpace(argumentsInJSON), toolProgressArgsLimit)
	}
	text := func(key string) string {
		value, _ := args[key].(string)
		return strings.Join(strings.Fields(value), " ")
	}
	summary := ""
	switch {
	case kind == "sub_agent" && text("user_prompt") != "":
		summary = text("user_prompt")
	case text("query") != "":
		summary = text("query")
		if filters, ok := args["filters"]; ok {
			if encoded, err := json.Marshal(filters); err == nil {
				summary += " | filters: " + string(encoded)
			}
		}
	default:
		encoded, _ := json.Marshal(args)
		summary = string(encoded)
	}
	return truncateRunes(summary, toolProgressArgsLimit)
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

// wrapTimedTool re-exposes exactly the run interfaces the wrapped tool
// implements so the ToolsNode dispatch is unchanged; every run goes through
// the observer of the current Agent run.
func wrapTimedTool(t einotool.BaseTool, obs *toolObserver) einotool.BaseTool {
	inv, isInv := t.(einotool.InvokableTool)
	str, isStr := t.(einotool.StreamableTool)
	switch {
	case isInv && isStr:
		return &timedBothTool{invokable: inv, streamable: str, obs: obs}
	case isInv:
		return &timedInvokableTool{InvokableTool: inv, obs: obs}
	case isStr:
		return &timedStreamableTool{StreamableTool: str, obs: obs}
	default:
		return t
	}
}

type timedInvokableTool struct {
	einotool.InvokableTool
	obs *toolObserver
}

func (t *timedInvokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	call := t.obs.begin(ctx, argumentsInJSON)
	out, err := t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)
	return t.obs.end(ctx, call, out, len(out), err)
}

type timedStreamableTool struct {
	einotool.StreamableTool
	obs *toolObserver
}

func (t *timedStreamableTool) StreamableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	call := t.obs.begin(ctx, argumentsInJSON)
	sr, err := t.StreamableTool.StreamableRun(ctx, argumentsInJSON, opts...)
	if _, endErr := t.obs.end(ctx, call, "", -1, err); endErr == nil && err != nil {
		return schema.StreamReaderFromArray([]string{toolErrorForModel(t.obs.name, errorText(err))}), nil
	}
	return sr, err
}

type timedBothTool struct {
	invokable  einotool.InvokableTool
	streamable einotool.StreamableTool
	obs        *toolObserver
}

func (t *timedBothTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.invokable.Info(ctx)
}

func (t *timedBothTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	call := t.obs.begin(ctx, argumentsInJSON)
	out, err := t.invokable.InvokableRun(ctx, argumentsInJSON, opts...)
	return t.obs.end(ctx, call, out, len(out), err)
}

func (t *timedBothTool) StreamableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	call := t.obs.begin(ctx, argumentsInJSON)
	sr, err := t.streamable.StreamableRun(ctx, argumentsInJSON, opts...)
	if _, endErr := t.obs.end(ctx, call, "", -1, err); endErr == nil && err != nil {
		return schema.StreamReaderFromArray([]string{toolErrorForModel(t.obs.name, errorText(err))}), nil
	}
	return sr, err
}

// errorText is the MCP server's own error text when err carries one.
func errorText(err error) string {
	var toolErr *agenttool.MCPToolError
	if errors.As(err, &toolErr) {
		return toolErr.Text
	}
	return err.Error()
}
