// Package strreplaceeditor registers the model-facing str_replace_editor tool
// (official @deepseek-ai/dsh-tool-str-replace-editor): view, create,
// str_replace, and insert over the fs seam. Read windows and observed-state
// records ride the fs events; version guards ride the write intents.
package strreplaceeditor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/fs"
	"dshgo/llm"
	"dshgo/tools"
)

// truncatedMessage marks clipped output.
const truncatedMessage = "<response clipped><NOTE>To save on context only part of this file has been shown to you. You should retry this tool after you have searched inside the file with `grep -n` in order to find the line numbers of what you are looking for.</NOTE>"

// DefaultDescription is the default model-facing tool description.
const DefaultDescription = `Custom editing tool for viewing, creating and editing files
* State is persistent across command calls and discussions with the user
* If path is a file, view displays the result of applying cat -n. If path is a directory, view lists non-hidden files and directories up to 2 levels deep
* The create command cannot be used if the specified path already exists as a file
* If a command generates a long output, it will be truncated and marked with <response clipped>
* A null placeholder for a parameter unused by the selected command is treated as omitted. Required parameters still need values; omit str_replace.new_str rather than setting it to null when deleting a match

Notes for using the str_replace command:
* The old_str parameter should match EXACTLY one or more consecutive lines from the original file. Be mindful of whitespaces!
* If the old_str parameter is not unique in the file, the replacement will not be performed. Make sure to include enough context in old_str to make it unique
* The new_str parameter should contain the edited lines that should replace the old_str`

// sandboxDenialMarker is the stable marker text a mapped sandbox denial
// carries so clients can recognize the fence without parsing details.
func sandboxDenialMarker(mode string) string {
	return fmt.Sprintf("The edit was denied by the sandbox (mode %s).", mode)
}

// Config is the tool configuration.
type Config struct {
	// MaxOutputChars caps rendered tool output. Zero applies the default.
	MaxOutputChars int
	// Description overrides the model-facing description.
	Description string
}

const defaultMaxOutputChars = 100_000

// PolicyResolver resolves the per-call sandbox fencing policy for one
// mutation (official ctx.sandboxPolicy.resolve). Only required when the
// mounted filesystem reports a sandbox mode.
type PolicyResolver interface {
	// ResolveMutationPolicy returns nil when the call runs unfenced.
	ResolveMutationPolicy(actor *agent.Agent) *fs.SandboxExecutionPolicy
}

// Deps carries the tool's composition inputs.
type Deps struct {
	// FS is the mounted filesystem backend.
	FS fs.FileSystem
	// Ctx is the plugin context: the fs decision waterfalls and the
	// observation records ride it.
	Ctx *cordis.Context
	// Policy is the sandbox policy service; required only when
	// FS.SandboxMode() is non-empty.
	Policy PolicyResolver
	// Agents resolves the calling agent from one execution's scope key
	// (the established resolveByScope pattern).
	Agents *agent.AgentRegistry
}

// mutationPolicy mirrors the official MutationPolicy: the policy layer only
// exists when the backend confines, and a confining backend without the
// service is a composition bug.
type mutationPolicy struct {
	resolver PolicyResolver
	mode     string
}

func newMutationPolicy(deps Deps) (*mutationPolicy, error) {
	mode := deps.FS.SandboxMode()
	if mode == "" {
		return &mutationPolicy{}, nil
	}
	if deps.Policy == nil {
		return nil, fmt.Errorf("str_replace_editor: the mounted filesystem confines but the sandbox policy service is missing")
	}
	return &mutationPolicy{resolver: deps.Policy, mode: mode}, nil
}

func (m *mutationPolicy) resolve(actor *agent.Agent) *fs.SandboxExecutionPolicy {
	if m.resolver == nil {
		return nil
	}
	return m.resolver.ResolveMutationPolicy(actor)
}

