// Package spillpolicy ports @deepseek-ai/dsh-spill-policy: a tools
// post-execute result transformer that keeps oversized plain-text tool
// results out of the model's context. When a final result's UTF-8 size
// exceeds MaxInlineBytes, it saves the FULL text to a session-scoped spill
// artifact (the spill.Store seam) and replaces the model-facing result with a
// bounded head/tail preview plus the backend's locator and retrieval
// guidance.
//
// The policy registers NO service and owns NO storage or preview mechanics:
// preview is dshgo/outputretention (TextRetainer), storage is the spill
// Store. The policy only decides WHEN to spill and composes the notice.
//
// Deliberately narrow:
//   - nil MaxInlineBytes: the policy registers nothing (a true no-op).
//   - Plain-text results only: a result carrying any non-text block is left
//     untouched (the policy knows only the final formatted text, not tool
//     internals).
//   - Nested composite calls skip this model-facing arm (their durable log
//     copies would be bounded by the official dispatch-log arm, deferred in
//     Go together with PTC run_code).
//   - Accepted value replacements pass through untouched: this presentation
//     policy cannot also replace content in the same mutually exclusive
//     decision.
//   - read is skipped to avoid a read → spill → read again loop.
//   - Best-effort: no session owner, no store backend, or a save failure
//     keeps the original result. A spill failure must NEVER turn a
//     successful tool call into an error or hide the inline result.
//
// It composes with other post-execute listeners: the listener delegates via
// next() first and bounds the resulting content projection, so a hook that
// replaced content still has its replacement bounded, and value replacements
// and block decisions pass through unchanged.
package spillpolicy

import (
	"context"
	"fmt"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/outputretention"
	"dshgo/session"
	"dshgo/spill"
	"dshgo/tools"
)

// Name is the cordis plugin name used by loader diagnostics.
const Name = "spill-policy"

// Config is the plugin config.
type Config struct {
	// MaxInlineBytes is the model-facing context cap for a plain-text tool
	// result, in UTF-8 bytes. Nil disables the policy entirely (no-op). When
	// set, a result larger than this is spilled and replaced with a preview
	// derived from this same budget.
	MaxInlineBytes *int
}

// ValidateConfig rejects a cap that would reach TextRetainer's budget assert
// per call. Validation happens at LOAD, not per call: a negative cap would
// otherwise turn every oversized-result call into an error. A bad config must
// fail the deployment, not the tool.
func ValidateConfig(config Config) error {
	if config.MaxInlineBytes == nil {
		return nil
	}
	if *config.MaxInlineBytes < 0 {
		return fmt.Errorf("spill-policy: maxInlineBytes must be a non-negative integer (got %d)", *config.MaxInlineBytes)
	}
	return nil
}

// flattenPlainText flattens all-text content to one string, or reports false
// when any block is non-text.
func flattenPlainText(content []llm.ContentBlock) (string, bool) {
	var text []byte
	for _, block := range content {
		if block.Type != llm.BlockText {
			return "", false
		}
		text = append(text, block.Text...)
	}
	return string(text), true
}

// spillNotice is the spill-notice line for a given omission and saved
// reference (no preview, no leading blank line).
func spillNotice(omitted outputretention.Omitted, ref spill.SpillRef) string {
	omission := outputretention.DescribeOmitted(omitted, "bytes")
	return fmt.Sprintf("(%s Full formatted result stored at: %s. %s)", omission, ref.Locator, ref.RetrievalHint)
}

// Spiller saves oversized text and builds the bounded replacement
// (preview + notice), or reports no replacement when the policy must keep the
// original (no session owner, no backend, storage failure, or no within-cap
// replacement). Shared by the model-facing post-execute arm so both it and a
// future dispatch-log arm produce byte-identical projections.
type Spiller struct {
	Store spill.Store
	// ResolveOwner maps an execution's agent key to its owning session id.
	// A nil resolver, or a false report, reads as "no session owner".
	ResolveOwner func(tools.ScopeKey) (session.SessionID, bool)
	Logger       cordis.Logger
	// Cap is the validated MaxInlineBytes budget.
	Cap int
}

