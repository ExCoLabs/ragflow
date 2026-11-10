package common

import (
	"fmt"
	"regexp"
	"strings"
)

var docVersionSuffix = regexp.MustCompile(`\(\d+\)\s*$`)
var docVersionNumber = regexp.MustCompile(`\((\d+)\)`)

// DocBaseName normalizes a document name so that re-uploaded copies
// ("file(1).pdf", "file(3).pdf") and DWG-derived markdown
// ("file - titulka.dwg(5).md") group with their source document.
func DocBaseName(name string) string {
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

// docVersionRank scores a copy within its basename group: a non-DWG
// document beats a DWG derivative regardless of suffix, otherwise the
// higher "(N)" upload suffix wins.
func docVersionRank(name string) [2]int {
	version := 0
	if m := docVersionNumber.FindStringSubmatch(name); m != nil {
		fmt.Sscanf(m[1], "%d", &version)
	}
	nonDWG := 1
	if strings.Contains(strings.ToLower(name), ".dwg") {
		nonDWG = 0
	}
	return [2]int{nonDWG, version}
}

// PreferNewestDocVersions drops chunks from older re-uploaded copies and
// DWG-derived files when a newer copy of the same base document was
// retrieved. Chunk order is preserved. Chunks without a docnm_kwd name
// are kept.
func PreferNewestDocVersions(chunks []map[string]any) []map[string]any {
	best := make(map[string][2]int)
	for _, chunk := range chunks {
		name, _ := chunk["docnm_kwd"].(string)
		base := DocBaseName(name)
		cur := docVersionRank(name)
		if prev, ok := best[base]; !ok || cur[0] > prev[0] || (cur[0] == prev[0] && cur[1] > prev[1]) {
			best[base] = cur
		}
	}
	out := make([]map[string]any, 0, len(chunks))
	for _, chunk := range chunks {
		name, _ := chunk["docnm_kwd"].(string)
		if b := best[DocBaseName(name)]; b == docVersionRank(name) {
			out = append(out, chunk)
		}
	}
	return out
}

// PreferNewestDocVersionIDs keeps chunk IDs whose document wins its
// basename group under the same rule as PreferNewestDocVersions, working
// directly on the retrieval field map so callers can prune parallel
// structures. Chunk IDs missing from field are kept.
func PreferNewestDocVersionIDs(ids []string, field map[string]map[string]any) []string {
	best := make(map[string][2]int)
	for _, id := range ids {
		name, _ := field[id]["docnm_kwd"].(string)
		base := DocBaseName(name)
		cur := docVersionRank(name)
		if prev, ok := best[base]; !ok || cur[0] > prev[0] || (cur[0] == prev[0] && cur[1] > prev[1]) {
			best[base] = cur
		}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		chunk, ok := field[id]
		if !ok {
			out = append(out, id)
			continue
		}
		name, _ := chunk["docnm_kwd"].(string)
		if b := best[DocBaseName(name)]; b == docVersionRank(name) {
			out = append(out, id)
		}
	}
	return out
}
