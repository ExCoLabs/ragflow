package component

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"ragflow/internal/tokenizer"
)

func bigText(tokens int) string {
	// ~1 token per word for the cl100k counter used by NumTokensFromString.
	return strings.Repeat("word ", tokens)
}

func totalTokens(msgs []*schema.Message) int {
	sum := 0
	for _, m := range msgs {
		if m != nil {
			sum += tokenizer.NumTokensFromString(m.Content)
		}
	}
	return sum
}

func TestFitToolResultMessages_NoTrimWhenUnderBudget(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.System, Content: "sys"},
		{Role: schema.User, Content: "q"},
		{Role: schema.Tool, Content: "small result", ToolCallID: "t1"},
	}
	out := fitToolResultMessages(msgs, 10000)
	if out[2].Content != "small result" {
		t.Fatalf("unexpected trim: %q", out[2].Content)
	}
}

func TestFitToolResultMessages_TrimsToolsProportionally(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.System, Content: bigText(100)},
		{Role: schema.User, Content: "question"},
		{Role: schema.Assistant, Content: "", ToolCalls: []schema.ToolCall{{ID: "t1"}, {ID: "t2"}}},
		{Role: schema.Tool, Content: bigText(3000), ToolCallID: "t1"},
		{Role: schema.Tool, Content: bigText(1000), ToolCallID: "t2"},
	}
	budget := 2000
	out := fitToolResultMessages(msgs, budget)
	if len(out) != len(msgs) {
		t.Fatalf("messages dropped: %d", len(out))
	}
	if out[3].ToolCallID != "t1" || out[4].ToolCallID != "t2" {
		t.Fatal("tool_call pairing broken")
	}
	if got := totalTokens(out); got > budget+16 { // +margin for the [truncated] marker
		t.Fatalf("over budget: %d > %d", got, budget)
	}
	big := tokenizer.NumTokensFromString(out[3].Content)
	small := tokenizer.NumTokensFromString(out[4].Content)
	if big <= small {
		t.Fatalf("expected proportional split, big=%d small=%d", big, small)
	}
	if !strings.Contains(out[3].Content, "[truncated]") {
		t.Fatal("missing truncation marker")
	}
}

func TestFitToolResultMessages_FloorWhenBudgetTiny(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: bigText(1500)},
		{Role: schema.Tool, Content: bigText(3000), ToolCallID: "t1"},
		{Role: schema.Tool, Content: bigText(2000), ToolCallID: "t2"},
	}
	out := fitToolResultMessages(msgs, 1600)
	for i := 1; i <= 2; i++ {
		got := tokenizer.NumTokensFromString(out[i].Content)
		if got == 0 || got > agentToolMsgFloorTokens+16 {
			t.Fatalf("tool %d expected <= floor+marker, got %d", i, got)
		}
		if !strings.HasSuffix(out[i].Content, "[truncated]") {
			t.Fatalf("tool %d not truncated", i)
		}
	}
}

func TestFitToolResultMessages_DisabledBudget(t *testing.T) {
	msgs := []*schema.Message{{Role: schema.Tool, Content: bigText(3000), ToolCallID: "t1"}}
	out := fitToolResultMessages(msgs, 0)
	if out[0].Content != msgs[0].Content {
		t.Fatal("budget<=0 must not trim")
	}
}
