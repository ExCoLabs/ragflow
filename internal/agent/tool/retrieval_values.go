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

// retrieval_values.go — the metadata value lister that accompanies a
// Retrieval tool with filter_args, so the model picks filter values that
// exist instead of guessing them.

package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"golang.org/x/sync/singleflight"
	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/dao"
)

const (
	metadataValuesToolSuffix   = "_filter_values"
	metadataValuesDefaultLimit = 50
	metadataValuesMaxLimit     = 200
	metadataValuesTTL          = 5 * time.Minute
	metadataValuesMaxEntries   = 64
)

// metadataValueCounter is the optional enhancer surface behind the value
// lister: documents per value of each key, among docIDs when non-nil.
type metadataValueCounter interface {
	MetadataValueCounts(ctx context.Context, kbIDs, keys, docIDs []string) (map[string]map[string]int64, error)
}

// metadataValueLister is implemented by the concrete retrieval service; the
// tool reaches it through the registry like the document scope checker.
type metadataValueLister interface {
	listMetadataValues(ctx context.Context, db *gorm.DB, req metadataValuesRequest) (*metadataValuesEntry, bool, error)
}

type metadataValuesRequest struct {
	DatasetIDs []string
	TenantID   string
	ProjectID  string
	Keys       []string
	Conditions []map[string]any
}

// metadataValuesEntry is one cached answer; read-only once stored.
type metadataValuesEntry struct {
	counts map[string]map[string]int64
	// scopeDocs is the number of documents counted over, -1 for whole datasets.
	scopeDocs    int
	requestScope []any
	expires      time.Time
}

// metadataValuesCache keeps value counts for a few minutes: one aggregation
// over a large dataset takes seconds, and an agent asks for several keys in a
// row. Failed loads are not cached.
type metadataValuesCache struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*metadataValuesEntry
	loads   singleflight.Group
}

func newMetadataValuesCache(ttl time.Duration, maxEntries int, now func() time.Time) *metadataValuesCache {
	return &metadataValuesCache{ttl: ttl, max: maxEntries, now: now, entries: map[string]*metadataValuesEntry{}}
}

var metadataValues = newMetadataValuesCache(metadataValuesTTL, metadataValuesMaxEntries, time.Now)

func (c *metadataValuesCache) get(key string, load func() (*metadataValuesEntry, error)) (*metadataValuesEntry, bool, error) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	fresh := ok && c.now().Before(entry.expires)
	c.mu.Unlock()
	if fresh {
		return entry, true, nil
	}
	loaded, err, _ := c.loads.Do(key, func() (any, error) {
		entry, err := load()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		entry.expires = c.now().Add(c.ttl)
		c.evictLocked()
		c.entries[key] = entry
		return entry, nil
	})
	if err != nil {
		return nil, false, err
	}
	return loaded.(*metadataValuesEntry), false, nil
}

// evictLocked drops expired entries and, at capacity, the one expiring first.
func (c *metadataValuesCache) evictLocked() {
	now := c.now()
	oldest := ""
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			delete(c.entries, key)
			continue
		}
		if oldest == "" || entry.expires.Before(c.entries[oldest].expires) {
			oldest = key
		}
	}
	if len(c.entries) >= c.max && oldest != "" {
		delete(c.entries, oldest)
	}
}

func metadataValuesCacheKey(kbIDs, keys []string, filter map[string]any) string {
	sortedKBs := slices.Sorted(slices.Values(kbIDs))
	sortedKeys := slices.Sorted(slices.Values(keys))
	scope, _ := json.Marshal(filter)
	return strings.Join(sortedKBs, ",") + "\x00" + strings.Join(sortedKeys, ",") + "\x00" + string(scope)
}

