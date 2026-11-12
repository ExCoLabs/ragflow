package service

import (
	"reflect"
	"testing"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
)

func TestRequestScopeUsesMetadataFilterValueTypes(t *testing.T) {
	metadata := common.MetaData{
		"project_id": {"2018_055": {"allowed", "other-phase"}, "2020_013": {"other-project"}},
		"phase":      {"sp": {"allowed", "other-project"}, "rp": {"other-phase"}},
	}
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"matching_scope", `{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"project_id","op":"in","value":["2018_055"]},{"key":"phase","op":"in","value":["sp"]}]}`, []string{"allowed"}},
		{"missing_metadata_fails_closed", `{"method":"manual","dataset_ids":["kb"],"manual":[{"key":"sector","op":"in","value":["g"]}]}`, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope, err := runtime.ParseRequestScope([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			filter := scope.Filter()
			conditions := make([]MetaFilterCondition, 0)
			for _, raw := range filter["manual"].([]any) {
				condition := raw.(map[string]any)
				conditions = append(conditions, MetaFilterCondition{Key: condition["key"].(string), Op: condition["op"].(string), Value: condition["value"]})
			}
			if got := ApplyMetaFilter(metadata, conditions, filter["logic"].(string)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("scope filter returned %v, want %v", got, tc.want)
			}
		})
	}
}
