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

package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"ragflow/internal/dao"
	"regexp"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"ragflow/internal/agent/runtime"
	"ragflow/internal/common"
)

// ErrGraphRAGNotSupported is returned by the Retrieval tool when
// callers pass use_kg=true. GraphRAG support is a future
// enhancement; users must either disable use_kg or fall back to
// the Python Canvas.
var ErrGraphRAGNotSupported = errors.New("GraphRAG 检索暂不支持，请使用 Python Canvas 或关闭 use_kg")

// retrievalToolName preserves the Python typo ("dateset") for backward
// compatibility with existing Canvas DSLs that reference the tool by name.
const retrievalToolName = "search_my_dateset"

const retrievalToolDescription = "This tool can be utilized for relevant content searching in the datasets."

// retrievalArgs is the JSON schema the model sends into InvokableRun. We
// accept both `query` (canonical) and `dataset_ids` / `use_kg` etc. to
// match the Python ToolMeta field set.
type retrievalArgs struct {
	Query                    string         `json:"query"`
	DatasetIDs               []string       `json:"dataset_ids,omitempty"`
	KBIDs                    []string       `json:"kb_ids,omitempty"`
	MemoryIDs                []string       `json:"memory_ids,omitempty"`
	UserID                   string         `json:"user_id,omitempty"`
	TopN                     int            `json:"top_n,omitempty"`
	RerankCandidatesCount    int            `json:"rerank_candidates_count,omitempty"`
	TopK                     int            `json:"top_k,omitempty"`
	KeywordsSimilarityWeight *float64       `json:"keywords_similarity_weight,omitempty"`
	UseKG                    bool           `json:"use_kg,omitempty"`
	SimilarityThreshold      *float64       `json:"similarity_threshold,omitempty"`
	RerankID                 string         `json:"rerank_id,omitempty"`
	CrossLanguages           []string       `json:"cross_languages,omitempty"`
	TOCEnhance               bool           `json:"toc_enhance,omitempty"`
	MetaDataFilter           map[string]any `json:"meta_data_filter,omitempty"`
	// ScopeDrop lets the model explicitly relax declared request-scope keys
	// (see `relax` in the request's meta_data_filter) when strict evidence
	// looks insufficient. Undeclared keys are ignored server-side.
	ScopeDrop []string `json:"scope_drop,omitempty"`
	// Filters are the model's explicit metadata conditions. Kept raw so a
	// malformed value cannot fail argument parsing for tools that never
	// declared filter_args, where it is ignored.
	Filters       json.RawMessage `json:"filters,omitempty"`
	RetrievalFrom string          `json:"retrieval_from,omitempty"`
	EmptyResponse string          `json:"empty_response,omitempty"`
	// FilterArgs is the DSL's filter_args. Never read from model arguments:
	// the allow-list is the tool author's.
	FilterArgs *filterArgsConfig `json:"-"`
	// FunctionName is the agent-visible tool name from the DSL. It is not
	// exposed to the model as part of the tool arguments.
	FunctionName string `json:"-"`
	// Description is the agent-visible tool description from the DSL. It is
	// not exposed to the model as part of the tool arguments.
	Description string `json:"-"`
}

// retrievalResult is the JSON shape returned to the model. The `_ERROR`
// field matches the Python tool's output convention; downstream components
// can pattern-match on it.
type retrievalResult struct {
	FormalizedContent string         `json:"formalized_content"`
	Chunks            []chunkPayload `json:"chunks,omitempty"`
	FiltersRelaxed    bool           `json:"filters_relaxed,omitempty"`
	// ScopeDroppedKeys reports which declared-relaxable request-scope keys
	// were dropped for this call; ScopeRelaxable tells the model which keys
	// it may still drop via scope_drop on a follow-up call.
	ScopeDroppedKeys []string `json:"scope_dropped_keys,omitempty"`
	ScopeRelaxable   []string `json:"scope_relaxable,omitempty"`
	// Diagnostics is set only by explicit-filter tools (filter_args).
	Diagnostics *retrievalDiagnostics `json:"diagnostics,omitempty"`
	Stub        bool                  `json:"stub,omitempty"`
	Error       string                `json:"_ERROR,omitempty"`
}

