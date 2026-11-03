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

package tool

import (
	"reflect"
	"testing"

	"ragflow/internal/common"
)

func appliedDiag(values ...interface{}) *common.MetadataFilterDiagnostic {
	d := &common.MetadataFilterDiagnostic{Status: "applied"}
	for _, v := range values {
		d.Conditions = append(d.Conditions, map[string]interface{}{"key": "k", "op": "=", "value": v})
	}
	return d
}

func TestFusionQueriesStripAppliedFilterWords(t *testing.T) {
	diag := appliedDiag("2023_079 nemocnica bb", []interface{}{"rp"})
	got := fusionQueries("2023_079 Nemocnica BB RP sektor G počet staníc potrubnej pošty",
		"Koľko staníc potrubnej pošty je v 3.PP JUH v projekte Nemocnica BB?", diag)
	want := []string{"sektor G počet staníc potrubnej pošty",
		"Koľko staníc potrubnej pošty je v 3.PP JUH v projekte"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFusionQueriesMatchFoldedDiacritics(t *testing.T) {
	got := fusionQueries("výkresy fáza ŠTÚDIA", "", appliedDiag("studia"))
	if want := []string{"výkresy fáza"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFusionQueriesWithoutAppliedFilterOnlyAddUserQuestion(t *testing.T) {
	diag := &common.MetadataFilterDiagnostic{Status: "no_matches",
		Conditions: []map[string]interface{}{{"key": "phase", "value": "sp"}}}
	got := fusionQueries("SP vykurovanie objekt", "aký spôsob vykurovania je navrhnutý", diag)
	if want := []string{"aký spôsob vykurovania je navrhnutý"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFusionQueriesDropDuplicatesAndOneWordLeftovers(t *testing.T) {
	got := fusionQueries("Nemocnica BB vykurovanie", "Nemocnica BB vykurovanie", appliedDiag("nemocnica bb"))
	if len(got) != 0 {
		t.Fatalf("got %q, want none", got)
	}
}

func chunk(id, content string) map[string]any {
	return map[string]any{"chunk_id": id, "content_with_weight": content}
}

func ids(chunks []map[string]any) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c["chunk_id"].(string)
	}
	return out
}

func TestFuseChunksRewardsAgreementAndCollapsesCopies(t *testing.T) {
	a := []map[string]any{chunk("a1", "x"), chunk("a2", "shared"), chunk("a3", "only a")}
	b := []map[string]any{chunk("b1", "shared"), chunk("b2", "y"), chunk("b3", "x")}
	got := ids(fuseChunks([][]map[string]any{a, b}, 3))
	// "shared" and "x" are in both lists; the copy b1 of a2 collapses into it.
	if want := []string{"a2", "a1", "b2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestFuseChunksSingleListKeepsOrderWithoutCopies(t *testing.T) {
	a := []map[string]any{chunk("a1", "p"), chunk("a2", "p"), chunk("a3", "q")}
	if got, want := ids(fuseChunks([][]map[string]any{a}, 10)), []string{"a1", "a3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
