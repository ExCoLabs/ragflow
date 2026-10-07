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

package task

import (
	"testing"

	"ragflow/internal/common"
)

func TestApplyChunkMetadataFields(t *testing.T) {
	chunks := []map[string]any{{"id": "c1"}, {"id": "c2", "meta_year_kwd": "stale"}}
	fields := common.ChunkMetadataFields(map[string]any{"year": "2024", "dept": "hr"}, []string{"year"})
	applyChunkMetadataFields(chunks, fields)
	for _, ck := range chunks {
		if ck["meta_year_kwd"] != "2024" || ck["meta_year_int"] != int64(2024) {
			t.Fatalf("chunk %v: whitelisted metadata must be stamped", ck)
		}
		if _, leaked := ck["meta_dept_kwd"]; leaked {
			t.Fatalf("chunk %v: keys outside the whitelist must not be stamped", ck)
		}
	}
}