// chunkPayload is the minimal chunk shape we surface. We don't try to
// match every Python field — the stub returns empty data; the wired
// implementation will populate the real shape.
type chunkPayload struct {
	ID         string  `json:"id,omitempty"`
	Content    string  `json:"content,omitempty"`
	DocumentID string  `json:"document_id,omitempty"`
	Score      float64 `json:"score,omitempty"`
}

// RetrievalTool is the Retrieval tool. It validates the input
// (rejecting use_kg=true with ErrGraphRAGNotSupported) and
// dispatches to the registered RetrievalService via
// SetRetrievalService. When no service is registered, the call
// surfaces ErrRetrievalServiceMissing.
type RetrievalTool struct {
	defaults retrievalArgs
	name     string
	desc     string
}

// NewRetrievalTool returns a RetrievalTool implementing eino's
// tool.InvokableTool interface.
func NewRetrievalTool() *RetrievalTool {
	return NewRetrievalToolWithDefaults(retrievalArgs{})
}

// NewRetrievalToolWithDefaults returns a RetrievalTool with node-level
// defaults from the Agent tool configuration.
func NewRetrievalToolWithDefaults(defaults retrievalArgs) *RetrievalTool {
	if len(defaults.DatasetIDs) == 0 && len(defaults.KBIDs) != 0 {
		defaults.DatasetIDs = append([]string(nil), defaults.KBIDs...)
	}
	name := defaults.FunctionName
	if strings.TrimSpace(name) == "" {
		name = retrievalToolName
	}
	desc := defaults.Description
	if strings.TrimSpace(desc) == "" {
		desc = retrievalToolDescription
	}
	return &RetrievalTool{defaults: defaults, name: name, desc: desc}
}

// Info returns the tool's metadata for the chat model. The schema mirrors
// the Python RetrievalParam ToolMeta (plan, field alignment).
func (r *RetrievalTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	extra := map[string]any{}
	if len(r.defaults.DatasetIDs) > 0 {
		extra["dataset_ids"] = r.defaults.DatasetIDs
	}
	params := map[string]*schema.ParameterInfo{
		"query": {
			Type:     schema.String,
			Desc:     "The keywords to search the dataset. The keywords should be the most important words/terms (including synonyms) from the original request.",
			Required: true,
		},
		"scope_drop": {
			Type: schema.Array,
			Desc: "Drop request-scope filter keys for this call, e.g. [\"phase\"] or [\"flow\",\"phase\"]. If a previous response reported scope_relaxable keys and its documents do not answer the request, call this tool again listing those keys here to broaden the search before concluding the information is missing. Only keys returned as scope_relaxable are accepted; other keys are ignored.",
		},
	}
	if cfg := r.defaults.FilterArgs; cfg != nil {
		params["filters"] = cfg.filtersParam(r.valuesToolName(),
			"Exact metadata conditions restricting which documents are searched; omit to search everything the request allows.")
	}
	return &schema.ToolInfo{
		Name:        r.name,
		Desc:        r.desc,
		Extra:       extra,
		ParamsOneOf: schema.NewParamsOneOfByParams(params),
	}, nil
}

