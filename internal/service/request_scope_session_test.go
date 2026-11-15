package service

import (
	"context"
	"errors"
	"testing"

	agruntime "ragflow/internal/agent/runtime"
	"ragflow/internal/entity"
)

func scopedContext(t *testing.T, project string) (context.Context, string) {
	t.Helper()
	raw := []byte(`{"method":"manual","logic":"and","dataset_ids":["kb-1"],"manual":[{"key":"project_id","op":"in","value":["` + project + `"]}]}`)
	scope, err := agruntime.ParseRequestScope(raw)
	if err != nil {
		t.Fatalf("parse scope: %v", err)
	}
	return agruntime.WithRequestScope(context.Background(), scope), scope.Fingerprint()
}

func TestSessionScopeBoundary(t *testing.T) {
	_, fpA := scopedContext(t, "2018_055")
	_, fpB := scopedContext(t, "2020_013")

	for _, tc := range []struct {
		name     string
		session  *entity.API4Conversation
		incoming string
		wantErr  bool
	}{
		{"nil_session", nil, fpA, false},
		{"matching_scope", &entity.API4Conversation{ID: "s1", Round: 2, DSL: entity.JSONMap{requestScopeFingerprintKey: fpA}}, fpA, false},
		{"both_unscoped", &entity.API4Conversation{ID: "s2", Round: 3, DSL: entity.JSONMap{}}, "", false},
		{"fresh_session_adopts_scope", &entity.API4Conversation{ID: "s3", Round: 0, DSL: entity.JSONMap{}}, fpA, false},
		{"scoped_history_rejects_new_scope", &entity.API4Conversation{ID: "s4", Round: 1, DSL: entity.JSONMap{requestScopeFingerprintKey: fpA}}, fpB, true},
		{"scoped_history_rejects_unscoped", &entity.API4Conversation{ID: "s5", Round: 1, DSL: entity.JSONMap{requestScopeFingerprintKey: fpA}}, "", true},
		{"unscoped_history_rejects_scope", &entity.API4Conversation{ID: "s6", Round: 2, DSL: entity.JSONMap{}}, fpA, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSessionScopeBoundary(tc.session, tc.incoming)
			if tc.wantErr && !errors.Is(err, ErrAgentSessionScopeMismatch) {
				t.Fatalf("expected scope mismatch error, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestScopedDatasetBindings(t *testing.T) {
	dsl := map[string]any{
		"components": map[string]any{
			"retrieval:abc": map[string]any{
				"obj": map[string]any{
					"params": map[string]any{
						"dataset_ids": []any{"kb-1", "kb-2"},
						"kb_ids":      []any{"kb-3"},
					},
				},
			},
			"begin:xyz": map[string]any{"obj": map[string]any{"params": map[string]any{"prologue": "hi"}}},
			"broken":    "not-a-dict",
		},
		"globals": map[string]any{
			"sys.dataset_id":  "kb-4",
			"sys.dataset_ids": []any{"kb-5"},
			"sys.user_id":     "user-1",
		},
	}
	bound := scopedDatasetBindings(dsl)
	for _, id := range []string{"kb-1", "kb-2", "kb-3", "kb-4", "kb-5"} {
		if !bound[id] {
			t.Fatalf("expected %s in bindings %v", id, bound)
		}
	}
	if len(bound) != 5 {
		t.Fatalf("unexpected bindings: %v", bound)
	}
	if got := scopedDatasetBindings(map[string]any{}); len(got) != 0 {
		t.Fatalf("expected no bindings, got %v", got)
	}
}

func TestPreflightRequestScope(t *testing.T) {
	svc := &AgentService{}
	dsl := map[string]any{
		"components": map[string]any{
			"retrieval:abc": map[string]any{
				"obj": map[string]any{"params": map[string]any{"dataset_ids": []any{"kb-1"}}},
			},
		},
	}

	// No scope → no preflight.
	if err := svc.preflightRequestScope(context.Background(), "caller", "owner", dsl, nil); err != nil {
		t.Fatalf("unscoped request rejected: %v", err)
	}

	// Target outside the agent's bindings fails before any storage lookup.
	unboundRaw := []byte(`{"method":"manual","logic":"and","dataset_ids":["kb-other"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]}]}`)
	unbound, err := agruntime.ParseRequestScope(unboundRaw)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.preflightRequestScope(context.Background(), "caller", "owner", dsl, unbound); !errors.Is(err, ErrRequestScopeInvalid) {
		t.Fatalf("expected ErrRequestScopeInvalid for unbound dataset, got %v", err)
	}
}
