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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLangfuseExportSendsOTLPTraceKeyedByRunID(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/otel/v1/traces" || !strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	run := NewRun("0123456789abcdef0123456789abcdef", "sess1", "msg1", "agent1", "user1", true)
	run.SetQuestion("Aká je výška atiky?")
	run.AddCall(Call{Model: "qwen", Mode: "stream", Start: time.Now(), Output: &Output{Content: "1,2 m"}, Error: "x"},
		[]map[string]string{{"role": "system", "content": "SYSTEM PROMPT"}}, []map[string]string{{"name": "search"}})
	run.SetAnswer("1,2 m [ID:0]")
	run.Finish(nil, false)

	l := NewLangfuseExporter(srv.URL, "pk", "sk", srv.Client())
	if err := l.Send(t.Context(), run.Snapshot()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want root + 1 generation", len(bodies))
	}
	all, _ := json.Marshal(bodies)
	for _, want := range []string{
		`"traceId":"0123456789abcdef0123456789abcdef"`,
		`"langfuse.session.id"`, `"sess1"`,
		`"langfuse.trace.input"`, "Aká je výška atiky?",
		`"langfuse.observation.type"`, `"generation"`,
		"SYSTEM PROMPT", `"langfuse.observation.level"`,
	} {
		if !strings.Contains(string(all), want) {
			t.Errorf("export missing %s", want)
		}
	}
}

func TestLangfuseExportReportsRejectedSpans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":"1","errorMessage":"nope"}}`))
	}))
	defer srv.Close()
	run := NewRun("0123456789abcdef0123456789abcdef", "", "", "", "", true)
	run.Finish(nil, false)
	l := NewLangfuseExporter(srv.URL, "pk", "sk", srv.Client())
	if err := l.Send(t.Context(), run.Snapshot()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want rejection surfaced", err)
	}
}
