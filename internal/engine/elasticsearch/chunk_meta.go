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

package elasticsearch

// Document metadata copied onto chunks (common.ChunkMetadataFields): the
// filter and boost clauses of the chunk query, the per-document update of the
// fields, and the scan of the doc-metadata index the backfill reads.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"

	"github.com/elastic/go-elasticsearch/v8/esapi"
	"go.uber.org/zap"

	"ragflow/internal/common"
)

// chunkMetaBoostScale turns a boost weight in [0, 1] into an ES clause boost
// in [1, 6]: enough to pull a chunk that wins only on metadata into the
// candidate pool without letting it outrank a strong text match on its own.
const chunkMetaBoostScale = 5.0

// chunkMetaFieldNameRE guards the field names spliced into the update script.
var chunkMetaFieldNameRE = regexp.MustCompile(`^meta_[A-Za-z][A-Za-z0-9_]*_(kwd|int|flt|dt)$`)

// chunkMetaTerm is a case-insensitive exact match on the keyword field.
// Numbers are stored as strings there too, so equality works for every value
// type and does not depend on which typed twin the indexer chose.
func chunkMetaTerm(key string, value interface{}) map[string]interface{} {
	return map[string]interface{}{
		"term": map[string]interface{}{
			common.ChunkMetaKwdField(key): map[string]interface{}{
				"value":            chunkMetaTermString(value),
				"case_insensitive": true,
			},
		},
	}
}

