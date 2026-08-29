package toolfs

import (
	"context"
	"fmt"

	"dshgo/fs"
	"dshgo/llm"
	"dshgo/tools"
)

// Caps is the resolved read caps (plugin config after defaulting).
type Caps struct {
	Limit         int
	MaxLineLength int
	MaxBytes      int
	StreamMinSize int
}

// DefaultCaps applies the official defaults.
func DefaultCaps() Caps {
	return Caps{Limit: ReadLimit, MaxLineLength: ReadMaxLineLength, MaxBytes: ReadMaxBytes, StreamMinSize: StreamMinSize}
}

// assertPositiveInteger guards the windowing arithmetic.
func assertPositiveInteger(name string, value int) error {
	if value < 1 {
		return fmt.Errorf("tool-fs: %s must be a positive integer", name)
	}
	return nil
}

// textBlock is the single-text render helper.
func textBlock(text string) []llm.ContentBlock {
	return []llm.ContentBlock{{Type: llm.BlockText, Text: text}}
}

// boolPtr is the explicit additionalProperties pointer the schema DSL
// requires on every object schema.
func boolPtr(value bool) *bool { return &value }

// Register installs the full read/write/edit filesystem tool suite and
// returns a teardown.
func Register(runtime *tools.ToolRuntime, deps RegisterDeps, caps Caps) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("tool-fs: a tool runtime is required")
	}
	if deps.Backend == nil {
		return nil, fmt.Errorf("tool-fs: a filesystem backend is required")
	}
	if deps.Ctx == nil {
		return nil, fmt.Errorf("tool-fs: a plugin context is required")
	}
	for _, pair := range []struct {
		name  string
		value int
	}{{"readLimit", caps.Limit}, {"readMaxLineLength", caps.MaxLineLength}, {"readMaxBytes", caps.MaxBytes}, {"readStreamMinSize", caps.StreamMinSize}} {
		if err := assertPositiveInteger(pair.name, pair.value); err != nil {
			return nil, err
		}
	}
	controller, err := newController(deps.Backend, deps.Ctx, deps.Policy, deps.ApproverSource, deps.Agents, deps.PermissionFolds)
	if err != nil {
		return nil, err
	}
	undoRead, err := registerRead(runtime, controller, caps)
	if err != nil {
		return nil, err
	}
	undoWrite, err := registerWrite(runtime, controller)
	if err != nil {
		undoRead()
		return nil, err
	}
	undoEdit, err := registerEdit(runtime, controller)
	if err != nil {
		undoRead()
		undoWrite()
		return nil, err
	}
	return func() { undoRead(); undoWrite(); undoEdit() }, nil
}

// chunksFromStream adapts fs.StreamText's iterator.
func chunksFromStream(next func() (string, bool)) TextChunks { return next }

// chunksFromWhole adapts one whole-file read.
func chunksFromWhole(content string) TextChunks {
	return func() (string, bool) {
		if content == "" {
			return "", false
		}
		whole := content
		content = ""
		return whole, true
	}
}

// routeChunks streams large or size-unknown files so a size-less backend
// never buffers an arbitrarily large file.
func routeChunks(ctx context.Context, controller *controller, target fs.Target, info *fs.Info, caps Caps) (TextChunks, error) {
	if info.Size == nil || *info.Size >= int64(caps.StreamMinSize) {
		next, err := controller.backend.StreamText(ctx, target)
		if err != nil {
			return nil, err
		}
		return chunksFromStream(next), nil
	}
	content, err := controller.backend.ReadText(ctx, target)
	if err != nil {
		return nil, err
	}
	return chunksFromWhole(content), nil
}

// parseReadArgs validates read arguments and applies the defaults.
func parseReadArgs(args map[string]any, maxLimit int) (filePath string, offset int, limit int, err error) {
	if blankArg(args, "file_path") {
		return "", 0, 0, fmt.Errorf("file_path must be a non-empty string")
	}
	offset = 1
	if value, ok := argInt(args, "offset"); ok {
		if offset, err = parsePositiveInteger(value, "offset"); err != nil {
			return "", 0, 0, err
		}
	}
	limit = maxLimit
	if value, ok := argInt(args, "limit"); ok {
		if limit, err = parsePositiveInteger(value, "limit"); err != nil {
			return "", 0, 0, err
		}
	}
	if limit > maxLimit {
		return "", 0, 0, fmt.Errorf("limit must be less than or equal to %d", maxLimit)
	}
	return argString(args, "file_path"), offset, limit, nil
}

