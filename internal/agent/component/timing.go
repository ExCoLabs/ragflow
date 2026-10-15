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
	"github.com/cloudwego/eino/schema"
)

// wrapTimedTool re-exposes exactly the run interfaces the wrapped tool
// implements so the ToolsNode dispatch is unchanged; every run is timed into
// the PhaseTimings accumulator of the current Agent run.
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

type timedInvokableTool struct {
	einotool.InvokableTool
	acc *runtime.PhaseTimings
}

func (t *timedInvokableTool) InvokableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (string, error) {
	start := time.Now()
	out, err := t.InvokableTool.InvokableRun(ctx, argumentsInJSON, opts...)
	t.acc.AddTool(time.Since(start))
	return out, err
}

type timedStreamableTool struct {
	einotool.StreamableTool
	acc *runtime.PhaseTimings
}

func (t *timedStreamableTool) StreamableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	start := time.Now()
	sr, err := t.StreamableTool.StreamableRun(ctx, argumentsInJSON, opts...)
	t.acc.AddTool(time.Since(start))
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
	start := time.Now()
	out, err := t.invokable.InvokableRun(ctx, argumentsInJSON, opts...)
	t.acc.AddTool(time.Since(start))
	return out, err
}

func (t *timedBothTool) StreamableRun(ctx context.Context, argumentsInJSON string, opts ...tool.Option) (*schema.StreamReader[string], error) {
	start := time.Now()
	sr, err := t.streamable.StreamableRun(ctx, argumentsInJSON, opts...)
	t.acc.AddTool(time.Since(start))
	return sr, err
}
