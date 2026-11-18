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

package llmaudit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNilRunIsSafe(t *testing.T) {
	var r *Run
	r.SetQuestion("q")
	r.SetAnswer("a")
	r.AddAgentTiming(AgentTiming{})
	r.AddCall(Call{}, nil, nil)
	r.Finish(nil, false)
	if r.Capturing() || r.RunID() != "" || FromContext(context.Background()) != nil {
		t.Fatal("nil run must be inert")
	}
}

func TestAddCallSnapshotsMessagesAtCallTime(t *testing.T) {
	r := NewRun("id", "", "", "", "", true)
	msgs := []map[string]string{{"role": "user", "content": "first"}}
	r.AddCall(Call{Model: "m"}, msgs, nil)
	msgs[0]["content"] = "mutated"
	if got := string(r.Snapshot().Calls[0].Messages); !strings.Contains(got, "first") {
		t.Fatalf("messages = %s, want the call-time copy", got)
	}
}

func TestAddCallStubsPastCap(t *testing.T) {
	r := NewRun("id", "", "", "", "", true)
	big := strings.Repeat("x", MaxRunBytes)
	r.AddCall(Call{Model: "m"}, []string{"small"}, nil)
	r.AddCall(Call{Model: "m", Output: &Output{Content: "out"}}, []string{big}, nil)
	s := r.Snapshot()
	if !s.Truncated || !s.Calls[1].Truncated || s.Calls[1].Messages != nil || s.Calls[1].Output != nil || s.Calls[1].InputBytes <= MaxRunBytes {
		t.Fatalf("second call not stubbed: %+v", s.Calls[1])
	}
	if s.Calls[0].Truncated {
		t.Fatal("first call must stay intact")
	}
}

func TestFinishOutcome(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		interrupted bool
		agent       string
		want        string
	}{
		{"ok", nil, false, "ok", "ok"},
		{"agent failure surfaced as text", nil, false, "error", "error"},
		{"canceled", context.Canceled, false, "", "canceled"},
		{"interrupted", errors.New("interrupt"), true, "", "interrupted"},
		{"error", errors.New("boom"), false, "", "error"},
	}
	for _, c := range cases {
		r := NewRun("id", "", "", "", "", true)
		if c.agent != "" {
			r.AddAgentTiming(AgentTiming{Outcome: c.agent, Model: "qwen", LLMMS: 5})
		}
		time.Sleep(time.Millisecond)
		r.Finish(c.err, c.interrupted)
		s := r.Snapshot()
		if s.Outcome != c.want {
			t.Errorf("%s: outcome = %q, want %q", c.name, s.Outcome, c.want)
		}
		if s.Totals().TotalMS < 1 {
			t.Errorf("%s: total_ms not measured", c.name)
		}
	}
}
