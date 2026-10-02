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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/elastic/go-elasticsearch/v8"

	"ragflow/internal/common"
)

func toJSONValue(t *testing.T, v interface{}) interface{} {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func assertJSON(t *testing.T, got interface{}, want string) {
	t.Helper()
	var w interface{}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("bad want JSON: %v", err)
	}
	if g := toJSONValue(t, got); !reflect.DeepEqual(g, w) {
		gb, _ := json.Marshal(g)
		t.Fatalf("got  %s\nwant %s", gb, want)
	}
}

func TestChunkMetaConditionClause(t *testing.T) {
	cases := []struct {
		cond map[string]interface{}
		want string
	}{
		{map[string]interface{}{"key": "dept", "op": "=", "value": " HR "},
			`{"term":{"meta_dept_kwd":{"value":"HR","case_insensitive":true}}}`},
		{map[string]interface{}{"key": "year", "op": "=", "value": float64(2024)},
			`{"term":{"meta_year_kwd":{"value":"2024","case_insensitive":true}}}`},
		{map[string]interface{}{"key": "dept", "op": "in", "value": "hr, it"},
			`{"bool":{"minimum_should_match":1,"should":[
				{"term":{"meta_dept_kwd":{"value":"hr","case_insensitive":true}}},
				{"term":{"meta_dept_kwd":{"value":"it","case_insensitive":true}}}]}}`},
		{map[string]interface{}{"key": "year", "op": ">=", "value": "2020"},
			`{"bool":{"minimum_should_match":1,"should":[
				{"range":{"meta_year_int":{"gte":2020}}},
				{"range":{"meta_year_flt":{"gte":2020}}}]}}`},
		{map[string]interface{}{"key": "date", "op": "<", "value": "2024-01-01"},
			`{"range":{"meta_date_dt":{"lt":"2024-01-01"}}}`},
		{map[string]interface{}{"key": "dept", "op": "contains", "value": "a*b"},
			`{"wildcard":{"meta_dept_kwd":{"value":"*a\\*b*","case_insensitive":true}}}`},
		{map[string]interface{}{"key": "dept", "op": "start with", "value": "h"},
			`{"prefix":{"meta_dept_kwd":{"value":"h","case_insensitive":true}}}`},
		{map[string]interface{}{"key": "dept", "op": "empty"},
			`{"bool":{"must_not":[{"exists":{"field":"meta_dept_kwd"}}]}}`},
	}
	for _, tc := range cases {
		got, err := chunkMetaConditionClause(tc.cond)
		if err != nil {
			t.Fatalf("%v: %v", tc.cond, err)
		}
		assertJSON(t, got, tc.want)
	}
	if _, err := chunkMetaConditionClause(map[string]interface{}{"key": "dept", "op": "≠", "value": "x"}); err == nil {
		t.Fatal("a negative operator must not be chunk-filterable")
	}
}

func TestBuildChunkMetaFilterClauseLogic(t *testing.T) {
	f := &common.ChunkMetaFilter{Logic: "or", Conditions: []map[string]interface{}{
		{"key": "dept", "op": "not empty"},
		{"key": "dept", "op": "end with", "value": "x"},
	}}
	got, err := buildChunkMetaFilterClause(f)
	if err != nil {
		t.Fatal(err)
	}
	assertJSON(t, got, `{"bool":{"minimum_should_match":1,"should":[
		{"exists":{"field":"meta_dept_kwd"}},
		{"wildcard":{"meta_dept_kwd":{"value":"*x","case_insensitive":true}}}]}}`)
}

func TestChunkMetaBoostShouldClauses(t *testing.T) {
	got := chunkMetaBoostShouldClauses([]common.ChunkMetaBoost{
		{Key: "dept", Op: "=", Value: "hr", Weight: 0.2},
		{Key: "date", Op: "max", Weight: 0.2},
		{Key: "dept", Op: "contains", Value: "h", Weight: 0.2},
		{Key: "date", Op: "≥", Value: "2024-01-01", Weight: 0},
	})
	assertJSON(t, got, `[
		{"term":{"meta_dept_kwd":{"value":"hr","case_insensitive":true,"boost":2}}},
		{"range":{"meta_date_dt":{"gte":"2024-01-01","boost":1}}}]`)
}

