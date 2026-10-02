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

package common

// Document metadata on chunks: fields, filter conditions and score boosts.
//
// A metadata filter is normally resolved to a list of document ids and pushed
// into the chunk query as a doc_id terms clause. That list is capped by the
// document engine's result window (10 000 on Elasticsearch) and grows with the
// match set, so a filter over a large dataset is either silently truncated or
// slow. When a dataset opts in (parser_config.chunk_metadata), the whitelisted
// metadata keys are copied onto every chunk as meta_<key>_kwd (plus a typed
// twin for numbers and dates), and a condition becomes one clause on the chunk
// query: exact and independent of how many documents match.
//
// The same fields carry a boost: weighted conditions that raise the fused
// score of matching chunks instead of excluding the others ("prefer the newest
// report", "prefer the department named in the question"). Boosts are applied
// twice: in the engine (should clauses, so a chunk that wins only on metadata
// still reaches the candidate pool) and in the fused similarity computed by
// the retrieval service (so the preference survives reranking).
//
// The field names ride on the dynamic templates of conf/mapping.json (*_kwd
// keyword, *_int integer, *_flt float, *_dt date), so no mapping change is
// needed.

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// ChunkMetadataConfigKey is the dataset parser_config key holding the
	// configuration ({"enabled", "fields", "ready"}).
	ChunkMetadataConfigKey = "chunk_metadata"
	// ChunkMetadataMaxFields caps the whitelist.
	ChunkMetadataMaxFields = 32
	// DefaultMetaBoostWeight is the weight of a boost the LLM derived.
	DefaultMetaBoostWeight = 0.15
	// DefaultMetaBoostMaxTotal caps the sum of boosts per chunk.
	DefaultMetaBoostMaxTotal = 0.3

	chunkMetaFieldPrefix = "meta_"
)

var (
	chunkMetaKeyRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	chunkMetaDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	chunkMetaIntRE  = regexp.MustCompile(`^[+-]?\d+$`)
	chunkMetaFltRE  = regexp.MustCompile(`^[+-]?\d+\.\d+$`)

	// Keys that would collide with RAGFlow's own chunk fields after
	// prefixing, or that are not usable as field names.
	chunkMetaReservedKeys = map[string]bool{
		"kb_id": true, "doc_id": true, "id": true, "content": true, "vec": true,
		"tks": true, "ltks": true, "kwd": true, "int": true, "flt": true, "dt": true,
	}

	// chunkMetaFilterOps are the operators a chunk-level filter can express.
	// "≠" and "not in" are left out for the same reason the doc-metadata
	// push-down refuses them: on a multi-valued field a must_not term excludes
	// a chunk whose other value would satisfy the in-memory semantics.
	chunkMetaFilterOps = map[string]bool{
		"=": true, ">": true, "<": true, "≥": true, "≤": true, "in": true,
		"contains": true, "not contains": true, "start with": true, "end with": true,
		"empty": true, "not empty": true,
	}

	// MetaBoostOps are the operators a boost condition may use. max/min have
	// no value: they prefer the highest/lowest value among the candidates.
	MetaBoostOps = map[string]bool{
		"=": true, "in": true, ">": true, "<": true, "≥": true, "≤": true,
		"contains": true, "max": true, "min": true,
	}
)

// ChunkMetadataConfig is parser_config.chunk_metadata.
type ChunkMetadataConfig struct {
	Enabled bool     `json:"enabled"`
	Fields  []string `json:"fields"`
	// Ready is set by the backfill once every existing chunk of the dataset
	// carries the fields. Until then queries keep using the doc-id path, so a
	// half-filtered result is never served.
	Ready bool `json:"ready"`
}

// Active reports whether queries may rely on the chunk fields.
func (c *ChunkMetadataConfig) Active() bool {
	return c != nil && c.Enabled && c.Ready && len(c.Fields) > 0
}

// ToMap renders the config as stored in parser_config.
func (c *ChunkMetadataConfig) ToMap() map[string]any {
	fields := make([]any, 0, len(c.Fields))
	for _, f := range c.Fields {
		fields = append(fields, f)
	}
	return map[string]any{"enabled": c.Enabled, "fields": fields, "ready": c.Ready}
}

