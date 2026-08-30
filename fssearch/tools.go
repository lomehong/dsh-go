package fssearch

import (
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// boolPtr returns a pointer to b (schema authoring helper).
func boolPtr(b bool) *bool { return &b }

// textBlocks renders one or more text lines as content blocks.
func textBlocks(lines ...string) []llm.ContentBlock {
	blocks := make([]llm.ContentBlock, 0, len(lines))
	for _, line := range lines {
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockText, Text: line})
	}
	return blocks
}

// toString is the render fallback for an unexpected value shape.
func toString(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

// Register installs the glob and grep tools (and their system-prompt
// guidance) on the composed registries. Execution uses the ctx's
// subprocess service. The returned undo unregisters both tools and
// disposes the prompt sections.
func Register(runtime *tools.ToolRuntime, prompt *systemprompt.SystemPrompt, ctx *cordis.Context, caps SearchCaps) (func(), error) {
	if caps.GlobMaxResults <= 0 {
		return nil, errArgs("globMaxResults must be positive")
	}
	if caps.GrepMaxMatches <= 0 {
		return nil, errArgs("grepMaxMatches must be positive")
	}
	if caps.GrepMaxLineBytes <= 0 {
		return nil, errArgs("grepMaxLineBytes must be positive")
	}
	if caps.RawOutputMaxBytes <= 0 {
		return nil, errArgs("rawOutputMaxBytes must be positive")
	}
	if caps.GraceMs <= 0 {
		return nil, errArgs("graceMs must be positive")
	}
	if caps.TimeoutMs <= 0 {
		return nil, errArgs("timeoutMs must be positive")
	}
	undoGlob, err := registerGlob(runtime, prompt, ctx, caps)
	if err != nil {
		return nil, err
	}
	undoGrep, err := registerGrep(runtime, prompt, ctx, caps)
	if err != nil {
		undoGlob()
		return nil, err
	}
	return func() { undoGrep(); undoGlob() }, nil
}

// globText is the tool:glob system-prompt guidance; the over-cap clause
// tracks the deployment's sampling switch.
func globText(caps SearchCaps) string {
	overCap := "while a larger one keeps the modification-time-ordered head."
	if caps.SampleOverCapGlobResults {
		overCap = "while a larger one is sampled across top-level entries, so it spans the tree instead of one subtree."
	}
	return "Use the glob tool — not shell find — to discover files by path pattern. A pattern with no \"/\" matches basenames at any depth, so \"*\" matches every file in the tree rather than its top level. " +
		"Results are files only, never directories, and include hidden and ignored files: a result that fits comes back in modification-time order, " + overCap
}

func globDescription(caps SearchCaps) string {
	overCap := "a larger result returns the first " + itoa(caps.GlobMaxResults) + " paths in modification-time order"
	if caps.SampleOverCapGlobResults {
		overCap = "a larger result instead returns " + itoa(caps.GlobMaxResults) + " paths sampled across top-level entries"
	}
	return "Find files whose paths match a glob pattern. Returns matching file paths — never directories — " +
		"including hidden and ignored files (VCS metadata directories are excluded). " +
		"Up to " + itoa(caps.GlobMaxResults) + " paths come back in modification-time order; " + overCap + ", " +
		"says so, and reports where the complete sorted list was saved. This tool does not enumerate directory entries."
}

// sectionOrUndo registers one prompt section when a prompt service is
// composed; on later failure the caller invokes the returned undo.
func sectionOrUndo(prompt *systemprompt.SystemPrompt, section systemprompt.PromptSection) (undo func(), rollback func(), err error) {
	if prompt == nil {
		return func() {}, func() {}, nil
	}
	undo, err = prompt.Section(nil, section)
	if err != nil {
		return nil, func() {}, err
	}
	return undo, undo, nil
}

func registerGlob(runtime *tools.ToolRuntime, prompt *systemprompt.SystemPrompt, ctx *cordis.Context, caps SearchCaps) (func(), error) {
	undo, rollback, err := sectionOrUndo(prompt, systemprompt.PromptSection{
		Name:  "tool:glob",
		Order: systemprompt.OrderToolGlob,
		Text:  globText(caps),
	})
	if err != nil {
		return nil, err
	}
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "glob",
		Description: globDescription(caps),
		Parameters: map[string]tools.PropSpec{
			"pattern": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "string",
				Description: "Glob pattern to match file paths against (e.g. \"**/*.ts\", \"src/**/*.test.js\"). " +
					"A pattern with no \"/\" matches the basename at any depth, so \"*\" and \"*.ts\" both search the whole tree; include a separator to anchor the depth.",
			}, Required: true},
			"path": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "Directory to search in. Defaults to the session workspace; a relative path resolves against it.",
			}},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"root":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
					"paths": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "array", Items: &tools.ValueSchemaSpec{Type: "string"}}, Required: true},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				outcome, ok := value.(map[string]any)
				if !ok {
					return textBlocks(toString(value))
				}
				root, _ := outcome["root"].(string)
				var paths []string
				if raw, ok := outcome["paths"].([]any); ok {
					for _, item := range raw {
						if s, ok := item.(string); ok {
							paths = append(paths, s)
						}
					}
				}
				return textBlocks(RenderGlobPaths(paths, caps, root, nil))
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			input, err := ParseGlobArgs(args)
			if err != nil {
				return nil, err
			}
			run, err := runRipgrep(ctx, exec.Signal, caps, "glob", BuildGlobCommand(input))
			if err != nil {
				return nil, err
			}
			root := "."
			if input.Path != "" {
				root = toWorkdirRelative(input.Path, run.Workdir)
			}
			if run.NoMatches {
				return map[string]any{"root": root, "paths": []any{}}, nil
			}
			paths := parseGlobLines(run.Stdout, run.Workdir)
			canonical := make([]any, 0, len(paths))
			for _, path := range paths {
				canonical = append(canonical, path)
			}
			return map[string]any{"root": root, "paths": canonical}, nil
		},
	})
	if err != nil {
		rollback()
		return nil, err
	}
	undoTool, err := runtime.Register(definition)
	if err != nil {
		rollback()
		return nil, err
	}
	return func() { undoTool(); undo() }, nil
}

