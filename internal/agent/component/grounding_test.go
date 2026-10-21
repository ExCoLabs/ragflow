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

// Citation-grounding tests. The Agent's post-stream grounding
// call reads chunks from state.Retrieval and makes a second LLM
// call to insert [ID:N] tags. The tests inject canned agentRunner
// + ChatInvoker to verify the call shape and the resulting
// outputs["content"] / outputs["grounding_status"].

package component

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"

	"ragflow/internal/agent/canvas"
	"ragflow/internal/agent/component/prompts"
	"ragflow/internal/agent/runtime"
)

// groundingTestInvoker records the chat request and returns a
// canned response. The LLM call shape (system + user messages)
// is the only thing the grounding path cares about, so we record
// the request verbatim and return a fixed string.
type groundingTestInvoker struct {
	mu        sync.Mutex
	lastReq   ChatInvokeRequest
	responses []string
	calls     int
	failAt    int
}

type groundingStreamingInvoker struct {
	groundingTestInvoker
	streamCalls int
}

func (g *groundingStreamingInvoker) Stream(ctx context.Context, db *gorm.DB, req ChatInvokeRequest, onDelta func(string, bool) error) (*ChatInvokeResponse, error) {
	g.streamCalls++
	resp, err := g.Invoke(ctx, db, req)
	if err != nil {
		return nil, err
	}
	mid := len(resp.Content) / 2
	for _, delta := range []string{resp.Content[:mid], resp.Content[mid:]} {
		if err := onDelta(delta, false); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// groundingPartialFailureInvoker streams a visible grounding delta and
// then returns an error, modelling a mid-stream grounding failure.
type groundingPartialFailureInvoker struct{}

func (groundingPartialFailureInvoker) Stream(_ context.Context, _ *gorm.DB, _ ChatInvokeRequest, onDelta func(string, bool) error) (*ChatInvokeResponse, error) {
	_ = onDelta("partial ", false)
	return nil, errors.New("grounding disconnected")
}

func (groundingPartialFailureInvoker) Invoke(_ context.Context, _ *gorm.DB, _ ChatInvokeRequest) (*ChatInvokeResponse, error) {
	return nil, errors.New("grounding disconnected")
}

func (g *groundingTestInvoker) Invoke(_ context.Context, _ *gorm.DB, req ChatInvokeRequest) (*ChatInvokeResponse, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastReq = req
	g.calls++
	if g.failAt == g.calls {
		return nil, errors.New("grounding provider failed")
	}
	if len(g.responses) == 0 {
		return &ChatInvokeResponse{Content: "grounded"}, nil
	}
	idx := g.calls - 1
	if idx >= len(g.responses) {
		idx = len(g.responses) - 1
	}
	return &ChatInvokeResponse{Content: g.responses[idx]}, nil
}

// TestGrounding_Applied: Cite=true + state has chunks → second
// LLM call is made and the grounded content replaces the original.
func TestGrounding_Applied(t *testing.T) {
	inv := &groundingStreamingInvoker{groundingTestInvoker: groundingTestInvoker{responses: []string{"original answer [ID:0]"}}}
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(inv)
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "original answer"}, nil
	})

	state := canvas.NewCanvasState("r1", "t1")
	state.SetRetrievalChunks([]map[string]any{
		{"id": "0", "content": "the source content"},
	})
	ctx := runtime.WithState(t.Context(), state)
	var streamed string
	var deltas int
	ctx = runtime.WithAgentMessageEmitter(ctx, func(content, _ string) {
		streamed += content
		deltas++
	})

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, want := out["content"], "original answer [ID:0]"; got != want {
		t.Errorf("content=%v, want %v", got, want)
	}
	if got := out["grounding_status"]; got != "applied" {
		t.Errorf("grounding_status=%v, want 'applied'", got)
	}
	if inv.calls != 1 {
		t.Errorf("expected 1 chat call, got %d", inv.calls)
	}
	// Grounded deltas are buffered: the client sees only the final
	// emission of the grounded answer, once.
	if inv.streamCalls != 1 || deltas != 1 || streamed != "original answer [ID:0]" {
		t.Errorf("stream calls=%d, deltas=%d, streamed=%q, want 1 / 1 / original answer [ID:0]", inv.streamCalls, deltas, streamed)
	}
	// System message should contain the citation prompt + sources block.
	if got := inv.lastReq.Messages[0].Role; got != schema.System {
		t.Errorf("first message role=%v, want System", got)
	}
	if !contains(inv.lastReq.Messages[0].Content, "ID: 0") {
		t.Errorf("system prompt missing source block: %q", inv.lastReq.Messages[0].Content)
	}
	if !strings.Contains(inv.lastReq.Messages[0].Content, "Return that same answer with citation insertions only.") {
		t.Error("grounding prompt must prohibit answer rewriting")
	}
}