func (m *mutationPolicy) mapError(err error, policy *fs.SandboxExecutionPolicy) error {
	if codeErr, ok := err.(*fs.Error); ok && codeErr.Code == fs.CodeSandboxDenied && policy != nil {
		return fs.NewError(fs.CodeSandboxDenied, sandboxDenialMarker(policy.Mode), err)
	}
	return err
}

// runner carries one tool call's resolution state.
type runner struct {
	backend fs.FileSystem
	ctx     *cordis.Context
	policy  *mutationPolicy
	chars   int
}

func maybeTruncate(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}
	return content[:maxChars] + truncatedMessage
}

// matchOffsets finds every occurrence offset of search in content.
func matchOffsets(content string, search string) []int {
	var offsets []int
	offset := 0
	for {
		at := strings.Index(content[offset:], search)
		if at < 0 {
			return offsets
		}
		at += offset
		offsets = append(offsets, at)
		offset = at + len(search)
	}
}

// lineNumbersAt maps match offsets to 1-based line numbers.
func lineNumbersAt(content string, offsets []int) []int {
	line, cursor := 1, 0
	numbers := make([]int, 0, len(offsets))
	for _, offset := range offsets {
		for cursor < offset {
			if content[cursor] == '\n' {
				line++
			}
			cursor++
		}
		numbers = append(numbers, line)
	}
	return numbers
}

// argString reads one string argument; absent reports missing.
func argString(args map[string]any, key string) (string, bool) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", false
	}
	value, _ := raw.(string)
	return value, true
}

// argJSONNull reports a key present with an explicit JSON null value.
func argJSONNull(args map[string]any, key string) bool {
	raw, ok := args[key]
	return ok && raw == nil
}

func (r *runner) recordObservation(target fs.Target, observation fs.Observation, actor any) {
	r.ctx.Waterfall(fs.EventObserved, fs.ObservedEvent{Target: target, Observation: observation, Actor: actor})
}

// resolveTarget applies the tool's path discipline: non-empty and absolute
// (the model faces the harness host path space directly).
func (r *runner) resolveTarget(ctx context.Context, path string) (fs.Target, error) {
	if strings.TrimSpace(path) == "" {
		return fs.Target{}, fmt.Errorf("path must be a non-empty string")
	}
	if !strings.HasPrefix(path, "/") && !filepathIsAbsolute(path) {
		return fs.Target{}, fmt.Errorf("The path %s is not an absolute path, it should start with `/`. Maybe you meant /%s?", path, path)
	}
	return r.backend.Resolve(ctx, path, "")
}

// statExisting resolves the target's presence and type discipline for one
// command; an absent target records the negative observation before failing.
func (r *runner) statExisting(ctx context.Context, target fs.Target, command string, actor any) (*fs.Info, error) {
	info, err := r.backend.Stat(ctx, target)
	if err != nil {
		return nil, err
	}
	if info == nil {
		r.recordObservation(target, fs.ObservationAbsent(), actor)
		return nil, fs.NewError(fs.CodeNotFound, fmt.Sprintf("The path %s does not exist. Please provide a valid path.", target.DisplayPath), nil)
	}
	if info.Type == fs.TypeDirectory && command != "view" {
		return nil, fs.NewError(fs.CodeNotRegularFile, fmt.Sprintf("The path %s is a directory and only the `view` command can be used on directories", target.DisplayPath), nil)
	}
	return info, nil
}