// InvokableRun executes the tool. It validates the input and
// dispatches to the registered RetrievalService. When no
// service is registered, the call surfaces
// ErrRetrievalServiceMissing.
func (r *RetrievalTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args retrievalArgs
	if argumentsInJSON != "" {
		if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
			return "", fmt.Errorf("retrieval: parse arguments: %w", err)
		}
	}
	args = r.mergeDefaults(args)
	resolvedQuery, err := resolveRetrievalQuery(ctx, args.Query)
	if err != nil {
		return "", err
	}
	args.Query = resolvedQuery
	resolvedUserID, err := resolveRetrievalUserID(ctx, args.UserID)
	if err != nil {
		return "", err
	}
	args.UserID = resolvedUserID
	common.Debug("agent retrieval tool: parsed arguments",
		zap.String("query", args.Query),
		zap.Strings("dataset_ids", args.DatasetIDs),
		zap.Int("top_n", args.TopN),
		zap.Int("top_k", args.TopK),
		zap.Float64p("keywords_similarity_weight", args.KeywordsSimilarityWeight),
		zap.Bool("use_kg", args.UseKG),
	)
	if args.Query == "" {
		return stubJSONWithErr(retrievalResult{FormalizedContent: args.EmptyResponse})
	}
	if args.UseKG {
		return stubJSON(retrievalResult{
			Stub:  true,
			Error: ErrGraphRAGNotSupported.Error(),
		}), ErrGraphRAGNotSupported
	}
	if args.RetrievalFrom == "" {
		return stubJSONWithErr(retrievalResult{FormalizedContent: args.EmptyResponse})
	}
	if args.RetrievalFrom != "dataset" && args.RetrievalFrom != "memory" {
		return "", fmt.Errorf("retrieval: unsupported retrieval_from %q", args.RetrievalFrom)
	}
	if args.RetrievalFrom == "dataset" && len(args.DatasetIDs) == 0 {
		return "", fmt.Errorf("retrieval: dataset_ids is required")
	}
	if args.RetrievalFrom == "memory" && len(args.MemoryIDs) == 0 {
		return "", fmt.Errorf("retrieval: memory_ids is required")
	}
	if err := r.validateRequestScopeBinding(ctx, args); err != nil {
		return "", err
	}
	var explicit *runtime.ExplicitFilters
	if cfg := r.defaults.FilterArgs; cfg != nil && args.RetrievalFrom == "dataset" {
		conditions, problems := cfg.parseModelFilters(args.Filters)
		if len(problems) > 0 {
			return stubJSONWithErr(retrievalResult{Error: invalidFiltersMessage(problems, cfg)})
		}
		explicit = &runtime.ExplicitFilters{Conditions: conditions}
		args.MetaDataFilter = withoutLLMMetaFilter(args.MetaDataFilter)
	}
	started := time.Now()
	resolvedDatasetIDs, err := resolveRetrievalDatasetIDs(ctx, args.DatasetIDs)
	if err != nil {
		return "", err
	}
	args.DatasetIDs = resolvedDatasetIDs
	resolvedFilter, err := resolveRetrievalFilter(ctx, args.MetaDataFilter)
	if err != nil {
		return "", err
	}
	args.MetaDataFilter = resolvedFilter

	// Dispatch to the registered RetrievalService. When the
	// default stub is in place, the call surfaces
	// ErrRetrievalServiceMissing; once a real impl is installed
	// via SetRetrievalService (or SetSimpleRetrievalService for
	// dev), the chunks flow through normally.
	//
	// Agentic: no dense-only rescue — a row the query's own words never
	// matched is not evidence the model asked for.
	var diagnostics *common.MetadataFilterDiagnostic
	if args.RetrievalFrom == "dataset" {
		diagnostics = &common.MetadataFilterDiagnostic{}
		if len(args.MetaDataFilter) == 0 {
			diagnostics.Method = "disabled"
			diagnostics.Status = "disabled"
			diagnostics.Logic = "and"
			diagnostics.Conditions = []map[string]interface{}{}
		}
	}
	projectID, err := retrievalProjectID(ctx, args.RetrievalFrom, r.defaults.MetaDataFilter)
	if err != nil {
		return "", err
	}
	searchReq := RetrievalRequest{
		Query:                    args.Query,
		ProjectID:                projectID,
		DatasetIDs:               args.DatasetIDs,
		MemoryIDs:                args.MemoryIDs,
		TopN:                     args.TopN,
		RerankCandidatesCount:    args.RerankCandidatesCount,
		TopK:                     args.TopK,
		KeywordsSimilarityWeight: args.KeywordsSimilarityWeight,
		UseKG:                    args.UseKG,
		SimilarityThreshold:      args.SimilarityThreshold,
		AllowDenseFallback:       new(false),
		RerankID:                 args.RerankID,
		CrossLanguages:           append([]string(nil), args.CrossLanguages...),
		TOCEnhance:               args.TOCEnhance,
		MetaDataFilter:           cloneStringAnyMap(args.MetaDataFilter),
		ScopeDrop:                append([]string(nil), args.ScopeDrop...),
		Explicit:                 explicit,
		RetrievalFrom:            args.RetrievalFrom,
		UserID:                   args.UserID,
		TenantID:                 retrievalTenantID(ctx),
		Diagnostics:              diagnostics,
	}

	var chunks []RetrievalChunk
	if args.RetrievalFrom == "memory" {
		chunks, err = GetMemoryRetrievalService().Search(ctx, dao.DB, searchReq)
	} else {
		chunks, err = GetRetrievalService().Search(ctx, dao.DB, searchReq)
	}
	if err != nil {
		return stubJSON(retrievalResult{
			Stub:  true,
			Error: err.Error(),
		}), err
	}
	// Duplicate document variants ((N).pdf copies of the same file) can
	// crowd top-N on large datasets; cap chunks per normalized document
	// name so distinct documents keep their slots. Rank order is preserved.
	chunks = capChunksPerDocument(chunks, maxChunksPerDocument)
	filtersRelaxed := diagnostics != nil &&
		(diagnostics.RelaxationAttempted || len(diagnostics.ScopeDroppedKeys) > 0)
	common.Debug("agent retrieval tool: search result",
		zap.Int("chunks_count", len(chunks)),
		zap.Bool("filters_relaxed", filtersRelaxed),
	)
	// Map the chunks into the result envelope. The retrievalResult
	// type carries the eino-tool envelope shape (chunkPayload, not
	// RetrievalChunk), so we translate.
	payload := make([]chunkPayload, 0, len(chunks))
	for _, c := range chunks {
		payload = append(payload, chunkPayload{
			ID:         c.ID,
			Content:    c.Content,
			DocumentID: c.DocumentID,
			Score:      c.Score,
		})
	}
	formalizedContent := renderChunks(chunks, args.Query)
	if args.RetrievalFrom == "memory" {
		formalizedContent = renderMemoryChunks(chunks)
	}
	if len(chunks) == 0 {
		formalizedContent = args.EmptyResponse
	}
	out := retrievalResult{FormalizedContent: formalizedContent, Chunks: payload, FiltersRelaxed: filtersRelaxed}
	if explicit != nil {
		out.Diagnostics = r.explicitDiagnostics(ctx, explicit, args.DatasetIDs, diagnostics.ScopeDroppedKeys, len(chunks), time.Since(started))
		if len(explicit.Conditions) > 0 {
			recordExplicitFilter(diagnostics, explicit)
		}
	}
	if scope := runtime.RequestScopeFromContext(ctx); scope != nil {
		var dropped map[string]bool
		if diagnostics != nil {
			out.ScopeDroppedKeys = diagnostics.ScopeDroppedKeys
			dropped = make(map[string]bool, len(diagnostics.ScopeDroppedKeys))
			for _, k := range diagnostics.ScopeDroppedKeys {
				dropped[k] = true
			}
		}
		for _, k := range scope.RelaxableKeys() {
			if !dropped[k] {
				out.ScopeRelaxable = append(out.ScopeRelaxable, k)
			}
		}
	}
	// Record chunks into canvas state so the Agent's post-stream
	// citation grounding call can read them. The recording is
	// best-effort — when the canvas state is not
	// attached (e.g. unit tests), we skip silently.
	if state, sErr := runtime.GetStateFromContext(ctx); sErr == nil && state != nil && args.RetrievalFrom == "dataset" {
		state.SetRetrievalReferences(referenceChunksFromRetrieval(chunks), referenceDocAggsFromRetrieval(chunks))
		if diagnostics != nil {
			diagnostic := map[string]any{
				"method":                 diagnostics.Method,
				"status":                 diagnostics.Status,
				"conditions":             diagnostics.Conditions,
				"logic":                  diagnostics.Logic,
				"matched_document_count": diagnostics.MatchedDocumentCount,
				"tool_name":              retrievalToolName,
				"query":                  args.Query,
				"dataset_ids":            args.DatasetIDs,
			}
			if diagnostics.AppliedOn != "" {
				// A filter on the chunks' metadata fields has no document count.
				diagnostic["applied_on"] = diagnostics.AppliedOn
			}
			if diagnostics.ProjectID != "" {
				diagnostic["project_id"] = diagnostics.ProjectID
			}
			if diagnostics.RelaxationAttempted {
				diagnostic["relaxation_attempted"] = true
			}
			if diagnostics.RelaxationStatus != "" {
				diagnostic["relaxation_status"] = diagnostics.RelaxationStatus
			}
			if len(diagnostics.ScopeDroppedKeys) > 0 {
				diagnostic["scope_dropped_keys"] = diagnostics.ScopeDroppedKeys
			}
			if len(diagnostics.OriginalConditions) > 0 {
				diagnostic["original_conditions"] = diagnostics.OriginalConditions
			}
			state.AppendMetadataFilterDiagnostic(diagnostic)
		}
	}
	result, err := stubJSONWithErr(out)
	if err != nil {
		return "", err
	}
	return result, nil
}

