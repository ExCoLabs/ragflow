package tool

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/engine/types"
	"ragflow/internal/entity"
	modelModule "ragflow/internal/entity/models"
	"ragflow/internal/permission"
	"ragflow/internal/service/nlp"
)

type projectPipelineEngine struct {
	engine.DocEngine
	requests []*types.SearchRequest
	rows     []map[string]any
	err      error
}

func (e *projectPipelineEngine) GetType() string { return "elasticsearch" }
func (e *projectPipelineEngine) Search(_ context.Context, req *types.SearchRequest) (*types.SearchResult, error) {
	e.requests = append(e.requests, req)
	return &types.SearchResult{Chunks: e.rows, Total: int64(len(e.rows))}, e.err
}
func (e *projectPipelineEngine) GetChunkIDs(rows []map[string]any) []string {
	var ids []string
	for _, r := range rows {
		ids = append(ids, r["id"].(string))
	}
	return ids
}
func (e *projectPipelineEngine) GetFields(rows []map[string]any, _ []string) map[string]map[string]any {
	fields := make(map[string]map[string]any)
	for _, r := range rows {
		fields[r["id"].(string)] = maps.Clone(r)
	}
	return fields
}
func (e *projectPipelineEngine) GetAggregation([]map[string]any, string) []map[string]any { return nil }
func (e *projectPipelineEngine) KNNScores(_ context.Context, rows []map[string]any, _ []float64, _ int) (map[string]any, error) {
	out := make(map[string]any)
	for _, r := range rows {
		out[r["id"].(string)] = 0.9
	}
	return out, nil
}
func (e *projectPipelineEngine) GetScores(raw map[string]any) map[string]float64 {
	out := make(map[string]float64)
	for k, v := range raw {
		out[k] = v.(float64)
	}
	return out
}

type projectPipelineEnhancer struct {
	retrievalEnhancer
	canonical, generated       []string
	canonicalErr, generatedErr error
	seenBase                   []string
	tocErr                     error
	tocInput                   int
}

func (e *projectPipelineEnhancer) FilterDocuments(_ context.Context, filter map[string]any, _ string, model *modelModule.ChatModel, base, kbs []string, _ *common.MetadataFilterDiagnostic) ([]string, error) {
	if filter["method"] != "manual" || model != nil {
		return nil, errors.New("hard scope must not use LLM")
	}
	return e.canonical, e.canonicalErr
}
func (e *projectPipelineEnhancer) ScopeDocuments(_ context.Context, _ map[string]any, _ string, _ *modelModule.ChatModel, base []string, _ []*entity.Knowledgebase, d *common.MetadataFilterDiagnostic) ([]string, *common.ChunkMetaScope, error) {
	e.seenBase = slices.Clone(base)
	if d != nil {
		*d = common.MetadataFilterDiagnostic{Method: "semi_auto", Status: "no_matches"}
	}
	return e.generated, nil, e.generatedErr
}
func (e *projectPipelineEnhancer) LabelQuestion(context.Context, string, []*entity.Knowledgebase) map[string]float64 {
	return nil
}
func (e *projectPipelineEnhancer) EnhanceTOC(_ context.Context, _ *modelModule.ChatModel, _, _ []string, _ string, _ int, rows []map[string]any) ([]map[string]any, error) {
	e.tocInput = len(rows)
	return append(slices.Clone(rows), map[string]any{"chunk_id": "toc-foreign", "doc_id": "nst-doc", "content_with_weight": "unrelated TOC"}), e.tocErr
}
func (e *projectPipelineEnhancer) RetrieveByChildren(_ context.Context, rows []map[string]any, _ []string) []map[string]any {
	return append(slices.Clone(rows), map[string]any{"chunk_id": "parent-foreign", "doc_id": "nst-doc", "content_with_weight": "unrelated parent"})
}

