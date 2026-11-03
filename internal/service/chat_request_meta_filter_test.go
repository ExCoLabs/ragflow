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

	"ragflow/internal/entity"
)

func TestApplyRequestMetaDataFilterReplacesChatFilter(t *testing.T) {
	stored := entity.JSONMap{"method": "semi_auto", "semi_auto": []interface{}{"dept"}}
	dialog := &entity.Chat{MetaDataFilter: &stored}
	kwargs := map[string]interface{}{
		"meta_data_filter": map[string]interface{}{
			"method": "manual",
			"manual": []interface{}{map[string]interface{}{"key": "dept", "op": "=", "value": "hr"}},
		},
		"reasoning": 5,
	}
	if err := applyRequestMetaDataFilter(dialog, kwargs); err != nil {
		t.Fatal(err)
	}
	if (*dialog.MetaDataFilter)["method"] != "manual" {
		t.Fatalf("the request filter must replace the chat's: %+v", *dialog.MetaDataFilter)
	}
	if _, left := kwargs["meta_data_filter"]; left || kwargs["reasoning"] != 5 {
		t.Fatalf("only meta_data_filter is consumed from kwargs: %+v", kwargs)
	}

	// The override runs through the chat's manual path: no LLM, and no match
	// is the answer rather than a fallback to the whole dataset.
	got := ApplyMetaDataFilter(t.Context(), *dialog.MetaDataFilter, scopeTestMetadata(), "q", nil, nil, nil)
	if !reflect.DeepEqual(got, []string{"d1"}) {
		t.Fatalf("doc ids = %v, want d1", got)
	}
	noMatch := map[string]interface{}{
		"method": "manual",
		"manual": []interface{}{map[string]interface{}{"key": "dept", "op": "=", "value": "nope"}},
	}
	if got := ApplyMetaDataFilter(t.Context(), noMatch, scopeTestMetadata(), "q", nil, nil, nil); !reflect.DeepEqual(got, []string{NoMatchDocIDSentinel}) {
		t.Fatalf("doc ids = %v, want the no-match sentinel", got)
	}
}

func TestApplyRequestMetaDataFilterEmptyManualIsNoFilter(t *testing.T) {
	stored := entity.JSONMap{"method": "auto"}
	dialog := &entity.Chat{MetaDataFilter: &stored}
	kwargs := map[string]interface{}{"meta_data_filter": map[string]interface{}{"method": "manual", "manual": []interface{}{}}}
	if err := applyRequestMetaDataFilter(dialog, kwargs); err != nil {
		t.Fatal(err)
	}
	if got := ApplyMetaDataFilter(t.Context(), *dialog.MetaDataFilter, scopeTestMetadata(), "q", nil, []string{"d9"}, nil); !reflect.DeepEqual(got, []string{"d9"}) {
		t.Fatalf("an empty manual list must leave the scope unfiltered, got %v", got)
	}
}

func TestApplyRequestMetaDataFilterRejects(t *testing.T) {
	for name, raw := range map[string]interface{}{
		"not an object":  "manual",
		"auto":           map[string]interface{}{"method": "auto"},
		"semi_auto":      map[string]interface{}{"method": "semi_auto", "semi_auto": []interface{}{"dept"}},
		"no method":      map[string]interface{}{"manual": []interface{}{}},
		"manual no list": map[string]interface{}{"method": "manual", "manual": "dept=hr"},
	} {
		if err := applyRequestMetaDataFilter(&entity.Chat{}, map[string]interface{}{"meta_data_filter": raw}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestApplyRequestMetaDataFilterAbsentKeepsChatFilter(t *testing.T) {
	stored := entity.JSONMap{"method": "auto"}
	dialog := &entity.Chat{MetaDataFilter: &stored}
	for _, kwargs := range []map[string]interface{}{{}, {"meta_data_filter": nil}} {
		if err := applyRequestMetaDataFilter(dialog, kwargs); err != nil || dialog.MetaDataFilter != &stored {
			t.Fatalf("kwargs %v: the chat's filter must stay (err %v)", kwargs, err)
		}
	}
}