// registerRead installs the `read` tool.
func registerRead(runtime *tools.ToolRuntime, controller *controller, caps Caps) (func(), error) {
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "read",
		Description: "Read a UTF-8 text file and return line-numbered content.",
		Parameters: map[string]tools.PropSpec{
			"file_path": {
				ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "Path to read, resolved by the filesystem backend."},
				Required:        true,
			},
			"offset": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "number", Description: "1-based first line to return. Defaults to 1."}},
			"limit":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "number", Description: fmt.Sprintf("Maximum number of lines to return. Defaults to %d.", caps.Limit)}},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(true),
				Properties: map[string]tools.PropSpec{
					"path":       {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
					"offset":     {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}},
					"lines":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "array", Items: &tools.ValueSchemaSpec{Type: "object", AdditionalProperties: boolPtr(true), Properties: map[string]tools.PropSpec{"number": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}}, "text": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}}}}}},
					"totalLines": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				outcome, ok := value.(map[string]any)
				if !ok {
					return textBlock(fmt.Sprintf("%v", value))
				}
				displayPath, _ := outcome["path"].(string)
				offset, _ := outcome["offset"].(int)
				totalLines, _ := outcome["totalLines"].(int)
				truncated, _ := outcome["truncatedByBytes"].(bool)
				var lines []FileTextLine
				if raw, ok := outcome["lines"].([]any); ok {
					for _, item := range raw {
						row, ok := item.(map[string]any)
						if !ok {
							continue
						}
						number, _ := row["number"].(int)
						text, _ := row["text"].(string)
						lines = append(lines, FileTextLine{Number: number, Text: text})
					}
				}
				return textBlock(FormatReadOutput(displayPath, offset, lines, totalLines, truncated))
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			filePath, offset, limit, err := parseReadArgs(args, caps.Limit)
			if err != nil {
				return nil, err
			}
			ctx := execSignal(exec)
			target, info, err := resolveRegularReadTarget(ctx, controller, filePath, exec)
			if err != nil {
				return nil, err
			}
			chunks, err := routeChunks(ctx, controller, target, info, caps)
			if err != nil {
				return nil, err
			}
			window, err := BuildWindow(chunks, ReadWindow{Offset: offset, Limit: limit, MaxLineLength: caps.MaxLineLength, MaxBytes: caps.MaxBytes}, target.DisplayPath)
			if err != nil {
				return nil, err
			}
			controller.recordObservation(target, fs.ObservationPresent(info.Version), exec)
			return map[string]any{
				"path":             target.DisplayPath,
				"offset":           offset,
				"lines":            linesToCanonical(window.Lines),
				"totalLines":       window.TotalLines,
				"truncatedByBytes": window.TruncatedByBytes,
			}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

// linesToCanonical projects the window lines into the lossless-JSON shape.
func linesToCanonical(lines []FileTextLine) []any {
	out := make([]any, 0, len(lines))
	for _, line := range lines {
		out = append(out, map[string]any{"number": line.Number, "text": line.Text})
	}
	return out
}

// formatWriteOutput renders the model-facing confirmation envelope (no file
// content is echoed back).
func formatWriteOutput(displayPath string, operation string) string {
	verb := "Updated"
	if operation == "create" {
		verb = "Created"
	}
	return fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s file\n</content>", displayPath, verb)
}

// registerWrite installs the `write` tool: an unconditional atomic
// create-or-overwrite unless the single policy slot produces an intent.
func registerWrite(runtime *tools.ToolRuntime, controller *controller) (func(), error) {
	params := map[string]tools.PropSpec{
		"file_path": {
			ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "Path to write, resolved by the filesystem backend."},
			Required:        true,
		},
		"content": {
			ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "Full UTF-8 text content to write."},
			Required:        true,
		},
	}
	for name, spec := range controller.schemaFields() {
		params[name] = spec
	}
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "write",
		Description: "Create or fully replace a UTF-8 text file.",
		Parameters:  params,
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(true),
				Properties: map[string]tools.PropSpec{
					"path":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
					"operation": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Enum: []any{"create", "update"}}},
					"before": {ValueSchemaSpec: tools.ValueSchemaSpec{OneOf: []*tools.ValueSchemaSpec{
						{Type: "string"},
						{Type: "null"},
					}}},
					"after": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				outcome, ok := value.(map[string]any)
				if !ok {
					return textBlock(fmt.Sprintf("%v", value))
				}
				displayPath, _ := outcome["path"].(string)
				operation, _ := outcome["operation"].(string)
				return textBlock(formatWriteOutput(displayPath, operation))
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			if err := blankPath(args); err != nil {
				return nil, err
			}
			filePath := argString(args, "file_path")
			content := argString(args, "content")
			ctx := execSignal(exec)
			policy, err := controller.resolvePolicy("write", args, exec)
			if err != nil {
				return nil, err
			}
			var policyRoot string
			if policy != nil {
				policyRoot = policy.WorkspaceRoot
			}
			target, err := controller.resolveTarget(ctx, filePath, policyRoot, execScope(exec))
			if err != nil {
				return nil, err
			}
			// Single-slot decision: the policy plugin produces
			// createIfAbsent/replaceIfVersion; the bare default is nil
			// (unconditional). No stat.
			decided := controller.waterfall(fs.EventWriteIntent, fs.WriteIntentEvent{Target: target, Actor: exec})
			var intent *fs.WriteIntent
			if owned, ok := decided.(*fs.WriteIntent); ok {
				intent = owned
			}
			outcome, err := controller.backend.WriteText(ctx, target, content, intent, policy)
			if err != nil {
				return nil, RemediateFsError(controller.mapError(err, policy))
			}
			controller.recordObservation(target, fs.ObservationPresent(outcome.Version), exec)
			return map[string]any{
				"path":      target.DisplayPath,
				"operation": outcome.Operation,
				"before":    stringOrNil(outcome.Before),
				"after":     outcome.After,
			}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

