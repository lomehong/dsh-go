package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/llm"
)

func assertViolations(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("violations = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("violation[%d] = %q, want %q (all: %v)", i, got[i], want[i], want)
		}
	}
}

// --- enforced-subset assertion -------------------------------------------------

func TestAssertRejectsUnsupportedKeywords(t *testing.T) {
	err := AssertSupportedJsonSchema(map[string]any{"type": "string", "minLength": 2})
	var schemaErr *JsonSchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("err = %v", err)
	}
	assertViolations(t, schemaErr.Violations, []string{"schema.minLength is not a supported keyword (subset: type/oneOf/properties/required/additionalProperties/items/enum/const + annotations)"})
	if schemaErr.Code() != "UNSUPPORTED_SCHEMA" {
		t.Fatalf("code = %q", schemaErr.Code())
	}
}

func TestAssertRejectsStructuralViolations(t *testing.T) {
	cases := []struct {
		name   string
		schema any
		want   string
	}{
		{"type array", map[string]any{"type": []any{"string", "null"}}, "schema.type must be a single type string (type arrays are not supported)"},
		{"bad type", map[string]any{"type": "strung"}, "schema.type must be one of object/array/string/number/integer/boolean/null"},
		{"type + oneOf", map[string]any{"type": "string", "oneOf": []any{}}, "schema cannot declare both type and oneOf"},
		{"no type no oneOf", map[string]any{"properties": map[string]any{}}, "schema.properties requires type or oneOf"},
		{"oneOf singleton", map[string]any{"oneOf": []any{map[string]any{"type": "null"}}}, "schema.oneOf must be an array of at least two schemas"},
		{"sibling beside oneOf", map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "string"}}, "enum": []any{"a"}}, "schema.enum is not supported beside oneOf"},
		{"required undeclared", map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{"missing"}}, `schema.required names "missing" which is not in properties`},
		{"additionalProperties type", map[string]any{"type": "object", "additionalProperties": "no"}, "schema.additionalProperties must be a boolean"},
		{"enum on object", map[string]any{"type": "object", "enum": []any{"a"}}, `schema.enum is not supported on type "object"`},
		{"enum empty", map[string]any{"type": "string", "enum": []any{}}, "schema.enum must be a non-empty array of string values"},
		{"enum wrong scalar", map[string]any{"type": "string", "enum": []any{1}}, "schema.enum must be a non-empty array of string values"},
		{"const wrong type", map[string]any{"type": "number", "const": "x"}, `schema.const must be a number value`},
		{"const outside enum", map[string]any{"type": "string", "enum": []any{"a"}, "const": "b"}, "schema.const must be one of schema.enum when both are declared"},
		{"nested node", map[string]any{"type": "array", "items": "nope"}, "schema.items must be a schema object"},
		{"misplaced items", map[string]any{"type": "string", "items": map[string]any{}}, `schema.items is not supported on type "string"`},
		{"annotation not JSON", map[string]any{"type": "string", "default": func() {}}, "schema.default annotation must be lossless JSON data"},
		{"description not string", map[string]any{"type": "string", "description": 3}, "schema.description must be a string"},
		{"root not a record", "string", "schema must be a schema object"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := AssertSupportedJsonSchema(testCase.schema)
			var schemaErr *JsonSchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("err = %v", err)
			}
			if len(schemaErr.Violations) != 1 || schemaErr.Violations[0] != testCase.want {
				t.Fatalf("violations = %v, want [%s]", schemaErr.Violations, testCase.want)
			}
		})
	}
}

func TestAssertAcceptsAnnotationOnlyAndNestedSubsets(t *testing.T) {
	// Annotation-only: the unconstrained-JSON form.
	if err := AssertSupportedJsonSchema(map[string]any{"description": "anything"}); err != nil {
		t.Fatalf("annotation-only: %v", err)
	}
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"mode":  map[string]any{"type": "string", "enum": []any{"fast", "slow"}},
			"count": map[string]any{"type": "integer", "const": 3},
			"any":   map[string]any{"default": map[string]any{"k": "v"}},
			"pair":  map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}},
		},
		"required":             []any{"mode"},
		"additionalProperties": false,
	}
	if err := AssertSupportedJsonSchema(nested); err != nil {
		t.Fatalf("nested: %v", err)
	}
	if err := AssertObjectJsonSchema(nested); err != nil {
		t.Fatalf("object root: %v", err)
	}
	// Object-root assertion rejects a scalar root.
	if err := AssertObjectJsonSchema(map[string]any{"type": "string"}); err == nil ||
		!strings.Contains(err.Error(), `schema.type must be "object" (structured output is object-rooted)`) {
		t.Fatalf("scalar root err = %v", err)
	}
}

func TestAssertRejectsCircularSchema(t *testing.T) {
	circular := map[string]any{"type": "object"}
	circular["properties"] = map[string]any{"self": circular}
	err := AssertSupportedJsonSchema(circular)
	if err == nil || !strings.Contains(err.Error(), "schema.properties.self is circular") {
		t.Fatalf("err = %v", err)
	}
}

// --- value validation ------------------------------------------------------------