// formatFileView renders cat -n styled content with the view_range discipline.
func formatFileView(path string, content string, maxChars int, viewRange []int) (string, error) {
	allLines := strings.Split(content, "\n")
	lines := allLines
	initialLine, finalLine := 1, 0
	prompt := fmt.Sprintf("Here's the content of %s with line numbers (which has a total of %d lines)", path, len(allLines))
	if viewRange != nil {
		if len(viewRange) != 2 {
			return "", fmt.Errorf("Invalid `view_range`. It should be a list of two integers.")
		}
		initialLine, finalLine = viewRange[0], viewRange[1]
		if initialLine < 1 || initialLine > len(allLines) {
			return "", fmt.Errorf("Invalid `view_range`: [%d, %d]. Its first element `%d` should be within the range of lines of the file: [1, %d]", viewRange[0], viewRange[1], initialLine, len(allLines))
		}
		if finalLine > len(allLines) {
			return "", fmt.Errorf("Invalid `view_range`: [%d, %d]. Its second element `%d` should be smaller than the number of lines in the file: `%d`", viewRange[0], viewRange[1], finalLine, len(allLines))
		}
		if finalLine != -1 && finalLine < initialLine {
			return "", fmt.Errorf("Invalid `view_range`: [%d, %d]. Its second element `%d` should be larger or equal than its first `%d`", viewRange[0], viewRange[1], finalLine, initialLine)
		}
		if finalLine == -1 {
			lines = allLines[initialLine-1:]
		} else {
			lines = allLines[initialLine-1 : finalLine]
		}
		prompt += fmt.Sprintf(" with view_range=[%d, %d]", initialLine, finalLine)
	}
	numbered := make([]string, 0, len(lines))
	for index, line := range lines {
		numbered = append(numbered, fmt.Sprintf("%6d  %s", initialLine+index, line))
	}
	return maybeTruncate(prompt+":\n"+strings.Join(numbered, "\n")+"\n", maxChars), nil
}

// listDirectoryView renders the two-level non-hidden tree in stable path
// order.
func (r *runner) listDirectoryView(ctx context.Context, target fs.Target, actor any) (string, error) {
	var visit func(dir fs.Target, depth int) ([]string, error)
	visit = func(dir fs.Target, depth int) ([]string, error) {
		entries, err := r.backend.ListDir(ctx, dir)
		if err != nil {
			return nil, err
		}
		rows := []string{}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name, ".") || entry.Name == "node_modules" || entry.Name == "__pycache__" {
				continue
			}
			marker := "?"
			switch entry.Type {
			case fs.TypeDirectory:
				marker = "d"
			case fs.TypeFile:
				marker = "f"
			}
			rows = append(rows, marker+"\t"+entry.Target.DisplayPath)
			if entry.Type == fs.TypeDirectory && depth < 2 {
				nested, err := visit(entry.Target, depth+1)
				if err != nil {
					return nil, err
				}
				rows = append(rows, nested...)
			}
		}
		return rows, nil
	}
	head, err := visit(target, 1)
	if err != nil {
		return "", err
	}
	rows := append([]string{"d\t" + target.DisplayPath}, head...)
	sort.Slice(rows, func(i, j int) bool {
		return rows[i][strings.Index(rows[i], "\t")+1:] < rows[j][strings.Index(rows[j], "\t")+1:]
	})
	listing := maybeTruncate(strings.Join(rows, "\n")+"\n", r.chars)
	return fmt.Sprintf("Here're the files and directories up to 2 levels deep in %s, excluding hidden items, node_modules, and Python cache directories:\n%s\n", target.DisplayPath, listing), nil
}

func (r *runner) viewPath(ctx context.Context, path string, viewRange []int, actor any) (string, error) {
	target, err := r.resolveTarget(ctx, path)
	if err != nil {
		return "", err
	}
	info, err := r.statExisting(ctx, target, "view", actor)
	if err != nil {
		return "", err
	}
	if info.Type == fs.TypeDirectory {
		if viewRange != nil {
			return "", fmt.Errorf("The `view_range` parameter is not allowed when `path` points to a directory.")
		}
		return r.listDirectoryView(ctx, target, actor)
	}
	if info.Type != fs.TypeFile {
		return "", fs.NewError(fs.CodeNotRegularFile, fmt.Sprintf("cannot view %q: not a regular file or directory", target.DisplayPath), nil)
	}
	content, err := r.backend.ReadText(ctx, target)
	if err != nil {
		return "", err
	}
	r.recordObservation(target, fs.ObservationPresent(info.Version), actor)
	return formatFileView(target.DisplayPath, content, r.chars, viewRange)
}

