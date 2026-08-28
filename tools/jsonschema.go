// Enforced JSON Schema subset shared by tool outputs, generated PTC mode
// types, subagents, and workflows. The subset accepts any JSON root, an
// annotation-only schema for unconstrained JSON, one scalar `type`, object
// `properties`/`required`/boolean `additionalProperties`, array `items`,
// type-correct scalar `enum`/`const`, and exact-one `oneOf`.
//
// Unsupported or misplaced keywords reject rather than being accepted without
// enforcement. Consumers that require an object root apply
// AssertObjectJsonSchema before accepting input.
//
// Port of packages/core/tools/src/json-schema.ts. Go adaptations: raw
// schemas are decoded JSON (map[string]any / []any); map key iteration is
// sorted so violation order is deterministic (JS iterates insertion order);
// numbers normalize int/int64/json.Number/float64 into one float64 view, the
// superset of shapes `encoding/json` and Go authors produce.
package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"dshgo/session"
)

// Error codes carried by this package's structured errors; extract via
// errors.As and read Code().
const (
	// CodeUnsupportedSchema marks a raw schema outside the enforced subset.
	CodeUnsupportedSchema = "UNSUPPORTED_SCHEMA"
	// CodeInvalidArgs marks model-generated tool arguments that fail their
	// declared parameter schema.
	CodeInvalidArgs = "INVALID_ARGS"
)

// JsonSchemaError reports a raw schema outside the enforced subset.
// Violations lists every offending path in walk order instead of stopping at
// the first author error.
type JsonSchemaError struct {
	Violations []string
}

func (e *JsonSchemaError) Error() string {
	return fmt.Sprintf("unsupported JSON schema: %s", strings.Join(e.Violations, "; "))
}

// Code returns UNSUPPORTED_SCHEMA.
func (e *JsonSchemaError) Code() string { return CodeUnsupportedSchema }

// The schema keywords the subset enforces.
var constraintKeywords = map[string]bool{
	"type": true, "oneOf": true, "properties": true, "required": true,
	"additionalProperties": true, "items": true, "enum": true, "const": true,
}

// Annotation keywords: carried, checked for lossless JSON, never validating.
var annotationKeywords = map[string]bool{
	"description": true, "title": true, "default": true, "examples": true,
}

var schemaTypeNames = []string{"object", "array", "string", "number", "integer", "boolean", "null"}

// Keywords that are invalid beside `oneOf`.
var oneOfSiblingKeywords = []string{"properties", "required", "additionalProperties", "items", "enum", "const"}

// scalarOnlyTypes are the types literal constraints may decorate.
var scalarOnlyTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true, "null": true,
}

func isSchemaTypeName(name string) bool {
	for _, candidate := range schemaTypeNames {
		if candidate == name {
			return true
		}
	}
	return false
}

// isSchemaRecord tests for the decoded-JSON record shape.
func isSchemaRecord(value any) (map[string]any, bool) {
	record, ok := value.(map[string]any)
	return record, ok
}

// isSchemaArray tests for the decoded-JSON array shape.
func isSchemaArray(value any) ([]any, bool) {
	array, ok := value.([]any)
	return array, ok
}

// jsonFloat views one JSON number through its float64 value. The accepted
// shapes are the ones `encoding/json` and this codebase's vocabulary produce.
func jsonFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsInf(typed, 0) && !math.IsNaN(typed)
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, !math.IsInf(parsed, 0) && !math.IsNaN(parsed)
	default:
		return 0, false
	}
}

// negativeZero reports the value the JSON wire cannot distinguish but the
// author DSL must reject as an enum/const entry.
func negativeZero(value float64) bool {
	return value == 0 && math.Signbit(value)
}

// scalarMatches reports whether one scalar is valid for a declared schema
// type (the author-side gate for enum and const entries).
func scalarMatches(schemaType string, value any) bool {
	switch schemaType {
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		parsed, ok := jsonFloat(value)
		return ok && !negativeZero(parsed)
	case "integer":
		parsed, ok := jsonFloat(value)
		return ok && !negativeZero(parsed) && parsed == math.Trunc(parsed)
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return false
	}
}

// scalarEqual compares two scalars the way JS SameValueZero membership does,
// normalizing number shapes.
func scalarEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if left, ok := jsonFloat(a); ok {
		right, ok := jsonFloat(b)
		return ok && left == right
	}
	return a == b
}