// ValidateChunkMetadataFields whitelists keys as field-name safe, trimmed,
// de-duplicated and capped.
func ValidateChunkMetadataFields(keys []any) ([]string, error) {
	out := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, raw := range keys {
		k, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("chunk_metadata.fields entries must be strings, got %v", raw)
		}
		k = strings.TrimSpace(k)
		if !chunkMetaKeyRE.MatchString(k) {
			return nil, fmt.Errorf("chunk_metadata field %q must match %s", k, chunkMetaKeyRE.String())
		}
		if chunkMetaReservedKeys[strings.ToLower(k)] {
			return nil, fmt.Errorf("chunk_metadata field %q collides with a chunk field", k)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	if len(out) > ChunkMetadataMaxFields {
		return nil, fmt.Errorf("chunk_metadata allows at most %d fields, got %d", ChunkMetadataMaxFields, len(out))
	}
	return out, nil
}

// ParseChunkMetadataConfig reads parser_config.chunk_metadata. It returns nil
// when the key is absent or invalid: this runs on every retrieval and a bad
// dataset setting must degrade to the doc-id path, not break search.
func ParseChunkMetadataConfig(parserConfig map[string]any) *ChunkMetadataConfig {
	if parserConfig == nil {
		return nil
	}
	raw, ok := parserConfig[ChunkMetadataConfigKey].(map[string]any)
	if !ok {
		return nil
	}
	var rawFields []any
	switch v := raw["fields"].(type) {
	case []any:
		rawFields = v
	case []string:
		for _, s := range v {
			rawFields = append(rawFields, s)
		}
	}
	fields, err := ValidateChunkMetadataFields(rawFields)
	if err != nil {
		Warn("Ignoring chunk_metadata config: " + err.Error())
		return nil
	}
	enabled, _ := raw["enabled"].(bool)
	ready, _ := raw["ready"].(bool)
	return &ChunkMetadataConfig{Enabled: enabled, Fields: fields, Ready: ready}
}

// ChunkMetadataConfigForDatasets is the configuration a query over several
// datasets can rely on: every dataset must be active, and only the keys they
// all carry are usable (a condition on a key one dataset lacks would silently
// exclude that dataset's chunks). Nil means "use the doc-id path".
func ChunkMetadataConfigForDatasets(parserConfigs []map[string]any) *ChunkMetadataConfig {
	if len(parserConfigs) == 0 {
		return nil
	}
	var shared map[string]bool
	for _, pc := range parserConfigs {
		cfg := ParseChunkMetadataConfig(pc)
		if !cfg.Active() {
			return nil
		}
		next := make(map[string]bool, len(cfg.Fields))
		for _, f := range cfg.Fields {
			if shared == nil || shared[f] {
				next[f] = true
			}
		}
		shared = next
	}
	if len(shared) == 0 {
		return nil
	}
	fields := make([]string, 0, len(shared))
	for f := range shared {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	return &ChunkMetadataConfig{Enabled: true, Fields: fields, Ready: true}
}

// Chunk field names for a metadata key.
func ChunkMetaKwdField(key string) string { return chunkMetaFieldPrefix + key + "_kwd" }
func ChunkMetaIntField(key string) string { return chunkMetaFieldPrefix + key + "_int" }
func ChunkMetaFltField(key string) string { return chunkMetaFieldPrefix + key + "_flt" }
func ChunkMetaDtField(key string) string  { return chunkMetaFieldPrefix + key + "_dt" }

// ChunkMetaFieldNames lists every chunk field the keys can produce.
func ChunkMetaFieldNames(keys []string) []string {
	out := make([]string, 0, 4*len(keys))
	for _, k := range keys {
		out = append(out, ChunkMetaKwdField(k), ChunkMetaIntField(k), ChunkMetaFltField(k), ChunkMetaDtField(k))
	}
	return out
}

// chunkMetaTyped classifies one scalar metadata value: "int", "flt", "dt" or
// "" when it has no typed twin. Strings count as numbers only when they are
// plain decimal literals ("2015_062" stays a string).
func chunkMetaTyped(value any) (string, any) {
	switch v := value.(type) {
	case bool:
		if v {
			return "int", int64(1)
		}
		return "int", int64(0)
	case int:
		return "int", int64(v)
	case int64:
		return "int", v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", nil
		}
		// JSON decodes every number as float64; an integral one was an int.
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return "int", int64(v)
		}
		return "flt", v
	case json.Number:
		return chunkMetaTyped(v.String())
	case string:
		s := strings.TrimSpace(v)
		switch {
		case chunkMetaDateRE.MatchString(s):
			return "dt", s
		case chunkMetaIntRE.MatchString(s):
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				return "int", n
			}
		case chunkMetaFltRE.MatchString(s):
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return "flt", f
			}
		}
	}
	return "", nil
}

