//
//  Copyright 2026 The InfiniFlow Authors. All Rights Reserved.
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//

// retrieval_filters.go — explicit metadata filters for the Retrieval tool.
//
// A Retrieval tool whose DSL declares filter_args lets the model state the
// metadata conditions itself (tool arg `filters`) instead of having an LLM
// filter guess them per search. The conditions are a hard document scope that
// ANDs with any request scope; matching is the manual metadata filter's.

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
)

const (
	defaultMaxFilterConditions = 6
	maxFilterConditionsLimit   = 32
	maxFilterValues            = 64
	maxFilterValueLength       = 512
	maxFilterKeyLength         = 64
)

// explicitFilterOps are the operators a tool may open to the model. Negations
// are left out on purpose: `not in` / `≠` cannot be pushed down for list-valued
// metadata and fall to the result-window limited in-memory filter.
var explicitFilterOps = []string{"=", "in", "contains"}

var explicitFilterOpAliases = map[string]string{"==": "=", "is": "="}

type filterKeyConfig struct {
	Key         string
	Ops         []string
	Description string
}

// filterArgsConfig is the parsed filter_args node param.
type filterArgsConfig struct {
	Keys          []filterKeyConfig
	MaxConditions int
	// ValuesTool names the companion value-listing tool; empty derives it from
	// the retrieval tool's own name.
	ValuesTool string
}

func canonicalFilterOp(op string) (string, bool) {
	op = strings.ToLower(strings.TrimSpace(op))
	if alias, ok := explicitFilterOpAliases[op]; ok {
		op = alias
	}
	return op, slices.Contains(explicitFilterOps, op)
}

func parseFilterArgs(raw any) (*filterArgsConfig, error) {
	params, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("filter_args must be an object")
	}
	for name := range params {
		switch name {
		case "keys", "max_conditions", "values_tool":
		default:
			return nil, fmt.Errorf("filter_args does not accept %s", name)
		}
	}
	cfg := &filterArgsConfig{MaxConditions: defaultMaxFilterConditions}
	items, _ := params["keys"].([]any)
	seen := map[string]bool{}
	for _, item := range items {
		entry := filterKeyConfig{}
		var ops []any
		switch typed := item.(type) {
		case string:
			entry.Key = typed
		case map[string]any:
			entry.Key, _ = typed["key"].(string)
			entry.Description, _ = typed["description"].(string)
			if rawOps, exists := typed["ops"]; exists {
				if ops, ok = rawOps.([]any); !ok {
					return nil, fmt.Errorf("filter_args key %q: ops must be a list", entry.Key)
				}
			}
		default:
			return nil, fmt.Errorf("filter_args keys must be strings or {key, ops, description} objects")
		}
		entry.Key = strings.TrimSpace(entry.Key)
		if entry.Key == "" || len(entry.Key) > maxFilterKeyLength || seen[entry.Key] {
			return nil, fmt.Errorf("filter_args keys need a unique, non-empty key of at most %d characters (got %q)", maxFilterKeyLength, entry.Key)
		}
		seen[entry.Key] = true
		for _, rawOp := range ops {
			text, _ := rawOp.(string)
			op, allowed := canonicalFilterOp(text)
			if !allowed {
				return nil, fmt.Errorf("filter_args key %q: op %q is not supported (use %s)", entry.Key, text, strings.Join(explicitFilterOps, ", "))
			}
			if !slices.Contains(entry.Ops, op) {
				entry.Ops = append(entry.Ops, op)
			}
		}
		if len(entry.Ops) == 0 {
			entry.Ops = slices.Clone(explicitFilterOps)
		}
		entry.Description = strings.TrimSpace(entry.Description)
		cfg.Keys = append(cfg.Keys, entry)
	}
	if len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("filter_args requires a non-empty keys list")
	}
	if rawMax, exists := params["max_conditions"]; exists {
		value, ok := strictInt(rawMax)
		if !ok || value <= 0 || value > maxFilterConditionsLimit {
			return nil, fmt.Errorf("filter_args max_conditions must be an integer in 1..%d", maxFilterConditionsLimit)
		}
		cfg.MaxConditions = value
	}
	if rawName, exists := params["values_tool"]; exists {
		name, ok := rawName.(string)
		if !ok {
			return nil, fmt.Errorf("filter_args values_tool must be a string")
		}
		cfg.ValuesTool = strings.TrimSpace(name)
	}
	return cfg, nil
}

