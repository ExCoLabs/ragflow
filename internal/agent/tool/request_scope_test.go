package tool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/entity"
	modelModule "ragflow/internal/entity/models"
	mcpclient "ragflow/internal/mcp/client"
	"ragflow/internal/permission"
	"ragflow/internal/service/nlp"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type scopedMCPChecker struct {
	RetrievalService
	calls int
	err   error
}

type unwrapStub struct {
	RetrievalService
	svc RetrievalService
}

func (u *unwrapStub) Unwrap() RetrievalService { return u.svc }

func (c *scopedMCPChecker) checkDocumentScope(_ context.Context, _ *gorm.DB, doc, dataset string) (scopedDocument, error) {
	c.calls++
	return scopedDocument{datasetID: "kb", documentID: doc, documentName: "canonical.xlsx"}, c.err
}

func scopedToolContext(t *testing.T, user, dataset string) context.Context {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"method": "manual", "dataset_ids": []string{dataset}, "manual": []any{map[string]any{"key": "phase", "op": "in", "value": []string{"sp"}}}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := runtime.ParseRequestScope(raw)
	if err != nil {
		t.Fatal(err)
	}
	state := runtime.NewCanvasState("run", "task")
	state.Sys["user_id"] = user
	return runtime.WithRequestScope(runtime.WithState(t.Context(), state), scope)
}