// chunkMetaString is the keyword form of a metadata value.
func chunkMetaString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func chunkMetaValues(raw any) []any {
	var values []any
	switch v := raw.(type) {
	case []any:
		values = v
	case []string:
		for _, s := range v {
			values = append(values, s)
		}
	default:
		values = []any{raw}
	}
	out := values[:0:0]
	for _, v := range values {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

func singleOrList[T any](values []T) any {
	if len(values) == 1 {
		return values[0]
	}
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// ChunkMetadataFields is the chunk-level copy of the whitelisted metadata.
// Every present key gets meta_<key>_kwd with the string form of each value (a
// list for multi-valued metadata); integers, floats and YYYY-MM-DD dates also
// get the typed twin so range operators and max/min boosts compare by value.
func ChunkMetadataFields(meta map[string]any, keys []string) map[string]any {
	out := map[string]any{}
	if len(meta) == 0 {
		return out
	}
	for _, key := range keys {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		values := chunkMetaValues(raw)
		if len(values) == 0 {
			continue
		}
		kwds := make([]string, 0, len(values))
		typed := map[string][]any{}
		for _, v := range values {
			kwds = append(kwds, chunkMetaString(v))
			if kind, coerced := chunkMetaTyped(v); kind != "" {
				typed[kind] = append(typed[kind], coerced)
			}
		}
		out[ChunkMetaKwdField(key)] = singleOrList(kwds)
		for kind, vals := range typed {
			var name string
			switch kind {
			case "int":
				name = ChunkMetaIntField(key)
			case "flt":
				name = ChunkMetaFltField(key)
			default:
				name = ChunkMetaDtField(key)
			}
			out[name] = singleOrList(vals)
		}
	}
	return out
}

// ChunkMetadataRemovalFields lists the chunk fields that must be dropped so a
// metadata edit that removes a key or changes its type leaves no stale twin.
func ChunkMetadataRemovalFields(meta map[string]any, keys []string) []string {
	keep := ChunkMetadataFields(meta, keys)
	out := make([]string, 0)
	for _, f := range ChunkMetaFieldNames(keys) {
		if _, ok := keep[f]; !ok {
			out = append(out, f)
		}
	}
	return out
}

// ChunkMetaFilter is a metadata filter applied on the chunk fields. Each
// condition is a {"key", "op", "value"} map with a canonical operator.
type ChunkMetaFilter struct {
	Conditions []map[string]any
	Logic      string
}

// ChunkMetaBoost is one weighted preference on a chunk metadata field.
type ChunkMetaBoost struct {
	Key    string  `json:"key"`
	Op     string  `json:"op"`
	Value  any     `json:"value,omitempty"`
	Weight float64 `json:"weight"`
}

// ChunkMetaScope is the chunk-level part of a resolved metadata
// configuration: a filter on the chunk fields and/or boosts. A nil scope, or
// one with neither, leaves retrieval unchanged.
type ChunkMetaScope struct {
	Filter        *ChunkMetaFilter
	Boosts        []ChunkMetaBoost
	BoostMaxTotal float64
}

// IsEmpty reports whether the scope changes nothing.
func (s *ChunkMetaScope) IsEmpty() bool {
	return s == nil || ((s.Filter == nil || len(s.Filter.Conditions) == 0) && len(s.Boosts) == 0)
}

// MergeChunkMetaScopes combines two scopes: the boosts of both, and the
// filter of a (b's when a has none). The cap of the scope that carries
// boosts applies; with boosts on both sides the larger cap wins.
func MergeChunkMetaScopes(a, b *ChunkMetaScope) *ChunkMetaScope {
	switch {
	case a.IsEmpty() && b.IsEmpty():
		return nil
	case b.IsEmpty():
		return a
	case a.IsEmpty():
		return b
	}
	out := &ChunkMetaScope{Filter: a.Filter, BoostMaxTotal: a.BoostMaxTotal}
	if out.Filter == nil || len(out.Filter.Conditions) == 0 {
		out.Filter = b.Filter
	}
	out.Boosts = append(append([]ChunkMetaBoost{}, a.Boosts...), b.Boosts...)
	switch {
	case len(a.Boosts) == 0:
		out.BoostMaxTotal = b.BoostMaxTotal
	case len(b.Boosts) > 0:
		out.BoostMaxTotal = math.Max(a.BoostMaxTotal, b.BoostMaxTotal)
	}
	return out
}

// ChunkMetaValueList splits an "in" value: a list, a JSON array string or a
// comma-separated string.
func ChunkMetaValueList(value any) []string {
	var items []any
	switch v := value.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case string:
		s := strings.TrimSpace(v)
		if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
			var parsed []any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				items = parsed
				break
			}
		}
		for _, part := range strings.Split(s, ",") {
			items = append(items, part)
		}
	case nil:
	default:
		items = []any{v}
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s := chunkMetaString(it); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func chunkMetaHasValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(v) != ""
	case []any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	}
	return true
}