func TestProjectScopeThroughRetrievalPipeline(t *testing.T) {
	if err := nlp.InitQueryBuilder(""); err != nil {
		t.Fatal(err)
	}
	backendErr := errors.New("backend unavailable")
	ids := make([]string, 120974)
	for i := range ids {
		ids[i] = fmt.Sprintf("project-doc-%d", i)
	}
	ids[len(ids)-1] = "bystricka-doc"
	for _, tc := range []struct {
		name                                          string
		canonical, generated, base                    []string
		canonicalErr, generatedErr, searchErr, tocErr error
		wantSearch, wantChunk, wantErr                bool
	}{
		{name: "generated no-match retains canonical scope", canonical: []string{"bystricka-doc"}, wantSearch: true, wantChunk: true},
		{name: "foreign generated results cannot escape", canonical: []string{"bystricka-doc"}, generated: []string{"nst-doc"}},
		{name: "explicit empty generated scope stays empty", canonical: []string{"bystricka-doc"}, generated: []string{}},
		{name: "manual no-match sentinel stays empty", canonical: []string{"bystricka-doc"}, generated: []string{"-999"}},
		{name: "existing document scope is authoritative", canonical: []string{"bystricka-doc"}, base: []string{"other-doc"}},
		{name: "no canonical match does not search dataset"},
		{name: "canonical failure propagates", canonicalErr: backendErr, wantErr: true},
		{name: "generated failure propagates", canonical: []string{"bystricka-doc"}, generatedErr: backendErr, wantErr: true},
		{name: "search failure propagates", canonical: []string{"bystricka-doc"}, searchErr: backendErr, wantSearch: true, wantErr: true},
		{name: "TOC failure still enforces scope", canonical: []string{"bystricka-doc"}, tocErr: backendErr, wantSearch: true, wantChunk: true},
		{name: "large scope reaches engine without truncation", canonical: ids, wantSearch: true, wantChunk: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err = db.AutoMigrate(&entity.Knowledgebase{}, &entity.UserTenant{}, &entity.Document{}); err != nil {
				t.Fatal(err)
			}
			previousDB := dao.DB
			dao.DB = db
			t.Cleanup(func() { dao.DB = previousDB })
			if err = db.Create(&entity.Document{ID: "bystricka-doc", ParserConfig: entity.JSONMap{}}).Error; err != nil {
				t.Fatal(err)
			}
			valid := string(entity.StatusValid)
			kb := &entity.Knowledgebase{ID: "kb-project", TenantID: "tenant", CreatedBy: "owner", Permission: string(entity.TenantPermissionMe), Status: &valid}
			if err = db.Create(kb).Error; err != nil {
				t.Fatal(err)
			}
			if err = db.Create(&entity.UserTenant{ID: "membership", UserID: "owner", TenantID: "tenant", Role: string(permission.RoleOwner), InvitedBy: "owner", Status: &valid}).Error; err != nil {
				t.Fatal(err)
			}
			docEngine := &projectPipelineEngine{rows: []map[string]any{{"id": "witness", "doc_id": "bystricka-doc", "kb_id": kb.ID, "content_ltks": "alpha ventilation", "content_with_weight": "alpha ventilation without opening windows", "_score": 0.9}}, err: tc.searchErr}
			enhancer := &projectPipelineEnhancer{canonical: tc.canonical, generated: tc.generated, canonicalErr: tc.canonicalErr, generatedErr: tc.generatedErr, tocErr: tc.tocErr}
			adapter := &NLPRetrievalAdapter{svc: nlp.NewRetrievalService(docEngine, &dao.DocumentDAO{}), kbDAO: fakeKnowledgebaseLookup{kbs: []*entity.Knowledgebase{kb}}, enhancer: enhancer, modelConstructors: RetrievalModelConstructors{
				Embedding: func(context.Context, string, string) (*modelModule.EmbeddingModel, error) { return nil, nil },
				Chat:      func(context.Context, string, string) (*modelModule.ChatModel, error) { return nil, nil },
			}}
			state := runtime.NewCanvasState("run", "task")
			state.Sys["user_id"] = "owner"
			weight, threshold := 1.0, 0.0
			diag := &common.MetadataFilterDiagnostic{}
			got, err := adapter.Search(runtime.WithState(t.Context(), state), db, RetrievalRequest{Query: "alpha ventilation", ProjectID: "2015_062", DatasetIDs: []string{kb.ID}, DocScope: tc.base, MetaDataFilter: projectFilterDefaults(), KeywordsSimilarityWeight: &weight, SimilarityThreshold: &threshold, TOCEnhance: true, Diagnostics: diag})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if tc.wantErr && !errors.Is(err, backendErr) {
				t.Fatalf("lost backend error: %v", err)
			}
			if (len(docEngine.requests) > 0) != tc.wantSearch {
				t.Fatalf("search calls=%d", len(docEngine.requests))
			}
			if tc.wantChunk {
				if len(got) != 1 || got[0].ID != "witness" || got[0].DocumentID != "bystricka-doc" {
					t.Fatalf("chunks=%v", got)
				}
			} else if len(got) > 0 {
				t.Fatalf("unexpected chunks=%v", got)
			}
			if diag.ProjectID != "2015_062" {
				t.Fatalf("project diagnostic=%v", diag)
			}
			for _, r := range docEngine.requests {
				scope := r.Filter["doc_id"].([]string)
				if !reflect.DeepEqual(scope, tc.canonical) {
					t.Fatalf("engine scope size=%d want=%d", len(scope), len(tc.canonical))
				}
			}
			if tc.wantSearch && !reflect.DeepEqual(enhancer.seenBase, tc.canonical) {
				t.Fatal("generated filter did not receive canonical base")
			}
		})
	}
}

