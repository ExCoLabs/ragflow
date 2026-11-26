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
	"errors"
	"reflect"
	"testing"

	"ragflow/internal/engine/types"
)

func metaValueCountsFixture(t *testing.T) *fakeMetaES {
	t.Helper()
	return &fakeMetaES{docs: []fakeMetaDoc{
		metaDoc(t, "doc-1", "kb-1", `{"project": "Nivy", "phase": ["DSP", "DRS"], "id": "doc-1"}`),
		metaDoc(t, "doc-2", "kb-1", `{"project": "Nivy", "phase": "DSP"}`),
		metaDoc(t, "doc-3", "kb-1", `{"project": "Eurovea", "phase": "DSP"}`),
		metaDoc(t, "doc-4", "kb-2", `{"project": "Other"}`),
	}}
}

// Without a document scope every row of the datasets counts, once per
// distinct value it holds.
func TestMetaValueCounts_WholeDatasets(t *testing.T) {
	fake := metaValueCountsFixture(t)
	counts, err := newMetaValuesEngine(t, fake).MetaValueCounts(t.Context(), "tenant-1", []string{"kb-1"}, nil)
	if err != nil {
		t.Fatalf("MetaValueCounts: %v", err)
	}
	want := map[string]int64{
		"project=string:Nivy":    2,
		"project=string:Eurovea": 1,
		"phase=string:DSP":       3,
		"phase=string:DRS":       1,
	}
	if got := facetCounts(counts); !reflect.DeepEqual(got, want) {
		t.Errorf("counts:\n got %v\nwant %v", got, want)
	}
	if len(fake.idBatches) != 0 {
		t.Errorf("an unscoped count sent %d ids filters", len(fake.idBatches))
	}
}

// A document scope counts those documents only, and an empty one nothing.
func TestMetaValueCounts_DocumentScope(t *testing.T) {
	fake := metaValueCountsFixture(t)
	eng := newMetaValuesEngine(t, fake)
	counts, err := eng.MetaValueCounts(t.Context(), "tenant-1", []string{"kb-1"}, []string{"doc-1", "doc-2", "doc-4"})
	if err != nil {
		t.Fatalf("MetaValueCounts: %v", err)
	}
	want := map[string]int64{"project=string:Nivy": 2, "phase=string:DSP": 2, "phase=string:DRS": 1}
	if got := facetCounts(counts); !reflect.DeepEqual(got, want) {
		t.Errorf("counts:\n got %v\nwant %v", got, want)
	}

	searches := fake.searches
	counts, err = eng.MetaValueCounts(t.Context(), "tenant-1", []string{"kb-1"}, []string{})
	if err != nil || len(counts) != 0 {
		t.Fatalf("empty scope: counts=%v err=%v", counts, err)
	}
	if fake.searches != searches {
		t.Errorf("an empty scope searched %d times", fake.searches-searches)
	}
}

func TestMetaValueCounts_RefusesPartialAnswers(t *testing.T) {
	fake := metaValueCountsFixture(t)
	fake.failedShards = 1
	_, err := newMetaValuesEngine(t, fake).MetaValueCounts(t.Context(), "tenant-1", []string{"kb-1"}, nil)
	if !errors.Is(err, types.ErrMetaValueSpaceIncomplete) {
		t.Fatalf("err = %v, want ErrMetaValueSpaceIncomplete", err)
	}
}