func TestValidateValueObjects(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"size": map[string]any{"type": "integer"},
		},
		"required":             []any{"name"},
		"additionalProperties": false,
	}
	if got := ValidateJsonSchemaValue(schema, map[string]any{"name": "x", "size": 2}, ""); len(got) != 0 {
		t.Fatalf("valid rejected: %v", got)
	}
	got := ValidateJsonSchemaValue(schema, map[string]any{"size": 1.5, "extra": true}, "")
	// Root-level children keep bare keys (propertyPath drops the empty root).
	assertViolations(t, got, []string{
		`missing required property "name"`,
		`"size" must be an integer`,
		`"extra" is not a declared property (additionalProperties: false)`,
	})
	// A non-object value against an object schema.
	got = ValidateJsonSchemaValue(schema, "nope", "value")
	assertViolations(t, got, []string{`"value" must be an object`})
}

func TestValidateValueArraysAndOneOf(t *testing.T) {
	arraySchema := map[string]any{"type": "array", "items": map[string]any{"type": "number"}}
	got := ValidateJsonSchemaValue(arraySchema, []any{1, "x", 3}, "value")
	assertViolations(t, got, []string{`"value[1]" must be a number`})
	got = ValidateJsonSchemaValue(arraySchema, 7, "value")
	assertViolations(t, got, []string{`"value" must be an array`})

	oneOf := map[string]any{"oneOf": []any{
		map[string]any{"type": "null"},
		map[string]any{"type": "string", "enum": []any{"a", "b"}},
	}}
	if got := ValidateJsonSchemaValue(oneOf, "a", "value"); len(got) != 0 {
		t.Fatalf("oneOf match rejected: %v", got)
	}
	got = ValidateJsonSchemaValue(oneOf, "z", "value")
	assertViolations(t, got, []string{`"value" must match exactly one oneOf branch (matched 0)`})
	// 7 matches both branches: still a failure.
	both := map[string]any{"oneOf": []any{map[string]any{}, map[string]any{}}}
	got = ValidateJsonSchemaValue(both, true, "value")
	assertViolations(t, got, []string{`"value" must match exactly one oneOf branch (matched 2)`})
}

func TestValidateValueScalarsAndLosslessGate(t *testing.T) {
	if got := ValidateJsonSchemaValue(map[string]any{"type": "number"}, 1.5, ""); len(got) != 0 {
		t.Fatalf("number rejected: %v", got)
	}
	if got := ValidateJsonSchemaValue(map[string]any{"type": "integer"}, float64(4), ""); len(got) != 0 {
		t.Fatalf("integer-as-float rejected: %v", got)
	}
	got := ValidateJsonSchemaValue(map[string]any{"type": "null"}, false, "value")
	assertViolations(t, got, []string{`"value" must be null`})

	// The empty schema accepts any lossless JSON value.
	if got := ValidateJsonSchemaValue(map[string]any{}, map[string]any{"k": []any{1, nil}}, ""); len(got) != 0 {
		t.Fatalf("unconstrained rejected: %v", got)
	}
	// A channel is not a lossless JSON value.
	got = ValidateJsonSchemaValue(map[string]any{}, make(chan int), "value")
	assertViolations(t, got, []string{`"value" must be a lossless JSON value`})
}

// --- author DSL ------------------------------------------------------------------