func chunkMetaTermString(value interface{}) string {
	switch v := value.(type) {
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return fmt.Sprintf("%d", int64(v))
		}
	case string:
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

// chunkMetaRange compares on the typed twin matching the value: dates on
// _dt, numbers on both numeric twins (a key may hold integers on some
// documents and decimals on others), anything else on the keyword field.
func chunkMetaRange(key, op string, value interface{}, boost float64) map[string]interface{} {
	esOp := rangeOps[op]
	coerced := coerceRangeValue(value, nil)
	bound := func(v interface{}) map[string]interface{} {
		b := map[string]interface{}{esOp: v}
		if boost > 0 {
			b["boost"] = boost
		}
		return b
	}
	switch v := coerced.(type) {
	case float64:
		clauses := []interface{}{
			map[string]interface{}{"range": map[string]interface{}{common.ChunkMetaIntField(key): bound(v)}},
			map[string]interface{}{"range": map[string]interface{}{common.ChunkMetaFltField(key): bound(v)}},
		}
		return map[string]interface{}{"bool": map[string]interface{}{"should": clauses, "minimum_should_match": 1}}
	case string:
		if dateRegex.MatchString(v) {
			return map[string]interface{}{"range": map[string]interface{}{common.ChunkMetaDtField(key): bound(v)}}
		}
	}
	return map[string]interface{}{"range": map[string]interface{}{common.ChunkMetaKwdField(key): bound(coerced)}}
}

func chunkMetaWildcard(field, pattern string) map[string]interface{} {
	return map[string]interface{}{
		"wildcard": map[string]interface{}{
			field: map[string]interface{}{"value": pattern, "case_insensitive": true},
		},
	}
}

// chunkMetaConditionClause turns one condition into one bool clause on the
// chunk fields. The service checks common.IsChunkFilterable first, so an
// error here means a caller skipped that check.
func chunkMetaConditionClause(flt map[string]interface{}) (map[string]interface{}, error) {
	key, _ := flt["key"].(string)
	op, _ := flt["op"].(string)
	op = common.NormalizeOperator(op)
	value := flt["value"]
	if key == "" {
		return nil, &UnsupportedMetaFilterError{Reason: "condition is missing a string key", FilterClause: flt}
	}
	kwd := common.ChunkMetaKwdField(key)
	switch op {
	case "empty":
		return map[string]interface{}{"bool": map[string]interface{}{
			"must_not": []interface{}{map[string]interface{}{"exists": map[string]interface{}{"field": kwd}}},
		}}, nil
	case "not empty":
		return map[string]interface{}{"exists": map[string]interface{}{"field": kwd}}, nil
	case "=":
		return chunkMetaTerm(key, value), nil
	case "in":
		members := common.ChunkMetaValueList(value)
		if len(members) == 0 {
			return nil, &UnsupportedMetaFilterError{Reason: "operator \"in\" requires at least one value", FilterClause: flt}
		}
		should := make([]interface{}, 0, len(members))
		for _, m := range members {
			should = append(should, chunkMetaTerm(key, m))
		}
		return map[string]interface{}{"bool": map[string]interface{}{"should": should, "minimum_should_match": 1}}, nil
	case ">", "<", "≥", "≤":
		return chunkMetaRange(key, op, value, 0), nil
	}
	text := coerceString(value, flt)
	if strings.TrimSpace(text) == "" {
		return nil, &UnsupportedMetaFilterError{Reason: fmt.Sprintf("operator %q requires a value", op), FilterClause: flt}
	}
	switch op {
	case "contains":
		return chunkMetaWildcard(kwd, "*"+escapeWildcard(text)+"*"), nil
	case "not contains":
		return map[string]interface{}{"bool": map[string]interface{}{
			"must":     []interface{}{map[string]interface{}{"exists": map[string]interface{}{"field": kwd}}},
			"must_not": []interface{}{chunkMetaWildcard(kwd, "*"+escapeWildcard(text)+"*")},
		}}, nil
	case "start with":
		return map[string]interface{}{"prefix": map[string]interface{}{
			kwd: map[string]interface{}{"value": text, "case_insensitive": true},
		}}, nil
	case "end with":
		return chunkMetaWildcard(kwd, "*"+escapeWildcard(text)), nil
	}
	return nil, &UnsupportedMetaFilterError{Reason: fmt.Sprintf("operator %q is not chunk-filterable", op), FilterClause: flt}
}

// buildChunkMetaFilterClause renders a filter as one clause for the chunk
// query's bool.filter.
func buildChunkMetaFilterClause(f *common.ChunkMetaFilter) (map[string]interface{}, error) {
	if f == nil || len(f.Conditions) == 0 {
		return nil, fmt.Errorf("chunk metadata filter has no conditions")
	}
	clauses := make([]interface{}, 0, len(f.Conditions))
	for _, c := range f.Conditions {
		clause, err := chunkMetaConditionClause(c)
		if err != nil {
			return nil, err
		}
		clauses = append(clauses, clause)
	}
	if f.Logic == "or" {
		return map[string]interface{}{"bool": map[string]interface{}{"should": clauses, "minimum_should_match": 1}}, nil
	}
	return map[string]interface{}{"bool": map[string]interface{}{"filter": clauses}}, nil
}

// chunkMetaBoostShouldClauses lifts matching chunks into the candidate pool.
// max/min have no fixed value to match and are scored by the retrieval
// service only; contains is left out because a boosted wildcard over the
// whole pool is expensive for a small ranking gain.
func chunkMetaBoostShouldClauses(boosts []common.ChunkMetaBoost) []interface{} {
	clauses := make([]interface{}, 0, len(boosts))
	for _, b := range boosts {
		esBoost := 1 + chunkMetaBoostScale*b.Weight
		switch b.Op {
		case "=":
			clause := chunkMetaTerm(b.Key, b.Value)
			clause["term"].(map[string]interface{})[common.ChunkMetaKwdField(b.Key)].(map[string]interface{})["boost"] = esBoost
			clauses = append(clauses, clause)
		case "in":
			members := common.ChunkMetaValueList(b.Value)
			if len(members) == 0 {
				continue
			}
			should := make([]interface{}, 0, len(members))
			for _, m := range members {
				should = append(should, chunkMetaTerm(b.Key, m))
			}
			clauses = append(clauses, map[string]interface{}{"bool": map[string]interface{}{
				"should": should, "minimum_should_match": 1, "boost": esBoost,
			}})
		case ">", "<", "≥", "≤":
			clauses = append(clauses, chunkMetaRange(b.Key, b.Op, b.Value, esBoost))
		}
	}
	return clauses
}

// applyChunkMetaScope adds the chunk metadata filter and boost clauses to a
// bool query built by buildBoolQueryFromCondition. Boost clauses go to
// should, which only adds score because the query always carries filter
// clauses (kb_id at least); when the query already uses should as a
// constraint (an id list), they are nested so they stay optional.
func applyChunkMetaScope(boolQuery map[string]interface{}, scope *common.ChunkMetaScope) (map[string]interface{}, error) {
	if scope.IsEmpty() {
		return boolQuery, nil
	}
	if boolQuery == nil {
		boolQuery = map[string]interface{}{"bool": map[string]interface{}{}}
	}
	boolMap, ok := boolQuery["bool"].(map[string]interface{})
	if !ok {
		return boolQuery, nil
	}
	if scope.Filter != nil && len(scope.Filter.Conditions) > 0 {
		clause, err := buildChunkMetaFilterClause(scope.Filter)
		if err != nil {
			return nil, err
		}
		filters, _ := boolMap["filter"].([]interface{})
		boolMap["filter"] = append(filters, clause)
	}
	boosts := chunkMetaBoostShouldClauses(scope.Boosts)
	if len(boosts) == 0 {
		return boolQuery, nil
	}
	if _, constrained := boolMap["minimum_should_match"]; constrained {
		must, _ := boolMap["must"].([]interface{})
		boolMap["must"] = append(must, map[string]interface{}{"bool": map[string]interface{}{
			"should":               boolMap["should"],
			"minimum_should_match": boolMap["minimum_should_match"],
		}})
		delete(boolMap, "minimum_should_match")
		boolMap["should"] = boosts
		return boolQuery, nil
	}
	should, _ := boolMap["should"].([]interface{})
	boolMap["should"] = append(should, boosts...)
	return boolQuery, nil
}

// UpdateChunkMetadata sets and removes chunk metadata fields on every chunk of
// the given documents with one update_by_query, so documents that share the
// same metadata are written together. Values travel as script params, so a
// multi-valued keyword stays a list.
func (e *Engine) UpdateChunkMetadata(ctx context.Context, indexName, datasetID string, docIDs []string, setFields map[string]interface{}, removeFields []string, refresh bool) error {
	if len(docIDs) == 0 {
		return nil
	}
	var script strings.Builder
	params := make(map[string]interface{}, len(setFields))
	i := 0
	for k, v := range setFields {
		if !chunkMetaFieldNameRE.MatchString(k) {
			return fmt.Errorf("invalid chunk metadata field %q", k)
		}
		name := fmt.Sprintf("s%d", i)
		i++
		params[name] = v
		fmt.Fprintf(&script, "ctx._source['%s'] = params['%s'];", k, name)
	}
	for _, k := range removeFields {
		if _, set := setFields[k]; set {
			continue
		}
		if !chunkMetaFieldNameRE.MatchString(k) {
			return fmt.Errorf("invalid chunk metadata field %q", k)
		}
		fmt.Fprintf(&script, "ctx._source.remove('%s');", k)
	}
	if script.Len() == 0 {
		return nil
	}
	body := map[string]interface{}{
		"query": map[string]interface{}{"bool": map[string]interface{}{"filter": []interface{}{
			map[string]interface{}{"term": map[string]interface{}{"kb_id": datasetID}},
			map[string]interface{}{"terms": map[string]interface{}{"doc_id": docIDs}},
		}}},
		"script": map[string]interface{}{"source": script.String(), "params": params, "lang": "painless"},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal chunk metadata update: %w", err)
	}
	waitForCompletion := true
	req := esapi.UpdateByQueryRequest{
		Index:             []string{indexName},
		Body:              bytes.NewReader(bodyBytes),
		Refresh:           &refresh,
		Conflicts:         "proceed",
		Slices:            "auto",
		WaitForCompletion: &waitForCompletion,
	}
	res, err := req.Do(ctx, e.client)
	if err != nil {
		return fmt.Errorf("chunk metadata update failed: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		if res.StatusCode == 404 {
			// No chunk index yet: nothing to update.
			return nil
		}
		respBody, _ := io.ReadAll(res.Body)
		return fmt.Errorf("chunk metadata update error: %s, body: %s", res.Status(), string(respBody))
	}
	return nil
}

// RefreshChunkIndex makes preceding no-refresh writes searchable.
func (e *Engine) RefreshChunkIndex(ctx context.Context, indexName string) error {
	res, err := esapi.IndicesRefreshRequest{Index: []string{indexName}}.Do(ctx, e.client)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.IsError() && res.StatusCode != 404 {
		return fmt.Errorf("refresh %s failed: %s", indexName, res.Status())
	}
	return nil
}

// ScanDocMetadata calls fn with every document's metadata of a dataset, paged
// with search_after so the scan is not cut at the result window.
func (e *Engine) ScanDocMetadata(ctx context.Context, tenantID, datasetID string, fn func(docID string, meta map[string]interface{}) error) error {
	indexName := buildMetadataIndexName(tenantID)
	exists, err := e.indexExists(ctx, indexName)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	const pageSize = 1000
	var after []interface{}
	for {
		body := map[string]interface{}{
			"size":    pageSize,
			"query":   map[string]interface{}{"term": map[string]interface{}{"kb_id": datasetID}},
			"sort":    []interface{}{map[string]interface{}{"id": "asc"}},
			"_source": []string{"id", "meta_fields"},
		}
		if after != nil {
			body["search_after"] = after
		}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		res, err := e.client.Search(
			e.client.Search.WithContext(ctx),
			e.client.Search.WithIndex(indexName),
			e.client.Search.WithBody(&buf),
		)
		if err != nil {
			return fmt.Errorf("doc metadata scan failed: %w", err)
		}
		var esResp SearchResponse
		if res.IsError() {
			respBody, _ := io.ReadAll(res.Body)
			res.Body.Close()
			return fmt.Errorf("doc metadata scan error: %s, body: %s", res.Status(), string(respBody))
		}
		err = json.NewDecoder(res.Body).Decode(&esResp)
		res.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to parse doc metadata scan: %w", err)
		}
		hits := esResp.Hits.Hits
		for _, hit := range hits {
			docID, _ := hit.Source["id"].(string)
			if docID == "" {
				docID = hit.ID
			}
			meta := map[string]interface{}{}
			switch v := hit.Source["meta_fields"].(type) {
			case map[string]interface{}:
				meta = v
			case string:
				if jsonErr := json.Unmarshal([]byte(v), &meta); jsonErr != nil {
					common.Warn("unreadable meta_fields in doc metadata scan", zap.String("doc_id", docID))
				}
			}
			if err := fn(docID, meta); err != nil {
				return err
			}
		}
		if len(hits) < pageSize || len(hits[len(hits)-1].Sort) == 0 {
			return nil
		}
		after = hits[len(hits)-1].Sort
	}
}
