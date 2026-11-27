package nlp

import (
	"context"
	"fmt"
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

func TestCollapseDuplicateChunksPreservesCompilationKinds(t *testing.T) {
	result := &RetrievalSearchResult{
		IDs: []string{"ordinary", "ordinary-list", "page", "page-list", "graph", "graph-list"},
		Field: map[string]map[string]interface{}{
			"ordinary":      {"content_with_weight": "same  text"},
			"ordinary-list": {"content_with_weight": "same text", "compile_kwd": []string{}},
			"page":          {"content_with_weight": "same text", "compile_kwd": "page_index"},
			"page-list":     {"content_with_weight": "same\ntext", "compile_kwd": []interface{}{"page_index"}},
			"graph":         {"content_with_weight": "same text", "compile_kwd": "graph"},
			"graph-list":    {"content_with_weight": "same text", "compile_kwd": []string{"graph"}},
		},
	}
	kept, duplicatesOf := collapseDuplicateChunks(result, []int{2, 0, 4, 3, 1, 5})
	if want := []int{2, 0, 4}; !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept = %v, want %v", kept, want)
	}
	if want := map[int][]int{2: {3}, 0: {1}, 4: {5}}; !reflect.DeepEqual(duplicatesOf, want) {
		t.Fatalf("duplicatesOf = %v, want %v", duplicatesOf, want)
	}
}

func TestPreferNewestDocVersionsPreservesUnverifiedCopies(t *testing.T) {
	result := &RetrievalSearchResult{
		IDs: []string{"old1", "old2", "keep", "new1", "dwg"},
		Field: map[string]map[string]interface{}{
			"old1": {"doc_id": "doc-old", "docnm_kwd": "Brief(1).docx", "content_with_weight": "x"},
			"old2": {"doc_id": "doc-old", "docnm_kwd": "Brief(1).docx", "content_with_weight": "y"},
			"keep": {"doc_id": "doc-keep", "docnm_kwd": "other.pdf", "content_with_weight": "z"},
			"new1": {"doc_id": "doc-new", "docnm_kwd": "Brief(2).docx", "content_with_weight": "w"},
			"dwg":  {"doc_id": "doc-dwg", "docnm_kwd": "010_A - t.dwg(9).md", "content_with_weight": "v"},
		},
		Chunks: []map[string]interface{}{
			{"doc_id": "doc-old"}, {"doc_id": "doc-old"}, {"doc_id": "doc-keep"}, {"doc_id": "doc-new"}, {"doc_id": "doc-dwg"},
		},
		Highlight: map[string]string{"old1": "h1", "new1": "h4"},
		Total:     5,
	}
	got := preferNewestDocVersions(result)
	if want := []string{"old1", "old2", "keep", "new1", "dwg"}; !reflect.DeepEqual(got.IDs, want) {
		t.Fatalf("IDs = %v, want %v", got.IDs, want)
	}
	if got.Total != 5 || len(got.Chunks) != 5 || len(got.Field) != 5 {
		t.Fatalf("sizes Total=%d Chunks=%d Field=%d", got.Total, len(got.Chunks), len(got.Field))
	}
	if got.Highlight["new1"] != "h4" || got.Highlight["old1"] != "h1" || len(got.Highlight) != 2 {
		t.Fatalf("Highlight = %v", got.Highlight)
	}
}