func TestValueSpecCompilation(t *testing.T) {
	open := true
	schema, err := ValueSchemaToJSONSchema(&ValueSchemaSpec{
		Type: "object",
		Properties: map[string]PropSpec{
			"mode":  {ValueSchemaSpec: ValueSchemaSpec{Type: "string", Enum: []any{"fast", "slow"}}, Required: true},
			"note":  {ValueSchemaSpec: ValueSchemaSpec{Type: "json", Description: "free form"}},
			"pair":  {ValueSchemaSpec: ValueSchemaSpec{OneOf: []*ValueSchemaSpec{{Type: "null"}, {Type: "integer"}}}},
			"extra": {ValueSchemaSpec: ValueSchemaSpec{Type: "boolean"}},
		},
		AdditionalProperties: &open,
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	encoded, _ := json.Marshal(schema)
	want := `{"additionalProperties":true,"properties":{"extra":{"type":"boolean"},"mode":{"enum":["fast","slow"],"type":"string"},"note":{"description":"free form"},"pair":{"oneOf":[{"type":"null"},{"type":"integer"}]}},"required":["mode"],"type":"object"}`
	if string(encoded) != want {
		t.Fatalf("compiled = %s\nwant    = %s", encoded, want)
	}
	// The compiled schema validates real values.
	if got := ValidateJsonSchemaValue(schema, map[string]any{"mode": "fast", "note": []any{1}}, ""); len(got) != 0 {
		t.Fatalf("value rejected: %v", got)
	}
}

func TestAuthorSchemaErrors(t *testing.T) {
	closed := false
	cases := []struct {
		name string
		spec *ValueSchemaSpec
		want string
	}{
		{"object without openness", &ValueSchemaSpec{Type: "object"}, "schema.additionalProperties must be explicitly true or false"},
		{"type + oneOf", &ValueSchemaSpec{Type: "json", OneOf: []*ValueSchemaSpec{{Type: "null"}, {Type: "null"}}}, "schema cannot declare both type and oneOf"},
		{"oneOf singleton", &ValueSchemaSpec{OneOf: []*ValueSchemaSpec{{Type: "null"}}}, "schema.oneOf must be an array of at least two value schemas"},
		{"missing type", &ValueSchemaSpec{}, "schema.type must be string/number/integer/boolean/null/array/object/json, or use oneOf"},
		{"unknown type", &ValueSchemaSpec{Type: "map"}, "schema.type must be string/number/integer/boolean/null/array/object/json, or use oneOf"},
		{"empty enum", &ValueSchemaSpec{Type: "string", Enum: []any{}}, "schema.enum must be a non-empty array of scalar values"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ValueSchemaToJSONSchema(testCase.spec)
			if err == nil || err.Error() != "unsupported JSON schema: "+testCase.want {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// Closed object with required properties compiles and validates.
	schema, err := ValueSchemaToJSONSchema(&ValueSchemaSpec{
		Type: "object",
		Properties: map[string]PropSpec{
			"a": {ValueSchemaSpec: ValueSchemaSpec{Type: "string"}, Required: true},
			"b": {ValueSchemaSpec: ValueSchemaSpec{Type: "integer"}},
		},
		AdditionalProperties: &closed,
	})
	if err != nil {
		t.Fatalf("closed compile: %v", err)
	}
	got := ValidateJsonSchemaValue(schema, map[string]any{"b": 1, "c": true}, "args")
	assertViolations(t, got, []string{
		`missing required property "args.a"`,
		`"args.c" is not a declared property (additionalProperties: false)`,
	})
}

func TestValidateArgsAndToolArgsError(t *testing.T) {
	spec := map[string]PropSpec{
		"path":  {ValueSchemaSpec: ValueSchemaSpec{Type: "string"}, Required: true},
		"depth": {ValueSchemaSpec: ValueSchemaSpec{Type: "integer", Const: 1}},
	}
	if got := ValidateArgs(spec, map[string]any{"path": "x", "depth": 1}); len(got) != 0 {
		t.Fatalf("valid rejected: %v", got)
	}
	got := ValidateArgs(spec, map[string]any{"depth": 2})
	assertViolations(t, got, []string{
		`missing required property "path"`,
		`"depth" must be 1`,
	})
	// Non-object arguments fail against the implicit object root.
	got = ValidateArgs(spec, 5)
	assertViolations(t, got, []string{`"arguments" must be an object`})
}

// --- DefineTool -------------------------------------------------------------------

func TestDefineToolValidationContract(t *testing.T) {
	tool, err := DefineTool(DefineToolOptions{
		Name:        "greet",
		Description: "greet someone",
		Parameters: map[string]PropSpec{
			"name": {ValueSchemaSpec: ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: ToolOutput{
			Schema: &ValueSchemaSpec{Type: "string"},
			Render: func(args map[string]any, value any) []llm.ContentBlock { return nil },
		},
		Execute: func(args map[string]any, exec *ToolRunContext) (any, error) {
			if exec.CallID != "call-1" {
				t.Fatalf("exec.CallID = %q", exec.CallID)
			}
			return "hello " + args["name"].(string), nil
		},
		IsConcurrencySafe: func(args map[string]any) bool { return true },
	})
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	// Execute validates before entering the body.
	_, err = tool.Execute(map[string]any{}, &ToolRunContext{ToolExecution: ToolExecution{CallID: "call-1"}})
	var argsErr *ToolArgsError
	if !errors.As(err, &argsErr) || argsErr.Code() != "INVALID_ARGS" {
		t.Fatalf("err = %v", err)
	}
	if len(argsErr.Violations) != 1 || argsErr.Violations[0] != `missing required property "name"` {
		t.Fatalf("violations = %v", argsErr.Violations)
	}
	value, err := tool.Execute(map[string]any{"name": "ada"}, &ToolRunContext{ToolExecution: ToolExecution{CallID: "call-1"}})
	if err != nil || value != "hello ada" {
		t.Fatalf("execute = %v, %v", value, err)
	}
	// Valid arguments classify with the user classifier.
	if !tool.IsConcurrencySafe(map[string]any{"name": "ada"}) {
		t.Fatal("safe call classified unsafe")
	}
}

func TestDefineToolTimeoutAndAuthorFailures(t *testing.T) {
	if _, err := DefineTool(DefineToolOptions{Name: "t", TimeoutMs: -1}); err == nil ||
		err.Error() != "defineTool(t): timeoutMs must be a positive finite number" {
		t.Fatalf("err = %v", err)
	}
	// An author schema failure surfaces at definition time, not call time.
	if _, err := DefineTool(DefineToolOptions{
		Name:       "t",
		Parameters: map[string]PropSpec{"x": {ValueSchemaSpec: ValueSchemaSpec{Type: "object"}}},
	}); err == nil || !strings.Contains(err.Error(), "additionalProperties must be explicitly true or false") {
		t.Fatalf("err = %v", err)
	}
}
