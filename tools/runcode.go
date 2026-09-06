// PTC run_code tool: the model-facing JavaScript execution tool that
// replaces individual native tool schemas in PTC presentation mode.
package tools

import (
	"fmt"
)

// RegisterRunCode creates and registers the run_code tool bound to the
// provided code runtime. In PTC mode, run_code is the ONLY tool the model
// sees; it composes multi-tool workflows in one round trip.
func (rt *ToolRuntime) RegisterRunCode(cr CodeRuntime) (func(), error) {
	if cr == nil {
		return nil, fmt.Errorf("run_code: code runtime is required")
	}
	rt.SetCodeRuntime(cr)
	def, err := DefineTool(DefineToolOptions{
		Name:        ReservedRunCodeName,
		Description: "Execute a JavaScript program that can call other tools via the tools global. Returns the completion value.",
		Parameters: map[string]PropSpec{
			"code": {ValueSchemaSpec: ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Execute: func(args map[string]any, exec *ToolRunContext) (any, error) {
			code, _ := args["code"].(string)
			if code == "" {
				return errorResult("run_code requires a non-empty code string"), nil
			}
			value, runErr := cr.Run(CodeRunRequest{Program: code})
			if runErr != nil {
				return nil, runErr
			}
			return map[string]any{"result": value}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return rt.Register(def)
}

func errorResult(text string) map[string]any {
	return map[string]any{"error": map[string]any{"message": text}}
}
