package tool

import "testing"

func cond(key, op string, value any) map[string]any {
	return map[string]any{"key": key, "op": op, "value": value}
}

func TestRelaxedMetadataFilterKeepsProject(t *testing.T) {
	filter := map[string]any{
		"logic": "and",
		"conditions": []any{
			cond("project", "contains", "2018_055 nst"),
			cond("phase", "in", []any{"sp"}),
			cond("flow", "=", "expedition"),
		},
	}
	relaxed, changed := relaxedMetadataFilter(filter)
	if !changed {
		t.Fatal("expected changed=true")
	}
	conds, _ := relaxed["conditions"].([]any)
	if len(conds) != 1 || conds[0].(map[string]any)["key"] != "project" {
		t.Fatalf("expected only project condition, got %v", conds)
	}
	if len(filter["conditions"].([]any)) != 3 {
		t.Fatal("original filter was mutated")
	}
}

func TestRelaxedMetadataFilterNoProjectDropsConditions(t *testing.T) {
	filter := map[string]any{
		"logic":      "and",
		"conditions": []any{cond("phase", "=", "sp"), cond("flow", "=", "in")},
	}
	relaxed, changed := relaxedMetadataFilter(filter)
	if !changed {
		t.Fatal("expected changed=true")
	}
	if _, ok := relaxed["conditions"]; ok {
		t.Fatalf("expected conditions removed, got %v", relaxed["conditions"])
	}
}

func TestRelaxedMetadataFilterNoChangeWhenOnlyProject(t *testing.T) {
	filter := map[string]any{
		"conditions": []any{cond("project", "contains", "2018_055")},
	}
	if _, changed := relaxedMetadataFilter(filter); changed {
		t.Fatal("expected changed=false for project-only filter")
	}
}

func TestRelaxedMetadataFilterNoConditions(t *testing.T) {
	filter := map[string]any{"logic": "and"}
	if got, changed := relaxedMetadataFilter(filter); changed || len(got) != 1 {
		t.Fatal("expected no change for filter without conditions")
	}
}