// TestGrounding_NoChunks: Cite=true but state has no chunks → no
// grounding call, status reflects "no_chunks".
func TestGrounding_NoChunks(t *testing.T) {
	inv := &groundingTestInvoker{}
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(inv)
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "answer"}, nil
	})

	state := canvas.NewCanvasState("r1", "t1")
	// No SetRetrievalChunks — state has no chunks recorded.
	ctx := runtime.WithState(t.Context(), state)

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, want := out["content"], "answer"; got != want {
		t.Errorf("content=%v, want %v (no grounding should be applied)", got, want)
	}
	if got := out["grounding_status"]; got != "no_chunks" {
		t.Errorf("grounding_status=%v, want 'no_chunks'", got)
	}
	if inv.calls != 0 {
		t.Errorf("expected 0 chat calls, got %d", inv.calls)
	}
}

// TestGrounding_CiteFalse: Cite=false → no grounding call, no
// grounding_status key.
func TestGrounding_CiteFalse(t *testing.T) {
	inv := &groundingTestInvoker{}
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(inv)
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "answer"}, nil
	})

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: false})
	out, err := c.Invoke(t.Context(), nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, ok := out["grounding_status"]; ok {
		t.Errorf("grounding_status should be absent when Cite=false, got %v", out["grounding_status"])
	}
	if inv.calls != 0 {
		t.Errorf("expected 0 chat calls, got %d", inv.calls)
	}
}

// TestGrounding_LLMError: grounding LLM call fails → original
// content is preserved, status reflects the error.
func TestGrounding_LLMError(t *testing.T) {
	errInv := &errInvoker{err: errors.New("llm down")}
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(errInv)
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "original"}, nil
	})

	state := canvas.NewCanvasState("r1", "t1")
	state.SetRetrievalChunks([]map[string]any{{"id": "0", "content": "x"}})
	ctx := runtime.WithState(t.Context(), state)

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, want := out["content"], "original"; got != want {
		t.Errorf("content=%v, want %v (original preserved on grounding failure)", got, want)
	}
	got, _ := out["grounding_status"].(string)
	if got == "" || got[:6] != "error:" {
		t.Errorf("grounding_status=%q, want 'error: ...'", got)
	}
}

// TestGrounding_EmptyContent: grounding LLM returns empty
// content → original is preserved.
func TestGrounding_EmptyContent(t *testing.T) {
	inv := &groundingTestInvoker{responses: []string{""}}
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(inv)
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "original"}, nil
	})

	state := canvas.NewCanvasState("r1", "t1")
	state.SetRetrievalChunks([]map[string]any{{"id": "0", "content": "x"}})
	ctx := runtime.WithState(t.Context(), state)

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, want := out["content"], "original"; got != want {
		t.Errorf("content=%v, want %v (empty grounding should preserve original)", got, want)
	}
}