func (r *runner) createFile(ctx context.Context, actor *agent.Agent, path string, fileText *string) (string, error) {
	if fileText == nil {
		return "", fmt.Errorf("Parameter `file_text` is required for command: create")
	}
	sandboxPolicy := r.policy.resolve(actor)
	target, err := r.resolveTarget(ctx, path)
	if err != nil {
		return "", err
	}
	existing, err := r.backend.Stat(ctx, target)
	if err != nil {
		return "", err
	}
	if existing != nil {
		return "", fmt.Errorf("File already exists at: %s. Cannot overwrite files using command `create`.", target.DisplayPath)
	}
	var intent *fs.WriteIntent = &fs.WriteIntent{Kind: fs.IntentCreateIfAbsent}
	decided := r.ctx.Waterfall(fs.EventWriteIntent, fs.WriteIntentEvent{Target: target, Actor: actor})
	if owned, ok := decided.(*fs.WriteIntent); ok && owned != nil {
		intent = owned
	}
	outcome, err := r.backend.WriteText(ctx, target, *fileText, intent, sandboxPolicy)
	if err != nil {
		return "", r.policy.mapError(err, sandboxPolicy)
	}
	r.recordObservation(target, fs.ObservationPresent(outcome.Version), actor)
	return fmt.Sprintf("New file created successfully at: %s", target.DisplayPath), nil
}

func (r *runner) replaceInFile(ctx context.Context, actor *agent.Agent, path string, oldStr *string, newStr *string, newStrNull bool) (string, error) {
	if newStrNull {
		return "", fmt.Errorf("Parameter `new_str` must be omitted or contain a string for command: str_replace")
	}
	if oldStr == nil {
		return "", fmt.Errorf("Parameter `old_str` is required for command: str_replace")
	}
	if len(*oldStr) == 0 {
		return "", fmt.Errorf("Parameter `old_str` is empty for command: str_replace")
	}
	newValue := ""
	if newStr != nil {
		newValue = *newStr
	}
	sandboxPolicy := r.policy.resolve(actor)
	target, err := r.resolveTarget(ctx, path)
	if err != nil {
		return "", err
	}
	decided := r.ctx.Waterfall(fs.EventEditIntent, fs.EditIntentEvent{Target: target, Actor: actor})
	var intentVersion *fs.Version
	if owned, ok := decided.(*fs.Version); ok && owned != nil {
		intentVersion = owned
	}
	info, err := r.statExisting(ctx, target, "str_replace", actor)
	if err != nil {
		return "", err
	}
	if info.Type != fs.TypeFile {
		return "", fs.NewError(fs.CodeNotRegularFile, fmt.Sprintf("cannot edit %q: not a regular file", target.DisplayPath), nil)
	}
	before, err := r.backend.ReadText(ctx, target)
	if err != nil {
		return "", err
	}
	offsets := matchOffsets(before, *oldStr)
	if len(offsets) == 0 {
		return "", fs.NewError(fs.CodeEditNotFound, fmt.Sprintf("No replacement was performed, old_str `%s` did not appear verbatim in %s.", *oldStr, target.DisplayPath), nil)
	}
	if len(offsets) > 1 {
		lines := lineNumbersAt(before, offsets)
		numbers := make([]string, 0, len(lines))
		for _, line := range lines {
			numbers = append(numbers, strconv.Itoa(line))
		}
		return "", fs.NewError(fs.CodeAmbiguousEdit, fmt.Sprintf("No replacement was performed. Multiple occurrences of old_str `%s` in lines [%s]. Please ensure it is unique", *oldStr, strings.Join(numbers, ", ")), nil)
	}
	offset := offsets[0]
	edited := before[:offset] + newValue + before[offset+len(*oldStr):]
	guard := fs.WriteIntent{Kind: fs.IntentReplaceIfVersion, Version: info.Version}
	if intentVersion != nil {
		guard.Version = *intentVersion
	}
	outcome, err := r.backend.WriteText(ctx, target, edited, &guard, sandboxPolicy)
	if err != nil {
		return "", r.policy.mapError(err, sandboxPolicy)
	}
	r.recordObservation(target, fs.ObservationPresent(outcome.Version), actor)
	return fmt.Sprintf("The file %s has been edited successfully.", target.DisplayPath), nil
}

