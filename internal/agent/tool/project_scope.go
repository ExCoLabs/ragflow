package tool

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"ragflow/internal/agent/runtime"
)

var projectHintPattern = regexp.MustCompile(`(?i)^\s*(?:projekt|project)\s*:\s*([^,\r\n]*)`)
var projectIDPattern = regexp.MustCompile(`\b[0-9]{4}_[0-9]{3}\b`)

func retrievalProjectID(ctx context.Context, from string, defaults map[string]any) (string, error) {
	if from != "dataset" {
		return "", nil
	}
	method, _ := defaults["method"].(string)
	fields, _ := defaults[method].([]any)
	configured := false
	for _, field := range fields {
		key, _ := field.(string)
		if m, ok := field.(map[string]any); ok {
			key, _ = m["key"].(string)
		}
		if key == "project" || key == "project_id" {
			configured = true
		}
	}
	if !configured {
		return "", nil
	}
	state, err := runtime.GetStateFromContext(ctx)
	if err != nil || state == nil {
		return "", nil
	}
	question, _ := state.Sys["query"].(string)
	hint := projectHintPattern.FindStringSubmatch(question)
	if len(hint) == 0 {
		return "", nil
	}
	ids := projectIDPattern.FindAllString(hint[1], -1)
	if len(ids) != 1 || !strings.HasPrefix(strings.TrimSpace(hint[1]), ids[0]) {
		return "", fmt.Errorf("retrieval: project hint must start with one canonical YYYY_NNN project id")
	}
	return ids[0], nil
}

func (a *NLPRetrievalAdapter) resolveProjectScope(ctx context.Context, req RetrievalRequest, base []string) ([]string, error) {
	if req.ProjectID == "" {
		return base, nil
	}
	if projectIDPattern.FindString(req.ProjectID) != req.ProjectID {
		return nil, fmt.Errorf("retrieval: invalid canonical project id")
	}
	if a.enhancer == nil {
		return nil, fmt.Errorf("retrieval: project scope service is not configured")
	}
	filter := map[string]any{"method": "manual", "logic": "and", "manual": []any{map[string]any{"key": "project_id", "op": "in", "value": req.ProjectID}}}
	ids, err := a.enhancer.FilterDocuments(ctx, filter, "", nil, base, req.DatasetIDs, nil)
	if err != nil {
		return nil, fmt.Errorf("retrieval: resolve project scope: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if len(base) > 0 {
		ids = intersectProjectScope(base, ids)
		if len(ids) == 0 {
			return nil, nil
		}
	}
	return ids, nil
}

func intersectProjectScope(canonical, current []string) []string {
	if current == nil {
		return canonical
	}
	allowed := make(map[string]bool, len(canonical))
	for _, id := range canonical {
		allowed[id] = true
	}
	out := make([]string, 0, len(current))
	for _, id := range current {
		if allowed[id] {
			out = append(out, id)
		}
	}
	return out
}

var docVersionSuffix = regexp.MustCompile(`\(\d+\)\s*$`)

// docBaseName normalizes a document name so that re-uploaded copies
// ("file(1).pdf", "file(3).pdf") and DWG-derived markdown
// ("file - titulka.dwg(5).md") group with their source document.
func docBaseName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	if i := strings.Index(s, ".dwg"); i >= 0 {
		s = s[:i]
		if j := strings.LastIndex(s, " - "); j >= 0 {
			s = s[:j]
		}
	} else if i := strings.LastIndex(s, "."); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(docVersionSuffix.ReplaceAllString(s, ""))
}

// preferNewestDocVersions drops chunks from older re-uploaded copies and
// DWG-derived files when a newer copy of the same base document was
// retrieved. A higher "(N)" suffix wins; a non-DWG document beats a DWG
// derivative regardless of suffix. Chunk order is preserved.
func preferNewestDocVersions(chunks []map[string]any) []map[string]any {
	best := make(map[string][2]int)
	for _, chunk := range chunks {
		name, _ := chunk["docnm_kwd"].(string)
		base := docBaseName(name)
		version := 0
		if m := regexp.MustCompile(`\((\d+)\)`).FindStringSubmatch(name); m != nil {
			fmt.Sscanf(m[1], "%d", &version)
		}
		nonDWG := 1
		if strings.Contains(strings.ToLower(name), ".dwg") {
			nonDWG = 0
		}
		cur := [2]int{nonDWG, version}
		if prev, ok := best[base]; !ok || cur[0] > prev[0] || (cur[0] == prev[0] && cur[1] > prev[1]) {
			best[base] = cur
		}
	}
	out := make([]map[string]any, 0, len(chunks))
	for _, chunk := range chunks {
		name, _ := chunk["docnm_kwd"].(string)
		base := docBaseName(name)
		version := 0
		if m := regexp.MustCompile(`\((\d+)\)`).FindStringSubmatch(name); m != nil {
			fmt.Sscanf(m[1], "%d", &version)
		}
		nonDWG := 1
		if strings.Contains(strings.ToLower(name), ".dwg") {
			nonDWG = 0
		}
		if b := best[base]; b[0] == nonDWG && b[1] == version {
			out = append(out, chunk)
		}
	}
	return out
}

func projectScopedChunks(chunks []map[string]any, ids []string) []map[string]any {
	allowed := make(map[string]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	out := make([]map[string]any, 0, len(chunks))
	for _, chunk := range chunks {
		id, _ := chunk["doc_id"].(string)
		if allowed[id] {
			out = append(out, chunk)
		}
	}
	return out
}
