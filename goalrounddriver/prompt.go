// Model-visible continuation prompt for one same-session goal round
// (official dsh-goal-round-driver prompt.ts).
package goalrounddriver

import (
	"encoding/json"
	"fmt"

	"dshgo/goal"
	"dshgo/llm"
)

// RenderGoalRoundPrompt renders the complete goal-round instruction retained
// in session history: a fresh one-block prompt for the agent's follow-up
// queue. The wording is the official prompt verbatim; the objective rides as
// JSON exactly like JSON.stringify.
func RenderGoalRoundPrompt(view *goal.GoalView, round int64) []llm.ContentBlock {
	objective, _ := json.Marshal(view.Objective)
	return []llm.ContentBlock{{
		Type: llm.BlockText,
		Text: "<goal_round>\n" +
			fmt.Sprintf("Objective: %s\n", objective) +
			fmt.Sprintf("Round: %d/%d\n\n", round, view.MaxGoalRounds) +
			"Continue working toward the objective in this same session. Treat the current workspace, " +
			"tool results, and durable session state as authoritative; inspect them instead of assuming " +
			"earlier narration is still current. Make concrete progress and verify the result. Before " +
			"claiming completion, gather evidence that the whole objective is achieved, read the current " +
			"goal, and mark it complete. If work remains, leave the goal active for the next round. Follow " +
			"the configured goal-tool policy before reporting a blocker.\n" +
			"</goal_round>",
	}}
}