// stringOrNil dereferences an optional string into the lossless-JSON shape.
func stringOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// formatEditOutput renders the edit success (single-match or replace-all) as
// the model-facing confirmation.
func formatEditOutput(displayPath string, replaceAll bool) string {
	if replaceAll {
		return fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", displayPath)
	}
	return fmt.Sprintf("The file %s has been updated successfully.", displayPath)
}

// registerEdit installs the `edit` tool: a literal unique-match replacement
// with an optional guard from the single intent slot.
func registerEdit(runtime *tools.ToolRuntime, controller *controller) (func(), error) {
	params := map[string]tools.PropSpec{
		"file_path": {
			ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "Path to edit, resolved by the filesystem backend."},
			Required:        true,
		},
		"old_string": {
			ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "Literal text to replace. Must match exactly."},
			Required:        true,
		},
		"new_string": {
			ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "Literal replacement text. Use an empty string to delete the match."},
			Required:        true,
		},
		"replace_all": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "boolean", Description: "Replace all matches. Defaults to false; when false, old_string must appear exactly once."}},
	}
	for name, spec := range controller.schemaFields() {
		params[name] = spec
	}
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "edit",
		Description: "Edit an existing UTF-8 text file by replacing literal text.",
		Parameters:  params,
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(true),
				Properties: map[string]tools.PropSpec{
					"path":   {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
					"before": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
					"after":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				outcome, ok := value.(map[string]any)
				if !ok {
					return textBlock(fmt.Sprintf("%v", value))
				}
				displayPath, _ := outcome["path"].(string)
				replaceAll := argBool(args, "replace_all")
				return textBlock(formatEditOutput(displayPath, replaceAll))
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			if err := blankPath(args); err != nil {
				return nil, err
			}
			oldString := argString(args, "old_string")
			newString := argString(args, "new_string")
			if len(oldString) == 0 {
				return nil, fmt.Errorf("old_string must be a non-empty string")
			}
			if oldString == newString {
				return nil, fmt.Errorf("old_string and new_string must differ")
			}
			replaceAll := argBool(args, "replace_all")
			ctx := execSignal(exec)
			policy, err := controller.resolvePolicy("edit", args, exec)
			if err != nil {
				return nil, err
			}
			var policyRoot string
			if policy != nil {
				policyRoot = policy.WorkspaceRoot
			}
			target, err := controller.resolveTarget(ctx, argString(args, "file_path"), policyRoot, execScope(exec))
			if err != nil {
				return nil, err
			}
			// The intent slot itself can throw FS_NOT_OBSERVED for an unread
			// target, so it sits inside the remediation path: both that
			// refusal and the guarded-mutation failure gain the remedy.
			decided := controller.waterfall(fs.EventEditIntent, fs.EditIntentEvent{Target: target, Actor: exec})
			var intent *fs.Version
			if owned, ok := decided.(*fs.Version); ok {
				intent = owned
			}
			outcome, err := controller.backend.EditText(ctx, target, fs.EditRequest{OldString: oldString, NewString: newString, ReplaceAll: replaceAll}, intent, policy)
			if err != nil {
				return nil, RemediateFsError(controller.mapError(err, policy))
			}
			controller.recordObservation(target, fs.ObservationPresent(outcome.Version), exec)
			return map[string]any{
				"path":   target.DisplayPath,
				"before": outcome.Before,
				"after":  outcome.After,
			}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

// resolveRegularReadTarget resolves a model-supplied path, observes absence,
// and requires a regular file.
func resolveRegularReadTarget(ctx context.Context, controller *controller, requestedPath string, exec *tools.ToolRunContext) (fs.Target, *fs.Info, error) {
	target, err := controller.resolveTarget(ctx, requestedPath, "", execScope(exec))
	if err != nil {
		return fs.Target{}, nil, err
	}
	info, err := controller.backend.Stat(ctx, target)
	if err != nil {
		return fs.Target{}, nil, err
	}
	if info == nil {
		controller.recordObservation(target, fs.ObservationAbsent(), exec)
		return fs.Target{}, nil, fs.NewError(fs.CodeNotFound, fmt.Sprintf("cannot read %q: not found", target.DisplayPath), nil)
	}
	if info.Type != fs.TypeFile {
		return fs.Target{}, nil, fs.NewError(fs.CodeNotRegularFile, fmt.Sprintf("cannot read %q: not a regular file", target.DisplayPath), nil)
	}
	return target, info, nil
}