// recordExplicitFilter reports the model's conditions where the references
// show the tool's metadata filter, in place of the LLM filter they replace.
func recordExplicitFilter(diagnostics *common.MetadataFilterDiagnostic, explicit *runtime.ExplicitFilters) {
	diagnostics.Method = "explicit"
	diagnostics.Logic = "and"
	diagnostics.Conditions = explicit.Conditions
	diagnostics.MatchedDocumentCount = explicit.Trace.ScopeDocs
	diagnostics.Status = "applied"
	if explicit.Trace.ScopeDocs == 0 {
		diagnostics.Status = "no_matches"
	}
}

func (r *RetrievalTool) mergeDefaults(args retrievalArgs) retrievalArgs {
	if len(args.DatasetIDs) == 0 && len(args.KBIDs) != 0 {
		args.DatasetIDs = append([]string(nil), args.KBIDs...)
	}
	if len(args.DatasetIDs) == 0 && len(r.defaults.DatasetIDs) != 0 {
		args.DatasetIDs = append([]string(nil), r.defaults.DatasetIDs...)
	}
	if len(args.MemoryIDs) == 0 && len(r.defaults.MemoryIDs) != 0 {
		args.MemoryIDs = append([]string(nil), r.defaults.MemoryIDs...)
	}
	if args.TopN <= 0 {
		args.TopN = r.defaults.TopN
	}
	if args.RerankCandidatesCount <= 0 {
		args.RerankCandidatesCount = r.defaults.RerankCandidatesCount
	}
	if args.TopK <= 0 {
		args.TopK = r.defaults.TopK
	}
	if args.KeywordsSimilarityWeight == nil {
		args.KeywordsSimilarityWeight = r.defaults.KeywordsSimilarityWeight
	}
	if args.SimilarityThreshold == nil {
		args.SimilarityThreshold = r.defaults.SimilarityThreshold
	}
	if args.EmptyResponse == "" {
		args.EmptyResponse = r.defaults.EmptyResponse
	}
	if args.UserID == "" {
		args.UserID = r.defaults.UserID
	}
	if args.RerankID == "" {
		args.RerankID = r.defaults.RerankID
	}
	if len(args.CrossLanguages) == 0 && len(r.defaults.CrossLanguages) != 0 {
		args.CrossLanguages = append([]string(nil), r.defaults.CrossLanguages...)
	}
	if args.MetaDataFilter == nil && r.defaults.MetaDataFilter != nil {
		args.MetaDataFilter = cloneStringAnyMap(r.defaults.MetaDataFilter)
	}
	if args.RetrievalFrom == "" {
		args.RetrievalFrom = r.defaults.RetrievalFrom
	}
	if args.RetrievalFrom == "" && len(args.DatasetIDs) > 0 {
		args.RetrievalFrom = "dataset"
	}
	if args.RetrievalFrom == "" && len(args.MemoryIDs) > 0 {
		args.RetrievalFrom = "memory"
	}
	args.TOCEnhance = args.TOCEnhance || r.defaults.TOCEnhance
	args.UseKG = args.UseKG || r.defaults.UseKG
	return args
}

func cloneStringAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func resolveRetrievalQuery(ctx context.Context, query string) (string, error) {
	state, err := runtime.GetStateFromContext(ctx)
	if err != nil || state == nil {
		return query, nil
	}
	resolved, err := runtime.ResolveTemplateAuto(query, state)
	if err != nil {
		return "", fmt.Errorf("retrieval: resolve query variables: %w", err)
	}
	return resolved, nil
}

// resolveRetrievalUserID resolves the memory user_id filter. Mirrors Python's
// Retrieval._retrieve_memory: a variable reference — `{sys.user_id}` template
// or bare `sys.*` / `env.*` / `component@param` form — is looked up in the
// canvas state; anything else is a literal user id and passes through.
//
// Two deliberate divergences from Python, which resolves only the fully
// braced form and passes bare refs through literally (agent/tools/retrieval.py
// `_retrieve_memory`):
//
//   - bare ref forms are resolved too, so a canvas storing `sys.user_id`
//     unbraced still filters per user; as a corollary, a literal user id
//     that collides with a live state variable name is substituted (RAGFlow
//     user ids are UUID-like and never look like refs);
//   - an unset bare `sys.*` / `env.*` ref resolves to "" (no user filter)
//     instead of the literal ref string, which could never match a real user
//     id and would silently empty the result. Bare `component@param` refs
//     keep the literal fallback because an "@" in the value is ambiguous
//     with an email-style literal user id.
func resolveRetrievalUserID(ctx context.Context, userID string) (string, error) {
	trimmed := strings.TrimSpace(userID)
	if trimmed == "" {
		return "", nil
	}
	state, err := runtime.GetStateFromContext(ctx)
	if err != nil || state == nil {
		return trimmed, nil
	}
	if strings.ContainsAny(trimmed, "{}") {
		resolved, err := runtime.ResolveTemplateAuto(trimmed, state)
		if err != nil {
			return "", fmt.Errorf("retrieval: resolve user_id variable: %w", err)
		}
		return strings.TrimSpace(resolved), nil
	}
	if value, getErr := state.GetVar(trimmed); getErr == nil && value != nil {
		if text, ok := value.(string); ok {
			return text, nil
		}
		return fmt.Sprintf("%v", value), nil
	}
	if strings.HasPrefix(trimmed, "sys.") || strings.HasPrefix(trimmed, "env.") {
		// An unset sys./env. ref cannot be a literal user id; treat it as
		// "no user filter" per the RetrievalRequest.UserID contract.
		return "", nil
	}
	return trimmed, nil
}

