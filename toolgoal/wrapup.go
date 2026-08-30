package toolgoal

import (
	"encoding/json"

	"dshgo/llm"
)

const grounding = "Report only what earlier rounds and tool results in this session actually establish; " +
	"when a detail is not in the session, say so instead of inventing it. "

// RenderWrapupContext renders the closing-message instruction injected after
// an autonomous goal round reports `complete` or `blocked`, replacing the
// former hard turn stop so the model still addresses the user once before the
// turn ends. The objective echoes for grounding; blockedReason is the
// validated report for `blocked`, omitted for `complete`. The result is a
// fresh one-block context for ToolRunContext.DeferContext.
func RenderWrapupContext(objective string, blockedReason *string) []llm.ContentBlock {
	objectiveJSON, _ := json.Marshal(objective)
	heading := "Objective: " + string(objectiveJSON) + "\n"
	var text string
	if blockedReason == nil {
		text = "<goal_complete>\n" +
			heading +
			"The goal is marked complete and this autonomous run is ending. Write the closing " +
			"message to the user now: state the outcome, summarize what was done and how it was " +
			"verified, and point to the concrete results (files, commits, or other artifacts). " +
			grounding +
			"Note anything the user should review or do next. Address the user directly. Do not " +
			"call any more tools in this run; further work waits for the user's next instruction.\n" +
			"</goal_complete>"
	} else {
		reasonJSON, _ := json.Marshal(*blockedReason)
		text = "<goal_blocked>\n" +
			heading +
			"Blocked: " + string(reasonJSON) + "\n" +
			"The goal is marked blocked and this autonomous run is ending. Write the closing " +
			"message to the user now: state what has been completed so far, describe the concrete " +
			"blocking condition and what you tried, and say exactly what you need from the user to " +
			"continue. " +
			grounding +
			"Address the user directly. Do not call any more tools in this run; further work " +
			"waits for the user's next instruction.\n" +
			"</goal_blocked>"
	}
	return []llm.ContentBlock{{Type: llm.BlockText, Text: text}}
}
