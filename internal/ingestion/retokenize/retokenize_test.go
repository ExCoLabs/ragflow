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

package retokenize

import (
	"context"
	"io"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"ragflow/internal/engine/elasticsearch"
	"ragflow/internal/ingestion/component"
	"ragflow/internal/tokenizer"
)

func requireTokenizerPool(t testing.TB) {
	t.Helper()
	if err := tokenizer.Init(nil); err != nil {
		t.Skipf("tokenizer pool unavailable: %v", err)
	}
}

// indexedChunk is a chunk as the ingestion Tokenizer component leaves it,
// reduced to the fields the index keeps.
func indexedChunk(t *testing.T, name, lang string, chunk map[string]any) map[string]interface{} {
	t.Helper()
	c, err := component.NewTokenizerComponent(map[string]any{"search_method": []any{"full_text"}})
	if err != nil {
		t.Fatalf("NewTokenizerComponent: %v", err)
	}
	out, err := c.Invoke(t.Context(), nil, map[string]any{
		"name":          name,
		"lang":          lang,
		"output_format": "chunks",
		"chunks":        []map[string]any{chunk},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	ck := out["chunks"].([]map[string]any)[0]
	stored := map[string]interface{}{"content_with_weight": ck["text"], "docnm_kwd": name}
	for _, k := range append([]string{"important_kwd", "question_kwd"}, TokenFields...) {
		if v, ok := ck[k]; ok {
			stored[k] = v
		}
	}
	return stored
}

func TestChunkFieldsMatchIngestion(t *testing.T) {
	requireTokenizerPool(t)
	for _, lang := range []string{"English", "Slovak"} {
		t.Run(lang, func(t *testing.T) {
			stored := indexedChunk(t, "Annual report.pdf", lang, map[string]any{
				"text":      "Running dogs chased the cats across 3 fields.",
				"questions": "Who chased the cats?\nWhere did they run?",
			})

			fields, err := ChunkFields(tokenizer.New(lang), stored)
			if err != nil {
				t.Fatalf("ChunkFields: %v", err)
			}
			for _, k := range []string{"content_ltks", "content_sm_ltks", "title_tks", "title_sm_tks", "question_tks"} {
				if fields[k] == "" || fields[k] != stored[k] {
					t.Errorf("%s = %q, ingestion stored %q", k, fields[k], stored[k])
				}
			}
			if changed := ChangedFields(stored, fields); changed != nil {
				t.Errorf("a freshly ingested chunk must be up to date, changed: %v", changed)
			}
		})
	}
}

// Keywords reach the index either from the Extractor (tokenized joined by
// spaces) or from the Tokenizer's comma-separated keywords field; both must
// come out the same.
func TestChunkFieldsKeywordsMatchIngestion(t *testing.T) {
	requireTokenizerPool(t)
	stored := indexedChunk(t, "doc.txt", "English", map[string]any{"text": "alpha", "keywords": "running dogs,annual report"})

	fields, err := ChunkFields(tokenizer.New("English"), stored)
	if err != nil {
		t.Fatalf("ChunkFields: %v", err)
	}
	if fields["important_tks"] == "" || fields["important_tks"] != stored["important_tks"] {
		t.Errorf("important_tks = %q, ingestion stored %q", fields["important_tks"], stored["important_tks"])
	}
}

func TestChunkFieldsLeaveAbsentInputsAlone(t *testing.T) {
	requireTokenizerPool(t)
	fields, err := ChunkFields(tokenizer.New("English"), map[string]interface{}{
		"content_with_weight": "plain text",
		"important_kwd":       []interface{}{},
		"docnm_kwd":           "",
	})
	if err != nil {
		t.Fatalf("ChunkFields: %v", err)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"content_ltks", "content_sm_ltks"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("fields = %v, want %v", keys, want)
	}
}

func TestChangedFields(t *testing.T) {
	source := map[string]interface{}{"content_ltks": "plain text", "content_sm_ltks": "p lain t ext"}
	fields := map[string]string{"content_ltks": "plain text", "content_sm_ltks": "plain text", "title_tks": "doc"}

	got := ChangedFields(source, fields)

	want := map[string]interface{}{"content_sm_ltks": "plain text", "title_tks": "doc"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedFields = %v, want %v", got, want)
	}
	if ChangedFields(source, map[string]string{"content_ltks": "plain text"}) != nil {
		t.Error("an up-to-date chunk must report no change")
	}
}

func TestParseFlags(t *testing.T) {
	opts, err := ParseFlags("retokenize", []string{"--kb-id", "kb1", "--kb-id=kb2", "--dry-run", "--slice", "2", "--slices", "4", "--language", "Slovak"}, io.Discard)
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	want := Options{DatasetIDs: []string{"kb1", "kb2"}, Language: "Slovak", DryRun: true, Slice: 2, Slices: 4, BatchSize: 500, ScanSize: 500}
	if !reflect.DeepEqual(*opts, want) {
		t.Errorf("options = %+v, want %+v", *opts, want)
	}

	for _, args := range [][]string{
		{},
		{"--kb-id", "kb1", "--slice", "4", "--slices", "4"},
		{"--kb-id", "kb1", "--slices", "0"},
		{"--kb-id", "kb1", "--batch-size", "0"},
		{"--kb-id", "kb1", "extra"},
		{"--kb-id", "kb1", "--config", "x.yaml"},
	} {
		if _, err := ParseFlags("retokenize", args, io.Discard); err == nil {
			t.Errorf("ParseFlags(%v) succeeded, want an error", args)
		}
	}
}

type fakeStore struct {
	chunks  []map[string]interface{}
	scans   []elasticsearch.ScanOptions
	indexes []string
	updates [][]elasticsearch.ChunkFieldUpdate
	failIDs map[string]bool
}

func (s *fakeStore) ScanChunks(_ context.Context, indexName, _ string, opts elasticsearch.ScanOptions, fn func(string, map[string]interface{}) error) error {
	s.indexes = append(s.indexes, indexName)
	s.scans = append(s.scans, opts)
	for _, ck := range s.chunks {
		if err := fn(ck["id"].(string), ck); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeStore) UpdateChunkFields(_ context.Context, _ string, updates []elasticsearch.ChunkFieldUpdate) (int, []string, error) {
	s.updates = append(s.updates, slices.Clone(updates))
	var failures []string
	for _, u := range updates {
		if s.failIDs[u.ID] {
			failures = append(failures, "chunk "+u.ID+": version conflict")
		}
	}
	return len(updates) - len(failures), failures, nil
}

func upToDate(t *testing.T, id string) map[string]interface{} {
	t.Helper()
	ck := map[string]interface{}{"id": id, "content_with_weight": "plain text"}
	fields, err := ChunkFields(tokenizer.New("English"), ck)
	if err != nil {
		t.Fatalf("ChunkFields: %v", err)
	}
	for k, v := range fields {
		ck[k] = v
	}
	return ck
}

func stale(id string) map[string]interface{} {
	return map[string]interface{}{"id": id, "content_with_weight": "plain text", "content_ltks": "p lain t ext", "content_sm_ltks": "p lain t ext"}
}

func TestRunDatasetScansASliceAndWritesOnlyStaleChunks(t *testing.T) {
	requireTokenizerPool(t)
	store := &fakeStore{chunks: []map[string]interface{}{upToDate(t, "c1"), stale("c2"), stale("c3"), stale("c4")}}
	var out strings.Builder

	res, err := RunDataset(t.Context(), store, Dataset{ID: "kb1", Name: "dataset", TenantID: "t1", Language: "English"},
		Options{Slice: 2, Slices: 4, BatchSize: 2, ScanSize: 100}, &out)
	if err != nil {
		t.Fatalf("RunDataset: %v", err)
	}

	if res != (Result{Scanned: 4, Updated: 3}) {
		t.Errorf("result = %+v", res)
	}
	if store.indexes[0] != "ragflow_t1" {
		t.Errorf("index = %q", store.indexes[0])
	}
	scan := store.scans[0]
	if scan.Slice != 2 || scan.Slices != 4 || scan.Size != 100 {
		t.Errorf("scan options = %+v", scan)
	}
	// The vector is never read and compiled artifacts are never touched.
	for _, f := range scan.Fields {
		if strings.HasSuffix(f, "_vec") {
			t.Errorf("scan reads vector field %s", f)
		}
	}
	if !reflect.DeepEqual(scan.ExcludeWithFields, compiledArtifactFields) {
		t.Errorf("excluded = %v", scan.ExcludeWithFields)
	}
	// Batches flush at BatchSize and only carry the token fields that changed.
	if len(store.updates) != 2 || len(store.updates[0]) != 2 || len(store.updates[1]) != 1 {
		t.Fatalf("update batches = %v", store.updates)
	}
	for _, batch := range store.updates {
		for _, u := range batch {
			if u.ID == "c1" {
				t.Error("an up-to-date chunk was written")
			}
			keys := make([]string, 0, len(u.Fields))
			for k := range u.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !reflect.DeepEqual(keys, []string{"content_ltks", "content_sm_ltks"}) {
				t.Errorf("chunk %s writes %v", u.ID, keys)
			}
		}
	}
}

func TestRunDatasetWritesNothingOnADryRun(t *testing.T) {
	requireTokenizerPool(t)
	store := &fakeStore{chunks: []map[string]interface{}{stale("c1"), upToDate(t, "c2")}}
	var out strings.Builder

	res, err := RunDataset(t.Context(), store, Dataset{ID: "kb1", TenantID: "t1"}, Options{BatchSize: 500, ScanSize: 500, DryRun: true}, &out)
	if err != nil {
		t.Fatalf("RunDataset: %v", err)
	}
	if res != (Result{Scanned: 2, Updated: 1}) {
		t.Errorf("result = %+v", res)
	}
	if len(store.updates) != 0 {
		t.Errorf("a dry run wrote %v", store.updates)
	}
	if !strings.Contains(out.String(), "1 would be updated") {
		t.Errorf("report = %q", out.String())
	}
}

func TestRunDatasetReportsFailedUpdatesAndContinues(t *testing.T) {
	requireTokenizerPool(t)
	store := &fakeStore{chunks: []map[string]interface{}{stale("c1"), stale("c2")}, failIDs: map[string]bool{"c1": true}}
	var out strings.Builder

	res, err := RunDataset(t.Context(), store, Dataset{ID: "kb1", TenantID: "t1"}, Options{BatchSize: 1, ScanSize: 500}, &out)
	if err != nil {
		t.Fatalf("RunDataset: %v", err)
	}
	if res != (Result{Scanned: 2, Updated: 1, Failed: 1}) {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(out.String(), "chunk c1: version conflict") {
		t.Errorf("report = %q", out.String())
	}
}