func (c *filterArgsConfig) key(name string) (filterKeyConfig, bool) {
	for _, entry := range c.Keys {
		if entry.Key == name {
			return entry, true
		}
	}
	return filterKeyConfig{}, false
}

func (c *filterArgsConfig) keyNames() []string {
	names := make([]string, 0, len(c.Keys))
	for _, entry := range c.Keys {
		names = append(names, entry.Key)
	}
	return names
}

// describeKeys renders the allow-list for the model: `project (=, contains) —
// project name; phase (in)`.
func (c *filterArgsConfig) describeKeys() string {
	parts := make([]string, 0, len(c.Keys))
	for _, entry := range c.Keys {
		part := fmt.Sprintf("%s (%s)", entry.Key, strings.Join(entry.Ops, ", "))
		if entry.Description != "" {
			part += " — " + entry.Description
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

func (c *filterArgsConfig) filtersParam(valuesTool, purpose string) *schema.ParameterInfo {
	var ops []string
	for _, op := range explicitFilterOps {
		if slices.ContainsFunc(c.Keys, func(entry filterKeyConfig) bool { return slices.Contains(entry.Ops, op) }) {
			ops = append(ops, op)
		}
	}
	return &schema.ParameterInfo{
		Type: schema.Array,
		Desc: fmt.Sprintf("%s A document must match all of them. Filterable keys and their operators: %s. At most %d conditions. "+
			"Values must exist in the data: look them up with %s instead of guessing.",
			purpose, c.describeKeys(), c.MaxConditions, valuesTool),
		ElemInfo: &schema.ParameterInfo{
			Type: schema.Object,
			SubParams: map[string]*schema.ParameterInfo{
				"key": {Type: schema.String, Enum: c.keyNames(), Required: true},
				"op":  {Type: schema.String, Enum: ops, Required: true},
				"value": {
					Type:     schema.Array,
					ElemInfo: &schema.ParameterInfo{Type: schema.String},
					Desc:     "One value for = and contains; one or more for in (matches any of them).",
					Required: true,
				},
			},
		},
	}
}

func (c *filterArgsConfig) boostParam(valuesTool string) *schema.ParameterInfo {
	return &schema.ParameterInfo{
		Type: schema.Array,
		Desc: fmt.Sprintf("Soft preferences: rank documents matching a value higher, without dropping the others. "+
			"Use filters for what the user fixed and boost for what should only be preferred, e.g. metadata that only part of the documents carry. "+
			"Keys: %s. At most %d entries. Boosts apply only to keys the dataset copies onto its chunks (chunk metadata); "+
			"the response's diagnostics say when a boost could not apply. Values must exist in the data: look them up with %s; "+
			"a value nothing carries boosts nothing. The boosts of one chunk add up to at most %g by default.",
			strings.Join(c.keyNames(), ", "), c.MaxConditions, valuesTool, common.DefaultMetaBoostMaxTotal),
		ElemInfo: &schema.ParameterInfo{
			Type: schema.Object,
			SubParams: map[string]*schema.ParameterInfo{
				"key": {Type: schema.String, Enum: c.keyNames(), Required: true},
				"value": {
					Type:     schema.Array,
					ElemInfo: &schema.ParameterInfo{Type: schema.String},
					Desc:     "One or more values; a document matching any of them is preferred.",
					Required: true,
				},
				"weight": {
					Type: schema.Number,
					Desc: fmt.Sprintf("Strength from 0 to 1; default %g.", common.DefaultMetaBoostWeight),
				},
			},
		},
	}
}

// parseModelFilters validates the model's `filters` argument against the
// allow-list and returns manual-filter conditions. Any problem rejects the
// whole argument: searching wider than the model asked for would hand it
// evidence it believes is filtered.
func (c *filterArgsConfig) parseModelFilters(raw json.RawMessage) ([]map[string]any, []string) {
	items, err := decodeModelFilterList(raw)
	if err != nil {
		return nil, []string{err.Error()}
	}
	if len(items) > c.MaxConditions {
		return nil, []string{fmt.Sprintf("filters: %d conditions given, at most %d are allowed", len(items), c.MaxConditions)}
	}
	conditions := make([]map[string]any, 0, len(items))
	var problems []string
	for index, item := range items {
		condition, problem := c.parseModelFilter(item)
		if problem != "" {
			problems = append(problems, fmt.Sprintf("filters[%d]: %s", index, problem))
			continue
		}
		conditions = append(conditions, condition)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return conditions, nil
}

// decodeModelFilterList accepts the declared array, a single condition object,
// and either of them double-encoded as a JSON string (a common model slip).
func decodeModelFilterList(raw json.RawMessage) ([]map[string]any, error) {
	return decodeModelArgList(raw, "filters", "{key, op, value}")
}

func decodeModelArgList(raw json.RawMessage, arg, shape string) ([]map[string]any, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, fmt.Errorf("%s: not valid JSON", arg)
	}
	if encoded, ok := decoded.(string); ok {
		if strings.TrimSpace(encoded) == "" {
			return nil, nil
		}
		if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
			return nil, fmt.Errorf("%s: must be a list of %s objects", arg, shape)
		}
	}
	if single, ok := decoded.(map[string]any); ok {
		decoded = []any{single}
	}
	list, ok := decoded.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: must be a list of %s objects", arg, shape)
	}
	items := make([]map[string]any, 0, len(list))
	for index, entry := range list {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: must be a %s object", arg, index, shape)
		}
		items = append(items, item)
	}
	return items, nil
}

