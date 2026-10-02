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

package service

import (
	"context"
	"strings"

	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/engine"
	"ragflow/internal/entity"

	"go.uber.org/zap"

	modelModule "ragflow/internal/entity/models"
)

// MetaScope is what a metadata configuration resolved to for one query.
//
// DocIDs and NoMatch keep the contract of ApplyMetaDataFilter (a doc id list,
// [NoMatchDocIDSentinel] for "manual matched nothing", nil + NoMatch for "the
// LLM filter matched nothing, do not filter"). ChunkMeta carries what the
// engine applies directly on the chunk metadata fields instead: the filter
// conditions that no longer need a doc id list, and the boosts.
type MetaScope struct {
	DocIDs    []string
	NoMatch   bool
	ChunkMeta *common.ChunkMetaScope
}

// MetaFilterNeedsLLM reports whether meta_data_filter calls the LLM, for the
// filter, the boost or both.
func MetaFilterNeedsLLM(metaDataFilter map[string]interface{}) bool {
	if metaDataFilter == nil {
		return false
	}
	if method, _ := metaDataFilter["method"].(string); method == "auto" || method == "semi_auto" {
		return true
	}
	if boost, ok := metaDataFilter["boost"].(map[string]interface{}); ok {
		method, _ := boost["method"].(string)
		return method == "auto" || method == "semi_auto"
	}
	return false
}

// ChunkMetadataConfigForKBs is the chunk metadata configuration a query over
// kbs may use on the configured engine, or nil (doc-id path).
func ChunkMetadataConfigForKBs(kbs []*entity.Knowledgebase) *common.ChunkMetadataConfig {
	if _, ok := engine.AsChunkMetadataStore(engine.Get()); !ok || len(kbs) == 0 {
		return nil
	}
	configs := make([]map[string]any, 0, len(kbs))
	for _, kb := range kbs {
		if kb == nil {
			return nil
		}
		configs = append(configs, map[string]any(kb.ParserConfig))
	}
	return common.ChunkMetadataConfigForDatasets(configs)
}

// ChunkMetadataConfigForKBIDs loads the datasets and resolves their shared
// chunk metadata configuration. Any lookup failure means the doc-id path.
func ChunkMetadataConfigForKBIDs(ctx context.Context, kbIDs []string) *common.ChunkMetadataConfig {
	if _, ok := engine.AsChunkMetadataStore(engine.Get()); !ok || len(kbIDs) == 0 || dao.DB == nil {
		return nil
	}
	kbs, err := dao.NewKnowledgebaseDAO().GetByIDs(ctx, dao.DB, kbIDs)
	if err != nil || len(kbs) != len(common.Deduplicate(kbIDs)) {
		return nil
	}
	return ChunkMetadataConfigForKBs(kbs)
}

