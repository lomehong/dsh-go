package compactionbasic

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// Tags wrapping the structured summary inside the landed checkpoint node.
const (
	SummaryOpenTag  = "<compacted-summary>"
	SummaryCloseTag = "</compacted-summary>"
)

// compactionInstruction is the summarization directive, delivered as the
// FINAL user message after the replayed conversation rather than as a
// distinct summarizer system prompt. Keeping the conversation's own system
// prompt, tools, and message prefix in front of it makes the auxiliary call
// a genuine prefix of the last routed request, so the provider's KV cache is
// reused instead of invalidated.
var compactionInstruction = strings.Join([]string{
	"You are now acting as a compaction engine for this AI coding assistant. Condense the conversation ABOVE into a structured checkpoint that lets another model resume the work with no loss of essential context.",
	"",
	"Output EXACTLY the Markdown structure below: keep every section, in order. Use terse bullets, not prose paragraphs. Write \"(none)\" for an empty section — never drop a section.",
	"",
	"## Primary Request and Intent",
	"- [the user's original and evolving goals; quote verbatim where the exact wording matters]",
	"",
	"## Key Technical Concepts",
	"- [technologies, frameworks, patterns, and conventions in play]",
	"",
	"## Files and Code",
	"- [exact path: why it matters, key changes or snippets]",
	"",
	"## Errors and Fixes",
	"- [error: how it was resolved, plus any related user feedback]",
	"",
	"## Pending Jobs",
	"- [explicitly requested work not yet completed]",
	"",
	"## Current Work",
	"- [precisely what was in progress at this checkpoint]",
	"",
	"## Next Step",
	"- [the single next action, directly in line with the most recent request, or \"(none)\"]",
	"",
	"## Critical Context",
	"- [decisions and their rationale, constraints, user preferences, open questions, data needed to continue]",
	"",
	"Rules:",
	"- Write concise English engineering prose. Preserve exact file paths, commands, error strings, identifiers, numeric values, function signatures, and syntax fragments.",
	"- Capture user feedback and explicit instructions faithfully, especially corrections.",
	"- Do NOT mention this summarization request or that the context was compacted.",
	"- Output only the checkpoint text: do not call any tool or take any other action.",
	"- If the conversation already contains a <compacted-summary> block, it is a PRIOR checkpoint. Do not copy it forward verbatim: preserve still-true facts, drop stale ones, and merge newer information into a single consolidated summary under the same structure.",
}, "\n")

// CheckpointPreamble is the framing that makes the replacement user message
// established context.
const CheckpointPreamble = "This is an automatically generated checkpoint condensing an earlier span of the conversation to free up context. Treat the captured context as established background and build on it without restating it. Continue the task directly from the messages that follow, without acknowledging this checkpoint."

// SummarizationInput is the replayed conversation surface the summarizer
// condenses. Reproducing the last routed request's system prompt, tools, and
// leading messages verbatim lets the auxiliary call reuse the provider's
// warm prefix cache; the trailing compaction instruction is then the only
// novel input.
type SummarizationInput struct {
	// System is the conversation's own system prompt, reused for
	// prefix-cache alignment; empty for a system-less request.
	System string
	// Tools are the conversation's tool schemas, reused for prefix-cache
	// alignment; nil when the request carried none.
	Tools []llm.ToolSchema
	// Messages are the shadowed region, in surface order, that precedes the
	// compaction instruction.
	Messages []llm.Message
}

// SummaryResult is safe summary content plus the exact auxiliary call
// envelope recorded with it.
type SummaryResult struct {
	// Summary is the safe text-only model output.
	Summary []llm.ContentBlock
	// RawOutput is the complete provider output before the text-only
	// summary projection.
	RawOutput []llm.ContentBlock
	// LlmStreamCall identifies exactly one call through the LLM stream seam.
	LlmStreamCall bool
	// Provider is the provider route the call used.
	Provider string
	// Model is the model the call used.
	Model string
	// MaxTokens is the generation cap the call sent.
	MaxTokens *int64
	// Usage is the provider-reported usage for this summarization request.
	Usage *llm.TokenUsage
}

// SummaryConfig is the resolved summarization call configuration.
type SummaryConfig struct {
	SummarizationProvider string
	SummarizationModel    string
	MaxTokens             int64
}

// Streamer is the LLM stream seam summarization calls ride; *llm.Runtime
// satisfies it.
type Streamer interface {
	Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk]
}

// AgentView is the agent face the summarizer and region transaction read:
// the routed-model history, fallback model, and session identity.
// *agent.Agent and *agentloop.ReactLoopAgent satisfy it.
type AgentView interface {
	// Session is the live session the agent drives.
	Session() *session.Session
	// OptionsProvider is the configured provider route; empty means unset.
	OptionsProvider() string
	// OptionsModel is the configured model; empty means unset.
	OptionsModel() string
}

// agentView adapts *agent.Agent to AgentView without importing the loop.
type agentView struct{ a *agent.Agent }

func (v agentView) Session() *session.Session { return v.a.Session }
func (v agentView) OptionsProvider() string   { return v.a.Options.Provider }
func (v agentView) OptionsModel() string      { return v.a.Options.Model }

