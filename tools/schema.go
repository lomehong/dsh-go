// Unified JSON-value schema DSL, compilation, and typed tool helper.
//
// Port of packages/core/tools/src/schema.ts. Go adaptations: the author DSL
// is typed structs instead of TS object literals (unknown author keys are a
// compile error rather than a runtime authorError); `required: true` is the
// PropertySpec.Required field, so it cannot appear where the source rejects
// it. Property ordering is sorted-key (JS: insertion order) so compiled
// schemas and violation lists are deterministic.
package tools

import (
	"fmt"
	"sort"

	"dshgo/llm"
)

// ValueSchemaSpec is one author-facing schema node for any lossless JSON
// value root. Type selects the fields in play; OneOf replaces Type and
// requires at least two branches; Type "json" is the unconstrained
// annotation-only node; Type "object" requires an explicit
// AdditionalProperties choice so a nested or output object never acquires an
// accidental open default.
type ValueSchemaSpec struct {
	Type                 string              `json:"type,omitempty"`
	OneOf                []*ValueSchemaSpec  `json:"oneOf,omitempty"`
	Properties           map[string]PropSpec `json:"properties,omitempty"`
	AdditionalProperties *bool               `json:"additionalProperties,omitempty"`
	Items                *ValueSchemaSpec    `json:"items,omitempty"`
	Enum                 []any               `json:"enum,omitempty"`
	Const                any                 `json:"const,omitempty"`
	Description          string              `json:"description,omitempty"`
	Title                string              `json:"title,omitempty"`
	// Default and Examples are non-validating annotations. A nil value means
	// absent — the Go DSL cannot express a literal JSON-null annotation
	// (encode such defaults through a wrapping string field instead).
	Default  any `json:"default,omitempty"`
	Examples any `json:"examples,omitempty"`
}

// PropSpec is one implicit parameter-root property, optionally required.
type PropSpec struct {
	ValueSchemaSpec
	Required bool `json:"required,omitempty"`
}

// ToolArgsError reports model-generated arguments that failed their declared
// parameter schema. Violations lists every offense in schema-walk order.
type ToolArgsError struct {
	Violations []string
}

func (e *ToolArgsError) Error() string {
	return fmt.Sprintf("invalid arguments: %s", joinViolations(e.Violations))
}

// Code returns INVALID_ARGS.
func (e *ToolArgsError) Code() string { return CodeInvalidArgs }

func joinViolations(violations []string) string {
	out := ""
	for i, violation := range violations {
		if i > 0 {
			out += "; "
		}
		out += violation
	}
	return out
}

func authorError(message string) error {
	return &JsonSchemaError{Violations: []string{message}}
}

// copyAnnotations copies the non-zero annotation fields into the compiled
// node (the JSON own-key checks of the source become zero-value checks).
func copyAnnotations(spec *ValueSchemaSpec, node map[string]any) {
	if spec.Description != "" {
		node["description"] = spec.Description
	}
	if spec.Title != "" {
		node["title"] = spec.Title
	}
	if spec.Default != nil {
		node["default"] = spec.Default
	}
	if spec.Examples != nil {
		node["examples"] = spec.Examples
	}
}