func grepDescription(caps SearchCaps) string {
	return "Search file contents with a ripgrep regular expression. Returns matching lines with line numbers, grouped by file. " +
		"Returns the first " + itoa(caps.GrepMaxMatches) + " matches inline; a capped result reports where the complete match list was saved. " +
		"Use read on a matched file for surrounding context."
}

func registerGrep(runtime *tools.ToolRuntime, prompt *systemprompt.SystemPrompt, ctx *cordis.Context, caps SearchCaps) (func(), error) {
	undo, rollback, err := sectionOrUndo(prompt, systemprompt.PromptSection{
		Name:  "tool:grep",
		Order: systemprompt.OrderToolGrep,
		Text:  "Use the grep tool — not shell grep or rg — to search file contents. Use read on a matched file when you need surrounding context.",
	})
	if err != nil {
		return nil, err
	}
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "grep",
		Description: grepDescription(caps),
		Parameters: map[string]tools.PropSpec{
			"pattern": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "Regular expression to search for (ripgrep syntax).",
			}, Required: true},
			"path": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "File or directory to search. Defaults to the session workspace; a relative path resolves against it.",
			}},
			"include": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "One glob filter for which files to search (e.g. \"*.ts\", \"*.{js,jsx}\"). Not a list; negation is not supported.",
			}},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"matches": {ValueSchemaSpec: tools.ValueSchemaSpec{
						Type: "array",
						Items: &tools.ValueSchemaSpec{
							Type:                 "object",
							AdditionalProperties: boolPtr(false),
							Properties: map[string]tools.PropSpec{
								"path":       {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
								"lineNumber": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
								"line":       {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
							},
						},
					}, Required: true},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				outcome, ok := value.(map[string]any)
				if !ok {
					return textBlocks(toString(value))
				}
				kept, seen, truncated := retainFromValue(outcome, caps)
				return textBlocks(FormatRetainedGrep(kept, seen, truncated, nil))
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			input, err := ParseGrepArgs(args)
			if err != nil {
				return nil, err
			}
			run, err := runRipgrep(ctx, exec.Signal, caps, "grep", BuildGrepCommand(input))
			if err != nil {
				return nil, err
			}
			if run.NoMatches {
				return map[string]any{"matches": []any{}}, nil
			}
			parsed, err := ParseGrepMatches(run.Stdout)
			if err != nil {
				return nil, err
			}
			matches := make([]any, 0, len(parsed))
			for _, raw := range parsed {
				matches = append(matches, map[string]any{
					"path":       toWorkdirRelative(raw.Path, run.Workdir),
					"lineNumber": raw.LineNumber,
					"line":       raw.Line,
				})
			}
			return map[string]any{"matches": matches}, nil
		},
	})
	if err != nil {
		rollback()
		return nil, err
	}
	undoTool, err := runtime.Register(definition)
	if err != nil {
		rollback()
		return nil, err
	}
	return func() { undoTool(); undo() }, nil
}

// retainFromValue projects the canonical value into the retained list the
// render face consumes.
func retainFromValue(outcome map[string]any, caps SearchCaps) (kept []GrepMatch, seen int, truncated bool) {
	var matches []GrepMatch
	if raw, ok := outcome["matches"].([]any); ok {
		for _, item := range raw {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			path, _ := row["path"].(string)
			lineNumber, _ := row["lineNumber"].(int)
			line, _ := row["line"].(string)
			matches = append(matches, GrepMatch{Path: path, LineNumber: lineNumber, Line: line})
		}
	}
	return retainGrepMatches(matches, caps.GrepMaxMatches, caps.GrepMaxLineBytes)
}