// listMetadataValues counts the documents per metadata value for the tool's
// datasets, inside the request scope (strict, nothing relaxed), the project
// hint and the caller's own conditions. Authorization runs on every call,
// before the cache is consulted.
func (a *NLPRetrievalAdapter) listMetadataValues(ctx context.Context, db *gorm.DB, req metadataValuesRequest) (*metadataValuesEntry, bool, error) {
	if a == nil || a.enhancer == nil {
		return nil, false, ErrRetrievalServiceMissing
	}
	counter, ok := a.enhancer.(metadataValueCounter)
	if !ok {
		return nil, false, fmt.Errorf("retrieval: metadata value listing is not configured")
	}
	datasets, err := a.resolveDatasets(ctx, db, RetrievalRequest{DatasetIDs: req.DatasetIDs, TenantID: req.TenantID})
	if err != nil {
		return nil, false, err
	}
	if len(datasets.tenantIDs) != 1 {
		return nil, false, fmt.Errorf("retrieval: datasets span multiple tenants")
	}
	requestScope := runtime.RequestScopeFromContext(ctx)
	requestScoped, err := requestScope.AppliesTo(datasets.kbIDs)
	if err != nil {
		return nil, false, fmt.Errorf("retrieval: request scope: %w", err)
	}
	if err := authorizeRetrievalDatasets(ctx, db, datasets.kbIDs); err != nil {
		return nil, false, err
	}

	var filter map[string]any
	if requestScoped {
		filter = requestScope.FilterAtLevel(0)
	}
	requested, _ := filter["manual"].([]any)
	conditions := req.Conditions
	if req.ProjectID != "" {
		if projectIDPattern.FindString(req.ProjectID) != req.ProjectID {
			return nil, false, fmt.Errorf("retrieval: invalid canonical project id")
		}
		conditions = append([]map[string]any{{"key": "project_id", "op": "in", "value": req.ProjectID}}, conditions...)
	}
	filter = withModelConditions(filter, conditions)

	return metadataValues.get(metadataValuesCacheKey(datasets.kbIDs, req.Keys, filter), func() (*metadataValuesEntry, error) {
		entry := &metadataValuesEntry{scopeDocs: -1, requestScope: requested}
		var docIDs []string
		if filter != nil {
			ids, err := a.resolveScope(ctx, filter, nil, datasets.kbIDs, "values", true)
			if err != nil {
				return nil, err
			}
			docIDs = append([]string{}, ids...)
			entry.scopeDocs = len(docIDs)
		}
		counts, err := counter.MetadataValueCounts(ctx, datasets.kbIDs, req.Keys, docIDs)
		if err != nil {
			return nil, fmt.Errorf("retrieval: count metadata values: %w", err)
		}
		entry.counts = counts
		return entry, nil
	})
}

// MetadataValuesTool lists the values of the metadata keys its Retrieval tool
// accepts in `filters`, with document counts.
type MetadataValuesTool struct {
	retrieval *RetrievalTool
}

type metadataValuesArgs struct {
	Key      string          `json:"key"`
	Contains string          `json:"contains,omitempty"`
	Limit    int             `json:"limit,omitempty"`
	Filters  json.RawMessage `json:"filters,omitempty"`
}

type metadataValueCount struct {
	Value string `json:"value"`
	Docs  int64  `json:"docs"`
}

type metadataValuesResult struct {
	Key          string               `json:"key,omitempty"`
	Values       []metadataValueCount `json:"values"`
	TotalValues  int                  `json:"total_values"`
	Truncated    bool                 `json:"truncated,omitempty"`
	Filters      []map[string]any     `json:"filters,omitempty"`
	RequestScope []any                `json:"request_scope,omitempty"`
	DocsInScope  *int                 `json:"docs_in_scope,omitempty"`
	Cached       bool                 `json:"cached"`
	Ms           int64                `json:"ms"`
	Error        string               `json:"_ERROR,omitempty"`
}

// CompanionTools implements companionToolProvider.
func (r *RetrievalTool) CompanionTools() []tool.BaseTool {
	if r.defaults.FilterArgs == nil {
		return nil
	}
	return []tool.BaseTool{&MetadataValuesTool{retrieval: r}}
}

// valuesToolName derives the companion's name from the retrieval tool's, so
// two retrieval tools on one Agent keep separate listers.
func (r *RetrievalTool) valuesToolName() string {
	if cfg := r.defaults.FilterArgs; cfg != nil && cfg.ValuesTool != "" {
		return cfg.ValuesTool
	}
	return r.name + metadataValuesToolSuffix
}

