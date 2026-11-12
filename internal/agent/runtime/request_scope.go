package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

type scopeCondition struct {
	Key   string   `json:"key"`
	Op    string   `json:"op"`
	Value []string `json:"value"`
}

type RequestScope struct {
	datasets   []string
	conditions []scopeCondition
}

type requestScopeKey struct{}

var scopeMetadataKey = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var scopeDatasetID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func ParseRequestScope(raw json.RawMessage) (*RequestScope, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > 16384 {
		return nil, fmt.Errorf("meta_data_filter exceeds 16 KiB")
	}
	var input struct {
		Method     string            `json:"method"`
		Logic      string            `json:"logic"`
		DatasetIDs []string          `json:"dataset_ids"`
		Manual     *[]scopeCondition `json:"manual"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("invalid meta_data_filter: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("meta_data_filter must be one object")
	}
	if input.Method != "manual" || input.Manual == nil {
		return nil, fmt.Errorf("meta_data_filter requires method=manual and a manual list")
	}
	if input.Logic != "" && input.Logic != "and" {
		return nil, fmt.Errorf("meta_data_filter.logic must be and")
	}
	if len(input.DatasetIDs) > 32 || len(*input.Manual) > 32 {
		return nil, fmt.Errorf("meta_data_filter allows at most 32 datasets and conditions")
	}
	scope := &RequestScope{}
	seen := make(map[string]bool)
	for _, id := range input.DatasetIDs {
		if !scopeDatasetID.MatchString(id) {
			return nil, fmt.Errorf("invalid meta_data_filter dataset id")
		}
		if !seen[id] {
			scope.datasets = append(scope.datasets, id)
			seen[id] = true
		}
	}
	for _, condition := range *input.Manual {
		if !scopeMetadataKey.MatchString(condition.Key) || condition.Op != "in" || len(condition.Value) == 0 || len(condition.Value) > 256 {
			return nil, fmt.Errorf("meta_data_filter conditions require a metadata key, op=in and 1..256 string values")
		}
		for _, value := range condition.Value {
			if strings.TrimSpace(value) == "" || len(value) > 512 || strings.ContainsRune(value, 0) {
				return nil, fmt.Errorf("invalid meta_data_filter condition value")
			}
		}
		scope.conditions = append(scope.conditions, condition)
	}
	if scope.HasFilters() && len(scope.datasets) == 0 {
		return nil, fmt.Errorf("agent meta_data_filter requires dataset_ids to distinguish project and methodology tools")
	}
	return scope, nil
}

func WithRequestScope(ctx context.Context, scope *RequestScope) context.Context {
	return context.WithValue(ctx, requestScopeKey{}, scope)
}

func RequestScopeFromContext(ctx context.Context) *RequestScope {
	scope, _ := ctx.Value(requestScopeKey{}).(*RequestScope)
	return scope
}

func (s *RequestScope) HasFilters() bool {
	return s != nil && len(s.conditions) > 0
}

func (s *RequestScope) DatasetIDs() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.datasets...)
}

func (s *RequestScope) Filter() map[string]any {
	if s == nil {
		return nil
	}
	conditions := make([]any, 0, len(s.conditions))
	for _, condition := range s.conditions {
		values := make([]any, len(condition.Value))
		for i, value := range condition.Value {
			values[i] = value
		}
		conditions = append(conditions, map[string]any{"key": condition.Key, "op": condition.Op, "value": values})
	}
	return map[string]any{"method": "manual", "logic": "and", "manual": conditions}
}

func (s *RequestScope) Fingerprint() string {
	if !s.HasFilters() {
		return ""
	}
	datasets := s.DatasetIDs()
	slices.Sort(datasets)
	conditions := make([]string, 0, len(s.conditions))
	for _, condition := range s.conditions {
		values := slices.Clone(condition.Value)
		slices.Sort(values)
		condition.Value = slices.Compact(values)
		raw, _ := json.Marshal(condition)
		conditions = append(conditions, string(raw))
	}
	slices.Sort(conditions)
	raw, _ := json.Marshal(map[string]any{"version": 1, "datasets": datasets, "conditions": slices.Compact(conditions)})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (s *RequestScope) AppliesTo(datasetIDs []string) (bool, error) {
	if !s.HasFilters() {
		return false, nil
	}
	selected := make(map[string]bool, len(s.datasets))
	for _, id := range s.datasets {
		selected[id] = true
	}
	inside, outside := false, false
	for _, id := range datasetIDs {
		if selected[id] {
			inside = true
		} else {
			outside = true
		}
	}
	if inside && outside {
		return false, fmt.Errorf("filtered retrieval cannot mix scoped and unscoped datasets")
	}
	return inside, nil
}
