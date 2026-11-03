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
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"ragflow/internal/entity"
)

func TestMetadataPushdownSearchesAllTenantIndices(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&entity.Knowledgebase{}); err != nil {
		t.Fatal(err)
	}
	status := string(entity.StatusValid)
	kbs := []entity.Knowledgebase{
		{ID: "kb-a", TenantID: "a", Status: &status},
		{ID: "kb-b", TenantID: "a", Status: &status},
		{ID: "kb-c", TenantID: "b", Status: &status},
	}
	if err := db.Create(&kbs).Error; err != nil {
		t.Fatal(err)
	}
	conditions := []map[string]interface{}{{"key": "author", "op": "=", "value": "Alice"}}
	for _, tc := range []struct {
		name         string
		missingIndex bool
		missingKB    bool
		total        int
	}{
		{name: "both tenants", total: 2},
		{name: "overflow falls back", total: metaPushdownMaxSize + 1},
		{name: "missing index falls back", missingIndex: true},
		{name: "missing dataset falls back", missingKB: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searches := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Elastic-Product", "Elasticsearch")
				if r.Method == http.MethodHead {
					if tc.missingIndex && r.URL.Path == "/ragflow_doc_meta_b" {
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}
				searches++
				if r.URL.Path != "/ragflow_doc_meta_a,ragflow_doc_meta_b/_search" {
					t.Errorf("search path = %s, want both tenant indices once", r.URL.Path)
				}
				if r.URL.Query().Get("allow_partial_search_results") != "false" {
					t.Error("partial tenant search results must trigger fallback")
				}
				fmt.Fprintf(w, `{"hits":{"total":{"value":%d,"relation":"eq"},"hits":[{"_id":"doc-a"},{"_id":"doc-b"}]}}`, tc.total)
			}))
			defer srv.Close()
			kbIDs := []string{"kb-a", "kb-b", "kb-c", "kb-a"}
			if tc.missingKB {
				kbIDs = append(kbIDs, "missing")
			}
			got := newTestEngine(t, srv.URL).FilterDocIdsByMetaPushdown(t.Context(), db, kbIDs, conditions, "and")
			if tc.missingIndex || tc.missingKB || tc.total > metaPushdownMaxSize {
				if got != nil {
					t.Fatalf("partial result = %v, want fallback", got)
				}
			} else if !reflect.DeepEqual(got, []string{"doc-a", "doc-b"}) {
				t.Fatalf("doc IDs = %v, want both tenants' documents", got)
			}
			if (tc.missingIndex || tc.missingKB) && searches != 0 {
				t.Fatal("searched despite incomplete tenant scope")
			}
		})
	}
}

func TestLargeMetadataDocumentScopeStaysWithinTermsLimit(t *testing.T) {
	ids := make([]string, 120974)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	query := buildBoolQueryFromCondition(map[string]interface{}{"doc_id": ids}, []string{"kb-a"}, false, false)
	root := query["bool"].(map[string]interface{})
	filters := root["filter"].([]interface{})
	if len(filters) != 2 {
		t.Fatalf("got %d filters, want dataset and document scope", len(filters))
	}
	if got := filters[0].(map[string]interface{})["terms"].(map[string]interface{})["kb_id"].([]string); len(got) != 1 || got[0] != "kb-a" {
		t.Fatal("dataset scope lost")
	}
	union := filters[1].(map[string]interface{})["bool"].(map[string]interface{})
	if union["minimum_should_match"] != 1 {
		t.Fatal("document batches must be ORed within the dataset scope")
	}
	var all []interface{}
	for _, b := range union["should"].([]interface{}) {
		batch := b.(map[string]interface{})["terms"].(map[string]interface{})["doc_id"].([]interface{})
		if len(batch) > 65536 {
			t.Fatal("terms query exceeds Elasticsearch default limit")
		}
		all = append(all, batch...)
	}
	if len(all) != len(ids) || all[len(all)-1] != ids[len(ids)-1] {
		t.Fatal("document scope was truncated")
	}
}

func TestMetadataPushdownReadsBeyondResultWindow(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&entity.Knowledgebase{}); err != nil {
		t.Fatal(err)
	}
	status := string(entity.StatusValid)
	if err := db.Create(&entity.Knowledgebase{ID: "kb-a", TenantID: "a", Status: &status}).Error; err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"", "http", "timeout", "shards", "empty", "inexact"} {
		t.Run("page_"+failure, func(t *testing.T) {
			cleared := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Elastic-Product", "Elasticsearch")
				if r.Method == http.MethodHead {
					return
				}
				if r.Method == http.MethodDelete {
					cleared = true
					fmt.Fprint(w, `{"succeeded":true,"num_freed":1}`)
					return
				}
				if r.URL.Path == "/_search/scroll" {
					switch failure {
					case "http":
						w.WriteHeader(http.StatusServiceUnavailable)
						fmt.Fprint(w, `{"error":"unavailable"}`)
						return
					case "timeout":
						fmt.Fprint(w, `{"_scroll_id":"next","timed_out":true,"hits":{"hits":[{"_id":"last-document"}]}}`)
						return
					case "shards":
						fmt.Fprint(w, `{"_scroll_id":"next","_shards":{"failed":1},"hits":{"hits":[{"_id":"last-document"}]}}`)
						return
					case "empty":
						fmt.Fprint(w, `{"_scroll_id":"next","hits":{"hits":[]}}`)
						return
					}
					fmt.Fprint(w, `{"_scroll_id":"next","_shards":{"failed":0},"hits":{"total":{"value":10001,"relation":"eq"},"hits":[{"_id":"last-document"}]}}`)
					return
				}
				var body map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				query, _ := json.Marshal(body["query"])
				if !strings.Contains(string(query), "kb-a") || !strings.Contains(string(query), "meta_fields.project.keyword") {
					t.Error("metadata scan lost dataset or project scope")
				}
				if r.URL.Query().Get("scroll") == "" {
					fmt.Fprint(w, `{"hits":{"total":{"value":10001,"relation":"eq"},"hits":[{"_id":"prefix"}]}}`)
					return
				}
				relation := "eq"
				if failure == "inexact" {
					relation = "gte"
				}
				fmt.Fprintf(w, `{"_scroll_id":"first","_shards":{"failed":0},"hits":{"total":{"value":10001,"relation":"%s"},"hits":[`, relation)
				for i := 0; i < 10000; i++ {
					if i > 0 {
						fmt.Fprint(w, ",")
					}
					fmt.Fprintf(w, `{"_id":"document-%d"}`, i)
				}
				fmt.Fprint(w, `]}}`)
			}))
			defer srv.Close()
			conditions := []map[string]interface{}{{"key": "project", "op": "contains", "value": "target"}}
			got := newTestEngine(t, srv.URL).FilterDocIdsByMetaPushdown(t.Context(), db, []string{"kb-a"}, conditions, "and")
			if failure != "" {
				if got != nil {
					t.Fatal("failed page must not return partial scope")
				}
			} else if len(got) != 10001 || got[len(got)-1] != "last-document" {
				t.Fatalf("got %d document IDs, want all 10001 including the last document", len(got))
			}
			if !cleared {
				t.Error("metadata scroll was not cleared")
			}
		})
	}
}