func schemaNodePointer(node any) uintptr {
	value := reflect.ValueOf(node)
	switch value.Kind() {
	case reflect.Map, reflect.Slice:
		return value.Pointer()
	default:
		return 0
	}
}

// Deferred work for the raw-schema walk. Go stacks grow, but the explicit
// task form keeps the leave-phase cycle bookkeeping identical to the source.
type schemaWalkTask struct {
	kind       string // "enter" | "leave" | "one-of-tail" | "object-tail"
	node       any
	record     map[string]any
	path       string
	properties any
}

// Validate object-only fields after its property schemas have been visited.
func checkObjectSchemaTail(node map[string]any, path string, properties any, violations *[]string) {
	if required, has := node["required"]; has {
		entries, ok := isSchemaArray(required)
		if !ok {
			*violations = append(*violations, fmt.Sprintf("%s.required must be an array of strings", path))
		} else {
			declared := map[string]any{}
			if record, ok := isSchemaRecord(properties); ok {
				declared = record
			}
			for _, entry := range entries {
				name, ok := entry.(string)
				if !ok {
					// Covered by the array-of-strings violation above; the
					// membership check only applies to named entries.
					continue
				}
				if _, declaredHas := declared[name]; !declaredHas {
					*violations = append(*violations, fmt.Sprintf("%s.required names %q which is not in properties", path, name))
				}
			}
		}
	}
	if additional, has := node["additionalProperties"]; has {
		if _, ok := additional.(bool); !ok {
			*violations = append(*violations, fmt.Sprintf("%s.additionalProperties must be a boolean", path))
		}
	}
}

