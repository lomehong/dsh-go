// Package toolresultpruner ports @deepseek-ai/dsh-compaction-tool-result-pruner:
// the replay-safe, model-free pruning service for over-budget tool-result
// surface nodes. Each replacement cites the shadowed event and is priced by
// a compaction/prune shadow-price event appended synchronously adjacent, so
// pure consumers subtract the shadowed node's heuristic price without
// per-node state.
package toolresultpruner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// PruneMarker is the fixed marker substituted for every removed middle
// span.
const PruneMarker = "\n\n[... tool result middle pruned ...]\n\n"

// Defaults are the low-friction budgets for coding-agent tool output.
var Defaults = ResolvedConfig{
	ThresholdChars: 8192,
	HeadChars:      4096,
	TailChars:      1024,
}

// ResolvedConfig is the validated, detached character-budget policy.
type ResolvedConfig struct {
	// ThresholdChars prunes when total text exceeds this many Unicode
	// code points.
	ThresholdChars int
	// HeadChars is the maximum leading Unicode code points retained.
	HeadChars int
	// TailChars is the maximum trailing Unicode code points retained.
	TailChars int
}

// Config is the raw plugin configuration; nil fields fall back to
// Defaults.
type Config struct {
	ThresholdChars *int `json:"thresholdChars,omitempty"`
	HeadChars      *int `json:"headChars,omitempty"`
	TailChars      *int `json:"tailChars,omitempty"`
}

// DecodeConfig strictly decodes the raw plugin configuration, rejecting
// unknown keys fail-loud.
func DecodeConfig(raw any) (Config, error) {
	var config Config
	if raw == nil {
		return config, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return config, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("ToolResultPruneConfig: %w", err)
	}
	return config, nil
}

// ResolveConfig validates the pruning budgets: headChars and tailChars are
// non-negative, thresholdChars positive, and the emitted replacement
// (head + marker + tail) must fit the threshold.
func ResolveConfig(config Config) (ResolvedConfig, error) {
	resolved := Defaults
	if config.ThresholdChars != nil {
		resolved.ThresholdChars = *config.ThresholdChars
	}
	if config.HeadChars != nil {
		resolved.HeadChars = *config.HeadChars
	}
	if config.TailChars != nil {
		resolved.TailChars = *config.TailChars
	}
	if resolved.ThresholdChars <= 0 {
		return ResolvedConfig{}, fmt.Errorf(
			"ToolResultPruneConfig: thresholdChars (%d) must be a positive integer", resolved.ThresholdChars)
	}
	if resolved.HeadChars < 0 {
		return ResolvedConfig{}, fmt.Errorf(
			"ToolResultPruneConfig: headChars (%d) must be a non-negative integer", resolved.HeadChars)
	}
	if resolved.TailChars < 0 {
		return ResolvedConfig{}, fmt.Errorf(
			"ToolResultPruneConfig: tailChars (%d) must be a non-negative integer", resolved.TailChars)
	}
	emittedChars := resolved.HeadChars + CodePointLength(PruneMarker) + resolved.TailChars
	if emittedChars > resolved.ThresholdChars {
		return ResolvedConfig{}, fmt.Errorf(
			"ToolResultPruneConfig: headChars + marker + tailChars (%d) must be at most thresholdChars (%d)",
			emittedChars, resolved.ThresholdChars)
	}
	return resolved, nil
}

// CodePointLength measures text in Unicode code points without splitting
// surrogate pairs.
func CodePointLength(text string) int {
	return utf8.RuneCountInString(text)
}