func (c *filterArgsConfig) parseModelFilter(item map[string]any) (map[string]any, string) {
	name, _ := item["key"].(string)
	name = strings.TrimSpace(name)
	entry, known := c.key(name)
	if !known {
		return nil, fmt.Sprintf("key %q is not filterable; filterable keys: %s", name, strings.Join(c.keyNames(), ", "))
	}
	rawOp, _ := item["op"].(string)
	op, _ := canonicalFilterOp(rawOp)
	if strings.TrimSpace(rawOp) == "" && len(entry.Ops) == 1 {
		op = entry.Ops[0]
	}
	if !slices.Contains(entry.Ops, op) {
		return nil, fmt.Sprintf("op %q is not allowed for key %q; allowed: %s", rawOp, name, strings.Join(entry.Ops, ", "))
	}
	rawValue, exists := item["value"]
	if !exists {
		rawValue = item["values"]
	}
	values, problem := modelFilterValues(rawValue)
	if problem != "" {
		return nil, problem
	}
	if op == "in" {
		list := make([]any, len(values))
		for i, value := range values {
			list[i] = value
		}
		return map[string]any{"key": name, "op": op, "value": list}, ""
	}
	if len(values) != 1 {
		hint := ""
		if slices.Contains(entry.Ops, "in") {
			hint = "; use op in to match any of several values"
		}
		return nil, fmt.Sprintf("op %q takes exactly one value, got %d%s", op, len(values), hint)
	}
	return map[string]any{"key": name, "op": op, "value": values[0]}, ""
}

