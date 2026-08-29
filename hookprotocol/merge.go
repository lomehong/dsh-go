// Merge ports hook-protocol/src/merge.ts: merge matched hooks into one
// most-restrictive outcome. Permission precedence is deny > ask > allow;
// the first continue:false stop is sticky; reasons for the winning rank are
// joined; and context and system messages accumulate in hook order.
package hookprotocol

import "strings"

// MergedDecision is the single decision a hook point resolves to after
// merging all matched hooks.
type MergedDecision string

// The merged decision values.
const (
	MergedAllow MergedDecision = "allow"
	MergedAsk   MergedDecision = "ask"
	MergedDeny  MergedDecision = "deny"
	MergedNone  MergedDecision = "none"
)

// MergedHookOutcome is the folded outcome of every hook that matched one
// point.
type MergedHookOutcome struct {
	// Decision is the most-restrictive permission decision across all hooks
	// (deny > ask > allow), or "none" when no hook expressed one.
	// block/deny both fold to deny; approve/allow both fold to allow.
	Decision MergedDecision
	// Reason joins ("\n\n") the reasons from every blocking/denying hook
	// that matches the winning rank; empty when none.
	Reason string
	// Stop is true when any hook asked to halt (continue:false).
	Stop bool
	// StopReason is the first halting hook's stopReason, when one halted.
	StopReason string
	// AdditionalContext holds every hook's additionalContext, in hook order
	// (no joining — the bridge decides).
	AdditionalContext []string
	// SystemMessages holds every hook's systemMessage, in hook order.
	SystemMessages []string
}

// rankOf ranks a single hook's decision for the deny>ask>allow precedence
// (higher = stricter).
func rankOf(decision HookDecision) int {
	switch decision {
	case DecisionDeny, DecisionBlock:
		return 3
	case DecisionAsk:
		return 2
	case DecisionApprove, DecisionAllow:
		return 1
	default:
		return 0 // no decision
	}
}

// decisionForRank collapses a ranked decision back to the merged enum.
func decisionForRank(maxRank int) MergedDecision {
	switch maxRank {
	case 3:
		return MergedDeny
	case 2:
		return MergedAsk
	case 1:
		return MergedAllow
	default:
		return MergedNone
	}
}

// MergeHookOutputs folds outputs (the results of every hook that matched a
// point, in hook order) into one MergedHookOutcome by the precedence rules
// above. An empty list yields a neutral outcome (decision "none", no stop,
// empty context) — the caller treats that as "no hook had anything to say".
func MergeHookOutputs(outputs []HookOutput) MergedHookOutcome {
	maxRank := 0
	// Keep reasons per rank so only objections explaining the winning
	// decision surface.
	reasonsByRank := map[int][]string{}
	var ranks []int
	stop := false
	stopReason := ""
	additionalContext := []string{}
	systemMessages := []string{}

	for _, out := range outputs {
		r := rankOf(out.Decision)
		if r > maxRank {
			maxRank = r
		}
		if (r == 3 || r == 2) && out.Reason != nil && *out.Reason != "" {
			if _, seen := reasonsByRank[r]; !seen {
				ranks = append(ranks, r)
			}
			reasonsByRank[r] = append(reasonsByRank[r], *out.Reason)
		}
		if out.Continue != nil && !*out.Continue && !stop {
			stop = true
			if out.StopReason != nil {
				stopReason = *out.StopReason
			}
		}
		if out.AdditionalContext != "" {
			additionalContext = append(additionalContext, out.AdditionalContext)
		}
		if out.SystemMessage != "" {
			systemMessages = append(systemMessages, out.SystemMessage)
		}
	}

	merged := MergedHookOutcome{
		Decision:          decisionForRank(maxRank),
		Stop:              stop,
		StopReason:        stopReason,
		AdditionalContext: additionalContext,
		SystemMessages:    systemMessages,
	}
	if reasons := reasonsByRank[maxRank]; len(reasons) > 0 {
		merged.Reason = strings.Join(reasons, "\n\n")
	}
	return merged
}
