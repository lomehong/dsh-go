package subagent

import (
	"dshgo/llm"
)

// Job outcome statuses (the `ctx.jobs` vocabulary the one-shot background
// path settles into; the jobs package itself lands with the boot round).
const (
	// JobStatusCompleted: the run produced final output.
	JobStatusCompleted = "completed"
	// JobStatusKilled: cancelled locally without a provider diagnosis.
	JobStatusKilled = "killed"
	// JobStatusFailed: provider-diagnosed abort or any other failure.
	JobStatusFailed = "failed"
)

// JobOutcome is the background-Task outcome one one-shot subagent run
// settles into.
type JobOutcome struct {
	// Status is one of JobStatusCompleted, JobStatusKilled, JobStatusFailed.
	Status string `json:"status"`
	// Output carries the child's final text on completion.
	Output string `json:"output,omitempty"`
	// Detail carries the failure rendering.
	Detail string `json:"detail,omitempty"`
}

// finalText flattens a child's final output blocks to the task's final
// text.
func finalText(blocks []llm.ContentBlock) string {
	text := ""
	for _, block := range blocks {
		if block.Type == llm.BlockText {
			text += block.Text
		}
	}
	return text
}

// failureDetail renders a failed stop reason with optional provider-authored
// detail.
func failureDetail(result SubagentResult) string {
	if result.Diagnostic == "" {
		return string(result.StopReason)
	}
	return string(result.StopReason) + "; diagnostic: " + result.Diagnostic
}

// RunOutcome maps a child result to the task outcome: completed carries
// final text, local cancellation (aborted without a diagnostic) is killed,
// and provider-diagnosed remote aborts plus every other reason are failed
// without partial output. Merge-extensible reasons remain failures with
// provider-authored detail.
func RunOutcome(result SubagentResult) JobOutcome {
	switch result.StopReason {
	case StopCompleted:
		return JobOutcome{Status: JobStatusCompleted, Output: finalText(result.Output)}
	case StopAborted:
		if result.Diagnostic == "" {
			return JobOutcome{Status: JobStatusKilled}
		}
		return JobOutcome{Status: JobStatusFailed, Detail: failureDetail(result)}
	default:
		return JobOutcome{Status: JobStatusFailed, Detail: failureDetail(result)}
	}
}

// SettleRun awaits the child result, disposes the run, then returns its
// task outcome. Result and disposal failures become failed; when both fail,
// both details survive.
func SettleRun(run SubagentRun) JobOutcome {
	var outcome JobOutcome
	result, err := run.Result()
	if err != nil {
		outcome = JobOutcome{Status: JobStatusFailed, Detail: errorString(err)}
	} else {
		outcome = RunOutcome(result)
	}
	if err := run.Dispose(); err != nil {
		prefix := ""
		if outcome.Detail != "" {
			prefix = outcome.Detail + "; "
		}
		return JobOutcome{Status: JobStatusFailed, Detail: prefix + "dispose failed: " + errorString(err)}
	}
	return outcome
}

// errorString renders one error for a job detail surface.
func errorString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
