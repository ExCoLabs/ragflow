package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustRequestScope(t *testing.T, raw string) *RequestScope {
	t.Helper()
	scope, err := ParseRequestScope(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func TestRequestScopeValidation(t *testing.T) {
	for _, raw := range []string{
		`null`, `{}`, `[]`, `{"method":"auto","manual":[]}`,
		`{"method":"manual"}`, `{"method":"manual","manual":null}`,
		`{"method":"manual","logic":"or","manual":[]}`,
		`{"method":"manual","manual":[],"unknown":true}`,
		`{"method":"manual","manual":[]} {}`,
		`{"method":"manual","manual":[{"key":"project_id","op":"in","value":["2018_055"]}]}`,
		`{"method":"manual","dataset_ids":["@begin.dataset"],"manual":[]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"contains","value":["2018_055"]}]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":"2018_055"}]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":[]}]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":[1]}]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":[" "]}]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project.id","op":"in","value":["x"]}]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":["x"],"unknown":true}]}`,
		strings.Repeat(" ", 16385),
	} {
		t.Run(raw[:min(len(raw), 100)], func(t *testing.T) {
			if _, err := ParseRequestScope(json.RawMessage(raw)); err == nil {
				t.Fatalf("invalid scope accepted: %.200s", raw)
			}
		})
	}
	if scope, err := ParseRequestScope(nil); scope != nil || err != nil {
		t.Fatalf("absent filter changed legacy behavior: %v %v", scope, err)
	}
}

func TestRequestScopeIsImmutableAndPerTurn(t *testing.T) {
	a := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb-project","kb-project"],"manual":[{"key":"project_id","op":"in","value":["2018_055","2020_013"]}]}`)
	b := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb-other"],"manual":[{"key":"phase","op":"in","value":["sp"]}]}`)
	root := t.Context()
	first := WithRequestScope(root, a)
	second := WithRequestScope(root, b)
	if RequestScopeFromContext(root) != nil || RequestScopeFromContext(first) != a || RequestScopeFromContext(second) != b {
		t.Fatal("request scopes leaked between contexts")
	}
	cancelled, cancel := context.WithCancel(first)
	cancel()
	if RequestScopeFromContext(context.WithoutCancel(cancelled)) != a {
		t.Fatal("detached run context lost scope")
	}
	ids := a.DatasetIDs()
	ids[0] = "tampered"
	filter := a.Filter()
	filter["manual"].([]any)[0].(map[string]any)["value"].([]any)[0] = "tampered"
	if !reflect.DeepEqual(a.DatasetIDs(), []string{"kb-project"}) || a.Filter()["manual"].([]any)[0].(map[string]any)["value"].([]any)[0] != "2018_055" {
		t.Fatal("caller mutated authoritative scope")
	}
	if RequestScopeFromContext(WithRequestScope(first, nil)) != nil {
		t.Fatal("explicitly cleared context inherited old scope")
	}
}

func TestRequestScopeDatasetBoundary(t *testing.T) {
	scope := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]}]}`)
	for _, tc := range []struct {
		ids     []string
		applies bool
		wantErr bool
	}{
		{[]string{"kb-project"}, true, false},
		{[]string{"kb-methodology"}, false, false},
		{[]string{"kb-project", "kb-methodology"}, false, true},
	} {
		got, err := scope.AppliesTo(tc.ids)
		if got != tc.applies || (err != nil) != tc.wantErr {
			t.Fatalf("AppliesTo(%v) = %v, %v", tc.ids, got, err)
		}
	}
	empty := mustRequestScope(t, `{"method":"manual","manual":[]}`)
	if empty.HasFilters() {
		t.Fatal("empty manual filter must not impose a scope")
	}
	if got, err := empty.AppliesTo([]string{"kb-project", "kb-methodology"}); got || err != nil {
		t.Fatal("empty filter changed legacy retrieval")
	}
}

func TestRequestScopeFingerprintTracksSemanticChanges(t *testing.T) {
	a := mustRequestScope(t, `{"method":"manual","dataset_ids":["a","b"],"manual":[{"key":"project_id","op":"in","value":["2018_055","2020_013"]},{"key":"phase","op":"in","value":["sp"]}]}`)
	b := mustRequestScope(t, `{"method":"manual","dataset_ids":["b","a","b"],"manual":[{"key":"phase","op":"in","value":["sp"]},{"key":"project_id","op":"in","value":["2020_013","2018_055","2018_055"]}]}`)
	if len(a.Fingerprint()) != 64 || a.Fingerprint() != b.Fingerprint() {
		t.Fatal("equivalent scopes produced different fingerprints")
	}
	changed := mustRequestScope(t, `{"method":"manual","dataset_ids":["a","b"],"manual":[{"key":"project_id","op":"in","value":["2018_055","2020_013"]},{"key":"phase","op":"in","value":["rp"]}]}`)
	if a.Fingerprint() == changed.Fingerprint() {
		t.Fatal("changed phase did not invalidate the scope fingerprint")
	}
	var absent *RequestScope
	if absent.Fingerprint() != "" || mustRequestScope(t, `{"method":"manual","manual":[]}`).Fingerprint() != "" {
		t.Fatal("absent and empty scopes must share legacy semantics")
	}
}

func TestRequestScopeKeepsLiteralMetadataValues(t *testing.T) {
	scope := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project","op":"in","value":["{sys.query}"]}]}`)
	values := scope.Filter()["manual"].([]any)[0].(map[string]any)["value"]
	if !reflect.DeepEqual(values, []any{"{sys.query}"}) {
		t.Fatal("request filter values must remain literal")
	}
}