// The retrieval page is widened to the rerank candidate budget so version
// dedup sees duplicates ranked below topN; TOC enhancement still receives at
// most topN chunks.
func TestCandidatePoolWideningAndTruncation(t *testing.T) {
	if err := nlp.InitQueryBuilder(""); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&entity.Knowledgebase{}, &entity.UserTenant{}, &entity.Document{}); err != nil {
		t.Fatal(err)
	}
	previousDB := dao.DB
	dao.DB = db
	t.Cleanup(func() { dao.DB = previousDB })
	valid := string(entity.StatusValid)
	kb := &entity.Knowledgebase{ID: "kb-pool", TenantID: "tenant", CreatedBy: "owner", Permission: string(entity.TenantPermissionMe), Status: &valid}
	if err = db.Create(kb).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&entity.UserTenant{ID: "membership", UserID: "owner", TenantID: "tenant", Role: string(permission.RoleOwner), InvitedBy: "owner", Status: &valid}).Error; err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"doc-old", "doc-keep", "doc-new"} {
		if err = db.Create(&entity.Document{ID: d, ParserConfig: entity.JSONMap{}}).Error; err != nil {
			t.Fatal(err)
		}
	}
	rows := []map[string]any{
		{"id": "old1", "doc_id": "doc-old", "docnm_kwd": "Brief(1).docx", "kb_id": kb.ID, "content_ltks": "brief", "content_with_weight": "old copy brief", "_score": 0.9},
		{"id": "old2", "doc_id": "doc-old", "docnm_kwd": "Brief(1).docx", "kb_id": kb.ID, "content_ltks": "brief", "content_with_weight": "old copy second brief", "_score": 0.85},
		{"id": "keep", "doc_id": "doc-keep", "docnm_kwd": "other.pdf", "kb_id": kb.ID, "content_ltks": "brief", "content_with_weight": "kept brief", "_score": 0.8},
		{"id": "new1", "doc_id": "doc-new", "docnm_kwd": "Brief(2).docx", "kb_id": kb.ID, "content_ltks": "brief", "content_with_weight": "new copy brief", "_score": 0.75},
	}
	docEngine := &projectPipelineEngine{rows: rows}
	enhancer := &projectPipelineEnhancer{}
	adapter := &NLPRetrievalAdapter{svc: nlp.NewRetrievalService(docEngine, &dao.DocumentDAO{}), kbDAO: fakeKnowledgebaseLookup{kbs: []*entity.Knowledgebase{kb}}, enhancer: enhancer, modelConstructors: RetrievalModelConstructors{
		Embedding: func(context.Context, string, string) (*modelModule.EmbeddingModel, error) { return nil, nil },
		Chat:      func(context.Context, string, string) (*modelModule.ChatModel, error) { return nil, nil },
	}}
	state := runtime.NewCanvasState("run", "task")
	state.Sys["user_id"] = "owner"
	weight, threshold := 1.0, 0.0
	got, err := adapter.Search(runtime.WithState(t.Context(), state), db, RetrievalRequest{Query: "brief", TopN: 2, RerankCandidatesCount: 4, DatasetIDs: []string{kb.ID}, KeywordsSimilarityWeight: &weight, SimilarityThreshold: &threshold, TOCEnhance: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(docEngine.requests) != 1 {
		t.Fatalf("search calls=%d", len(docEngine.requests))
	}
	if enhancer.tocInput != 2 {
		t.Fatalf("TOC input=%d want 2", enhancer.tocInput)
	}
	docs := map[string]bool{}
	for _, c := range got {
		docs[c.DocumentID] = true
	}
	if !docs["doc-old"] {
		t.Fatalf("unverified document content was dropped by filename: %v", got)
	}
}

func TestPreferNewestDocVersionsPreservesUnverifiedDocuments(t *testing.T) {
	chunks := []map[string]any{
		{"doc_id": "old", "docnm_kwd": "Brief_SK-R00_DSP2024(1).docx"},
		{"doc_id": "mid", "docnm_kwd": "Brief_SK-R00_DSP2024(2).docx"},
		{"doc_id": "new", "docnm_kwd": "Brief_SK-R00_DSP2024(3).docx"},
		{"doc_id": "dwg5", "docnm_kwd": "010_G00_DSP_E1_POZ_TSP_001_R00 - titulka.dwg(5).md"},
		{"doc_id": "src", "docnm_kwd": "010_G00_DSP_E1_POZ_TSP_001_R00.pdf"},
		{"doc_id": "solo", "docnm_kwd": "VÝSLEDKY z merania AMBER DNV.pdf"},
		{"doc_id": "nodup", "docnm_kwd": "plain.pdf"},
	}
	got := preferNewestDocVersions(chunks)
	ids := make([]string, 0, len(got))
	for _, c := range got {
		ids = append(ids, c["doc_id"].(string))
	}
	want := []string{"old", "mid", "new", "dwg5", "src", "solo", "nodup"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids=%v want=%v", ids, want)
	}
	if docBaseName("010_G00_DSP_E1_POZ_TSP_001_R00 - titulka.dwg(5).md") != docBaseName("010_G00_DSP_E1_POZ_TSP_001_R00.pdf") {
		t.Fatal("DWG derivative must group with its source document")
	}
}

type projectDiagnosticService struct{ runtime.RetrievalService }

func (projectDiagnosticService) Search(_ context.Context, _ *gorm.DB, req RetrievalRequest) ([]RetrievalChunk, error) {
	*req.Diagnostics = common.MetadataFilterDiagnostic{
		ProjectID:           req.ProjectID,
		OriginalConditions:  []map[string]any{{"key": "project_id", "op": "=", "value": req.ProjectID}, {"key": "phase", "op": "=", "value": "rp"}},
		Conditions:          []map[string]any{{"key": "project_id", "op": "=", "value": req.ProjectID}},
		RelaxationAttempted: true, RelaxationStatus: "applied",
	}
	return nil, nil
}

func TestProjectAndRelaxationDiagnosticsReachReferences(t *testing.T) {
	previous := GetRetrievalService()
	SetRetrievalService(projectDiagnosticService{})
	t.Cleanup(func() { SetRetrievalService(previous) })
	state := runtime.NewCanvasState("run", "task")
	state.Sys["query"] = "Projekt: 2015_062 Bystricka, Otázka: akustika?"
	rt := NewRetrievalToolWithDefaults(retrievalArgs{DatasetIDs: []string{"kb"}, MetaDataFilter: projectFilterDefaults()})
	if _, err := rt.InvokableRun(runtime.WithState(t.Context(), state), `{"query":"bystricka akustika"}`); err != nil {
		t.Fatal(err)
	}
	filters := state.Retrieval["metadata_filters"].([]any)
	got := filters[0].(map[string]any)
	if got["project_id"] != "2015_062" || got["relaxation_attempted"] != true || got["relaxation_status"] != "applied" {
		t.Fatalf("diagnostic=%v", got)
	}
	if len(got["original_conditions"].([]map[string]any)) != 2 || len(got["conditions"].([]map[string]any)) != 1 {
		t.Fatalf("conditions=%v", got)
	}
}

func projectFilterDefaults() map[string]any {
	return map[string]any{"method": "semi_auto", "semi_auto": []any{map[string]any{"key": "project", "op": "contains"}, map[string]any{"key": "phase", "op": "in"}}}
}

func TestProjectScopeUsesOriginalHint(t *testing.T) {
	for _, tc := range []struct {
		question, from, want string
		enabled, wantErr     bool
	}{
		{"Projekt: 2015_062 Bystricka, Fáza: Podklady, Otázka: opatrenia?", "dataset", "2015_062", true, false},
		{" Project: 2018_055 NST, Question: zero?", "dataset", "2018_055", true, false},
		{"aka je nula 2018_055?", "dataset", "", true, false},
		{"Projekt: 2015_062 Bystricka, Otázka: opatrenia?", "memory", "", true, false},
		{"Projekt: 2015_062 Bystricka, Otázka: opatrenia?", "dataset", "", false, false},
		{"Projekt: 2015_062 a 2018_055, Otázka: porovnaj", "dataset", "", true, true},
		{"Projekt: Bystricka, Otázka: opatrenia?", "dataset", "", true, true},
		{"Projekt: 2015_0629, Otázka: opatrenia?", "dataset", "", true, true},
	} {
		t.Run(tc.question+tc.from, func(t *testing.T) {
			state := runtime.NewCanvasState("run", "task")
			state.Sys["query"] = tc.question
			defaults := map[string]any{}
			if tc.enabled {
				defaults = projectFilterDefaults()
			}
			got, err := retrievalProjectID(runtime.WithState(t.Context(), state), tc.from, defaults)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("id=%q error=%v", got, err)
			}
		})
	}
}

