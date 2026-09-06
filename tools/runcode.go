// PTC run_code tool: the model-facing JavaScript execution tool that
// replaces individual native tool schemas in PTC presentation mode. Host
// tools are exposed as async functions on the `tools` global, so one
// run_code call composes multi-tool workflows in a single round trip.
package tools

import (
	"encoding/json"
	"fmt"

	"dshgo/coderuntime"
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
			bindings := rt.buildToolBindings(exec)
			result, runErr := cr.Run(CodeRunRequest{
				Program:  code,
				Bindings: bindings,
				Signal:   exec.Signal,
			})
			if runErr != nil {
				return nil, runErr
			}
			if result.Error != nil {
				return errorResult(fmt.Sprintf("[%s] %s", result.Error.Kind, result.Error.Message)), nil
			}
			return map[string]any{"result": result.Value}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return rt.Register(def)
}

// buildToolBindings constructs the `tools` global's function table: every
// visible tool (except run_code itself) becomes an async function that
// dispatches through the tool's Execute body with the parent's execution
// context (signal + scope carry through).
func (rt *ToolRuntime) buildToolBindings(exec *ToolRunContext) []coderuntime.CodeBindingNamespace {
	rt.mu.Lock()
	view := rt.viewLocked(exec.Agent)
	functions := make(map[string]coderuntime.CodeBindingFunction)
	for name, def := range view.visible {
		if name == ReservedRunCodeName {
			continue
		}
		toolDef := def
		functions[name] = func(args coderuntime.CodeJSONValue) (coderuntime.CodeJSONValue, error) {
			var toolArgs map[string]any
			if b, marshalErr := json.Marshal(args); marshalErr == nil {
				json.Unmarshal(b, &toolArgs)
			}
			if toolArgs == nil {
				toolArgs = map[string]any{}
			}
			value, execErr := toolDef.Execute(toolArgs, exec)
			if execErr != nil {
				return nil, execErr
			}
			return value, nil
		}
	}
	rt.mu.Unlock()
	return []coderuntime.CodeBindingNamespace{
		{Global: "tools", Functions: functions},
	}
}

func errorResult(text string) map[string]any {
	return map[string]any{"error": map[string]any{"message": text}}
}