// compileValueSpec compiles one author node without applying any consumer
// root restriction. The Go call stack replaces the source's explicit task
// graph; cycle detection keeps self-referential specs from looping.
func compileValueSpec(spec *ValueSchemaSpec, path string, allowRequired bool, seen map[*ValueSchemaSpec]bool) (map[string]any, error) {
	if spec == nil {
		return nil, authorError(fmt.Sprintf("%s must be a value schema object", path))
	}
	if seen[spec] {
		return nil, authorError(fmt.Sprintf("%s is circular", path))
	}
	seen[spec] = true
	defer delete(seen, spec)

	node := map[string]any{}
	if len(spec.OneOf) > 0 {
		if spec.Type != "" {
			return nil, authorError(fmt.Sprintf("%s cannot declare both type and oneOf", path))
		}
		if len(spec.OneOf) < 2 {
			return nil, authorError(fmt.Sprintf("%s.oneOf must be an array of at least two value schemas", path))
		}
		branches := make([]any, 0, len(spec.OneOf))
		copyAnnotations(spec, node)
		for index, branch := range spec.OneOf {
			compiled, err := compileValueSpec(branch, fmt.Sprintf("%s.oneOf[%d]", path, index), false, seen)
			if err != nil {
				return nil, err
			}
			branches = append(branches, compiled)
		}
		node["oneOf"] = branches
		return node, nil
	}
	if spec.Type == "" {
		return nil, authorError(fmt.Sprintf("%s.type must be string/number/integer/boolean/null/array/object/json, or use oneOf", path))
	}

	switch spec.Type {
	case "json":
		copyAnnotations(spec, node)
	case "object":
		if spec.AdditionalProperties == nil {
			return nil, authorError(fmt.Sprintf("%s.additionalProperties must be explicitly true or false", path))
		}
		node["type"] = "object"
		copyAnnotations(spec, node)
		node["additionalProperties"] = *spec.AdditionalProperties
		if spec.Properties != nil {
			compiled, err := compilePropertyMap(spec.Properties, fmt.Sprintf("%s.properties", path), seen)
			if err != nil {
				return nil, err
			}
			node["properties"] = compiled.properties
			if len(compiled.required) > 0 {
				required := make([]any, 0, len(compiled.required))
				for _, name := range compiled.required {
					required = append(required, name)
				}
				node["required"] = required
			}
		}
	case "array":
		node["type"] = "array"
		copyAnnotations(spec, node)
		if spec.Items != nil {
			compiled, err := compileValueSpec(spec.Items, fmt.Sprintf("%s.items", path), false, seen)
			if err != nil {
				return nil, err
			}
			node["items"] = compiled
		}
	case "string", "number", "integer", "boolean", "null":
		node["type"] = spec.Type
		copyAnnotations(spec, node)
		if spec.Enum != nil {
			// Non-empty is the author-layer gate; entry scalar-type matching
			// is enforced by AssertSupportedJsonSchema below.
			if len(spec.Enum) == 0 {
				return nil, authorError(fmt.Sprintf("%s.enum must be a non-empty array of scalar values", path))
			}
			node["enum"] = spec.Enum
		}
		if spec.Const != nil {
			if !scalarMatches(spec.Type, spec.Const) {
				return nil, authorError(fmt.Sprintf("%s.const must be a %s value", path, spec.Type))
			}
			node["const"] = spec.Const
		}
	default:
		return nil, authorError(fmt.Sprintf("%s.type must be string/number/integer/boolean/null/array/object/json, or use oneOf", path))
	}
	return node, nil
}

// compiledPropertyMap is the compiled form of one implicit property map.
type compiledPropertyMap struct {
	properties map[string]any
	required   []string
}

// compilePropertyMap compiles one implicit property map, collecting
// per-property requiredness in sorted-key order.
func compilePropertyMap(spec map[string]PropSpec, path string, seen map[*ValueSchemaSpec]bool) (*compiledPropertyMap, error) {
	if spec == nil {
		return nil, authorError(fmt.Sprintf("%s must be an object of value schemas", path))
	}
	compiled := &compiledPropertyMap{properties: map[string]any{}}
	for _, key := range sortedPropKeys(spec) {
		property := spec[key]
		// The typed DSL makes `required` true-or-absent by construction;
		// nested specs carry no Required field at all.
		if property.Required {
			compiled.required = append(compiled.required, key)
		}
		node, err := compileValueSpec(&property.ValueSchemaSpec, fmt.Sprintf("%s.%s", path, key), true, seen)
		if err != nil {
			return nil, err
		}
		compiled.properties[key] = node
	}
	return compiled, nil
}

