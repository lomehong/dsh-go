package toolralph

import (
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/workflow"
)

// RunStatus is one foreground run's terminal verdict.
type RunStatus string

const (
	RunComplete      RunStatus = "complete"
	RunBlocked       RunStatus = "blocked"
	RunBudgetLimited RunStatus = "budget-limited"
)

// RoundFailureStatus marks the child-failed terminal arm.
const RoundFailureStatus = "round-failed"

// RunResult is one graceful terminal run value.
type RunResult struct {
	Status        RunStatus        `json:"status"`
	RoundsStarted int64            `json:"roundsStarted"`
	Report        RalphRoundReport `json:"report"`
}

// RoundFailure is one child-failure terminal value: the run ends without a
// structured report from that round, carrying the last durable handoff.
type RoundFailure struct {
	Status        string            `json:"status"`
	RoundsStarted int64             `json:"roundsStarted"`
	LastReport    *RalphRoundReport `json:"lastReport"`
}

// RalphMeta is the fixed workflow identity block.
func RalphMeta() map[string]any {
	return map[string]any{
		"name":        "ralph-loop",
		"description": "Fresh-agent rounds toward one objective with a bounded structured handoff.",
		"phases":      []any{map[string]any{"title": "Fresh-agent rounds"}},
	}
}

// roundPrompt renders one round's prompt (the fixed script's segments,
// joined by blank lines).
func roundPrompt(objective string, round, maxRounds int64, prior string) string {
	return strings.Join([]string{
		"You are one fresh worker in a foreground Ralph loop. You receive no parent conversation and no prior child session. Do not call the ralph tool: this round already is its worker.",
		"Immutable objective:\n" + objective,
		fmt.Sprintf("Ralph round: %d of %d.", round, maxRounds),
		"The shared workspace and its current working tree are the long-term memory and source of truth. Inspect them before acting, preserve existing work, perform concrete in-scope work, and verify what you change. Treat the previous report only as a bounded handoff; confirm it against the workspace.",
		"Previous structured handoff:\n" + prior,
		"Return one report with exact normalized strings. Use status continue with at least one nextSteps entry while useful work remains; complete only with concrete evidence and no nextSteps; blocked only when no meaningful progress is possible without human input or an external-state change. blocker must be empty unless blocked.",
	}, "\n\n")
}

// Program builds the fixed, deployment-owned orchestration. The model
// supplies data only; it cannot alter the loop, provider route, schema, or
// handoff validation. This is the Go-realm counterpart of the official
// plain-JS script: one fresh structured-output child per round, only the
// bounded handoff crossing rounds. The terminal values are native typed
// results (no worker-realm JSON boundary exists on the Go deployment, so
// the official defensive terminal decode collapses to Go's type system).
func Program(objective string, maxRounds, maxHandoffChars int64) workflow.Script {
	return func(api *workflow.ScriptAPI) (any, error) {
		var previous *RalphRoundReport
		previousJSON := "(none — this is the first round)"
		api.Phase("Fresh-agent rounds")
		for round := int64(1); round <= maxRounds; round++ {
			if round > 1 && previous != nil {
				encoded, err := json.Marshal(previous)
				if err != nil {
					return nil, fmt.Errorf("ralph: previous handoff failed to encode: %w", err)
				}
				previousJSON = string(encoded)
			}
			prompt := roundPrompt(objective, round, maxRounds, previousJSON)
			rawReport, err := api.Agent(prompt, workflow.AgentCall{
				Label:  fmt.Sprintf("Ralph round %d", round),
				Phase:  "Fresh-agent rounds",
				Schema: ReportSchema(),
			})
			if err != nil {
				return nil, err
			}
			if rawReport == nil {
				// A child-run failure or run cancellation: the item-null
				// discipline, surfaced as the round-failed terminal.
				return RoundFailure{Status: RoundFailureStatus, RoundsStarted: round, LastReport: previous}, nil
			}
			// The child's structured result crossed a provider boundary:
			// the defensive handoff validation gates every round.
			expected := StatusContinue
			if declared := decodeTerminalRoundStatus(rawReport); declared != "" {
				expected = declared
			}
			report, err := decodeReport(rawReport, expected, maxHandoffChars)
			if err != nil {
				return nil, err
			}
			if report.Status == StatusComplete {
				return RunResult{Status: RunComplete, RoundsStarted: round, Report: report}, nil
			}
			if report.Status == StatusBlocked {
				return RunResult{Status: RunBlocked, RoundsStarted: round, Report: report}, nil
			}
			previous = &report
		}
		if previous == nil {
			return nil, fmt.Errorf("ralph: budget-limited run ended without a final handoff")
		}
		return RunResult{Status: RunBudgetLimited, RoundsStarted: maxRounds, Report: *previous}, nil
	}
}

