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

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/elastic/go-elasticsearch/v8/esapi"
)

// scanKeepAlive is how long a scroll context lives between two pages;
// scanKeepAliveParam is the same in Elasticsearch time units.
const (
	scanKeepAlive      = 5 * time.Minute
	scanKeepAliveParam = "5m"
)

// ScanOptions selects what ScanChunks streams.
type ScanOptions struct {
	// Fields is the _source subset returned with every chunk.
	Fields []string
	// ExcludeWithFields skips chunks that carry any of these fields.
	ExcludeWithFields []string
	// Slice and Slices split the scan into Slices disjoint parts, of which
	// this scan takes part Slice (0-based). Slices <= 1 scans everything.
	Slice  int
	Slices int
	// Size is the number of chunks per page.
	Size int
}

// ScanBody is the initial scroll request ScanChunks sends for one dataset.
//
// A slice is only asked for when there is more than one: a single-slice
// scroll is the plain scan, and {"id": 0, "max": 1} is rejected by
// Elasticsearch.
func ScanBody(datasetID string, opts ScanOptions) map[string]interface{} {
	boolQuery := map[string]interface{}{
		"filter": []interface{}{map[string]interface{}{"term": map[string]interface{}{"kb_id": datasetID}}},
	}
	if len(opts.ExcludeWithFields) > 0 {
		mustNot := make([]interface{}, 0, len(opts.ExcludeWithFields))
		for _, field := range opts.ExcludeWithFields {
			mustNot = append(mustNot, map[string]interface{}{"exists": map[string]interface{}{"field": field}})
		}
		boolQuery["must_not"] = mustNot
	}
	body := map[string]interface{}{
		"query":   map[string]interface{}{"bool": boolQuery},
		"_source": opts.Fields,
		"sort":    []string{"_doc"},
	}
	if opts.Size > 0 {
		body["size"] = opts.Size
	}
	if opts.Slices > 1 {
		body["slice"] = map[string]interface{}{"id": opts.Slice, "max": opts.Slices}
	}
	return body
}

type scrollPage struct {
	ScrollID string `json:"_scroll_id"`
	Hits     struct {
		Hits []struct {
			ID     string                 `json:"_id"`
			Source map[string]interface{} `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// ScanChunks streams every chunk of datasetID in indexName through fn, using a
// (optionally sliced) scroll in _doc order. It stops at the first error fn
// returns. Unlike Search it is not bounded by max_result_window, so it is
// meant for maintenance passes over a whole dataset.
func (e *Engine) ScanChunks(ctx context.Context, indexName, datasetID string, opts ScanOptions, fn func(id string, source map[string]interface{}) error) error {
	body, err := json.Marshal(ScanBody(datasetID, opts))
	if err != nil {
		return fmt.Errorf("failed to marshal scan request: %w", err)
	}
	res, err := esapi.SearchRequest{
		Index:  []string{indexName},
		Body:   bytes.NewReader(body),
		Scroll: scanKeepAlive,
	}.Do(ctx, e.client)
	page, err := decodeScrollPage(res, err)
	if err != nil {
		return fmt.Errorf("scan %s: %w", indexName, err)
	}

	scrollID := page.ScrollID
	defer func() {
		if scrollID == "" {
			return
		}
		// Scroll contexts hold segments open until they expire; free this
		// one now rather than in scanKeepAlive.
		clearBody, _ := json.Marshal(map[string]interface{}{"scroll_id": []string{scrollID}})
		if res, err := (esapi.ClearScrollRequest{Body: bytes.NewReader(clearBody)}).Do(context.WithoutCancel(ctx), e.client); err == nil {
			res.Body.Close()
		}
	}()

	for len(page.Hits.Hits) > 0 {
		for _, hit := range page.Hits.Hits {
			if err := fn(hit.ID, hit.Source); err != nil {
				return err
			}
		}
		scrollBody, _ := json.Marshal(map[string]interface{}{
			"scroll":    scanKeepAliveParam,
			"scroll_id": scrollID,
		})
		res, err := esapi.ScrollRequest{Body: bytes.NewReader(scrollBody)}.Do(ctx, e.client)
		page, err = decodeScrollPage(res, err)
		if err != nil {
			return fmt.Errorf("scroll %s: %w", indexName, err)
		}
		if page.ScrollID != "" {
			scrollID = page.ScrollID
		}
	}
	return nil
}

func decodeScrollPage(res *esapi.Response, err error) (*scrollPage, error) {
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.IsError() {
		bodyBytes, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("%s: %s", res.Status(), string(bodyBytes))
	}
	var page scrollPage
	if err := json.NewDecoder(res.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &page, nil
}

// ChunkFieldUpdate sets Fields on chunk ID and leaves every other field of the
// chunk as it is.
type ChunkFieldUpdate struct {
	ID     string
	Fields map[string]interface{}
}

// UpdateChunkFields applies partial updates to chunks by id in one bulk
// request. It returns how many chunks were updated and one message per chunk
// that was not; err is reserved for the request as a whole failing. The index
// refreshes on its own schedule.
func (e *Engine) UpdateChunkFields(ctx context.Context, indexName string, updates []ChunkFieldUpdate) (int, []string, error) {
	if len(updates) == 0 {
		return 0, nil, nil
	}
	var buf bytes.Buffer
	for _, update := range updates {
		action, err := json.Marshal(map[string]interface{}{
			"update": map[string]interface{}{"_index": indexName, "_id": update.ID},
		})
		if err != nil {
			return 0, nil, fmt.Errorf("failed to marshal bulk action: %w", err)
		}
		buf.Write(action)
		buf.WriteByte('\n')
		if err := jsonIterator.NewEncoder(&buf).Encode(map[string]interface{}{"doc": update.Fields}); err != nil {
			return 0, nil, fmt.Errorf("failed to encode update for chunk %s: %w", update.ID, err)
		}
	}

	res, err := esapi.BulkRequest{Body: bytes.NewReader(buf.Bytes())}.Do(ctx, e.client)
	if err != nil {
		return 0, nil, fmt.Errorf("failed to execute bulk request: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		bodyBytes, _ := io.ReadAll(res.Body)
		return 0, nil, fmt.Errorf("elasticsearch bulk request returned error: %s, body: %s", res.Status(), string(bodyBytes))
	}

	var bulkResponse struct {
		Items []map[string]struct {
			ID     string          `json:"_id"`
			Status int             `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&bulkResponse); err != nil {
		return 0, nil, fmt.Errorf("failed to parse bulk response: %w", err)
	}
	updated := 0
	var failures []string
	for _, item := range bulkResponse.Items {
		for _, op := range item {
			if len(op.Error) > 0 && string(op.Error) != "null" {
				failures = append(failures, fmt.Sprintf("chunk %s: %s", op.ID, string(op.Error)))
				continue
			}
			updated++
		}
	}
	return updated, failures, nil
}