func TestRequestScopeRelaxLevels(t *testing.T) {
	scope := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"phase","op":"in","value":["sp"]},{"key":"flow","op":"in","value":["in"]}],"relax":["flow","phase"]}`)
	if scope.RelaxLevels() != 2 {
		t.Fatalf("levels=%d", scope.RelaxLevels())
	}
	keys := func(f map[string]any) []string {
		var out []string
		if f == nil {
			return out
		}
		for _, c := range f["manual"].([]any) {
			out = append(out, c.(map[string]any)["key"].(string))
		}
		return out
	}
	if got := keys(scope.FilterAtLevel(0)); !reflect.DeepEqual(got, []string{"project_id", "phase", "flow"}) {
		t.Fatalf("level0=%v", got)
	}
	if got := keys(scope.FilterAtLevel(1)); !reflect.DeepEqual(got, []string{"project_id", "phase"}) {
		t.Fatalf("level1=%v", got)
	}
	if got := keys(scope.FilterAtLevel(2)); !reflect.DeepEqual(got, []string{"project_id"}) {
		t.Fatalf("level2=%v", got)
	}
	if got := keys(scope.FloorFilter()); !reflect.DeepEqual(got, []string{"project_id"}) {
		t.Fatalf("floor=%v", got)
	}
	if !reflect.DeepEqual(scope.DroppedKeys(2), []string{"flow", "phase"}) || scope.DroppedKeys(0) != nil {
		t.Fatalf("dropped=%v", scope.DroppedKeys(2))
	}
	open := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"phase","op":"in","value":["sp"]}],"relax":["phase"]}`)
	if open.FloorFilter() != nil {
		t.Fatal("fully relaxed scope must have no metadata floor")
	}
}

func TestRequestScopeRelaxValidation(t *testing.T) {
	for _, raw := range []string{
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"phase","op":"in","value":["sp"]}],"relax":["flow"]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"phase","op":"in","value":["sp"]}],"relax":["phase","phase"]}`,
		`{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"phase","op":"in","value":["sp"]}],"relax":["bad.key"]}`,
	} {
		if _, err := ParseRequestScope(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid relax accepted: %s", raw)
		}
	}
}

func TestRequestScopeRelaxChangesFingerprint(t *testing.T) {
	a := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"flow","op":"in","value":["in"]}]}`)
	b := mustRequestScope(t, `{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"flow","op":"in","value":["in"]}],"relax":["flow"]}`)
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("relax policy must be part of the session scope boundary")
	}
}
