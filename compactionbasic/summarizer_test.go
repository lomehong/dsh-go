package compactionbasic

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

// fakeStreamer is a deterministic one-shot stream over fixed chunks.
type fakeStreamer struct {
	chunks []llm.StreamChunk
	seen   llm.GenerateOptions
}

func (f *fakeStreamer) Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk] {
	f.seen = options
	return func(yield func(llm.StreamChunk) bool) {
		for _, chunk := range f.chunks {
			if !yield(chunk) {
				return
			}
		}
	}
}

func textChunk(text string) llm.StreamChunk {
	return llm.StreamChunk{Type: "text-delta", Text: text}
}

func usageChunk(usage llm.TokenUsage) llm.StreamChunk {
	return llm.StreamChunk{Type: "usage", Usage: &usage}
}

func finishChunk(kind string) llm.StreamChunk {
	return llm.StreamChunk{Type: "finish", Reason: &llm.FinishReason{Kind: kind}}
}

func TestFrameSummaryWrapsVerbatim(t *testing.T) {
	summary := []llm.ContentBlock{{Type: llm.BlockText, Text: "## Primary Request and Intent\n- do the thing"}}
	framed := FrameSummary(summary)
	if len(framed) != 3 {
		t.Fatalf("framed blocks wrong: %d", len(framed))
	}
	if framed[0].Text != CheckpointPreamble+"\n\n"+SummaryOpenTag {
		t.Fatalf("preamble/open wrong: %q", framed[0].Text)
	}
	if framed[1].Text != "## Primary Request and Intent\n- do the thing" || framed[2].Text != SummaryCloseTag {
		t.Fatalf("body/close wrong: %q / %q", framed[1].Text, framed[2].Text)
	}
	if !strings.HasPrefix(CheckpointPreamble, "This is an automatically generated checkpoint condensing") {
		t.Fatal("preamble must stay verbatim")
	}
}

func TestFinishErrorMapping(t *testing.T) {
	err := finishError(llm.FinishReason{Kind: "max-tokens"})
	var llmErr *llm.Error
	if !errors.As(err, &llmErr) || llmErr.Code() != "MAX_TOKENS" || !strings.Contains(err.Error(), "truncated at the token cap") {
		t.Fatalf("max-tokens mapping wrong: %v", err)
	}
	err = finishError(llm.FinishReason{Kind: "error", Failure: &llm.LlmFailure{Code: "PROVIDER", Message: "boom"}})
	if !errors.As(err, &llmErr) || llmErr.Code() != "PROVIDER" {
		t.Fatalf("error mapping wrong: %v", err)
	}
	if finishError(llm.FinishReason{Kind: "stop"}) != nil {
		t.Fatal("normal stop must not map to an error")
	}
}

func TestSummaryTextFiltersAndRejectsImages(t *testing.T) {
	blocks := []llm.ContentBlock{
		{Type: llm.BlockText, Text: "keep"},
		{Type: llm.BlockToolResult, Content: []llm.ContentBlock{{Type: llm.BlockImage, Attachment: "att"}}},
		{Type: llm.BlockToolCall, Name: "ignored"},
	}
	if !contentHasImage(blocks) {
		t.Fatal("nested image must be detected")
	}
	_, err := summaryText(blocks)
	var llmErr *llm.LlmError
	if err == nil || !errors.As(err, &llmErr) || llmErr.Code() != "UNSUPPORTED_CONTENT" {
		t.Fatalf("image summary must fail: %v", err)
	}
	kept, err := summaryText([]llm.ContentBlock{{Type: llm.BlockText, Text: "keep"}, {Type: llm.BlockToolCall, Name: "x"}})
	if err != nil || len(kept) != 1 || kept[0].Text != "keep" {
		t.Fatalf("text filter wrong: %#v %v", kept, err)
	}
}

