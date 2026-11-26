package tool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"ragflow/internal/common"
)

func boostToolParams() map[string]any {
	params := explicitToolParams()
	params["keywords_similarity_weight"] = 1.0
	params["similarity_threshold"] = 0.0
	filterArgs := params["filter_args"].(map[string]any)
	filterArgs["keys"] = append(filterArgs["keys"].([]any), "sector")
	return params
}

func TestExplicitBoostsParse(t *testing.T) {
	cfg := buildExplicitTool(t, boostToolParams()).defaults.FilterArgs
	rows, problems := cfg.parseModelBoosts(json.RawMessage(`[
		{"key":"sector","value":["g","h"]},
		{"key":"phase","value":"DSP","weight":0.4},
		{"key":"ext","op":"=","value":["pdf"],"weight":null}]`))
	if problems != nil {
		t.Fatal(problems)
	}
	want := []map[string]any{
		{"key": "sector", "op": "in", "value": []any{"g", "h"}, "weight": common.DefaultMetaBoostWeight},
		{"key": "phase", "op": "in", "value": []any{"DSP"}, "weight": 0.4},
		{"key": "ext", "op": "in", "value": []any{"pdf"}, "weight": common.DefaultMetaBoostWeight},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %v", rows)
	}
	if rows, problems := cfg.parseModelBoosts(json.RawMessage(`"[{\"key\":\"sector\",\"value\":\"g\"}]"`)); problems != nil || len(rows) != 1 {
		t.Fatalf("double-encoded boost: rows %v, problems %v", rows, problems)
	}
	if rows, problems := cfg.parseModelBoosts(nil); len(rows) != 0 || problems != nil {
		t.Fatalf("absent boost: rows %v, problems %v", rows, problems)
	}
}

// A bad boost rejects the call before any search, like a bad filter.
func TestExplicitBoostsRejectInvalidArguments(t *testing.T) {
	tool := buildExplicitTool(t, boostToolParams())
	for name, tc := range map[string]struct{ boost, want string }{
		"unknown key":   {`[{"key":"author","value":["x"]}]`, `boost[0]: key "author" cannot be boosted`},
		"weight > 1":    {`[{"key":"sector","value":["g"],"weight":2}]`, `weight must be a number from 0 to 1`},
		"weight string": {`[{"key":"sector","value":["g"],"weight":"high"}]`, `weight must be a number`},
		"no value":      {`[{"key":"sector"}]`, `value must be a string`},
		"empty value":   {`[{"key":"sector","value":[""]}]`, `non-empty`},
		"range op":      {`[{"key":"sector","op":"contains","value":["g"]}]`, `exact values only`},
		"too many":      {`[{"key":"ext","value":"a"},{"key":"ext","value":"b"},{"key":"ext","value":"c"},{"key":"ext","value":"d"}]`, `at most 3`},
		"not a list":    {`"sector=g"`, `boost: must be a list of {key, value, weight} objects`},
		"scalar items":  {`["sector"]`, `boost[0]: must be a {key, value, weight} object`},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &scopedEmptyRetrievalService{}
			useRetrievalService(t, svc)
			out, err := tool.InvokableRun(t.Context(), `{"query":"alpha","boost":`+tc.boost+`}`)
			if err != nil {
				t.Fatalf("a boost mistake must be tool output, got error %v", err)
			}
			var result retrievalResult
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(result.Error, "Invalid boost, nothing was searched") || !strings.Contains(result.Error, tc.want) || !strings.Contains(result.Error, "Boostable keys: project, phase, ext, sector") {
				t.Fatalf("message = %q, want %q and the keys", result.Error, tc.want)
			}
			if svc.calls != 0 {
				t.Fatalf("searched %d times with an invalid boost", svc.calls)
			}
		})
	}
}

func TestExplicitBoostSchema(t *testing.T) {
	info, err := buildExplicitTool(t, boostToolParams()).Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	schema, err := info.ParamsOneOf.ToJSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(schema)
	var decoded struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	raw = decoded.Properties["boost"]
	var boost struct {
		Type        string `json:"type"`
		Description string `json:"description"`
		Items       struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Type string   `json:"type"`
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &boost); err != nil {
		t.Fatal(err)
	}
	if boost.Type != "array" || !reflect.DeepEqual(boost.Items.Properties["key"].Enum, []string{"project", "phase", "ext", "sector"}) ||
		boost.Items.Properties["weight"].Type != "number" || boost.Items.Properties["value"].Type != "array" {
		t.Fatalf("boost schema = %s", raw)
	}
	for _, want := range []string{"without dropping the others", "Use filters for what the user fixed", "chunk metadata", "At most 3"} {
		if !strings.Contains(boost.Description, want) {
			t.Errorf("boost description lacks %q: %s", want, boost.Description)
		}
	}
}