func TestVerifiedDocCopyPruningPreservesUniqueEvidence(t *testing.T) {
	result := &RetrievalSearchResult{
		IDs: []string{"old-shared", "old-unique", "new-shared"},
		Field: map[string]map[string]interface{}{
			"old-shared": {"doc_id": "old", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
			"old-unique": {"doc_id": "old", "docnm_kwd": "project/sp/report(1).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "Openings 200 mm"},
			"new-shared": {"doc_id": "new", "docnm_kwd": "project/sp/report(2).pdf", "kb_id": "kb", "document_content_hash": "same", "content_with_weight": "same evidence"},
		},
		Highlight: map[string]string{"old-shared": "duplicate", "old-unique": "witness", "new-shared": "copy"},
		Total:     3,
	}
	got := preferNewestDocVersions(result)
	if !reflect.DeepEqual(got.IDs, []string{"old-unique", "new-shared"}) {
		t.Fatalf("IDs = %v", got.IDs)
	}
	if got.Total != 2 || len(got.Field) != 2 || len(got.Chunks) != 2 || len(got.Highlight) != 2 || got.Highlight["old-unique"] != "witness" {
		t.Fatalf("parallel result structures lost unique evidence: %#v", got)
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

// knnRecordingEngine records how many candidates reached the scorer.
type knnRecordingEngine struct {
	retrievalCountEngine
	scoredCounts []int
}

func (e *knnRecordingEngine) KNNScores(ctx context.Context, chunks []map[string]interface{}, vec []float64, n int) (map[string]interface{}, error) {
	e.scoredCounts = append(e.scoredCounts, len(chunks))
	return e.retrievalCountEngine.KNNScores(ctx, chunks, vec, n)
}

// crowdedRetrievalRows: three texts with four identical copies each rank
// ahead of six distinct texts, as uploaded copies X.pdf, X(2).pdf ... do.
func crowdedRetrievalRows() []map[string]interface{} {
	rows := make([]map[string]interface{}, 0, 18)
	score := 0.99
	for _, text := range []string{"A", "B", "C"} {
		for copyNo := 1; copyNo <= 4; copyNo++ {
			rows = append(rows, map[string]interface{}{
				"id": fmt.Sprintf("%s%d", text, copyNo), "content_ltks": "alpha",
				"content_with_weight": "alpha " + text, "docnm_kwd": fmt.Sprintf("%s(%d).pdf", text, copyNo), "_score": score,
			})
			score -= 0.01
		}
	}
	for i := 0; i < 6; i++ {
		rows = append(rows, map[string]interface{}{
			"id": fmt.Sprintf("d%d", i), "content_ltks": "alpha",
			"content_with_weight": fmt.Sprintf("alpha distinct %d", i), "docnm_kwd": fmt.Sprintf("d%d.pdf", i), "_score": score,
		})
		score -= 0.01
	}
	return rows
}

func runCrowdedRetrieval(t *testing.T, collapse *bool) (*RetrievalResult, *knnRecordingEngine) {
	t.Helper()
	oldQueryBuilder := globalQueryBuilder
	globalQueryBuilder = NewQueryBuilder()
	defer func() { globalQueryBuilder = oldQueryBuilder }()

	eng := &knnRecordingEngine{retrievalCountEngine: retrievalCountEngine{rows: crowdedRetrievalRows()}}
	service := NewRetrievalService(eng, &dao.DocumentDAO{})
	threshold, vectorWeight, window := 0.5, 1.0, 6
	result, err := service.Retrieval(t.Context(), &RetrievalRequest{
		Question:               "alpha",
		TenantIDs:              []string{"tenant-1"},
		Page:                   1,
		PageSize:               6,
		RerankCandidatesCount:  &window,
		SimilarityThreshold:    &threshold,
		VectorSimilarityWeight: &vectorWeight,
		CollapseDuplicates:     collapse,
	})
	if err != nil {
		t.Fatalf("Retrieval failed: %v", err)
	}
	return result, eng
}

func TestRetrievalCollapsesCopiesBeforeTheRerankWindow(t *testing.T) {
	result, eng := runCrowdedRetrieval(t, nil)

	if want := []int{collapseOverfetchCount(6)}; !reflect.DeepEqual(eng.searchLimits, want) {
		t.Fatalf("search limits = %v, want %v", eng.searchLimits, want)
	}
	if want := []int{6}; !reflect.DeepEqual(eng.scoredCounts, want) {
		t.Fatalf("scored candidates = %v, want one distinct text per rerank slot %v", eng.scoredCounts, want)
	}
	if want := []string{"A1", "B1", "C1", "d0", "d1", "d2"}; !reflect.DeepEqual(chunkIDs(result.Chunks), want) {
		t.Fatalf("chunk ids = %v, want %v", chunkIDs(result.Chunks), want)
	}
	if result.Total != 6 {
		t.Fatalf("total = %d, want the 6 distinct texts of the window", result.Total)
	}
	dups, _ := result.Chunks[0]["duplicates"].([]map[string]interface{})
	if len(dups) != 3 || dups[0]["chunk_id"] != "A2" || dups[0]["document_name"] != "A(2).pdf" {
		t.Fatalf("duplicates of A1 = %v, want A2..A4", result.Chunks[0]["duplicates"])
	}
	if sim, _ := dups[0]["similarity"].(float64); sim != result.Chunks[0]["similarity"] {
		t.Fatalf("copy similarity = %v, want its representative's %v", sim, result.Chunks[0]["similarity"])
	}
	counts := map[string]interface{}{}
	for _, agg := range result.DocAggs {
		counts[agg["doc_name"].(string)] = agg["count"]
	}
	if len(counts) != 6 || counts["A(1).pdf"] != 1 || counts["A(2).pdf"] != nil {
		t.Fatalf("doc_aggs = %v, want one entry per kept chunk", counts)
	}
}

func TestRetrievalWithoutCollapseKeepsTheRerankWindow(t *testing.T) {
	result, eng := runCrowdedRetrieval(t, new(false))

	if want := []int{6}; !reflect.DeepEqual(eng.searchLimits, want) || !reflect.DeepEqual(eng.scoredCounts, want) {
		t.Fatalf("search limits = %v, scored = %v, want exactly the rerank window", eng.searchLimits, eng.scoredCounts)
	}
	if want := []string{"A1", "A2", "A3", "A4", "B1", "B2"}; !reflect.DeepEqual(chunkIDs(result.Chunks), want) {
		t.Fatalf("chunk ids = %v, want %v", chunkIDs(result.Chunks), want)
	}
}

func TestCollapseOverfetchCount(t *testing.T) {
	for window, want := range map[int]int{1: 3, 64: 192, 128: 384, 400: 1024, 2000: 2000} {
		if got := collapseOverfetchCount(window); got != want {
			t.Errorf("collapseOverfetchCount(%d) = %d, want %d", window, got, want)
		}
	}
}