// ApplyMetaDataScope resolves meta_data_filter like ApplyMetaDataFilter and,
// when chunkMeta is active for the queried datasets, also on the chunk
// metadata fields:
//
//   - filter conditions whose keys are whitelisted and whose operators are
//     chunk-safe become ChunkMeta.Filter instead of a doc id list (exact, and
//     not capped by the engine's result window); the rest are resolved to
//     document ids as before;
//   - meta_data_filter.boost yields ChunkMeta.Boosts. The boost is configured
//     like the filter and independently of it: manual (fixed preferences,
//     applied in every mode but "off"), semi_auto (the LLM fills in values for
//     the listed keys; op and weight may be pinned per key; a key the filter
//     also lists belongs to the filter), auto (the LLM tags each condition
//     hard or soft, soft ones become boosts of auto_weight).
//
// Filter and boost share one LLM call; meta_data_filter.instructions is passed
// to it as guidance. With chunkMeta nil or inactive this is exactly
// ApplyMetaDataFilter and the boost is ignored.
func ApplyMetaDataScope(
	ctx context.Context,
	metaDataFilter map[string]interface{},
	metaData common.MetaData,
	question string,
	chatModel *modelModule.ChatModel,
	baseDocIDs []string,
	kbIDs []string,
	chunkMeta *common.ChunkMetadataConfig,
	manualValueResolver ...ManualValueResolver,
) *MetaScope {
	if !chunkMeta.Active() {
		if _, hasBoost := metaDataFilter["boost"].(map[string]interface{}); hasBoost {
			common.Debug("Metadata boost ignored: chunk metadata is not active on every queried dataset")
		}
		docIDs, noMatch := ApplyMetaDataFilter(ctx, metaDataFilter, metaData, question, chatModel, baseDocIDs, kbIDs, manualValueResolver...)
		return &MetaScope{DocIDs: docIDs, NoMatch: noMatch}
	}

	scope := &MetaScope{DocIDs: baseDocIDs}
	if metaDataFilter == nil {
		return scope
	}
	keys := chunkMeta.Fields
	filterMethod, _ := metaDataFilter["method"].(string)
	boostCfg := common.ParseMetaBoost(metaDataFilter["boost"], keys)
	chunkScope := &common.ChunkMetaScope{BoostMaxTotal: boostCfg.MaxTotal}
	// "off" switches the fixed preferences off too: the form keeps hidden
	// manual rows on save.
	if boostCfg.Method != "off" {
		chunkScope.Boosts = append(chunkScope.Boosts, boostCfg.Manual...)
	}
	defer func() {
		if !chunkScope.IsEmpty() {
			scope.ChunkMeta = chunkScope
		}
	}()

	// applyHard puts conditions on the chunk fields when possible, on doc ids
	// otherwise. It returns false when the conditions matched no document.
	applyHard := func(conditions []MetaFilterCondition, logic string) bool {
		maps := metaConditionMaps(conditions)
		if common.IsChunkFilterable(maps, keys) {
			chunkScope.Filter = common.NewChunkMetaFilter(maps, logic)
			common.Debug("Metadata filter applied on chunk fields", zap.Any("filter", chunkScope.Filter))
			return true
		}
		scope.DocIDs = constrainDocIDs(baseDocIDs, runDocMetaFilter(ctx, metaData, kbIDs, conditions, logic))
		return len(scope.DocIDs) > 0
	}

	if filterMethod == "manual" {
		manual, _ := metaDataFilter["manual"].([]interface{})
		if len(manualValueResolver) > 0 && manualValueResolver[0] != nil {
			resolved := make([]interface{}, 0, len(manual))
			for _, item := range manual {
				if cond, ok := item.(map[string]interface{}); ok {
					resolved = append(resolved, manualValueResolver[0](cond))
				}
			}
			manual = resolved
		}
		if conditions := metaConditionsFromMaps(manual); len(conditions) > 0 {
			logic, _ := metaDataFilter["logic"].(string)
			if logic == "" {
				logic = "and"
			}
			if !applyHard(conditions, logic) {
				scope.DocIDs = []string{NoMatchDocIDSentinel}
			}
		}
	}

	llmFilter := filterMethod == "auto" || filterMethod == "semi_auto"
	if !llmFilter && !boostCfg.UsesLLM() {
		return scope
	}

	var filterKeys []string
	constraints := map[string]string{}
	if filterMethod == "semi_auto" {
		semi, _ := metaDataFilter["semi_auto"].([]interface{})
		for _, item := range semi {
			switch v := item.(type) {
			case string:
				filterKeys = append(filterKeys, v)
			case map[string]interface{}:
				if key, _ := v["key"].(string); key != "" {
					filterKeys = append(filterKeys, key)
					if op, _ := v["op"].(string); op != "" {
						constraints[key] = op
					}
				}
			}
		}
	}
	isFilterKey := make(map[string]bool, len(filterKeys))
	for _, k := range filterKeys {
		isFilterKey[k] = true
	}
	boostKeys := map[string]common.MetaBoostKey{}
	var boostKeyOrder []string
	if boostCfg.Method == "semi_auto" {
		for _, bk := range boostCfg.SemiAuto {
			if isFilterKey[bk.Key] {
				continue
			}
			if _, dup := boostKeys[bk.Key]; !dup {
				boostKeyOrder = append(boostKeyOrder, bk.Key)
			}
			boostKeys[bk.Key] = bk
			if bk.Op != "" {
				constraints[bk.Key] = bk.Op
			}
		}
	}

	offered := metaData
	if filterMethod != "auto" && boostCfg.Method != "auto" {
		offered = common.MetaData{}
		for _, k := range append(append([]string{}, filterKeys...), boostKeyOrder...) {
			if values, ok := metaData[k]; ok {
				offered[k] = values
			}
		}
	}
	if len(offered) == 0 {
		common.Debug("Metadata filter/boost: no offered keys carry values; skipping the LLM call")
		return scope
	}

	var promptConstraints map[string]string
	if (filterMethod == "semi_auto" || boostCfg.Method == "semi_auto") && len(constraints) > 0 {
		promptConstraints = constraints
	}
	generated, err := GenMetaFilter(ctx, chatModel, offered, question, promptConstraints, MetaFilterPromptOptions{
		AllowSoft:    boostCfg.Method == "auto",
		Instructions: MetaFilterInstructions(metaDataFilter),
	})
	if err != nil {
		common.Warn("Failed to generate meta filter", zap.Error(err))
		return scope
	}

	var hard []MetaFilterCondition
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		allowed[k] = true
	}
	addBoost := func(c MetaFilterCondition, weight float64) {
		item := map[string]any{"key": c.Key, "op": c.Op, "value": c.Value, "weight": weight}
		if b, ok := common.MetaBoostFromMap(item, boostCfg.AutoWeight, allowed); ok {
			chunkScope.Boosts = append(chunkScope.Boosts, b)
		}
	}
	for _, c := range generated.Conditions {
		soft := strings.EqualFold(strings.TrimSpace(c.Strength), "soft")
		c.Strength = ""
		switch bk, isBoostKey := boostKeys[c.Key]; {
		case isBoostKey:
			weight := boostCfg.AutoWeight
			if bk.Weight != nil {
				weight = *bk.Weight
			}
			addBoost(c, weight)
		case soft && boostCfg.Method == "auto":
			addBoost(c, boostCfg.AutoWeight)
		case llmFilter && (filterMethod == "auto" || isFilterKey[c.Key]):
			hard = append(hard, c)
		case boostCfg.Method == "auto":
			// The filter did not ask for this key; only the boost offered its values.
			addBoost(c, boostCfg.AutoWeight)
		default:
			common.Debug("Dropping metadata condition: neither filter nor boost asked for its key", zap.String("key", c.Key))
		}
	}

	if llmFilter {
		logic := generated.Logic
		if logic == "" {
			logic = "and"
		}
		matched := false
		if len(hard) > 0 {
			matched = applyHard(hard, logic)
		}
		if !matched && chunkScope.Filter == nil {
			// An LLM filter that matches nothing means "no filter".
			scope.DocIDs = nil
			scope.NoMatch = true
		}
	}
	return scope
}

