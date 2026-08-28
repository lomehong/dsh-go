package tokenmeter

import (
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

func TestEstimateContentTypedArms(t *testing.T) {
	// text: ceil(8/4)=2 + overhead 4.
	text := EstimateContent([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcdefgh"}})
	if text != 6 {
		t.Fatalf("text price wrong: %d", text)
	}
	// reasoning prices like text.
	reasoning := EstimateContent([]llm.ContentBlock{{Type: llm.BlockReasoning, Text: "abcd"}})
	if reasoning != 5 {
		t.Fatalf("reasoning price wrong: %d", reasoning)
	}
	// tool-call: name ceil(4/4)=1 + args ceil(6/4)=2 + 4.
	call := EstimateContent([]llm.ContentBlock{{Type: llm.BlockToolCall, Name: "read", Arguments: "{\"a\":1}"}})
	if call != 7 {
		t.Fatalf("tool-call price wrong: %d", call)
	}
	// tool-result: recursion + overhead.
	result := EstimateContent([]llm.ContentBlock{{
		Type:    llm.BlockToolResult,
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "abcd"}},
	}})
	if result != 5+4 {
		t.Fatalf("tool-result price wrong: %d", result)
	}
	// unknown blocks fall through to the structural JSON price.
	unknown := EstimateContent([]llm.ContentBlock{{Type: "custom", Text: "abcd"}})
	structural := EstimateStructuralBlock(llm.ContentBlock{Type: "custom", Text: "abcd"})
	if unknown != structural || structural <= blockOverhead {
		t.Fatalf("unknown block must price structurally: %d vs %d", unknown, structural)
	}
}

func TestEstimateMessageAddsRoleOverhead(t *testing.T) {
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcd"}}, llm.MessageSource{})
	if got := EstimateMessage(message); got != 1+blockOverhead+RoleOverhead {
		t.Fatalf("message price wrong: %d", got)
	}
}

func TestEstimateHeaderParts(t *testing.T) {
	if got := EstimateHeader(nil); got != 0 {
		t.Fatalf("absent header must price 0, got %d", got)
	}
	empty := &session.EpochHeader{}
	if got := EstimateHeader(empty); got != 0 {
		t.Fatalf("header without system or tools must price 0, got %d", got)
	}
	emptyTools := &session.EpochHeader{Tools: []llm.ToolSchema{}}
	if got := EstimateToolsTokens(emptyTools); got != 0 {
		t.Fatalf("empty tool list must price 0, got %d", got)
	}
	full := &session.EpochHeader{
		System: "abcd",
		Tools:  []llm.ToolSchema{{Name: "read"}},
	}
	// system: ceil(4/4)=1 + role 4; tools priced as structural JSON.
	if got := EstimateSystemTokens(full); got != 5 {
		t.Fatalf("system price wrong: %d", got)
	}
	if got := EstimateToolsTokens(full); got <= blockOverhead {
		t.Fatalf("tools price wrong: %d", got)
	}
	if got := EstimateHeader(full); got != EstimateSystemTokens(full)+EstimateToolsTokens(full) {
		t.Fatalf("header total must decompose: %d", got)
	}
}

func TestImageBlocksPriceStructurally(t *testing.T) {
	block := llm.ContentBlock{Type: llm.BlockImage, Attachment: map[string]any{"id": "att-1"}}
	price := EstimateContent([]llm.ContentBlock{block})
	if price != EstimateStructuralBlock(block) {
		t.Fatalf("image must price structurally: %d vs %d", price, EstimateStructuralBlock(block))
	}
}