func sortedPropKeys(spec map[string]PropSpec) []string {
	keys := make([]string, 0, len(spec))
	for key := range spec {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ValueSchemaToJSONSchema compiles one author-facing value schema to the
// enforced raw JSON Schema subset. The author-only `json` node becomes an
// annotation-only schema. The result passes AssertSupportedJsonSchema.
func ValueSchemaToJSONSchema(spec *ValueSchemaSpec) (map[string]any, error) {
	schema, err := compileValueSpec(spec, "schema", false, map[*ValueSchemaSpec]bool{})
	if err != nil {
		return nil, err
	}
	if err := AssertSupportedJsonSchema(schema); err != nil {
		return nil, err
	}
	return schema, nil
}

// ParameterSchemaToJSONSchema compiles the implicit open parameter object
// into raw JSON Schema: an object-rooted schema with no implicit-root
// openness override.
func ParameterSchemaToJSONSchema(spec map[string]PropSpec) (map[string]any, error) {
	compiled, err := compilePropertyMap(spec, "parameters", map[*ValueSchemaSpec]bool{})
	if err != nil {
		return nil, err
	}
	schema := map[string]any{
		"type":       "object",
		"properties": compiled.properties,
	}
	if len(compiled.required) > 0 {
		required := make([]any, 0, len(compiled.required))
		for _, name := range compiled.required {
			required = append(required, name)
		}
		schema["required"] = required
	}
	if err := AssertSupportedJsonSchema(schema); err != nil {
		return nil, err
	}
	return schema, nil
}

// ValidateArgs validates model-generated arguments against an implicit
// parameter schema. Returns path-qualified violations; empty means valid.
func ValidateArgs(spec map[string]PropSpec, args any) []string {
	schema, err := ParameterSchemaToJSONSchema(spec)
	if err != nil {
		// An author schema that cannot compile is a developer error, not a
		// model error; surface it as the schema rejection it is.
		return []string{err.Error()}
	}
	return ValidateJsonSchemaValue(schema, args, "")
}

// ToolOutput declares the canonical output schema plus the pure Native
// render of one validated value.
type ToolOutput struct {
	Schema *ValueSchemaSpec
	Render func(args map[string]any, value any) []llm.ContentBlock
}

// DefineToolOptions carries one DefineTool call.
type DefineToolOptions struct {
	Name        string
	Description string
	Parameters  map[string]PropSpec
	Output      ToolOutput
	// TimeoutMs is an optional positive cooperative timeout budget in
	// milliseconds; zero means none.
	TimeoutMs float64
	// Execute runs after argument validation and returns the canonical
	// value declared by Output.Schema.
	Execute func(args map[string]any, exec *ToolRunContext) (any, error)
	// IsConcurrencySafe is the pure sibling-overlap classifier.
	IsConcurrencySafe func(args map[string]any) bool
	// PresentationMeta projects the replayable presentation payload for
	// direct top-level calls.
	PresentationMeta func(args map[string]any, value any) any
	// FinalizeContent transforms every normalized outcome's content.
	FinalizeContent func(exec *ToolExecution, result *ToolExecutionResult) []llm.ContentBlock
}

// ToolDefinition is a registry-ready tool definition.
type ToolDefinition struct {
	Name         string
	Description  string
	Parameters   map[string]any
	OutputSchema map[string]any
	TimeoutMs    float64
	// PresentationMeta projects one validated canonical value into the
	// replayable presentation payload for direct top-level calls.
	PresentationMeta func(args map[string]any, value any) any
	// FinalizeContent is the last-mile content transform for every
	// normalized outcome; arguments stay raw because invalid-input failures
	// also reach it.
	FinalizeContent func(exec *ToolExecution, result *ToolExecutionResult) []llm.ContentBlock

	render            func(args map[string]any, value any) []llm.ContentBlock
	validate          func(args any) []string
	execute           func(args map[string]any, exec *ToolRunContext) (any, error)
	isConcurrencySafe func(args map[string]any) bool
}

// DefineTool defines a first-party tool with strict execution validation:
// compile-time author errors fail loud here, execute-time argument errors
// surface as ToolArgsError, and the concurrency classifier defaults to
// rejecting arguments it cannot validate.
func DefineTool(options DefineToolOptions) (*ToolDefinition, error) {
	if options.TimeoutMs != 0 && (options.TimeoutMs <= 0 || options.TimeoutMs != options.TimeoutMs) {
		return nil, fmt.Errorf("defineTool(%s): timeoutMs must be a positive finite number", options.Name)
	}
	parameters, err := ParameterSchemaToJSONSchema(options.Parameters)
	if err != nil {
		return nil, err
	}
	outputSchema, err := ValueSchemaToJSONSchema(options.Output.Schema)
	if err != nil {
		return nil, err
	}
	tool := &ToolDefinition{
		Name:              options.Name,
		Description:       options.Description,
		Parameters:        parameters,
		OutputSchema:      outputSchema,
		TimeoutMs:         options.TimeoutMs,
		PresentationMeta:  options.PresentationMeta,
		FinalizeContent:   options.FinalizeContent,
		render:            options.Output.Render,
		execute:           options.Execute,
		isConcurrencySafe: options.IsConcurrencySafe,
	}
	tool.validate = func(args any) []string {
		return ValidateJsonSchemaValue(parameters, args, "")
	}
	return tool, nil
}

// ValidateArgsValue validates candidate arguments against this tool's
// compiled parameter schema.
func (t *ToolDefinition) ValidateArgsValue(args any) []string {
	return t.validate(args)
}

// Execute validates arguments and runs the tool body. Invalid arguments
// return a ToolArgsError without entering the body.
func (t *ToolDefinition) Execute(args any, exec *ToolRunContext) (any, error) {
	if violations := t.validate(args); len(violations) > 0 {
		return nil, &ToolArgsError{Violations: violations}
	}
	typed, _ := args.(map[string]any)
	return t.execute(typed, exec)
}

// Render projects one validated canonical value into model-facing content.
func (t *ToolDefinition) Render(args map[string]any, value any) []llm.ContentBlock {
	return t.render(args, value)
}

// IsConcurrencySafe classifies sibling overlap; invalid arguments classify
// as unsafe (the source's soft-validation wrapper).
func (t *ToolDefinition) IsConcurrencySafe(args map[string]any) bool {
	if t.isConcurrencySafe == nil {
		return false
	}
	return t.isConcurrencySafe(args)
}