// Collect every violation for one raw schema tree.
func checkSchemaNode(root any, rootPath string, violations *[]string, seen map[uintptr]struct{}) {
	tasks := []schemaWalkTask{{kind: "enter", node: root, path: rootPath}}
	for len(tasks) > 0 {
		task := tasks[len(tasks)-1]
		tasks = tasks[:len(tasks)-1]
		switch task.kind {
		case "leave":
			delete(seen, schemaNodePointer(task.node))
			continue
		case "one-of-tail":
			for _, key := range oneOfSiblingKeywords {
				if _, has := task.record[key]; has {
					*violations = append(*violations, fmt.Sprintf("%s.%s is not supported beside oneOf", task.path, key))
				}
			}
			continue
		case "object-tail":
			checkObjectSchemaTail(task.record, task.path, task.properties, violations)
			continue
		}

		node, ok := isSchemaRecord(task.node)
		if !ok {
			*violations = append(*violations, fmt.Sprintf("%s must be a schema object", task.path))
			continue
		}
		pointer := schemaNodePointer(node)
		if _, circular := seen[pointer]; circular {
			*violations = append(*violations, fmt.Sprintf("%s is circular", task.path))
			continue
		}
		seen[pointer] = struct{}{}
		tasks = append(tasks, schemaWalkTask{kind: "leave", node: node})

		keys := sortedKeys(node)
		for _, key := range keys {
			if constraintKeywords[key] {
				continue
			}
			if annotationKeywords[key] {
				if !session.IsJsonValue(node[key]) {
					*violations = append(*violations, fmt.Sprintf("%s.%s annotation must be lossless JSON data", task.path, key))
				}
				continue
			}
			*violations = append(*violations, fmt.Sprintf("%s.%s is not a supported keyword (subset: type/oneOf/properties/required/additionalProperties/items/enum/const + annotations)", task.path, key))
		}
		if description, has := node["description"]; has {
			if _, ok := description.(string); !ok {
				*violations = append(*violations, fmt.Sprintf("%s.description must be a string", task.path))
			}
		}
		if title, has := node["title"]; has {
			if _, ok := title.(string); !ok {
				*violations = append(*violations, fmt.Sprintf("%s.title must be a string", task.path))
			}
		}

		typeValue, hasType := node["type"]
		_, hasOneOf := node["oneOf"]
		if hasType && hasOneOf {
			*violations = append(*violations, fmt.Sprintf("%s cannot declare both type and oneOf", task.path))
			continue
		}
		if !hasType && !hasOneOf {
			for _, key := range oneOfSiblingKeywords {
				if _, has := node[key]; has {
					*violations = append(*violations, fmt.Sprintf("%s.%s requires type or oneOf", task.path, key))
				}
			}
			continue
		}

		if hasOneOf {
			branches, ok := isSchemaArray(node["oneOf"])
			tasks = append(tasks, schemaWalkTask{kind: "one-of-tail", record: node, path: task.path})
			if !ok || len(branches) < 2 {
				*violations = append(*violations, fmt.Sprintf("%s.oneOf must be an array of at least two schemas", task.path))
			} else {
				for index := len(branches) - 1; index >= 0; index-- {
					tasks = append(tasks, schemaWalkTask{kind: "enter", node: branches[index], path: fmt.Sprintf("%s.oneOf[%d]", task.path, index)})
				}
			}
			continue
		}

		typeName, ok := typeValue.(string)
		if !ok || !isSchemaTypeName(typeName) {
			if _, isArray := isSchemaArray(typeValue); isArray {
				*violations = append(*violations, fmt.Sprintf("%s.type must be a single type string (type arrays are not supported)", task.path))
			} else {
				*violations = append(*violations, fmt.Sprintf("%s.type must be one of %s", task.path, strings.Join(schemaTypeNames, "/")))
			}
			continue
		}

		allowedFor := map[string][]string{
			"properties":           {"object"},
			"required":             {"object"},
			"additionalProperties": {"object"},
			"items":                {"array"},
			"enum":                 {"string", "number", "integer", "boolean", "null"},
			"const":                {"string", "number", "integer", "boolean", "null"},
		}
		for _, keyword := range []string{"additionalProperties", "const", "enum", "items", "properties", "required"} {
			types := allowedFor[keyword]
			if _, has := node[keyword]; has && !containsString(types, typeName) {
				*violations = append(*violations, fmt.Sprintf("%s.%s is not supported on type %q", task.path, keyword, typeName))
			}
		}

		switch typeName {
		case "object":
			properties, hasProperties := node["properties"]
			tasks = append(tasks, schemaWalkTask{kind: "object-tail", record: node, path: task.path, properties: properties})
			if hasProperties {
				record, ok := isSchemaRecord(properties)
				if !ok {
					*violations = append(*violations, fmt.Sprintf("%s.properties must be an object of schemas", task.path))
				} else {
					for _, key := range sortedKeys(record) {
						tasks = append(tasks, schemaWalkTask{kind: "enter", node: record[key], path: fmt.Sprintf("%s.properties.%s", task.path, key)})
					}
				}
			}
		case "array":
			if items, has := node["items"]; has {
				tasks = append(tasks, schemaWalkTask{kind: "enter", node: items, path: fmt.Sprintf("%s.items", task.path)})
			}
		default:
			// Scalar node: enum must be a non-empty array of matching
			// scalars; const must match and stay inside a declared enum.
			allowed, hasEnum := node["enum"]
			enumEntries, _ := isSchemaArray(allowed)
			enumValid := hasEnum && enumEntries != nil && len(enumEntries) > 0
			if enumValid {
				for _, entry := range enumEntries {
					if !scalarMatches(typeName, entry) {
						enumValid = false
						break
					}
				}
			}
			if hasEnum && !enumValid {
				*violations = append(*violations, fmt.Sprintf("%s.enum must be a non-empty array of %s values", task.path, typeName))
			}
			if declaredConst, hasConst := node["const"]; hasConst {
				if !scalarMatches(typeName, declaredConst) {
					*violations = append(*violations, fmt.Sprintf("%s.const must be a %s value", task.path, typeName))
				} else if enumValid && !containsScalar(enumEntries, declaredConst) {
					*violations = append(*violations, fmt.Sprintf("%s.const must be one of %s.enum when both are declared", task.path, task.path))
				}
			}
		}
	}
}