// IsChunkFilterable reports whether every condition can be applied on the
// chunk fields of keys: known key, chunk-safe operator, a value where the
// operator needs one. Operators are compared after NormalizeOperator.
func IsChunkFilterable(conditions []map[string]any, keys []string) bool {
	if len(conditions) == 0 {
		return false
	}
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		allowed[k] = true
	}
	for _, c := range conditions {
		key, _ := c["key"].(string)
		op, _ := c["op"].(string)
		op = NormalizeOperator(op)
		if !allowed[key] || !chunkMetaFilterOps[op] {
			return false
		}
		if op != "empty" && op != "not empty" && !chunkMetaHasValue(c["value"]) {
			return false
		}
		if op == "in" && len(ChunkMetaValueList(c["value"])) == 0 {
			return false
		}
	}
	return true
}

// NewChunkMetaFilter copies conditions with canonical operators; logic
// defaults to "and".
func NewChunkMetaFilter(conditions []map[string]any, logic string) *ChunkMetaFilter {
	out := make([]map[string]any, 0, len(conditions))
	for _, c := range conditions {
		op, _ := c["op"].(string)
		out = append(out, map[string]any{"key": c["key"], "op": NormalizeOperator(op), "value": c["value"]})
	}
	if logic != "or" {
		logic = "and"
	}
	return &ChunkMetaFilter{Conditions: out, Logic: logic}
}

// MetaBoostKey is a key offered to the LLM in semi_auto boost mode; Op and
// Weight may be pinned per key the way the filter's semi_auto pins op.
type MetaBoostKey struct {
	Key    string
	Op     string
	Weight *float64
}

// MetaBoostConfig is meta_data_filter.boost.
type MetaBoostConfig struct {
	Method     string // off | manual | auto | semi_auto
	Manual     []ChunkMetaBoost
	SemiAuto   []MetaBoostKey
	AutoWeight float64
	MaxTotal   float64
}

// UsesLLM reports whether the boost asks the LLM for conditions.
func (c MetaBoostConfig) UsesLLM() bool {
	return c.Method == "auto" || c.Method == "semi_auto"
}

func clampWeight(raw any, def float64) float64 {
	var v float64
	switch x := raw.(type) {
	case float64:
		v = x
	case int:
		v = float64(x)
	case int64:
		v = float64(x)
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return def
		}
		v = f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return def
		}
		v = f
	default:
		return def
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return def
	}
	return math.Min(1, math.Max(0, v))
}