// runDocMetaFilter resolves conditions to document ids: push-down to the doc
// metadata index when the engine supports it, in-memory otherwise (see
// ApplyMetaDataFilter).
func runDocMetaFilter(ctx context.Context, metaData common.MetaData, kbIDs []string, conditions []MetaFilterCondition, logic string) []string {
	if len(conditions) > 0 && len(kbIDs) > 0 {
		if docEngine := engine.Get(); docEngine != nil {
			if ids := docEngine.FilterDocIdsByMetaPushdown(ctx, dao.DB, kbIDs, metaConditionMaps(conditions), logic); ids != nil {
				return ids
			}
		}
	}
	return ApplyMetaFilter(metaData, conditions, logic)
}

func metaConditionMaps(conditions []MetaFilterCondition) []map[string]interface{} {
	out := make([]map[string]interface{}, len(conditions))
	for i, c := range conditions {
		out[i] = map[string]interface{}{"key": c.Key, "op": c.Op, "value": c.Value}
	}
	return out
}

func metaConditionsFromMaps(items []interface{}) []MetaFilterCondition {
	out := make([]MetaFilterCondition, 0, len(items))
	for _, item := range items {
		cond, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		c := MetaFilterCondition{Value: cond["value"]}
		c.Key, _ = cond["key"].(string)
		c.Op, _ = cond["op"].(string)
		out = append(out, c)
	}
	return out
}

// MetadataConditionToChunkMeta applies a metadata_condition ({"logic",
// "conditions": [{"name", "comparison_operator", "value"}]}) on the chunk
// fields when every condition is chunk-filterable, so it is not resolved to a
// doc id list. Nil means "use the doc-id path".
func MetadataConditionToChunkMeta(metadataCondition map[string]interface{}, chunkMeta *common.ChunkMetadataConfig) *common.ChunkMetaScope {
	if !chunkMeta.Active() || metadataCondition == nil {
		return nil
	}
	input := common.ParseAndConvert(metadataCondition)
	if input == nil {
		return nil
	}
	maps := make([]map[string]interface{}, 0, len(input.Conditions))
	for _, c := range input.Conditions {
		maps = append(maps, map[string]interface{}{"key": c.Key, "op": c.Operator, "value": c.Value})
	}
	if !common.IsChunkFilterable(maps, chunkMeta.Fields) {
		return nil
	}
	return &common.ChunkMetaScope{Filter: common.NewChunkMetaFilter(maps, input.Logic), BoostMaxTotal: common.DefaultMetaBoostMaxTotal}
}

// MetadataBoostToChunkMeta reads the /retrieval metadata_boost parameter: a
// list of {"key", "op", "value", "weight"} or {"manual": [...], "max_total"}.
// It returns nil when chunk metadata is not active (the boost is ignored).
func MetadataBoostToChunkMeta(raw interface{}, chunkMeta *common.ChunkMetadataConfig) *common.ChunkMetaScope {
	if !chunkMeta.Active() || raw == nil {
		return nil
	}
	var cfg common.MetaBoostConfig
	switch v := raw.(type) {
	case []interface{}:
		cfg = common.ParseMetaBoost(map[string]interface{}{"method": "manual", "manual": v}, chunkMeta.Fields)
	case map[string]interface{}:
		m := make(map[string]interface{}, len(v)+1)
		for k, x := range v {
			m[k] = x
		}
		m["method"] = "manual"
		cfg = common.ParseMetaBoost(m, chunkMeta.Fields)
	default:
		return nil
	}
	if len(cfg.Manual) == 0 {
		return nil
	}
	return &common.ChunkMetaScope{Boosts: cfg.Manual, BoostMaxTotal: cfg.MaxTotal}
}
