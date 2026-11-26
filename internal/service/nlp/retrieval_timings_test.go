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

package nlp

import (
	"testing"
	"time"
)

func TestRetrievalTimings(t *testing.T) {
	// No accumulator on the context: every recording site is a no-op.
	var none *RetrievalTimings
	none.add(embedPhase, time.Now())
	if embed, search, rerank := none.Snapshot(); embed+search+rerank != 0 {
		t.Fatalf("nil timings = %v %v %v", embed, search, rerank)
	}
	if retrievalTimingsFrom(t.Context()) != nil {
		t.Fatal("a bare context carries timings")
	}

	timings := &RetrievalTimings{}
	if retrievalTimingsFrom(WithRetrievalTimings(t.Context(), timings)) != timings {
		t.Fatal("timings not read back from the context")
	}
	// The candidate search embeds the query first, so its 50 ms contain the
	// embedding's 20 ms; a second attempt adds to both.
	now := time.Now()
	for range 2 {
		timings.add(embedPhase, now.Add(-20*time.Millisecond))
		timings.add(searchPhase, now.Add(-50*time.Millisecond))
		timings.add(rerankPhase, now.Add(-10*time.Millisecond))
	}
	embed, search, rerank := timings.Snapshot()
	if embed < 40*time.Millisecond || rerank < 20*time.Millisecond {
		t.Fatalf("embed=%v rerank=%v, want at least 40ms and 20ms", embed, rerank)
	}
	// 100 ms of candidate search minus the embedding inside it; never the sum.
	if search < 50*time.Millisecond || search >= 100*time.Millisecond {
		t.Fatalf("search=%v, want the embedding taken out of ~100ms", search)
	}
}