func TestScopedMCPRequiresDocumentIdentityAndChecker(t *testing.T) {
	previous := GetRetrievalService()
	t.Cleanup(func() { SetRetrievalService(previous) })
	checker := &scopedMCPChecker{}
	SetRetrievalService(checker)
	ctx := scopedToolContext(t, "owner", "kb")
	for _, tc := range []struct{ name, args string }{
		{"unsupported_search", `{"document_id":"doc"}`},
		{"document_read", `{"document_name":"same.pdf"}`},
		{"document_search", `{"document_id":123}`},
		{"file_query", `{"document_id":"doc","dataset_id":123}`},
		{"file_preview", `[]`},
		{"file_sheets", `null`},
		{"navigo_project_people", `{"project_number":"p","document_name":"x.pdf"}`},
		{"navigo_find_firm", `{"query":"firm","dataset_id":"kb"}`},
	} {
		if _, err := authorizeScopedMCP(ctx, tc.name, json.RawMessage(tc.args)); err == nil {
			t.Fatalf("unsafe scoped call accepted: %s", tc.name)
		}
	}
	// Non-document MCP tools (CRM lookups, calculators) pass through
	// unchanged — they carry no dataset identity to enforce.
	for _, tc := range []struct{ name, args string }{
		{"navigo_project_people", `{"project_number":"2018_055"}`},
		{"navigo_find_firm", `{"query":"siebert"}`},
	} {
		raw := json.RawMessage(tc.args)
		got, err := authorizeScopedMCP(ctx, tc.name, raw)
		if err != nil || string(got) != string(raw) {
			t.Fatalf("unrelated MCP tool blocked under scope: %s err=%v", tc.name, err)
		}
	}
	if checker.calls != 0 {
		t.Fatal("invalid calls reached the checker")
	}
	for _, name := range []string{"document_read", "document_search", "file_query", "file_preview", "file_sheets"} {
		raw, err := authorizeScopedMCP(ctx, name, json.RawMessage(`{"document_id":"doc","document_name":"untrusted.pdf","query":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil || args["document_id"] != "doc" || args["dataset_id"] != "kb" || args["document_name"] != "canonical.xlsx" || args["query"] != "x" {
			t.Fatalf("authorized arguments = %s, %v", raw, err)
		}
	}
	failure := errors.New("scope unavailable")
	checker.err = failure
	if _, err := authorizeScopedMCP(ctx, "document_read", json.RawMessage(`{"document_id":"doc"}`)); !errors.Is(err, failure) {
		t.Fatalf("checker failure did not propagate: %v", err)
	}
	SetRetrievalService(&struct{ RetrievalService }{})
	if _, err := authorizeScopedMCP(ctx, "document_read", json.RawMessage(`{"document_id":"doc"}`)); err == nil {
		t.Fatal("missing scope checker must fail closed")
	}
	// The registry may hold a bridge wrapper; the inner checker must still
	// be found through Unwrap.
	checker.err = nil
	SetRetrievalService(&unwrapStub{svc: checker})
	if _, err := authorizeScopedMCP(ctx, "document_read", json.RawMessage(`{"document_id":"doc"}`)); err != nil {
		t.Fatalf("wrapped scope checker not resolved: %v", err)
	}
	raw := json.RawMessage(`{"document_name":"legacy.pdf"}`)
	if got, err := authorizeScopedMCP(t.Context(), "legacy_tool", raw); err != nil || string(got) != string(raw) {
		t.Fatal("unfiltered legacy tool changed")
	}
}

func TestScopedMCPRejectsBeforeNetwork(t *testing.T) {
	defer mcpLoopbackOverride(t)()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusInternalServerError) }))
	defer server.Close()
	adapter := NewMCPToolAdapterFull(mcpclient.Tool{Name: "document_read"}, server.URL, nil, time.Second, server.Client())
	adapter.SetVisibleName("document_read_alias")
	_, err := adapter.InvokableRun(scopedToolContext(t, "owner", "kb"), `{"document_name":"unverified.pdf"}`)
	if err == nil || !strings.Contains(err.Error(), "require document_id") || calls != 0 {
		t.Fatalf("request escaped before scope validation: calls=%d error=%v", calls, err)
	}
}

type scopedRetrievalRecorder struct{ calls int }

func (r *scopedRetrievalRecorder) Search(context.Context, *gorm.DB, RetrievalRequest) ([]RetrievalChunk, error) {
	r.calls++
	return nil, nil
}

func TestScopedRetrievalCannotOverrideBindings(t *testing.T) {
	previous := GetRetrievalService()
	t.Cleanup(func() { SetRetrievalService(previous) })
	for _, tc := range []struct {
		name, configured, arguments string
		scoped, wantErr             bool
	}{
		{"fixed_project", "kb", `{"query":"q"}`, true, false},
		{"fixed_methodology", "methodology", `{"query":"q"}`, true, false},
		{"project_redirect", "kb", `{"query":"q","dataset_ids":["other"]}`, true, true},
		{"methodology_redirect", "methodology", `{"query":"q","dataset_ids":["kb"]}`, true, true},
		{"added_dataset", "kb", `{"query":"q","dataset_ids":["kb","other"]}`, true, true},
		{"dynamic_binding", "begin@dataset", `{"query":"q"}`, true, true},
		{"memory_override", "kb", `{"query":"q","retrieval_from":"memory","memory_ids":["mem"]}`, true, true},
		{"legacy_behavior", "kb", `{"query":"q","dataset_ids":["other"]}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &scopedRetrievalRecorder{}
			SetRetrievalService(recorder)
			ctx := t.Context()
			if tc.scoped {
				ctx = scopedToolContext(t, "owner", "kb")
			}
			adapter := NewRetrievalToolWithDefaults(retrievalArgs{RetrievalFrom: "dataset", DatasetIDs: []string{tc.configured}})
			_, err := adapter.InvokableRun(ctx, tc.arguments)
			if (err != nil) != tc.wantErr || (recorder.calls > 0) == tc.wantErr {
				t.Fatalf("search calls=%d error=%v", recorder.calls, err)
			}
		})
	}
}

func TestScopedRetrievalAcceptsImplicitDatasetBinding(t *testing.T) {
	previous := GetRetrievalService()
	t.Cleanup(func() { SetRetrievalService(previous) })
	// Persisted agent tools carry dataset_ids without retrieval_from; the
	// scope check must apply the same implicit-dataset fallback as arg
	// normalization instead of rejecting the configured binding.
	recorder := &scopedRetrievalRecorder{}
	SetRetrievalService(recorder)
	ctx := scopedToolContext(t, "owner", "kb")
	adapter := NewRetrievalToolWithDefaults(retrievalArgs{DatasetIDs: []string{"kb"}})
	if _, err := adapter.InvokableRun(ctx, `{"query":"q"}`); err != nil {
		t.Fatalf("implicit dataset binding rejected under scope: %v", err)
	}
	if recorder.calls != 1 {
		t.Fatalf("retrieval did not reach the service: calls=%d", recorder.calls)
	}
}

