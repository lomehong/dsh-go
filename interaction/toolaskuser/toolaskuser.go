// Package toolaskuser ports packages/interaction/tool-ask-user: the
// model-facing Consumer of the user-questions capability seam. The tool
// pauses until a UI provider returns a human answer, then feeds that answer
// back into the agent loop as an ordinary tool result.
package toolaskuser

import (
	"encoding/json"
	"fmt"

	"dshgo/interaction/userquestions"
	"dshgo/llm"
	"dshgo/tools"
)

// Name is the tool's wire name.
const Name = "ask_user_question"

const description = "Ask the user a concise question when you need confirmation, a choice, or missing information before proceeding. " +
	"Send one or more questions, each with a stable id that will be echoed in the answer."

// PluginName is the message source plugin name used for any injected notices.
const PluginName = "tool-ask-user"

// Register defines the ask_user_question tool on the runtime (global
// registration). It returns the tool's registration disposer.
func Register(runtime *tools.ToolRuntime, questions *userquestions.Service) (func(), error) {
	openObject := func() *bool { value := true; return &value }
	closedObject := func() *bool { value := false; return &value }
	if runtime == nil {
		return nil, fmt.Errorf("toolaskuser: a tool runtime is required")
	}
	if questions == nil {
		return nil, fmt.Errorf("toolaskuser: a userquestions service is required")
	}
	tool, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        Name,
		Description: description,
		Parameters: map[string]tools.PropSpec{
			"questions": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type: "array",
					Items: &tools.ValueSchemaSpec{
						Type:                 "object",
						AdditionalProperties: openObject(),
						Properties: map[string]tools.PropSpec{
							"id": {ValueSchemaSpec: tools.ValueSchemaSpec{
								Type:        "string",
								Description: "Stable id for this question; echoed in the answer.",
							}, Required: true},
							"question": {ValueSchemaSpec: tools.ValueSchemaSpec{
								Type:        "string",
								Description: "The specific question to ask the user.",
							}, Required: true},
							"header": {ValueSchemaSpec: tools.ValueSchemaSpec{
								Type:        "string",
								Description: `Optional short heading for the question, such as "Confirm" or "Choose Mode".`,
							}},
							"options": {ValueSchemaSpec: tools.ValueSchemaSpec{
								Type: "array",
								Items: &tools.ValueSchemaSpec{
									Type:                 "object",
									AdditionalProperties: openObject(),
									Properties: map[string]tools.PropSpec{
										"label": {ValueSchemaSpec: tools.ValueSchemaSpec{
											Type:        "string",
											Description: "Short user-facing option label.",
										}, Required: true},
										"description": {ValueSchemaSpec: tools.ValueSchemaSpec{
											Type:        "string",
											Description: "One sentence explaining the tradeoff or impact.",
										}},
									},
								},
								Description: "Optional choices to show the user. If you recommend one, put it first and append \"(Recommended)\" to that label.",
							}},
							"multi_select": {ValueSchemaSpec: tools.ValueSchemaSpec{
								Type:        "boolean",
								Description: "Whether the user may select more than one option. Defaults to false.",
							}},
						},
					},
					Description: "Questions to ask the user before continuing.",
				},
				Required: true,
			},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: closedObject(),
				Properties: map[string]tools.PropSpec{
					"answers": {
						ValueSchemaSpec: tools.ValueSchemaSpec{
							Type: "array",
							Items: &tools.ValueSchemaSpec{
								Type:                 "object",
								AdditionalProperties: closedObject(),
								Properties: map[string]tools.PropSpec{
									"id":       {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
									"selected": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "array", Items: &tools.ValueSchemaSpec{Type: "string"}}, Required: true},
									"custom":   {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
								},
							},
						},
						Required: true,
					},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				encoded, err := json.Marshal(value)
				if err != nil {
					encoded = []byte("{}")
				}
				return []llm.ContentBlock{{Type: llm.BlockText, Text: string(encoded)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			raw, ok := args["questions"].([]any)
			if !ok {
				return nil, fmt.Errorf("ask_user_question requires a questions array")
			}
			parsed := make([]userquestions.AskUserQuestionItem, 0, len(raw))
			for index, entry := range raw {
				object, ok := entry.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("question %d must be an object", index)
				}
				item, err := parseQuestion(object)
				if err != nil {
					return nil, err
				}
				parsed = append(parsed, item)
			}
			request := userquestions.Request{Questions: parsed, Signal: exec.Signal}
			if caller := questions.AgentByScope(exec.Agent); caller != nil {
				request.Agent = caller
			}
			answer, err := questions.Ask(request)
			if err != nil {
				return nil, err
			}
			answers := make([]map[string]any, 0, len(answer.Answers))
			for _, item := range answer.Answers {
				encoded := map[string]any{"id": item.ID, "selected": append([]string(nil), item.Selected...)}
				if item.Custom != "" {
					encoded["custom"] = item.Custom
				}
				answers = append(answers, encoded)
			}
			return map[string]any{"answers": answers}, nil
		},
		IsConcurrencySafe: func(map[string]any) bool { return false },
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(tool)
}

// parseQuestion maps one model-supplied question object onto the seam's
// item, tolerating absent optional fields.
func parseQuestion(object map[string]any) (userquestions.AskUserQuestionItem, error) {
	id, _ := object["id"].(string)
	question, _ := object["question"].(string)
	if id == "" || question == "" {
		return userquestions.AskUserQuestionItem{}, fmt.Errorf(
			"each question requires a non-empty id and question")
	}
	item := userquestions.AskUserQuestionItem{ID: id, Question: question}
	if header, ok := object["header"].(string); ok {
		item.Header = header
	}
	if multiSelect, ok := object["multi_select"].(bool); ok {
		item.MultiSelect = multiSelect
	}
	if rawOptions, ok := object["options"].([]any); ok {
		options := make([]userquestions.AskUserQuestionOption, 0, len(rawOptions))
		for index, rawOption := range rawOptions {
			optionObject, ok := rawOption.(map[string]any)
			if !ok {
				return userquestions.AskUserQuestionItem{}, fmt.Errorf("question %q option %d must be an object", id, index)
			}
			label, _ := optionObject["label"].(string)
			if label == "" {
				return userquestions.AskUserQuestionItem{}, fmt.Errorf(
					"question %q option %d requires a non-empty label", id, index)
			}
			option := userquestions.AskUserQuestionOption{Label: label}
			if optionDescription, ok := optionObject["description"].(string); ok {
				option.Description = optionDescription
			}
			options = append(options, option)
		}
		item.Options = options
	}
	return item, nil
}
