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
	"context"
	"sync"
	"time"
)

// RetrievalTimings accumulates where one caller's Retrieval calls spent their
// time. A caller that retries (relaxation levels) gets the sum.
type RetrievalTimings struct {
	mu     sync.Mutex
	embed  time.Duration
	search time.Duration
	rerank time.Duration
}

type retrievalTimingsCtxKey struct{}

// WithRetrievalTimings attaches an accumulator to ctx. Without one nothing is
// measured.
func WithRetrievalTimings(ctx context.Context, t *RetrievalTimings) context.Context {
	return context.WithValue(ctx, retrievalTimingsCtxKey{}, t)
}

func retrievalTimingsFrom(ctx context.Context) *RetrievalTimings {
	t, _ := ctx.Value(retrievalTimingsCtxKey{}).(*RetrievalTimings)
	return t
}

func (t *RetrievalTimings) add(phase func(*RetrievalTimings) *time.Duration, since time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	*phase(t) += time.Since(since)
	t.mu.Unlock()
}

func embedPhase(t *RetrievalTimings) *time.Duration  { return &t.embed }
func searchPhase(t *RetrievalTimings) *time.Duration { return &t.search }
func rerankPhase(t *RetrievalTimings) *time.Duration { return &t.rerank }

// Snapshot returns (embedding, engine search, scoring/rerank). The search
// phase is measured around the whole candidate search, which embeds the query
// first, so the embedding time is taken back out of it.
func (t *RetrievalTimings) Snapshot() (embed, search, rerank time.Duration) {
	if t == nil {
		return 0, 0, 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.embed, max(t.search-t.embed, 0), t.rerank
}
