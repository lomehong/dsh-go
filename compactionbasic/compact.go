package compactionbasic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"dshgo/commands"
	"dshgo/compaction"
)

// The human-facing /compact command over the backend-independent compaction
// seam (official command-compact): argument-free manual compaction whose
// expected capability failures convert into concise human-only outcomes.

// compactUsage is the rejection text for any argument supply.
const compactUsage = "Usage: /compact (no arguments)"

// compactStarter starts one manual compaction for the invocation's receiving
// agent: the composition binds the maintenance owner from the invocation (the
// exact receiving agent rides the dispatch). signal carries the dispatching
// UI request's cancellation.
type compactStarter func(invocation commands.Invocation, signal context.Context) (*compaction.Result, error)

// manualFailureText converts one expected capability failure into its
// concise human-only outcome text. The closed ManualCompactionKind union is
// handled exhaustively; an unknown kind is a composition bug and fails loud.
func manualFailureText(kind ManualCompactionKind) (string, error) {
	switch kind {
	case ManualBusy:
		return "Compaction is unavailable because this process has an active compaction, or the agent is not idle.", nil
	case ManualCancelled:
		return "Compaction cancelled.", nil
	case ManualChanged:
		return "The history selected for compaction changed before it could be replaced. The conversation is unchanged; the attempt is recorded in the session log.", nil
	case ManualSummary:
		return "Compaction could not produce a useful summary. The conversation is unchanged; the attempt is recorded in the session log.", nil
	case ManualCommit:
		return "Compaction did not finish cleanly; some session history may have changed. Inspect the current session state before retrying.", nil
	case ManualPersistence:
		return "Compaction finished, but the session could not be saved.", nil
	default:
		return "", fmt.Errorf("unknown manual compaction error kind: %s", string(kind))
	}
}

// RegisterCompactCommand registers /compact for every composed human-command
// adapter. The returned disposer unregisters the command and then waits for
// in-flight handlers to quiesce (the composite-teardown drain), so no new
// invocation can enter while started handlers settle.
func RegisterCompactCommand(runtime *commands.CommandRuntime, compactNow compactStarter) (func(), error) {
	var active sync.WaitGroup
	handler := func(invocation commands.Invocation) (commands.CommandResult, error) {
		if strings.TrimSpace(invocation.RawInput) != "" {
			return commands.CommandResult{Kind: commands.ResultError, HasText: true, Text: compactUsage}, nil
		}
		active.Add(1)
		defer active.Done()
		result, err := compactNow(invocation, invocation.Context)
		if err != nil {
			if invocation.Context != nil && invocation.Context.Err() != nil {
				return commands.CommandResult{Kind: commands.ResultError, HasText: true, Text: "Compaction cancelled."}, nil
			}
			var manual *ManualCompactionError
			if errors.As(err, &manual) {
				text, mappingErr := manualFailureText(manual.Kind)
				if mappingErr != nil {
					return commands.CommandResult{}, mappingErr
				}
				return commands.CommandResult{Kind: commands.ResultError, HasText: true, Text: text}, nil
			}
			return commands.CommandResult{}, err
		}
		if result == nil {
			return commands.CommandResult{Kind: commands.ResultSuccess, HasText: true, Text: "No compactable history yet."}, nil
		}
		summarySeq := result.SummarySeq
		return commands.CommandResult{
			Kind:           commands.ResultSuccess,
			HasText:        true,
			Text:           fmt.Sprintf("Compacted %d history items (~%d tokens).", len(result.ShadowedSeqs), result.ShadowedTokenCount),
			SourceEventSeq: &summarySeq,
		}, nil
	}
	undo, err := runtime.Register(nil, commands.CommandDefinition{
		Name:        "compact",
		Description: "Compact older conversation history",
		Handler:     handler,
	})
	if err != nil {
		return nil, err
	}
	return func() {
		undo()
		active.Wait()
	}, nil
}
