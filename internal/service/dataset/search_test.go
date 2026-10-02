package dataset

import (
	"testing"

	"ragflow/internal/service"
)

func TestSearchDatasetRequestToSearchDatasetsRequest(t *testing.T) {
	page := 2
	pageSize := 15
	knnTopK := 128
	knnNumCandidates := 256
	useKG := true
	tocEnhance := true
	keyword := true
	similarityThreshold := 0.42
	vectorSimilarityWeight := 0.8
	searchID := "search-1"
	rerankID := "rerank-1"
	chatID := "glm-4@ZHIPU"
	includeKnowledgeCompilation := false
	req := &service.SearchDatasetRequest{
		Question:               "hello world",
		Page:                   &page,
		PageSize:               &pageSize,
		DocumentIDs:            []string{"doc-1", "doc-2"},
		UseKG:                  &useKG,
		TOCEnhance:             &tocEnhance,
		KNNTopK:                &knnTopK,
		KNNNumCandidates:       &knnNumCandidates,
		CrossLanguages:         []string{"en", "zh"},
		SearchID:               &searchID,
		MetadataCondition:      map[string]interface{}{"logic": "and"},
		RerankID:               &rerankID,
		ChatID:                 &chatID,
		Keyword:                &keyword,
		SimilarityThreshold:    &similarityThreshold,
		VectorSimilarityWeight: &vectorSimilarityWeight,
		IncludeCompiledChunks:  &includeKnowledgeCompilation,
	}

	converted := req.ToSearchDatasetsRequest("dataset-1")
	if len(converted.DatasetIDs) != 1 || converted.DatasetIDs[0] != "dataset-1" {
		t.Fatalf("dataset_ids=%v want [dataset-1]", converted.DatasetIDs)
	}
	if converted.Question != req.Question || converted.Page != req.Page || converted.PageSize != req.PageSize {
		t.Fatalf("converted request did not preserve pagination/question fields: %#v", converted)
	}
	if len(converted.DocumentIDs) != 2 || converted.DocumentIDs[0] != "doc-1" || converted.DocumentIDs[1] != "doc-2" {
		t.Fatalf("document_ids=%v want [doc-1 doc-2]", converted.DocumentIDs)
	}
	if converted.UseKG != req.UseKG || converted.KNNTopK != req.KNNTopK || converted.KNNNumCandidates != req.KNNNumCandidates || converted.SearchID != req.SearchID || converted.TOCEnhance != req.TOCEnhance {
		t.Fatalf("converted request did not preserve optional fields: %#v", converted)
	}
	if converted.MetadataCondition["logic"] != "and" || converted.RerankID != req.RerankID || converted.ChatID != req.ChatID || converted.Keyword != req.Keyword {
		t.Fatalf("converted request did not preserve search config fields: %#v", converted)
	}
	if converted.SimilarityThreshold != req.SimilarityThreshold || converted.VectorSimilarityWeight != req.VectorSimilarityWeight {
		t.Fatalf("converted request did not preserve thresholds: %#v", converted)
	}
	if converted.IncludeCompiledChunks != req.IncludeCompiledChunks {
		t.Fatalf("converted request did not preserve include_knowledge_compilation: %#v", converted)
	}
}

func TestChatModelTenantID(t *testing.T) {
	// A chat_id names one of the requesting user's models, which may differ
	// from the tenant that owns the dataset.
	if got := chatModelTenantID("user-1", "kb-tenant", "glm-4@ZHIPU"); got != "user-1" {
		t.Fatalf("with chat_id got %q, want user-1", got)
	}
	if got := chatModelTenantID("user-1", "kb-tenant", ""); got != "kb-tenant" {
		t.Fatalf("without chat_id got %q, want kb-tenant", got)
	}
}