func (r *runner) insertInFile(ctx context.Context, actor *agent.Agent, path string, insertLine *int, newStr *string) (string, error) {
	if insertLine == nil {
		return "", fmt.Errorf("Parameter `insert_line` is required for command: insert")
	}
	if newStr == nil {
		return "", fmt.Errorf("Parameter `new_str` is required for command: insert")
	}
	sandboxPolicy := r.policy.resolve(actor)
	target, err := r.resolveTarget(ctx, path)
	if err != nil {
		return "", err
	}
	decided := r.ctx.Waterfall(fs.EventEditIntent, fs.EditIntentEvent{Target: target, Actor: actor})
	var intentVersion *fs.Version
	if owned, ok := decided.(*fs.Version); ok && owned != nil {
		intentVersion = owned
	}
	info, err := r.statExisting(ctx, target, "insert", actor)
	if err != nil {
		return "", err
	}
	if info.Type != fs.TypeFile {
		return "", fs.NewError(fs.CodeNotRegularFile, fmt.Sprintf("cannot insert into %q: not a regular file", target.DisplayPath), nil)
	}
	before, err := r.backend.ReadText(ctx, target)
	if err != nil {
		return "", err
	}
	lines := strings.Split(before, "\n")
	if *insertLine < 0 || *insertLine > len(lines) {
		return "", fmt.Errorf("Invalid `insert_line` parameter: %d. It should be within the range of lines of the file: [0, %d]", *insertLine, len(lines))
	}
	after := make([]string, 0, len(lines)+1)
	after = append(after, lines[:*insertLine]...)
	after = append(after, strings.Split(*newStr, "\n")...)
	after = append(after, lines[*insertLine:]...)
	guard := fs.WriteIntent{Kind: fs.IntentReplaceIfVersion, Version: info.Version}
	if intentVersion != nil {
		guard.Version = *intentVersion
	}
	outcome, err := r.backend.WriteText(ctx, target, strings.Join(after, "\n"), &guard, sandboxPolicy)
	if err != nil {
		return "", r.policy.mapError(err, sandboxPolicy)
	}
	r.recordObservation(target, fs.ObservationPresent(outcome.Version), actor)
	return fmt.Sprintf("The file %s has been edited successfully.", target.DisplayPath), nil
}