// DecodeRunResult defensively decodes the run's terminal value after the
// engine's JSON round-trip (the Go readRunResult): the value crosses the
// engine boundary as lossless JSON, so a typed expectation must be
// re-validated, never asserted.
func DecodeRunResult(value any, maxRounds, maxHandoffChars int64) (RunResult, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return RunResult{}, fmt.Errorf("Ralph workflow returned a malformed terminal result")
	}
	var decoded struct {
		Status        string            `json:"status"`
		RoundsStarted int64             `json:"roundsStarted"`
		Report        RalphRoundReport  `json:"report"`
		LastReport    *RalphRoundReport `json:"lastReport"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return RunResult{}, fmt.Errorf("Ralph workflow returned a malformed terminal result")
	}
	if decoded.RoundsStarted < 1 || decoded.RoundsStarted > maxRounds {
		return RunResult{}, fmt.Errorf("Ralph workflow returned a malformed terminal result")
	}
	switch RunStatus(decoded.Status) {
	case RunComplete:
		if _, err := decodeReport(mapFromReport(decoded.Report), StatusComplete, maxHandoffChars); err != nil {
			return RunResult{}, err
		}
		return RunResult{Status: RunComplete, RoundsStarted: decoded.RoundsStarted, Report: decoded.Report}, nil
	case RunBlocked:
		if _, err := decodeReport(mapFromReport(decoded.Report), StatusBlocked, maxHandoffChars); err != nil {
			return RunResult{}, err
		}
		return RunResult{Status: RunBlocked, RoundsStarted: decoded.RoundsStarted, Report: decoded.Report}, nil
	case RunBudgetLimited:
		if decoded.RoundsStarted != maxRounds {
			return RunResult{}, fmt.Errorf("Ralph workflow returned budget-limited before the round limit")
		}
		if _, err := decodeReport(mapFromReport(decoded.Report), StatusContinue, maxHandoffChars); err != nil {
			return RunResult{}, err
		}
		return RunResult{Status: RunBudgetLimited, RoundsStarted: decoded.RoundsStarted, Report: decoded.Report}, nil
	case RoundFailureStatus:
		if decoded.RoundsStarted == 1 {
			if decoded.LastReport != nil {
				return RunResult{}, fmt.Errorf("Ralph workflow returned an invalid first-round failure")
			}
			return RunResult{}, fmt.Errorf("Ralph round 1 child failed before producing a structured report.")
		}
		if decoded.LastReport == nil {
			return RunResult{}, fmt.Errorf("Ralph workflow returned a round failure without its last handoff")
		}
		last := *decoded.LastReport
		if _, err := decodeReport(mapFromReport(last), StatusContinue, maxHandoffChars); err != nil {
			return RunResult{}, err
		}
		return RunResult{}, fmt.Errorf("Ralph round %d child failed before producing a structured report.\nLast successful handoff:\n%s", decoded.RoundsStarted, mustJSONRound(last))
	default:
		return RunResult{}, fmt.Errorf("Ralph workflow returned an unknown terminal status")
	}
}

// mapFromReport rebuilds the decoded record shape decodeReport validates.
func mapFromReport(report RalphRoundReport) map[string]any {
	evidence := make([]any, 0, len(report.Evidence))
	for _, item := range report.Evidence {
		evidence = append(evidence, item)
	}
	nextSteps := make([]any, 0, len(report.NextSteps))
	for _, item := range report.NextSteps {
		nextSteps = append(nextSteps, item)
	}
	return map[string]any{
		"status":    string(report.Status),
		"summary":   report.Summary,
		"evidence":  evidence,
		"nextSteps": nextSteps,
		"blocker":   report.Blocker,
	}
}

// mustJSONRound renders one report for a failure message.
func mustJSONRound(report RalphRoundReport) string {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Sprintf("%%!unrenderable report: %v", err)
	}
	return string(encoded)
}

// decodeTerminalRoundStatus extracts the report's declared status when the
// value already carries one of the three verdicts.
func decodeTerminalRoundStatus(value any) RalphRoundStatus {
	record, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	status, _ := record["status"].(string)
	switch RalphRoundStatus(status) {
	case StatusContinue:
		return StatusContinue
	case StatusComplete:
		return StatusComplete
	case StatusBlocked:
		return StatusBlocked
	}
	return ""
}