func TestWithModelBoosts(t *testing.T) {
	row := map[string]any{"key": "sector", "op": "in", "value": []any{"g"}, "weight": 0.15}
	if got := withModelBoosts(nil, []map[string]any{row}); !reflect.DeepEqual(got, map[string]any{
		"method": "disabled", "boost": map[string]any{"method": "manual", "manual": []any{row}},
	}) {
		t.Fatalf("boost-only filter = %v", got)
	}
	if got := withModelBoosts(map[string]any{"method": "manual"}, nil); !reflect.DeepEqual(got, map[string]any{"method": "manual"}) {
		t.Fatalf("no rows changed the filter: %v", got)
	}

	fixedRow := map[string]any{"key": "flow", "op": "=", "value": "out", "weight": 0.2}
	fixed := make([]any, 1, 4)
	fixed[0] = fixedRow
	conditions := []any{map[string]any{"key": "ext", "op": "in", "value": []any{"pdf"}}}
	dsl := map[string]any{"method": "manual", "manual": conditions, "boost": map[string]any{"method": "manual", "manual": fixed, "max_total": 0.5}}
	original, _ := json.Marshal(dsl)
	got := withModelBoosts(dsl, []map[string]any{row})
	want := map[string]any{"method": "manual", "manual": conditions, "boost": map[string]any{"method": "manual", "manual": []any{fixedRow, row}, "max_total": 0.5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged = %v", got)
	}
	if after, _ := json.Marshal(dsl); string(after) != string(original) || fixed[:2][1] != nil {
		t.Fatalf("the DSL filter was modified: %s", after)
	}
}

// End to end on active chunk metadata: the boost reaches the scorer next to
// the DSL's fixed rows, scopes nothing, needs no LLM and is reported back.
func TestExplicitBoostsReachTheScorerWithoutNarrowing(t *testing.T) {
	enhancer, docEngine, ctx := newExplicitPipeline(t, "")
	enhancer.chunkMeta = &common.ChunkMetadataConfig{Enabled: true, Ready: true, Fields: []string{"sector", "flow"}}
	params := boostToolParams()
	fixedRow := map[string]any{"key": "flow", "op": "=", "value": "out", "weight": 0.2}
	params["meta_data_filter"] = map[string]any{"method": "auto", "boost": map[string]any{"method": "manual", "manual": []any{fixedRow}}}
	tool := buildExplicitTool(t, params)

	previousLogger := common.Logger
	core, logs := observer.New(zap.InfoLevel)
	common.Logger = zap.New(core)
	t.Cleanup(func() { common.Logger = previousLogger })

	result := runExplicitTool(t, tool, ctx, `{"query":"acoustic","filters":[{"key":"project","op":"contains","value":["Nivy"]}],"boost":[{"key":"sector","value":["g"]}]}`)
	if got := chunkDocs(result); !reflect.DeepEqual(got, []string{"doc-a", "doc-b"}) {
		t.Fatalf("chunks from %v, want the filtered doc-a and doc-b", got)
	}
	modelRow := map[string]any{"key": "sector", "op": "in", "value": []any{"g"}, "weight": common.DefaultMetaBoostWeight}
	wantBoosts := []common.ChunkMetaBoost{
		{Key: "flow", Op: "=", Value: "out", Weight: 0.2},
		{Key: "sector", Op: "in", Value: []any{"g"}, Weight: common.DefaultMetaBoostWeight},
	}
	last := docEngine.requests[len(docEngine.requests)-1]
	if last.ChunkMeta == nil || !reflect.DeepEqual(last.ChunkMeta.Boosts, wantBoosts) {
		t.Fatalf("scorer boosts = %+v, want %+v", last.ChunkMeta, wantBoosts)
	}
	diag := result.Diagnostics
	if !reflect.DeepEqual(diag.Boosts, []map[string]any{modelRow}) || diag.BoostInactive != "" || diag.BoostIgnoredKeys != nil {
		t.Fatalf("diagnostics = %+v", diag)
	}
	if tool.defaults.MetaDataFilter["boost"].(map[string]any)["manual"].([]any)[0].(map[string]any)["key"] != "flow" ||
		len(tool.defaults.MetaDataFilter["boost"].(map[string]any)["manual"].([]any)) != 1 {
		t.Fatalf("the tool's own boost was modified: %v", tool.defaults.MetaDataFilter)
	}
	var line string
	for _, entry := range logs.All() {
		if strings.HasPrefix(entry.Message, "agent_retrieval_timings") {
			line = entry.Message
		}
	}
	if !strings.Contains(line, ` boosts=[{"key":"sector","op":"in","value":["g"],"weight":0.15}] `) || strings.Contains(line, "boost_inactive") {
		t.Fatalf("timing line = %q", line)
	}

	// Boost alone: the whole dataset is still searched.
	result = runExplicitTool(t, tool, ctx, `{"query":"acoustic","boost":[{"key":"sector","value":["g"]}]}`)
	if got := chunkDocs(result); len(got) != 3 || result.Diagnostics.DocsInScope != nil {
		t.Fatalf("boost-only call: chunks from %v, diagnostics %+v", got, result.Diagnostics)
	}
	scoped := enhancer.scoped[len(enhancer.scoped)-1]
	if method := scoped["method"]; method != "disabled" {
		t.Fatalf("boost-only filter method = %v, want disabled", method)
	}
}

func TestExplicitBoostsReportWhatCouldNotApply(t *testing.T) {
	enhancer, docEngine, ctx := newExplicitPipeline(t, "")
	tool := buildExplicitTool(t, boostToolParams())
	previousLogger := common.Logger
	core, logs := observer.New(zap.InfoLevel)
	common.Logger = zap.New(core)
	t.Cleanup(func() { common.Logger = previousLogger })

	// Not active (nil, or enabled but not backfilled): nothing reaches the
	// scorer, nothing reads metadata, and the model is told.
	for _, cfg := range []*common.ChunkMetadataConfig{nil, {Enabled: true, Fields: []string{"sector"}}} {
		enhancer.chunkMeta = cfg
		result := runExplicitTool(t, tool, ctx, `{"query":"acoustic","boost":[{"key":"sector","value":["g"]}]}`)
		if len(chunkDocs(result)) != 3 || enhancer.scopeCalls != 0 {
			t.Fatalf("cfg %+v: chunks %v, scope calls %d", cfg, chunkDocs(result), enhancer.scopeCalls)
		}
		if last := docEngine.requests[len(docEngine.requests)-1]; last.ChunkMeta != nil {
			t.Fatalf("cfg %+v: scorer got %+v", cfg, last.ChunkMeta)
		}
		if diag := result.Diagnostics; diag.BoostInactive != boostInactiveReason || diag.Boosts != nil {
			t.Fatalf("cfg %+v: diagnostics %+v", cfg, diag)
		}
	}
	last := logs.All()[len(logs.All())-1].Message
	if !strings.Contains(last, " boosts=[] boost_inactive=true ") {
		t.Fatalf("timing line = %q", last)
	}

	// Active, but sector is not copied onto the chunks: phase applies, sector
	// is named.
	enhancer.chunkMeta = &common.ChunkMetadataConfig{Enabled: true, Ready: true, Fields: []string{"phase"}}
	result := runExplicitTool(t, tool, ctx, `{"query":"acoustic","boost":[{"key":"sector","value":["g"]},{"key":"phase","value":["DSP"]},{"key":"sector","value":["h"]}]}`)
	diag := result.Diagnostics
	if !reflect.DeepEqual(diag.BoostIgnoredKeys, []string{"sector"}) || len(diag.Boosts) != 1 || diag.Boosts[0]["key"] != "phase" || diag.BoostInactive != "" {
		t.Fatalf("diagnostics = %+v", diag)
	}
	if boosts := docEngine.requests[len(docEngine.requests)-1].ChunkMeta.Boosts; len(boosts) != 1 || boosts[0].Key != "phase" {
		t.Fatalf("scorer boosts = %+v", boosts)
	}
	if last := logs.All()[len(logs.All())-1].Message; !strings.Contains(last, ` boost_ignored_keys=["sector"] `) {
		t.Fatalf("timing line = %q", last)
	}

	// No boost argument: the diagnostics carry no boost fields at all.
	out, err := tool.InvokableRun(ctx, `{"query":"acoustic"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "boost") {
		t.Fatalf("unboosted call reports boosts: %s", out)
	}
}

func TestCapChunksPerDocumentByBaseName(t *testing.T) {
	chunks := []RetrievalChunk{
		{ID: "a1", DocumentID: "d1", DocumentName: "Projekty/2018_055/SP/X.pdf"},
		{ID: "a2", DocumentID: "d1", DocumentName: "Projekty/2018_055/SP/X.pdf"},
		{ID: "b1", DocumentID: "d2", DocumentName: "X(1).pdf"},
		{ID: "c1", DocumentID: "d3", DocumentName: `Archiv\X(2).pdf`},
		{ID: "d1", DocumentID: "d4", DocumentName: "Projekty/2018_055/SP/X.docx"},
		{ID: "e1", DocumentID: "d5"},
		{ID: "e2", DocumentID: "d5"},
		{ID: "e3", DocumentID: "d5"},
	}
	ids := func(got []RetrievalChunk) []string {
		out := []string{}
		for _, c := range got {
			out = append(out, c.ID)
		}
		return out
	}
	if got := ids(capChunksPerDocument(append([]RetrievalChunk(nil), chunks...), 2, true)); !reflect.DeepEqual(got, []string{"a1", "a2", "d1", "e1", "e2"}) {
		t.Fatalf("by base name: %v", got)
	}
	if got := ids(capChunksPerDocument(append([]RetrievalChunk(nil), chunks...), 2, false)); !reflect.DeepEqual(got, []string{"a1", "a2", "b1", "c1", "d1", "e1", "e2"}) {
		t.Fatalf("default: %v", got)
	}
}
