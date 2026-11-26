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

package runtime

import "context"

// AgentProgressEvent tells the client that an Agent started or finished a
// tool call or a sub-agent run. It is carried as its own stream event, never
// as answer content.
type AgentProgressEvent struct {
	// Kind is "tool" or "sub_agent".
	Kind string `json:"kind"`
	// Phase is "start" or "finish"; the two events of a call share CallID.
	Phase  string `json:"phase"`
	CallID string `json:"call_id,omitempty"`
	// Name is the tool name as the model called it.
	Name string `json:"name"`
	// Agent is the calling sub-agent's path ("retrieval_agent",
	// "outer/inner"); empty when the canvas Agent itself made the call.
	Agent string `json:"agent,omitempty"`
	// Args is a short summary of the call's arguments (start only).
	Args string `json:"args,omitempty"`
	// OK, Error, DurationMs and ResultChars describe a finished call.
	OK          *bool  `json:"ok,omitempty"`
	Error       string `json:"error,omitempty"`
	DurationMs  int64  `json:"duration_ms,omitempty"`
	ResultChars int    `json:"result_chars,omitempty"`
}

type AgentProgressEmitter func(AgentProgressEvent)

type agentProgressEmitterCtxKey struct{}
type agentProgressEnabledCtxKey struct{}
type subAgentPathCtxKey struct{}

// WithAgentProgressEmitter installs the service layer's progress callback.
// Nothing is emitted until an Agent opts in (WithAgentProgressEnabled).
func WithAgentProgressEmitter(ctx context.Context, emit AgentProgressEmitter) context.Context {
	if emit == nil {
		return ctx
	}
	return context.WithValue(ctx, agentProgressEmitterCtxKey{}, emit)
}

// WithAgentProgressEnabled turns progress events on for an Agent run and,
// through the context, for the sub-agents it calls.
func WithAgentProgressEnabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentProgressEnabledCtxKey{}, true)
}

// EmitAgentProgress sends ev when the run opted in and a callback is
// installed; it reports whether it did.
func EmitAgentProgress(ctx context.Context, ev AgentProgressEvent) bool {
	if enabled, _ := ctx.Value(agentProgressEnabledCtxKey{}).(bool); !enabled {
		return false
	}
	emit, _ := ctx.Value(agentProgressEmitterCtxKey{}).(AgentProgressEmitter)
	if emit == nil {
		return false
	}
	emit(ev)
	return true
}

// WithSubAgent appends a sub-agent's name to the path of the run in ctx.
func WithSubAgent(ctx context.Context, name string) context.Context {
	if path := SubAgentPath(ctx); path != "" {
		name = path + "/" + name
	}
	return context.WithValue(ctx, subAgentPathCtxKey{}, name)
}

// SubAgentPath is the chain of sub-agent names leading to the run in ctx;
// empty for the canvas Agent itself.
func SubAgentPath(ctx context.Context) string {
	path, _ := ctx.Value(subAgentPathCtxKey{}).(string)
	return path
}
