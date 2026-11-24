package service

import (
	"context"
	"errors"
	"testing"

	agruntime "ragflow/internal/agent/runtime"
)

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
			"agent:tool": map[string]any{"obj": map[string]any{"params": map[string]any{
				"tools": []any{
					map[string]any{"component_name": "Retrieval", "params": map[string]any{"dataset_ids": []any{"kb-tool-1"}}},
					map[string]any{"component_name": "Retrieval", "params": map[string]any{"kb_ids": []any{"kb-tool-2"}}},
				},
			}}},
			"broken": "not-a-dict",
		},
		"globals": map[string]any{
			"sys.dataset_id":  "kb-4",
			"sys.dataset_ids": []any{"kb-5"},
			"sys.user_id":     "user-1",
		},
	}
	bound := scopedDatasetBindings(dsl)
	for _, id := range []string{"kb-1", "kb-2", "kb-3", "kb-4", "kb-5", "kb-tool-1", "kb-tool-2"} {
		if !bound[id] {
			t.Fatalf("expected %s in bindings %v", id, bound)
		}
	}
	if len(bound) != 7 {
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
