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

package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"ragflow/internal/engine/types"
)

// countingDocEngine answers MetaValueCounts with a fixed result.
type countingDocEngine struct {
	cappedScanDocEngine
	rows []types.MetaValueCount
	err  error

	gotKBIDs  []string
	gotDocIDs []string
}

func (e *countingDocEngine) MetaValueCounts(_ context.Context, _ string, kbIDs, docIDs []string) ([]types.MetaValueCount, error) {
	e.gotKBIDs, e.gotDocIDs = kbIDs, docIDs
	return e.rows, e.err
}

// The aggregation supplies the counts; only the requested keys are kept and
// values are rendered the way the metadata filters compare them.
func TestGetMetaValueCountsByKBs_UsesTheAggregation(t *testing.T) {
	seedValueSpaceKB(t)
	docEngine := &countingDocEngine{rows: []types.MetaValueCount{
		{Key: "project", Value: "Nivy", Docs: 2},
		{Key: "year", Value: float64(2024), Docs: 3},
		{Key: "year", Value: "2024", Docs: 1},
		{Key: "draft", Value: true, Docs: 5},
		{Key: "author", Value: "Ann", Docs: 7},
	}}

	counts, err := NewMetadataServiceForTest(nil, docEngine).GetMetaValueCountsByKBs(t.Context(), []string{"kb-1"}, []string{"project", "year", "draft", "phase"}, []string{"doc-1"})
	if err != nil {
		t.Fatalf("GetMetaValueCountsByKBs: %v", err)
	}
	want := map[string]map[string]int64{
		"project": {"Nivy": 2},
		"year":    {"2024": 4},
		"draft":   {},
		"phase":   {},
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("counts:\n got %v\nwant %v", counts, want)
	}
	if !reflect.DeepEqual(docEngine.gotKBIDs, []string{"kb-1"}) || !reflect.DeepEqual(docEngine.gotDocIDs, []string{"doc-1"}) {
		t.Errorf("scoped to %v %v", docEngine.gotKBIDs, docEngine.gotDocIDs)
	}
	if docEngine.searches != 0 {
		t.Errorf("the capped scan ran %d times; the aggregation replaces it", docEngine.searches)
	}
}

// A partial aggregation is an error, not a quietly smaller answer.
func TestGetMetaValueCountsByKBs_RefusesPartialAnswers(t *testing.T) {
	seedValueSpaceKB(t)
	docEngine := &countingDocEngine{err: types.ErrMetaValueSpaceIncomplete}
	_, err := NewMetadataServiceForTest(nil, docEngine).GetMetaValueCountsByKBs(t.Context(), []string{"kb-1"}, []string{"project"}, nil)
	if !errors.Is(err, types.ErrMetaValueSpaceIncomplete) {
		t.Fatalf("err = %v, want ErrMetaValueSpaceIncomplete", err)
	}
}

// Engines without the aggregation count over the flattened scan, inside the
// document scope when one is given.
func TestGetMetaValueCountsByKBs_FallsBackToTheScan(t *testing.T) {
	seedValueSpaceKB(t)
	docEngine := &cappedScanDocEngine{records: []map[string]interface{}{
		{"id": "doc-1", "meta_fields": map[string]interface{}{"project": "Nivy", "phase": []interface{}{"DSP", "DRS"}}},
		{"id": "doc-2", "meta_fields": map[string]interface{}{"project": "Nivy", "phase": "DSP"}},
		{"id": "doc-3", "meta_fields": map[string]interface{}{"project": "Eurovea"}},
	}}
	svc := NewMetadataServiceForTest(nil, docEngine)

	counts, err := svc.GetMetaValueCountsByKBs(t.Context(), []string{"kb-1"}, []string{"project", "phase"}, nil)
	if err != nil {
		t.Fatalf("GetMetaValueCountsByKBs: %v", err)
	}
	want := map[string]map[string]int64{"project": {"Nivy": 2, "Eurovea": 1}, "phase": {"DSP": 2, "DRS": 1}}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("counts:\n got %v\nwant %v", counts, want)
	}

	counts, err = svc.GetMetaValueCountsByKBs(t.Context(), []string{"kb-1"}, []string{"project", "phase"}, []string{"doc-2", "doc-3"})
	if err != nil {
		t.Fatalf("scoped GetMetaValueCountsByKBs: %v", err)
	}
	want = map[string]map[string]int64{"project": {"Nivy": 1, "Eurovea": 1}, "phase": {"DSP": 1}}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("scoped counts:\n got %v\nwant %v", counts, want)
	}

	searches := docEngine.searches
	counts, err = svc.GetMetaValueCountsByKBs(t.Context(), []string{"kb-1"}, []string{"project"}, []string{})
	if err != nil || len(counts["project"]) != 0 || docEngine.searches != searches {
		t.Errorf("empty scope: counts=%v err=%v extra searches=%d", counts, err, docEngine.searches-searches)
	}
}