// Replace spills text and composes the replacement, or returns false when
// the original must stay inline.
func (s *Spiller) Replace(ctx context.Context, text string, totalBytes int, ownerSession session.SessionID, toolName string, callID string, label string) (string, bool) {
	if ownerSession == "" {
		s.warn(fmt.Sprintf("spill-policy: no session owner for %s %s; keeping the inline content", toolName, label))
		return "", false
	}
	if s.Store == nil {
		s.warn("spill-policy: no ctx.spillStore backend loaded; keeping the inline content")
		return "", false
	}
	ref, err := s.Store.SaveText(ctx, spill.SaveTextSpill{
		Owner:         spill.SpillOwner{SessionID: ownerSession},
		Source:        spill.SpillSource{ToolName: toolName, CallID: callID, Label: label},
		SuggestedName: toolName + ".txt",
		Content:       text,
	})
	if err != nil {
		// Best-effort: a storage failure (permissions, no space, backend
		// down) must never fail the call or hide the content — keep the
		// original inline.
		s.warn(fmt.Sprintf("spill-policy: saveText failed for %s: %v; keeping the inline content", toolName, err))
		return "", false
	}

	// Reserve the notice's byte cost INSIDE the cap so the replacement
	// (preview + blank line + notice) never exceeds the documented cap — a
	// naive preview that spent the whole budget then appended the notice
	// could be larger than the cap, and for a marginally-over result even
	// larger than the original. The reservation uses a notice priced at the
	// worst-case omission count (the full byte total): its digit count
	// bounds the real count's, so the reserved size is a safe upper bound
	// and the final notice is never longer than what we reserved. "\n\n" is
	// the 2-byte join.
	reserve := len(spillNotice(outputretention.ExactOmitted(totalBytes), ref)) + 2
	previewBudget := s.Cap - reserve
	if previewBudget < 0 {
		previewBudget = 0
	}
	headBytes := (previewBudget + 1) / 2
	tailBytes := previewBudget / 2
	retainer := outputretention.NewTextRetainer(outputretention.TextStrategy{
		Kind: "headTail", HeadBytes: headBytes, TailBytes: tailBytes,
	})
	retainer.PushString(text)
	kept := retainer.Finish()
	replacedText := kept.Text
	if replacedText != "" {
		replacedText += "\n\n"
	}
	replacedText += spillNotice(kept.OmittedBytes, ref)
	// Invariant: the policy NEVER emits a replacement larger than the cap.
	// When the notice alone exceeds the cap (a tiny cap or a long spill
	// root), there is no within-cap replacement, so keep the inline content
	// — spilling would break the advertised cap. A within-cap replacement
	// is always smaller than the original, which is > cap by the entry
	// condition, so this one check subsumes "not smaller than the original"
	// too. The spill file already written is a harmless orphan; cleanup is
	// deferred.
	if len(replacedText) > s.Cap {
		s.warn(fmt.Sprintf("spill-policy: spill notice for %s exceeds maxInlineBytes; keeping the inline content", toolName))
		return "", false
	}
	return replacedText, true
}

func (s *Spiller) warn(message string) {
	if s.Logger != nil {
		s.Logger.Warn(message)
	}
}

// Attach installs the post-execute listener. A nil store is legitimate: the
// listener then warns and keeps every result inline. The returned disposer
// detaches it.
func Attach(runtime *tools.ToolRuntime, store spill.Store, logger cordis.Logger, config Config, resolveOwner func(tools.ScopeKey) (session.SessionID, bool)) (func(), error) {
	// Nil cap ⇒ no automatic spill policy: register nothing at all.
	if config.MaxInlineBytes == nil {
		return func() {}, nil
	}
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	spiller := &Spiller{Store: store, ResolveOwner: resolveOwner, Logger: logger, Cap: *config.MaxInlineBytes}
	detach := runtime.OnPostExecute(nil, func(exec *tools.ToolExecution, result *tools.ToolExecutionResult, next func(*tools.ToolExecutionResult) *tools.PostToolDecision) *tools.PostToolDecision {
		// Delegate first so a downstream listener (e.g. a hook) settles the
		// result; we bound whatever it accepted. A block passes through —
		// spill only shapes accepted plain-text results, never corrective
		// feedback.
		decision := next(result)
		if decision.Kind != tools.PostAccept || decision.HasValue || exec.Parent != nil || exec.Name == "read" {
			return decision
		}
		content := result.Content
		if decision.HasContent {
			content = decision.ReplaceContent
		}
		text, ok := flattenPlainText(content)
		if !ok {
			return decision
		}
		totalBytes := len(text)
		if totalBytes <= *config.MaxInlineBytes {
			return decision
		}
		var owner session.SessionID
		if spiller.ResolveOwner != nil && exec.Agent != nil {
			owner, _ = spiller.ResolveOwner(exec.Agent)
		}
		replacedText, ok := spiller.Replace(context.Background(), text, totalBytes, owner, exec.Name, exec.CallID, "result")
		if !ok {
			return decision
		}
		return &tools.PostToolDecision{
			Kind:               tools.PostAccept,
			ReplaceContent:     []llm.ContentBlock{{Type: llm.BlockText, Text: replacedText}},
			HasContent:         true,
			AdditionalContexts: decision.AdditionalContexts,
		}
	})
	return detach, nil
}
