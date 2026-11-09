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
	"fmt"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"ragflow/internal/common"
)

// rrfK is the usual reciprocal-rank-fusion constant: rank 0 and rank 5 of
// one list weigh about the same, so agreement between lists decides.
const rrfK = 60

// fusionQueries returns the extra queries query_fusion runs next to the
// agent's own: that query and the user's question, each without the words
// of the metadata filter that was applied.
//
// The agent tends to repeat the scope in its query ("2023_079 Nemocnica BB
// RP …"). Once the filter holds the search to that scope, those words match
// the header of nearly every chunk in it and push the answer out of the top
// results. Duplicates of query and empty or one-word leftovers are dropped.
func fusionQueries(query, userQuestion string, diag *common.MetadataFilterDiagnostic) []string {
	terms := appliedFilterTerms(diag)
	seen := map[string]bool{foldPhrase(query): true}
	var out []string
	for _, q := range []string{stripTerms(query, terms), stripTerms(userQuestion, terms)} {
		key := foldPhrase(q)
		if len(strings.Fields(key)) < 2 || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, q)
	}
	return out
}

func appliedFilterTerms(diag *common.MetadataFilterDiagnostic) map[string]bool {
	terms := map[string]bool{}
	if diag == nil || diag.Status != "applied" {
		return terms
	}
	for _, c := range diag.Conditions {
		values, ok := c["value"].([]interface{})
		if !ok {
			values = []interface{}{c["value"]}
		}
		for _, v := range values {
			for _, t := range strings.Fields(foldPhrase(fmt.Sprint(v))) {
				terms[t] = true
			}
		}
	}
	return terms
}

func stripTerms(q string, terms map[string]bool) string {
	if len(terms) == 0 {
		return strings.TrimSpace(q)
	}
	kept := make([]string, 0, len(strings.Fields(q)))
	for _, w := range strings.Fields(q) {
		if folded := foldPhrase(w); folded == "" || !allIn(strings.Fields(folded), terms) {
			kept = append(kept, w)
		}
	}
	return strings.Trim(strings.Join(kept, " "), " ,;:")
}

func allIn(tokens []string, set map[string]bool) bool {
	for _, t := range tokens {
		if !set[t] {
			return false
		}
	}
	return len(tokens) > 0
}

// foldPhrase lowercases, drops diacritics and keeps letters, digits and "_"
// ("2023_079" is one project code), everything else separating words.
func foldPhrase(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// fuseChunks merges ranked chunk lists by reciprocal rank and keeps topN.
// Byte-identical copies of one file are indexed as separate documents, so
// chunks are also collapsed by content: one slot per distinct text.
func fuseChunks(lists [][]map[string]any, topN int) []map[string]any {
	type entry struct {
		chunk map[string]any
		score float64
		first int
	}
	byKey := map[string]*entry{}
	order := 0
	for _, list := range lists {
		seenInList := map[string]bool{}
		for rank, c := range list {
			key := chunkFusionKey(c)
			if seenInList[key] {
				continue
			}
			seenInList[key] = true
			e, ok := byKey[key]
			if !ok {
				e = &entry{chunk: c, first: order}
				byKey[key] = e
				order++
			}
			e.score += 1.0 / float64(rrfK+rank+1)
		}
	}
	entries := make([]*entry, 0, len(byKey))
	for _, e := range byKey {
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].score != entries[j].score {
			return entries[i].score > entries[j].score
		}
		return entries[i].first < entries[j].first
	})
	if topN > 0 && len(entries) > topN {
		entries = entries[:topN]
	}
	out := make([]map[string]any, len(entries))
	for i, e := range entries {
		out[i] = e.chunk
	}
	return out
}

func chunkFusionKey(c map[string]any) string {
	content := strings.Join(strings.Fields(contentFromMap(c)), " ")
	if content == "" {
		return "id:" + StringFromMap(c, "chunk_id")
	}
	if len(content) > 400 {
		content = content[:400]
	}
	return "content:" + content
}
