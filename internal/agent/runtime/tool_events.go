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

import "context"

// ToolCallEvent is the "data" payload of a "tool_call" run event: one when an
// Agent tool starts and one when it returns, paired by ID. Tool rounds stream
// nothing else, so clients show these to say what a long run is doing.
type ToolCallEvent struct {
	ID          string  `json:"id,omitempty"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	ElapsedTime float64 `json:"elapsed_time,omitempty"`
	Error       bool    `json:"error,omitempty"`
}

const (
	ToolCallStarted  = "started"
	ToolCallFinished = "finished"
)

type ToolCallEmitter func(ToolCallEvent)

type toolCallEmitterCtxKey struct{}

// WithToolCallEmitter installs the service-layer callback for tool_call events.
func WithToolCallEmitter(ctx context.Context, emit ToolCallEmitter) context.Context {
	if emit == nil {
		return ctx
	}
	return context.WithValue(ctx, toolCallEmitterCtxKey{}, emit)
}

// EmitToolCall forwards ev when a callback is installed.
func EmitToolCall(ctx context.Context, ev ToolCallEvent) {
	if emit, ok := ctx.Value(toolCallEmitterCtxKey{}).(ToolCallEmitter); ok {
		emit(ev)
	}
}
