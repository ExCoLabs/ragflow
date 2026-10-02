package nlp

import (
	"reflect"
	"testing"

	"ragflow/internal/dao"
)

func TestCollapseDuplicateChunksKeepsBestRankedCopy(t *testing.T) {
	result := &RetrievalSearchResult{
		IDs: []string{"a", "b", "c", "d", "e", "f"},
		Field: map[string]map[string]interface{}{
			"a": {"content_with_weight": "same  text"},
			"b": {"content_with_weight": "other"},
			"c": {"content_with_weight": " same\ntext "},
			"d": {"content_with_weight": ""},
			"e": {},
			"f": {"content_with_weight": "same text"},
		},
	}
	// Rank order: c is the best ranked copy of "same text".
	kept, duplicatesOf := collapseDuplicateChunks(result, []int{2, 1, 0, 3, 4, 5})

	if want := []int{2, 1, 3, 4}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
	if want := map[int][]int{2: {0, 5}}; !reflect.DeepEqual(duplicatesOf, want) {
		t.Fatalf("duplicatesOf = %v, want %v", duplicatesOf, want)
	}
}

func collapseRetrievalRows() []map[string]interface{} {
	return []map[string]interface{}{
		{"id": "a1", "content_ltks": "alpha", "content_with_weight": "alpha paragraph", "docnm_kwd": "a.pdf", "kb_id": "kb-1", "_score": 0.9},
		{"id": "b1", "content_ltks": "alpha", "content_with_weight": "alpha  paragraph\n", "docnm_kwd": "b.pdf", "kb_id": "kb-2", "_score": 0.8},
		{"id": "a2", "content_ltks": "alpha", "content_with_weight": "alpha distinct", "docnm_kwd": "a.pdf", "kb_id": "kb-1", "_score": 0.7},
		{"id": "c1", "content_ltks": "alpha", "content_with_weight": "alpha paragraph", "docnm_kwd": "c.pdf", "kb_id": "kb-1", "_score": 0.6},
	}
}

func runCollapseRetrieval(t *testing.T, collapse *bool) *RetrievalResult {
	t.Helper()
	oldQueryBuilder := globalQueryBuilder
	globalQueryBuilder = NewQueryBuilder()
	defer func() { globalQueryBuilder = oldQueryBuilder }()

	service := NewRetrievalService(&retrievalCountEngine{rows: collapseRetrievalRows()}, &dao.DocumentDAO{})
	top := 10
	threshold := 0.5
	vectorWeight := 1.0
	result, err := service.Retrieval(t.Context(), &RetrievalRequest{
		Question:               "alpha",
		TenantIDs:              []string{"tenant-1"},
		Page:                   1,
		PageSize:               10,
		KNNTopK:                &top,
		SimilarityThreshold:    &threshold,
		VectorSimilarityWeight: &vectorWeight,
		CollapseDuplicates:     collapse,
	})
	if err != nil {
		t.Fatalf("Retrieval failed: %v", err)
	}
	return result
}

func chunkIDs(chunks []map[string]interface{}) []string {
	ids := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		ids = append(ids, chunk["chunk_id"].(string))
	}
	return ids
}

func TestRetrievalCollapsesDuplicateChunksByDefault(t *testing.T) {
	result := runCollapseRetrieval(t, nil)

	if want := []string{"a1", "a2"}; !reflect.DeepEqual(chunkIDs(result.Chunks), want) {
		t.Fatalf("chunk ids = %v, want %v", chunkIDs(result.Chunks), want)
	}
	if result.Total != 2 {
		t.Fatalf("total = %d, want 2", result.Total)
	}

	duplicates, ok := result.Chunks[0]["duplicates"].([]map[string]interface{})
	if !ok || len(duplicates) != 2 {
		t.Fatalf("duplicates = %#v, want two entries", result.Chunks[0]["duplicates"])
	}
	first := duplicates[0]
	if first["chunk_id"] != "b1" || first["document_name"] != "b.pdf" || first["dataset_id"] != "kb-2" || first["document_id"] != "" {
		t.Fatalf("first duplicate = %#v", first)
	}
	if sim, _ := first["similarity"].(float64); sim <= 0 {
		t.Fatalf("first duplicate similarity = %v, want the copy's own score", first["similarity"])
	}
	if duplicates[1]["chunk_id"] != "c1" {
		t.Fatalf("second duplicate = %#v, want c1", duplicates[1])
	}
	if got, ok := result.Chunks[1]["duplicates"].([]map[string]interface{}); !ok || len(got) != 0 {
		t.Fatalf("distinct chunk duplicates = %#v, want empty list", result.Chunks[1]["duplicates"])
	}

	counts := map[string]interface{}{}
	for _, agg := range result.DocAggs {
		counts[agg["doc_name"].(string)] = agg["count"]
	}
	if want := map[string]interface{}{"a.pdf": 2}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("doc_aggs = %v, want %v", counts, want)
	}
}

func TestRetrievalCollapseDuplicatesOptOut(t *testing.T) {
	result := runCollapseRetrieval(t, new(false))

	if want := []string{"a1", "b1", "a2", "c1"}; !reflect.DeepEqual(chunkIDs(result.Chunks), want) {
		t.Fatalf("chunk ids = %v, want %v", chunkIDs(result.Chunks), want)
	}
	if result.Total != 4 {
		t.Fatalf("total = %d, want 4", result.Total)
	}
	for _, chunk := range result.Chunks {
		if _, ok := chunk["duplicates"]; ok {
			t.Fatalf("chunk %v carries duplicates with collapsing disabled", chunk["chunk_id"])
		}
	}
}