func resolveRetrievalDatasetIDs(ctx context.Context, datasetIDs []string) ([]string, error) {
	state, err := runtime.GetStateFromContext(ctx)
	if err != nil || state == nil {
		return compactStrings(datasetIDs), nil
	}
	resolved := make([]string, 0, len(datasetIDs))
	for _, datasetID := range datasetIDs {
		if !strings.Contains(datasetID, "@") {
			resolved = append(resolved, datasetID)
			continue
		}
		value, getErr := state.GetVar(datasetID)
		if getErr != nil {
			return nil, fmt.Errorf("retrieval: resolve dataset variable %q: %w", datasetID, getErr)
		}
		if value == nil {
			return nil, fmt.Errorf("retrieval: dataset variable %q is empty", datasetID)
		}
		switch typed := value.(type) {
		case string:
			resolved = append(resolved, typed)
		case []string:
			resolved = append(resolved, typed...)
		case []any:
			for _, item := range typed {
				text, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("retrieval: dataset variable %q contains non-string value", datasetID)
				}
				resolved = append(resolved, text)
			}
		default:
			return nil, fmt.Errorf("retrieval: dataset variable %q must be a string or string list", datasetID)
		}
	}
	return compactStrings(resolved), nil
}

func resolveRetrievalFilter(ctx context.Context, filter map[string]any) (map[string]any, error) {
	if filter == nil {
		return nil, nil
	}
	state, err := runtime.GetStateFromContext(ctx)
	if err != nil || state == nil {
		return cloneStringAnyMap(filter), nil
	}
	resolved, err := resolveRetrievalValue(filter, state)
	if err != nil {
		return nil, err
	}
	result, ok := resolved.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("retrieval: metadata filter must be an object")
	}
	return result, nil
}

func resolveRetrievalValue(value any, state *runtime.CanvasState) (any, error) {
	switch typed := value.(type) {
	case string:
		resolved, err := runtime.ResolveTemplateAuto(typed, state)
		if err != nil {
			return nil, fmt.Errorf("retrieval: resolve metadata filter value: %w", err)
		}
		return resolved, nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			resolved, err := resolveRetrievalValue(item, state)
			if err != nil {
				return nil, err
			}
			result[key] = resolved
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			resolved, err := resolveRetrievalValue(item, state)
			if err != nil {
				return nil, err
			}
			result[index] = resolved
		}
		return result, nil
	default:
		return value, nil
	}
}

// renderChunks concatenates the retrieved chunks into a human-
// readable content string. Mirrors Python's
// `kb_prompt(kbinfos, ...)` format: each chunk gets a header
// line with its ID and document, then the content.
func renderChunks(chunks []RetrievalChunk, query string) string {
	var sb strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&sb, "[ID:%s] %s\n", c.ID, c.Content)
	}
	return sb.String()
}

