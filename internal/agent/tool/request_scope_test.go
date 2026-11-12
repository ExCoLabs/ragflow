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
	"ragflow/internal/entity"
	mcpclient "ragflow/internal/mcp/client"
	"ragflow/internal/permission"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type scopedMCPChecker struct {
	RetrievalService
	calls int
	err   error
}

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
	} {
		if _, err := authorizeScopedMCP(ctx, tc.name, json.RawMessage(tc.args)); err == nil {
			t.Fatalf("unsafe scoped call accepted: %s", tc.name)
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