// ViewAgent adapts an *agent.Agent to the AgentView seam.
func ViewAgent(a *agent.Agent) AgentView { return agentView{a} }

// summarizeTarget resolves the exact provider/model the summarization call
// uses: explicit config, then the latest durably routed request, then the
// agent's options.
func summarizeTarget(config SummaryConfig, view AgentView) (Target, error) {
	var configured *Target
	if len(config.SummarizationProvider) > 0 {
		configured = &Target{Provider: config.SummarizationProvider, Model: config.SummarizationModel}
	}
	var agentTarget *Target
	if len(view.OptionsProvider()) > 0 && len(view.OptionsModel()) > 0 {
		agentTarget = &Target{Provider: view.OptionsProvider(), Model: view.OptionsModel()}
	}
	var latest *Target
	if header := view.Session().RequestHeader(); header != nil {
		if len(header.Config.Provider) > 0 && len(header.Config.Model) > 0 {
			latest = &Target{Provider: header.Config.Provider, Model: header.Config.Model}
		}
	}
	target := configured
	if target == nil {
		target = latest
	}
	if target == nil {
		target = agentTarget
	}
	if target == nil {
		return Target{}, fmt.Errorf(
			"no provider/model available for summarization: set both BasicCompactionConfig summarization fields, " +
				"route one request, or set both AgentOptions fields")
	}
	return *target, nil
}

// SummarizeWithLlm runs the default cache-reusing stream summarization call:
// replay the conversation prefix, then append the compaction instruction as
// the final user message so the provider's warm prefix cache is reused.
func SummarizeWithLlm(runtime Streamer, config SummaryConfig, input SummarizationInput, view AgentView, signal context.Context) (SummaryResult, error) {
	result := SummaryResult{}
	target, err := summarizeTarget(config, view)
	if err != nil {
		return result, err
	}
	assembler := llm.NewBlockAssembler()
	messages := append(append([]llm.Message{}, input.Messages...), llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: compactionInstruction}},
		llm.MessageSource{Kind: "plugin", Plugin: "dsh-compaction-basic"},
	))
	options := llm.GenerateOptions{
		Provider:  target.Provider,
		Model:     target.Model,
		Messages:  messages,
		System:    input.System,
		Tools:     append([]llm.ToolSchema{}, input.Tools...),
		MaxTokens: &config.MaxTokens,
		SessionID: string(view.Session().ID()),
		Purpose:   "compaction",
		Context:   signal,
	}
	for chunk := range runtime.Stream(options) {
		assembler.Push(chunk)
	}
	if finishErr := finishError(assembler.Finish()); finishErr != nil {
		return result, finishErr
	}
	rawOutput := assembler.Blocks()
	summary, err := summaryText(rawOutput)
	if err != nil {
		return result, err
	}
	hasText := false
	for _, block := range summary {
		if strings.TrimSpace(block.Text) != "" {
			hasText = true
			break
		}
	}
	if !hasText {
		return result, fmt.Errorf("summarization produced no text summary content")
	}
	result = SummaryResult{
		Summary:       summary,
		RawOutput:     rawOutput,
		LlmStreamCall: true,
		Provider:      options.Provider,
		Model:         options.Model,
		MaxTokens:     &config.MaxTokens,
		Usage:         assembler.Usage(),
	}
	return result, nil
}

// FrameSummary wraps raw summary blocks in the durable checkpoint framing:
// the content for the synthesized replacement user message.
func FrameSummary(summary []llm.ContentBlock) []llm.ContentBlock {
	framed := make([]llm.ContentBlock, 0, len(summary)+2)
	framed = append(framed, llm.ContentBlock{
		Type: llm.BlockText,
		Text: CheckpointPreamble + "\n\n" + SummaryOpenTag,
	})
	framed = append(framed, summary...)
	framed = append(framed, llm.ContentBlock{Type: llm.BlockText, Text: SummaryCloseTag})
	return framed
}

// finishError maps a terminal summarization finish to its fail-closed error.
func finishError(finish llm.FinishReason) error {
	switch finish.Kind {
	case "error", "aborted":
		failure := finish.Failure
		if failure == nil {
			return fmt.Errorf("summarization failed")
		}
		return llm.NewError(failure.Code, failure.Message, nil)
	case "max-tokens":
		return llm.NewError(
			"MAX_TOKENS",
			"summarization truncated at the token cap (incomplete checkpoint)",
			nil)
	default:
		return nil
	}
}

// summaryText rejects visual output and keeps only text before synthesizing
// a user message.
func summaryText(blocks []llm.ContentBlock) ([]llm.ContentBlock, error) {
	if contentHasImage(blocks) {
		return nil, llm.NewLlmError("compaction summary cannot contain image output", "UNSUPPORTED_CONTENT", llm.LlmFailure{})
	}
	summary := make([]llm.ContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == llm.BlockText {
			summary = append(summary, block)
		}
	}
	return summary, nil
}

// contentHasImage reports whether any block (recursively through tool
// results) is an image.
func contentHasImage(blocks []llm.ContentBlock) bool {
	for _, block := range blocks {
		switch block.Type {
		case llm.BlockImage:
			return true
		case llm.BlockToolResult:
			if contentHasImage(block.Content) {
				return true
			}
		}
	}
	return false
}