func TestCheckDocumentScopeEnforcesActorAndMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, user, target, requested, doc string
		canonical                          []string
		wantOK                             bool
	}{
		{"authorized", "owner", "kb", "", "doc", []string{"doc"}, true},
		{"missing_metadata", "owner", "kb", "", "doc", nil, false},
		{"foreign_metadata_match", "owner", "kb", "", "doc", []string{"other"}, false},
		{"wrong_dataset_argument", "owner", "kb", "other", "doc", []string{"doc"}, false},
		{"outside_target", "owner", "other", "", "doc", []string{"doc"}, false},
		{"unauthorized_actor", "outsider", "kb", "", "doc", []string{"doc"}, false},
		{"missing_document", "owner", "kb", "", "absent", []string{"absent"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&entity.Document{}, &entity.Knowledgebase{}, &entity.UserTenant{}); err != nil {
				t.Fatal(err)
			}
			valid := string(entity.StatusValid)
			for _, row := range []any{
				&entity.Knowledgebase{ID: "kb", TenantID: "tenant", CreatedBy: "owner", Permission: string(entity.TenantPermissionMe), Status: &valid},
				&entity.UserTenant{ID: "membership", UserID: "owner", TenantID: "tenant", Role: string(permission.RoleOwner), InvitedBy: "owner", Status: &valid},
				&entity.Document{ID: "doc", KbID: "kb", ParserConfig: entity.JSONMap{}, Status: &valid},
			} {
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			adapter := &NLPRetrievalAdapter{enhancer: &projectPipelineEnhancer{canonical: tc.canonical}}
			got, err := adapter.checkDocumentScope(scopedToolContext(t, tc.user, tc.target), db, tc.doc, tc.requested)
			if (err == nil) != tc.wantOK || (tc.wantOK && (got.datasetID != "kb" || got.documentID != "doc")) {
				t.Fatalf("document=%+v error=%v", got, err)
			}
		})
	}
}

// relaxScopeEnhancer resolves the request scope per filter level: a filter
// still containing "flow" yields only the strict doc, a relaxed filter yields
// the relaxed doc. Engine rows exist only for the relaxed doc, so the strict
// level must come back sparse and trigger the bounded relaxation.
type relaxScopeEnhancer struct {
	retrievalEnhancer
	calls []map[string]any
}

func relaxFilterHasKey(filter map[string]any, key string) bool {
	for _, c := range filter["manual"].([]any) {
		if c.(map[string]any)["key"] == key {
			return true
		}
	}
	return false
}

func (e *relaxScopeEnhancer) FilterDocuments(_ context.Context, filter map[string]any, _ string, _ *modelModule.ChatModel, base, kbs []string, _ *common.MetadataFilterDiagnostic) ([]string, error) {
	e.calls = append(e.calls, filter)
	if relaxFilterHasKey(filter, "flow") {
		return []string{"doc-strict"}, nil
	}
	return []string{"doc-relaxed"}, nil
}

func (e *relaxScopeEnhancer) LabelQuestion(context.Context, string, []*entity.Knowledgebase) map[string]float64 {
	return nil
}
func (e *relaxScopeEnhancer) EnhanceTOC(_ context.Context, _ *modelModule.ChatModel, _, _ []string, _ string, _ int, rows []map[string]any) ([]map[string]any, error) {
	return rows, nil
}
func (e *relaxScopeEnhancer) RetrieveByChildren(_ context.Context, rows []map[string]any, _ []string) []map[string]any {
	return rows
}

// relaxRows gives the relaxed doc three confident hits; strict-only fixtures
// that need weak strict hits swap them in via newRelaxPipelineRows.
func relaxRows(kbID string) []map[string]any {
	return []map[string]any{
		{"id": "w1", "doc_id": "doc-relaxed", "docnm_kwd": "TS-a.pdf", "kb_id": kbID, "content_ltks": "acoustic", "content_with_weight": "acoustic measures", "_score": 0.9},
		{"id": "w2", "doc_id": "doc-relaxed", "docnm_kwd": "TS-a.pdf", "kb_id": kbID, "content_ltks": "acoustic", "content_with_weight": "acoustic measures detail", "_score": 0.8},
		{"id": "w3", "doc_id": "doc-relaxed", "docnm_kwd": "TS-a.pdf", "kb_id": kbID, "content_ltks": "acoustic", "content_with_weight": "acoustic measures table", "_score": 0.7},
	}
}

// knnEngine overrides the flat 0.9 vector similarity so a level can be made
// to look weak without changing which documents the scope selects.
type knnEngine struct {
	projectPipelineEngine
	score float64
}

func (e *knnEngine) KNNScores(_ context.Context, rows []map[string]any, _ []float64, _ int) (map[string]any, error) {
	out := make(map[string]any)
	for _, r := range rows {
		out[r["id"].(string)] = e.score
	}
	return out, nil
}