func sortedKeys(record map[string]any) []string {
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func containsScalar(values []any, needle any) bool {
	for _, value := range values {
		if scalarEqual(value, needle) {
			return true
		}
	}
	return false
}

// AssertSupportedJsonSchema asserts that an arbitrary raw schema uses only
// the enforced subset. Annotation-only schemas are accepted as the standard
// unconstrained-JSON form; callers that require an object root use
// AssertObjectJsonSchema.
func AssertSupportedJsonSchema(schema any) error {
	violations := []string{}
	checkSchemaNode(schema, "schema", &violations, map[uintptr]struct{}{})
	if len(violations) > 0 {
		return &JsonSchemaError{Violations: violations}
	}
	return nil
}

// AssertObjectJsonSchema asserts the enforced subset plus the object-root
// constraint retained by subagent and workflow structured outputs.
func AssertObjectJsonSchema(schema any) error {
	violations := []string{}
	checkSchemaNode(schema, "schema", &violations, map[uintptr]struct{}{})
	if len(violations) == 0 {
		record, ok := isSchemaRecord(schema)
		if !ok {
			violations = append(violations, `schema.type must be "object" (structured output is object-rooted)`)
		} else if typeValue, has := record["type"]; !has || typeValue != "object" {
			violations = append(violations, `schema.type must be "object" (structured output is object-rooted)`)
		}
	}
	if len(violations) > 0 {
		return &JsonSchemaError{Violations: violations}
	}
	return nil
}

// Root-aware diagnostic path for the parameter validator's empty sentinel.
func diagnosticPath(path string) string {
	if path == "" {
		return "arguments"
	}
	return path
}

// Append one object property without a leading dot at an implicit root.
func propertyPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// losslessValueViolation is the generic containment diagnostic owned by one
// valid schema node.
func losslessValueViolation(path string) []string {
	return []string{fmt.Sprintf("%q must be a lossless JSON value", diagnosticPath(path))}
}

// checkScalarValue validates one scalar node after its primitive type check.
func checkScalarValue(node map[string]any, value any, path string) []string {
	if allowed, has := node["enum"]; has {
		if !containsScalar(allowed.([]any), value) {
			encoded, _ := json.Marshal(allowed)
			return []string{fmt.Sprintf("%q must be one of %s", diagnosticPath(path), encoded)}
		}
	}
	if declaredConst, has := node["const"]; has {
		if !scalarEqual(value, declaredConst) {
			encoded, _ := json.Marshal(declaredConst)
			return []string{fmt.Sprintf("%q must be %s", diagnosticPath(path), encoded)}
		}
	}
	return nil
}

// One child evaluation deferred by a container or exact-one union frame.
type valueChild struct {
	node  map[string]any
	value any
	path  string
}

// Explicit call frame for schema-value validation.
type valueFrame struct {
	node           map[string]any
	value          any
	path           string
	kind           string // "" | "oneOf" | "object" | "array"
	phase          string // "start" | "children"
	children       []valueChild
	childIndex     int
	violations     []string
	tailViolations []string
	matches        int
}

// Validate one trusted schema/value pair. Total for arbitrary values;
// returns path-qualified violations in walk order; empty means valid.
func checkValue(schema map[string]any, value any, path string) []string {
	frames := []valueFrame{{node: schema, value: value, path: path, phase: "start"}}
	var rootResult []string
	rootSet := false

	receive := func(result []string) {
		if len(frames) == 0 {
			rootResult = result
			rootSet = true
			return
		}
		parent := &frames[len(frames)-1]
		if parent.kind == "oneOf" {
			if len(result) == 0 {
				parent.matches++
			}
		} else {
			parent.violations = append(parent.violations, result...)
		}
	}
	finish := func(result []string) {
		frames = frames[:len(frames)-1]
		receive(result)
	}

	for len(frames) > 0 {
		frame := &frames[len(frames)-1]
		if frame.phase == "children" {
			if frame.childIndex < len(frame.children) {
				child := frame.children[frame.childIndex]
				frame.childIndex++
				frames = append(frames, valueFrame{node: child.node, value: child.value, path: child.path, phase: "start"})
				continue
			}
			if frame.kind == "oneOf" {
				if frame.matches == 1 {
					finish(nil)
				} else {
					finish([]string{fmt.Sprintf("%q must match exactly one oneOf branch (matched %d)", diagnosticPath(frame.path), frame.matches)})
				}
				continue
			}
			frame.violations = append(frame.violations, frame.tailViolations...)
			if len(frame.violations) > 0 {
				finish(frame.violations)
			} else if !session.IsJsonValue(frame.value) {
				if frame.kind == "object" {
					finish([]string{fmt.Sprintf("%q must be a lossless JSON object", diagnosticPath(frame.path))})
				} else {
					finish([]string{fmt.Sprintf("%q must be a dense lossless JSON array", diagnosticPath(frame.path))})
				}
			} else {
				finish(nil)
			}
			continue
		}

		nodeType, hasType := frame.node["type"]
		if branches, hasOneOf := frame.node["oneOf"]; hasOneOf {
			_ = hasOneOf
			frame.kind = "oneOf"
			entries := branches.([]any)
			for _, branch := range entries {
				frame.children = append(frame.children, valueChild{node: branch.(map[string]any), value: frame.value, path: frame.path})
			}
			frame.childIndex = 0
			frame.matches = 0
			frame.phase = "children"
			continue
		}
		if !hasType {
			if session.IsJsonValue(frame.value) {
				finish(nil)
			} else {
				finish(losslessValueViolation(frame.path))
			}
			continue
		}

		switch nodeType {
		case "object":
			record, ok := frame.value.(map[string]any)
			if !ok {
				finish([]string{fmt.Sprintf("%q must be an object", diagnosticPath(frame.path))})
				break
			}
			properties := map[string]any{}
			if declared, has := frame.node["properties"]; has {
				if typed, ok := declared.(map[string]any); ok {
					properties = typed
				}
			}
			var violations []string
			if required, has := frame.node["required"]; has {
				if entries, ok := required.([]any); ok {
					for _, entry := range entries {
						key := entry.(string)
						if _, present := record[key]; !present {
							violations = append(violations, fmt.Sprintf("missing required property %q", propertyPath(frame.path, key)))
						}
					}
				}
			}
			var children []valueChild
			for _, key := range sortedKeys(properties) {
				if value, present := record[key]; present {
					children = append(children, valueChild{node: properties[key].(map[string]any), value: value, path: propertyPath(frame.path, key)})
				}
			}
			var tailViolations []string
			if additional, has := frame.node["additionalProperties"]; has {
				if closed, ok := additional.(bool); ok && !closed {
					for _, key := range sortedKeys(record) {
						if _, declared := properties[key]; !declared {
							tailViolations = append(tailViolations, fmt.Sprintf("%q is not a declared property (additionalProperties: false)", propertyPath(frame.path, key)))
						}
					}
				}
			}
			frame.kind = "object"
			frame.children = children
			frame.childIndex = 0
			frame.violations = violations
			frame.tailViolations = tailViolations
			frame.phase = "children"
		case "array":
			entries, ok := frame.value.([]any)
			if !ok {
				finish([]string{fmt.Sprintf("%q must be an array", diagnosticPath(frame.path))})
				break
			}
			var children []valueChild
			if items, has := frame.node["items"]; has {
				itemSchema := items.(map[string]any)
				for index, entry := range entries {
					children = append(children, valueChild{node: itemSchema, value: entry, path: fmt.Sprintf("%s[%d]", frame.path, index)})
				}
			}
			frame.kind = "array"
			frame.children = children
			frame.childIndex = 0
			frame.violations = nil
			frame.phase = "children"
		case "string":
			if text, ok := frame.value.(string); ok {
				finish(checkScalarValue(frame.node, text, frame.path))
			} else {
				finish([]string{fmt.Sprintf("%q must be a string", diagnosticPath(frame.path))})
			}
		case "number":
			parsed, ok := jsonFloat(frame.value)
			switch {
			case !isNumberShape(frame.value):
				finish([]string{fmt.Sprintf("%q must be a number", diagnosticPath(frame.path))})
			case !ok:
				finish([]string{fmt.Sprintf("%q must be a finite JSON number", diagnosticPath(frame.path))})
			default:
				finish(checkScalarValue(frame.node, parsed, frame.path))
			}
		case "integer":
			parsed, ok := jsonFloat(frame.value)
			if !ok || parsed != math.Trunc(parsed) {
				finish([]string{fmt.Sprintf("%q must be an integer", diagnosticPath(frame.path))})
			} else {
				finish(checkScalarValue(frame.node, parsed, frame.path))
			}
		case "boolean":
			if flag, ok := frame.value.(bool); ok {
				finish(checkScalarValue(frame.node, flag, frame.path))
			} else {
				finish([]string{fmt.Sprintf("%q must be a boolean", diagnosticPath(frame.path))})
			}
		case "null":
			if frame.value == nil {
				finish(checkScalarValue(frame.node, nil, frame.path))
			} else {
				finish([]string{fmt.Sprintf("%q must be null", diagnosticPath(frame.path))})
			}
		}
	}

	if rootSet {
		return rootResult
	}
	return losslessValueViolation(path)
}

// isNumberShape reports whether the value is a number of any accepted Go
// shape, including non-finite floats (which fail the finiteness gate next).
func isNumberShape(value any) bool {
	switch value.(type) {
	case float64, int, int64, json.Number:
		return true
	default:
		return false
	}
}

// ValidateJsonSchemaValue validates a candidate value against an asserted
// raw schema. The function is total for arbitrary values and returns
// path-qualified violations in walk order; empty means valid.
func ValidateJsonSchemaValue(schema map[string]any, value any, path string) []string {
	return checkValue(schema, value, path)
}
