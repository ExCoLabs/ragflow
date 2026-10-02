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

package common

import (
	"math"
	"reflect"
	"testing"
)

func activeConfig(fields ...any) map[string]any {
	return map[string]any{ChunkMetadataConfigKey: map[string]any{"enabled": true, "ready": true, "fields": fields}}
}

func TestValidateChunkMetadataFields(t *testing.T) {
	got, err := ValidateChunkMetadataFields([]any{" year ", "author", "year"})
	if err != nil || !reflect.DeepEqual(got, []string{"year", "author"}) {
		t.Fatalf("got %v, %v; want trimmed and de-duplicated", got, err)
	}
	for _, bad := range [][]any{{"2nd"}, {"a-b"}, {"doc_id"}, {"KWD"}, {7}} {
		if _, err := ValidateChunkMetadataFields(bad); err == nil {
			t.Fatalf("%v: want an error", bad)
		}
	}
	tooMany := make([]any, ChunkMetadataMaxFields+1)
	for i := range tooMany {
		tooMany[i] = "k" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := ValidateChunkMetadataFields(tooMany); err == nil {
		t.Fatal("want an error above the field cap")
	}
}

func TestParseChunkMetadataConfig(t *testing.T) {
	if ParseChunkMetadataConfig(nil) != nil || ParseChunkMetadataConfig(map[string]any{}) != nil {
		t.Fatal("absent config must be nil")
	}
	invalid := map[string]any{ChunkMetadataConfigKey: map[string]any{"enabled": true, "fields": []any{"doc_id"}}}
	if ParseChunkMetadataConfig(invalid) != nil {
		t.Fatal("an invalid whitelist must degrade to nil")
	}
	cfg := ParseChunkMetadataConfig(map[string]any{ChunkMetadataConfigKey: map[string]any{"enabled": true, "fields": []any{"year"}}})
	if cfg == nil || cfg.Active() {
		t.Fatalf("enabled but not backfilled must not be active: %+v", cfg)
	}
	if !ParseChunkMetadataConfig(activeConfig("year")).Active() {
		t.Fatal("enabled + ready + fields must be active")
	}
}

func TestChunkMetadataConfigForDatasets(t *testing.T) {
	cfg := ChunkMetadataConfigForDatasets([]map[string]any{activeConfig("year", "author"), activeConfig("author", "dept")})
	if cfg == nil || !reflect.DeepEqual(cfg.Fields, []string{"author"}) {
		t.Fatalf("want the shared keys only, got %+v", cfg)
	}
	notReady := map[string]any{ChunkMetadataConfigKey: map[string]any{"enabled": true, "fields": []any{"author"}}}
	if ChunkMetadataConfigForDatasets([]map[string]any{activeConfig("author"), notReady}) != nil {
		t.Fatal("one inactive dataset must disable the chunk path")
	}
	if ChunkMetadataConfigForDatasets([]map[string]any{activeConfig("year"), activeConfig("dept")}) != nil {
		t.Fatal("no shared key must disable the chunk path")
	}
}

func TestChunkMetadataFields(t *testing.T) {
	meta := map[string]any{
		"year":    float64(2024),
		"score":   "4.5",
		"code":    "2015_062",
		"date":    "2024-03-01",
		"tags":    []any{"a", "b", ""},
		"empty":   "",
		"skipped": "x",
	}
	got := ChunkMetadataFields(meta, []string{"year", "score", "code", "date", "tags", "empty", "missing"})
	want := map[string]any{
		"meta_year_kwd":  "2024",
		"meta_year_int":  int64(2024),
		"meta_score_kwd": "4.5",
		"meta_score_flt": 4.5,
		"meta_code_kwd":  "2015_062",
		"meta_date_kwd":  "2024-03-01",
		"meta_date_dt":   "2024-03-01",
		"meta_tags_kwd":  []any{"a", "b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
	remove := ChunkMetadataRemovalFields(map[string]any{"year": "n/a"}, []string{"year"})
	if !reflect.DeepEqual(remove, []string{"meta_year_int", "meta_year_flt", "meta_year_dt"}) {
		t.Fatalf("a value that lost its type must drop the stale twins, got %v", remove)
	}
}

func TestIsChunkFilterable(t *testing.T) {
	keys := []string{"year", "dept"}
	cases := []struct {
		name  string
		conds []map[string]any
		want  bool
	}{
		{"equal", []map[string]any{{"key": "year", "op": "=", "value": "2024"}}, true},
		{"alias", []map[string]any{{"key": "year", "op": ">=", "value": "2020"}}, true},
		{"empty op needs no value", []map[string]any{{"key": "dept", "op": "empty"}}, true},
		{"unknown key", []map[string]any{{"key": "author", "op": "=", "value": "x"}}, false},
		{"negative op", []map[string]any{{"key": "dept", "op": "≠", "value": "x"}}, false},
		{"not in", []map[string]any{{"key": "dept", "op": "not in", "value": "x"}}, false},
		{"missing value", []map[string]any{{"key": "dept", "op": "="}}, false},
		{"empty in", []map[string]any{{"key": "dept", "op": "in", "value": " , "}}, false},
		{"no conditions", nil, false},
	}
	for _, tc := range cases {
		if got := IsChunkFilterable(tc.conds, keys); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	f := NewChunkMetaFilter([]map[string]any{{"key": "year", "op": "<=", "value": "2020"}}, "")
	if f.Logic != "and" || f.Conditions[0]["op"] != "≤" {
		t.Fatalf("operators must be canonical and logic default to and: %+v", f)
	}
}

func TestParseMetaBoost(t *testing.T) {
	raw := map[string]any{
		"manual": []any{
			map[string]any{"key": "dept", "op": "=", "value": "hr", "weight": 0.2},
			map[string]any{"key": "date", "op": "max", "weight": 3.0},
			map[string]any{"key": "dept", "op": "≠", "value": "hr"},
			map[string]any{"key": "other", "op": "=", "value": "x"},
			map[string]any{"key": "dept", "op": "="},
		},
		"semi_auto":   []any{"dept", map[string]any{"key": "date", "op": ">=", "weight": "0.05"}, map[string]any{"key": "other"}},
		"auto_weight": 0.1,
		"max_total":   -1,
	}
	cfg := ParseMetaBoost(raw, []string{"dept", "date"})
	if cfg.Method != "manual" {
		t.Fatalf("a boost with manual rows and no method is manual, got %q", cfg.Method)
	}
	if cfg.AutoWeight != 0.1 || cfg.MaxTotal != 0 {
		t.Fatalf("weights must be clamped to [0,1]: %+v", cfg)
	}
	wantManual := []ChunkMetaBoost{{Key: "dept", Op: "=", Value: "hr", Weight: 0.2}, {Key: "date", Op: "max", Weight: 1}}
	if !reflect.DeepEqual(cfg.Manual, wantManual) {
		t.Fatalf("manual = %+v, want %+v", cfg.Manual, wantManual)
	}
	if len(cfg.SemiAuto) != 2 || cfg.SemiAuto[1].Op != "≥" || cfg.SemiAuto[1].Weight == nil || *cfg.SemiAuto[1].Weight != 0.05 {
		t.Fatalf("semi_auto = %+v", cfg.SemiAuto)
	}
	if ParseMetaBoost(nil, nil).Method != "off" || ParseMetaBoost(map[string]any{"method": "bogus"}, nil).Method != "off" {
		t.Fatal("absent or unknown method must be off")
	}
}

func TestMetaBoostScores(t *testing.T) {
	chunks := []map[string]any{
		{"meta_dept_kwd": "HR", "meta_date_dt": "2024-05-01", "meta_size_int": float64(10)},
		{"meta_dept_kwd": []any{"it", "ops"}, "meta_date_dt": "2022-01-01", "meta_size_int": float64(30)},
		{"meta_size_flt": 20.5},
	}
	boosts := []ChunkMetaBoost{
		{Key: "dept", Op: "=", Value: "hr", Weight: 0.1},
		{Key: "dept", Op: "in", Value: "ops, legal", Weight: 0.05},
		{Key: "date", Op: "max", Weight: 0.2},
		{Key: "size", Op: ">", Value: "15", Weight: 0.01},
	}
	got := MetaBoostScores(boosts, chunks, 0.25)
	want := []float64{0.25, 0.06, 0.01}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("scores = %v, want %v", got, want)
		}
	}
	single := MetaBoostScores([]ChunkMetaBoost{{Key: "date", Op: "min", Weight: 0.1}}, chunks[:1], 1)
	if single[0] != 0.1 {
		t.Fatalf("a single candidate is the extreme value: %v", single)
	}
	fields := MetaBoostFieldNames(boosts)
	if len(fields) != 12 || fields[0] != "meta_date_kwd" {
		t.Fatalf("field names = %v", fields)
	}
}

func TestMergeChunkMetaScopes(t *testing.T) {
	filter := &ChunkMetaScope{Filter: &ChunkMetaFilter{Conditions: []map[string]any{{"key": "dept"}}}, BoostMaxTotal: 0.3}
	boost := &ChunkMetaScope{Boosts: []ChunkMetaBoost{{Key: "dept"}}, BoostMaxTotal: 0.5}
	if MergeChunkMetaScopes(nil, &ChunkMetaScope{}) != nil {
		t.Fatal("two empty scopes merge to nil")
	}
	merged := MergeChunkMetaScopes(filter, boost)
	if merged.Filter != filter.Filter || len(merged.Boosts) != 1 || merged.BoostMaxTotal != 0.5 {
		t.Fatalf("merged = %+v", merged)
	}
}
