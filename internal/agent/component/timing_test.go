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
	"errors"
	"testing"

	"ragflow/internal/agent/runtime"

	einotool "github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

type stubTool struct{ err error }

func (stubTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "navigo_find_firm"}, nil
}

func (s stubTool) InvokableRun(context.Context, string, ...einotool.Option) (string, error) {
	return "{}", s.err
}

func TestTimedToolEmitsToolCallPair(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantErr bool
	}{{"ok", nil, false}, {"failed", errors.New("boom"), true}} {
		t.Run(tc.name, func(t *testing.T) {
			var got []runtime.ToolCallEvent
			ctx := runtime.WithToolCallEmitter(context.Background(), func(ev runtime.ToolCallEvent) { got = append(got, ev) })
			acc := &runtime.PhaseTimings{}
			tool := wrapTimedTool(stubTool{err: tc.err}, acc).(einotool.InvokableTool)
			if _, err := tool.InvokableRun(ctx, "{}"); (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("events = %+v, want started+finished", got)
			}
			if got[0].Status != runtime.ToolCallStarted || got[0].Name != "navigo_find_firm" {
				t.Errorf("started = %+v", got[0])
			}
			if got[1].Status != runtime.ToolCallFinished || got[1].Name != "navigo_find_firm" || got[1].Error != tc.wantErr {
				t.Errorf("finished = %+v", got[1])
			}
		})
	}
}

func TestTimedToolWithoutEmitter(t *testing.T) {
	tool := wrapTimedTool(stubTool{}, &runtime.PhaseTimings{}).(einotool.InvokableTool)
	if _, err := tool.InvokableRun(context.Background(), "{}"); err != nil {
		t.Fatal(err)
	}
}