func renderMemoryChunks(chunks []RetrievalChunk) string {
	var builder strings.Builder
	for index, chunk := range chunks {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(chunk.Content)
	}
	return builder.String()
}

func retrievalTenantID(ctx context.Context) string {
	state, err := runtime.GetStateFromContext(ctx)
	if err != nil || state == nil {
		return ""
	}
	if tenantID, _ := state.Sys["tenant_id"].(string); tenantID != "" {
		return tenantID
	}
	userID, _ := state.Sys["user_id"].(string)
	return userID
}

func referenceChunksFromRetrieval(chunks []RetrievalChunk) []map[string]any {
	out := make([]map[string]any, 0, len(chunks))
	for idx, c := range chunks {
		id := c.ID
		if id == "" {
			id = fmt.Sprint(idx)
		}
		chunk := map[string]any{
			"id":                  id,
			"chunk_id":            c.ID,
			"content":             c.Content,
			"content_with_weight": c.Content,
			"document_id":         c.DocumentID,
			"doc_id":              c.DocumentID,
			"document_name":       c.DocumentName,
			"docnm_kwd":           c.DocumentName,
			"dataset_id":          c.DatasetID,
			"kb_id":               c.DatasetID,
			"image_id":            c.ImageID,
			"img_id":              c.ImageID,
			"similarity":          c.Score,
			"term_similarity":     c.TermSimilarity,
			"vector_similarity":   c.VectorSimilarity,
		}
		if c.URL != "" {
			chunk["url"] = c.URL
			chunk["document_url"] = c.URL
		}
		if c.Positions != nil {
			chunk["positions"] = c.Positions
			chunk["position_int"] = c.Positions
		}
		out = append(out, chunk)
	}
	return out
}

func referenceDocAggsFromRetrieval(chunks []RetrievalChunk) []map[string]any {
	byDocID := make(map[string]map[string]any)
	order := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if c.DocumentID == "" && c.DocumentName == "" {
			continue
		}
		key := c.DocumentID
		if key == "" {
			key = c.DocumentName
		}
		agg, exists := byDocID[key]
		if !exists {
			agg = map[string]any{
				"count":      0,
				"doc_id":     c.DocumentID,
				"doc_name":   c.DocumentName,
				"dataset_id": c.DatasetID,
				"kb_id":      c.DatasetID,
			}
			if c.URL != "" {
				agg["url"] = c.URL
			}
			byDocID[key] = agg
			order = append(order, key)
		}
		agg["count"] = agg["count"].(int) + 1
	}

	out := make([]map[string]any, 0, len(order))
	for _, key := range order {
		out = append(out, byDocID[key])
	}
	return out
}

// stubJSONWithErr is the (string, error) variant for call sites
// that need to propagate marshal failures.
func stubJSONWithErr(r retrievalResult) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("retrieval: marshal result: %w", err)
	}
	return string(b), nil
}

// stubJSON marshals the result and returns it as a string. Marshaling
// failures are converted to a plain string error so the model can still
// surface something to the user.
func stubJSON(r retrievalResult) string {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Sprintf(`{"_ERROR":"retrieval: marshal stub result: %s","stub":true}`, err)
	}
	return string(b)
}

// maxChunksPerDocument bounds how many top-ranked chunks a single
// (normalized) document may occupy in one retrieval result.
const maxChunksPerDocument = 2

var dupSuffixRe = regexp.MustCompile(`\(\d+\)(\.[A-Za-z0-9]+)?$`)

// capChunksPerDocument keeps at most max chunks per logical document,
// identified by normalized document name (duplicate "(N)" file variants
// count as one) with document_id as fallback when no name is present.
func capChunksPerDocument(chunks []RetrievalChunk, max int) []RetrievalChunk {
	if max <= 0 {
		return chunks
	}
	seen := map[string]int{}
	out := chunks[:0]
	for _, c := range chunks {
		key := c.DocumentName
		if key == "" {
			key = c.DocumentID
		}
		key = dupSuffixRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(key)), "$1")
		if seen[key] >= max {
			continue
		}
		seen[key]++
		out = append(out, c)
	}
	return out
}
