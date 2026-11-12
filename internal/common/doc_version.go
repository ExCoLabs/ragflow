package common

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var docVersionSuffix = regexp.MustCompile(`\(\d+\)\s*$`)
var docVersionNumber = regexp.MustCompile(`\((\d+)\)(?:\.[^.()]+)?$`)

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

type docCopyIdentity struct {
	directory  string
	name       string
	dataset    string
	hash       string
	content    string
	documentID string
}

func docCopyGroup(chunk map[string]any) docCopyIdentity {
	name, _ := chunk["docnm_kwd"].(string)
	name = strings.ReplaceAll(strings.TrimSpace(name), `\`, "/")
	hash, _ := chunk["document_content_hash"].(string)
	dataset, _ := chunk["kb_id"].(string)
	content, _ := chunk["content_with_weight"].(string)
	content = strings.Join(strings.Fields(content), " ")
	directory, filename := path.Split(name)
	if strings.TrimSpace(hash) != "" && strings.TrimSpace(dataset) != "" && directory != "" && strings.TrimSpace(filename) != "" && content != "" {
		return docCopyIdentity{directory: directory, name: DocBaseName(filename), dataset: dataset, hash: hash, content: content}
	}
	docID, _ := chunk["doc_id"].(string)
	return docCopyIdentity{name: name, documentID: docID}
}

// docVersionRank scores a copy within its verified group: a non-DWG
// document beats a DWG derivative regardless of suffix, otherwise the
// higher "(N)" upload suffix wins.
func docVersionRank(name string) [2]int {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), `\`, "/"))
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

// PreferNewestDocVersions prefers a representative only when the full
// source directory, dataset, document hash and chunk text agree.
// Upload suffixes do not establish revision order or content identity.
// Unverified documents and chunk order are preserved.
func PreferNewestDocVersions(chunks []map[string]any) []map[string]any {
	best := make(map[docCopyIdentity][2]int)
	for _, chunk := range chunks {
		name, _ := chunk["docnm_kwd"].(string)
		base := docCopyGroup(chunk)
		cur := docVersionRank(name)
		if prev, ok := best[base]; !ok || cur[0] > prev[0] || (cur[0] == prev[0] && cur[1] > prev[1]) {
			best[base] = cur
		}
	}
	out := make([]map[string]any, 0, len(chunks))
	for _, chunk := range chunks {
		name, _ := chunk["docnm_kwd"].(string)
		if b := best[docCopyGroup(chunk)]; b == docVersionRank(name) {
			out = append(out, chunk)
		}
	}
	return out
}

// PreferNewestDocVersionIDs keeps chunk IDs whose document wins its
// verified copy group under the same rule as PreferNewestDocVersions, working
// directly on the retrieval field map so callers can prune parallel
// structures. Chunk IDs missing from field are kept.
func PreferNewestDocVersionIDs(ids []string, field map[string]map[string]any) []string {
	best := make(map[docCopyIdentity][2]int)
	for _, id := range ids {
		name, _ := field[id]["docnm_kwd"].(string)
		base := docCopyGroup(field[id])
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
		if b := best[docCopyGroup(chunk)]; b == docVersionRank(name) {
			out = append(out, id)
		}
	}
	return out
}
