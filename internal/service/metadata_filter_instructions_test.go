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

package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderMetaFilterTemplateInstructions(t *testing.T) {
	guidance := "Filter on region only when the question names a place. {% endif %}"
	render := func(opts MetaFilterPromptOptions) string {
		t.Helper()
		out, err := renderMetaFilterTemplate("2026-01-01", `{"region":["eu"]}`, "q", "", opts)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	with := render(MetaFilterPromptOptions{Instructions: guidance})
	if !strings.Contains(with, "- Filtering guidance:\n\"\"\"\n"+guidance+"\n\"\"\"") {
		t.Fatalf("the guidance block must carry the text verbatim:\n%s", with)
	}
	if !strings.Contains(with, "Follow the filtering guidance below") || strings.Contains(with, "and strength") {
		t.Fatal("the rule mentions strength only when soft conditions are allowed")
	}
	if strings.Contains(render(MetaFilterPromptOptions{Instructions: guidance, AllowSoft: true}), "operators and strength") == false {
		t.Fatal("with soft conditions the rule must mention strength")
	}

	without := render(MetaFilterPromptOptions{})
	if strings.Contains(without, "Filtering guidance") || strings.Contains(without, "{%") || strings.Contains(without, "{{") {
		t.Fatalf("no guidance block and no template syntax without instructions:\n%s", without)
	}
}

func TestMetaFilterInstructionsCapped(t *testing.T) {
	if got := MetaFilterInstructions(map[string]interface{}{"instructions": "  use eu  "}); got != "use eu" {
		t.Fatalf("got %q", got)
	}
	if MetaFilterInstructions(nil) != "" || MetaFilterInstructions(map[string]interface{}{"instructions": 7}) != "" {
		t.Fatal("absent or non-string instructions are empty")
	}
	long := MetaFilterInstructions(map[string]interface{}{"instructions": strings.Repeat("é", MetaFilterInstructionsLimit+10)})
	if utf8.RuneCountInString(long) != MetaFilterInstructionsLimit+1 || !strings.HasSuffix(long, "…") {
		t.Fatalf("instructions must be capped at %d characters, got %d", MetaFilterInstructionsLimit, utf8.RuneCountInString(long))
	}
}

func TestInstructionsReachTheLLMCall(t *testing.T) {
	filter := map[string]interface{}{"method": "auto", "instructions": "HR means human_resources"}
	for name, cfg := range map[string]bool{"doc-id path": false, "chunk path": true} {
		model, driver := newMetaFilterModel(`{"logic":"and","conditions":[]}`)
		chunkMeta := activeChunkMeta
		if !cfg {
			chunkMeta = nil
		}
		ApplyMetaDataScope(t.Context(), filter, scopeTestMetadata(), "hr reports", model, nil, nil, chunkMeta)
		if driver.calls != 1 || !strings.Contains(driver.prompt, "HR means human_resources") {
			t.Fatalf("%s: the instructions must reach the prompt (calls=%d)", name, driver.calls)
		}
	}
}