func (t *MetadataValuesTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	cfg := t.retrieval.defaults.FilterArgs
	return &schema.ToolInfo{
		Name: t.retrieval.valuesToolName(),
		Desc: fmt.Sprintf("Lists the values one metadata key takes in the documents %s searches, with the number of documents holding each value, most frequent first. "+
			"Call it before passing `filters` to %s, and use the values exactly as returned. Counts may lag new uploads by a few minutes.",
			t.retrieval.name, t.retrieval.name),
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{
			"key": {
				Type:     schema.String,
				Enum:     cfg.keyNames(),
				Desc:     "The metadata key to list. " + cfg.describeKeys(),
				Required: true,
			},
			"contains": {
				Type: schema.String,
				Desc: "Keep only values containing this text (case and diacritics are ignored).",
			},
			"limit": {
				Type: schema.Integer,
				Desc: fmt.Sprintf("Maximum values to return (default %d, at most %d).", metadataValuesDefaultLimit, metadataValuesMaxLimit),
			},
			"filters": cfg.filtersParam("this tool",
				"Optional conditions on other keys: count only documents matching them, e.g. the phases of one project."),
		}),
	}, nil
}

func (t *MetadataValuesTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	started := time.Now()
	cfg := t.retrieval.defaults.FilterArgs
	fail := func(message string) (string, error) {
		return marshalMetadataValues(metadataValuesResult{Values: []metadataValueCount{}, Error: message})
	}
	var args metadataValuesArgs
	if argumentsInJSON != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return fail("Invalid arguments: expected {key, contains?, limit?, filters?}.")
		}
	}
	args.Key = strings.TrimSpace(args.Key)
	if _, known := cfg.key(args.Key); !known {
		return fail(fmt.Sprintf("Key %q cannot be listed; available keys: %s.", args.Key, strings.Join(cfg.keyNames(), ", ")))
	}
	conditions, problems := cfg.parseModelFilters(args.Filters)
	if len(problems) > 0 {
		return fail(invalidFiltersMessage(problems, cfg))
	}

	bound := t.retrieval.mergeDefaults(retrievalArgs{})
	if bound.RetrievalFrom != "dataset" || len(bound.DatasetIDs) == 0 {
		return "", fmt.Errorf("retrieval: dataset_ids is required")
	}
	if err := t.retrieval.validateRequestScopeBinding(ctx, bound); err != nil {
		return "", err
	}
	datasetIDs, err := resolveRetrievalDatasetIDs(ctx, bound.DatasetIDs)
	if err != nil {
		return "", err
	}
	projectID, err := retrievalProjectID(ctx, "dataset", t.retrieval.defaults.MetaDataFilter)
	if err != nil {
		return "", err
	}
	svc := GetRetrievalService()
	if unwrapper, ok := svc.(interface{ Unwrap() RetrievalService }); ok {
		svc = unwrapper.Unwrap()
	}
	lister, ok := svc.(metadataValueLister)
	if !ok {
		return "", fmt.Errorf("retrieval: metadata value listing is not configured")
	}
	entry, cached, err := lister.listMetadataValues(ctx, dao.DB, metadataValuesRequest{
		DatasetIDs: datasetIDs,
		TenantID:   retrievalTenantID(ctx),
		ProjectID:  projectID,
		Keys:       cfg.keyNames(),
		Conditions: conditions,
	})
	if err != nil {
		return "", err
	}

	needle := foldMetadataValue(args.Contains)
	matched := make([]metadataValueCount, 0, len(entry.counts[args.Key]))
	for value, docs := range entry.counts[args.Key] {
		if needle == "" || strings.Contains(foldMetadataValue(value), needle) {
			matched = append(matched, metadataValueCount{Value: value, Docs: docs})
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].Docs != matched[j].Docs {
			return matched[i].Docs > matched[j].Docs
		}
		return matched[i].Value < matched[j].Value
	})
	limit := args.Limit
	if limit <= 0 {
		limit = metadataValuesDefaultLimit
	}
	limit = min(limit, metadataValuesMaxLimit)
	out := metadataValuesResult{
		Key:          args.Key,
		Values:       matched[:min(limit, len(matched))],
		TotalValues:  len(matched),
		Truncated:    len(matched) > limit,
		Filters:      conditions,
		RequestScope: entry.requestScope,
		Cached:       cached,
		Ms:           time.Since(started).Milliseconds(),
	}
	if entry.scopeDocs >= 0 {
		out.DocsInScope = &entry.scopeDocs
	}
	return marshalMetadataValues(out)
}

func marshalMetadataValues(out metadataValuesResult) (string, error) {
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("retrieval: marshal metadata values: %w", err)
	}
	return string(encoded), nil
}

// foldMetadataValue lower-cases and strips diacritics, for the `contains`
// convenience match only; filters themselves match as the metadata filter does.
func foldMetadataValue(value string) string {
	decomposed := norm.NFD.String(strings.ToLower(strings.TrimSpace(value)))
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}, decomposed)
}