func keySet(keys []string) map[string]bool {
	if keys == nil {
		return nil
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// ParseMetaBoost reads meta_data_filter.boost. Conditions with unknown
// operators, or on keys outside keys (when keys is non-nil), are dropped.
func ParseMetaBoost(raw any, keys []string) MetaBoostConfig {
	cfg := MetaBoostConfig{Method: "off", AutoWeight: DefaultMetaBoostWeight, MaxTotal: DefaultMetaBoostMaxTotal}
	m, ok := raw.(map[string]any)
	if !ok {
		return cfg
	}
	method, hasMethod := m["method"].(string)
	if !hasMethod {
		if manual, _ := m["manual"].([]any); len(manual) > 0 {
			method = "manual"
		}
	}
	switch method {
	case "manual", "auto", "semi_auto":
		cfg.Method = method
	}
	if v, ok := m["auto_weight"]; ok {
		cfg.AutoWeight = clampWeight(v, DefaultMetaBoostWeight)
	}
	if v, ok := m["max_total"]; ok {
		cfg.MaxTotal = clampWeight(v, DefaultMetaBoostMaxTotal)
	}
	allowed := keySet(keys)
	if manual, ok := m["manual"].([]any); ok {
		for _, item := range manual {
			if b, ok := MetaBoostFromMap(item, cfg.AutoWeight, allowed); ok {
				cfg.Manual = append(cfg.Manual, b)
			}
		}
	}
	if semi, ok := m["semi_auto"].([]any); ok {
		for _, item := range semi {
			bk := MetaBoostKey{}
			switch v := item.(type) {
			case string:
				bk.Key = v
			case map[string]any:
				bk.Key, _ = v["key"].(string)
				if op, _ := v["op"].(string); MetaBoostOps[NormalizeOperator(op)] {
					bk.Op = NormalizeOperator(op)
				}
				if w, ok := v["weight"]; ok && w != nil && w != "" {
					weight := clampWeight(w, cfg.AutoWeight)
					bk.Weight = &weight
				}
			}
			if bk.Key == "" || (allowed != nil && !allowed[bk.Key]) {
				continue
			}
			cfg.SemiAuto = append(cfg.SemiAuto, bk)
		}
	}
	return cfg
}

// MetaBoostFromMap converts one {"key", "op", "value", "weight"} map. max/min
// need no value; every other operator does.
func MetaBoostFromMap(item any, defaultWeight float64, allowed map[string]bool) (ChunkMetaBoost, bool) {
	m, ok := item.(map[string]any)
	if !ok {
		return ChunkMetaBoost{}, false
	}
	key, _ := m["key"].(string)
	op := "="
	if raw, ok := m["op"].(string); ok && raw != "" {
		op = NormalizeOperator(raw)
	}
	if key == "" || !MetaBoostOps[op] {
		return ChunkMetaBoost{}, false
	}
	if allowed != nil && !allowed[key] {
		return ChunkMetaBoost{}, false
	}
	if op != "max" && op != "min" && !chunkMetaHasValue(m["value"]) {
		return ChunkMetaBoost{}, false
	}
	weight := defaultWeight
	if w, ok := m["weight"]; ok {
		weight = clampWeight(w, defaultWeight)
	}
	return ChunkMetaBoost{Key: key, Op: op, Value: m["value"], Weight: weight}, true
}

// MetaBoostFieldNames lists the chunk fields the scorer reads back per hit.
func MetaBoostFieldNames(boosts []ChunkMetaBoost) []string {
	keys := make([]string, 0, len(boosts))
	seen := map[string]bool{}
	for _, b := range boosts {
		if !seen[b.Key] {
			seen[b.Key] = true
			keys = append(keys, b.Key)
		}
	}
	sort.Strings(keys)
	return ChunkMetaFieldNames(keys)
}

func chunkKwdValues(chunk map[string]any, key string) []string {
	switch v := chunk[ChunkMetaKwdField(key)].(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			out = append(out, chunkMetaString(x))
		}
		return out
	case []string:
		return v
	default:
		return []string{chunkMetaString(v)}
	}
}

func firstOf(v any) any {
	if list, ok := v.([]any); ok {
		if len(list) == 0 {
			return nil
		}
		return list[0]
	}
	return v
}

// dateOrdinal maps YYYY-MM-DD... to a comparable number.
func dateOrdinal(s string) (float64, bool) {
	if len(s) < 10 || !chunkMetaDateRE.MatchString(s[:10]) {
		return 0, false
	}
	y, _ := strconv.Atoi(s[:4])
	m, _ := strconv.Atoi(s[5:7])
	d, _ := strconv.Atoi(s[8:10])
	return float64(y*10000 + m*100 + d), true
}

// chunkTypedValue is the numeric view of a chunk's value for ranges and
// max/min; dates become ordinal-like numbers so "newest" works on _dt too.
func chunkTypedValue(chunk map[string]any, key string) (float64, bool) {
	for _, name := range []string{ChunkMetaIntField(key), ChunkMetaFltField(key)} {
		switch v := firstOf(chunk[name]).(type) {
		case float64:
			return v, true
		case int64:
			return float64(v), true
		case int:
			return float64(v), true
		case json.Number:
			if f, err := v.Float64(); err == nil {
				return f, true
			}
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f, true
			}
		}
	}
	if s, ok := firstOf(chunk[ChunkMetaDtField(key)]).(string); ok {
		return dateOrdinal(s)
	}
	return 0, false
}

