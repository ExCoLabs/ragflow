package tool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
	"ragflow/internal/dao"
	"ragflow/internal/entity"
	modelModule "ragflow/internal/entity/models"
	"ragflow/internal/permission"
	"ragflow/internal/service/nlp"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func useRetrievalService(t *testing.T, svc RetrievalService) {
	t.Helper()
	previous := GetRetrievalService()
	SetRetrievalService(svc)
	t.Cleanup(func() { SetRetrievalService(previous) })
}

func semiAutoToolParams() map[string]any {
	return map[string]any{
		"function_name": "search_archa_data",
		"description":   "Search project documents.",
		"dataset_ids":   []any{"kb-1"},
		"top_n":         5,
		"meta_data_filter": map[string]any{
			"method":    "semi_auto",
			"semi_auto": []any{map[string]any{"key": "project", "op": "contains"}, map[string]any{"key": "phase", "op": "in"}},
			"boost":     map[string]any{"method": "auto"},
		},
	}
}

// A DSL without filter_args must keep its model-facing schema, its request to
// the retrieval service and its result byte for byte, even when the model
// sends a `filters` argument nobody declared.
func TestRetrievalWithoutFilterArgsIsUnchanged(t *testing.T) {
	svc := &capturingRetrievalService{}
	useRetrievalService(t, svc)
	built, err := BuildByName("retrieval", semiAutoToolParams())
	if err != nil {
		t.Fatal(err)
	}
	tool := built.(*RetrievalTool)
	info, err := tool.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	schema, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	schemaJSON, _ := json.Marshal(schema)
	const wantSchema = `{"properties":{"query":{"description":"The keywords to search the dataset. The keywords should be the most important words/terms (including synonyms) from the original request.","type":"string"},"scope_drop":{"description":"Drop request-scope filter keys for this call, e.g. [\"phase\"] or [\"flow\",\"phase\"]. If a previous response reported scope_relaxable keys and its documents do not answer the request, call this tool again listing those keys here to broaden the search before concluding the information is missing. Only keys returned as scope_relaxable are accepted; other keys are ignored.","type":"array"}},"required":["query"],"type":"object"}`
	if string(schemaJSON) != wantSchema {
		t.Fatalf("schema changed:\n%s", schemaJSON)
	}
	for _, args := range []string{
		`{"query":"alpha","boost":[{"key":"sector","value":["g"],"weight":0.5}]}`,
		`{"query":"alpha","boost":"not a boost list","filters":7}`,
	} {
		out, err := tool.InvokableRun(t.Context(), args)
		if err != nil {
			t.Fatal(err)
		}
		const wantOut = `{"formalized_content":"[ID:ck-1] answer\n","chunks":[{"id":"ck-1","content":"answer"}]}`
		if out != wantOut {
			t.Fatalf("%s: result changed:\n%s", args, out)
		}
	}
	out, err := tool.InvokableRun(t.Context(), `{"query":"alpha","filters":[{"key":"project","op":"=","value":["x"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	const wantOut = `{"formalized_content":"[ID:ck-1] answer\n","chunks":[{"id":"ck-1","content":"answer"}]}`
	if out != wantOut {
		t.Fatalf("result changed:\n%s", out)
	}
	req := svc.req
	if !reflect.DeepEqual(req.MetaDataFilter, semiAutoToolParams()["meta_data_filter"]) {
		t.Fatalf("meta_data_filter changed: %v", req.MetaDataFilter)
	}
	if req.Explicit != nil {
		t.Fatalf("undeclared filters reached the search: %+v", req.Explicit)
	}
	tools, err := BuildAll([]string{"retrieval"}, map[string]map[string]any{"retrieval": semiAutoToolParams()})
	if err != nil || len(tools) != 1 {
		t.Fatalf("BuildAll: %d tools, err=%v; want the retrieval tool alone", len(tools), err)
	}
}

func explicitToolParams() map[string]any {
	params := semiAutoToolParams()
	params["dataset_ids"] = []any{"kb-project"}
	params["filter_args"] = map[string]any{
		"keys": []any{
			map[string]any{"key": "project", "ops": []any{"contains", "="}, "description": "project name"},
			map[string]any{"key": "phase", "ops": []any{"in", "=="}},
			"ext",
		},
		"max_conditions": 3,
	}
	return params
}

func buildExplicitTool(t *testing.T, params map[string]any) *RetrievalTool {
	t.Helper()
	built, err := BuildByName("retrieval", params)
	if err != nil {
		t.Fatal(err)
	}
	return built.(*RetrievalTool)
}

func TestFilterArgsConfig(t *testing.T) {
	cfg := buildExplicitTool(t, explicitToolParams()).defaults.FilterArgs
	want := []filterKeyConfig{
		{Key: "project", Ops: []string{"contains", "="}, Description: "project name"},
		{Key: "phase", Ops: []string{"in", "="}},
		{Key: "ext", Ops: []string{"=", "in", "contains"}},
	}
	if !reflect.DeepEqual(cfg.Keys, want) || cfg.MaxConditions != 3 {
		t.Fatalf("config = %+v", cfg)
	}

	for name, filterArgs := range map[string]any{
		"not an object":      "project",
		"no keys":            map[string]any{"keys": []any{}},
		"negation":           map[string]any{"keys": []any{map[string]any{"key": "phase", "ops": []any{"not in"}}}},
		"unknown op":         map[string]any{"keys": []any{map[string]any{"key": "phase", "ops": []any{"≠"}}}},
		"duplicate key":      map[string]any{"keys": []any{"phase", "phase"}},
		"empty key":          map[string]any{"keys": []any{" "}},
		"bad max_conditions": map[string]any{"keys": []any{"phase"}, "max_conditions": 0},
		"unknown option":     map[string]any{"keys": []any{"phase"}, "logic": "or"},
	} {
		params := explicitToolParams()
		params["filter_args"] = filterArgs
		if _, err := BuildByName("retrieval", params); err == nil {
			t.Errorf("%s: filter_args %v was accepted", name, filterArgs)
		}
	}
	params := explicitToolParams()
	params["retrieval_from"] = "memory"
	if _, err := BuildByName("retrieval", params); err == nil {
		t.Error("filter_args was accepted on memory retrieval")
	}
}

// The model sees the allow-list in the schema, and the value lister comes
// with the retrieval tool under a name derived from it.
func TestExplicitFiltersSchemaAndCompanion(t *testing.T) {
	tools, err := BuildAll([]string{"search_archa_data"}, map[string]map[string]any{
		"search_archa_data": func() map[string]any {
			params := explicitToolParams()
			params["component_name"] = "Retrieval"
			return params
		}(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools=%d, want retrieval + value lister", len(tools))
	}
	info, _ := tools[0].Info(t.Context())
	schema, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(schema)
	var decoded struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
			Items       struct {
				Type       string `json:"type"`
				Required   []string
				Properties map[string]struct {
					Type string   `json:"type"`
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	filters := decoded.Properties["filters"]
	if filters.Type != "array" || filters.Items.Type != "object" {
		t.Fatalf("filters schema = %s", raw)
	}
	if got := filters.Items.Properties["key"].Enum; !reflect.DeepEqual(got, []string{"project", "phase", "ext"}) {
		t.Fatalf("key enum = %v", got)
	}
	if got := filters.Items.Properties["op"].Enum; !reflect.DeepEqual(got, []string{"=", "in", "contains"}) {
		t.Fatalf("op enum = %v", got)
	}
	for _, want := range []string{"project (contains, =) — project name", "phase (in, =)", "At most 3", "search_archa_data_filter_values"} {
		if !strings.Contains(filters.Description, want) {
			t.Errorf("filters description lacks %q: %s", want, filters.Description)
		}
	}
	valuesInfo, _ := tools[1].Info(t.Context())
	if valuesInfo.Name != "search_archa_data_filter_values" {
		t.Fatalf("value lister name = %q", valuesInfo.Name)
	}
	if _, err := valuesInfo.ParamsOneOf.ToJSONSchema(); err != nil {
		t.Fatal(err)
	}

	params := explicitToolParams()
	params["filter_args"].(map[string]any)["values_tool"] = "list_metadata_values"
	named := buildExplicitTool(t, params).CompanionTools()
	if info, _ := named[0].Info(t.Context()); info.Name != "list_metadata_values" {
		t.Fatalf("configured value lister name = %q", info.Name)
	}
}

// Anything outside the allow-list rejects the call before a search runs: a
// wider search than the model asked for would pass as filtered evidence.
func TestExplicitFiltersRejectInvalidArguments(t *testing.T) {
	tool := buildExplicitTool(t, explicitToolParams())
	for name, tc := range map[string]struct{ filters, want string }{
		"unknown key":       {`[{"key":"author","op":"=","value":["x"]}]`, `key "author" is not filterable`},
		"op not allowed":    {`[{"key":"project","op":"in","value":["x"]}]`, `op "in" is not allowed for key "project"`},
		"negation":          {`[{"key":"phase","op":"not in","value":["x"]}]`, `op "not in" is not allowed`},
		"missing value":     {`[{"key":"phase","op":"in"}]`, `value must be a string`},
		"empty value":       {`[{"key":"phase","op":"in","value":[" "]}]`, `non-empty`},
		"several for equal": {`[{"key":"phase","op":"=","value":["a","b"]}]`, `takes exactly one value, got 2; use op in`},
		"non-string value":  {`[{"key":"phase","op":"in","value":[true]}]`, `value must be a string`},
		"too many":          {`[{"key":"ext","op":"=","value":"a"},{"key":"ext","op":"=","value":"b"},{"key":"ext","op":"=","value":"c"},{"key":"ext","op":"=","value":"d"}]`, `at most 3`},
		"not a list":        {`"project=x"`, `must be a list`},
		"scalar items":      {`["project"]`, `filters[0]: must be a {key, op, value} object`},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &scopedEmptyRetrievalService{}
			useRetrievalService(t, svc)
			out, err := tool.InvokableRun(t.Context(), `{"query":"alpha","filters":`+tc.filters+`}`)
			if err != nil {
				t.Fatalf("a filter mistake must be tool output, got error %v", err)
			}
			var result retrievalResult
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result.Error, tc.want) || !strings.Contains(result.Error, "project (contains, =)") {
				t.Fatalf("message = %q, want %q and the allow-list", result.Error, tc.want)
			}
			if svc.calls != 0 {
				t.Fatalf("searched %d times with invalid filters", svc.calls)
			}
		})
	}
}

// Valid filters reach the search as manual conditions, and the tool's own
// LLM filter and boost are gone from the request, so nothing can call the
// filter generator.
func TestExplicitFiltersReplaceTheLLMFilter(t *testing.T) {
	svc := &capturingRetrievalService{}
	useRetrievalService(t, svc)
	tool := buildExplicitTool(t, explicitToolParams())
	_, err := tool.InvokableRun(t.Context(), `{"query":"alpha","filters":[
		{"key":"project","op":"contains","value":["Nivy"]},
		{"key":"phase","value":["DSP","DRS"],"op":"IN"},
		{"key":"ext","op":"==","value":"pdf"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"key": "project", "op": "contains", "value": "Nivy"},
		{"key": "phase", "op": "in", "value": []any{"DSP", "DRS"}},
		{"key": "ext", "op": "=", "value": "pdf"},
	}
	if svc.req.Explicit == nil || !reflect.DeepEqual(svc.req.Explicit.Conditions, want) {
		t.Fatalf("explicit = %+v", svc.req.Explicit)
	}
	if svc.req.MetaDataFilter != nil {
		t.Fatalf("LLM filter still on the request: %v", svc.req.MetaDataFilter)
	}

	// No filters from the model: still no LLM filter, and nothing to scope by.
	if _, err := tool.InvokableRun(t.Context(), `{"query":"alpha"}`); err != nil {
		t.Fatal(err)
	}
	if svc.req.Explicit == nil || len(svc.req.Explicit.Conditions) != 0 || svc.req.MetaDataFilter != nil {
		t.Fatalf("request = explicit %+v, filter %v", svc.req.Explicit, svc.req.MetaDataFilter)
	}
}

func TestWithoutLLMMetaFilter(t *testing.T) {
	manualRows := []any{map[string]any{"key": "flow", "op": "=", "value": "out", "weight": 0.2}}
	conditions := []any{map[string]any{"key": "ext", "op": "in", "value": []any{"pdf"}}}
	for name, tc := range map[string]struct{ in, want map[string]any }{
		"nil":                    {nil, nil},
		"semi_auto + auto boost": {map[string]any{"method": "semi_auto", "semi_auto": []any{"phase"}, "boost": map[string]any{"method": "auto"}}, nil},
		"auto":                   {map[string]any{"method": "auto"}, nil},
		"disabled":               {map[string]any{"method": "disabled"}, nil},
		"manual filter stays": {
			map[string]any{"method": "manual", "manual": conditions, "boost": map[string]any{"method": "semi_auto", "semi_auto": []any{"phase"}}},
			map[string]any{"method": "manual", "manual": conditions},
		},
		"fixed boost rows stay without the LLM part": {
			map[string]any{"method": "semi_auto", "semi_auto": []any{"phase"}, "boost": map[string]any{"method": "auto", "manual": manualRows, "max_total": 0.5}},
			map[string]any{"method": "disabled", "semi_auto": []any{"phase"}, "boost": map[string]any{"method": "manual", "manual": manualRows, "max_total": 0.5}},
		},
		"boost switched off": {map[string]any{"method": "auto", "boost": map[string]any{"method": "off", "manual": manualRows}}, nil},
	} {
		original, _ := json.Marshal(tc.in)
		if got := withoutLLMMetaFilter(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
		if after, _ := json.Marshal(tc.in); string(after) != string(original) {
			t.Errorf("%s: the tool's own filter was modified: %s", name, after)
		}
	}
}

// explicitDocs is the metadata the pipeline fixture filters over.
var explicitDocs = map[string]map[string]string{
	"doc-a": {"project": "Nivy Tower", "phase": "DSP", "flow": "out"},
	"doc-b": {"project": "Nivy Tower", "phase": "DRS", "flow": "in"},
	"doc-c": {"project": "Eurovea", "phase": "DSP", "flow": "in"},
}

// explicitEnhancer evaluates manual filters over explicitDocs and records
// which resolution path each one took.
type explicitEnhancer struct {
	retrievalEnhancer
	slow, fast []map[string]any
	scopeCalls int
	countCalls int
	countDocs  [][]string
	countErr   error
	// chunkMeta is what ChunkMetadataConfig reports; scoped records the
	// filters ScopeDocuments was given.
	chunkMeta *common.ChunkMetadataConfig
	scoped    []map[string]any
}

func (e *explicitEnhancer) ChunkMetadataConfig([]*entity.Knowledgebase) *common.ChunkMetadataConfig {
	return e.chunkMeta
}

func explicitFilterKeys(filter map[string]any) []string {
	var keys []string
	for _, item := range filter["manual"].([]any) {
		keys = append(keys, item.(map[string]any)["key"].(string))
	}
	return keys
}

func matchExplicitDocs(filter map[string]any) []string {
	var ids []string
	for id, meta := range explicitDocs {
		matched := true
		for _, item := range filter["manual"].([]any) {
			condition := item.(map[string]any)
			stored := meta[condition["key"].(string)]
			switch condition["op"] {
			case "=":
				matched = matched && stored == condition["value"]
			case "contains":
				matched = matched && strings.Contains(stored, condition["value"].(string))
			case "in":
				// A string list is the request scope's shape, []any the model's.
				switch values := condition["value"].(type) {
				case []any:
					matched = matched && slices.Contains(values, any(stored))
				case []string:
					matched = matched && slices.Contains(values, stored)
				}
			}
		}
		if matched {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (e *explicitEnhancer) FilterDocuments(_ context.Context, filter map[string]any, _ string, _ *modelModule.ChatModel, _, _ []string, _ *common.MetadataFilterDiagnostic) ([]string, error) {
	e.slow = append(e.slow, filter)
	return matchExplicitDocs(filter), nil
}

func (e *explicitEnhancer) PushdownDocuments(_ context.Context, filter map[string]any, _, _ []string) ([]string, error) {
	e.fast = append(e.fast, filter)
	return matchExplicitDocs(filter), nil
}

// ScopeDocuments hands fixed boosts on to the scorer the way
// service.ApplyMetaDataScope does on active chunk metadata, and never narrows.
func (e *explicitEnhancer) ScopeDocuments(_ context.Context, filter map[string]any, _ string, _ *modelModule.ChatModel, base []string, _ []*entity.Knowledgebase, _ *common.MetadataFilterDiagnostic) ([]string, *common.ChunkMetaScope, error) {
	e.scopeCalls++
	e.scoped = append(e.scoped, filter)
	if !e.chunkMeta.Active() {
		return base, nil, nil
	}
	boost := common.ParseMetaBoost(filter["boost"], e.chunkMeta.Fields)
	if len(boost.Manual) == 0 {
		return base, nil, nil
	}
	return base, &common.ChunkMetaScope{Boosts: boost.Manual, BoostMaxTotal: boost.MaxTotal}, nil
}

func (e *explicitEnhancer) MetadataValueCounts(_ context.Context, _, keys, docIDs []string) (map[string]map[string]int64, error) {
	e.countCalls++
	e.countDocs = append(e.countDocs, docIDs)
	if e.countErr != nil {
		return nil, e.countErr
	}
	counts := map[string]map[string]int64{}
	for _, key := range keys {
		counts[key] = map[string]int64{}
	}
	for id, meta := range explicitDocs {
		if docIDs != nil && !slices.Contains(docIDs, id) {
			continue
		}
		for _, key := range keys {
			if value := meta[key]; value != "" {
				counts[key][value]++
			}
		}
	}
	return counts, nil
}

func (e *explicitEnhancer) LabelQuestion(context.Context, string, []*entity.Knowledgebase) map[string]float64 {
	return nil
}
func (e *explicitEnhancer) EnhanceTOC(_ context.Context, _ *modelModule.ChatModel, _, _ []string, _ string, _ int, rows []map[string]any) ([]map[string]any, error) {
	return rows, nil
}
func (e *explicitEnhancer) RetrieveByChildren(_ context.Context, rows []map[string]any, _ []string) []map[string]any {
	return rows
}

// newExplicitPipeline installs a real NLPRetrievalAdapter over a fake engine
// that returns one chunk per document whatever the scope, so only the
// adapter's own scoping decides what comes back. scopeJSON may be empty.
func newExplicitPipeline(t *testing.T, scopeJSON string) (*explicitEnhancer, *projectPipelineEngine, context.Context) {
	t.Helper()
	if err := nlp.InitQueryBuilder(""); err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&entity.Knowledgebase{}, &entity.UserTenant{}, &entity.Document{}); err != nil {
		t.Fatal(err)
	}
	previousDB := dao.DB
	dao.DB = db
	t.Cleanup(func() { dao.DB = previousDB })
	valid := string(entity.StatusValid)
	kb := &entity.Knowledgebase{ID: "kb-project", TenantID: "tenant", CreatedBy: "owner", Permission: string(entity.TenantPermissionMe), Status: &valid}
	if err = db.Create(kb).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&entity.UserTenant{ID: "membership", UserID: "owner", TenantID: "tenant", Role: string(permission.RoleOwner), InvitedBy: "owner", Status: &valid}).Error; err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, id := range []string{"doc-a", "doc-b", "doc-c"} {
		if err = db.Create(&entity.Document{ID: id, KbID: kb.ID, ParserConfig: entity.JSONMap{}}).Error; err != nil {
			t.Fatal(err)
		}
		rows = append(rows, map[string]any{"id": "chunk-" + id, "doc_id": id, "docnm_kwd": id + ".pdf", "kb_id": kb.ID, "content_ltks": "acoustic", "content_with_weight": "acoustic " + id, "_score": 0.9})
	}
	docEngine := &projectPipelineEngine{rows: rows}
	enhancer := &explicitEnhancer{}
	adapter := &NLPRetrievalAdapter{svc: nlp.NewRetrievalService(docEngine, &dao.DocumentDAO{}), kbDAO: fakeKnowledgebaseLookup{kbs: []*entity.Knowledgebase{kb}}, enhancer: enhancer, modelConstructors: RetrievalModelConstructors{
		Embedding: func(context.Context, string, string) (*modelModule.EmbeddingModel, error) { return nil, nil },
		Chat: func(context.Context, string, string) (*modelModule.ChatModel, error) {
			t.Error("a chat model was resolved: explicit filters must not need an LLM")
			return nil, nil
		},
	}}
	useRetrievalService(t, adapter)
	previousCache := metadataValues
	metadataValues = newMetadataValuesCache(metadataValuesTTL, metadataValuesMaxEntries, time.Now)
	t.Cleanup(func() { metadataValues = previousCache })

	state := runtime.NewCanvasState("run", "task")
	state.Sys["user_id"] = "owner"
	ctx := runtime.WithState(t.Context(), state)
	if scopeJSON != "" {
		scope, err := runtime.ParseRequestScope(json.RawMessage(scopeJSON))
		if err != nil {
			t.Fatal(err)
		}
		ctx = runtime.WithRequestScope(ctx, scope)
	}
	return enhancer, docEngine, ctx
}

func explicitPipelineTool(t *testing.T) *RetrievalTool {
	t.Helper()
	params := explicitToolParams()
	params["keywords_similarity_weight"] = 1.0
	params["similarity_threshold"] = 0.0
	return buildExplicitTool(t, params)
}

type explicitToolResult struct {
	Chunks           []chunkPayload `json:"chunks"`
	ScopeDroppedKeys []string       `json:"scope_dropped_keys"`
	Diagnostics      *struct {
		Filters          []map[string]any `json:"filters"`
		RequestScope     []map[string]any `json:"request_scope"`
		DocsInScope      *int             `json:"docs_in_scope"`
		Boosts           []map[string]any `json:"boosts"`
		BoostInactive    string           `json:"boost_inactive"`
		BoostIgnoredKeys []string         `json:"boost_ignored_keys"`
		Chunks           int              `json:"chunks"`
		Ms               map[string]int64 `json:"ms"`
		Hint             string           `json:"hint"`
	} `json:"diagnostics"`
	Error string `json:"_ERROR"`
}

func runExplicitTool(t *testing.T, tool *RetrievalTool, ctx context.Context, args string) explicitToolResult {
	t.Helper()
	out, err := tool.InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var result explicitToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return result
}

func chunkDocs(result explicitToolResult) []string {
	docs := make([]string, 0, len(result.Chunks))
	for _, chunk := range result.Chunks {
		docs = append(docs, chunk.DocumentID)
	}
	slices.Sort(docs)
	return docs
}

// End to end without a request scope: the model's conditions alone scope the
// search, through the push-down path, with no filter LLM anywhere, and the
// result says what was applied and how long each stage took.
func TestExplicitFiltersScopeTheSearchWithoutAnLLM(t *testing.T) {
	enhancer, docEngine, ctx := newExplicitPipeline(t, "")
	result := runExplicitTool(t, explicitPipelineTool(t), ctx,
		`{"query":"acoustic","filters":[{"key":"project","op":"contains","value":["Nivy"]},{"key":"phase","op":"in","value":["DSP"]}]}`)

	if got := chunkDocs(result); !reflect.DeepEqual(got, []string{"doc-a"}) {
		t.Fatalf("chunks from %v, want doc-a only", got)
	}
	if enhancer.scopeCalls != 0 || len(enhancer.slow) != 0 || len(enhancer.fast) != 1 {
		t.Fatalf("LLM-filter calls=%d, flattened-scan resolutions=%d, push-down resolutions=%d; want 0, 0, 1", enhancer.scopeCalls, len(enhancer.slow), len(enhancer.fast))
	}
	if len(docEngine.requests) != 1 {
		t.Fatalf("engine searches=%d", len(docEngine.requests))
	}
	diag := result.Diagnostics
	if diag == nil || diag.DocsInScope == nil || *diag.DocsInScope != 1 || diag.Chunks != 1 || len(diag.Filters) != 2 || diag.RequestScope != nil {
		t.Fatalf("diagnostics = %+v", diag)
	}
	for _, stage := range []string{"total", "scope", "embedding", "search", "rerank"} {
		if _, ok := diag.Ms[stage]; !ok {
			t.Errorf("diagnostics lack the %s timing: %v", stage, diag.Ms)
		}
	}

	// No filters: the whole dataset, still without the LLM filter.
	result = runExplicitTool(t, explicitPipelineTool(t), ctx, `{"query":"acoustic"}`)
	if got := chunkDocs(result); len(got) != 3 || result.Diagnostics.DocsInScope != nil || len(result.Diagnostics.Filters) != 0 {
		t.Fatalf("unfiltered call: chunks from %v, diagnostics %+v", got, result.Diagnostics)
	}
	if enhancer.scopeCalls != 0 {
		t.Fatalf("LLM-filter calls=%d", enhancer.scopeCalls)
	}
}

// The request scope is a floor: model conditions AND with it, so a condition
// naming another project finds nothing instead of widening the search.
func TestExplicitFiltersIntersectTheRequestScope(t *testing.T) {
	enhancer, docEngine, ctx := newExplicitPipeline(t,
		`{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project","op":"in","value":["Nivy Tower"]}]}`)
	tool := explicitPipelineTool(t)

	result := runExplicitTool(t, tool, ctx, `{"query":"acoustic","filters":[{"key":"phase","op":"=","value":"DSP"}]}`)
	if got := chunkDocs(result); !reflect.DeepEqual(got, []string{"doc-a"}) {
		t.Fatalf("chunks from %v, want doc-a (Nivy ∩ DSP)", got)
	}
	if len(enhancer.fast) != 1 || !reflect.DeepEqual(explicitFilterKeys(enhancer.fast[0]), []string{"project", "phase"}) {
		t.Fatalf("resolved %v, want one filter carrying the request scope and the model condition", enhancer.fast)
	}
	if scope := result.Diagnostics.RequestScope; len(scope) != 1 || scope[0]["key"] != "project" {
		t.Fatalf("request scope not reported: %v", scope)
	}

	searches := len(docEngine.requests)
	result = runExplicitTool(t, tool, ctx, `{"query":"acoustic","filters":[{"key":"project","op":"=","value":"Eurovea"}]}`)
	if len(result.Chunks) != 0 || *result.Diagnostics.DocsInScope != 0 || !strings.Contains(result.Diagnostics.Hint, "search_archa_data_filter_values") {
		t.Fatalf("widening attempt: chunks=%v diagnostics=%+v", result.Chunks, result.Diagnostics)
	}
	if len(docEngine.requests) != searches {
		t.Fatal("an empty scope was searched")
	}
	if keys := explicitFilterKeys(enhancer.fast[1]); !reflect.DeepEqual(keys, []string{"project", "project"}) {
		t.Fatalf("the request condition was replaced, not intersected: %v", keys)
	}
}

// Relaxation and scope_drop only ever touch keys the request declared
// relaxable; the model's own conditions and hard request keys stay on.
func TestExplicitFiltersSurviveRequestScopeRelaxation(t *testing.T) {
	const scope = `{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project","op":"in","value":["Nivy Tower"]},{"key":"flow","op":"in","value":["in"]}],"relax":["flow"]}`
	enhancer, _, ctx := newExplicitPipeline(t, scope)
	tool := explicitPipelineTool(t)

	// Strict level: Nivy ∩ flow=in ∩ DSP is empty, so the server drops flow.
	result := runExplicitTool(t, tool, ctx, `{"query":"acoustic","filters":[{"key":"phase","op":"=","value":"DSP"}]}`)
	if got := chunkDocs(result); !reflect.DeepEqual(got, []string{"doc-a"}) {
		t.Fatalf("chunks from %v, want doc-a", got)
	}
	if len(enhancer.fast) != 2 ||
		!reflect.DeepEqual(explicitFilterKeys(enhancer.fast[0]), []string{"project", "flow", "phase"}) ||
		!reflect.DeepEqual(explicitFilterKeys(enhancer.fast[1]), []string{"project", "phase"}) {
		t.Fatalf("levels resolved %v", enhancer.fast)
	}
	if !reflect.DeepEqual(result.ScopeDroppedKeys, []string{"flow"}) {
		t.Fatalf("dropped %v", result.ScopeDroppedKeys)
	}
	if scope := result.Diagnostics.RequestScope; len(scope) != 1 || scope[0]["key"] != "project" {
		t.Fatalf("reported request scope %v, want the remaining project condition", scope)
	}

	enhancer.fast = nil
	result = runExplicitTool(t, tool, ctx, `{"query":"acoustic","scope_drop":["flow","project","phase"],"filters":[{"key":"phase","op":"=","value":"DRS"}]}`)
	if len(enhancer.fast) != 1 || !reflect.DeepEqual(explicitFilterKeys(enhancer.fast[0]), []string{"project", "phase"}) {
		t.Fatalf("scope_drop resolved %v, want project and the model's phase kept", enhancer.fast)
	}
	if got := chunkDocs(result); !reflect.DeepEqual(got, []string{"doc-b"}) {
		t.Fatalf("chunks from %v, want doc-b", got)
	}
}

// Without filter_args the request scope resolves exactly as before: the
// push-down path is opt-in.
func TestRequestScopeWithoutFilterArgsKeepsItsResolutionPath(t *testing.T) {
	enhancer, _, ctx := newExplicitPipeline(t,
		`{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project","op":"in","value":["Nivy Tower"]}]}`)
	params := explicitToolParams()
	delete(params, "filter_args")
	delete(params, "meta_data_filter")
	params["keywords_similarity_weight"] = 1.0
	params["similarity_threshold"] = 0.0
	result := runExplicitTool(t, buildExplicitTool(t, params), ctx, `{"query":"acoustic","filters":[{"key":"phase","op":"=","value":"DSP"}]}`)
	if got := chunkDocs(result); !reflect.DeepEqual(got, []string{"doc-a", "doc-b"}) {
		t.Fatalf("chunks from %v, want the request scope's two documents", got)
	}
	if len(enhancer.fast) != 0 || len(enhancer.slow) != 1 || result.Diagnostics != nil {
		t.Fatalf("push-down resolutions=%d, standard=%d, diagnostics=%v", len(enhancer.fast), len(enhancer.slow), result.Diagnostics)
	}
}

type metadataValuesOutput struct {
	Key          string               `json:"key"`
	Values       []metadataValueCount `json:"values"`
	TotalValues  int                  `json:"total_values"`
	Truncated    bool                 `json:"truncated"`
	RequestScope []map[string]any     `json:"request_scope"`
	DocsInScope  *int                 `json:"docs_in_scope"`
	Cached       bool                 `json:"cached"`
	Error        string               `json:"_ERROR"`
}

func runValuesTool(t *testing.T, tool *RetrievalTool, ctx context.Context, args string) metadataValuesOutput {
	t.Helper()
	out, err := tool.CompanionTools()[0].(*MetadataValuesTool).InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var result metadataValuesOutput
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return result
}

// Values come with document counts, most frequent first, and repeated calls
// (any key of the tool) are served from the cache.
func TestMetadataValuesToolListsAndCaches(t *testing.T) {
	enhancer, _, ctx := newExplicitPipeline(t, "")
	tool := explicitPipelineTool(t)

	result := runValuesTool(t, tool, ctx, `{"key":"phase"}`)
	want := []metadataValueCount{{Value: "DSP", Docs: 2}, {Value: "DRS", Docs: 1}}
	if !reflect.DeepEqual(result.Values, want) || result.TotalValues != 2 || result.Cached || result.DocsInScope != nil {
		t.Fatalf("result = %+v", result)
	}
	result = runValuesTool(t, tool, ctx, `{"key":"project","contains":"NÍVY","limit":1}`)
	if !reflect.DeepEqual(result.Values, []metadataValueCount{{Value: "Nivy Tower", Docs: 2}}) || !result.Cached {
		t.Fatalf("folded contains on a cached entry = %+v", result)
	}
	result = runValuesTool(t, tool, ctx, `{"key":"project","limit":1}`)
	if len(result.Values) != 1 || result.TotalValues != 2 || !result.Truncated {
		t.Fatalf("limit = %+v", result)
	}
	if enhancer.countCalls != 1 || enhancer.countDocs[0] != nil {
		t.Fatalf("metadata was counted %d times over %v; want once, unscoped", enhancer.countCalls, enhancer.countDocs)
	}

	// Conditions on other keys count inside the matching documents only.
	result = runValuesTool(t, tool, ctx, `{"key":"phase","filters":[{"key":"project","op":"=","value":"Eurovea"}]}`)
	if !reflect.DeepEqual(result.Values, []metadataValueCount{{Value: "DSP", Docs: 1}}) || *result.DocsInScope != 1 || result.Cached {
		t.Fatalf("filtered values = %+v", result)
	}
	if !reflect.DeepEqual(enhancer.countDocs[1], []string{"doc-c"}) {
		t.Fatalf("counted over %v, want doc-c", enhancer.countDocs[1])
	}

	if result = runValuesTool(t, tool, ctx, `{"key":"author"}`); !strings.Contains(result.Error, "available keys: project, phase, ext") {
		t.Fatalf("unlisted key = %+v", result)
	}
	if result = runValuesTool(t, tool, ctx, `{"key":"phase","filters":[{"key":"author","op":"=","value":"x"}]}`); !strings.Contains(result.Error, "not filterable") {
		t.Fatalf("invalid filter = %+v", result)
	}
	if enhancer.countCalls != 2 {
		t.Fatalf("rejected calls counted metadata: %d", enhancer.countCalls)
	}
}

// The request scope bounds the listing like it bounds retrieval, and a scope
// matching nothing lists nothing.
func TestMetadataValuesToolRespectsTheRequestScope(t *testing.T) {
	enhancer, _, ctx := newExplicitPipeline(t,
		`{"method":"manual","dataset_ids":["kb-project"],"manual":[{"key":"project","op":"in","value":["Nivy Tower"]}]}`)
	tool := explicitPipelineTool(t)

	result := runValuesTool(t, tool, ctx, `{"key":"phase"}`)
	want := []metadataValueCount{{Value: "DRS", Docs: 1}, {Value: "DSP", Docs: 1}}
	if !reflect.DeepEqual(result.Values, want) || *result.DocsInScope != 2 || len(result.RequestScope) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(enhancer.countDocs[0], []string{"doc-a", "doc-b"}) {
		t.Fatalf("counted over %v", enhancer.countDocs[0])
	}

	result = runValuesTool(t, tool, ctx, `{"key":"phase","filters":[{"key":"project","op":"=","value":"Eurovea"}]}`)
	if len(result.Values) != 0 || *result.DocsInScope != 0 {
		t.Fatalf("outside the request scope = %+v", result)
	}
	if got := enhancer.countDocs[1]; got == nil || len(got) != 0 {
		t.Fatalf("an empty scope must count over no documents, not all: %v", got)
	}
}

// Dataset authorization runs on every call, also when the answer is cached.
func TestMetadataValuesToolAuthorizesBeforeTheCache(t *testing.T) {
	enhancer, _, ctx := newExplicitPipeline(t, "")
	tool := explicitPipelineTool(t)
	if result := runValuesTool(t, tool, ctx, `{"key":"phase"}`); len(result.Values) != 2 {
		t.Fatalf("owner = %+v", result)
	}

	state := runtime.NewCanvasState("run", "task")
	state.Sys["user_id"] = "stranger"
	_, err := tool.CompanionTools()[0].(*MetadataValuesTool).InvokableRun(runtime.WithState(t.Context(), state), `{"key":"phase"}`)
	if !errors.Is(err, permission.ErrPermissionDenied) {
		t.Fatalf("stranger error = %v, want permission denial", err)
	}
	if enhancer.countCalls != 1 {
		t.Fatalf("metadata counted %d times", enhancer.countCalls)
	}
}

func TestMetadataValuesCache(t *testing.T) {
	now := time.Unix(1000, 0)
	cache := newMetadataValuesCache(5*time.Minute, 2, func() time.Time { return now })
	loads := map[string]int{}
	load := func(key string) func() (*metadataValuesEntry, error) {
		return func() (*metadataValuesEntry, error) {
			loads[key]++
			return &metadataValuesEntry{scopeDocs: loads[key]}, nil
		}
	}

	if _, cached, _ := cache.get("a", load("a")); cached {
		t.Fatal("first read reported cached")
	}
	now = now.Add(4 * time.Minute)
	if entry, cached, _ := cache.get("a", load("a")); !cached || entry.scopeDocs != 1 || loads["a"] != 1 {
		t.Fatalf("within the TTL: cached=%v loads=%d", cached, loads["a"])
	}
	now = now.Add(2 * time.Minute)
	if entry, cached, _ := cache.get("a", load("a")); cached || entry.scopeDocs != 2 {
		t.Fatalf("past the TTL: cached=%v entry=%+v", cached, entry)
	}

	// A failed load is reported and retried, never remembered.
	boom := errors.New("doc store down")
	if _, _, err := cache.get("b", func() (*metadataValuesEntry, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, cached, err := cache.get("b", load("b")); err != nil || cached || loads["b"] != 1 {
		t.Fatalf("after a failure: cached=%v err=%v loads=%d", cached, err, loads["b"])
	}

	// At capacity the entry expiring first makes room.
	now = now.Add(time.Minute)
	cache.get("c", load("c"))
	if len(cache.entries) != 2 {
		t.Fatalf("entries=%d, want the cap of 2", len(cache.entries))
	}
	if _, kept := cache.entries["c"]; !kept {
		t.Fatal("the new entry was not stored")
	}

	first := metadataValuesCacheKey([]string{"kb-2", "kb-1"}, []string{"phase", "project"}, nil)
	if first != metadataValuesCacheKey([]string{"kb-1", "kb-2"}, []string{"project", "phase"}, nil) {
		t.Fatal("the cache key depends on dataset or key order")
	}
	if first == metadataValuesCacheKey([]string{"kb-1", "kb-2"}, []string{"project", "phase"}, map[string]any{"manual": []any{}}) {
		t.Fatal("the cache key ignores the scope")
	}
}

// compact_result returns each chunk once, with its document name, ids, page
// and score, and keeps the scope and diagnostics fields; the references the
// answer cites from are recorded as before.
func TestRetrievalCompactResult(t *testing.T) {
	chunks := []RetrievalChunk{
		{ID: "c1", Content: "alpha text", DocumentID: "d1", DocumentName: "TS-a.pdf", DatasetID: "kb-1", PageNum: 3, Score: 0.87654},
		{ID: "c2", Content: "beta text", DocumentID: "d2", DocumentName: "TS-b.pdf", DatasetID: "kb-1", Score: 0.5},
	}
	useRetrievalService(t, staticRetrievalService{chunks: chunks})
	params := semiAutoToolParams()
	delete(params, "meta_data_filter")
	params["compact_result"] = true
	params["empty_response"] = "Nothing found."
	tool := buildExplicitTool(t, params)

	state := runtime.NewCanvasState("run", "task")
	out, err := tool.InvokableRun(runtime.WithState(t.Context(), state), `{"query":"alpha"}`)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"chunks":[` +
		`{"id":"c1","document_name":"TS-a.pdf","document_id":"d1","dataset_id":"kb-1","page":3,"score":0.877,"content":"alpha text"},` +
		`{"id":"c2","document_name":"TS-b.pdf","document_id":"d2","dataset_id":"kb-1","score":0.5,"content":"beta text"}]}`
	if out != want {
		t.Fatalf("compact result:\n got %s\nwant %s", out, want)
	}
	if strings.Count(out, "alpha text") != 1 {
		t.Fatal("chunk text is repeated")
	}
	if got := state.GetRetrievalChunks(); len(got) != 2 || got[0]["chunk_id"] != "c1" {
		t.Fatalf("references = %v", got)
	}

	useRetrievalService(t, staticRetrievalService{})
	out, err = tool.InvokableRun(t.Context(), `{"query":"alpha"}`)
	if err != nil || out != `{"chunks":[],"message":"Nothing found."}` {
		t.Fatalf("empty compact result = %s, err=%v", out, err)
	}

	params["compact_result"] = "yes"
	if _, err := BuildByName("retrieval", params); err == nil {
		t.Fatal("a non-boolean compact_result was accepted")
	}

	// A PDF chunk's page comes from its first position when the engine
	// stored page_num_int as a list.
	for name, tc := range map[string]struct {
		chunk RetrievalChunk
		want  int
	}{
		"scalar page":            {RetrievalChunk{PageNum: 4, Positions: [][]float64{{9, 1, 2, 3, 4}}}, 4},
		"positions as floats":    {RetrievalChunk{Positions: [][]float64{{7, 1, 2, 3, 4}, {8, 1, 2, 3, 4}}}, 7},
		"positions decoded JSON": {RetrievalChunk{Positions: []any{[]any{float64(12), float64(1)}}}, 12},
		"no positions":           {RetrievalChunk{}, 0},
		"malformed positions":    {RetrievalChunk{Positions: []any{"x"}}, 0},
	} {
		if got := chunkPage(tc.chunk); got != tc.want {
			t.Errorf("%s: page = %d, want %d", name, got, tc.want)
		}
	}
}

// compact_result composes with filter_args: the diagnostics ride along.
func TestRetrievalCompactResultKeepsDiagnostics(t *testing.T) {
	_, _, ctx := newExplicitPipeline(t, "")
	params := explicitToolParams()
	params["keywords_similarity_weight"] = 1.0
	params["similarity_threshold"] = 0.0
	params["compact_result"] = true
	out, err := buildExplicitTool(t, params).InvokableRun(ctx, `{"query":"acoustic","filters":[{"key":"phase","op":"in","value":["DRS"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		FormalizedContent *string        `json:"formalized_content"`
		Chunks            []compactChunk `json:"chunks"`
		Diagnostics       *struct {
			DocsInScope int `json:"docs_in_scope"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.FormalizedContent != nil || len(result.Chunks) != 1 || result.Chunks[0].DocumentName != "doc-b.pdf" || result.Chunks[0].DatasetID != "kb-project" {
		t.Fatalf("compact result = %s", out)
	}
	if result.Diagnostics == nil || result.Diagnostics.DocsInScope != 1 {
		t.Fatalf("diagnostics = %s", out)
	}
}