func TestApplyChunkMetaScope(t *testing.T) {
	scope := &common.ChunkMetaScope{
		Filter: &common.ChunkMetaFilter{Logic: "and", Conditions: []map[string]interface{}{{"key": "dept", "op": "=", "value": "hr"}}},
		Boosts: []common.ChunkMetaBoost{{Key: "year", Op: "=", Value: "2024", Weight: 0}},
	}
	base := buildBoolQueryFromCondition(map[string]interface{}{"doc_id": []string{"d1"}}, []string{"kb1"}, false, false)
	got, err := applyChunkMetaScope(base, scope)
	if err != nil {
		t.Fatal(err)
	}
	boolMap := got["bool"].(map[string]interface{})
	if len(boolMap["filter"].([]interface{})) != 3 {
		t.Fatalf("filter = %v, want kb_id + doc_id + metadata", boolMap["filter"])
	}
	if _, constrained := boolMap["minimum_should_match"]; constrained {
		t.Fatal("boost clauses must stay optional")
	}
	if len(boolMap["should"].([]interface{})) != 1 {
		t.Fatalf("should = %v", boolMap["should"])
	}

	// An id list already uses should as a constraint: it moves under must.
	withIDs := buildBoolQueryFromCondition(map[string]interface{}{"id": []string{"c1"}}, []string{"kb1"}, false, false)
	got, err = applyChunkMetaScope(withIDs, &common.ChunkMetaScope{Boosts: scope.Boosts})
	if err != nil {
		t.Fatal(err)
	}
	boolMap = got["bool"].(map[string]interface{})
	if _, constrained := boolMap["minimum_should_match"]; constrained {
		t.Fatal("boost clauses must not inherit the id constraint")
	}
	must := boolMap["must"].([]interface{})
	inner := must[len(must)-1].(map[string]interface{})["bool"].(map[string]interface{})
	if inner["minimum_should_match"] != 1 || len(inner["should"].([]interface{})) != 2 {
		t.Fatalf("id constraint lost: %v", inner)
	}
}

func newRecordingEngine(t *testing.T, handler func(path string, body []byte) string) *Engine {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(handler(r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery, body)))
	}))
	t.Cleanup(server.Close)
	client, err := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{server.URL}})
	if err != nil {
		t.Fatalf("new elasticsearch client: %v", err)
	}
	return &Engine{client: client}
}

func TestUpdateChunkMetadataRequest(t *testing.T) {
	var path string
	var body map[string]interface{}
	e := newRecordingEngine(t, func(p string, b []byte) string {
		path = p
		_ = json.Unmarshal(b, &body)
		return `{"updated":1}`
	})
	err := e.UpdateChunkMetadata(t.Context(), "ragflow_t", "kb1", []string{"d1", "d2"},
		map[string]interface{}{"meta_tags_kwd": []interface{}{"a", "b"}}, []string{"meta_tags_kwd", "meta_tags_int"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "POST /ragflow_t/_update_by_query?") || !strings.Contains(path, "refresh=false") {
		t.Fatalf("path = %s", path)
	}
	script := body["script"].(map[string]interface{})
	if script["source"] != "ctx._source['meta_tags_kwd'] = params['s0'];ctx._source.remove('meta_tags_int');" {
		t.Fatalf("script = %v", script["source"])
	}
	if !reflect.DeepEqual(script["params"], map[string]interface{}{"s0": []interface{}{"a", "b"}}) {
		t.Fatalf("a multi-valued keyword must travel as a list: %v", script["params"])
	}
	assertJSON(t, body["query"], `{"bool":{"filter":[{"term":{"kb_id":"kb1"}},{"terms":{"doc_id":["d1","d2"]}}]}}`)

	if err := e.UpdateChunkMetadata(t.Context(), "ragflow_t", "kb1", []string{"d1"}, map[string]interface{}{"x'];": 1}, nil, false); err == nil {
		t.Fatal("a field name outside meta_*_{kwd,int,flt,dt} must be refused")
	}
}

func TestScanDocMetadataPages(t *testing.T) {
	var afters []interface{}
	e := newRecordingEngine(t, func(p string, b []byte) string {
		if strings.HasPrefix(p, "HEAD ") {
			return ``
		}
		var req map[string]interface{}
		_ = json.Unmarshal(b, &req)
		afters = append(afters, req["search_after"])
		if req["search_after"] == nil {
			hits := make([]string, 1000)
			for i := range hits {
				hits[i] = `{"_id":"x","_source":{"id":"d` + string(rune('a'+i%26)) + `","meta_fields":{"k":"v"}},"sort":["d"]}`
			}
			return `{"hits":{"total":{"value":1001},"hits":[` + strings.Join(hits, ",") + `]}}`
		}
		return `{"hits":{"total":{"value":1001},"hits":[{"_id":"last","_source":{"id":"last","meta_fields":"{\"k\":\"w\"}"},"sort":["last"]}]}}`
	})
	seen := 0
	var last map[string]interface{}
	err := e.ScanDocMetadata(t.Context(), "t", "kb1", func(docID string, meta map[string]interface{}) error {
		seen++
		last = meta
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 1001 || len(afters) != 2 || last["k"] != "w" {
		t.Fatalf("seen=%d pages=%d last=%v", seen, len(afters), last)
	}
}
