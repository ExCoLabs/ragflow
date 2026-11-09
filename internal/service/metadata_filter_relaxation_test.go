package service

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"ragflow/internal/common"
)

func TestGeneratedMetadataRelaxation(t *testing.T) {
	meta := common.MetaData{
		"project": {"P": {"p1", "p2"}, "Q": {"q1"}},
		"phase":   {"sp": {"p1", "p2", "q1"}},
		"flow":    {"in": {"p1", "q1"}, "out": {"p2"}},
		"sector":  {"G": {"p1", "q1"}, "H": {"p2"}},
	}
	for _, tc := range []struct {
		name, method, answer string
		enabled              bool
		base, want           []string
		attempted            bool
	}{
		{"semi auto preserves project and base", "semi_auto", `{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`, true, []string{"p2", "q1"}, []string{"p2"}, true},
		{"auto preserves project", "auto", `{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`, true, nil, []string{"p1", "p2"}, true},
		{"successful filter is unchanged", "semi_auto", `{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"phase","op":"=","value":"sp"}],"logic":"and"}`, true, nil, []string{"p1", "p2"}, false},
		{"no project cannot broaden", "semi_auto", `{"conditions":[{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`, true, nil, []string{NoMatchDocIDSentinel}, false},
		{"negative project cannot broaden", "semi_auto", `{"conditions":[{"key":"project","op":"not contains","value":"Q"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`, true, nil, []string{NoMatchDocIDSentinel}, false},
		{"or cannot broaden", "semi_auto", `{"conditions":[{"key":"project","op":"=","value":"missing"},{"key":"phase","op":"=","value":"rp"}],"logic":"or"}`, true, nil, []string{NoMatchDocIDSentinel}, false},
		{"sector is retained", "semi_auto", `{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"sector","op":"=","value":"missing"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`, true, nil, []string{NoMatchDocIDSentinel}, true},
		{"empty base intersection stays empty", "semi_auto", `{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`, true, []string{"q1"}, []string{NoMatchDocIDSentinel}, true},
		{"disabled retains existing behavior", "semi_auto", `{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`, false, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := map[string]interface{}{"method": tc.method, "semi_auto": []interface{}{"project", "phase", "sector", "flow"}}
			if tc.enabled {
				filter["relax_generated_keys"] = []interface{}{"phase", "flow"}
			}
			before, _ := json.Marshal(filter)
			var diag common.MetadataFilterDiagnostic
			got := ApplyMetaDataFilterWithDiagnostics(context.Background(), filter, meta, "question", metaFilterChatModel(tc.answer), tc.base, nil, &diag)
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scope=%v want=%v diagnostic=%+v", got, tc.want, diag)
			}
			if diag.RelaxationAttempted != tc.attempted {
				t.Fatalf("attempted=%v want=%v", diag.RelaxationAttempted, tc.attempted)
			}
			if tc.attempted && (len(diag.OriginalConditions) == 0 || len(diag.Conditions) == 0 || diag.RelaxationStatus == "") {
				t.Fatalf("incomplete audit: %+v", diag)
			}
			after, _ := json.Marshal(filter)
			if string(before) != string(after) {
				t.Fatal("filter mutated")
			}
		})
	}
}

func TestMetadataScopeUsesGeneratedRelaxation(t *testing.T) {
	for _, active := range []bool{false, true} {
		config := &common.ChunkMetadataConfig{Enabled: active, Ready: active, Fields: []string{"project", "phase"}}
		filter := map[string]interface{}{"method": "semi_auto", "semi_auto": []interface{}{"project", "phase"}, "relax_generated_keys": []interface{}{"phase"}}
		meta := common.MetaData{"project": {"P": {"p1"}, "Q": {"q1"}}, "phase": {"sp": {"p1", "q1"}}}
		var diagnostic common.MetadataFilterDiagnostic
		scope := ApplyMetaDataScopeWithDiagnostics(context.Background(), filter, meta, "question", metaFilterChatModel(`{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`), []string{"p1", "q1"}, nil, config, &diagnostic)
		if !reflect.DeepEqual(scope.DocIDs, []string{"p1"}) || !diagnostic.RelaxationAttempted || diagnostic.RelaxationStatus != "applied" {
			t.Fatalf("active=%v scope=%+v diagnostic=%+v", active, scope, diagnostic)
		}
	}
}

func TestGeneratedRelaxationWithNilDiagnostics(t *testing.T) {
	filter := map[string]interface{}{"method": "semi_auto", "semi_auto": []interface{}{"project", "phase"}, "relax_generated_keys": []string{"phase"}}
	meta := common.MetaData{"project": {"P": {"p1"}}, "phase": {"sp": {"p1"}}}
	got := ApplyMetaDataFilterWithDiagnostics(context.Background(), filter, meta, "question", metaFilterChatModel(`{"conditions":[{"key":"project","op":"=","value":"P"},{"key":"phase","op":"=","value":"rp"}],"logic":"and"}`), nil, nil, nil)
	if !reflect.DeepEqual(got, []string{"p1"}) {
		t.Fatalf("scope=%v", got)
	}
}

func TestManualMetadataFilterNeverRelaxes(t *testing.T) {
	filter := map[string]interface{}{
		"method": "manual", "relax_generated_keys": []interface{}{"phase"},
		"manual": []interface{}{map[string]interface{}{"key": "project", "op": "=", "value": "P"}, map[string]interface{}{"key": "phase", "op": "=", "value": "rp"}},
	}
	var diag common.MetadataFilterDiagnostic
	got := ApplyMetaDataFilterWithDiagnostics(context.Background(), filter, common.MetaData{"project": {"P": {"p1"}}, "phase": {"sp": {"p1"}}}, "question", nil, nil, nil, &diag)
	if !reflect.DeepEqual(got, []string{NoMatchDocIDSentinel}) || diag.RelaxationAttempted {
		t.Fatalf("manual scope=%v audit=%+v", got, diag)
	}
}