// TestGrounding_HeldContentEmittedOnce: when cite=true holds answer
// deltas during the ReAct stream, the grounded final answer still reaches
// the client and is not duplicated at the end.
func TestGrounding_HeldContentEmittedOnce(t *testing.T) {
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(&groundingStreamingInvoker{groundingTestInvoker: groundingTestInvoker{responses: []string{"answer without citations [ID:0]"}}})
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{
			Role:    schema.Assistant,
			Content: "answer without citations",
		}, nil
	})

	state := canvas.NewCanvasState("r1", "t1")
	state.SetRetrievalChunks([]map[string]any{{"id": "0", "content": "x"}})
	ctx := runtime.WithState(t.Context(), state)
	var contentEvents []string
	ctx = runtime.WithAgentMessageEmitter(ctx, func(content, _ string) {
		if content != "" {
			contentEvents = append(contentEvents, content)
		}
	})

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got, want := out["content"], "answer without citations [ID:0]"; got != want {
		t.Errorf("content=%v, want %v", got, want)
	}
	joined := strings.Join(contentEvents, "")
	if joined != "answer without citations [ID:0]" {
		t.Errorf("content events=%v, joined=%q, want answer without citations [ID:0]", contentEvents, joined)
	}
}

// TestGrounding_PartialStreamFailure_FallsBack: grounding deltas are
// buffered, so a mid-stream failure leaks nothing to the client — the
// invocation succeeds with the original answer and an error status.
func TestGrounding_PartialStreamFailure_FallsBack(t *testing.T) {
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(groundingPartialFailureInvoker{})
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "original"}, nil
	})

	state := canvas.NewCanvasState("r1", "t1")
	state.SetRetrievalChunks([]map[string]any{{"id": "0", "content": "x"}})
	ctx := runtime.WithState(t.Context(), state)
	var emitted []string
	ctx = runtime.WithAgentMessageEmitter(ctx, func(content, _ string) {
		if content != "" {
			emitted = append(emitted, content)
		}
	})

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := out["content"]; got != "original" {
		t.Errorf("content=%v, want original answer on grounding failure", got)
	}
	got, _ := out["grounding_status"].(string)
	if !strings.HasPrefix(got, "error:") {
		t.Errorf("grounding_status=%q, want 'error: ...'", got)
	}
	if len(emitted) != 1 || emitted[0] != "original" {
		t.Errorf("emitted=%v, want exactly one emission of the original answer", emitted)
	}
}

// TestGrounding_BatchedChunks: a chunk set larger than one batch budget
// is grounded through multiple sequential calls, each feeding its output
// to the next.
func TestGrounding_BatchedChunks(t *testing.T) {
	inv := &groundingStreamingInvoker{groundingTestInvoker: groundingTestInvoker{responses: []string{"original [ID:0]", "original [ID:0][ID:13]", "original [ID:0][ID:13][ID:26]"}}}
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(inv)
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "original"}, nil
	})

	// 30 chunks x 3000 chars exceeds the 40k batch budget => >=3 batches.
	var chunks []map[string]any
	for i := 0; i < 30; i++ {
		chunks = append(chunks, map[string]any{"id": fmt.Sprintf("%d", i), "content": strings.Repeat("x", 3100)})
	}
	state := canvas.NewCanvasState("r1", "t1")
	state.SetRetrievalChunks(chunks)
	ctx := runtime.WithState(t.Context(), state)

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if inv.calls < 2 {
		t.Fatalf("expected multiple grounding calls, got %d", inv.calls)
	}
	if got := out["content"]; got != "original [ID:0][ID:13][ID:26]" {
		t.Errorf("content=%v, want original answer with citations from all batches", got)
	}
	if got := out["grounding_status"]; got != "applied" {
		t.Errorf("grounding_status=%v, want 'applied'", got)
	}
}

func TestCitationGroundingAnchorsMatchCanonicalPrefixes(t *testing.T) {
	for _, text := range []string{
		"Prvá veta [ID:0].\nDruhá veta [ID:1].",
		"a[ID:0] [ID:1]b", "a[ID:0]\u00a0[ID:1]b", "a \n [ID:0]b [ID:1]",
		"甲 乙 [ID:0]。 丙 [ID:1]", "[ID:0]\t text [ID: 1 ] [ID:0]",
	} {
		want := make(map[string]int)
		for _, span := range citationGroundingMarkerRe.FindAllStringIndex(text, -1) {
			id := citationGroundingIDRe.FindStringSubmatch(text[span[0]:span[1]])[1]
			want[fmt.Sprintf("%d:%s", len(citationGroundingText(text[:span[0]])), id)]++
		}
		got := citationGroundingAnchors(text, map[string]bool{"0": true, "1": true})
		if len(got) != len(want) {
			t.Fatalf("anchors=%v want=%v", got, want)
		}
		for anchor, count := range want {
			if got[anchor] != count {
				t.Fatalf("anchors=%v want=%v", got, want)
			}
		}
	}
}

