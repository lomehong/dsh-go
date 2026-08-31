// Package toolralph ports packages/workflow/tool-ralph: the model-facing
// foreground Ralph loop over the workflow and subagent seams. A fixed
// deployment-owned program starts one fresh structured-output child per
// round, carrying only the immutable objective and the previous bounded
// structured handoff between them. The Go program is the engine's native
// realm counterpart of the official plain-JS script (the engine executes
// exactly one realm form per deployment).
package toolralph

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Config is the deployment policy for the fixed Ralph workflow.
type Config struct {
	// SubagentProvider is the fresh structured-output provider used for
	// every round (default "spawn").
	SubagentProvider string
	// MaxRounds is the default and deployment ceiling for one call's round
	// count (default 256).
	MaxRounds *int64
	// MaxHandoffChars is the maximum serialized characters in one
	// structured handoff (default 16384).
	MaxHandoffChars *int64
	// MaxResultChars is the maximum characters in a successful
	// parent-facing terminal text (default 16384).
	MaxResultChars *int64
}

// ResolvedConfig is the validated deployment policy.
type ResolvedConfig struct {
	SubagentProvider string
	MaxRounds        int64
	MaxHandoffChars  int64
	MaxResultChars   int64
}

const (
	defaultMaxRounds       = int64(256)
	defaultMaxHandoffChars = int64(16384)
	defaultMaxResultChars  = int64(16384)
)

// ResolveConfig validates defaults even when a caller invokes Register
// without Loader normalization.
func ResolveConfig(config Config) (ResolvedConfig, error) {
	provider := config.SubagentProvider
	if provider == "" {
		provider = "spawn"
	}
	maxRounds := valueOr(config.MaxRounds, defaultMaxRounds)
	maxHandoff := valueOr(config.MaxHandoffChars, defaultMaxHandoffChars)
	maxResult := valueOr(config.MaxResultChars, defaultMaxResultChars)
	if provider == "" || provider != strings.TrimSpace(provider) {
		return ResolvedConfig{}, fmt.Errorf("subagentProvider must be a non-empty normalized string")
	}
	for _, check := range []struct {
		name  string
		value int64
	}{
		{"maxRounds", maxRounds},
		{"maxHandoffChars", maxHandoff},
		{"maxResultChars", maxResult},
	} {
		if check.value < 1 {
			return ResolvedConfig{}, fmt.Errorf("%s must be a positive safe integer", check.name)
		}
	}
	return ResolvedConfig{SubagentProvider: provider, MaxRounds: maxRounds, MaxHandoffChars: maxHandoff, MaxResultChars: maxResult}, nil
}

func valueOr(value *int64, fallback int64) int64 {
	if value == nil {
		return fallback
	}
	return *value
}

// ResolveMaxRounds resolves one model-selected cap against the deployment
// ceiling.
func ResolveMaxRounds(requested *int64, ceiling int64) (int64, error) {
	value := ceiling
	if requested != nil {
		value = *requested
	}
	if value < 1 {
		return 0, fmt.Errorf("Ralph maxRounds must be a positive safe integer")
	}
	if value > ceiling {
		return 0, fmt.Errorf("Ralph maxRounds %d exceeds the deployment ceiling %d", value, ceiling)
	}
	return value, nil
}

// RalphRoundStatus is one round report's terminal verdict.
type RalphRoundStatus string

const (
	StatusContinue RalphRoundStatus = "continue"
	StatusComplete RalphRoundStatus = "complete"
	StatusBlocked  RalphRoundStatus = "blocked"
)

// RalphRoundReport is the structured handoff one child returns per round.
type RalphRoundReport struct {
	Status    RalphRoundStatus `json:"status"`
	Summary   string           `json:"summary"`
	Evidence  []string         `json:"evidence"`
	NextSteps []string         `json:"nextSteps"`
	Blocker   string           `json:"blocker"`
}

// ReportSchema is the object-rooted JSON Schema every round child's result
// must satisfy (the fixed script's reportSchema).
func ReportSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status":    map[string]any{"type": "string", "enum": []string{"continue", "complete", "blocked"}},
			"summary":   map[string]any{"type": "string"},
			"evidence":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"nextSteps": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"blocker":   map[string]any{"type": "string"},
		},
		"required":             []string{"status", "summary", "evidence", "nextSteps", "blocker"},
		"additionalProperties": false,
	}
}

// normalizedText mirrors the fixed script's text gate: non-empty and
// already trimmed.
func normalizedText(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok && len(text) > 0 && text == strings.TrimSpace(text)
}

// normalizedList mirrors the fixed script's list gate.
func normalizedList(value any) ([]string, bool) {
	raw, ok := value.([]any)
	if !ok {
		return nil, false
	}
	list := make([]string, 0, len(raw))
	for _, item := range raw {
		text, ok := normalizedText(item)
		if !ok {
			return nil, false
		}
		list = append(list, text)
	}
	return list, true
}

// decodeReport validates one structured child result (the fixed script's
// validateReport across the provider boundary). The value arrives decoded
// from the engine's JSON round-trip: only plain JSON shapes exist.
func decodeReport(value any, expectedStatus RalphRoundStatus, maxChars int64) (RalphRoundReport, error) {
	record, ok := value.(map[string]any)
	if !ok || sortedKeys(record) != "blocker,evidence,nextSteps,status,summary" {
		return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned a malformed round report")
	}
	rawStatus, _ := record["status"].(string)
	status := RalphRoundStatus(rawStatus)
	switch status {
	case StatusContinue, StatusComplete, StatusBlocked:
	default:
		return RalphRoundReport{}, fmt.Errorf("Ralph round report status is invalid")
	}
	if status != expectedStatus {
		return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned a malformed round report")
	}
	summary, okText := normalizedText(record["summary"])
	evidence, okEvidence := normalizedList(record["evidence"])
	nextSteps, okNext := normalizedList(record["nextSteps"])
	blocker, blockerIsString := record["blocker"].(string)
	if !okText || !okEvidence || !okNext || !blockerIsString || blocker != strings.TrimSpace(blocker) {
		return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned a malformed round report")
	}
	report := RalphRoundReport{
		Status: expectedStatus, Summary: summary,
		Evidence: evidence, NextSteps: nextSteps, Blocker: blocker,
	}
	if expectedStatus == StatusContinue && (len(report.NextSteps) == 0 || report.Blocker != "") {
		return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned an invalid continuing report")
	}
	if expectedStatus == StatusComplete &&
		(len(report.Evidence) == 0 || len(report.NextSteps) != 0 || report.Blocker != "") {
		return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned an invalid completion report")
	}
	if expectedStatus == StatusBlocked {
		if _, ok := normalizedText(report.Blocker); !ok {
			return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned an invalid blocked report")
		}
	}
	serialized, err := json.Marshal(report)
	if err != nil {
		return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned a malformed round report")
	}
	if int64(len(serialized)) > maxChars {
		return RalphRoundReport{}, fmt.Errorf("Ralph workflow returned an oversized handoff (%d > %d)", len(serialized), maxChars)
	}
	return report, nil
}

// sortedKeys renders one record's exact-key fingerprint.
func sortedKeys(record map[string]any) string {
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