func newRelaxPipelineRows(t *testing.T, scopeJSON string, rows []map[string]any, knnScore float64) (*NLPRetrievalAdapter, *relaxScopeEnhancer, *projectPipelineEngine, *gorm.DB, context.Context) {
	t.Helper()
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
	kb := &entity.Knowledgebase{ID: "kb-project", TenantID: "tenant", CreatedBy: "owner", Permission: string(entity.TenantPermissionMe), Status: &valid}
	if err = db.Create(kb).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&entity.UserTenant{ID: "membership", UserID: "owner", TenantID: "tenant", Role: string(permission.RoleOwner), InvitedBy: "owner", Status: &valid}).Error; err != nil {
		t.Fatal(err)
	}
	for _, docID := range []string{"doc-strict", "doc-relaxed"} {
		if err = db.Create(&entity.Document{ID: docID, KbID: kb.ID, ParserConfig: entity.JSONMap{}}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if rows == nil {
		rows = relaxRows(kb.ID)
	}
	docEngine := &projectPipelineEngine{rows: rows}
	var searchEngine engine.DocEngine = docEngine
	if knnScore > 0 {
		searchEngine = &knnEngine{projectPipelineEngine: *docEngine, score: knnScore}
	}
	enhancer := &relaxScopeEnhancer{}
	adapter := &NLPRetrievalAdapter{svc: nlp.NewRetrievalService(searchEngine, &dao.DocumentDAO{}), kbDAO: fakeKnowledgebaseLookup{kbs: []*entity.Knowledgebase{kb}}, enhancer: enhancer, modelConstructors: RetrievalModelConstructors{
		Embedding: func(context.Context, string, string) (*modelModule.EmbeddingModel, error) { return nil, nil },
		Chat:      func(context.Context, string, string) (*modelModule.ChatModel, error) { return nil, nil },
	}}
	scope, err := runtime.ParseRequestScope(json.RawMessage(scopeJSON))
	if err != nil {
		t.Fatal(err)
	}
	state := runtime.NewCanvasState("run", "task")
	state.Sys["user_id"] = "owner"
	ctx := runtime.WithRequestScope(runtime.WithState(t.Context(), state), scope)
	return adapter, enhancer, docEngine, db, ctx
}

func newRelaxPipeline(t *testing.T, scopeJSON string) (*NLPRetrievalAdapter, *relaxScopeEnhancer, *projectPipelineEngine, *gorm.DB, context.Context) {
	return newRelaxPipelineRows(t, scopeJSON, nil, 0)
}

func TestRequestScopeRelaxesOnlyDeclaredKeys(t *testing.T) {
	adapter, enhancer, _, db, ctx := newRelaxPipeline(t,
		`{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"flow","op":"in","value":["in"]}],"relax":["flow"]}`)
	weight, threshold := 1.0, 0.0
	diag := &common.MetadataFilterDiagnostic{}
	got, err := adapter.Search(ctx, db, RetrievalRequest{Query: "acoustic", TopN: 5, DatasetIDs: []string{"kb-project"}, KeywordsSimilarityWeight: &weight, SimilarityThreshold: &threshold, Diagnostics: diag})
	if err != nil {
		t.Fatal(err)
	}
	if len(enhancer.calls) != 2 {
		t.Fatalf("scope calls=%d want 2 (strict + relaxed)", len(enhancer.calls))
	}
	if !relaxFilterHasKey(enhancer.calls[0], "flow") || relaxFilterHasKey(enhancer.calls[1], "flow") {
		t.Fatalf("relaxation did not drop flow: %v", enhancer.calls)
	}
	if !relaxFilterHasKey(enhancer.calls[1], "project_id") {
		t.Fatal("undeclared key was relaxed")
	}
	if len(got) != 3 || got[0].DocumentID != "doc-relaxed" {
		t.Fatalf("chunks=%v", got)
	}
	if len(diag.ScopeDroppedKeys) != 1 || diag.ScopeDroppedKeys[0] != "flow" {
		t.Fatalf("diagnostics=%v", diag.ScopeDroppedKeys)
	}
}

func TestRequestScopeWithoutRelaxStaysStrict(t *testing.T) {
	adapter, enhancer, _, db, ctx := newRelaxPipeline(t,
		`{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"flow","op":"in","value":["in"]}]}`)
	weight, threshold := 1.0, 0.0
	got, err := adapter.Search(ctx, db, RetrievalRequest{Query: "acoustic", DatasetIDs: []string{"kb-project"}, KeywordsSimilarityWeight: &weight, SimilarityThreshold: &threshold})
	if err != nil {
		t.Fatal(err)
	}
	if len(enhancer.calls) != 1 {
		t.Fatalf("scope calls=%d want 1 (no relaxation declared)", len(enhancer.calls))
	}
	if len(got) != 0 {
		t.Fatalf("strict scope leaked chunks=%v", got)
	}
}

func TestRequestScopeRelaxesOnWeakHits(t *testing.T) {
	rows := []map[string]any{
		{"id": "s1", "doc_id": "doc-strict", "docnm_kwd": "TS-b.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic faint", "_score": 0.2},
		{"id": "s2", "doc_id": "doc-strict", "docnm_kwd": "TS-b.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic faint detail", "_score": 0.18},
		{"id": "s3", "doc_id": "doc-strict", "docnm_kwd": "TS-b.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic faint table", "_score": 0.15},
	}
	rows = append(rows, relaxRows("kb-project")...)
	adapter, enhancer, _, db, ctx := newRelaxPipelineRows(t,
		`{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"flow","op":"in","value":["in"]}],"relax":["flow"]}`,
		rows, 0.1)
	weight, threshold := 0.0, 0.0
	diag := &common.MetadataFilterDiagnostic{}
	got, err := adapter.Search(ctx, db, RetrievalRequest{Query: "acoustic", TopN: 5, DatasetIDs: []string{"kb-project"}, KeywordsSimilarityWeight: &weight, SimilarityThreshold: &threshold, Diagnostics: diag})
	if err != nil {
		t.Fatal(err)
	}
	if len(enhancer.calls) != 2 {
		scores := make([]float64, 0, len(got))
		for _, c := range got {
			scores = append(scores, c.Score)
		}
		t.Fatalf("scope calls=%d want 2 (weak strict page relaxes); got=%v scores=%v", len(enhancer.calls), len(got), scores)
	}
	if len(got) == 0 || got[0].DocumentID != "doc-relaxed" {
		t.Fatalf("expected relaxed-doc chunks on top, got %v", got)
	}
	if len(diag.ScopeDroppedKeys) != 1 || diag.ScopeDroppedKeys[0] != "flow" {
		t.Fatalf("diagnostics=%v", diag.ScopeDroppedKeys)
	}
}

func TestRequestScopeFullPageStaysStrict(t *testing.T) {
	rows := []map[string]any{
		{"id": "f1", "doc_id": "doc-strict", "docnm_kwd": "TS-c.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic a", "_score": 0.2},
		{"id": "f2", "doc_id": "doc-strict", "docnm_kwd": "TS-c.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic b", "_score": 0.19},
		{"id": "f3", "doc_id": "doc-strict", "docnm_kwd": "TS-c.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic c", "_score": 0.18},
		{"id": "f4", "doc_id": "doc-strict", "docnm_kwd": "TS-c.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic d", "_score": 0.17},
		{"id": "f5", "doc_id": "doc-strict", "docnm_kwd": "TS-c.pdf", "kb_id": "kb-project", "content_ltks": "acoustic", "content_with_weight": "acoustic e", "_score": 0.16},
	}
	adapter, enhancer, _, db, ctx := newRelaxPipelineRows(t,
		`{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"flow","op":"in","value":["in"]}],"relax":["flow"]}`,
		rows, 0)
	weight, threshold := 1.0, 0.0
	diag := &common.MetadataFilterDiagnostic{}
	got, err := adapter.Search(ctx, db, RetrievalRequest{Query: "acoustic", TopN: 5, DatasetIDs: []string{"kb-project"}, KeywordsSimilarityWeight: &weight, SimilarityThreshold: &threshold, Diagnostics: diag})
	if err != nil {
		t.Fatal(err)
	}
	if len(enhancer.calls) != 1 {
		t.Fatalf("scope calls=%d want 1 (full strict page wins)", len(enhancer.calls))
	}
	if len(got) != 5 || got[0].DocumentID != "doc-strict" {
		t.Fatalf("chunks=%v", got)
	}
	if len(diag.ScopeDroppedKeys) != 0 {
		t.Fatalf("unexpected relaxation: %v", diag.ScopeDroppedKeys)
	}
}