func modelFilterValues(raw any) ([]string, string) {
	list, isList := raw.([]any)
	if !isList {
		list = []any{raw}
	}
	if len(list) > maxFilterValues {
		return nil, fmt.Sprintf("at most %d values per condition", maxFilterValues)
	}
	values := make([]string, 0, len(list))
	for _, item := range list {
		var value string
		switch typed := item.(type) {
		case string:
			value = strings.TrimSpace(typed)
		case float64:
			value = strconv.FormatFloat(typed, 'f', -1, 64)
		default:
			return nil, "value must be a string or a list of strings"
		}
		if value == "" || len(value) > maxFilterValueLength || strings.ContainsRune(value, 0) {
			return nil, fmt.Sprintf("values must be non-empty strings of at most %d characters", maxFilterValueLength)
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, "value is required"
	}
	return values, ""
}

// parseModelBoosts validates the model's `boost` argument like
// parseModelFilters and returns manual boost rows; any problem rejects the
// call so the model never reads an unboosted ranking as a boosted one.
func (c *filterArgsConfig) parseModelBoosts(raw json.RawMessage) ([]map[string]any, []string) {
	items, err := decodeModelArgList(raw, "boost", "{key, value, weight}")
	if err != nil {
		return nil, []string{err.Error()}
	}
	if len(items) > c.MaxConditions {
		return nil, []string{fmt.Sprintf("boost: %d entries given, at most %d are allowed", len(items), c.MaxConditions)}
	}
	rows := make([]map[string]any, 0, len(items))
	var problems []string
	for index, item := range items {
		row, problem := c.parseModelBoost(item)
		if problem != "" {
			problems = append(problems, fmt.Sprintf("boost[%d]: %s", index, problem))
			continue
		}
		rows = append(rows, row)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return rows, nil
}

func (c *filterArgsConfig) parseModelBoost(item map[string]any) (map[string]any, string) {
	name, _ := item["key"].(string)
	name = strings.TrimSpace(name)
	if _, known := c.key(name); !known {
		return nil, fmt.Sprintf("key %q cannot be boosted; boostable keys: %s", name, strings.Join(c.keyNames(), ", "))
	}
	if rawOp, given := item["op"]; given {
		text, _ := rawOp.(string)
		if op, _ := canonicalFilterOp(text); op != "=" && op != "in" {
			return nil, fmt.Sprintf("boost matches exact values only; omit op (got %q)", text)
		}
	}
	rawValue, exists := item["value"]
	if !exists {
		rawValue = item["values"]
	}
	values, problem := modelFilterValues(rawValue)
	if problem != "" {
		return nil, problem
	}
	weight := common.DefaultMetaBoostWeight
	if rawWeight, given := item["weight"]; given && rawWeight != nil {
		number, ok := rawWeight.(float64)
		if !ok || number < 0 || number > 1 {
			return nil, "weight must be a number from 0 to 1"
		}
		weight = number
	}
	list := make([]any, len(values))
	for i, value := range values {
		list[i] = value
	}
	return map[string]any{"key": name, "op": "in", "value": list, "weight": weight}, ""
}

func invalidFiltersMessage(problems []string, cfg *filterArgsConfig) string {
	return fmt.Sprintf("Invalid filters, nothing was searched: %s. Filterable keys and operators: %s. Fix the filters and call the tool again.",
		strings.Join(problems, "; "), cfg.describeKeys())
}

// withoutLLMMetaFilter is the tool's meta_data_filter with every LLM-driven
// part removed: the model states its conditions itself, so the auto/semi_auto
// filter and boost (one GenMetaFilter call per search) must not run. Fixed
// parts — a manual filter, manual boost rows — are deterministic and stay.
// Nil means nothing is left to apply.
func withoutLLMMetaFilter(filter map[string]any) map[string]any {
	if filter == nil {
		return nil
	}
	out := cloneStringAnyMap(filter)
	method, _ := out["method"].(string)
	if method == "auto" || method == "semi_auto" {
		method = "disabled"
		out["method"] = method
	}
	manual, _ := out["manual"].([]any)
	keep := method == "manual" && len(manual) > 0
	if boost, ok := out["boost"].(map[string]any); ok {
		boostMethod, hasMethod := boost["method"].(string)
		rows, _ := boost["manual"].([]any)
		switch {
		case len(rows) == 0 || (hasMethod && boostMethod != "manual" && boostMethod != "auto" && boostMethod != "semi_auto"):
			delete(out, "boost")
		case boostMethod == "auto" || boostMethod == "semi_auto":
			fixed := cloneStringAnyMap(boost)
			fixed["method"] = "manual"
			out["boost"] = fixed
			keep = true
		default:
			keep = true
		}
	}
	if !keep {
		return nil
	}
	return out
}

func invalidBoostMessage(problems []string, cfg *filterArgsConfig) string {
	return fmt.Sprintf("Invalid boost, nothing was searched: %s. Boostable keys: %s. Fix the boost and call the tool again.",
		strings.Join(problems, "; "), strings.Join(cfg.keyNames(), ", "))
}

// withModelBoosts appends the model's boost rows to the filter's manual boost.
// Fresh maps and slices: filter may share them with the tool's DSL defaults.
func withModelBoosts(filter map[string]any, rows []map[string]any) map[string]any {
	if len(rows) == 0 {
		return filter
	}
	out := cloneStringAnyMap(filter)
	if out == nil {
		out = map[string]any{"method": "disabled"}
	}
	boost := map[string]any{}
	if existing, ok := out["boost"].(map[string]any); ok {
		boost = cloneStringAnyMap(existing)
	}
	fixed, _ := boost["manual"].([]any)
	manual := append([]any(nil), fixed...)
	for _, row := range rows {
		manual = append(manual, row)
	}
	boost["method"] = "manual"
	boost["manual"] = manual
	out["boost"] = boost
	return out
}

// retrievalDiagnostics is the per-call report an explicit-filter tool returns
// to the model next to the chunks.
type retrievalDiagnostics struct {
	Filters      []map[string]any `json:"filters"`
	RequestScope []any            `json:"request_scope,omitempty"`
	DocsInScope  *int             `json:"docs_in_scope,omitempty"`
	// Boosts are the rows that reached the scorer; BoostInactive and
	// BoostIgnoredKeys say why a requested boost did not.
	Boosts           []map[string]any `json:"boosts,omitempty"`
	BoostInactive    string           `json:"boost_inactive,omitempty"`
	BoostIgnoredKeys []string         `json:"boost_ignored_keys,omitempty"`
	Chunks           int              `json:"chunks"`
	Ms               retrievalMs      `json:"ms"`
	Hint             string           `json:"hint,omitempty"`
}

type retrievalMs struct {
	Total     int64 `json:"total"`
	Scope     int64 `json:"scope"`
	Embedding int64 `json:"embedding"`
	Search    int64 `json:"search"`
	Rerank    int64 `json:"rerank"`
}

func (r *RetrievalTool) explicitDiagnostics(ctx context.Context, explicit *runtime.ExplicitFilters, datasetIDs, droppedKeys []string, chunks int, total time.Duration) *retrievalDiagnostics {
	trace := explicit.Trace
	out := &retrievalDiagnostics{
		Filters:          explicit.Conditions,
		RequestScope:     activeRequestScope(ctx, datasetIDs, droppedKeys),
		Boosts:           trace.Boost.Applied,
		BoostInactive:    trace.Boost.Inactive,
		BoostIgnoredKeys: trace.Boost.IgnoredKeys,
		Chunks:           chunks,
		Ms: retrievalMs{
			Total:     total.Milliseconds(),
			Scope:     trace.Scope.Milliseconds(),
			Embedding: trace.Embed.Milliseconds(),
			Search:    trace.Search.Milliseconds(),
			Rerank:    trace.Rerank.Milliseconds(),
		},
	}
	if out.Filters == nil {
		out.Filters = []map[string]any{}
	}
	docs := -1
	if trace.ScopeResolved {
		docs = trace.ScopeDocs
		out.DocsInScope = &docs
		if docs == 0 {
			bounds := "these filters"
			if out.RequestScope != nil {
				bounds = "these filters within the request scope"
			}
			out.Hint = fmt.Sprintf("No document matches %s. Check the values with %s or drop a condition.", bounds, r.valuesToolName())
		}
	}
	filters, _ := json.Marshal(out.Filters)
	boosts := []byte("[]")
	if len(out.Boosts) > 0 {
		boosts, _ = json.Marshal(out.Boosts)
	}
	skipped := ""
	if out.BoostInactive != "" {
		skipped = " boost_inactive=true"
	}
	if len(out.BoostIgnoredKeys) > 0 {
		ignored, _ := json.Marshal(out.BoostIgnoredKeys)
		skipped += " boost_ignored_keys=" + string(ignored)
	}
	sessionID, tenantID := "", ""
	if state, err := runtime.GetStateFromContext(ctx); err == nil && state != nil {
		sessionID, _ = state.Sys["session_id"].(string)
		tenantID, _ = state.Sys["tenant_id"].(string)
	}
	// Info, like agent_run_timings, so the two lines appear together.
	common.Info(fmt.Sprintf(
		"agent_retrieval_timings tool=%s total_ms=%d scope_ms=%d embedding_ms=%d search_ms=%d rerank_ms=%d "+
			"attempts=%d docs_in_scope=%d chunks=%d filters=%s boosts=%s%s session=%s tenant=%s",
		r.name, out.Ms.Total, out.Ms.Scope, out.Ms.Embedding, out.Ms.Search, out.Ms.Rerank,
		trace.Attempts, docs, chunks, filters, boosts, skipped, sessionID, tenantID))
	return out
}

// activeRequestScope lists the request-scope conditions that bound this call
// (the UI filter the model cannot widen), minus the keys dropped for it.
func activeRequestScope(ctx context.Context, datasetIDs, droppedKeys []string) []any {
	scope := runtime.RequestScopeFromContext(ctx)
	if applies, err := scope.AppliesTo(datasetIDs); err != nil || !applies {
		return nil
	}
	filter, _ := scope.FilterDropping(droppedKeys)
	conditions, _ := filter["manual"].([]any)
	return conditions
}