func TestGroundingDoesNotReuseUnseenPriorSourceForNewClaims(t *testing.T) {
	validation := validateCitationGrounding("Prvá veta [ID:1]. Druhá veta.", "Prvá veta [ID:1]. Druhá veta [ID:1][ID:0].",
		[]prompts.CitationSource{{ID: "0", Content: "Current source"}}, map[string]bool{"0": true, "1": true})
	if validation.Reason != "source_not_in_batch" || validation.AddedCitations != 0 {
		t.Fatalf("validation=%+v", validation)
	}
}

func TestFinalAnswerCitationValidation(t *testing.T) {
	refs := []map[string]any{{"id": "9f3f90ff9766dcff", "chunk_id": "9f3f90ff9766dcff", "content": "Source"}, {"id": "0", "content": ""}, {"chunk_id": "alias", "content": "Source"}}
	for _, tc := range []struct {
		name, input, want, status        string
		noRefs                           bool
		removed, valid, literal, pending int
	}{
		{"observed_typo", "Áno [ID:9f3f90ff9766dcff][ID:9f3f90ff90ff9766dcff].", "Áno [ID:9f3f90ff9766dcff].", "invalid_removed", false, 1, 1, 0, 0},
		{"valid_alias_and_empty_content", "Text [ID:alias][ID:0].", "Text [ID:alias][ID:0].", "valid", false, 0, 2, 0, 0},
		{"fullwidth", "Text 【ID:0】 【ID:bad】.", "Text 【ID:0】.", "invalid_removed", false, 1, 1, 0, 0},
		{"merged", "Text [ID:0, ID:alias].", "Text [ID:0, ID:alias].", "unresolved_markers", false, 0, 0, 0, 1},
		{"no_refs", "Text [ID:0].", "Text.", "no_references", true, 1, 0, 0, 0},
		{"no_citations", "Rok [2026], dokument ID:abc.", "Rok [2026], dokument ID:abc.", "no_citations", true, 0, 0, 0, 0},
		{"partial_marker", "Text [ID:unfinished", "Text [ID:unfinished", "unresolved_markers", false, 0, 0, 0, 1},
		{"partial_multiline", "Text [ID:unfinished\n\nDôležitý fakt [ID:0].", "Text [ID:unfinished\n\nDôležitý fakt [ID:0].", "unresolved_markers", false, 0, 1, 0, 1},
		{"partial_nested", "Text [ID:unfinished dôležitý fakt [ID:0].", "Text [ID:unfinished dôležitý fakt [ID:0].", "unresolved_markers", false, 0, 1, 0, 1},
		{"ambiguous_text", "Text [ID:Sem patrí dôležitá poznámka].", "Text [ID:Sem patrí dôležitá poznámka].", "unresolved_markers", false, 0, 0, 0, 1},
		{"inline_code", "Ukážka `[ID:bad]`; text [ID:bad].", "Ukážka `[ID:bad]`; text.", "invalid_removed", false, 1, 0, 1, 0},
		{"fenced_code", "```text\n[ID:bad]\n```\nText [ID:bad].", "```text\n[ID:bad]\n```\nText.", "invalid_removed", false, 1, 0, 1, 0},
		{"link_destination", "[Súbor](https://example.test/a[ID:bad]) text [ID:bad].", "[Súbor](https://example.test/a[ID:bad]) text.", "invalid_removed", false, 1, 0, 1, 0},
		{"bare_url", "https://example.test/a[ID:bad] text [ID:bad].", "https://example.test/a[ID:bad] text.", "invalid_removed", false, 1, 0, 1, 0},
		{"escaped_literal", "Ukážka \\[ID:bad]; text [ID:bad].", "Ukážka \\[ID:bad]; text.", "invalid_removed", false, 1, 0, 1, 0},
		{"bold", "**Áno [ID:bad]** [ID:0].", "**Áno** [ID:0].", "invalid_removed", false, 1, 1, 0, 0},
		{"opening_emphasis", "Text [ID:bad]*ďalej* [ID:0].", "Text *ďalej* [ID:0].", "invalid_removed", false, 1, 1, 0, 0},
		{"word_boundary", "word [ID:bad]next", "word next", "invalid_removed", false, 1, 0, 0, 0},
		{"mismatched_brackets", "Text [ID:0】.", "Text [ID:0】.", "unresolved_markers", false, 0, 0, 0, 1},
		{"image_payload", "![graf](https://example.test/image.png) [ID:bad]", "![graf](https://example.test/image.png) ", "invalid_removed", false, 1, 0, 0, 0},
		{"bold_marker_only", "**[ID:bad]**", "****", "empty_after_cleanup", false, 1, 0, 0, 0},
		{"heading_marker_only", "## [ID:bad]", "## ", "empty_after_cleanup", false, 1, 0, 0, 0},
		{"marker_only", "[ID:bad]", "", "empty_after_cleanup", false, 1, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sources := refs
			if tc.noRefs {
				sources = nil
			}
			got := validateFinalAnswerCitations(tc.input, sources)
			if got.Content != tc.want || got.Status != tc.status || got.RemovedCount != tc.removed || got.ValidCount != tc.valid || got.LiteralCount != tc.literal || got.UnresolvedCount != tc.pending {
				t.Fatalf("validation=%+v, want content=%q status=%s removed=%d valid=%d literal=%d unresolved=%d", got, tc.want, tc.status, tc.removed, tc.valid, tc.literal, tc.pending)
			}
			if again := validateFinalAnswerCitations(got.Content, sources); again.Content != got.Content || again.RemovedCount != 0 {
				t.Fatal("final cleanup must be idempotent")
			}
		})
	}
}

