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
	"time"

	"ragflow/internal/agent/runtime"

	"github.com/cloudwego/eino/components/tool"
	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

// wrapTimedTool re-exposes exactly the run interfaces the wrapped tool
// implements so the ToolsNode dispatch is unchanged; every run is timed into
// the PhaseTimings accumulator of the current Agent run and reported as a
// started/finished tool_call event pair.
func wrapTimedTool(t einotool.BaseTool, acc *runtime.PhaseTimings) einotool.BaseTool {
	inv, isInv := t.(einotool.InvokableTool)
	str, isStr := t.(einotool.StreamableTool)
	switch {
	case isInv && isStr:
		return &timedBothTool{invokable: inv, streamable: str, acc: acc}
	case isInv:
		return &timedInvokableTool{InvokableTool: inv, acc: acc}
	case isStr:
		return &timedStreamableTool{StreamableTool: str, acc: acc}
	default:
		return t
	}
}

// startToolCall emits the started event and returns the finisher for the
// same run; a streamable tool counts as finished once its stream opens.
func startToolCall(ctx context.Context, t einotool.BaseTool, acc *runtime.PhaseTimings) func(error) {
	start := time.Now()
	ev := runtime.ToolCallEvent{ID: compose.GetToolCallID(ctx), Status: runtime.ToolCallStarted}
	if info, err := t.Info(ctx); err == nil && info != nil {
		ev.Name = info.Name
	}
	runtime.EmitToolCall(ctx, ev)
	return func(err error) {
		elapsed := time.Since(start)
		acc.AddTool(elapsed)
		ev.Status, ev.ElapsedTime, ev.Error = runtime.ToolCallFinished, elapsed.Seconds(), err != nil
		runtime.EmitToolCall(ctx, ev)
	}
}

type timedInvokableTool struct {
	einotool.InvokableTool
	acc *runtime.PhaseTimings
}

func (t *timedInvokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	finish := startToolCall(ctx, t.InvokableTool, t.acc)
	out, err := t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)
	finish(err)
	return out, err
}

type timedStreamableTool struct {
	einotool.StreamableTool
	acc *runtime.PhaseTimings
}

func (t *timedStreamableTool) StreamableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	finish := startToolCall(ctx, t.StreamableTool, t.acc)
	sr, err := t.StreamableTool.StreamableRun(ctx, argumentsInJSON, opts...)
	finish(err)
	return sr, err
}

type timedBothTool struct {
	invokable  einotool.InvokableTool
	streamable einotool.StreamableTool
	acc        *runtime.PhaseTimings
}

func (t *timedBothTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return t.invokable.Info(ctx)
}

func (t *timedBothTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	finish := startToolCall(ctx, t.invokable, t.acc)
	out, err := t.invokable.InvokableRun(ctx, argumentsInJSON, opts...)
	finish(err)
	return out, err
}

func (t *timedBothTool) StreamableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	finish := startToolCall(ctx, t.streamable, t.acc)
	sr, err := t.streamable.StreamableRun(ctx, argumentsInJSON, opts...)
	finish(err)
	return sr, err
}
