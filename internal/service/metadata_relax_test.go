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
	"reflect"
	"testing"
)

func conds(keys ...string) []MetaFilterCondition {
	out := make([]MetaFilterCondition, len(keys))
	for i, k := range keys {
		out[i] = MetaFilterCondition{Key: k, Op: "=", Value: k + "-value"}
	}
	return out
}

func keysOf(c []MetaFilterCondition) []string {
	out := make([]string, len(c))
	for i, x := range c {
		out[i] = x.Key
	}
	return out
}

func TestRelaxMetaConditionsDropsLowestPriorityKey(t *testing.T) {
	got := relaxMetaConditions(conds("phase", "project", "flow"), "and", []string{"project", "phase", "flow"})
	if want := []string{"phase", "project"}; !reflect.DeepEqual(keysOf(got), want) {
		t.Fatalf("got %v, want %v", keysOf(got), want)
	}
}

func TestRelaxMetaConditionsDropsUnlistedKeyFirst(t *testing.T) {
	got := relaxMetaConditions(conds("project", "sector", "phase"), "", []string{"project", "phase"})
	if want := []string{"project", "phase"}; !reflect.DeepEqual(keysOf(got), want) {
		t.Fatalf("got %v, want %v", keysOf(got), want)
	}
}

func TestRelaxMetaConditionsWithoutPriorityDropsLastGenerated(t *testing.T) {
	got := relaxMetaConditions(conds("project", "phase"), "and", nil)
	if want := []string{"project"}; !reflect.DeepEqual(keysOf(got), want) {
		t.Fatalf("got %v, want %v", keysOf(got), want)
	}
}

func TestRelaxMetaConditionsKeepsLastConditionAndOrLogic(t *testing.T) {
	if got := relaxMetaConditions(conds("project"), "and", nil); got != nil {
		t.Fatalf("single condition relaxed to %v", keysOf(got))
	}
	// Dropping an OR branch would widen nothing the other branches do not.
	if got := relaxMetaConditions(conds("project", "phase"), "or", nil); got != nil {
		t.Fatalf("or logic relaxed to %v", keysOf(got))
	}
}

func TestMatchRelaxingStopsAtFirstMatchingSubset(t *testing.T) {
	var tried [][]string
	match := func(c []MetaFilterCondition) (int, bool) {
		tried = append(tried, keysOf(c))
		return len(c), len(c) == 2
	}
	got, n, ok := matchRelaxing(conds("project", "phase", "flow"), "and", []string{"project", "phase", "flow"}, match)
	if !ok || n != 2 || !reflect.DeepEqual(keysOf(got), []string{"project", "phase"}) {
		t.Fatalf("got %v n=%d ok=%v", keysOf(got), n, ok)
	}
	if len(tried) != 2 {
		t.Fatalf("tried %v, want two attempts", tried)
	}
}

func TestMatchRelaxingReportsOriginalWhenNothingMatches(t *testing.T) {
	orig := conds("project", "phase")
	got, n, ok := matchRelaxing(orig, "and", nil, func([]MetaFilterCondition) (int, bool) { return 0, false })
	if ok || n != 0 || !reflect.DeepEqual(got, orig) {
		t.Fatalf("got %v n=%d ok=%v", keysOf(got), n, ok)
	}
}