func metaBoostMatches(b ChunkMetaBoost, chunk map[string]any) bool {
	switch b.Op {
	case "=":
		target := strings.ToLower(chunkMetaString(b.Value))
		for _, v := range chunkKwdValues(chunk, b.Key) {
			if strings.ToLower(v) == target {
				return true
			}
		}
	case "in":
		members := map[string]bool{}
		for _, m := range ChunkMetaValueList(b.Value) {
			members[strings.ToLower(m)] = true
		}
		for _, v := range chunkKwdValues(chunk, b.Key) {
			if members[strings.ToLower(v)] {
				return true
			}
		}
	case "contains":
		needle := strings.ToLower(chunkMetaString(b.Value))
		for _, v := range chunkKwdValues(chunk, b.Key) {
			if strings.Contains(strings.ToLower(v), needle) {
				return true
			}
		}
	case ">", "<", "≥", "≤":
		have, ok := chunkTypedValue(chunk, b.Key)
		if !ok {
			return false
		}
		kind, coerced := chunkMetaTyped(b.Value)
		var want float64
		switch kind {
		case "int":
			want = float64(coerced.(int64))
		case "flt":
			want = coerced.(float64)
		case "dt":
			want, _ = dateOrdinal(coerced.(string))
		default:
			return false
		}
		switch b.Op {
		case ">":
			return have > want
		case "<":
			return have < want
		case "≥":
			return have >= want
		case "≤":
			return have <= want
		}
	}
	return false
}

// MetaBoostScores is the additive score per chunk: the sum of weight·match,
// with max/min normalised over the candidate pool (1 for the extreme value,
// 0 for the other end), capped at maxTotal so metadata never outranks
// relevance on its own.
func MetaBoostScores(boosts []ChunkMetaBoost, chunks []map[string]any, maxTotal float64) []float64 {
	scores := make([]float64, len(chunks))
	if len(boosts) == 0 || len(chunks) == 0 {
		return scores
	}
	for _, b := range boosts {
		if b.Op == "max" || b.Op == "min" {
			vals := make([]float64, len(chunks))
			present := make([]bool, len(chunks))
			lo, hi := math.Inf(1), math.Inf(-1)
			for i, c := range chunks {
				if v, ok := chunkTypedValue(c, b.Key); ok {
					vals[i], present[i] = v, true
					lo, hi = math.Min(lo, v), math.Max(hi, v)
				}
			}
			for i := range chunks {
				if !present[i] {
					continue
				}
				frac := 1.0
				if hi != lo {
					frac = (vals[i] - lo) / (hi - lo)
					if b.Op == "min" {
						frac = 1 - frac
					}
				}
				scores[i] += b.Weight * frac
			}
			continue
		}
		for i, c := range chunks {
			if metaBoostMatches(b, c) {
				scores[i] += b.Weight
			}
		}
	}
	limit := math.Max(0, maxTotal)
	for i := range scores {
		scores[i] = math.Min(limit, scores[i])
	}
	return scores
}