func TestSummarizeWithLlmStreamCall(t *testing.T) {
	streamer := &fakeStreamer{
		chunks: []llm.StreamChunk{
			textChunk("check"), textChunk("point"),
			usageChunk(llm.TokenUsage{InputTokens: 10, OutputTokens: 2}),
			finishChunk("stop"),
		},
	}
	view := staticAgentView(t, map[string]any{})
	config := SummaryConfig{MaxTokens: 4096}
	result, err := SummarizeWithLlm(streamer, config, SummarizationInput{
		System:   "sys",
		Messages: []llm.Message{llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}}, llm.MessageSource{})},
	}, view, context.Background())
	if err != nil {
		t.Fatalf("summarize failed: %v", err)
	}
	if len(result.Summary) != 1 || result.Summary[0].Text != "checkpoint" {
		t.Fatalf("summary wrong: %#v", result.Summary)
	}
	if len(result.RawOutput) != 1 {
		t.Fatalf("raw output wrong: %#v", result.RawOutput)
	}
	if !result.LlmStreamCall || result.Provider != "deepseek" || result.Model != "chat" {
		t.Fatalf("call facts wrong: %+v", result)
	}
	if result.MaxTokens == nil || *result.MaxTokens != 4096 || result.Usage == nil || result.Usage.OutputTokens != 2 {
		t.Fatalf("envelope accounting wrong: %+v", result)
	}
	// The instruction rides as the final user message after the replay.
	messages := streamer.seen.Messages
	if len(messages) != 2 {
		t.Fatalf("messages wrong: %d", len(messages))
	}
	last := messages[len(messages)-1]
	if last.Content[0].Text != compactionInstruction {
		t.Fatal("the compaction instruction must be the final message verbatim")
	}
	if last.Source.Kind != "plugin" || last.Source.Plugin != "dsh-compaction-basic" {
		t.Fatalf("instruction source wrong: %+v", last.Source)
	}
	if streamer.seen.System != "sys" || streamer.seen.MaxTokens == nil || *streamer.seen.MaxTokens != 4096 {
		t.Fatalf("envelope wrong: %+v", streamer.seen)
	}
	if streamer.seen.Purpose != "compaction" || streamer.seen.SessionID != "summarize" {
		t.Fatalf("routing fields wrong: %+v", streamer.seen)
	}
}

func TestSummarizeWithLlmNoTargetFails(t *testing.T) {
	streamer := &fakeStreamer{chunks: nil}
	empty := staticAgentView(t, nil)
	_, err := SummarizeWithLlm(streamer, SummaryConfig{MaxTokens: 1}, SummarizationInput{}, empty, context.Background())
	if err == nil || !strings.Contains(err.Error(), "no provider/model available for summarization") {
		t.Fatalf("targetless summarize must fail loud: %v", err)
	}
}

func TestSummarizeWithLlmEmptyOutputFails(t *testing.T) {
	streamer := &fakeStreamer{chunks: nil}
	view := staticAgentView(t, map[string]any{})
	_, err := SummarizeWithLlm(streamer, SummaryConfig{MaxTokens: 8}, SummarizationInput{}, view, context.Background())
	if err == nil || !strings.Contains(err.Error(), "summarization produced no text summary content") {
		t.Fatalf("empty output must fail loud: %v", err)
	}
}

// staticAgentView builds a view over a detached session carrying an optional
// request/header snapshot.
func staticAgentView(t *testing.T, header map[string]any) AgentView {
	t.Helper()
	sess := newTestSession(t, "summarize")
	if header != nil {
		appendEvent(t, sess, session.EventRequestHeader, session.RequestHeaderData{
			Header: testHeader("deepseek", "chat"),
			Reason: session.HeaderReasonInitial,
		}, nil)
	}
	return staticView{sess: sess}
}

type staticView struct{ sess *session.Session }

func (v staticView) Session() *session.Session { return v.sess }
func (v staticView) OptionsProvider() string   { return "" }
func (v staticView) OptionsModel() string      { return "" }
