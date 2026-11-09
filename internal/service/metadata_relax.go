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

import "strings"

// relaxMetaConditions returns the conditions without the least important one,
// or nil when nothing is left to drop. Importance is the order of keyPriority
// (the semi_auto key list: listed first = kept longest); keys outside it go
// first, later-generated before earlier among equals.
//
// An LLM filter that matches nothing usually has one wrong value (a phase
// named in the question that the corpus files under another one) next to a
// right one (the project). Dropping the whole filter then searches every
// project; dropping the wrong condition keeps the scope the user asked for.
func relaxMetaConditions(conditions []MetaFilterCondition, logic string, keyPriority []string) []MetaFilterCondition {
	if len(conditions) < 2 || !strings.EqualFold(strings.TrimSpace(logic), "and") && strings.TrimSpace(logic) != "" {
		return nil
	}
	rank := make(map[string]int, len(keyPriority))
	for i, k := range keyPriority {
		if _, seen := rank[k]; !seen {
			rank[k] = i
		}
	}
	drop, dropRank := -1, -1
	for i, c := range conditions {
		r, ok := rank[c.Key]
		if !ok {
			r = len(keyPriority)
		}
		if r >= dropRank {
			drop, dropRank = i, r
		}
	}
	out := make([]MetaFilterCondition, 0, len(conditions)-1)
	out = append(out, conditions[:drop]...)
	return append(out, conditions[drop+1:]...)
}

// matchRelaxing runs conditions through match and, while they select nothing,
// retries with one condition fewer (relaxMetaConditions). It returns the
// conditions that matched with match's result, or the original conditions
// with a zero result when no subset does.
func matchRelaxing[T any](conditions []MetaFilterCondition, logic string, keyPriority []string,
	match func([]MetaFilterCondition) (T, bool)) ([]MetaFilterCondition, T, bool) {
	for current := conditions; current != nil; current = relaxMetaConditions(current, logic, keyPriority) {
		if res, ok := match(current); ok {
			return current, res, true
		}
	}
	var zero T
	return conditions, zero, false
}
