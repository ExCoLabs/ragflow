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

package elasticsearch

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"
)

type esRequest struct {
	method, path, query string
	body                string
}

// newRecordingEngine returns an engine whose requests are answered by
// respond and recorded in order.
func newRecordingEngine(t *testing.T, respond func(r esRequest) string) (*Engine, *[]esRequest) {
	t.Helper()
	var seen []esRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := esRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: string(body)}
		seen = append(seen, req)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respond(req)))
	}))
	t.Cleanup(server.Close)

	client, err := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{server.URL}})
	if err != nil {
		t.Fatalf("new elasticsearch client: %v", err)
	}
	return &Engine{client: client}, &seen
}

func TestScanBodySelectsTheDatasetAndOnlySlicesWhenAsked(t *testing.T) {
	single := ScanBody("kb1", ScanOptions{Fields: []string{"content_ltks"}, ExcludeWithFields: []string{"compile_kwd"}, Size: 50})

	wantQuery := map[string]interface{}{"bool": map[string]interface{}{
		"filter":   []interface{}{map[string]interface{}{"term": map[string]interface{}{"kb_id": "kb1"}}},
		"must_not": []interface{}{map[string]interface{}{"exists": map[string]interface{}{"field": "compile_kwd"}}},
	}}
	if !reflect.DeepEqual(single["query"], wantQuery) {
		t.Errorf("query = %#v", single["query"])
	}
	if single["size"] != 50 || !reflect.DeepEqual(single["_source"], []string{"content_ltks"}) {
		t.Errorf("body = %#v", single)
	}
	// A one-slice scroll is the plain scan; Elasticsearch rejects {"id": 0, "max": 1}.
	if _, ok := single["slice"]; ok {
		t.Error("single-slice scan asks for a slice")
	}

	sliced := ScanBody("kb1", ScanOptions{Slice: 3, Slices: 8})
	if !reflect.DeepEqual(sliced["slice"], map[string]interface{}{"id": 3, "max": 8}) {
		t.Errorf("slice = %#v", sliced["slice"])
	}
}

func TestScanChunksPagesThroughTheScrollAndClearsIt(t *testing.T) {
	pages := []string{
		`{"_scroll_id":"s1","hits":{"hits":[{"_id":"c1","_source":{"content_ltks":"a"}},{"_id":"c2","_source":{"content_ltks":"b"}}]}}`,
		`{"_scroll_id":"s2","hits":{"hits":[{"_id":"c3","_source":{"content_ltks":"c"}}]}}`,
		`{"_scroll_id":"s2","hits":{"hits":[]}}`,
	}
	engine, seen := newRecordingEngine(t, func(r esRequest) string {
		if r.method == http.MethodDelete {
			return `{"succeeded":true}`
		}
		page := pages[0]
		pages = pages[1:]
		return page
	})

	var ids []string
	err := engine.ScanChunks(t.Context(), "ragflow_t1", "kb1", ScanOptions{Slice: 1, Slices: 2, Size: 2}, func(id string, source map[string]interface{}) error {
		ids = append(ids, id+"="+source["content_ltks"].(string))
		return nil
	})
	if err != nil {
		t.Fatalf("ScanChunks: %v", err)
	}
	if want := []string{"c1=a", "c2=b", "c3=c"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("chunks = %v, want %v", ids, want)
	}

	requests := *seen
	if len(requests) != 4 {
		t.Fatalf("requests = %+v", requests)
	}
	first := requests[0]
	if first.path != "/ragflow_t1/_search" || !strings.Contains(first.query, "scroll=300000ms") {
		t.Errorf("first request = %s %s?%s", first.method, first.path, first.query)
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(first.body), &body); err != nil {
		t.Fatalf("scan body: %v", err)
	}
	if !reflect.DeepEqual(body["slice"], map[string]interface{}{"id": float64(1), "max": float64(2)}) {
		t.Errorf("slice = %v", body["slice"])
	}
	if requests[1].path != "/_search/scroll" || !strings.Contains(requests[1].body, `"s1"`) || !strings.Contains(requests[1].body, `"5m"`) {
		t.Errorf("second request = %+v", requests[1])
	}
	if requests[2].path != "/_search/scroll" || !strings.Contains(requests[2].body, `"s2"`) {
		t.Errorf("third request = %+v", requests[2])
	}
	if last := requests[3]; last.method != http.MethodDelete || !strings.Contains(last.body, `"s2"`) {
		t.Errorf("scroll not cleared: %+v", last)
	}
}

func TestUpdateChunkFieldsSendsPartialUpdatesAndReportsItemErrors(t *testing.T) {
	engine, seen := newRecordingEngine(t, func(esRequest) string {
		return `{"errors":true,"items":[` +
			`{"update":{"_id":"c1","status":200}},` +
			`{"update":{"_id":"c2","status":409,"error":{"type":"version_conflict_engine_exception"}}}]}`
	})

	updated, failures, err := engine.UpdateChunkFields(t.Context(), "ragflow_t1", []ChunkFieldUpdate{
		{ID: "c1", Fields: map[string]interface{}{"content_ltks": "a"}},
		{ID: "c2", Fields: map[string]interface{}{"title_tks": "b"}},
	})
	if err != nil {
		t.Fatalf("UpdateChunkFields: %v", err)
	}
	if updated != 1 || len(failures) != 1 || !strings.Contains(failures[0], "c2") {
		t.Errorf("updated = %d, failures = %v", updated, failures)
	}

	req := (*seen)[0]
	if req.path != "/_bulk" || strings.Contains(req.query, "refresh") {
		t.Errorf("request = %s?%s", req.path, req.query)
	}
	var lines []map[string]interface{}
	scanner := bufio.NewScanner(strings.NewReader(req.body))
	for scanner.Scan() {
		var line map[string]interface{}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatalf("bulk line %q: %v", scanner.Text(), err)
		}
		lines = append(lines, line)
	}
	want := []map[string]interface{}{
		{"update": map[string]interface{}{"_index": "ragflow_t1", "_id": "c1"}},
		{"doc": map[string]interface{}{"content_ltks": "a"}},
		{"update": map[string]interface{}{"_index": "ragflow_t1", "_id": "c2"}},
		{"doc": map[string]interface{}{"title_tks": "b"}},
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("bulk body = %v", lines)
	}
}

func TestUpdateChunkFieldsWithNothingToWriteSendsNoRequest(t *testing.T) {
	engine, seen := newRecordingEngine(t, func(esRequest) string { return `{}` })
	if updated, failures, err := engine.UpdateChunkFields(t.Context(), "ragflow_t1", nil); err != nil || updated != 0 || failures != nil {
		t.Fatalf("UpdateChunkFields(nil) = %d, %v, %v", updated, failures, err)
	}
	if len(*seen) != 0 {
		t.Errorf("requests = %v", *seen)
	}
}