func TestProjectScopeCannotBeOverriddenByToolArguments(t *testing.T) {
	prev := GetRetrievalService()
	svc := &capturingRetrievalService{}
	SetRetrievalService(svc)
	t.Cleanup(func() { SetRetrievalService(prev) })
	state := runtime.NewCanvasState("run", "task")
	state.Sys["query"] = "Projekt: 2015_062 Bystricka, Otázka: opatrenia?"
	rt := NewRetrievalToolWithDefaults(retrievalArgs{DatasetIDs: []string{"kb"}, MetaDataFilter: projectFilterDefaults()})
	_, err := rt.InvokableRun(runtime.WithState(t.Context(), state), `{"query":"2018_055 NST unrelated","project_id":"2018_055","meta_data_filter":{"method":"disabled"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if svc.req.ProjectID != "2015_062" {
		t.Fatalf("scope=%q", svc.req.ProjectID)
	}
}

type projectScopeEnhancer struct {
	retrievalEnhancer
	ids    []string
	err    error
	filter map[string]any
	base   []string
}

func (e *projectScopeEnhancer) FilterDocuments(_ context.Context, filter map[string]any, _ string, model *modelModule.ChatModel, base, kbs []string, _ *common.MetadataFilterDiagnostic) ([]string, error) {
	if model != nil {
		return nil, errors.New("hard scope must not generate a filter")
	}
	e.filter = filter
	e.base = append([]string(nil), base...)
	return e.ids, e.err
}

func TestCanonicalProjectScopeIsManualAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ids     []string
		err     error
		wantErr bool
	}{
		{"resolved", []string{"bystricka-doc"}, nil, false},
		{"no match", nil, nil, false},
		{"backend error", nil, errors.New("metadata unavailable"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &projectScopeEnhancer{ids: tc.ids, err: tc.err}
			a := &NLPRetrievalAdapter{enhancer: e}
			got, err := a.resolveProjectScope(t.Context(), RetrievalRequest{ProjectID: "2015_062", DatasetIDs: []string{"kb"}}, []string{"bystricka-doc", "nst-doc"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if err == nil && !reflect.DeepEqual(got, tc.ids) {
				t.Fatalf("scope=%v", got)
			}
			want := map[string]any{"method": "manual", "logic": "and", "manual": []any{map[string]any{"key": "project_id", "op": "in", "value": "2015_062"}}}
			if !reflect.DeepEqual(e.filter, want) {
				t.Fatalf("filter=%v", e.filter)
			}
			if !reflect.DeepEqual(e.base, []string{"bystricka-doc", "nst-doc"}) {
				t.Fatalf("base=%v", e.base)
			}
		})
	}
}

func TestProjectScopeIntersectionAndEnhancementGuard(t *testing.T) {
	canonical := []string{"bystricka-doc"}
	for _, scope := range [][]string{nil, {"bystricka-doc", "nst-doc"}} {
		got := intersectProjectScope(canonical, scope)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("scope=%v got=%v", scope, got)
		}
	}
	if got := intersectProjectScope(canonical, []string{"nst-doc"}); len(got) != 0 {
		t.Fatalf("foreign scope=%v", got)
	}
	chunks := []map[string]any{{"doc_id": "bystricka-doc"}, {"doc_id": "nst-doc"}, {"content": "missing document id"}}
	got := projectScopedChunks(chunks, canonical)
	if len(got) != 1 || got[0]["doc_id"] != "bystricka-doc" {
		t.Fatalf("chunks=%v", got)
	}
}