func TestFinalCitationsDoNotInferPositionalIDs(t *testing.T) {
	got := validateFinalAnswerCitations("Áno [ID:0].", []map[string]any{{"id": "9f3f90ff9766dcff", "content": "Source"}})
	if got.Content != "Áno." || got.RemovedCount != 1 || got.ValidCount != 0 {
		t.Fatalf("a source at position zero must not invent source ID 0: %+v", got)
	}
}

func TestFinalCitationCleanupAfterGroundingFailure(t *testing.T) {
	original := "Áno [ID:9f3f90ff9766dcff][ID:9f3f90ff90ff9766dcff]."
	for _, providerError := range []bool{false, true} {
		previous := getDefaultChatInvoker()
		if providerError {
			SetDefaultChatInvoker(&errInvoker{err: errors.New("provider failed")})
		} else {
			SetDefaultChatInvoker(&groundingStreamingInvoker{groundingTestInvoker: groundingTestInvoker{responses: []string{"A rewritten answer [ID:9f3f90ff9766dcff]."}}})
		}
		withAgentRunner(t, func(context.Context, AgentParam) (*schema.Message, error) {
			return schema.AssistantMessage(original, nil), nil
		})
		state := canvas.NewCanvasState("run", "session")
		state.SetRetrievalChunks([]map[string]any{{"id": "9f3f90ff9766dcff", "content": "Source"}})
		var emitted []string
		ctx := runtime.WithAgentMessageEmitter(runtime.WithState(t.Context(), state), func(content, _ string) {
			if content != "" {
				emitted = append(emitted, content)
			}
		})
		out, err := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true}).Invoke(ctx, nil, map[string]any{"user_prompt": "question"})
		SetDefaultChatInvoker(previous)
		if err != nil {
			t.Fatal(err)
		}
		want := "Áno [ID:9f3f90ff9766dcff]."
		if out["content"] != want || out["citation_validation_status"] != "invalid_removed" || out["removed_citation_count"] != 1 || out["final_valid_citation_count"] != 1 || out["final_citation_count"] != 1 || out["citation_count_before_validation"] != 2 || out["unresolved_citation_count"] != 0 || len(emitted) != 1 || emitted[0] != want {
			t.Fatalf("out=%v emitted=%q", out, emitted)
		}
	}
}

func TestFinalCitationEmissionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, original, want, status      string
		cite, streamed, noRefs, wantError bool
		removed, unresolved               int
	}{
		{"no_refs", "Text [ID:0].", "Text.", "no_references", true, false, true, false, 1, 0},
		{"cite_disabled", "Text [ID:bad].", "Text [ID:bad].", "", false, false, false, false, 0, 0},
		{"already_streamed", "Text [ID:bad].", "Text [ID:bad].", "skipped_streamed", true, true, false, false, 0, 1},
		{"marker_only", "[ID:bad]", "", "", true, false, true, true, 0, 0},
		{"bold_marker_only", "**[ID:bad]**", "", "", true, false, true, true, 0, 0},
		{"partial_marker", "Text [ID:unfinished", "Text [ID:unfinished", "unresolved_markers", true, false, false, false, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := getDefaultChatInvoker()
			SetDefaultChatInvoker(&errInvoker{err: errors.New("provider failed")})
			t.Cleanup(func() { SetDefaultChatInvoker(previous) })
			withAgentRunner(t, func(ctx context.Context, _ AgentParam) (*schema.Message, error) {
				if tc.streamed {
					runtime.EmitAgentMessage(ctx, tc.original, "")
				}
				return schema.AssistantMessage(tc.original, nil), nil
			})
			state := canvas.NewCanvasState("run", "session")
			if !tc.noRefs {
				state.SetRetrievalChunks([]map[string]any{{"id": "0", "content": "Source"}})
			}
			var emitted []string
			ctx := runtime.WithAgentMessageEmitter(runtime.WithState(t.Context(), state), func(content, _ string) {
				if content != "" {
					emitted = append(emitted, content)
				}
			})
			out, err := NewAgentComponent(AgentParam{ModelID: "stub", Cite: tc.cite}).Invoke(ctx, nil, map[string]any{"user_prompt": "question"})
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "no answer text after citation validation") || len(emitted) != 0 {
					t.Fatalf("err=%v emitted=%q", err, emitted)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if out["content"] != tc.want || len(emitted) != 1 || emitted[0] != tc.want {
				t.Fatalf("out=%v emitted=%q", out, emitted)
			}
			if tc.cite {
				if out["citation_validation_status"] != tc.status || out["removed_citation_count"] != tc.removed || out["unresolved_citation_count"] != tc.unresolved {
					t.Fatalf("out=%v", out)
				}
			} else if _, present := out["citation_validation_status"]; present {
				t.Fatal("cite=false must not silently change streamed citation behavior")
			}
		})
	}
}

func TestGroundingPreservesTextAndCitationIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, original, candidate, want, status string
	}{
		{"valid_addition", "Ochrana je potrebná.", "Ochrana je potrebná [ID:0].", "Ochrana je potrebná [ID:0].", "applied"},
		{"valid_hex", "Áno.", "Áno [ID:e52600578f031081].", "Áno [ID:e52600578f031081].", "applied"},
		{"rewritten_number", "Výška je 136,200 m.", "Výška je 136,20 m [ID:0].", "Výška je 136,200 m.", "rejected: content_changed"},
		{"changed_fact", "Ochrana je potrebná.", "Ochrana nie je potrebná [ID:0].", "Ochrana je potrebná.", "rejected: content_changed"},
		{"meta_reply", "Ochrana je potrebná.", "Please share the text you'd like me to cite.", "Ochrana je potrebná.", "rejected: content_changed"},
		{"appended_note", "Požiadavky sú A a B.", "Požiadavky sú A a B [ID:0].\n\nPoznámka: niektoré ID neboli nájdené.", "Požiadavky sú A a B.", "rejected: content_changed"},
		{"unknown_id", "Ochrana je potrebná.", "Ochrana je potrebná [ID:unknown].", "Ochrana je potrebná.", "rejected: unknown_citation"},
		{"merged_ids", "Ochrana je potrebná.", "Ochrana je potrebná [ID:0, ID:1].", "Ochrana je potrebná.", "rejected: malformed_citation"},
		{"marker_only", "Ochrana je potrebná.", "[ID:0]", "Ochrana je potrebná.", "rejected: empty_response"},
		{"format_only", "Ochrana je potrebná.", "Ochrana  je potrebná.", "Ochrana je potrebná.", "unchanged"},
		{"changed_layout_with_citation", "Prvá veta.\n\nDruhá veta.", "Prvá veta. Druhá veta [ID:0].", "Prvá veta.\n\nDruhá veta.", "rejected: format_changed"},
		{"removed_existing", "Prvá veta [ID:0]. Druhá veta.", "Prvá veta. Druhá veta [ID:1].", "Prvá veta [ID:0]. Druhá veta.", "rejected: existing_citation_changed"},
		{"moved_existing", "Prvá veta [ID:0]. Druhá veta.", "Prvá veta. Druhá veta [ID:0][ID:1].", "Prvá veta [ID:0]. Druhá veta.", "rejected: existing_citation_changed"},
		{"lost_duplicate", "Prvá veta [ID:0]. Druhá veta [ID:0].", "Prvá veta [ID:0]. Druhá veta [ID:1].", "Prvá veta [ID:0]. Druhá veta [ID:0].", "rejected: existing_citation_changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &groundingStreamingInvoker{groundingTestInvoker: groundingTestInvoker{responses: []string{tc.candidate}}}
			previous := getDefaultChatInvoker()
			SetDefaultChatInvoker(inv)
			t.Cleanup(func() { SetDefaultChatInvoker(previous) })
			withAgentRunner(t, func(context.Context, AgentParam) (*schema.Message, error) {
				return schema.AssistantMessage(tc.original, nil), nil
			})
			state := canvas.NewCanvasState("run", "session")
			state.SetRetrievalChunks([]map[string]any{{"id": "0", "content": "First source"}, {"id": "1", "content": "Second source"}, {"id": "e52600578f031081", "content": "Hash source"}})
			ctx := runtime.WithState(t.Context(), state)
			var emitted string
			ctx = runtime.WithAgentMessageEmitter(ctx, func(content, _ string) { emitted += content })
			out, err := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true}).Invoke(ctx, nil, map[string]any{"user_prompt": "question"})
			if err != nil {
				t.Fatal(err)
			}
			if out["content"] != tc.want || out["grounding_status"] != tc.status || emitted != tc.want {
				t.Fatalf("content=%q status=%q emitted=%q; want %q / %q", out["content"], out["grounding_status"], emitted, tc.want, tc.status)
			}
		})
	}
}