// filepathIsAbsolute reports absolute paths across host platforms.
func filepathIsAbsolute(path string) bool {
	return strings.HasPrefix(path, "/") || (len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/')) || strings.HasPrefix(path, "\\\\")
}

// Register installs str_replace_editor over one mounted filesystem backend.
// The returned disposer tears it down.
func Register(runtime *tools.ToolRuntime, deps Deps, config Config) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("str_replace_editor: a tool runtime is required")
	}
	if deps.FS == nil {
		return nil, fmt.Errorf("str_replace_editor: a filesystem backend is required")
	}
	if deps.Ctx == nil {
		return nil, fmt.Errorf("str_replace_editor: a plugin context is required")
	}
	policy, err := newMutationPolicy(deps)
	if err != nil {
		return nil, err
	}
	chars := config.MaxOutputChars
	if chars == 0 {
		chars = defaultMaxOutputChars
	}
	description := config.Description
	if description == "" {
		description = DefaultDescription
	}
	r := &runner{backend: deps.FS, ctx: deps.Ctx, policy: policy, chars: chars}

	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "str_replace_editor",
		Description: description,
		Parameters: map[string]tools.PropSpec{
			"command": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Enum:        []any{"view", "create", "str_replace", "insert"},
					Description: "The commands to run. Allowed options are: `view`, `create`, `str_replace`, `insert`.",
				},
				Required: true,
			},
			"path": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "Absolute path to file or directory, e.g. `/repo/file.py` or `/repo`.",
				},
				Required: true,
			},
			"file_text": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "Required string parameter of `create` command, with the content of the file to be created. A null placeholder is treated as omitted by commands that do not use this parameter.",
				},
			},
			"insert_line": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "integer",
					Description: "Required integer parameter of `insert` command. The `new_str` will be inserted AFTER the line `insert_line` of `path`. A null placeholder is treated as omitted by commands that do not use this parameter.",
				},
			},
			"new_str": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "Optional string parameter of `str_replace` command containing the new string (if omitted, no string will be added). Required string parameter of `insert` command containing the string to insert. A null placeholder is accepted only by commands that do not use this parameter.",
				},
			},
			"old_str": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "Required string parameter of `str_replace` command containing the string in `path` to replace. A null placeholder is treated as omitted by commands that do not use this parameter.",
				},
			},
			"view_range": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "array",
					Items:       &tools.ValueSchemaSpec{Type: "integer"},
					Description: "Optional parameter of `view` command when `path` points to a file. If omitted or null, the full file is shown. If provided, the file will be shown in the indicated line number range, e.g. [11, 12] will show lines 11 and 12. Indexing at 1 to start. Setting `[start_line, -1]` shows all lines from `start_line` to the end of the file.",
				},
			},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "string"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				text, _ := value.(string)
				return []llm.ContentBlock{{Type: llm.BlockText, Text: text}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			command, _ := args["command"].(string)
			path, _ := argString(args, "path")
			ctx := context.Background()
			var signal context.Context
			if exec != nil && exec.Signal != nil {
				signal = exec.Signal
			} else {
				signal = ctx
			}
			actor := resolveExecAgent(deps.Agents, exec)
			switch command {
			case "view":
				var viewRange []int
				if raw, ok := args["view_range"].([]any); ok {
					for _, item := range raw {
						number, _ := item.(float64)
						viewRange = append(viewRange, int(number))
					}
				}
				return r.viewPath(signal, path, viewRange, actor)
			case "create":
				var fileText *string
				if value, ok := argString(args, "file_text"); ok {
					fileText = &value
				}
				return r.createFile(signal, actor, path, fileText)
			case "str_replace":
				var oldStr, newStr *string
				if value, ok := argString(args, "old_str"); ok {
					oldStr = &value
				}
				if argJSONNull(args, "new_str") {
					return nil, fmt.Errorf("Parameter `new_str` must be omitted or contain a string for command: str_replace")
				}
				if value, ok := argString(args, "new_str"); ok {
					newStr = &value
				}
				return r.replaceInFile(signal, actor, path, oldStr, newStr, false)
			case "insert":
				var insertLine *int
				if raw, ok := args["insert_line"].(float64); ok {
					line := int(raw)
					insertLine = &line
				}
				var newStr *string
				if value, ok := argString(args, "new_str"); ok {
					newStr = &value
				}
				return r.insertInFile(signal, actor, path, insertLine, newStr)
			default:
				return nil, fmt.Errorf("Unknown command: %s", command)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

// resolveExecAgent resolves the calling agent for policy resolution and
// observation attribution.
func resolveExecAgent(agents *agent.AgentRegistry, exec *tools.ToolRunContext) *agent.Agent {
	if agents == nil || exec == nil || exec.Agent == nil {
		return nil
	}
	for _, candidate := range agents.List() {
		if candidate.Scope == exec.Agent {
			return candidate
		}
	}
	return nil
}