func TestGroundingBatchFailurePreservesAcceptedContent(t *testing.T) {
	for _, tc := range []struct {
		name, first, second, want, status string
		failAt, calls                     int
	}{
		{"rejected_later", "original [ID:0]", "A different answer [ID:13]", "original [ID:0]", "rejected: content_changed", 0, 2},
		{"provider_error_later", "original [ID:0]", "", "original [ID:0]", "error:", 2, 2},
		{"future_source_not_visible", "original [ID:13]", "", "original", "rejected: unknown_citation", 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := &groundingStreamingInvoker{groundingTestInvoker: groundingTestInvoker{responses: []string{tc.first, tc.second}, failAt: tc.failAt}}
			previous := getDefaultChatInvoker()
			SetDefaultChatInvoker(inv)
			t.Cleanup(func() { SetDefaultChatInvoker(previous) })
			withAgentRunner(t, func(context.Context, AgentParam) (*schema.Message, error) {
				return schema.AssistantMessage("original", nil), nil
			})
			state := canvas.NewCanvasState("run", "session")
			var chunks []map[string]any
			for i := 0; i < 30; i++ {
				chunks = append(chunks, map[string]any{"id": fmt.Sprint(i), "content": strings.Repeat("x", 3000)})
			}
			state.SetRetrievalChunks(chunks)
			var emitted []string
			ctx := runtime.WithAgentMessageEmitter(runtime.WithState(t.Context(), state), func(content, _ string) {
				if content != "" {
					emitted = append(emitted, content)
				}
			})
			out, err := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true}).Invoke(ctx, nil, map[string]any{"user_prompt": "question"})
			if err != nil {
				t.Fatal(err)
			}
			status, _ := out["grounding_status"].(string)
			if out["content"] != tc.want || !strings.HasPrefix(status, tc.status) || inv.calls != tc.calls {
				t.Fatalf("content=%q status=%q calls=%d", out["content"], status, inv.calls)
			}
			if len(emitted) != 1 || emitted[0] != tc.want {
				t.Fatalf("emitted=%q, want exactly one accepted answer", emitted)
			}
		})
	}
}

type errInvoker struct {
	err error
}

func (e *errInvoker) Invoke(_ context.Context, _ *gorm.DB, _ ChatInvokeRequest) (*ChatInvokeResponse, error) {
	return nil, e.err
}

// contains is a tiny strings.Contains alias kept local to avoid an
// extra import in this single-use case.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestCitationGroundingHasSubstantiveText: marker-only strings are rejected;
// real answer text (with or without markers) is accepted.
func TestCitationGroundingHasSubstantiveText(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"[ID:0]", false},
		{"[ID:0] [ID:1]", false},
		{"[ID: 0]\n[ID:1]", false},
		{"[ID:simple-0]", false},
		{"[ID:ck-1]", false},
		{"[ID:simple-0] [ID:ck-1]", false},
		{"[ID: simple-0]", false},
		{"[2026]", true},
		{"  [2026]  ", true},
		{"The reimbursement request requires manager approval.", true},
		{"The reimbursement request requires manager approval. [ID:0]", true},
		{"The reimbursement request requires manager approval. [ID:simple-0]", true},
		{"答案[ID:0]", true},
		{"答案[ID:ck-1]", true},
	}
	for _, tc := range cases {
		if got := citationGroundingHasSubstantiveText(tc.in); got != tc.want {
			t.Errorf("citationGroundingHasSubstantiveText(%q)=%v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestGrounding_MarkerOnlyFallsBack: non-streaming grounding that returns
// only [ID:…] markers must keep the first-pass answer (#20613).
func TestGrounding_MarkerOnlyFallsBack(t *testing.T) {
	inv := &groundingTestInvoker{responses: []string{"[ID:0] [ID:1]"}}
	prev := getDefaultChatInvoker()
	SetDefaultChatInvoker(inv)
	defer SetDefaultChatInvoker(prev)

	withAgentRunner(t, func(_ context.Context, _ AgentParam) (*schema.Message, error) {
		return &schema.Message{Role: schema.Assistant, Content: "The reimbursement request requires manager approval."}, nil
	})

	state := canvas.NewCanvasState("r1", "t1")
	state.SetRetrievalChunks([]map[string]any{{"id": "0", "content": "policy text"}})
	ctx := runtime.WithState(t.Context(), state)

	c := NewAgentComponent(AgentParam{ModelID: "stub", Cite: true})
	out, err := c.Invoke(ctx, nil, map[string]any{"user_prompt": "q"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	want := "The reimbursement request requires manager approval."
	if got, _ := out["content"].(string); got != want {
		t.Errorf("content=%q, want %q (marker-only grounding must not replace original)", got, want)
	}
	if got := out["grounding_status"]; got == "applied" {
		t.Errorf("grounding_status=%v, want not applied for marker-only response", got)
	}
	if inv.calls != 1 {
		t.Errorf("expected 1 grounding call, got %d", inv.calls)
	}
}
